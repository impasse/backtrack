package media

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/bluenviron/gortmplib/pkg/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
)

func TestMuxerWritesVideoAndAudioToSeparateTracks(t *testing.T) {
	sps := decodeHex(t, "6764000bacd942046c0440000003004000000f03c50a6580")
	pps := decodeHex(t, "68ebe3cb22c0")
	muxer, initData, _, err := NewMuxer(
		"stream", "session", "epoch", time.Unix(0, 0).UTC(),
		&codecs.H264{SPS: sps, PPS: pps},
		&codecs.MPEG4Audio{Config: &mpeg4audio.Config{Type: mpeg4audio.ObjectTypeAACLC, SampleRate: 48000, ChannelConfig: 2}},
		2*time.Second, 4*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}

	var fragment []byte
	for i := 0; i <= 60; i++ {
		pts := time.Duration(i) * time.Second / 30
		if i%2 == 0 {
			muxer.PushAudio(pts, bytes.Repeat([]byte{0x11}, 40))
		}
		nalu := bytes.Repeat([]byte{0x22}, 96)
		nalu[0] = 0x41
		if i%60 == 0 {
			nalu[0] = 0x65
		}
		_, data, pushErr := muxer.PushVideo(pts, pts, [][]byte{nalu}, time.Unix(0, 0).UTC())
		if pushErr != nil {
			t.Fatal(pushErr)
		}
		if data != nil {
			fragment = data
		}
	}
	if len(fragment) == 0 {
		t.Fatal("target-duration fragment was not emitted")
	}

	dir := t.TempDir()
	initPath := filepath.Join(dir, "init.mp4")
	fragmentPath := filepath.Join(dir, "segment.m4s")
	if err = os.WriteFile(initPath, initData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fragmentPath, fragment, 0o600); err != nil {
		t.Fatal(err)
	}
	initFile, err := mp4.ReadMP4File(initPath)
	if err != nil {
		t.Fatal(err)
	}
	mediaFile, err := mp4.ReadMP4File(fragmentPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed := mediaFile.Segments[0].Fragments[0]
	if len(parsed.Moof.Trafs) != 2 {
		t.Fatalf("fragment has %d track fragments, want video and audio", len(parsed.Moof.Trafs))
	}

	sampleCounts := map[uint32]int{}
	for _, traf := range parsed.Moof.Trafs {
		trex, ok := initFile.Init.Moov.Mvex.GetTrex(traf.Tfhd.TrackID)
		if !ok {
			t.Fatalf("missing trex defaults for track %d", traf.Tfhd.TrackID)
		}
		samples, sampleErr := parsed.GetFullSamples(trex)
		if sampleErr != nil {
			t.Fatal(sampleErr)
		}
		sampleCounts[traf.Tfhd.TrackID] = len(samples)
		if traf.Tfhd.TrackID == 1 {
			for i, sample := range samples {
				if len(sample.Data) < 5 {
					t.Fatalf("video sample %d is too short", i)
				}
				prefixSize := binary.BigEndian.Uint32(sample.Data[:4])
				if int(prefixSize) != len(sample.Data)-4 {
					t.Fatalf("video sample %d has an invalid AVCC prefix: %d bytes for %d-byte payload", i, prefixSize, len(sample.Data)-4)
				}
			}
		}
	}
	if sampleCounts[1] != 60 || sampleCounts[2] != 30 {
		t.Fatalf("unexpected track sample counts: video=%d audio=%d", sampleCounts[1], sampleCounts[2])
	}
}

func TestMuxerRejectsBackwardsDecodeTimestamp(t *testing.T) {
	sps := decodeHex(t, "6764000bacd942046c0440000003004000000f03c50a6580")
	pps := decodeHex(t, "68ebe3cb22c0")
	muxer, _, _, err := NewMuxer("stream", "session", "epoch", time.Now(), &codecs.H264{SPS: sps, PPS: pps}, nil, 2*time.Second, 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	key := [][]byte{{0x65, 0x01}}
	if _, _, err = muxer.PushVideo(time.Second, time.Second, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = muxer.PushVideo(900*time.Millisecond, 900*time.Millisecond, [][]byte{{0x41, 0x02}}, time.Now()); err == nil {
		t.Fatal("expected backwards decode timestamp to be rejected")
	}
}

func TestMuxerBoundsBufferedMediaSamples(t *testing.T) {
	sps := decodeHex(t, "6764000bacd942046c0440000003004000000f03c50a6580")
	pps := decodeHex(t, "68ebe3cb22c0")
	muxer, _, _, err := NewMuxer("stream", "session", "epoch", time.Now(), &codecs.H264{SPS: sps, PPS: pps}, nil, 2*time.Second, 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	muxer.SetMaxBufferedSamples(1)
	if _, _, err = muxer.PushVideo(0, 0, [][]byte{{0x65, 0x01}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := muxer.BufferedSamples(); got != 1 {
		t.Fatalf("buffered sample count = %d, want 1", got)
	}
	if _, _, err = muxer.PushVideo(time.Second/30, time.Second/30, [][]byte{{0x41, 0x02}}, time.Now()); err == nil {
		t.Fatal("expected configured buffered-sample limit to reject another packet")
	}
}

func decodeHex(t *testing.T, v string) []byte {
	t.Helper()
	b, err := hex.DecodeString(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
