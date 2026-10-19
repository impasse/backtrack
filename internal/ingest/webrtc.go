package ingest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"backtrack/internal/events"
	mp4box "github.com/abema/go-mp4"
	"github.com/bluenviron/gortmplib/pkg/message"
	mediopus "github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	rtpcodecs "github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// WHIPSession is a negotiated browser publisher. Its close callback releases
// the per-stream publisher slot and finalizes the shared DVR recording path.
type WHIPSession struct {
	ID     string
	Answer string
	close  func()
}

func (w *WHIPSession) Close() {
	if w != nil && w.close != nil {
		w.close()
	}
}

type messagePipe struct {
	messages chan message.Message
	done     chan struct{}
	closed   sync.Once
	received atomic.Uint64
	sent     atomic.Uint64
}

func newMessagePipe() *messagePipe {
	return &messagePipe{messages: make(chan message.Message, 512), done: make(chan struct{})}
}
func (p *messagePipe) BytesReceived() uint64       { return p.received.Load() }
func (p *messagePipe) BytesSent() uint64           { return p.sent.Load() }
func (p *messagePipe) Write(message.Message) error { return nil }
func (p *messagePipe) Read() (message.Message, error) {
	select {
	case msg := <-p.messages:
		return msg, nil
	case <-p.done:
		return nil, io.EOF
	}
}
func (p *messagePipe) Close() { p.closed.Do(func() { close(p.done) }) }
func (p *messagePipe) Push(msg message.Message) error {
	select {
	case p.messages <- msg:
		return nil
	case <-p.done:
		return io.ErrClosedPipe
	}
}

// StartWHIP authenticates the channel's publish key, negotiates a WHIP SDP
// offer, and routes H.264/Opus RTP into the same recorder as RTMP publishers.
func (s *Server) StartWHIP(ctx context.Context, streamID, key, offer string) (*WHIPSession, error) {
	if strings.TrimSpace(offer) == "" {
		return nil, errors.New("SDP offer is required")
	}
	ok, err := s.store.AuthenticatePublish(ctx, streamID, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("invalid publish key or disabled channel")
	}
	s.mu.Lock()
	if s.active[streamID] {
		s.mu.Unlock()
		return nil, errors.New("channel already has an active publisher")
	}
	s.active[streamID] = true
	s.publisherInfo[streamID] = "WebRTC"
	s.mu.Unlock()
	started := atomic.Bool{}
	finish := func(sessionID string) {
		s.mu.Lock()
		delete(s.active, streamID)
		delete(s.publisherInfo, streamID)
		delete(s.webrtcSessions, sessionID)
		s.mu.Unlock()
		if s.events != nil && started.Load() {
			s.events.Publish(events.Event{Type: "stream.stopped", StreamID: streamID})
		}
	}
	cleanupOnError := func() { finish("") }
	me := &webrtc.MediaEngine{}
	if err = me.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}, PayloadType: 102}, webrtc.RTPCodecTypeVideo); err != nil {
		cleanupOnError()
		return nil, err
	}
	if err = me.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		cleanupOnError()
		return nil, err
	}
	interceptors := &interceptor.Registry{}
	if err = webrtc.RegisterDefaultInterceptors(me, interceptors); err != nil {
		cleanupOnError()
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	if err = settings.SetEphemeralUDPPortRange(50000, 50100); err != nil {
		cleanupOnError()
		return nil, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(interceptors), webrtc.WithSettingEngine(settings))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}})
	if err != nil {
		cleanupOnError()
		return nil, err
	}
	if sdpHasMedia(offer, "video") {
		if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			_ = pc.Close()
			cleanupOnError()
			return nil, err
		}
	}
	if sdpHasMedia(offer, "audio") {
		if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			_ = pc.Close()
			cleanupOnError()
			return nil, err
		}
	}
	pipe := newMessagePipe()
	sessionID := uuid.NewString()
	var closeOnce sync.Once
	var recordStarted sync.Once
	closeSession := func() {
		closeOnce.Do(func() {
			pipe.Close()
			_ = pc.Close()
			finish(sessionID)
		})
	}
	startRecorder := func() {
		recordStarted.Do(func() {
			started.Store(true)
			if s.events != nil {
				s.events.Publish(events.Event{Type: "stream.started", StreamID: streamID})
			}
			go func() {
				if recordErr := s.record(context.Background(), streamID, pipe, pipe.Close, nil); recordErr != nil {
					s.logger.Warn("WebRTC recording ended", "stream_id", streamID, "error", recordErr)
				}
				closeSession()
			}()
		})
	}
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		go func() {
			for {
				if _, _, e := receiver.ReadRTCP(); e != nil {
					return
				}
			}
		}()
		go func() {
			var readErr error
			switch strings.ToLower(track.Codec().MimeType) {
			case strings.ToLower(webrtc.MimeTypeH264):
				readErr = s.readWHIPH264(pc, pipe, track, startRecorder)
			case strings.ToLower(webrtc.MimeTypeOpus):
				readErr = s.readWHIPOpus(pipe, track, startRecorder)
			default:
				readErr = fmt.Errorf("unsupported WebRTC codec %s; use H.264 video and Opus audio", track.Codec().MimeType)
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrClosedPipe) {
				s.logger.Warn("WebRTC track ended", "stream_id", streamID, "codec", track.Codec().MimeType, "error", readErr)
				closeSession()
			}
		}()
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateClosed {
			closeSession()
		}
	})
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		closeSession()
		return nil, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		closeSession()
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		closeSession()
		return nil, err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		closeSession()
		return nil, ctx.Err()
	case <-time.After(20 * time.Second):
		closeSession()
		return nil, errors.New("ICE gathering timed out")
	}
	if pc.LocalDescription() == nil {
		closeSession()
		return nil, errors.New("WebRTC answer was not generated")
	}
	session := &WHIPSession{ID: sessionID, Answer: pc.LocalDescription().SDP, close: closeSession}
	s.mu.Lock()
	select {
	case <-pipe.done:
		s.mu.Unlock()
		return nil, errors.New("WebRTC publisher ended during negotiation")
	default:
	}
	s.webrtcSessions[sessionID] = closeSession
	s.mu.Unlock()
	return session, nil
}

