package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"backtrack/internal/model"
	mp4av1 "github.com/Eyevinn/mp4ff/av1"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/bluenviron/gortmplib/pkg/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/av1"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
)

const VideoTimescale = 90000

type Sample struct {
	Data     []byte
	PTS      int64
	DTS      int64
	Duration uint32
	Sync     bool
}

// AV1Config contains the ISO BMFF decoder configuration and coded dimensions.
type AV1Config struct {
	Record *mp4.Av1CBox
	Width  uint16
	Height uint16
}

// VideoConfig describes the single video track supported by a media epoch.
type VideoConfig struct {
	H264 *codecs.H264
	H265 *codecs.H265
	AV1  *AV1Config
}

func (c *VideoConfig) Clone() *VideoConfig {
	if c == nil {
		return nil
	}
	out := &VideoConfig{}
	if c.H264 != nil {
		out.H264 = &codecs.H264{SPS: append([]byte(nil), c.H264.SPS...), PPS: append([]byte(nil), c.H264.PPS...)}
	}
	if c.H265 != nil {
		out.H265 = &codecs.H265{VPS: append([]byte(nil), c.H265.VPS...), SPS: append([]byte(nil), c.H265.SPS...), PPS: append([]byte(nil), c.H265.PPS...)}
	}
	if c.AV1 != nil {
		av1Config := &AV1Config{Width: c.AV1.Width, Height: c.AV1.Height}
		if c.AV1.Record != nil {
			rec := *c.AV1.Record
			rec.ConfigOBUs = append([]byte(nil), rec.ConfigOBUs...)
			av1Config.Record = &rec
		}
		out.AV1 = av1Config
	}
	return out
}

func (c *VideoConfig) CodecName() string {
	switch {
	case c != nil && c.H264 != nil:
		return "H.264"
	case c != nil && c.H265 != nil:
		return "HEVC"
	case c != nil && c.AV1 != nil:
		return "AV1"
	default:
		return "unknown video"
	}
}

func (c *VideoConfig) IsAV1RandomAccess(tu [][]byte) bool {
	if c == nil || c.AV1 == nil || c.AV1.Record == nil {
		return false
	}
	var bitstream av1.Bitstream = tu
	data, err := bitstream.Marshal()
	if err != nil {
		return false
	}
	sequence, err := c.AV1.Record.SequenceHeader()
	if err != nil {
		return false
	}
	randomAccess, err := mp4av1.IsRAPSample(data, sequence)
	return err == nil && randomAccess
}

type Muxer struct {
	streamID, sessionID, epochID string
	anchor                       time.Time
	target, maximum              time.Duration
	sequence                     uint64
	videoTrack                   uint32
	audioTrack                   uint32
	videoScale                   uint32
	audioScale                   uint32
	audioRate                    int
	init                         []byte
	video                        []Sample
	audio                        []Sample
	segmentStartDTS              int64
	started                      bool
	lastVideoDTS                 int64
	codecHash                    string
	videoCodec                   string
	parameterSets                [][]byte
	av1Sequence                  *mp4av1.SequenceHeader
	maxBufferedSamples           int
}

