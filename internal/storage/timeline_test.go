package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"backtrack/internal/model"
)

func TestTimelineGapDoesNotOverlapCommittedMedia(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stream, err := store.CreateStream(ctx, "timeline", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSession(ctx, stream.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	epoch := model.Epoch{ID: "timeline-epoch", SessionID: session.ID, StartedAt: time.Now().UTC(), InitPath: filepath.Join("streams", stream.ID, session.ID, "timeline-epoch", "init.mp4"), CodecConfigHash: "codec", VideoTimescale: 90000}
	if err = store.WriteImmutableFile(epoch.InitPath, []byte("init")); err != nil {
		t.Fatal(err)
	}
	if err = store.BeginEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	for i, span := range [][2]int{{1, 4}, {6, 8}} {
		_, err = store.Commit(ctx, PendingSegment{Segment: model.Segment{StreamID: stream.ID, SessionID: session.ID, EpochID: epoch.ID, Sequence: uint64(i + 1), StartTime: base.Add(time.Duration(span[0]) * time.Second), EndTime: base.Add(time.Duration(span[1]) * time.Second), TimeScale: 90000, RandomAccess: true}, Data: []byte{byte(i + 1)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.InitBytes != 4 || stats.SourceBytes != 6 || stats.UsedBytes != 6 {
		t.Fatalf("storage stats = init:%d source:%d used:%d, want 4/6/6", stats.InitBytes, stats.SourceBytes, stats.UsedBytes)
	}
	if err = os.MkdirAll(filepath.Join(root, "data", "tmp", "exports", "job"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "data", "tmp", "exports", "job", "part.mp4"), []byte("temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats, err = store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TempBytes != 4 || stats.UsedBytes != 10 {
		t.Fatalf("storage stats with temporary export = temp:%d used:%d, want 4/10", stats.TempBytes, stats.UsedBytes)
	}
	segments, err := store.Segments(ctx, stream.ID, base, base.Add(10*time.Second))
	if err != nil || len(segments) != 2 {
		t.Fatalf("query test segments = %d, %v", len(segments), err)
	}
	if err = store.SetSegmentPinned(ctx, segments[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateGap(ctx, model.Gap{StreamID: stream.ID, SessionID: session.ID, StartTime: base.Add(2 * time.Second), EndTime: base.Add(7 * time.Second), Reason: "stream_disconnected"}); err != nil {
		t.Fatal(err)
	}

	ranges, err := store.Timeline(ctx, stream.ID, base.Add(time.Second), base.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 3 {
		t.Fatalf("timeline returned %d ranges, want 3: %#v", len(ranges), ranges)
	}
	want := []struct {
		start, end int
		available  bool
	}{
		{1, 4, true},
		{4, 6, false},
		{6, 8, true},
	}
	for i, v := range ranges {
		gotStart := int(v.Start.Sub(base).Seconds())
		gotEnd := int(v.End.Sub(base).Seconds())
		if gotStart != want[i].start || gotEnd != want[i].end || v.Available != want[i].available {
			t.Fatalf("range %d = [%d,%d) available=%v, want [%d,%d) available=%v", i, gotStart, gotEnd, v.Available, want[i].start, want[i].end, want[i].available)
		}
	}
	if len(ranges[0].SegmentIDs) != 1 || ranges[0].SegmentIDs[0] != segments[0].ID || !ranges[0].Pinned || ranges[2].Pinned {
		t.Fatalf("timeline segment protection metadata = %#v, %#v", ranges[0], ranges[2])
	}
	sequence, err := store.DiscontinuitySequence(ctx, stream.ID, base.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 1 {
		t.Fatalf("discontinuity sequence = %d, want 1", sequence)
	}

	buckets, err := store.TimelineBuckets(ctx, stream.ID, base, base.Add(48*time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 || buckets[0].RecordedMS != 5_000 || buckets[0].SegmentCount != 2 || buckets[1].RecordedMS != 0 || buckets[1].SegmentCount != 0 {
		t.Fatalf("coarse timeline buckets = %#v, want two days with 5s/2 segments then empty", buckets)
	}
}
