package playback

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"backtrack/internal/media"
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

func LowLatencyPlaylist(segments []model.Segment, parts []media.Part, initURL func(string) string, discontinuitySequence uint64, partTarget time.Duration, streamID string, active bool) string {
	if partTarget <= 0 {
		partTarget = 500 * time.Millisecond
	}
	maxDuration := 1.0
	for _, segment := range segments {
		if duration := segment.EndTime.Sub(segment.StartTime).Seconds(); duration > maxDuration {
			maxDuration = duration
		}
	}
	groups := make(map[string][]media.Part)
	sort.Slice(parts, func(i, j int) bool { return parts[i].StartTime.Before(parts[j].StartTime) })
	for _, part := range parts {
		key := fmt.Sprintf("%s/%d", part.EpochID, part.Sequence)
		groups[key] = append(groups[key], part)
	}
	for key := range groups {
		group := groups[key]
		for i := 1; i < len(group); i++ {
			for j := i; j > 0 && group[j].Index < group[j-1].Index; j-- {
				group[j], group[j-1] = group[j-1], group[j]
			}
		}
		groups[key] = group
	}
	partSeconds := partTarget.Seconds()
	holdBack := partSeconds * 3
	mediaSequence := uint64(0)
	if len(segments) > 0 {
		mediaSequence = segments[0].Sequence
	} else if len(parts) > 0 {
		mediaSequence = parts[0].Sequence
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:9\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", int(math.Ceil(maxDuration)), mediaSequence)
	fmt.Fprintf(&b, "#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=%.3f\n", holdBack)
	fmt.Fprintf(&b, "#EXT-X-PART-INF:PART-TARGET=%.3f\n", partSeconds)
	if discontinuitySequence > 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", discontinuitySequence)
	}
	lastEpoch, lastSession := "", ""
	emitted := make(map[string]bool)
	for i, segment := range segments {
		if i > 0 && (segment.EpochID != lastEpoch || segment.SessionID != lastSession || segment.StartTime.Sub(segments[i-1].EndTime) > time.Second) {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if segment.EpochID != lastEpoch {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initURL(segment.EpochID))
		}
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", segment.StartTime.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
		key := fmt.Sprintf("%s/%d", segment.EpochID, segment.Sequence)
		for _, part := range groups[key] {
			writeLLPart(&b, part, streamID)
		}
		if len(groups[key]) > 0 {
			emitted[key] = true
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n/api/media/%s.m4s\n", segment.EndTime.Sub(segment.StartTime).Seconds(), segment.ID)
		lastEpoch, lastSession = segment.EpochID, segment.SessionID
	}
	var openParts []media.Part
	for key, group := range groups {
		if emitted[key] {
			continue
		}
		if len(openParts) == 0 || group[len(group)-1].EndTime.After(openParts[len(openParts)-1].EndTime) {
			openParts = group
		}
	}
	if len(openParts) > 0 {
		open := openParts[0]
		if lastEpoch != "" && lastEpoch != open.EpochID {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if lastEpoch != open.EpochID {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initURL(open.EpochID))
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", open.StartTime.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
		}
		for _, part := range openParts {
			writeLLPart(&b, part, streamID)
		}
		if active {
			nextSequence := open.Sequence
			nextIndex := openParts[len(openParts)-1].Index + 1
			fmt.Fprintf(&b, "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"/api/media/parts/%s-s%d-p%d.m4s?streamId=%s\"\n", open.EpochID, nextSequence, nextIndex, url.QueryEscape(streamID))
		}
	} else if active && len(segments) > 0 {
		last := segments[len(segments)-1]
		fmt.Fprintf(&b, "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"/api/media/parts/%s-s%d-p0.m4s?streamId=%s\"\n", last.EpochID, last.Sequence+1, url.QueryEscape(streamID))
	}
	return b.String()
}

func writeLLPart(b *strings.Builder, part media.Part, streamID string) {
	fmt.Fprintf(b, "#EXT-X-PART:DURATION=%.3f,URI=\"/api/media/parts/%s.m4s?streamId=%s\"", part.EndTime.Sub(part.StartTime).Seconds(), part.ID, url.QueryEscape(streamID))
	if part.Independent {
		b.WriteString(",INDEPENDENT=YES")
	}
	b.WriteByte('\n')
}