func NewMuxer(streamID, sessionID, epochID string, anchor time.Time, video any, audio any, target, maximum time.Duration) (*Muxer, []byte, string, error) {
	var config *VideoConfig
	switch value := video.(type) {
	case *VideoConfig:
		config = value
	case *codecs.H264:
		config = &VideoConfig{H264: value}
	case *codecs.H265:
		config = &VideoConfig{H265: value}
	case *AV1Config:
		config = &VideoConfig{AV1: value}
	}
	if config == nil {
		return nil, nil, "", errors.New("video configuration is required")
	}
	var opusConfig *opus.IDHeader
	if audio != nil {
		switch value := audio.(type) {
		case *codecs.MPEG4Audio:
			if value.Config == nil || value.Config.Type != mpeg4audio.ObjectTypeAACLC || value.Config.SampleRate <= 0 {
				return nil, nil, "", errors.New("only AAC-LC with a valid sample rate is supported")
			}
		case *codecs.Opus:
			if value.IDHeader == nil || value.IDHeader.ChannelCount == 0 {
				return nil, nil, "", errors.New("valid Opus configuration is required")
			}
			opusConfig = value.IDHeader
		default:
			return nil, nil, "", fmt.Errorf("unsupported audio configuration %T", audio)
		}
	}
	init := mp4.CreateEmptyInit()
	vt := init.AddEmptyTrack(VideoTimescale, "video", "und")
	var videoCodec string
	var params [][]byte
	switch {
	case config.H264 != nil && len(config.H264.SPS) > 0 && len(config.H264.PPS) > 0:
		videoCodec, params = "H.264", [][]byte{config.H264.SPS, config.H264.PPS}
		if err := vt.SetAVCDescriptor("avc1", [][]byte{config.H264.SPS}, [][]byte{config.H264.PPS}, true); err != nil {
			return nil, nil, "", err
		}
	case config.H265 != nil && len(config.H265.VPS) > 0 && len(config.H265.SPS) > 0 && len(config.H265.PPS) > 0:
		videoCodec, params = "HEVC", [][]byte{config.H265.VPS, config.H265.SPS, config.H265.PPS}
		if err := vt.SetHEVCDescriptor("hvc1", [][]byte{config.H265.VPS}, [][]byte{config.H265.SPS}, [][]byte{config.H265.PPS}, nil, true); err != nil {
			return nil, nil, "", err
		}
	case config.AV1 != nil && config.AV1.Record != nil && config.AV1.Width > 0 && config.AV1.Height > 0:
		videoCodec = "AV1"
		var av1Record bytes.Buffer
		if err := config.AV1.Record.Encode(&av1Record); err != nil {
			return nil, nil, "", fmt.Errorf("encode AV1 decoder configuration: %w", err)
		}
		params = [][]byte{av1Record.Bytes()}
		if err := vt.SetAV1Descriptor("av01", config.AV1.Record, config.AV1.Width, config.AV1.Height); err != nil {
			return nil, nil, "", err
		}
	default:
		return nil, nil, "", fmt.Errorf("valid %s video configuration is required", config.CodecName())
	}
	var at *mp4.TrakBox
	var audioScale uint32
	if audio != nil {
		if opusConfig != nil {
			audioScale = 48000
		} else {
			audioScale = uint32(audio.(*codecs.MPEG4Audio).Config.SampleRate)
		}
		at = init.AddEmptyTrack(audioScale, "audio", "und")
		if opusConfig != nil {
			dops := &mp4.DopsBox{Version: 0, OutputChannelCount: opusConfig.ChannelCount, PreSkip: opusConfig.PreSkip, InputSampleRate: opusConfig.InputSampleRate, OutputGain: int16(opusConfig.OutputGain), ChannelMappingFamily: opusConfig.ChannelMappingFamily}
			if dops.ChannelMappingFamily != 0 {
				if len(opusConfig.ChannelMappingTable) < 2 {
					return nil, nil, "", errors.New("invalid Opus channel mapping")
				}
				dops.StreamCount = opusConfig.ChannelMappingTable[0]
				dops.CoupledCount = opusConfig.ChannelMappingTable[1]
				dops.ChannelMapping = append([]byte(nil), opusConfig.ChannelMappingTable[2:]...)
			}
			at.Mdia.Minf.Stbl.Stsd.AddChild(mp4.CreateAudioSampleEntryBox("Opus", uint16(opusConfig.ChannelCount), 16, 48000, dops))
		} else if err := at.SetAACDescriptor(uint8(audio.(*codecs.MPEG4Audio).Config.Type), audio.(*codecs.MPEG4Audio).Config.SampleRate); err != nil {
			return nil, nil, "", err
		}
	}
	var ib bytes.Buffer
	if err := init.Encode(&ib); err != nil {
		return nil, nil, "", err
	}
	h := sha256.New()
	h.Write([]byte(videoCodec))
	for _, p := range params {
		h.Write(p)
	}
	if audio != nil {
		if opusConfig != nil {
			h.Write([]byte("Opus"))
			opusBytes, _ := opusConfig.Marshal()
			h.Write(opusBytes)
		} else {
			aac := audio.(*codecs.MPEG4Audio)
			h.Write([]byte{byte(aac.Config.Type), aac.Config.ChannelConfig})
			binary.Write(h, binary.BigEndian, int32(aac.Config.SampleRate))
		}
	}
	m := &Muxer{streamID: streamID, sessionID: sessionID, epochID: epochID, anchor: anchor.UTC(), target: target, maximum: maximum, videoTrack: vt.Tkhd.TrackID, videoScale: VideoTimescale, audioScale: audioScale, init: ib.Bytes(), codecHash: hex.EncodeToString(h.Sum(nil)), videoCodec: videoCodec, parameterSets: cloneByteSlices(params)}
	if config.AV1 != nil && config.AV1.Record != nil {
		m.av1Sequence, _ = config.AV1.Record.SequenceHeader()
	}
	if at != nil {
		m.audioTrack = at.Tkhd.TrackID
		if opusConfig != nil {
			m.audioRate = 48000
		} else {
			m.audioRate = audio.(*codecs.MPEG4Audio).Config.SampleRate
		}
	}
	return m, m.init, m.codecHash, nil
}

