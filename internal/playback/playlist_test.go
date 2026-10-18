package playback

import (
	"strings"
	"testing"
	"time"

	"backtrack/internal/model"
)

func TestPlaylistIncludesSlidingDiscontinuitySequence(t *testing.T) {
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	segments := []model.Segment{{
		ID: "segment-current", EpochID: "epoch-current", SessionID: "session-current", Sequence: 42,
		StartTime: start, EndTime: start.Add(2 * time.Second),
	}}
	playlist := Playlist(segments, func(epoch string) string { return "/api/media/init/" + epoch }, false, 7)
	if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:42\n#EXT-X-DISCONTINUITY-SEQUENCE:7\n") {
		t.Fatalf("playlist missing sequence tags:\n%s", playlist)
	}
	if strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("live playlist unexpectedly ended:\n%s", playlist)
	}
}
