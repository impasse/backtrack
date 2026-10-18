package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"backtrack/internal/model"
)

func TestRecoverQuarantinesCommittedSegmentWithBadChecksum(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stream, err := store.CreateStream(ctx, "test", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSession(ctx, stream.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	epoch := model.Epoch{ID: "epoch-test", SessionID: session.ID, StartedAt: time.Now().UTC(), InitPath: filepath.Join("streams", stream.ID, session.ID, "epoch-test", "init.mp4"), CodecConfigHash: "codec", VideoTimescale: 90000}
	if err = store.WriteImmutableFile(epoch.InitPath, []byte("init")); err != nil {
		t.Fatal(err)
	}
	if err = store.BeginEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Millisecond)
	segment, err := store.Commit(ctx, PendingSegment{Segment: model.Segment{StreamID: stream.ID, SessionID: session.ID, EpochID: epoch.ID, Sequence: 1, StartTime: start, EndTime: start.Add(2 * time.Second), TimeScale: 90000, RandomAccess: true}, Data: []byte("valid-fragment")})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root(), segment.Path), []byte("bad-fragment!!"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err = store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetSegment(ctx, segment.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("quarantined segment lookup error = %v, want sql.ErrNoRows", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), "lost+found"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("lost+found has %d entries, want 1", len(entries))
	}
}