func (m *Muxer) InitSegment() []byte { return append([]byte(nil), m.init...) }
func (m *Muxer) CodecHash() string   { return m.codecHash }

func (m *Muxer) SetMaxBufferedSamples(max int) { m.maxBufferedSamples = max }
func (m *Muxer) BufferedSamples() int          { return len(m.video) + len(m.audio) }

// PushVideo consumes one complete H.26x access unit or AV1 temporal unit.
func (m *Muxer) PushVideo(pts, dts time.Duration, au [][]byte, wallNow time.Time) (*model.Segment, []byte, error) {
	if len(au) == 0 {
		return nil, nil, nil
	}
	key := false
	data := make([]byte, 0, 1024)
	if m.videoCodec == "AV1" {
		var bitstream av1.Bitstream = au
		encoded, err := bitstream.Marshal()
		if err != nil {
			return nil, nil, fmt.Errorf("marshal AV1 temporal unit: %w", err)
		}
		data = encoded
		if m.av1Sequence != nil {
			key, _ = mp4av1.IsRAPSample(encoded, m.av1Sequence)
		}
	}
	for _, nalu := range au {
		if m.videoCodec == "AV1" {
			break
		}
		if len(nalu) == 0 {
			continue
		}
		typ := byte(0)
		if m.videoCodec == "H.264" {
			typ = nalu[0] & 0x1f
			if typ == 5 {
				key = true
			}
		}
		if m.videoCodec == "HEVC" {
			typ = (nalu[0] >> 1) & 0x3f
			if h265.IsRandomAccess([][]byte{nalu}) {
				key = true
			}
		}
		paramIndex := -1
		if m.videoCodec == "H.264" {
			if typ == 7 {
				paramIndex = 0
			}
			if typ == 8 {
				paramIndex = 1
			}
		}
		if m.videoCodec == "HEVC" {
			if typ == 32 {
				paramIndex = 0
			}
			if typ == 33 {
				paramIndex = 1
			}
			if typ == 34 {
				paramIndex = 2
			}
		}
		if paramIndex >= 0 {
			if paramIndex >= len(m.parameterSets) || !bytes.Equal(nalu, m.parameterSets[paramIndex]) {
				return nil, nil, fmt.Errorf("in-band %s parameter set change requires a new media epoch", m.videoCodec)
			}
			continue
		}
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(nalu)))
		data = append(data, prefix[:]...)
		data = append(data, nalu...)
	}
	if len(data) == 0 {
		return nil, nil, nil
	}
	pt := durationTicks(pts, m.videoScale)
	dt := durationTicks(dts, m.videoScale)
	if dt < 0 || pt < 0 {
		return nil, nil, fmt.Errorf("negative %s timestamps require a new media epoch", m.videoCodec)
	}
	if m.started && dt < m.lastVideoDTS {
		return nil, nil, fmt.Errorf("%s decode timestamp moved backwards; a new media epoch is required", m.videoCodec)
	}
	compositionOffset := pt - dt
	if compositionOffset > (1<<31)-1 || compositionOffset < -(1<<31) {
		return nil, nil, errors.New("H.264 composition offset exceeds the MP4 sample limit")
	}
	var out *model.Segment
	var blob []byte
	var err error
	if m.started && key && time.Duration(dt-m.segmentStartDTS)*time.Second/time.Duration(m.videoScale) >= m.target {
		if len(m.video) > 0 {
			m.finishVideoDuration(dt)
		}
		out, blob, err = m.flush(wallNow)
		if err != nil {
			return nil, nil, err
		}
		pendingAudio := m.audio
		m.start(dt)
		audioStart := dt * int64(m.audioScale) / int64(m.videoScale)
		for _, sample := range pendingAudio {
			if sample.DTS+int64(sample.Duration) >= audioStart {
				m.audio = append(m.audio, sample)
			}
		}
	}
	if !m.started {
		if !key {
			return nil, nil, nil
		}
		m.start(dt)
	}
	if m.maxBufferedSamples > 0 && m.BufferedSamples() >= m.maxBufferedSamples {
		return out, blob, fmt.Errorf("media packet buffer reached configured max_packet_queue=%d", m.maxBufferedSamples)
	}
	if len(m.video) > 0 {
		prev := &m.video[len(m.video)-1]
		delta := dt - prev.DTS
		if delta > 0 && delta <= int64(m.videoScale*2) {
			prev.Duration = uint32(delta)
		} else if prev.Duration == 0 {
			prev.Duration = uint32(m.videoScale / 30)
		}
	}
	m.video = append(m.video, Sample{Data: data, PTS: pt, DTS: dt, Sync: key})
	m.lastVideoDTS = dt
	if time.Duration(dt-m.segmentStartDTS)*time.Second/time.Duration(m.videoScale) > m.maximum {
		return out, blob, errors.New("segment exceeded maximum duration without a random access point")
	}
	return out, blob, nil
}

