package clips

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"backtrack/internal/config"
	"backtrack/internal/events"
	"backtrack/internal/model"
	"backtrack/internal/storage"
)

type Service struct {
	cfg     config.Config
	store   *storage.Store
	logger  *slog.Logger
	events  *events.Hub
	queue   chan model.ClipJob
	mu      sync.Mutex
	running int
	cancel  map[string]context.CancelFunc
	workers sync.WaitGroup
}

func New(cfg config.Config, store *storage.Store, logger *slog.Logger, eventHub *events.Hub) *Service {
	capacity := cfg.Clips.MaxQueueSize
	if capacity < 1 {
		capacity = 1
	}
	return &Service{cfg: cfg, store: store, logger: logger, events: eventHub, queue: make(chan model.ClipJob, capacity), cancel: map[string]context.CancelFunc{}}
}

func (s *Service) Create(ctx context.Context, req model.ClipRequest) (model.ClipJob, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.SourceClipID = strings.TrimSpace(req.SourceClipID)
	reencode := req.SourceClipID != ""
	if reencode {
		source, err := s.store.ClipJob(ctx, req.SourceClipID)
		if err != nil {
			return model.ClipJob{}, errors.New("favorite clip not found")
		}
		if source.Status != "COMPLETED" || !source.Pinned {
			return model.ClipJob{}, errors.New("only completed favorite clips can be re-encoded")
		}
		file, err := s.store.OpenClip(ctx, req.SourceClipID)
		if err != nil {
			return model.ClipJob{}, fmt.Errorf("open favorite clip: %w", err)
		}
		_ = file.Close()
		req.Name = source.Request.Name
		req.StreamID = source.Request.StreamID
		req.StartTime = source.Request.StartTime
		req.EndTime = source.Request.EndTime
		if req.Mode == "" {
			req.Mode = "fast"
		}
		req.GapPolicy = "skip"
		req.SaveToLibrary = false
	}
	if len([]rune(req.Name)) > 100 {
		return model.ClipJob{}, errors.New("clip name must be 100 characters or fewer")
	}
	if req.StreamID == "" || req.StartTime.IsZero() || req.EndTime.IsZero() || !req.EndTime.After(req.StartTime) {
		return model.ClipJob{}, errors.New("streamId and a positive startTime/endTime range are required")
	}
	if req.Name == "" {
		stream, err := s.store.GetStream(ctx, req.StreamID)
		if err != nil {
			return model.ClipJob{}, err
		}
		req.Name = fmt.Sprintf("%s · %s", stream.Name, req.StartTime.Local().Format("2006-01-02 15:04"))
	}
	if req.EndTime.Sub(req.StartTime) > time.Duration(s.cfg.Clips.MaxDuration) {
		return model.ClipJob{}, errors.New("clip exceeds configured maximum duration")
	}
	reserveCheck := int64(64 << 20)
	if maxOutput := int64(s.cfg.Clips.MaxOutputSize); maxOutput < reserveCheck {
		reserveCheck = maxOutput
	}
	if _, err := s.store.EnsureFreeSpace(reserveCheck, int64(s.cfg.Storage.ReservedFreeSpace)); err != nil {
		return model.ClipJob{}, fmt.Errorf("insufficient disk space for clip export: %w", err)
	}
	if req.Mode == "" {
		req.Mode = s.cfg.Clips.DefaultMode
	}
	if req.Mode != "fast" && req.Mode != "accurate" {
		return model.ClipJob{}, errors.New("mode must be fast or accurate")
	}
	if req.QualityPreset == "" {
		req.QualityPreset = "balanced"
	}
	if req.QualityPreset != "quality" && req.QualityPreset != "balanced" && req.QualityPreset != "compact" {
		return model.ClipJob{}, errors.New("qualityPreset must be quality, balanced, or compact")
	}
	if req.Resolution == "" {
		req.Resolution = "720p"
	}
	if req.Resolution != "4k" && req.Resolution != "1080p" && req.Resolution != "720p" && req.Resolution != "480p" && req.Resolution != "source" {
		return model.ClipJob{}, errors.New("resolution must be source, 4k, 1080p, 720p, or 480p")
	}
	if req.GapPolicy == "" {
		req.GapPolicy = s.cfg.Clips.DefaultGapPolicy
	}
	if req.GapPolicy != "reject" && req.GapPolicy != "skip" && req.GapPolicy != "fill" {
		return model.ClipJob{}, errors.New("gapPolicy must be reject, skip, or fill")
	}
	if req.GapPolicy == "fill" {
		if req.Mode != "accurate" {
			return model.ClipJob{}, errors.New("gapPolicy=fill requires accurate mode so generated black and silence can be encoded with the source")
		}
	}
	segments, err := s.store.Segments(ctx, req.StreamID, req.StartTime, req.EndTime)
	if err != nil {
		return model.ClipJob{}, err
	}
	if len(segments) == 0 {
		if reencode {
			return model.ClipJob{}, errors.New("original live recording is no longer available for this favorite")
		}
		return model.ClipJob{}, errors.New("there are no committed segments in the requested range")
	}
	if err = validateCoverage(segments, req); err != nil {
		return model.ClipJob{}, err
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return model.ClipJob{}, errors.New("ffmpeg is required for clip export")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return model.ClipJob{}, errors.New("ffprobe is required to validate exported clips")
	}
	s.mu.Lock()
	if len(s.queue) >= cap(s.queue) {
		s.mu.Unlock()
		return model.ClipJob{}, errors.New("clip queue is full")
	}
	if reencode {
		jobs, jobsErr := s.store.ClipJobs(ctx)
		if jobsErr != nil {
			s.mu.Unlock()
			return model.ClipJob{}, jobsErr
		}
		for _, existing := range jobs {
			if existing.Request.SourceClipID == req.SourceClipID && (existing.Status == "QUEUED" || existing.Status == "RESOLVING" || existing.Status == "RESERVED" || existing.Status == "RUNNING" || existing.Status == "VERIFYING") {
				s.mu.Unlock()
				return model.ClipJob{}, errors.New("this favorite clip is already being re-encoded")
			}
		}
	}
	job, err := s.store.CreateClipJob(ctx, req)
	if err == nil {
		s.queue <- job
		if s.events != nil {
			s.events.Publish(events.Event{Type: "clip.progress", ClipID: job.ID, Progress: 0})
		}
	}
	s.mu.Unlock()
	return job, err
}

