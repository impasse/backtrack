package ingest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"backtrack/internal/config"
	"backtrack/internal/events"
	"backtrack/internal/media"
	"backtrack/internal/model"
	"backtrack/internal/storage"
	mp4av1 "github.com/Eyevinn/mp4ff/av1"
	mp4ff "github.com/Eyevinn/mp4ff/mp4"
	"github.com/abema/go-mp4"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/codecs"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	mediopus "github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/google/uuid"
)

type Server struct {
	addr               string
	cfg                config.Config
	store              *storage.Store
	logger             *slog.Logger
	events             *events.Hub
	mu                 sync.Mutex
	active             map[string]bool
	connections        map[net.Conn]struct{}
	bufferedSamples    map[string]int64
	webrtcSessions     map[string]func()
	handlers           sync.WaitGroup
	listener           net.Listener
	rateMu             sync.Mutex
	rateWindowStart    time.Time
	rateWindowBytes    int64
	ingestBitsPerSec   atomic.Int64
	commitLatencyNS    atomic.Int64
	onSegmentCommitted func(model.Segment)
}

type observedConn struct {
	gortmplib.Conn
	observe      func(message.Message)
	consumeAudio bool
}

func (c observedConn) Read() (message.Message, error) {
	m, err := c.Conn.Read()
	if err == nil && c.observe != nil {
		c.observe(m)
	}
	if err == nil && c.consumeAudio {
		switch m.(type) {
		case *message.Audio, *message.AudioExSequenceStart, *message.AudioExCodedFrames, *message.AudioExSequenceEnd:
			// gortmplib's Reader has a fixed track map after Initialize. Audio
			// tracks introduced later are handled by the observer and consumed
			// here instead of being rejected as an unknown track.
			return &message.SetChunkSize{}, nil
		}
	}
	return m, err
}

func New(addr string, cfg config.Config, store *storage.Store, logger *slog.Logger, eventHub *events.Hub) *Server {
	return &Server{addr: addr, cfg: cfg, store: store, logger: logger, events: eventHub, active: map[string]bool{}, connections: map[net.Conn]struct{}{}, bufferedSamples: map[string]int64{}, webrtcSessions: map[string]func(){}}
}

func (s *Server) SetSegmentCommittedHandler(handler func(model.Segment)) {
	s.onSegmentCommitted = handler
}

func (s *Server) IsActive(id string) bool { s.mu.Lock(); defer s.mu.Unlock(); return s.active[id] }

func (s *Server) ActivePublishers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

func (s *Server) setBufferedSamples(streamID string, count int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if count <= 0 {
		delete(s.bufferedSamples, streamID)
		return
	}
	s.bufferedSamples[streamID] = count
}

func (s *Server) PacketQueueDepth() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, count := range s.bufferedSamples {
		total += count
	}
	return total
}

func (s *Server) observeIngestBytes(n int) {
	if n <= 0 {
		return
	}
	now := time.Now()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rateWindowStart.IsZero() || now.Sub(s.rateWindowStart) >= 5*time.Second {
		s.rateWindowStart = now
		s.rateWindowBytes = 0
	}
	s.rateWindowBytes += int64(n)
	elapsed := now.Sub(s.rateWindowStart).Seconds()
	if elapsed > 0 {
		s.ingestBitsPerSec.Store(int64(float64(s.rateWindowBytes*8) / elapsed))
	}
}

func (s *Server) IngestBitsPerSecond() int64 {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rateWindowStart.IsZero() || time.Since(s.rateWindowStart) >= 5*time.Second {
		return 0
	}
	return s.ingestBitsPerSec.Load()
}

func (s *Server) SegmentCommitLatency() time.Duration {
	return time.Duration(s.commitLatencyNS.Load())
}

