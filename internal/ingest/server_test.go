package ingest

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"backtrack/internal/config"
	"backtrack/internal/storage"
	"github.com/abema/go-mp4"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/codecs"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
)

func TestH264ConfigFromAccessUnit(t *testing.T) {
	initial := &codecs.H264{SPS: []byte{0x67, 0x01}, PPS: []byte{0x68, 0x02}}
	updated, changed := h264ConfigFromAccessUnit(initial, [][]byte{{0x67, 0x03}, {0x68, 0x04}, {0x65, 0x05}})
	if !changed {
		t.Fatal("parameter changes were not detected")
	}
	if string(updated.SPS) != string([]byte{0x67, 0x03}) || string(updated.PPS) != string([]byte{0x68, 0x04}) {
		t.Fatalf("updated parameters = %v/%v", updated.SPS, updated.PPS)
	}
	if string(initial.SPS) != string([]byte{0x67, 0x01}) || string(initial.PPS) != string([]byte{0x68, 0x02}) {
		t.Fatal("input codec parameters were mutated")
	}
	_, changed = h264ConfigFromAccessUnit(updated, [][]byte{{0x67, 0x03}, {0x68, 0x04}, {0x65, 0x06}})
	if changed {
		t.Fatal("identical parameter sets were reported as a codec change")
	}
}

func TestMediaEpochClockKeepsTimelineContinuousAcrossTimestampReset(t *testing.T) {
	anchor := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	previous := 30 * time.Second
	frame := time.Second / 30

	continuedAnchor, continuedStart := mediaEpochClock(anchor, previous, previous+2*time.Second)
	if !continuedAnchor.Equal(anchor) || !continuedStart.Equal(anchor.Add(previous+2*time.Second)) {
		t.Fatalf("continuous clock = %s, %s", continuedAnchor, continuedStart)
	}

	resetAnchor, resetStart := mediaEpochClock(anchor, previous, 100*time.Millisecond)
	want := anchor.Add(previous + frame)
	if !resetStart.Equal(want) || !resetAnchor.Equal(want.Add(-100*time.Millisecond)) {
		t.Fatalf("reset clock = %s, %s; want %s start", resetAnchor, resetStart, want)
	}
}

func TestRTMPTimestampUnwrapperHandles32BitMillisecondRollover(t *testing.T) {
	var clock rtmpTimestampUnwrapper
	beforeWrap := time.Duration(uint64(1)<<32-100) * time.Millisecond
	if got := clock.Unwrap(beforeWrap); got != beforeWrap {
		t.Fatalf("first timestamp = %s, want %s", got, beforeWrap)
	}
	afterWrap := 50 * time.Millisecond
	got := clock.Unwrap(afterWrap)
	want := time.Duration(uint64(1)<<32)*time.Millisecond + afterWrap
	if got != want {
		t.Fatalf("unwrapped timestamp = %s, want %s", got, want)
	}
	if next := clock.Unwrap(100 * time.Millisecond); next <= got {
		t.Fatalf("timestamp did not advance after rollover: %s <= %s", next, got)
	}
}