func validateCoverage(segments []model.Segment, req model.ClipRequest) error {
	cursor := req.StartTime
	for _, seg := range segments {
		a, b := seg.StartTime, seg.EndTime
		if a.Before(req.StartTime) {
			a = req.StartTime
		}
		if b.After(req.EndTime) {
			b = req.EndTime
		}
		if a.After(cursor.Add(500*time.Millisecond)) && req.GapPolicy == "reject" {
			return fmt.Errorf("requested range contains a recording gap near %s", cursor.Format(time.RFC3339))
		}
		if b.After(cursor) {
			cursor = b
		}
	}
	if cursor.Before(req.EndTime.Add(-500*time.Millisecond)) && req.GapPolicy == "reject" {
		return fmt.Errorf("requested end is beyond the committed media frontier %s", cursor.Format(time.RFC3339))
	}
	return nil
}

func (s *Service) Run(ctx context.Context) {
	if err := s.store.FailInterruptedClipJobs(ctx); err != nil {
		s.logger.Error("could not recover clip jobs", "error", err)
	}
	jobs, err := s.store.ClipJobs(ctx)
	if err == nil {
		for i := len(jobs) - 1; i >= 0; i-- {
			if jobs[i].Status == "QUEUED" {
				select {
				case s.queue <- jobs[i]:
				default:
					s.logger.Warn("queued clip job exceeds configured queue size", "job_id", jobs[i].ID)
				}
			}
		}
	}
	workers := s.cfg.Clips.MaxConcurrentJobs
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			s.worker(ctx)
		}()
	}
	<-ctx.Done()
	s.workers.Wait()
}
func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.queue:
			if ctx.Err() != nil {
				return
			}
			current, statusErr := s.store.ClipJob(ctx, job.ID)
			if statusErr != nil || current.Status != "QUEUED" {
				continue
			}
			s.mu.Lock()
			s.running++
			jobCtx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.Clips.JobTimeout))
			s.cancel[job.ID] = cancel
			s.mu.Unlock()
			s.runJob(jobCtx, job)
			cancel()
			s.mu.Lock()
			delete(s.cancel, job.ID)
			s.running--
			s.mu.Unlock()
		}
	}
}