func (s *Server) StorageWriteLatency() time.Duration {
	return s.store.MediaWriteLatency()
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.listener = ln
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		s.closeConnections()
		s.CloseWebRTC()
	}()
	s.logger.Info("RTMP listener ready", "addr", s.addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.closeConnections()
				s.handlers.Wait()
				return nil
			}
			s.closeConnections()
			s.handlers.Wait()
			return err
		}
		s.mu.Lock()
		s.connections[conn] = struct{}{}
		s.handlers.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.handlers.Done()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) closeConnections() {
	s.mu.Lock()
	connections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.connections, conn)
		s.mu.Unlock()
	}()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Duration(s.cfg.Ingest.ReadTimeout)))
	sc := &gortmplib.ServerConn{RW: conn}
	if err := sc.Initialize(); err != nil {
		s.logger.Warn("RTMP handshake failed", "error", err)
		return
	}
	if err := sc.AcceptConn(); err != nil {
		s.logger.Warn("RTMP negotiation failed", "error", err)
		return
	}
	if !sc.Publish || sc.URL == nil {
		s.logger.Warn("RTMP playback is not accepted on the ingest port")
		return
	}
	streamID, secret, err := publishCredentials(sc.URL)
	if err != nil {
		s.logger.Warn("invalid RTMP publish URL", "error", err)
		_ = sc.RejectAction()
		return
	}
	ok, err := s.store.AuthenticatePublish(ctx, streamID, secret)
	if err != nil || !ok {
		s.logger.Warn("unauthorized RTMP publisher", "stream_id", streamID)
		_ = sc.RejectAction()
		return
	}
	s.mu.Lock()
	if s.active[streamID] {
		s.mu.Unlock()
		s.logger.Warn("duplicate publisher rejected", "stream_id", streamID)
		_ = sc.RejectAction()
		return
	}
	s.active[streamID] = true
	s.mu.Unlock()
	if s.events != nil {
		s.events.Publish(events.Event{Type: "stream.started", StreamID: streamID})
	}
	defer func() {
		s.mu.Lock()
		delete(s.active, streamID)
		s.mu.Unlock()
		if s.events != nil {
			s.events.Publish(events.Event{Type: "stream.stopped", StreamID: streamID})
		}
	}()
	if err = sc.AcceptAction(); err != nil {
		return
	}
	if err = s.record(ctx, streamID, sc, func() { _ = conn.Close() }, func(deadline time.Time) error { return conn.SetDeadline(deadline) }); err != nil {
		s.logger.Warn("RTMP recording ended", "stream_id", streamID, "error", err)
	}
}

// The OBS server URL is rtmp://host:1935/live and the stream key is <stream-id>?key=<one-time-key>.
func publishCredentials(u *url.URL) (string, string, error) {
	parts := strings.Split(strings.Trim(path.Clean(u.Path), "/"), "/")
	if len(parts) != 2 || parts[0] != "live" {
		return "", "", errors.New("expected /live/<stream-id>")
	}
	key := u.Query().Get("key")
	if key == "" {
		return "", "", errors.New("publish key is required")
	}
	return parts[1], key, nil
}