func (m *Muxer) PushAudio(pts time.Duration, au []byte) error {
	return m.PushAudioSample(pts, au, 1024)
}

// PushAudioSample consumes one AAC or Opus packet with duration in the audio track timescale.
func (m *Muxer) PushAudioSample(pts time.Duration, au []byte, duration uint32) error {
	if m.audioTrack == 0 || len(au) == 0 {
		return nil
	}
	if m.maxBufferedSamples > 0 && m.BufferedSamples() >= m.maxBufferedSamples {
		return fmt.Errorf("media packet buffer reached configured max_packet_queue=%d", m.maxBufferedSamples)
	}
	ticks := int64(pts) * int64(m.audioScale) / int64(time.Second)
	if duration == 0 {
		return errors.New("audio packet duration must be positive")
	}
	m.audio = append(m.audio, Sample{Data: append([]byte(nil), au...), PTS: ticks, DTS: ticks, Duration: duration, Sync: true})
	return nil
}

func (m *Muxer) start(dts int64) {
	m.started = true
	m.segmentStartDTS = dts
	m.video = nil
	m.audio = nil
}
func (m *Muxer) finishVideoDuration(nextDTS int64) {
	if len(m.video) > 0 {
		p := &m.video[len(m.video)-1]
		if nextDTS > p.DTS {
			p.Duration = uint32(nextDTS - p.DTS)
		}
	}
}