func sdpHasMedia(sdp, kind string) bool {
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "m="+kind+" ") {
			return true
		}
	}
	return false
}

func (s *Server) CloseWHIP(id string) bool {
	s.mu.Lock()
	closeSession := s.webrtcSessions[id]
	s.mu.Unlock()
	if closeSession == nil {
		return false
	}
	closeSession()
	return true
}

func (s *Server) CloseWebRTC() {
	s.mu.Lock()
	closers := make([]func(), 0, len(s.webrtcSessions))
	for _, closeSession := range s.webrtcSessions {
		closers = append(closers, closeSession)
	}
	s.mu.Unlock()
	for _, closeSession := range closers {
		closeSession()
	}
}

func (s *Server) readWHIPH264(pc *webrtc.PeerConnection, pipe *messagePipe, track *webrtc.TrackRemote, startRecorder func()) error {
	dep := &rtpcodecs.H264Packet{}
	var frameTimestamp uint32
	var frameStarted bool
	var frame []byte
	var sps, pps []byte
	var sentConfig bool
	var timestampBase uint32
	var timestampSet bool
	pliInterval := time.Duration(s.cfg.Media.TargetSegmentDuration)
	if pliInterval <= 0 || pliInterval > time.Duration(s.cfg.Media.MaxSegmentDuration)/2 {
		pliInterval = time.Duration(s.cfg.Media.MaxSegmentDuration) / 2
	}
	if pliInterval <= 0 {
		pliInterval = 2 * time.Second
	}
	pliTicker := time.NewTicker(pliInterval)
	defer pliTicker.Stop()
	go func() {
		for {
			select {
			case <-pipe.done:
				return
			case <-pliTicker.C:
				_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
			}
		}
	}()
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return err
		}
		if !timestampSet {
			timestampBase = pkt.Timestamp
			timestampSet = true
		}
		if frameStarted && pkt.Timestamp != frameTimestamp {
			frame = nil
		}
		frameTimestamp, frameStarted = pkt.Timestamp, true
		nalu, err := dep.Unmarshal(pkt.Payload)
		if err != nil {
			continue
		}
		frame = append(frame, nalu...)
		if !pkt.Marker {
			continue
		}
		nalus := splitAnnexB(frame)
		frame = nil
		for _, unit := range nalus {
			if len(unit) == 0 {
				continue
			}
			switch unit[0] & 0x1f {
			case 7:
				sps = append([]byte(nil), unit...)
			case 8:
				pps = append([]byte(nil), unit...)
			}
		}
		if !sentConfig && len(sps) != 0 && len(pps) != 0 {
			avcc := &mp4box.AVCDecoderConfiguration{ConfigurationVersion: 1, Profile: sps[1], ProfileCompatibility: sps[2], Level: sps[3], Reserved: 0x3f, LengthSizeMinusOne: 3, Reserved2: 7, NumOfSequenceParameterSets: 1, SequenceParameterSets: []mp4box.AVCParameterSet{{NALUnit: sps}}, NumOfPictureParameterSets: 1, PictureParameterSets: []mp4box.AVCParameterSet{{NALUnit: pps}}}
			avcc.SetType(mp4box.BoxTypeAvcC())
			if err = pipe.Push(&message.Video{FrameType: message.VideoFrameTypeKeyFrame, Codec: message.CodecH264, PacketType: message.VideoPacketTypeConfig, AVCConfig: avcc}); err != nil {
				return err
			}
			sentConfig = true
			startRecorder()
		}
		if !sentConfig {
			continue
		}
		keyframe := false
		avccAU := make([]byte, 0, len(frame)+64)
		for _, unit := range nalus {
			if len(unit) == 0 {
				continue
			}
			if unit[0]&0x1f == 5 {
				keyframe = true
			}
			var size [4]byte
			binary.BigEndian.PutUint32(size[:], uint32(len(unit)))
			avccAU = append(avccAU, size[:]...)
			avccAU = append(avccAU, unit...)
		}
		if len(avccAU) == 0 {
			continue
		}
		pts := time.Duration(uint32(pkt.Timestamp-timestampBase)) * time.Second / 90000
		frameType := message.VideoFrameTypeInterFrame
		if keyframe {
			frameType = message.VideoFrameTypeKeyFrame
		}
		if err = pipe.Push(&message.Video{DTS: pts, FrameType: frameType, Codec: message.CodecH264, PacketType: message.VideoPacketTypeAU, AU: avccAU}); err != nil {
			return err
		}
	}
}