func (s *Server) record(ctx context.Context, streamID string, conn gortmplib.Conn, closeIngest func(), setDeadline func(time.Time) error) error {
	var av1Initial *media.AV1Config
	r := &gortmplib.Reader{Conn: observedConn{Conn: conn, observe: func(msg message.Message) {
		if sequence, ok := msg.(*message.VideoExSequenceStart); ok && sequence.FourCC == message.FourCCAV1 && sequence.AV1Config != nil {
			av1Initial, _ = av1ConfigFromSequence(sequence.AV1Config)
		}
	}}}
	if err := r.Initialize(); err != nil {
		return err
	}
	var video *media.VideoConfig
	var audio any
	for _, track := range r.Tracks() {
		switch c := track.Codec.(type) {
		case *codecs.H264:
			if video != nil {
				return errors.New("multiple video tracks are unsupported")
			}
			video = &media.VideoConfig{H264: c}
		case *codecs.H265:
			if video != nil {
				return errors.New("multiple video tracks are unsupported")
			}
			video = &media.VideoConfig{H265: c}
		case *codecs.AV1:
			if video != nil {
				return errors.New("multiple video tracks are unsupported")
			}
			if av1Initial == nil {
				return errors.New("AV1 sequence header is missing or invalid")
			}
			video = &media.VideoConfig{AV1: av1Initial}
		case *codecs.MPEG4Audio:
			if audio != nil {
				return errors.New("multiple AAC tracks are unsupported")
			}
			audio = c
		case *codecs.Opus:
			if audio != nil {
				return errors.New("multiple audio tracks are unsupported")
			}
			audio = c
		default:
			return fmt.Errorf("unsupported ingest codec %T; accepted video formats are H.264, HEVC, and AV1, with AAC-LC or Opus audio", track.Codec)
		}
	}
	if video == nil {
		return errors.New("stream must include H.264, HEVC, or AV1 video")
	}
	if video.AV1 != nil && (video.AV1.Record == nil || video.AV1.Width == 0 || video.AV1.Height == 0) {
		return errors.New("AV1 sequence header is missing or invalid")
	}
	now := time.Now().UTC()
	keyframeDeadline := now.Add(time.Duration(s.cfg.Media.KeyframeWaitTimeout))
	session, err := s.store.BeginSession(ctx, streamID, time.Duration(s.cfg.Ingest.ReconnectGrace))
	if err != nil {
		return err
	}
	// The RTMP listener closes publisher sockets on shutdown. Let the recorder
	// finish its current segment and persist the final epoch/session state even
	// though the listener context has been canceled.
	recordCtx := context.WithoutCancel(ctx)
	defer s.setBufferedSamples(streamID, 0)
	if session.EndedAt != nil && now.After(*session.EndedAt) {
		gapStart := *session.EndedAt
		previousSegments, segmentErr := s.store.SessionSegments(recordCtx, session.ID)
		if segmentErr != nil {
			return segmentErr
		}
		if len(previousSegments) > 0 && previousSegments[len(previousSegments)-1].EndTime.After(gapStart) {
			gapStart = previousSegments[len(previousSegments)-1].EndTime
		}
		if now.After(gapStart) {
			if gapErr := s.store.CreateGap(recordCtx, model.Gap{StreamID: streamID, SessionID: session.ID, StartTime: gapStart, EndTime: now, Reason: "stream_disconnected"}); gapErr != nil {
				return gapErr
			}
		}
	}
	var callbackErr error
	var keyframeSeen bool
	var epoch *model.Epoch
	var muxer *media.Muxer
	var epochClockAnchor time.Time
	currentVideo := cloneVideo(video)
	currentAudio := cloneAudio(audio)
	audioCallbackRegistered := audio != nil
	dynamicAudio := false
	var pendingVideo *media.VideoConfig
	var pendingAudio any
	codecChangePending := false
	var lastVideoDTS time.Duration
	var rtmpClock rtmpTimestampUnwrapper
	var bitrateWindowStart time.Time
	var bitrateWindowBytes int64
	maxWindowBytes := int64(s.cfg.Ingest.MaxBitrateMbps) * 1_000_000 * 5 / 8
	accountBytes := func(n int) error {
		if n <= 0 {
			return nil
		}
		s.observeIngestBytes(n)
		now := time.Now()
		if bitrateWindowStart.IsZero() || now.Sub(bitrateWindowStart) >= 5*time.Second {
			bitrateWindowStart = now
			bitrateWindowBytes = 0
		}
		bitrateWindowBytes += int64(n)
		if bitrateWindowBytes > maxWindowBytes {
			return fmt.Errorf("ingest bitrate exceeded configured limit of %d Mbps", s.cfg.Ingest.MaxBitrateMbps)
		}
		return nil
	}
	commit := func(seg *model.Segment, b []byte) {
		if callbackErr != nil || seg == nil {
			return
		}
		commitStarted := time.Now()
		var committed model.Segment
		committed, callbackErr = s.store.Commit(recordCtx, storage.PendingSegment{Segment: *seg, Data: b, ReservedFreeSpace: int64(s.cfg.Storage.ReservedFreeSpace)})
		commitLatency := time.Since(commitStarted)
		s.commitLatencyNS.Store(commitLatency.Nanoseconds())
		if callbackErr != nil {
			if s.events != nil {
				stats, _ := s.store.Stats(recordCtx)
				s.events.Publish(events.Event{Type: "storage.warning", FreeBytes: stats.FreeBytes})
			}
			closeIngest()
			return
		}
		if s.onSegmentCommitted != nil {
			s.onSegmentCommitted(committed)
		}
		if s.events != nil {
			s.events.Publish(events.Event{Type: "segment.committed", StreamID: streamID})
			s.events.Publish(events.Event{Type: "timeline.updated", StreamID: streamID})
		}
	}
	openEpoch := func(videoConfig *media.VideoConfig, audioConfig any, anchor, startedAt time.Time) error {
		m, next, freeBytes, openErr := s.startMediaEpoch(recordCtx, streamID, session.ID, anchor, startedAt, videoConfig, audioConfig)
		if openErr != nil {
			if s.events != nil && freeBytes > 0 {
				s.events.Publish(events.Event{Type: "storage.warning", FreeBytes: freeBytes})
			}
			return openErr
		}
		muxer = m
		epoch = &next
		epochClockAnchor = anchor
		s.setBufferedSamples(streamID, 0)
		return nil
	}
	closeEpoch := func(endedAt time.Time) error {
		if muxer != nil {
			seg, data, closeErr := muxer.Close()
			if closeErr != nil {
				return closeErr
			}
			commit(seg, data)
			if callbackErr != nil {
				return callbackErr
			}
			muxer = nil
			s.setBufferedSamples(streamID, 0)
		}
		if epoch != nil {
			if endErr := s.store.EndEpoch(recordCtx, epoch.ID, endedAt); endErr != nil {
				return endErr
			}
			epoch = nil
		}
		return nil
	}
	effectiveVideo := func() *media.VideoConfig {
		if codecChangePending {
			return pendingVideo
		}
		return currentVideo
	}
	effectiveAudio := func() any {
		if codecChangePending {
			return pendingAudio
		}
		return currentAudio
	}
	queueCodecChange := func(videoConfig *media.VideoConfig, audioConfig any) error {
		if !codecChangePending {
			oldEnd := epochClockAnchor.Add(lastVideoDTS + time.Second/30)
			if epoch != nil && oldEnd.Before(epoch.StartedAt) {
				oldEnd = epoch.StartedAt
			}
			if closeErr := closeEpoch(oldEnd); closeErr != nil {
				return closeErr
			}
		}
		pendingVideo = cloneVideo(videoConfig)
		pendingAudio = cloneAudio(audioConfig)
		codecChangePending = true
		return nil
	}
	startPendingEpoch := func(at time.Time) error {
		if !codecChangePending || pendingVideo == nil {
			return errors.New("codec change is waiting for valid video parameters")
		}
		videoConfig, audioConfig := cloneVideo(pendingVideo), cloneAudio(pendingAudio)
		epochAnchor, startedAt := mediaEpochClock(now, lastVideoDTS, at.Sub(now))
		if openErr := openEpoch(videoConfig, audioConfig, epochAnchor, startedAt); openErr != nil {
			return openErr
		}
		currentVideo, currentAudio = videoConfig, audioConfig
		pendingVideo, pendingAudio = nil, nil
		codecChangePending = false
		return nil
	}
	if err = openEpoch(currentVideo, currentAudio, now, now); err != nil {
		_ = s.store.EndSession(recordCtx, session.ID)
		return err
	}
	processVideo := func(codec string, pts, dts time.Duration, au [][]byte) {
		if callbackErr != nil {
			return
		}
		packetBytes := 0
		isKeyframe := false
		hasVCL := false
		for _, nalu := range au {
			packetBytes += len(nalu) + 4
			if len(nalu) > 0 {
				if codec == "H.264" {
					typ := nalu[0] & 0x1f
					hasVCL = hasVCL || (typ >= 1 && typ <= 5)
					isKeyframe = isKeyframe || typ == 5
				} else if codec == "HEVC" {
					typ := h265.NALUType((nalu[0] >> 1) & 0x3f)
					hasVCL = hasVCL || typ <= 31
					isKeyframe = isKeyframe || h265.IsRandomAccess([][]byte{nalu})
				} else {
					hasVCL = true
				}
			}
		}
		if codec == "AV1" {
			isKeyframe = effectiveVideo().IsAV1RandomAccess(au)
		}
		if isKeyframe {
			keyframeSeen = true
		}
		if callbackErr = accountBytes(packetBytes); callbackErr != nil {
			closeIngest()
			return
		}
		unwrappedDTS := rtmpClock.Unwrap(dts)
		pts += unwrappedDTS - dts
		dts = unwrappedDTS
		baseVideo, baseAudio := effectiveVideo(), effectiveAudio()
		updatedVideo, changed := videoConfigFromAccessUnit(baseVideo, codec, au)
		if changed || (hasVCL && muxer != nil && dts < lastVideoDTS) {
			if !changed {
				updatedVideo = baseVideo
			}
			if switchErr := queueCodecChange(updatedVideo, baseAudio); switchErr != nil {
				callbackErr = switchErr
				closeIngest()
				return
			}
		}
		if muxer == nil {
			if !isKeyframe {
				return
			}
			if !codecChangePending {
				callbackErr = errors.New("media epoch is unavailable before a random access frame")
				closeIngest()
				return
			}
			if switchErr := startPendingEpoch(now.Add(dts)); switchErr != nil {
				callbackErr = switchErr
				closeIngest()
				return
			}
		}
		if hasVCL {
			lastVideoDTS = dts
		}
		seg, b, e := muxer.PushVideo(pts, dts, au, time.Now().UTC())
		commit(seg, b)
		s.setBufferedSamples(streamID, int64(muxer.BufferedSamples()))
		if callbackErr != nil {
			return
		}
		if e != nil {
			callbackErr = e
			closeIngest()
			return
		}
	}
	for _, track := range r.Tracks() {
		switch track.Codec.(type) {
		case *codecs.H264:
			r.OnDataH264(track, func(pts, dts time.Duration, au [][]byte) { processVideo("H.264", pts, dts, au) })
		case *codecs.H265:
			r.OnDataH265(track, func(pts, dts time.Duration, au [][]byte) { processVideo("HEVC", pts, dts, au) })
		case *codecs.AV1:
			r.OnDataAV1(track, func(pts time.Duration, tu [][]byte) { processVideo("AV1", pts, pts, tu) })
		case *codecs.MPEG4Audio:
			r.OnDataMPEG4Audio(track, func(pts time.Duration, au []byte) {
				if callbackErr != nil {
					return
				}
				if callbackErr = accountBytes(len(au)); callbackErr != nil {
					closeIngest()
					return
				}
				pts = rtmpClock.Unwrap(pts)
				if muxer != nil {
					if pushErr := muxer.PushAudio(pts, au); pushErr != nil {
						callbackErr = pushErr
						closeIngest()
						return
					}
					s.setBufferedSamples(streamID, int64(muxer.BufferedSamples()))
				}
			})
		case *codecs.Opus:
			r.OnDataOpus(track, func(pts time.Duration, packet []byte) {
				if callbackErr != nil {
					return
				}
				if callbackErr = accountBytes(len(packet)); callbackErr != nil {
					closeIngest()
					return
				}
				pts = rtmpClock.Unwrap(pts)
				if muxer != nil {
					duration := mediopus.PacketDuration2(packet)
					if duration <= 0 {
						callbackErr = errors.New("invalid Opus packet duration")
						closeIngest()
						return
					}
					if pushErr := muxer.PushAudioSample(pts, packet, uint32(duration)); pushErr != nil {
						callbackErr = pushErr
						closeIngest()
						return
					}
					s.setBufferedSamples(streamID, int64(muxer.BufferedSamples()))
				}
			})
		}
	}
	r.Conn = observedConn{Conn: r.Conn, consumeAudio: !audioCallbackRegistered, observe: func(msg message.Message) {
		switch videoMessage := msg.(type) {
		case *message.Video:
			if videoMessage.FrameType != message.VideoFrameTypeKeyFrame && videoMessage.FrameType != message.VideoFrameTypeInterFrame {
				break
			}
			if videoMessage.Codec != message.CodecH264 {
				callbackErr = fmt.Errorf("video codec changed to unsupported RTMP codec %d", videoMessage.Codec)
				closeIngest()
				return
			}
			if videoMessage.PacketType == message.VideoPacketTypeConfig && videoMessage.AVCConfig != nil {
				updated := &codecs.H264{}
				for _, parameter := range videoMessage.AVCConfig.SequenceParameterSets {
					if len(parameter.NALUnit) != 0 {
						updated.SPS = append([]byte(nil), parameter.NALUnit...)
						break
					}
				}
				for _, parameter := range videoMessage.AVCConfig.PictureParameterSets {
					if len(parameter.NALUnit) != 0 {
						updated.PPS = append([]byte(nil), parameter.NALUnit...)
						break
					}
				}
				if len(updated.SPS) == 0 || len(updated.PPS) == 0 {
					callbackErr = errors.New("H.264 sequence header did not contain valid SPS/PPS")
					closeIngest()
					return
				}
				baseVideo, baseAudio := effectiveVideo(), effectiveAudio()
				updatedVideo := &media.VideoConfig{H264: updated}
				if !reflect.DeepEqual(baseVideo, updatedVideo) {
					if switchErr := queueCodecChange(updatedVideo, baseAudio); switchErr != nil {
						callbackErr = switchErr
						closeIngest()
					}
				}
			}
		case *message.VideoExSequenceStart:
			var updatedVideo *media.VideoConfig
			switch videoMessage.FourCC {
			case message.FourCCHEVC:
				vps, sps, pps := hevcParams(videoMessage.HEVCConfig)
				updatedVideo = &media.VideoConfig{H265: &codecs.H265{VPS: vps, SPS: sps, PPS: pps}}
			case message.FourCCAV1:
				var configErr error
				var av1Config *media.AV1Config
				av1Config, configErr = av1ConfigFromSequence(videoMessage.AV1Config)
				if configErr != nil {
					callbackErr = configErr
				} else {
					updatedVideo = &media.VideoConfig{AV1: av1Config}
				}
			default:
				callbackErr = fmt.Errorf("unsupported enhanced RTMP video codec %v", videoMessage.FourCC)
			}
			if callbackErr != nil {
				closeIngest()
				return
			}
			if effectiveVideo().CodecName() != updatedVideo.CodecName() {
				callbackErr = errors.New("video codec cannot change during a publish; reconnect using the new codec")
				closeIngest()
				return
			}
			if !reflect.DeepEqual(effectiveVideo(), updatedVideo) {
				if switchErr := queueCodecChange(updatedVideo, effectiveAudio()); switchErr != nil {
					callbackErr = switchErr
					closeIngest()
				}
			}
			return
		}
		if callbackErr != nil {
			return
		}
		var updated any
		var incompatibleAudio bool
		var endedAudio bool
		var dynamicAudioDTS time.Duration
		var dynamicAudioAU []byte
		switch audioMessage := msg.(type) {
		case *message.Audio:
			if audioMessage.Codec == message.CodecMPEG4Audio && audioMessage.AACType == message.AudioAACTypeConfig && audioMessage.AACConfig != nil {
				updated = &codecs.MPEG4Audio{Config: audioMessage.AACConfig}
			} else if audioMessage.Codec == message.CodecMPEG4Audio && audioMessage.AACType == message.AudioAACTypeAU {
				dynamicAudioDTS, dynamicAudioAU = audioMessage.DTS, audioMessage.AU
			} else if audioMessage.Codec != 0 && audioMessage.Codec != message.CodecMPEG4Audio {
				incompatibleAudio = true
			}
		case *message.AudioExSequenceStart:
			if audioMessage.FourCC == message.FourCCMP4A && audioMessage.AACConfig != nil {
				updated = &codecs.MPEG4Audio{Config: audioMessage.AACConfig}
			} else if audioMessage.FourCC == message.FourCCOpus && audioMessage.OpusConfig != nil {
				updated = &codecs.Opus{IDHeader: audioMessage.OpusConfig}
			} else {
				incompatibleAudio = true
			}
		case *message.AudioExCodedFrames:
			if audioMessage.FourCC != message.FourCCMP4A && audioMessage.FourCC != message.FourCCOpus {
				incompatibleAudio = true
			} else {
				dynamicAudioDTS, dynamicAudioAU = audioMessage.DTS, audioMessage.Payload
			}
		case *message.AudioExSequenceEnd:
			if audioMessage.FourCC == message.FourCCMP4A || audioMessage.FourCC == message.FourCCOpus {
				endedAudio = true
			} else {
				incompatibleAudio = true
			}
		}
		if incompatibleAudio {
			callbackErr = errors.New("unsupported audio codec or sequence type during publish; accepted audio codecs are AAC-LC and Opus")
			closeIngest()
			return
		}
		if endedAudio {
			if effectiveAudio() != nil {
				if switchErr := queueCodecChange(effectiveVideo(), nil); switchErr != nil {
					callbackErr = switchErr
					closeIngest()
					return
				}
				dynamicAudio = !audioCallbackRegistered
			}
			return
		}
		if len(dynamicAudioAU) > 0 && dynamicAudio && muxer != nil && effectiveAudio() != nil {
			if callbackErr = accountBytes(len(dynamicAudioAU)); callbackErr != nil {
				closeIngest()
				return
			}
			if pushErr := muxer.PushAudio(rtmpClock.Unwrap(dynamicAudioDTS), dynamicAudioAU); pushErr != nil {
				callbackErr = pushErr
				closeIngest()
				return
			}
			s.setBufferedSamples(streamID, int64(muxer.BufferedSamples()))
		}
		if updated == nil {
			return
		}
		baseVideo, baseAudio := effectiveVideo(), effectiveAudio()
		if reflect.DeepEqual(baseAudio, updated) {
			return
		}
		if switchErr := queueCodecChange(baseVideo, updated); switchErr != nil {
			callbackErr = switchErr
			closeIngest()
			return
		}
		if baseAudio == nil && updated != nil {
			dynamicAudio = !audioCallbackRegistered
		}
	}}
	for {
		if setDeadline != nil {
			deadline := time.Now().Add(time.Duration(s.cfg.Ingest.ReadTimeout))
			if !keyframeSeen && keyframeDeadline.Before(deadline) {
				deadline = keyframeDeadline
			}
			if deadlineErr := setDeadline(deadline); deadlineErr != nil {
				err = deadlineErr
				break
			}
		}
		err = r.Read()
		if !keyframeSeen && !time.Now().Before(keyframeDeadline) {
			err = errors.New("no video random access frame arrived before keyframe_wait_timeout")
			break
		}
		if err != nil {
			break
		}
	}
	if epoch != nil || muxer != nil {
		if closeErr := closeEpoch(epochClockAnchor.Add(lastVideoDTS + time.Second/30)); closeErr != nil && callbackErr == nil {
			callbackErr = closeErr
		}
	}
	_ = s.store.EndSession(recordCtx, session.ID)
	if callbackErr != nil {
		return callbackErr
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *Server) startMediaEpoch(ctx context.Context, streamID, sessionID string, anchor, startedAt time.Time, video *media.VideoConfig, audio any) (*media.Muxer, model.Epoch, int64, error) {
	epoch := model.Epoch{ID: uuid.NewString(), SessionID: sessionID, StartedAt: startedAt, VideoTimescale: media.VideoTimescale}
	switch value := audio.(type) {
	case *codecs.MPEG4Audio:
		if value.Config != nil {
			epoch.AudioTimescale = uint32(value.Config.SampleRate)
		}
	case *codecs.Opus:
		epoch.AudioTimescale = 48000
	}
	muxer, init, codecHash, err := media.NewMuxer(streamID, sessionID, epoch.ID, anchor, video, audio, time.Duration(s.cfg.Media.TargetSegmentDuration), time.Duration(s.cfg.Media.MaxSegmentDuration))
	if err != nil {
		return nil, epoch, 0, err
	}
	muxer.SetMaxBufferedSamples(s.cfg.Media.MaxPacketQueue)
	epoch.CodecConfigHash = codecHash
	epoch.InitPath = path.Join("streams", streamID, sessionID, epoch.ID, "init.mp4")
	freeBytes, err := s.store.EnsureFreeSpace(int64(len(init)), int64(s.cfg.Storage.ReservedFreeSpace))
	if err != nil {
		return nil, epoch, freeBytes, err
	}
	if err = writeAtomic(s.store.Root(), epoch.InitPath, init); err != nil {
		return nil, epoch, freeBytes, err
	}
	if err = s.store.BeginEpoch(ctx, epoch); err != nil {
		return nil, epoch, freeBytes, err
	}
	return muxer, epoch, freeBytes, nil
}

func cloneVideo(value *media.VideoConfig) *media.VideoConfig { return value.Clone() }

func cloneAudio(value any) any {
	switch audio := value.(type) {
	case *codecs.MPEG4Audio:
		if audio.Config == nil {
			return &codecs.MPEG4Audio{}
		}
		config := *audio.Config
		return &codecs.MPEG4Audio{Config: &config}
	case *codecs.Opus:
		if audio.IDHeader == nil {
			return &codecs.Opus{}
		}
		config := *audio.IDHeader
		config.ChannelMappingTable = append([]uint8(nil), audio.IDHeader.ChannelMappingTable...)
		return &codecs.Opus{IDHeader: &config}
	default:
		return nil
	}
}

func mediaEpochClock(anchor time.Time, previousDTS, nextDTS time.Duration) (time.Time, time.Time) {
	epochAnchor := anchor
	if nextDTS < previousDTS {
		epochAnchor = anchor.Add(previousDTS + time.Second/30 - nextDTS)
	}
	return epochAnchor, epochAnchor.Add(nextDTS)
}

type rtmpTimestampUnwrapper struct {
	lastRaw time.Duration
	offset  time.Duration
	seen    bool
}

func (u *rtmpTimestampUnwrapper) Unwrap(raw time.Duration) time.Duration {
	const wrapThreshold = 24 * time.Hour
	const timestampRange = time.Duration(uint64(1)<<32) * time.Millisecond
	if u.seen && raw < u.lastRaw && u.lastRaw-raw > wrapThreshold {
		u.offset += timestampRange
	}
	u.lastRaw = raw
	u.seen = true
	return raw + u.offset
}

func videoConfigFromAccessUnit(current *media.VideoConfig, codec string, au [][]byte) (*media.VideoConfig, bool) {
	updated := cloneVideo(current)
	if updated == nil {
		return nil, false
	}
	changed := false
	switch codec {
	case "H.264":
		if updated.H264 == nil {
			return updated, false
		}
		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}
			switch nalu[0] & 0x1f {
			case 7:
				if !reflect.DeepEqual(updated.H264.SPS, nalu) {
					updated.H264.SPS = append([]byte(nil), nalu...)
					changed = true
				}
			case 8:
				if !reflect.DeepEqual(updated.H264.PPS, nalu) {
					updated.H264.PPS = append([]byte(nil), nalu...)
					changed = true
				}
			}
		}
	case "HEVC":
		if updated.H265 == nil {
			return updated, false
		}
		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}
			switch h265.NALUType((nalu[0] >> 1) & 0x3f) {
			case h265.NALUType_VPS_NUT:
				if !reflect.DeepEqual(updated.H265.VPS, nalu) {
					updated.H265.VPS = append([]byte(nil), nalu...)
					changed = true
				}
			case h265.NALUType_SPS_NUT:
				if !reflect.DeepEqual(updated.H265.SPS, nalu) {
					updated.H265.SPS = append([]byte(nil), nalu...)
					changed = true
				}
			case h265.NALUType_PPS_NUT:
				if !reflect.DeepEqual(updated.H265.PPS, nalu) {
					updated.H265.PPS = append([]byte(nil), nalu...)
					changed = true
				}
			}
		}
	}
	return updated, changed
}

