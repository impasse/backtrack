package thumbnail

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"backtrack/internal/model"
	"backtrack/internal/storage"
)

// Service generates one small preview about every six seconds of recorded media.
type Service struct {
	store  *storage.Store
	logger *slog.Logger

	mu     sync.Mutex
	queue  []model.Segment
	queued map[string]struct{}
	wake   chan struct{}
}

func New(store *storage.Store, logger *slog.Logger) *Service {
	return &Service{store: store, logger: logger, queued: make(map[string]struct{}), wake: make(chan struct{}, 1)}
}

func (s *Service) Enqueue(segment model.Segment) {
	if segment.Sequence%3 != 0 || !segment.RandomAccess {
		return
	}
	if s.store.HasPreview(segment.ID) {
		return
	}
	s.mu.Lock()
	if _, exists := s.queued[segment.ID]; !exists {
		s.queued[segment.ID] = struct{}{}
		s.queue = append(s.queue, segment)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Run(ctx context.Context) {
	segments, err := s.store.PreviewSegments(ctx)
	if err != nil {
		s.logger.Warn("could not queue existing segments for preview generation", "error", err)
	} else {
		for _, segment := range segments {
			s.Enqueue(segment)
		}
	}
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			s.work(ctx)
		}()
	}
	<-ctx.Done()
	workers.Wait()
}

func (s *Service) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		for {
			segment, ok := s.pop()
			if !ok {
				break
			}
			if err := s.generate(ctx, segment); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Debug("could not generate timeline preview", "segment", segment.ID, "error", err)
			}
			s.mu.Lock()
			delete(s.queued, segment.ID)
			more := len(s.queue) > 0
			s.mu.Unlock()
			if more {
				select {
				case s.wake <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (s *Service) pop() (model.Segment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return model.Segment{}, false
	}
	v := s.queue[0]
	s.queue[0] = model.Segment{}
	s.queue = s.queue[1:]
	return v, true
}

func (s *Service) generate(ctx context.Context, segment model.Segment) error {
	if s.store.HasPreview(segment.ID) {
		return nil
	}
	init, err := s.store.OpenInit(ctx, segment.EpochID)
	if err != nil {
		return err
	}
	defer init.Close()
	media, err := s.store.OpenSegment(segment)
	if err != nil {
		return err
	}
	defer media.Close()
	work, err := os.MkdirTemp("", "backtrack-preview-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	input := filepath.Join(work, "segment.mp4")
	file, err := os.Create(input)
	if err != nil {
		return err
	}
	_, copyInitErr := io.Copy(file, init)
	_, copyMediaErr := io.Copy(file, media)
	closeErr := file.Close()
	if err = errors.Join(copyInitErr, copyMediaErr, closeErr); err != nil {
		return err
	}
	output := filepath.Join(work, "preview.jpg")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-ss", "0.25", "-i", input, "-frames:v", "1", "-vf", "scale=320:180:force_original_aspect_ratio=decrease,pad=320:180:(ow-iw)/2:(oh-ih)/2", "-q:v", "7", output)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New(strings.TrimSpace(stderr.String()))
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return err
	}
	return s.store.SavePreview(segment.ID, data)
}