func (m *Muxer) flush(wallNow time.Time) (*model.Segment, []byte, error) {
	if len(m.video) == 0 {
		return nil, nil, nil
	}
	last := m.video[len(m.video)-1]
	if last.Duration == 0 {
		last.Duration = uint32(m.videoScale / 30)
	}
	endDTS := last.DTS + int64(last.Duration)
	seg := mp4.NewMediaSegment()
	frag, err := mp4.CreateFragment(uint32(m.sequence+1), m.videoTrack)
	if err != nil {
		return nil, nil, err
	}
	seg.AddFragment(frag)
	for _, s := range m.video {
		flags := uint32(0x01010000)
		if s.Sync {
			flags = 0x02000000
		}
		full := mp4.FullSample{Sample: mp4.Sample{Flags: flags, Dur: s.Duration, Size: uint32(len(s.Data)), CompositionTimeOffset: int32(s.PTS - s.DTS)}, DecodeTime: uint64(s.DTS), Data: s.Data}
		if err = frag.AddFullSampleToTrack(full, m.videoTrack); err != nil {
			return nil, nil, err
		}
	}
	if m.audioTrack != 0 {
		var audioSamples []Sample
		for _, s := range m.audio {
			if s.DTS+int64(s.Duration) < int64(m.segmentStartDTS)*int64(m.audioScale)/int64(m.videoScale) || s.DTS > endDTS*int64(m.audioScale)/int64(m.videoScale) {
				continue
			}
			audioSamples = append(audioSamples, s)
		}
		if len(audioSamples) > 0 {
			// CreateFragment starts with only a video traf. mp4ff's AddSampleToTrack
			// does not reliably reject a missing track when a fragment has one traf,
			// so add the AAC traf explicitly before appending any audio samples.
			audioTraf := &mp4.TrafBox{}
			if err = audioTraf.AddChild(mp4.CreateTfhd(m.audioTrack)); err != nil {
				return nil, nil, err
			}
			if err = audioTraf.AddChild(mp4.CreateTfdt(0)); err != nil {
				return nil, nil, err
			}
			if err = frag.Moof.AddChild(audioTraf); err != nil {
				return nil, nil, err
			}
		}
		for _, s := range audioSamples {
			full := mp4.FullSample{Sample: mp4.Sample{Flags: 0x02000000, Dur: s.Duration, Size: uint32(len(s.Data))}, DecodeTime: uint64(s.DTS), Data: s.Data}
			if err = frag.AddFullSampleToTrack(full, m.audioTrack); err != nil {
				return nil, nil, err
			}
		}
	}
	var buf bytes.Buffer
	if err = seg.Encode(&buf); err != nil {
		return nil, nil, err
	}
	m.sequence++
	start := m.anchor.Add(time.Duration(m.segmentStartDTS) * time.Second / time.Duration(m.videoScale))
	end := m.anchor.Add(time.Duration(endDTS) * time.Second / time.Duration(m.videoScale))
	minPTS, maxPTS := m.video[0].PTS, m.video[0].PTS
	for _, s := range m.video {
		if s.PTS < minPTS {
			minPTS = s.PTS
		}
		if s.PTS > maxPTS {
			maxPTS = s.PTS
		}
	}
	_ = wallNow
	v := model.Segment{StreamID: m.streamID, SessionID: m.sessionID, EpochID: m.epochID, Sequence: m.sequence, StartTime: start, EndTime: end, FirstPTS: minPTS, LastPTS: maxPTS, FirstDTS: m.video[0].DTS, LastDTS: endDTS, TimeScale: m.videoScale, RandomAccess: m.video[0].Sync, State: "COMMITTED"}
	data := append([]byte(nil), buf.Bytes()...)
	return &v, data, nil
}

func (m *Muxer) Close() (*model.Segment, []byte, error) {
	if len(m.video) == 0 {
		return nil, nil, nil
	}
	m.finishVideoDuration(m.lastVideoDTS + int64(m.videoScale/30))
	s, b, err := m.flush(time.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	if s == nil {
		return nil, nil, fmt.Errorf("could not flush final media fragment")
	}
	return s, b, nil
}

func cloneByteSlices(values [][]byte) [][]byte {
	ret := make([][]byte, len(values))
	for i := range values {
		ret[i] = append([]byte(nil), values[i]...)
	}
	return ret
}

func durationTicks(d time.Duration, scale uint32) int64 {
	return int64(d) * int64(scale) / int64(time.Second)
}