func h264ConfigFromAccessUnit(current *codecs.H264, au [][]byte) (*codecs.H264, bool) {
	if current == nil {
		return nil, false
	}
	updated, changed := videoConfigFromAccessUnit(&media.VideoConfig{H264: current}, "H.264", au)
	if updated == nil {
		return nil, changed
	}
	return updated.H264, changed
}

func hevcParams(config *mp4.HvcC) ([]byte, []byte, []byte) {
	var vps, sps, pps []byte
	if config == nil {
		return nil, nil, nil
	}
	for _, array := range config.NaluArrays {
		if len(array.Nalus) == 0 || len(array.Nalus[0].NALUnit) == 0 {
			continue
		}
		switch h265.NALUType(array.NaluType) {
		case h265.NALUType_VPS_NUT:
			vps = append([]byte(nil), array.Nalus[0].NALUnit...)
		case h265.NALUType_SPS_NUT:
			sps = append([]byte(nil), array.Nalus[0].NALUnit...)
		case h265.NALUType_PPS_NUT:
			pps = append([]byte(nil), array.Nalus[0].NALUnit...)
		}
	}
	return vps, sps, pps
}

func av1ConfigFromSequence(config *mp4.Av1C) (*media.AV1Config, error) {
	if config == nil {
		return nil, errors.New("AV1 decoder configuration is missing")
	}
	record := &mp4ff.Av1CBox{CodecConfRec: mp4av1.CodecConfRec{
		Version: 1, SeqProfile: config.SeqProfile, SeqLevelIdx0: config.SeqLevelIdx0, SeqTier0: config.SeqTier0,
		HighBitdepth: config.HighBitdepth, TwelveBit: config.TwelveBit, MonoChrome: config.Monochrome,
		ChromaSubsamplingX: config.ChromaSubsamplingX, ChromaSubsamplingY: config.ChromaSubsamplingY,
		ChromaSamplePosition: config.ChromaSamplePosition, InitialPresentationDelayPresent: config.InitialPresentationDelayPresent,
		InitialPresentationDelayMinusOne: config.InitialPresentationDelayMinusOne, ConfigOBUs: append([]byte(nil), config.ConfigOBUs...),
	}}
	sequence, err := record.SequenceHeader()
	if err != nil {
		return nil, fmt.Errorf("parse AV1 sequence header: %w", err)
	}
	width, height := sequence.Width(), sequence.Height()
	if width <= 0 || height <= 0 || width > 65535 || height > 65535 {
		return nil, fmt.Errorf("unsupported AV1 dimensions %dx%d", width, height)
	}
	return &media.AV1Config{Record: record, Width: uint16(width), Height: uint16(height)}, nil
}

func writeAtomic(root, rel string, data []byte) error {
	dir := path.Join(root, path.Dir(rel))
	if err := osMkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp := path.Join(dir, "init.mp4.tmp")
	dst := path.Join(root, rel)
	f, err := osCreate(tmp)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = osRename(tmp, dst); err != nil {
		return err
	}
	d, err := osOpen(dir)
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	return err
}

var osMkdirAll = os.MkdirAll
var osCreate = func(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
}
var osRename = os.Rename
var osOpen = os.Open

func NewPublishKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
