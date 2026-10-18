package retention

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"backtrack/internal/config"
	"backtrack/internal/model"
	"backtrack/internal/storage"
)

func TestCleanupAppliesIndependentClipQuotasAndPreservesPinnedExports(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stream, err := store.CreateStream(ctx, "clip-quota", "secret")
	if err != nil {
		t.Fatal(err)
	}
	createClip := func(name string, size int, saved, pinned bool) string {
		t.Helper()
		now := time.Now().UTC()
		job, createErr := store.CreateClipJob(ctx, model.ClipRequest{StreamID: stream.ID, StartTime: now, EndTime: now.Add(time.Second)})
		if createErr != nil {
			t.Fatal(createErr)
		}
		rel := filepath.Join("clips", name+".mp4")
		if createErr = store.WriteImmutableFile(rel, make([]byte, size)); createErr != nil {
			t.Fatal(createErr)
		}
		expires := now.Add(time.Hour)
		if saved {
			expires = time.Time{}
		}
		if createErr = store.CompleteClip(ctx, job.ID, rel, int64(size), saved, expires); createErr != nil {
			t.Fatal(createErr)
		}
		if pinned {
			if createErr = store.SetClipPinned(ctx, job.ID, true); createErr != nil {
				t.Fatal(createErr)
			}
		}
		return job.ID
	}
	oldSaved := createClip("old-saved", 7, true, false)
	createClip("pinned-saved", 11, true, true)
	oldTemporary := createClip("old-temp", 5, false, false)
	cfg := config.Config{}
	cfg.Retention.MaxAge = config.Duration(24 * time.Hour)
	cfg.Retention.MaxSize = config.ByteSize(1 << 30)
	cfg.Retention.CleanupInterval = config.Duration(time.Minute)
	cfg.Clips.LibraryMaxSize = config.ByteSize(10)
	cfg.Clips.TemporaryMaxSize = config.ByteSize(3)
	cfg.Storage.ReservedFreeSpace = 0
	manager := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err = manager.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LibraryBytes != 11 || stats.TemporaryClipBytes != 0 || stats.ClipBytes != 11 {
		t.Fatalf("clip stats after quota cleanup = library:%d temporary:%d total:%d, want 11/0/11", stats.LibraryBytes, stats.TemporaryClipBytes, stats.ClipBytes)
	}
	if _, err = store.ClipPath(ctx, oldSaved); err == nil {
		t.Fatal("old unpinned saved clip survived its configured quota")
	}
	if _, err = store.ClipPath(ctx, oldTemporary); err == nil {
		t.Fatal("old temporary clip survived its configured quota")
	}
}
