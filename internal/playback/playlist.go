package playback

import (
	"fmt"
	"math"
	"strings"
	"time"

	"backtrack/internal/model"
)

func Playlist(segments []model.Segment, initURL func(string) string, endList bool, discontinuitySequence uint64) string {
	var b strings.Builder
	maxDuration := 1.0
	for _, s := range segments {
		d := s.EndTime.Sub(s.StartTime).Seconds()
		if d > maxDuration {
			maxDuration = d
		}
	}
	if len(segments) == 0 {
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		if endList {
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		return b.String()
	}
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", int(math.Ceil(maxDuration)), segments[0].Sequence)
	if discontinuitySequence > 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", discontinuitySequence)
	}
	lastEpoch, lastSession := "", ""
	for i, s := range segments {
		if i > 0 && (s.EpochID != lastEpoch || s.SessionID != lastSession || s.StartTime.Sub(segments[i-1].EndTime) > time.Second) {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if s.EpochID != lastEpoch {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initURL(s.EpochID))
		}
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", s.StartTime.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n/api/media/%s.m4s\n", s.EndTime.Sub(s.StartTime).Seconds(), s.ID)
		lastEpoch, lastSession = s.EpochID, s.SessionID
	}
	if endList {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}