func (s *Server) readWHIPOpus(pipe *messagePipe, track *webrtc.TrackRemote, startRecorder func()) error {
	var set bool
	var base uint32
	var sentConfig bool
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return err
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		if !set {
			base, set = pkt.Timestamp, true
		}
		pts := time.Duration(uint32(pkt.Timestamp-base)) * time.Second / 48000
		if !sentConfig {
			config := &mediopus.IDHeader{Version: 1, ChannelCount: 2, InputSampleRate: 48000, ChannelMappingFamily: 0}
			if err = pipe.Push(&message.AudioExSequenceStart{FourCC: message.FourCCOpus, OpusConfig: config}); err != nil {
				return err
			}
			sentConfig = true
			startRecorder()
		}
		if err = pipe.Push(&message.AudioExCodedFrames{DTS: pts, FourCC: message.FourCCOpus, Payload: append([]byte(nil), pkt.Payload...)}); err != nil {
			return err
		}
	}
}

func splitAnnexB(data []byte) [][]byte {
	var out [][]byte
	startCode := func(i int) (int, bool) {
		if i+3 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			return 3, true
		}
		if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			return 4, true
		}
		return 0, false
	}
	var start int = -1
	for i := 0; i < len(data); {
		if n, ok := startCode(i); ok {
			if start >= 0 && i > start {
				out = append(out, append([]byte(nil), data[start:i]...))
			}
			i += n
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		out = append(out, append([]byte(nil), data[start:]...))
	}
	return out
}
