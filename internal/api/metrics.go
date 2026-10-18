package api

import (
	"fmt"
	"net/http"
)

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not read storage metrics")
		return
	}
	queued, running, failed, err := s.store.ClipJobCounts(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not read clip metrics")
		return
	}
	failedTotal, err := s.store.ClipJobFailedTotal(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not read failed clip count")
		return
	}
	latestTime, err := s.store.LatestCommittedEnd(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not read latest committed media time")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP dvr_storage_free_bytes Available bytes on the media filesystem.\n# TYPE dvr_storage_free_bytes gauge\ndvr_storage_free_bytes %d\n", stats.FreeBytes)
	_, _ = fmt.Fprintf(w, "# HELP dvr_storage_used_bytes Bytes used by source media, exported clips, and active temporary exports.\n# TYPE dvr_storage_used_bytes gauge\ndvr_storage_used_bytes %d\n", stats.UsedBytes)
	_, _ = fmt.Fprintf(w, "# HELP dvr_storage_source_bytes Bytes occupied by indexed source fragments and epoch initialization files.\n# TYPE dvr_storage_source_bytes gauge\ndvr_storage_source_bytes %d\n", stats.SourceBytes)
	_, _ = fmt.Fprintf(w, "# HELP dvr_storage_clip_bytes Bytes occupied by exported clips.\n# TYPE dvr_storage_clip_bytes gauge\ndvr_storage_clip_bytes %d\n", stats.ClipBytes)
	_, _ = fmt.Fprintf(w, "# HELP dvr_storage_clip_library_bytes Bytes occupied by saved library clips.\n# TYPE dvr_storage_clip_library_bytes gauge\ndvr_storage_clip_library_bytes %d\n", stats.LibraryBytes)
	_, _ = fmt.Fprintf(w, "# HELP dvr_storage_temporary_clip_bytes Bytes occupied by completed temporary downloads.\n# TYPE dvr_storage_temporary_clip_bytes gauge\ndvr_storage_temporary_clip_bytes %d\n", stats.TemporaryClipBytes)
	_, _ = fmt.Fprintf(w, "# HELP dvr_storage_temp_bytes Bytes occupied by active temporary exports.\n# TYPE dvr_storage_temp_bytes gauge\ndvr_storage_temp_bytes %d\n", stats.TempBytes)
	_, _ = fmt.Fprintf(w, "# HELP dvr_segments_committed Number of committed or retiring media segments.\n# TYPE dvr_segments_committed gauge\ndvr_segments_committed %d\n", stats.SegmentCount)
	active := 0
	var ingestRate, commitLatency, storageWrite float64
	if s.ingest != nil {
		active = s.ingest.ActivePublishers()
		ingestRate = float64(s.ingest.IngestBitsPerSecond())
		commitLatency = s.ingest.SegmentCommitLatency().Seconds()
		storageWrite = s.ingest.StorageWriteLatency().Seconds()
	}
	latestCommitted := float64(0)
	if !latestTime.IsZero() {
		latestCommitted = float64(latestTime.Unix())
	}
	_, _ = fmt.Fprintf(w, "# HELP dvr_ingest_bitrate Current video and AAC payload rate in bits per second over the last five seconds.\n# TYPE dvr_ingest_bitrate gauge\ndvr_ingest_bitrate %.0f\n", ingestRate)
	queueDepth := int64(0)
	if s.ingest != nil {
		queueDepth = s.ingest.PacketQueueDepth()
	}
	_, _ = fmt.Fprintf(w, "# HELP dvr_packet_queue_depth Audio and video samples buffered for the current segment across active publishers.\n# TYPE dvr_packet_queue_depth gauge\ndvr_packet_queue_depth %d\n", queueDepth)
	_, _ = fmt.Fprintf(w, "# HELP dvr_segment_commit_latency Time spent committing the most recent segment, in seconds.\n# TYPE dvr_segment_commit_latency gauge\ndvr_segment_commit_latency %.9f\n", commitLatency)
	_, _ = fmt.Fprintf(w, "# HELP dvr_latest_committed_walltime End time of the most recently committed segment as Unix seconds.\n# TYPE dvr_latest_committed_walltime gauge\ndvr_latest_committed_walltime %.3f\n", latestCommitted)
	_, _ = fmt.Fprintf(w, "# HELP dvr_playlist_generation_latency Time spent building the most recent HLS playlist, in seconds.\n# TYPE dvr_playlist_generation_latency gauge\ndvr_playlist_generation_latency %.9f\n", float64(s.playlistLatencyNS.Load())/1e9)
	_, _ = fmt.Fprintf(w, "# HELP dvr_storage_write_latency Time spent writing, syncing, and publishing the most recent media fragment, in seconds.\n# TYPE dvr_storage_write_latency gauge\ndvr_storage_write_latency %.9f\n", storageWrite)
	_, _ = fmt.Fprintf(w, "# HELP dvr_segment_recovery_total Indexed segments validated during startup recovery.\n# TYPE dvr_segment_recovery_total counter\ndvr_segment_recovery_total %d\n", s.store.RecoveryTotal())
	_, _ = fmt.Fprintf(w, "# HELP dvr_ingest_active_publishers Current authenticated RTMP publishers.\n# TYPE dvr_ingest_active_publishers gauge\ndvr_ingest_active_publishers %d\n", active)
	_, _ = fmt.Fprintf(w, "# HELP dvr_clip_jobs_queued Current queued clip jobs.\n# TYPE dvr_clip_jobs_queued gauge\ndvr_clip_jobs_queued %d\n", queued)
	_, _ = fmt.Fprintf(w, "# HELP dvr_clip_jobs_running Current clip jobs being resolved, encoded, or verified.\n# TYPE dvr_clip_jobs_running gauge\ndvr_clip_jobs_running %d\n", running)
	_, _ = fmt.Fprintf(w, "# HELP dvr_clip_jobs_failed Current failed clip jobs retained in the task history.\n# TYPE dvr_clip_jobs_failed gauge\ndvr_clip_jobs_failed %d\n", failed)
	_, _ = fmt.Fprintf(w, "# HELP dvr_clip_jobs_failed_total Total clip jobs that have entered the failed state since the counter was created.\n# TYPE dvr_clip_jobs_failed_total counter\ndvr_clip_jobs_failed_total %d\n", failedTotal)
}