func (s *Service) runJob(ctx context.Context, job model.ClipJob) {
	fail := func(err error) {
		current, e := s.store.ClipJob(context.Background(), job.ID)
		if e == nil && (current.Status == "CANCELED" || current.Status == "EXPIRED") {
			return
		}
		_ = s.updateJob(context.Background(), job.ID, "FAILED", err.Error(), 0)
		s.logger.Error("clip export failed", "job_id", job.ID, "error", err)
	}
	_ = s.updateJob(ctx, job.ID, "RESOLVING", "", 0)
	var segments []model.Segment
	var err error
	segments, err = s.store.Segments(ctx, job.Request.StreamID, job.Request.StartTime, job.Request.EndTime)
	if err != nil {
		fail(err)
		return
	}
	if len(segments) == 0 {
		if job.Request.SourceClipID != "" {
			fail(errors.New("original live recording is no longer available for this favorite"))
		} else {
			fail(errors.New("there are no committed segments in the requested range"))
		}
		return
	}
	ids := make([]string, 0, len(segments))
	for _, v := range segments {
		ids = append(ids, v.ID)
	}
	if err = s.store.LeaseSegments(ctx, ids, job.ID, time.Now().UTC().Add(time.Duration(s.cfg.Clips.JobTimeout)+5*time.Minute)); err != nil {
		fail(err)
		return
	}
	defer s.store.ReleaseLeases(context.Background(), job.ID)
	_ = s.updateJob(ctx, job.ID, "RESERVED", "", 0)
	workRel := filepath.Join("tmp", "exports", job.ID)
	work := filepath.Join(s.store.Root(), workRel)
	if err = os.MkdirAll(work, 0o700); err != nil {
		fail(err)
		return
	}
	defer os.RemoveAll(work)
	var endOffset float64
	startOffset, endOffset := virtualClipOffsets(segments, job.Request, job.Request.GapPolicy == "fill")
	duration := endOffset - startOffset
	if duration <= 0 {
		fail(errors.New("selected range does not overlap committed media"))
		return
	}
	if err = s.updateJob(ctx, job.ID, "RUNNING", "", 0); err != nil {
		fail(err)
		return
	}
	tmpOut := filepath.Join(work, "clip.tmp.mp4")
	if job.Request.Mode == "fast" {
		playlist := filepath.Join(work, "source.m3u8")
		if err = s.writeSourcePlaylist(ctx, playlist, work, segments, job.Request); err != nil {
			fail(err)
			return
		}
		args := []string{"-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,crypto,data", "-allowed_extensions", "ALL", "-i", playlist, "-ss", fmt.Sprintf("%.6f", startOffset), "-t", fmt.Sprintf("%.6f", duration), "-c", "copy", "-avoid_negative_ts", "make_zero", "-fs", fmt.Sprint(int64(s.cfg.Clips.MaxOutputSize)), "-f", "mp4", tmpOut}
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		cmd.Dir = work
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work}
		output, runErr := cmd.CombinedOutput()
		if runErr != nil {
			fail(fmt.Errorf("ffmpeg: %w: %s", runErr, strings.TrimSpace(string(output))))
			return
		}
	} else {
		lastProgressAt := time.Time{}
		lastProgress := 0.0
		reportProgress := func(fraction float64) {
			fraction = math.Max(0, math.Min(1, fraction))
			progress := fraction * 0.96
			now := time.Now()
			if progress-lastProgress < 0.005 && now.Sub(lastProgressAt) < time.Second {
				return
			}
			if s.updateJob(ctx, job.ID, "RUNNING", "", progress) == nil {
				lastProgress = progress
				lastProgressAt = now
			}
		}
		if err = s.exportAccurate(ctx, work, tmpOut, segments, job.Request, reportProgress); err != nil {
			fail(err)
			return
		}
	}
	if err = s.updateJob(ctx, job.ID, "VERIFYING", "", 0.98); err != nil {
		fail(err)
		return
	}
	if err = validateMP4(ctx, tmpOut, duration, job.Request.Mode, time.Duration(s.cfg.Media.TargetSegmentDuration)); err != nil {
		fail(err)
		return
	}
	fi, err := os.Stat(tmpOut)
	if err != nil {
		fail(err)
		return
	}
	if fi.Size() > int64(s.cfg.Clips.MaxOutputSize) {
		fail(errors.New("export exceeds configured output size limit"))
		return
	}
	if err = ctx.Err(); err != nil {
		fail(err)
		return
	}
	rel := filepath.Join("clips", "completed", job.ID+".mp4")
	dst := filepath.Join(s.store.Root(), rel)
	if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		fail(err)
		return
	}
	if err = os.Rename(tmpOut, dst); err != nil {
		fail(err)
		return
	}
	if d, e := os.Open(filepath.Dir(dst)); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	expires := time.Time{}
	if !job.Request.SaveToLibrary {
		expires = time.Now().UTC().Add(time.Duration(s.cfg.Clips.TemporaryFileRetention))
	}
	if job.Request.SourceClipID != "" {
		oldPath, replaceErr := s.store.ReplaceFavoriteClip(ctx, job.Request.SourceClipID, job.ID, rel, fi.Size(), job.Request)
		if replaceErr != nil {
			_ = os.Remove(dst)
			fail(replaceErr)
			return
		}
		oldPath = filepath.Clean(oldPath)
		if oldPath != rel && !filepath.IsAbs(oldPath) && oldPath != ".." && !strings.HasPrefix(oldPath, ".."+string(os.PathSeparator)) {
			_ = os.Remove(filepath.Join(s.store.Root(), oldPath))
		}
	} else if err = s.store.CompleteClip(ctx, job.ID, rel, fi.Size(), job.Request.SaveToLibrary, expires); err != nil {
		_ = os.Remove(dst)
		fail(err)
		return
	}
	if s.events != nil {
		completedID := job.ID
		if job.Request.SourceClipID != "" {
			completedID = job.Request.SourceClipID
		}
		s.events.Publish(events.Event{Type: "clip.completed", ClipID: completedID, Progress: 1})
	}
	s.logger.Info("clip exported", "job_id", job.ID, "size_bytes", fi.Size())
}

