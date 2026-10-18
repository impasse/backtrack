package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"backtrack/internal/model"
)

func TestDeleteLastSegmentReclaimsEndedEpochInit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stream, err := store.CreateStream(ctx, "epoch-init-retention", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSession(ctx, stream.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	epoch := model.Epoch{ID: "retained-epoch", SessionID: session.ID, StartedAt: time.Now().UTC(), InitPath: filepath.Join("streams", stream.ID, session.ID, "retained-epoch", "init.mp4"), CodecConfigHash: "codec", VideoTimescale: 90000}
	initData := []byte("init-file")
	if err = store.WriteImmutableFile(epoch.InitPath, initData); err != nil {
		t.Fatal(err)
	}
	if err = store.BeginEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	segment, err := store.Commit(ctx, PendingSegment{Segment: model.Segment{StreamID: stream.ID, SessionID: session.ID, EpochID: epoch.ID, Sequence: 1, StartTime: now, EndTime: now.Add(time.Second), TimeScale: 90000, RandomAccess: true}, Data: []byte("fragment")})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EndEpoch(ctx, epoch.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	freed, err := store.DeleteSegmentWithFreedBytes(ctx, segment.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	wantFreed := int64(len(initData) + len("fragment"))
	if freed != wantFreed {
		t.Fatalf("freed bytes = %d, want %d", freed, wantFreed)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.InitBytes != 0 || stats.SourceBytes != 0 || stats.SegmentCount != 0 {
		t.Fatalf("stats after deleting ended epoch = %#v, want no source media", stats)
	}
	if _, err = store.OpenInit(ctx, epoch.ID); err == nil {
		t.Fatal("ended epoch init file still exists after its last segment was deleted")
	}
}
