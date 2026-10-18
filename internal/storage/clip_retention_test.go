package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"backtrack/internal/model"
)

func TestClipStorageStatsAndQuotaCandidatesSeparateLibraryAndTemporaryFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stream, err := store.CreateStream(ctx, "clip-retention", "secret")
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
	createClip("saved-old", 7, true, false)
	createClip("saved-pinned", 11, true, true)
	createClip("temporary", 5, false, false)
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ClipBytes != 23 || stats.LibraryBytes != 18 || stats.TemporaryClipBytes != 5 || stats.UsedBytes != 23 {
		t.Fatalf("clip storage stats = %#v, want all/library/temp/used = 23/18/5/23", stats)
	}
	library, err := store.ClipRetentionCandidates(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(library) != 1 || library[0].SizeBytes != 7 {
		t.Fatalf("saved clip eviction candidates = %#v, want only the unpinned 7-byte export", library)
	}
	temporary, err := store.ClipRetentionCandidates(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 1 || temporary[0].SizeBytes != 5 {
		t.Fatalf("temporary clip eviction candidates = %#v, want the unpinned 5-byte export", temporary)
	}
}
