package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"backtrack/internal/config"
	"backtrack/internal/model"
	"backtrack/internal/storage"
)

func TestVODPlaylistOnlyEndsForCompletedSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stream, err := store.CreateStream(ctx, "vod-state", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSession(ctx, stream.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Server.AdminToken = "test-vod-admin-token-which-is-long-enough"
	server := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/playback/vod/"+session.ID+".m3u8", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Server.AdminToken)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("active VOD response = %d %q, want conflict without ENDLIST", response.Code, response.Body.String())
	}
	if err = store.EndSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/playback/vod/"+session.ID+".m3u8", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Server.AdminToken)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("completed VOD response = %d %q, want ended playlist", response.Code, response.Body.String())
	}
}

func TestDVRPlaylistDoesNotEndBeyondCommittedMediaFrontier(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stream, err := store.CreateStream(ctx, "dvr-frontier", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSession(ctx, stream.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-3 * time.Second).Truncate(time.Millisecond)
	epoch := model.Epoch{ID: "dvr-epoch", SessionID: session.ID, StartedAt: at, InitPath: filepath.Join("streams", stream.ID, session.ID, "dvr-epoch", "init.mp4"), CodecConfigHash: "codec", VideoTimescale: 90000}
	if err = store.WriteImmutableFile(epoch.InitPath, []byte("init")); err != nil {
		t.Fatal(err)
	}
	if err = store.BeginEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	_, err = store.Commit(ctx, storage.PendingSegment{Segment: model.Segment{StreamID: stream.ID, SessionID: session.ID, EpochID: epoch.ID, Sequence: 1, StartTime: at, EndTime: at.Add(2 * time.Second), TimeScale: 90000, RandomAccess: true}, Data: []byte("fragment")})
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Server.AdminToken = "test-dvr-admin-token-which-is-long-enough"
	server := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
	request := func(end time.Time) *httptest.ResponseRecorder {
		t.Helper()
		url := "/api/playback/dvr/" + stream.ID + ".m3u8?start=" + at.Format(time.RFC3339Nano) + "&end=" + end.Format(time.RFC3339Nano)
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.Server.AdminToken)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	response := request(at.Add(4 * time.Second))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("DVR range extending beyond committed frontier = %d %q, want an extendable playlist", response.Code, response.Body.String())
	}
	response = request(at.Add(time.Second))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("fixed DVR range within committed media = %d %q, want a completed snapshot", response.Code, response.Body.String())
	}
}