func (s *Service) updateJob(ctx context.Context, id, status, message string, progress float64) error {
	err := s.store.UpdateClipJob(ctx, id, status, message, progress)
	if err == nil && s.events != nil {
		s.events.Publish(events.Event{Type: "clip.progress", ClipID: id, Progress: progress})
	}
	return err
}

func (s *Service) writeSourcePlaylist(ctx context.Context, file, work string, segments []model.Segment, req model.ClipRequest) error {
	var b strings.Builder
	max := 2.0
	for _, v := range segments {
		if d := v.EndTime.Sub(v.StartTime).Seconds(); d > max {
			max = d
		}
	}
	if req.GapPolicy == "fill" {
		if d := segments[0].StartTime.Sub(req.StartTime).Seconds(); d > max {
			max = d
		}
		if d := req.EndTime.Sub(segments[len(segments)-1].EndTime).Seconds(); d > max {
			max = d
		}
		for i := 1; i < len(segments); i++ {
			if gap := segments[i].StartTime.Sub(segments[i-1].EndTime); gap > 50*time.Millisecond && gap.Seconds() > max {
				max = gap.Seconds()
			}
		}
	}
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", int(math.Ceil(max)), segments[0].Sequence)
	lastEpoch := ""
	firstStart := segments[0].StartTime
	if req.GapPolicy == "fill" && req.StartTime.Before(firstStart) {
		if err := s.appendFiller(ctx, &b, work, "leading", firstStart.Sub(req.StartTime)); err != nil {
			return err
		}
		b.WriteString("#EXT-X-DISCONTINUITY\n")
	}
	for i, v := range segments {
		if i > 0 {
			previous := segments[i-1]
			gap := v.StartTime.Sub(previous.EndTime)
			if gap > 50*time.Millisecond && req.GapPolicy == "fill" {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
				if err := s.appendFiller(ctx, &b, work, fmt.Sprintf("gap-%d", i), gap); err != nil {
					return err
				}
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			} else if v.EpochID != lastEpoch || v.SessionID != previous.SessionID || gap > 50*time.Millisecond {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
		}
		if v.EpochID != lastEpoch {
			init, err := s.store.EpochInitPath(ctx, v.EpochID)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", fileURI(filepath.Join(s.store.Root(), init)))
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", v.EndTime.Sub(v.StartTime).Seconds(), fileURI(s.store.SegmentPath(v)))
		lastEpoch = v.EpochID
	}
	lastEnd := segments[len(segments)-1].EndTime
	if req.GapPolicy == "fill" && req.EndTime.After(lastEnd) {
		b.WriteString("#EXT-X-DISCONTINUITY\n")
		if err := s.appendFiller(ctx, &b, work, "trailing", req.EndTime.Sub(lastEnd)); err != nil {
			return err
		}
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return os.WriteFile(file, []byte(b.String()), 0o600)
}

func (s *Service) appendFiller(ctx context.Context, playlist *strings.Builder, work, name string, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	file := filepath.Join(work, "gap-"+name+".ts")
	seconds := fmt.Sprintf("%.6f", duration.Seconds())
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=1280x720:r=30",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
		"-t", seconds, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-pix_fmt", "yuv420p", "-g", "30", "-sc_threshold", "0", "-c:a", "aac", "-b:a", "128k",
		"-f", "mpegts", file,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = work
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("generate gap filler: %w: %s", err, strings.TrimSpace(string(output)))
	}
	fmt.Fprintf(playlist, "#EXTINF:%.3f,\n%s\n", duration.Seconds(), fileURI(file))
	return nil
}
func fileURI(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

func (s *Service) exportAccurate(ctx context.Context, work, output string, segments []model.Segment, req model.ClipRequest, reportProgress func(float64)) error {
	width, height, err := s.outputResolution(ctx, segments[0], req.Resolution)
	if err != nil {
		return err
	}
	groups := make([][]model.Segment, 0, len(segments))
	for _, seg := range segments {
		if len(groups) == 0 {
			groups = append(groups, []model.Segment{seg})
			continue
		}
		current := groups[len(groups)-1]
		previous := current[len(current)-1]
		if seg.EpochID != previous.EpochID || seg.SessionID != previous.SessionID || seg.StartTime.Sub(previous.EndTime) > 50*time.Millisecond {
			groups = append(groups, []model.Segment{seg})
		} else {
			groups[len(groups)-1] = append(current, seg)
		}
	}

	files := make([]string, 0, len(groups)*2+2)
	maxOutput := int64(s.cfg.Clips.MaxOutputSize)
	var temporaryBytes int64
	var totalEncodeDuration float64
	progressCursor := req.StartTime
	for _, group := range groups {
		groupStart := maxTime(req.StartTime, group[0].StartTime)
		groupStart = maxTime(groupStart, progressCursor)
		groupEnd := minTime(req.EndTime, group[len(group)-1].EndTime)
		if groupEnd.After(groupStart) {
			totalEncodeDuration += groupEnd.Sub(groupStart).Seconds()
			progressCursor = groupEnd
		}
	}
	var encodedDuration float64
	cursor := req.StartTime
	partIndex := 0
	addFiller := func(duration time.Duration) error {
		if duration <= 50*time.Millisecond {
			return nil
		}
		name := fmt.Sprintf("part-%03d.mp4", partIndex)
		partIndex++
		path := filepath.Join(work, name)
		if err := s.generateFillerMP4(ctx, path, duration, req.QualityPreset, width, height, maxOutput); err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		temporaryBytes += info.Size()
		if temporaryBytes > maxOutput {
			return errors.New("export exceeds configured output size limit")
		}
		files = append(files, name)
		return nil
	}

	for groupIndex, group := range groups {
		groupStart := group[0].StartTime
		groupEnd := group[len(group)-1].EndTime
		clipStart := maxTime(req.StartTime, groupStart)
		clipStart = maxTime(clipStart, cursor)
		clipEnd := minTime(req.EndTime, groupEnd)
		if !clipEnd.After(clipStart) {
			continue
		}
		if clipStart.After(cursor) && req.GapPolicy == "fill" {
			if err := addFiller(clipStart.Sub(cursor)); err != nil {
				return err
			}
		}

		playlist := filepath.Join(work, fmt.Sprintf("source-%03d.m3u8", groupIndex))
		playlistReq := req
		playlistReq.GapPolicy = "skip"
		if err := s.writeSourcePlaylist(ctx, playlist, work, group, playlistReq); err != nil {
			return err
		}
		partName := fmt.Sprintf("part-%03d.mp4", partIndex)
		partIndex++
		partPath := filepath.Join(work, partName)
		duration := clipEnd.Sub(clipStart).Seconds()
		offset := clipStart.Sub(groupStart).Seconds()
		audioScale, err := s.store.EpochAudioTimescale(ctx, group[0].EpochID)
		if err != nil {
			return err
		}
		if err = s.encodeAccurateGroup(ctx, work, playlist, partPath, offset, duration, audioScale > 0, req.QualityPreset, width, height, maxOutput, func(fraction float64) {
			if totalEncodeDuration > 0 {
				reportProgress((encodedDuration + duration*fraction) / totalEncodeDuration)
			}
		}); err != nil {
			return err
		}
		encodedDuration += duration
		info, err := os.Stat(partPath)
		if err != nil {
			return err
		}
		temporaryBytes += info.Size()
		if temporaryBytes > maxOutput {
			return errors.New("export exceeds configured output size limit")
		}
		files = append(files, partName)
		cursor = clipEnd
	}
	if req.GapPolicy == "fill" && req.EndTime.After(cursor) {
		if err := addFiller(req.EndTime.Sub(cursor)); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return errors.New("selected range does not overlap committed media")
	}
	if len(files) == 1 {
		return os.Rename(filepath.Join(work, files[0]), output)
	}
	var concat strings.Builder
	for _, name := range files {
		fmt.Fprintf(&concat, "file '%s'\n", name)
	}
	concatPath := filepath.Join(work, "parts.concat")
	if err := os.WriteFile(concatPath, []byte(concat.String()), 0o600); err != nil {
		return err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", concatPath, "-c", "copy", "-movflags", "+faststart", "-fs", fmt.Sprint(maxOutput), "-f", "mp4", output}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = work
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work}
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("concatenate accurate clip parts: %w: %s", err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func (s *Service) encodeAccurateGroup(ctx context.Context, work, playlist, output string, offset, duration float64, hasAudio bool, qualityPreset string, width, height int, maxOutput int64, reportProgress func(float64)) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,crypto,data", "-allowed_extensions", "ALL", "-i", playlist}
	audioInput := "0:a:0"
	if !hasAudio {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo")
		audioInput = "1:a:0"
	}
	videoFilter := fmt.Sprintf("[0:v:0]trim=start=%.6f:duration=%.6f,setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,fps=30[v]", offset, duration, width, height, width, height)
	audioFilter := fmt.Sprintf("[%s]atrim=start=%.6f:duration=%.6f,asetpts=PTS-STARTPTS,aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo[a]", audioInput, offset, duration)
	crf, preset := accurateEncodingSettings(qualityPreset)
	args = append(args, "-filter_complex", videoFilter+";"+audioFilter, "-map", "[v]", "-map", "[a]", "-c:v", "libx264", "-preset", preset, "-crf", crf, "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2", "-movflags", "+faststart", "-fs", fmt.Sprint(maxOutput), "-progress", "pipe:1", "-nostats", "-f", "mp4", output)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = work
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work}
	return runFFmpegWithProgress(ctx, cmd, duration, reportProgress)
}

func accurateEncodingSettings(qualityPreset string) (crf, preset string) {
	switch qualityPreset {
	case "quality":
		return "18", "medium"
	case "compact":
		return "27", "veryfast"
	default:
		return "23", "veryfast"
	}
}

func accurateResolution(resolution string) (width, height int) {
	switch resolution {
	case "4k":
		return 3840, 2160
	case "1080p":
		return 1920, 1080
	case "480p":
		return 854, 480
	case "source":
		return 0, 0
	default:
		return 1280, 720
	}
}

func probeMediaDuration(ctx context.Context, path string) (float64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("read source clip duration: %w", err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return 0, errors.New("could not read source clip duration")
	}
	return duration, nil
}

func probeVideoResolution(ctx context.Context, path string) (int, int, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=s=x:p=0", path)
	output, err := cmd.Output()
	if err != nil {
		return 0, 0, fmt.Errorf("read source video resolution: %w", err)
	}
	var width, height int
	if _, err = fmt.Sscanf(strings.TrimSpace(string(output)), "%dx%d", &width, &height); err != nil || width < 1 || height < 1 {
		return 0, 0, errors.New("could not read source video resolution")
	}
	return width, height, nil
}

func (s *Service) exportFavoriteAccurate(ctx context.Context, input, output string, req model.ClipRequest, reportProgress func(float64)) error {
	width, height := accurateResolution(req.Resolution)
	if req.Resolution == "source" {
		var err error
		width, height, err = probeVideoResolution(ctx, input)
		if err != nil {
			return err
		}
	}
	width -= width % 2
	height -= height % 2
	if width < 2 || height < 2 {
		return errors.New("source video resolution is too small to encode")
	}
	crf, preset := accurateEncodingSettings(req.QualityPreset)
	videoFilter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,fps=30", width, height, width, height)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-i", input,
		"-map", "0:v:0", "-map", "0:a:0?", "-vf", videoFilter,
		"-c:v", "libx264", "-preset", preset, "-crf", crf, "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2",
		"-movflags", "+faststart", "-fs", fmt.Sprint(int64(s.cfg.Clips.MaxOutputSize)),
		"-progress", "pipe:1", "-nostats", "-f", "mp4", output,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = filepath.Dir(output)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(output)}
	duration, err := probeMediaDuration(ctx, input)
	if err != nil {
		return err
	}
	return runFFmpegWithProgress(ctx, cmd, duration, reportProgress)
}

func (s *Service) exportFavoriteFast(ctx context.Context, input, output string, duration float64, reportProgress func(float64)) error {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-i", input,
		"-map", "0:v:0", "-map", "0:a?", "-c", "copy", "-movflags", "+faststart",
		"-fs", fmt.Sprint(int64(s.cfg.Clips.MaxOutputSize)),
		"-progress", "pipe:1", "-nostats", "-f", "mp4", output,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = filepath.Dir(output)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(output)}
	return runFFmpegWithProgress(ctx, cmd, duration, reportProgress)
}

func (s *Service) outputResolution(ctx context.Context, first model.Segment, resolution string) (int, int, error) {
	if resolution != "source" {
		width, height := accurateResolution(resolution)
		return width, height, nil
	}
	path := s.store.SegmentPath(first)
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=s=x:p=0", path)
	output, err := cmd.Output()
	if err != nil {
		return 0, 0, fmt.Errorf("read source video resolution: %w", err)
	}
	var width, height int
	if _, err = fmt.Sscanf(strings.TrimSpace(string(output)), "%dx%d", &width, &height); err != nil || width < 1 || height < 1 {
		return 0, 0, errors.New("could not read source video resolution")
	}
	return width, height, nil
}

func runFFmpegWithProgress(ctx context.Context, cmd *exec.Cmd, duration float64, reportProgress func(float64)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	stderrDone := make(chan []byte, 1)
	go func() {
		output, _ := io.ReadAll(stderr)
		stderrDone <- output
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok || key != "out_time_us" || reportProgress == nil || duration <= 0 {
			continue
		}
		microseconds, parseErr := strconv.ParseFloat(value, 64)
		if parseErr == nil {
			reportProgress(microseconds / (duration * 1_000_000))
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	stderrOutput := <-stderrDone
	if scanErr != nil {
		return fmt.Errorf("read ffmpeg progress: %w", scanErr)
	}
	if waitErr != nil {
		return fmt.Errorf("run ffmpeg clip export: %w: %s", waitErr, strings.TrimSpace(string(stderrOutput)))
	}
	if reportProgress != nil {
		reportProgress(1)
	}
	return nil
}

func (s *Service) generateFillerMP4(ctx context.Context, output string, duration time.Duration, qualityPreset string, width, height int, maxOutput int64) error {
	crf, preset := accurateEncodingSettings(qualityPreset)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:r=30", width, height),
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
		"-t", fmt.Sprintf("%.6f", duration.Seconds()), "-vf", "fps=30",
		"-c:v", "libx264", "-preset", preset, "-crf", crf, "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2", "-movflags", "+faststart",
		"-fs", fmt.Sprint(maxOutput), "-f", "mp4", output,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = filepath.Dir(output)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(output)}
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("generate black/silence filler: %w: %s", err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func virtualClipOffsets(segments []model.Segment, req model.ClipRequest, fillGaps bool) (float64, float64) {
	if len(segments) == 0 {
		return 0, req.EndTime.Sub(req.StartTime).Seconds()
	}
	first := segments[0].StartTime
	mapTime := func(target time.Time) float64 {
		elapsed := 0.0
		if fillGaps && req.StartTime.Before(first) {
			prefixEnd := first
			if target.Before(prefixEnd) {
				if target.Before(req.StartTime) {
					return 0
				}
				return target.Sub(req.StartTime).Seconds()
			}
			elapsed = prefixEnd.Sub(req.StartTime).Seconds()
		}
		for i, seg := range segments {
			if i > 0 {
				gapStart, gapEnd := segments[i-1].EndTime, seg.StartTime
				if fillGaps && gapEnd.Sub(gapStart) > 50*time.Millisecond {
					if target.Before(gapEnd) {
						if target.After(gapStart) {
							return elapsed + target.Sub(gapStart).Seconds()
						}
						return elapsed
					}
					elapsed += gapEnd.Sub(gapStart).Seconds()
				}
			}
			if target.Before(seg.StartTime) {
				return elapsed
			}
			if !target.After(seg.EndTime) {
				return elapsed + target.Sub(seg.StartTime).Seconds()
			}
			elapsed += seg.EndTime.Sub(seg.StartTime).Seconds()
		}
		lastEnd := segments[len(segments)-1].EndTime
		if fillGaps && target.After(lastEnd) {
			elapsed += target.Sub(lastEnd).Seconds()
		}
		return elapsed
	}
	return mapTime(req.StartTime), mapTime(req.EndTime)
}

func validateMP4(ctx context.Context, file string, expectedDuration float64, mode string, targetSegmentDuration time.Duration) error {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", file)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("ffprobe could not read exported MP4: %w", err)
	}
	var data struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err = json.Unmarshal(out, &data); err != nil {
		return err
	}
	video, audio := false, false
	for _, v := range data.Streams {
		if v.CodecType == "video" {
			video = true
		}
		if v.CodecType == "audio" {
			audio = true
		}
	}
	if !video {
		return errors.New("export contains no video stream")
	}
	if len(data.Streams) == 0 {
		return errors.New("export contains no media streams")
	}
	if data.Format.Duration == "" {
		return errors.New("export duration is missing")
	}
	var actualDuration float64
	if _, err = fmt.Sscan(data.Format.Duration, &actualDuration); err != nil || actualDuration <= 0 {
		return errors.New("export duration is invalid")
	}
	if err = validateExportDuration(actualDuration, expectedDuration, mode, targetSegmentDuration); err != nil {
		return err
	}
	if mode == "accurate" && !audio {
		return errors.New("accurate export contains no audio stream")
	}
	return nil
}

func validateExportDuration(actual, expected float64, mode string, targetSegmentDuration time.Duration) error {
	if actual <= 0 || expected <= 0 {
		return errors.New("export duration is invalid")
	}
	tolerance := 0.5
	if mode == "fast" {
		tolerance = math.Max(2, targetSegmentDuration.Seconds()*1.5)
	}
	if actual < expected-tolerance {
		return fmt.Errorf("export duration %.3fs is shorter than expected %.3fs (tolerance %.3fs)", actual, expected, tolerance)
	}
	if mode == "accurate" && math.Abs(actual-expected) > tolerance {
		return fmt.Errorf("accurate export duration %.3fs differs from expected %.3fs (tolerance %.3fs)", actual, expected, tolerance)
	}
	return nil
}

func (s *Service) Cancel(ctx context.Context, id string) error {
	s.mu.Lock()
	cancel := s.cancel[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := s.store.PurgeClipJob(ctx, id); err != nil {
		return err
	}
	if s.events != nil {
		s.events.Publish(events.Event{Type: "clip.progress", ClipID: id})
	}
	return nil
}