func TestPublishCreatesNewEpochAfterCodecConfigurationChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	store, err := storage.Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stream, err := store.CreateStream(ctx, "epoch-switch", "publish-secret")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	cfg := config.Config{}
	cfg.Ingest.ReconnectGrace = config.Duration(time.Minute)
	cfg.Ingest.ReadTimeout = config.Duration(5 * time.Second)
	cfg.Ingest.MaxBitrateMbps = 100
	cfg.Ingest.MaxPublishersPerStream = 1
	cfg.Media.TargetSegmentDuration = config.Duration(time.Second)
	cfg.Media.MaxSegmentDuration = config.Duration(5 * time.Second)
	cfg.Media.KeyframeWaitTimeout = config.Duration(5 * time.Second)
	cfg.Media.MaxPacketQueue = 2048
	cfg.Storage.ReservedFreeSpace = 0
	server := New(addr, cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ListenAndServe(ctx) }()
	serverStopped := false
	t.Cleanup(func() {
		cancel()
		if serverStopped {
			return
		}
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("RTMP listener did not stop after cancellation")
		}
	})
	if err = waitForTCP(addr, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse("rtmp://" + addr + "/live/" + stream.ID + "?key=publish-secret")
	if err != nil {
		t.Fatal(err)
	}
	client := &gortmplib.Client{URL: u, Publish: true}
	if err = client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	track := &gortmplib.Track{Codec: &codecs.H264{SPS: testH264SPS, PPS: testH264PPS}}
	writer := &gortmplib.Writer{Conn: client, Tracks: []*gortmplib.Track{track}}
	if err = writer.Initialize(); err != nil {
		client.Close()
		t.Fatal(err)
	}
	client.NetConn().SetWriteDeadline(time.Now().Add(8 * time.Second))
	writeFrame := func(at time.Duration, idr bool) {
		t.Helper()
		nal := []byte{0x41, 0x9a}
		if idr {
			nal[0] = 0x65
		}
		if writeErr := writer.WriteH264(track, at, at, [][]byte{nal}); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	writeAAC := func(at time.Duration, sampleRate int) {
		t.Helper()
		if err := client.Write(&message.Audio{
			ChunkStreamID: message.AudioChunkStreamID, MessageStreamID: 0x1000000,
			Codec: message.CodecMPEG4Audio, AACType: message.AudioAACTypeConfig,
			AACConfig: &mpeg4audio.AudioSpecificConfig{Type: mpeg4audio.ObjectTypeAACLC, SampleRate: sampleRate, ChannelConfig: 2, ChannelCount: 2},
			DTS:       at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeAudioFrame := func(at time.Duration) {
		t.Helper()
		if err := client.Write(&message.Audio{
			ChunkStreamID: message.AudioChunkStreamID, MessageStreamID: 0x1000000,
			Codec: message.CodecMPEG4Audio, AACType: message.AudioAACTypeAU, AU: []byte{0x01, 0x02}, DTS: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i <= 22; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		writeFrame(at, i%5 == 0 || i == 22)
	}
	changedPPS := append([]byte(nil), testH264PPS...)
	changedPPS[len(changedPPS)-1] ^= 1
	if err = client.Write(&message.Video{
		ChunkStreamID: message.VideoChunkStreamID, MessageStreamID: 0x1000000,
		Codec: message.CodecH264, FrameType: message.VideoFrameTypeKeyFrame,
		PacketType: message.VideoPacketTypeConfig, AVCConfig: testAVCConfig(testH264SPS, changedPPS),
		DTS: 2300 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 23; i <= 29; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		writeFrame(at, i%5 == 0)
	}
	writeAAC(3000*time.Millisecond, 44100)
	for i := 30; i <= 39; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		writeFrame(at, i%5 == 0)
		writeAudioFrame(at)
	}
	writeAAC(3900*time.Millisecond, 48000)
	for i := 40; i <= 55; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		writeFrame(at, i%5 == 0)
		writeAudioFrame(at)
	}
	if err = client.Write(&message.AudioExSequenceEnd{ChunkStreamID: message.AudioChunkStreamID, MessageStreamID: 0x1000000, FourCC: message.FourCCMP4A}); err != nil {
		t.Fatal(err)
	}
	for i := 56; i <= 65; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		writeFrame(at, i%5 == 0)
	}
	for i := 1; i <= 20; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		writeFrame(at, i%5 == 1)
	}
	drainDeadline := time.Now().Add(3 * time.Second)
	drained := false
	for time.Now().Before(drainDeadline) {
		var epochs, committed int
		if err = store.DB().QueryRow(`SELECT COUNT(*) FROM epochs e JOIN sessions s ON s.id=e.session_id WHERE s.stream_id=?`, stream.ID).Scan(&epochs); err != nil {
			t.Fatal(err)
		}
		if err = store.DB().QueryRow(`SELECT COUNT(*) FROM segments WHERE stream_id=? AND state='COMMITTED'`, stream.ID).Scan(&committed); err != nil {
			t.Fatal(err)
		}
		if epochs == 6 && committed >= 6 {
			drained = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !drained {
		t.Fatal("publisher did not process the queued media before shutdown")
	}
	cancel()
	select {
	case serveErr := <-serveDone:
		if serveErr != nil {
			t.Fatalf("RTMP listener shutdown failed: %v", serveErr)
		}
		serverStopped = true
	case <-time.After(3 * time.Second):
		t.Fatal("RTMP listener did not wait for publisher shutdown")
	}
	deadline := time.Now().Add(3 * time.Second)
	for server.IsActive(stream.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if server.IsActive(stream.ID) {
		t.Fatal("publisher remained active after server shutdown")
	}
	var sessions, epochs, committed, codecConfigs int
	var audio44k, audio48k, videoOnly int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE stream_id=?`, stream.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM epochs e JOIN sessions s ON s.id=e.session_id WHERE s.stream_id=?`, stream.ID).Scan(&epochs); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM segments WHERE stream_id=? AND state='COMMITTED'`, stream.ID).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT COUNT(DISTINCT e.codec_config_hash) FROM epochs e JOIN sessions s ON s.id=e.session_id WHERE s.stream_id=?`, stream.ID).Scan(&codecConfigs); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT SUM(audio_timescale=44100), SUM(audio_timescale=48000), SUM(audio_timescale=0) FROM epochs e JOIN sessions s ON s.id=e.session_id WHERE s.stream_id=?`, stream.ID).Scan(&audio44k, &audio48k, &videoOnly); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || epochs != 6 || codecConfigs != 4 || committed < 6 {
		t.Fatalf("publish result has %d sessions, %d epochs, %d codec configurations, %d committed segments; want 1, 6, 4, at least 6", sessions, epochs, codecConfigs, committed)
	}
	if audio44k != 1 || audio48k != 1 || videoOnly != 4 {
		t.Fatalf("audio track epochs: 44.1 kHz=%d, 48 kHz=%d, video-only=%d; want 1, 1, 4", audio44k, audio48k, videoOnly)
	}
	rows, err := store.DB().Query(`SELECT e.started_at_ms, e.ended_at_ms FROM epochs e JOIN sessions s ON s.id=e.session_id WHERE s.stream_id=? ORDER BY e.started_at_ms`, stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var previousEnd int64
	for rows.Next() {
		var start, end int64
		if err = rows.Scan(&start, &end); err != nil {
			t.Fatal(err)
		}
		if start < previousEnd {
			t.Fatalf("epoch timeline overlaps: start %d precedes prior end %d", start, previousEnd)
		}
		if end < start {
			t.Fatalf("epoch ends before it starts: %d < %d", end, start)
		}
		previousEnd = end
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}

var (
	testH264SPS = []byte{0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20}
	testH264PPS = []byte{0x08, 0x06, 0x07, 0x08}
)

func testAVCConfig(sps, pps []byte) *mp4.AVCDecoderConfiguration {
	config := &mp4.AVCDecoderConfiguration{
		ConfigurationVersion:       1,
		Profile:                    sps[1],
		ProfileCompatibility:       sps[2],
		Level:                      sps[3],
		Reserved:                   0b111111,
		LengthSizeMinusOne:         3,
		Reserved2:                  0b111,
		NumOfSequenceParameterSets: 1,
		SequenceParameterSets:      []mp4.AVCParameterSet{{Length: uint16(len(sps)), NALUnit: sps}},
		NumOfPictureParameterSets:  1,
		PictureParameterSets:       []mp4.AVCParameterSet{{Length: uint16(len(pps)), NALUnit: pps}},
	}
	config.SetType(mp4.BoxTypeAvcC())
	return config
}

func waitForTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return context.DeadlineExceeded
}
