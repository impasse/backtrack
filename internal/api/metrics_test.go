package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"backtrack/internal/config"
	"backtrack/internal/storage"
)

func TestMetricsExposePlanRuntimeSignals(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var cfg config.Config
	cfg.Server.AdminToken = "test-metrics-admin-token-which-is-long-enough"
	server := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Server.AdminToken)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, name := range []string{
		"dvr_ingest_bitrate",
		"dvr_packet_queue_depth",
		"dvr_segment_commit_latency",
		"dvr_latest_committed_walltime",
		"dvr_playlist_generation_latency",
		"dvr_storage_free_bytes",
		"dvr_storage_write_latency",
		"dvr_clip_jobs_running",
		"dvr_clip_jobs_failed_total",
		"dvr_segment_recovery_total",
	} {
		if !strings.Contains(response.Body.String(), name) {
			t.Errorf("/metrics response omitted %s", name)
		}
	}
}
