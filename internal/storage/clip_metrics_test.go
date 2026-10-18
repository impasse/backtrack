package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"backtrack/internal/model"
)

func TestClipFailureCounterCountsTransitionsAndSurvivesDeletion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stream, err := store.CreateStream(ctx, "failed-clip-counter", "secret")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateClipJob(ctx, model.ClipRequest{StreamID: stream.ID, StartTime: time.Now().UTC(), EndTime: time.Now().UTC().Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateClipJob(ctx, job.ID, "FAILED", "expected test failure", 0); err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateClipJob(ctx, job.ID, "FAILED", "same failure state", 0); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteClip(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	count, err := store.ClipJobFailedTotal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("failed total = %d, want 1", count)
	}
}
