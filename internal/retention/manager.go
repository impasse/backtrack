package retention

import (
	"context"
	"log/slog"
	"time"

	"backtrack/internal/config"
	"backtrack/internal/events"
	"backtrack/internal/storage"
)

type Manager struct {
	cfg    config.Config
	store  *storage.Store
	logger *slog.Logger
	events *events.Hub
}

func New(cfg config.Config, store *storage.Store, logger *slog.Logger, eventHub *events.Hub) *Manager {
	return &Manager{cfg: cfg, store: store, logger: logger, events: eventHub}
}

func (m *Manager) Run(ctx context.Context) {
	interval := time.Duration(m.cfg.Retention.CleanupInterval)
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Cleanup(ctx); err != nil {
				m.logger.Error("retention cleanup failed", "error", err)
			}
		}
	}
}

func (m *Manager) Cleanup(ctx context.Context) error {
	expired, err := m.store.ExpiredClipJobs(ctx)
	if err != nil {
		return err
	}
	for _, id := range expired {
		if err = m.store.DeleteClip(ctx, id); err != nil {
			m.logger.Warn("expired clip cleanup failed", "job_id", id, "error", err)
		}
	}
	defaults := storage.RetentionPolicy{MaxAgeHours: time.Duration(m.cfg.Retention.MaxAge).Hours(), MaxSizeBytes: int64(m.cfg.Retention.MaxSize), PreservePinned: m.cfg.Retention.PreservePinned}
	policy, err := m.store.GetRetentionPolicy(ctx, defaults)
	if err != nil {
		return err
	}
	stats, err := m.store.Stats(ctx)
	if err != nil {
		return err
	}
	for _, quota := range []struct {
		saved bool
		limit int64
		used  *int64
		name  string
	}{
		{saved: true, limit: int64(m.cfg.Clips.LibraryMaxSize), used: &stats.LibraryBytes, name: "saved clip library"},
		{saved: false, limit: int64(m.cfg.Clips.TemporaryMaxSize), used: &stats.TemporaryClipBytes, name: "temporary clips"},
	} {
		if quota.limit <= 0 || *quota.used <= quota.limit {
			continue
		}
		candidates, queryErr := m.store.ClipRetentionCandidates(ctx, quota.saved)
		if queryErr != nil {
			return queryErr
		}
		for _, candidate := range candidates {
			if *quota.used <= quota.limit {
				break
			}
			freedBytes, deleteErr := m.store.DeleteClipWithFreedBytes(ctx, candidate.JobID)
			if deleteErr != nil {
				m.logger.Warn("clip storage quota skipped export", "job_id", candidate.JobID, "quota", quota.name, "error", deleteErr)
				continue
			}
			*quota.used -= freedBytes
		}
	}
	stats, err = m.store.Stats(ctx)
	if err != nil {
		return err
	}
	candidates, err := m.store.RetentionCandidates(ctx, time.Now().UTC().Add(-time.Duration(policy.MaxAgeHours*float64(time.Hour))), policy.PreservePinned)
	if err != nil {
		return err
	}
	for _, v := range candidates {
		if err = m.store.DeleteSegment(ctx, v.ID, policy.PreservePinned); err != nil {
			m.logger.Warn("retention skipped segment", "segment_id", v.ID, "error", err)
		} else if m.events != nil {
			m.events.Publish(events.Event{Type: "timeline.updated", StreamID: v.StreamID})
		}
	}
	stats, err = m.store.Stats(ctx)
	if err != nil {
		return err
	}
	if stats.SourceBytes > policy.MaxSizeBytes || stats.FreeBytes > 0 && stats.FreeBytes < int64(m.cfg.Storage.ReservedFreeSpace) {
		all, queryErr := m.store.RetentionCandidates(ctx, time.Time{}, policy.PreservePinned)
		if queryErr != nil {
			return queryErr
		}
		for _, v := range all {
			if stats.SourceBytes <= policy.MaxSizeBytes && (stats.FreeBytes == 0 || stats.FreeBytes >= int64(m.cfg.Storage.ReservedFreeSpace)) {
				break
			}
			freedBytes, deleteErr := m.store.DeleteSegmentWithFreedBytes(ctx, v.ID, policy.PreservePinned)
			if deleteErr != nil {
				continue
			}
			stats.SourceBytes -= freedBytes
			stats.UsedBytes -= freedBytes
			stats.FreeBytes += freedBytes
		}
	}
	if stats.SourceBytes > policy.MaxSizeBytes || stats.FreeBytes > 0 && stats.FreeBytes < int64(m.cfg.Storage.ReservedFreeSpace) {
		m.logger.Error("storage pressure remains after retention; new ingest should be refused", "source_bytes", stats.SourceBytes, "used_bytes", stats.UsedBytes, "free_bytes", stats.FreeBytes)
		if m.events != nil {
			m.events.Publish(events.Event{Type: "storage.warning", FreeBytes: stats.FreeBytes})
		}
	}
	return nil
}
