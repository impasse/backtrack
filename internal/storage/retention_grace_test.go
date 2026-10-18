package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"backtrack/internal/model"
)

func TestRetentionCandidatesRespectPublishedSegmentCacheGrace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stream, err := store.CreateStream(ctx, "retention-grace", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSession(ctx, stream.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	epoch := model.Epoch{ID: "retention-epoch", SessionID: session.ID, StartedAt: time.Now().UTC().Add(-time.Hour), InitPath: filepath.Join("streams", stream.ID, session.ID, "retention-epoch", "init.mp4"), CodecConfigHash: "codec", VideoTimescale: 90000}
	if err = store.WriteImmutableFile(epoch.InitPath, []byte("init")); err != nil {
		t.Fatal(err)
	}
	if err = store.BeginEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, age := range []time.Duration{3 * time.Minute, 10 * time.Second} {
		end := now.Add(-age)
		_, err = store.Commit(ctx, PendingSegment{Segment: model.Segment{
			StreamID: stream.ID, SessionID: session.ID, EpochID: epoch.ID, Sequence: uint64(i + 1),
			StartTime: end.Add(-time.Second), EndTime: end, TimeScale: 90000, RandomAccess: true,
		}, Data: []byte{byte(i + 1)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := store.RetentionCandidates(ctx, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Sequence != 1 {
		t.Fatalf("retention candidates = %#v, want only the segment older than the published-URL grace period", candidates)
	}
}
