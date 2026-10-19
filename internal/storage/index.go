package storage

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"backtrack/internal/model"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type Store struct {
	db            *sql.DB
	root          string
	database      string
	recoveryTotal atomic.Uint64
	writeLatency  atomic.Int64
}

var ErrInvalidSessionMerge = errors.New("invalid session merge")

const (
	SegmentURLCacheMaxAge = 60 * time.Second
	segmentRetentionGrace = SegmentURLCacheMaxAge + 5*time.Second
)

var ErrStreamHasActiveClipJobs = errors.New("channel has active clip exports")

func Open(root, database string) (*Store, error) {
	var err error
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	database, err = filepath.Abs(database)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(database), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{db: db, root: root, database: database}
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS session_counter(singleton INTEGER PRIMARY KEY CHECK(singleton=1), value INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS streams(id TEXT PRIMARY KEY, name TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, publish_key_hash TEXT NOT NULL, publish_key_encrypted TEXT NOT NULL DEFAULT '', created_at_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, stream_id TEXT NOT NULL REFERENCES streams(id), started_at_ms INTEGER NOT NULL, ended_at_ms INTEGER, status TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_by_stream_start ON sessions(stream_id, started_at_ms DESC);
CREATE TABLE IF NOT EXISTS epochs(id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id), started_at_ms INTEGER NOT NULL, ended_at_ms INTEGER, init_path TEXT NOT NULL, codec_config_hash TEXT NOT NULL, video_timescale INTEGER NOT NULL DEFAULT 0, audio_timescale INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS segments(id TEXT PRIMARY KEY, stream_id TEXT NOT NULL REFERENCES streams(id), session_id TEXT NOT NULL REFERENCES sessions(id), epoch_id TEXT NOT NULL REFERENCES epochs(id), seq INTEGER NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, first_pts INTEGER NOT NULL, last_pts INTEGER NOT NULL, first_dts INTEGER NOT NULL, last_dts INTEGER NOT NULL, timescale INTEGER NOT NULL, size_bytes INTEGER NOT NULL, path TEXT NOT NULL UNIQUE, random_access INTEGER NOT NULL, checksum TEXT NOT NULL, state TEXT NOT NULL, pinned INTEGER NOT NULL DEFAULT 0, created_at_ms INTEGER NOT NULL, UNIQUE(epoch_id, seq), CHECK(end_ms > start_ms));
CREATE INDEX IF NOT EXISTS segments_by_time ON segments(stream_id, start_ms, end_ms, state);
CREATE INDEX IF NOT EXISTS segments_by_epoch_seq ON segments(epoch_id, seq);
CREATE TABLE IF NOT EXISTS recording_gaps(id TEXT PRIMARY KEY, stream_id TEXT NOT NULL REFERENCES streams(id), session_id TEXT REFERENCES sessions(id), start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, reason TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS merged_history_ranges(id TEXT PRIMARY KEY, stream_id TEXT NOT NULL REFERENCES streams(id), session_id TEXT NOT NULL REFERENCES sessions(id), start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, created_at_ms INTEGER NOT NULL, CHECK(end_ms > start_ms));
CREATE INDEX IF NOT EXISTS merged_history_ranges_by_stream_time ON merged_history_ranges(stream_id, start_ms, end_ms);
CREATE TABLE IF NOT EXISTS clip_jobs(id TEXT PRIMARY KEY, stream_id TEXT NOT NULL REFERENCES streams(id), request_json TEXT NOT NULL, status TEXT NOT NULL, progress REAL NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS clips(id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES clip_jobs(id), path TEXT NOT NULL UNIQUE, size_bytes INTEGER NOT NULL, saved_to_library INTEGER NOT NULL DEFAULT 0, pinned INTEGER NOT NULL DEFAULT 0, expires_at_ms INTEGER);
CREATE TABLE IF NOT EXISTS retention_policies(stream_id TEXT PRIMARY KEY REFERENCES streams(id), max_age_ms INTEGER, max_size_bytes INTEGER, updated_at_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS segment_leases(id TEXT PRIMARY KEY, segment_id TEXT NOT NULL REFERENCES segments(id), owner_id TEXT NOT NULL, expires_at_ms INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS segment_leases_expiry ON segment_leases(expires_at_ms);
CREATE TABLE IF NOT EXISTS storage_volumes(id TEXT PRIMARY KEY, root_path TEXT NOT NULL UNIQUE, enabled INTEGER NOT NULL DEFAULT 1, updated_at_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS auth_sessions(id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, expires_at_ms INTEGER NOT NULL, role TEXT NOT NULL DEFAULT 'admin');
CREATE TABLE IF NOT EXISTS system_settings(key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at_ms INTEGER NOT NULL);
INSERT OR IGNORE INTO schema_migrations(version, applied_at_ms) VALUES(1, strftime('%s','now') * 1000);
`)
	if err != nil {
		return err
	}
	var encryptedKeyColumn int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('streams') WHERE name='publish_key_encrypted'`).Scan(&encryptedKeyColumn); err != nil {
		return err
	}
	if encryptedKeyColumn == 0 {
		if _, err = s.db.Exec(`ALTER TABLE streams ADD COLUMN publish_key_encrypted TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	var roleColumn int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('auth_sessions') WHERE name='role'`).Scan(&roleColumn); err != nil {
		return err
	}
	if roleColumn == 0 {
		_, err = s.db.Exec(`ALTER TABLE auth_sessions ADD COLUMN role TEXT NOT NULL DEFAULT 'admin'`)
	}
	if err != nil {
		return err
	}
	var sessionIDsMigrated int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=2`).Scan(&sessionIDsMigrated); err != nil {
		return err
	}
	if sessionIDsMigrated == 0 {
		if err = s.migrateSessionIDs(context.Background()); err != nil {
			return err
		}
	}
	var globalSessionIDsMigrated int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=3`).Scan(&globalSessionIDsMigrated); err != nil {
		return err
	}
	if globalSessionIDsMigrated == 0 {
		if err = s.migrateGlobalSessionIDs(context.Background()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateSessionIDs(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,started_at_ms FROM sessions ORDER BY started_at_ms,id`)
	if err != nil {
		return err
	}
	type sessionRow struct {
		id, day string
		started int64
	}
	items := make([]sessionRow, 0)
	for rows.Next() {
		var item sessionRow
		if err = rows.Scan(&item.id, &item.started); err != nil {
			rows.Close()
			return err
		}
		item.day = time.UnixMilli(item.started).Local().Format("20060102")
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	counts := map[string]int{}
	for _, item := range items {
		counts[item.day]++
		newID := fmt.Sprintf("%s%02d", item.day, counts[item.day])
		if _, err = tx.ExecContext(ctx, `UPDATE epochs SET session_id=? WHERE session_id=?`, newID, item.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE segments SET session_id=? WHERE session_id=?`, newID, item.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recording_gaps SET session_id=? WHERE session_id=?`, newID, item.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE merged_history_ranges SET session_id=? WHERE session_id=?`, newID, item.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET id=? WHERE id=?`, newID, item.id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at_ms) VALUES(2,?)`, time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) migrateGlobalSessionIDs(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions ORDER BY started_at_ms,id`)
	if err != nil {
		return err
	}
	type sessionIDChange struct{ oldID, temporaryID, newID string }
	changes := make([]sessionIDChange, 0)
	for rows.Next() {
		var oldID string
		if err = rows.Scan(&oldID); err != nil {
			rows.Close()
			return err
		}
		changes = append(changes, sessionIDChange{oldID: oldID, temporaryID: "__session_migration_" + uuid.NewString()})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for i := range changes {
		changes[i].newID = fmt.Sprintf("%d", i+1)
		for _, table := range []string{"epochs", "segments", "recording_gaps", "merged_history_ranges"} {
			if _, err = tx.ExecContext(ctx, `UPDATE `+table+` SET session_id=? WHERE session_id=?`, changes[i].temporaryID, changes[i].oldID); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET id=? WHERE id=?`, changes[i].temporaryID, changes[i].oldID); err != nil {
			return err
		}
	}
	for _, change := range changes {
		for _, table := range []string{"epochs", "segments", "recording_gaps", "merged_history_ranges"} {
			if _, err = tx.ExecContext(ctx, `UPDATE `+table+` SET session_id=? WHERE session_id=?`, change.newID, change.temporaryID); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET id=? WHERE id=?`, change.newID, change.temporaryID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_counter(singleton,value) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET value=excluded.value`, len(changes)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at_ms) VALUES(3,?)`, time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DB() *sql.DB                      { return s.db }
func (s *Store) Root() string                     { return s.root }
func (s *Store) RecoveryTotal() uint64            { return s.recoveryTotal.Load() }
func (s *Store) MediaWriteLatency() time.Duration { return time.Duration(s.writeLatency.Load()) }

func (s *Store) LatestCommittedEnd(ctx context.Context) (time.Time, error) {
	var value int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(end_ms),0) FROM segments WHERE state='COMMITTED'`).Scan(&value); err != nil {
		return time.Time{}, err
	}
	if value == 0 {
		return time.Time{}, nil
	}
	return time.UnixMilli(value).UTC(), nil
}

func (s *Store) LatestCommittedEndForStream(ctx context.Context, streamID string) (time.Time, error) {
	var ms sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(end_ms) FROM segments WHERE stream_id=? AND state='COMMITTED'`, streamID).Scan(&ms)
	if err != nil || !ms.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(ms.Int64).UTC(), nil
}

func (s *Store) WriteImmutableFile(rel string, data []byte) error {
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("invalid storage-relative path")
	}
	dst := filepath.Join(s.root, rel)
	dir := filepath.Dir(dst)
	if err := ensureSafeDir(s.root, dir); err != nil {
		return err
	}
	tmp := dst + "." + uuid.NewString() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	d, err := os.Open(dir)
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	return err
}

func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *Store) SaveAuthSession(ctx context.Context, token, role string, expires time.Time) error {
	if role != "admin" && role != "viewer" {
		return errors.New("invalid authentication role")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at_ms<=?`, time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_sessions(id,token_hash,expires_at_ms,role) VALUES(?,?,?,?)`, uuid.NewString(), HashSecret(token), expires.UnixMilli(), role)
	return err
}

func (s *Store) AuthSessionRole(ctx context.Context, token string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM auth_sessions WHERE token_hash=? AND expires_at_ms>?`, HashSecret(token), time.Now().UTC().UnixMilli()).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func (s *Store) HasAuthSession(ctx context.Context, token string) (bool, error) {
	role, err := s.AuthSessionRole(ctx, token)
	return role != "", err
}

func (s *Store) DeleteAuthSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash=?`, HashSecret(token))
	return err
}

type RetentionPolicy struct {
	MaxAgeHours    float64 `json:"maxAgeHours"`
	MaxSizeBytes   int64   `json:"maxSizeBytes"`
	PreservePinned bool    `json:"preservePinned"`
}

type StorageStats struct {
	Root               string `json:"root"`
	Database           string `json:"database"`
	UsedBytes          int64  `json:"usedBytes"`
	SourceBytes        int64  `json:"sourceBytes"`
	InitBytes          int64  `json:"initBytes"`
	FreeBytes          int64  `json:"freeBytes"`
	SegmentCount       int64  `json:"segmentCount"`
	ClipBytes          int64  `json:"clipBytes"`
	LibraryBytes       int64  `json:"libraryBytes"`
	TemporaryClipBytes int64  `json:"temporaryClipBytes"`
	TempBytes          int64  `json:"tempBytes"`
}

func (s *Store) GetRetentionPolicy(ctx context.Context, defaults RetentionPolicy) (RetentionPolicy, error) {
	read := func(k, fallback string) (string, error) {
		var v string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key=?`, k).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return fallback, nil
		}
		return v, err
	}
	age, err := read("retention.max_age_hours", fmt.Sprint(defaults.MaxAgeHours))
	if err != nil {
		return defaults, err
	}
	size, err := read("retention.max_size_bytes", fmt.Sprint(defaults.MaxSizeBytes))
	if err != nil {
		return defaults, err
	}
	pin, err := read("retention.preserve_pinned", fmt.Sprint(defaults.PreservePinned))
	if err != nil {
		return defaults, err
	}
	var result RetentionPolicy
	if _, err = fmt.Sscan(age, &result.MaxAgeHours); err != nil {
		return defaults, err
	}
	if _, err = fmt.Sscan(size, &result.MaxSizeBytes); err != nil {
		return defaults, err
	}
	if _, err = fmt.Sscan(pin, &result.PreservePinned); err != nil {
		return defaults, err
	}
	return result, nil
}

func (s *Store) SetRetentionPolicy(ctx context.Context, p RetentionPolicy) error {
	if p.MaxAgeHours <= 0 || p.MaxAgeHours > 24*365*10 || p.MaxSizeBytes <= 0 {
		return errors.New("retention age and size must be positive and within supported bounds")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]string{"retention.max_age_hours": fmt.Sprint(p.MaxAgeHours), "retention.max_size_bytes": fmt.Sprint(p.MaxSizeBytes), "retention.preserve_pinned": fmt.Sprint(p.PreservePinned)} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO system_settings(key,value,updated_at_ms) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at_ms=excluded.updated_at_ms`, k, v, time.Now().UTC().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Stats(ctx context.Context) (StorageStats, error) {
	var st StorageStats
	st.Root = s.root
	st.Database = s.database
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0),COUNT(*) FROM segments WHERE state IN ('COMMITTED','RETIRING')`).Scan(&st.SourceBytes, &st.SegmentCount); err != nil {
		return st, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT init_path FROM epochs`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var rel string
		if err = rows.Scan(&rel); err != nil {
			rows.Close()
			return st, err
		}
		rel = filepath.Clean(rel)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			rows.Close()
			return st, errors.New("invalid epoch initialization path")
		}
		root, openErr := os.OpenRoot(s.root)
		if openErr != nil {
			rows.Close()
			return st, openErr
		}
		file, openErr := root.Open(rel)
		_ = root.Close()
		if errors.Is(openErr, os.ErrNotExist) {
			continue
		}
		if openErr != nil {
			rows.Close()
			return st, openErr
		}
		info, statErr := file.Stat()
		_ = file.Close()
		if statErr != nil {
			rows.Close()
			return st, statErr
		}
		if !info.Mode().IsRegular() {
			rows.Close()
			return st, errors.New("epoch initialization path is not a regular file")
		}
		st.InitBytes += info.Size()
	}
	if err = rows.Close(); err != nil {
		return st, err
	}
	st.SourceBytes += st.InitBytes
	clipRows, err := s.db.QueryContext(ctx, `SELECT path,saved_to_library FROM clips`)
	if err != nil {
		return st, err
	}
	clipRoot, err := os.OpenRoot(s.root)
	if err != nil {
		clipRows.Close()
		return st, err
	}
	for clipRows.Next() {
		var rel string
		var saved int
		if err = clipRows.Scan(&rel, &saved); err != nil {
			clipRows.Close()
			_ = clipRoot.Close()
			return st, err
		}
		rel = filepath.Clean(rel)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			clipRows.Close()
			_ = clipRoot.Close()
			return st, errors.New("invalid clip path")
		}
		file, openErr := clipRoot.Open(rel)
		if errors.Is(openErr, os.ErrNotExist) {
			continue
		}
		if openErr != nil {
			clipRows.Close()
			_ = clipRoot.Close()
			return st, openErr
		}
		info, statErr := file.Stat()
		_ = file.Close()
		if statErr != nil {
			clipRows.Close()
			_ = clipRoot.Close()
			return st, statErr
		}
		if !info.Mode().IsRegular() {
			clipRows.Close()
			_ = clipRoot.Close()
			return st, errors.New("clip path is not a regular file")
		}
		st.ClipBytes += info.Size()
		if saved != 0 {
			st.LibraryBytes += info.Size()
		} else {
			st.TemporaryClipBytes += info.Size()
		}
	}
	if err = clipRows.Close(); err != nil {
		_ = clipRoot.Close()
		return st, err
	}
	if err = clipRoot.Close(); err != nil {
		return st, err
	}
	tempRoot := filepath.Join(s.root, "tmp", "exports")
	err = filepath.WalkDir(tempRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		if info.Mode().IsRegular() {
			st.TempBytes += info.Size()
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	st.UsedBytes = st.SourceBytes + st.ClipBytes + st.TempBytes
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.root, &fs); err == nil {
		st.FreeBytes = int64(fs.Bavail) * int64(fs.Bsize)
	}
	return st, nil
}

func (s *Store) ClipJobCounts(ctx context.Context) (queued, running, failed int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,COUNT(*) FROM clip_jobs GROUP BY status`)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err = rows.Scan(&status, &count); err != nil {
			return 0, 0, 0, err
		}
		switch status {
		case "QUEUED":
			queued = count
		case "RUNNING", "RESOLVING", "RESERVED", "VERIFYING":
			running += count
		case "FAILED":
			failed = count
		}
	}
	err = rows.Err()
	return
}

func (s *Store) ClipJobFailedTotal(ctx context.Context) (int64, error) {
	var value int64
	err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM system_settings WHERE key='metrics.clip_jobs_failed_total'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return value, err
}

func (s *Store) EnsureFreeSpace(additionalBytes, reserveBytes int64) (int64, error) {
	if additionalBytes < 0 || reserveBytes < 0 {
		return 0, errors.New("disk space requirements must be non-negative")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.root, &fs); err != nil {
		return 0, err
	}
	free := int64(fs.Bavail) * int64(fs.Bsize)
	if additionalBytes > free || reserveBytes > free-additionalBytes {
		return free, fmt.Errorf("storage free-space reserve would be crossed: free=%d required=%d reserve=%d", free, additionalBytes, reserveBytes)
	}
	return free, nil
}

func scanSegment(row interface{ Scan(...any) error }) (model.Segment, error) {
	var v model.Segment
	var st, en int64
	var ra, pin int
	err := row.Scan(&v.ID, &v.StreamID, &v.SessionID, &v.EpochID, &v.Sequence, &st, &en, &v.FirstPTS, &v.LastPTS, &v.FirstDTS, &v.LastDTS, &v.TimeScale, &v.SizeBytes, &v.Path, &ra, &v.Checksum, &v.State, &pin)
	v.StartTime = time.UnixMilli(st).UTC()
	v.EndTime = time.UnixMilli(en).UTC()
	v.RandomAccess = ra != 0
	v.Pinned = pin != 0
	return v, err
}

func (s *Store) RetentionCandidates(ctx context.Context, cutoff time.Time, preservePinned bool) ([]model.Segment, error) {
	cutoffMS := cutoff.UnixMilli()
	if cutoff.IsZero() {
		cutoffMS = int64(^uint64(0) >> 1)
	}
	retentionCutoff := time.Now().UTC().Add(-segmentRetentionGrace).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT id,stream_id,session_id,epoch_id,seq,start_ms,end_ms,first_pts,last_pts,first_dts,last_dts,timescale,size_bytes,path,random_access,checksum,state,pinned FROM segments WHERE state='COMMITTED' AND (?=0 OR pinned=0) AND end_ms<? AND end_ms<? AND NOT EXISTS(SELECT 1 FROM segment_leases l WHERE l.segment_id=segments.id AND l.expires_at_ms>?) ORDER BY start_ms`, boolInt(preservePinned), cutoffMS, retentionCutoff, time.Now().UTC().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Segment{}
	for rows.Next() {
		v, e := scanSegment(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) LeaseSegments(ctx context.Context, ids []string, owner string, expires time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var state string
		if err = tx.QueryRowContext(ctx, `SELECT state FROM segments WHERE id=?`, id).Scan(&state); err != nil {
			return err
		}
		if state != "COMMITTED" {
			return fmt.Errorf("segment %s is not available", id)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO segment_leases(id,segment_id,owner_id,expires_at_ms) VALUES(?,?,?,?)`, uuid.NewString(), id, owner, expires.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) ReleaseLeases(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM segment_leases WHERE owner_id=?`, owner)
	return err
}

func (s *Store) DeleteSegment(ctx context.Context, id string, preservePinned bool) error {
	_, err := s.DeleteSegmentWithFreedBytes(ctx, id, preservePinned)
	return err
}

// DeleteSegmentWithFreedBytes removes a committed segment and, once its ended
// epoch has no remaining readable segments, removes that epoch's init file too.
// The returned count reflects the actual media bytes unlinked from storage.
func (s *Store) DeleteSegmentWithFreedBytes(ctx context.Context, id string, preservePinned bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var v model.Segment
	var ra, pinned int
	err = tx.QueryRowContext(ctx, `SELECT id,stream_id,session_id,epoch_id,seq,start_ms,end_ms,first_pts,last_pts,first_dts,last_dts,timescale,size_bytes,path,random_access,checksum,state,pinned FROM segments WHERE id=?`, id).Scan(&v.ID, &v.StreamID, &v.SessionID, &v.EpochID, &v.Sequence, new(int64), new(int64), &v.FirstPTS, &v.LastPTS, &v.FirstDTS, &v.LastDTS, &v.TimeScale, &v.SizeBytes, &v.Path, &ra, &v.Checksum, &v.State, &pinned)
	if err != nil {
		return 0, err
	}
	if v.State != "COMMITTED" || preservePinned && pinned != 0 {
		return 0, errors.New("segment is protected or already retiring")
	}
	var leases int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM segment_leases WHERE segment_id=? AND expires_at_ms>?`, id, time.Now().UTC().UnixMilli()).Scan(&leases); err != nil {
		return 0, err
	}
	if leases > 0 {
		return 0, errors.New("segment is in use")
	}
	res, err := tx.ExecContext(ctx, `UPDATE segments SET state='RETIRING' WHERE id=? AND state='COMMITTED' AND (?=0 OR pinned=0)`, id, boolInt(preservePinned))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errors.New("segment is protected or already retiring")
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	rel := filepath.Clean(v.Path)
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return 0, errors.New("invalid segment path")
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return 0, err
	}
	freedBytes := v.SizeBytes
	err = root.Remove(rel)
	if err != nil {
		_ = root.Close()
		return freedBytes, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE segments SET state='DELETED' WHERE id=? AND state='RETIRING'`, id)
	if err != nil {
		_ = root.Close()
		return freedBytes, err
	}
	initBytes, initErr := s.removeUnusedEpochInit(ctx, root, v.EpochID)
	previewErr := root.Remove(filepath.Join("previews", v.ID+".jpg"))
	if errors.Is(previewErr, os.ErrNotExist) {
		previewErr = nil
	}
	closeErr := root.Close()
	return freedBytes + initBytes, errors.Join(initErr, previewErr, closeErr)
}

func (s *Store) removeUnusedEpochInit(ctx context.Context, root *os.Root, epochID string) (int64, error) {
	var rel string
	var ended sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT init_path,ended_at_ms FROM epochs WHERE id=?`, epochID).Scan(&rel, &ended)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !ended.Valid {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var remaining int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE epoch_id=? AND state IN ('COMMITTED','RETIRING')`, epochID).Scan(&remaining); err != nil || remaining > 0 {
		return 0, err
	}
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return 0, errors.New("invalid epoch initialization path")
	}
	file, err := root.Open(rel)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	info, err := file.Stat()
	_ = file.Close()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("epoch initialization path is not a regular file")
	}
	if err = root.Remove(rel); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *Store) CreateStream(ctx context.Context, name, secret string) (model.Stream, error) {
	if name == "" || len(name) > 80 || secret == "" {
		return model.Stream{}, errors.New("name and publish key are required")
	}
	stream := model.Stream{ID: uuid.NewString(), Name: name, Enabled: true, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO streams(id,name,enabled,publish_key_hash,created_at_ms) VALUES(?,?,?,?,?)`, stream.ID, stream.Name, 1, HashSecret(secret), stream.CreatedAt.UnixMilli())
	return stream, err
}

func (s *Store) ListStreams(ctx context.Context) ([]model.Stream, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,enabled,created_at_ms FROM streams ORDER BY created_at_ms DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Stream
	for rows.Next() {
		var v model.Stream
		var enabled int
		var ms int64
		if err = rows.Scan(&v.ID, &v.Name, &enabled, &ms); err != nil {
			return nil, err
		}
		v.Enabled = enabled != 0
		v.CreatedAt = time.UnixMilli(ms).UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetStream(ctx context.Context, id string) (model.Stream, error) {
	var v model.Stream
	var enabled int
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,enabled,created_at_ms FROM streams WHERE id=?`, id).Scan(&v.ID, &v.Name, &enabled, &ms)
	v.Enabled = enabled != 0
	v.CreatedAt = time.UnixMilli(ms).UTC()
	return v, err
}

func (s *Store) StreamPublishKey(ctx context.Context, id string) (string, error) {
	var encrypted string
	err := s.db.QueryRowContext(ctx, `SELECT publish_key_encrypted FROM streams WHERE id=?`, id).Scan(&encrypted)
	return encrypted, err
}

func (s *Store) SetStreamPublishKey(ctx context.Context, id, secret, encrypted string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE streams SET publish_key_hash=?,publish_key_encrypted=? WHERE id=?`, HashSecret(secret), encrypted, id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) SetStreamEnabled(ctx context.Context, id string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE streams SET enabled=? WHERE id=?`, boolInt(enabled), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RenameStream(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return errors.New("channel name must be between 1 and 80 characters")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE streams SET name=? WHERE id=?`, name, id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteStream removes a channel and all of its retained media and exports.
func (s *Store) DeleteStream(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM streams WHERE id=?`, id).Scan(&exists); err != nil {
		return err
	}
	var activeJobs int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM clip_jobs WHERE stream_id=? AND status IN ('QUEUED','RESOLVING','RESERVED','RUNNING','VERIFYING')`, id).Scan(&activeJobs); err != nil {
		return err
	}
	if activeJobs > 0 {
		return ErrStreamHasActiveClipJobs
	}
	paths, err := collectStringColumn(ctx, tx, `SELECT path FROM segments WHERE stream_id=?`, id)
	if err != nil {
		return err
	}
	streamPrefix := filepath.Join("streams", id)
	if err = validatePathsUnder(paths, streamPrefix); err != nil {
		return err
	}
	initPaths, err := collectStringColumn(ctx, tx, `SELECT e.init_path FROM epochs e JOIN sessions s ON s.id=e.session_id WHERE s.stream_id=?`, id)
	if err != nil {
		return err
	}
	if err = validatePathsUnder(initPaths, streamPrefix); err != nil {
		return err
	}
	paths = append(paths, initPaths...)
	clipPaths, err := collectStringColumn(ctx, tx, `SELECT c.path FROM clips c JOIN clip_jobs j ON j.id=c.job_id WHERE j.stream_id=?`, id)
	if err != nil {
		return err
	}
	if err = validatePathsUnder(clipPaths, filepath.Join("clips", "completed")); err != nil {
		return err
	}
	paths = append(paths, clipPaths...)
	jobIDs, err := collectStringColumn(ctx, tx, `SELECT id FROM clip_jobs WHERE stream_id=?`, id)
	if err != nil {
		return err
	}
	for _, statement := range []string{
		`DELETE FROM segment_leases WHERE segment_id IN (SELECT id FROM segments WHERE stream_id=?)`,
		`DELETE FROM segments WHERE stream_id=?`,
		`DELETE FROM recording_gaps WHERE stream_id=?`,
		`DELETE FROM epochs WHERE session_id IN (SELECT id FROM sessions WHERE stream_id=?)`,
		`DELETE FROM sessions WHERE stream_id=?`,
		`DELETE FROM clips WHERE job_id IN (SELECT id FROM clip_jobs WHERE stream_id=?)`,
		`DELETE FROM clip_jobs WHERE stream_id=?`,
		`DELETE FROM retention_policies WHERE stream_id=?`,
		`DELETE FROM streams WHERE id=?`,
	} {
		if _, err = tx.ExecContext(ctx, statement, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	var cleanupErrors []error
	for _, path := range paths {
		if removeErr := root.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, removeErr)
		}
	}
	if filepath.Base(id) == id && id != "." && id != ".." {
		if removeErr := root.RemoveAll(filepath.Join("streams", id)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, removeErr)
		}
	}
	for _, jobID := range jobIDs {
		if filepath.Base(jobID) == jobID && jobID != "." && jobID != ".." {
			if removeErr := root.RemoveAll(filepath.Join("tmp", "exports", jobID)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, removeErr)
			}
		}
	}
	if closeErr := root.Close(); closeErr != nil {
		cleanupErrors = append(cleanupErrors, closeErr)
	}
	return errors.Join(cleanupErrors...)
}

func collectStringColumn(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func validatePathsUnder(paths []string, dir string) error {
	prefix := filepath.Clean(dir) + string(os.PathSeparator)
	for i, path := range paths {
		clean := filepath.Clean(path)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || !strings.HasPrefix(clean, prefix) {
			return errors.New("invalid channel media path")
		}
		paths[i] = clean
	}
	return nil
}

func (s *Store) Sessions(ctx context.Context, streamID string) ([]model.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.stream_id,s.started_at_ms,s.ended_at_ms,s.status,COUNT(g.id) FROM sessions s LEFT JOIN segments g ON g.session_id=s.id AND g.state='COMMITTED' WHERE s.stream_id=? GROUP BY s.id ORDER BY s.started_at_ms DESC`, streamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Session{}
	for rows.Next() {
		var v model.Session
		var started int64
		var ended sql.NullInt64
		if err = rows.Scan(&v.ID, &v.StreamID, &started, &ended, &v.Status, &v.SegmentCount); err != nil {
			return nil, err
		}
		v.StartedAt = time.UnixMilli(started).UTC()
		if ended.Valid {
			e := time.UnixMilli(ended.Int64).UTC()
			v.EndedAt = &e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MergeSessions moves all recording metadata from the selected ended sessions
// into the earliest session and removes the now-empty source session rows.
func (s *Store) MergeSessions(ctx context.Context, streamID string, sessionIDs []string) (string, error) {
	if len(sessionIDs) < 2 || len(sessionIDs) > 50 {
		return "", fmt.Errorf("%w: select between 2 and 50 sessions", ErrInvalidSessionMerge)
	}
	unique := make(map[string]struct{}, len(sessionIDs))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	type sessionRow struct {
		id      string
		started int64
		ended   sql.NullInt64
		status  string
	}
	selected := make([]sessionRow, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		if id == "" {
			return "", fmt.Errorf("%w: invalid session id", ErrInvalidSessionMerge)
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		var v sessionRow
		err = tx.QueryRowContext(ctx, `SELECT id,started_at_ms,ended_at_ms,status FROM sessions WHERE id=? AND stream_id=?`, id, streamID).Scan(&v.id, &v.started, &v.ended, &v.status)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: one or more sessions were not found", ErrInvalidSessionMerge)
		}
		if err != nil {
			return "", err
		}
		if v.status == "recording" || !v.ended.Valid {
			return "", fmt.Errorf("%w: active sessions cannot be merged", ErrInvalidSessionMerge)
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE session_id=? AND state='COMMITTED'`, id).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			return "", fmt.Errorf("%w: sessions without recorded video cannot be merged", ErrInvalidSessionMerge)
		}
		selected = append(selected, v)
	}
	if len(selected) < 2 {
		return "", fmt.Errorf("%w: select at least 2 different sessions", ErrInvalidSessionMerge)
	}
	target := selected[0]
	maxEnd := target.ended.Int64
	for _, v := range selected[1:] {
		if v.started < target.started {
			target = v
		}
		if v.ended.Int64 > maxEnd {
			maxEnd = v.ended.Int64
		}
	}
	for _, v := range selected {
		if v.id == target.id {
			continue
		}
		for _, table := range []string{"epochs", "segments", "recording_gaps", "merged_history_ranges"} {
			if _, err = tx.ExecContext(ctx, `UPDATE `+table+` SET session_id=? WHERE session_id=?`, target.id, v.id); err != nil {
				return "", err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, v.id); err != nil {
			return "", err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET started_at_ms=?,ended_at_ms=?,status='disconnected' WHERE id=?`, target.started, maxEnd, target.id); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return target.id, nil
}

// MergeHistoryRanges joins selected recorded ranges into one virtual history
// range. Segment timestamps and disconnect gaps remain intact; playback skips
// gaps because the DVR playlist only includes committed media segments.
func (s *Store) MergeHistoryRanges(ctx context.Context, streamID string, selectedRanges []model.HistoryRangeSelection) (string, error) {
	if len(selectedRanges) < 2 || len(selectedRanges) > 50 {
		return "", fmt.Errorf("%w: select between 2 and 50 recording ranges", ErrInvalidSessionMerge)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return "", err
	}
	type sessionRow struct {
		id      string
		started int64
		ended   sql.NullInt64
		status  string
	}
	sessions := make(map[string]sessionRow)
	type rangeKey struct {
		sessionID string
		start     int64
		end       int64
	}
	uniqueRanges := make(map[rangeKey]struct{}, len(selectedRanges))
	keys := make([]rangeKey, 0, len(selectedRanges))
	for _, selected := range selectedRanges {
		if selected.SessionID == "" || !selected.End.After(selected.Start) {
			return "", fmt.Errorf("%w: invalid recording range", ErrInvalidSessionMerge)
		}
		key := rangeKey{sessionID: selected.SessionID, start: selected.Start.UnixMilli(), end: selected.End.UnixMilli()}
		if _, ok := uniqueRanges[key]; ok {
			continue
		}
		uniqueRanges[key] = struct{}{}
		keys = append(keys, key)
		v, ok := sessions[selected.SessionID]
		if !ok {
			err = tx.QueryRowContext(ctx, `SELECT id,started_at_ms,ended_at_ms,status FROM sessions WHERE id=? AND stream_id=?`, selected.SessionID, streamID).Scan(&v.id, &v.started, &v.ended, &v.status)
			if errors.Is(err, sql.ErrNoRows) {
				return "", fmt.Errorf("%w: one or more sessions were not found", ErrInvalidSessionMerge)
			}
			if err != nil {
				return "", err
			}
			if v.status == "recording" || !v.ended.Valid {
				return "", fmt.Errorf("%w: active sessions cannot be merged", ErrInvalidSessionMerge)
			}
			sessions[selected.SessionID] = v
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE session_id=? AND stream_id=? AND state='COMMITTED' AND start_ms<? AND end_ms>?`, selected.SessionID, streamID, key.end, key.start).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			return "", fmt.Errorf("%w: recording range has no committed video", ErrInvalidSessionMerge)
		}
	}
	if len(keys) < 2 {
		return "", fmt.Errorf("%w: select at least 2 different recording ranges", ErrInvalidSessionMerge)
	}
	var target sessionRow
	maxEnd := int64(0)
	for _, v := range sessions {
		if target.id == "" || v.started < target.started {
			target = v
		}
		if v.ended.Int64 > maxEnd {
			maxEnd = v.ended.Int64
		}
	}
	if len(sessions) > 1 {
		for _, v := range sessions {
			if v.id == target.id {
				continue
			}
			for _, table := range []string{"epochs", "segments", "recording_gaps", "merged_history_ranges"} {
				if _, err = tx.ExecContext(ctx, `UPDATE `+table+` SET session_id=? WHERE session_id=?`, target.id, v.id); err != nil {
					return "", err
				}
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, v.id); err != nil {
				return "", err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET started_at_ms=?,ended_at_ms=?,status='disconnected' WHERE id=?`, target.started, maxEnd, target.id); err != nil {
			return "", err
		}
	}
	minStart, maxRangeEnd := keys[0].start, keys[0].end
	for _, key := range keys[1:] {
		if key.start < minStart {
			minStart = key.start
		}
		if key.end > maxRangeEnd {
			maxRangeEnd = key.end
		}
	}
	markerRows, err := tx.QueryContext(ctx, `SELECT start_ms,end_ms FROM merged_history_ranges WHERE session_id=? AND start_ms<=? AND end_ms>=?`, target.id, maxRangeEnd, minStart)
	if err != nil {
		return "", err
	}
	for markerRows.Next() {
		var a, b int64
		if err = markerRows.Scan(&a, &b); err != nil {
			markerRows.Close()
			return "", err
		}
		if a < minStart {
			minStart = a
		}
		if b > maxRangeEnd {
			maxRangeEnd = b
		}
	}
	if err = markerRows.Err(); err != nil {
		markerRows.Close()
		return "", err
	}
	if err = markerRows.Close(); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM merged_history_ranges WHERE session_id=? AND start_ms<=? AND end_ms>=?`, target.id, maxRangeEnd, minStart); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO merged_history_ranges(id,stream_id,session_id,start_ms,end_ms,created_at_ms) VALUES(?,?,?,?,?,?)`, uuid.NewString(), streamID, target.id, minStart, maxRangeEnd, time.Now().UTC().UnixMilli()); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return target.id, nil
}

func (s *Store) SessionSegments(ctx context.Context, sessionID string) ([]model.Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,stream_id,session_id,epoch_id,seq,start_ms,end_ms,first_pts,last_pts,first_dts,last_dts,timescale,size_bytes,path,random_access,checksum,state,pinned FROM segments WHERE session_id=? AND state='COMMITTED' ORDER BY start_ms,seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Segment{}
	for rows.Next() {
		var v model.Segment
		var st, en int64
		var ra, pin int
		if err = rows.Scan(&v.ID, &v.StreamID, &v.SessionID, &v.EpochID, &v.Sequence, &st, &en, &v.FirstPTS, &v.LastPTS, &v.FirstDTS, &v.LastDTS, &v.TimeScale, &v.SizeBytes, &v.Path, &ra, &v.Checksum, &v.State, &pin); err != nil {
			return nil, err
		}
		v.StartTime = time.UnixMilli(st).UTC()
		v.EndTime = time.UnixMilli(en).UTC()
		v.RandomAccess = ra != 0
		v.Pinned = pin != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) SessionEnded(ctx context.Context, sessionID string) (bool, error) {
	var ended sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT ended_at_ms FROM sessions WHERE id=?`, sessionID).Scan(&ended)
	return ended.Valid, err
}

func (s *Store) EpochInitPath(ctx context.Context, epochID string) (string, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT init_path FROM epochs WHERE id=?`, epochID).Scan(&p)
	return p, err
}

func (s *Store) EpochAudioTimescale(ctx context.Context, epochID string) (uint32, error) {
	var timescale uint32
	err := s.db.QueryRowContext(ctx, `SELECT audio_timescale FROM epochs WHERE id=?`, epochID).Scan(&timescale)
	return timescale, err
}

func (s *Store) LatestSegments(ctx context.Context, streamID string, duration time.Duration) ([]model.Segment, error) {
	end := time.Now().UTC()
	start := end.Add(-duration)
	return s.Segments(ctx, streamID, start, end)
}

func (s *Store) NextStreamSequence(ctx context.Context, streamID string) (uint64, error) {
	var sequence uint64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM segments WHERE stream_id=?`, streamID).Scan(&sequence)
	return sequence, err
}

func (s *Store) DiscontinuitySequence(ctx context.Context, streamID string, through time.Time) (uint64, error) {
	var count uint64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT start_ms, session_id, epoch_id,
				LAG(end_ms) OVER (ORDER BY start_ms,seq) AS previous_end,
				LAG(session_id) OVER (ORDER BY start_ms,seq) AS previous_session,
				LAG(epoch_id) OVER (ORDER BY start_ms,seq) AS previous_epoch
			FROM segments
			WHERE stream_id=? AND state='COMMITTED' AND start_ms<=?
		)
		WHERE previous_end IS NOT NULL AND (session_id<>previous_session OR epoch_id<>previous_epoch OR start_ms-previous_end>1000)
	`, streamID, through.UnixMilli()).Scan(&count)
	return count, err
}

func (s *Store) AuthenticatePublish(ctx context.Context, id, secret string) (bool, error) {
	var expected string
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT publish_key_hash,enabled FROM streams WHERE id=?`, id).Scan(&expected, &enabled)
	if err != nil {
		return false, err
	}
	actual := HashSecret(secret)
	return enabled != 0 && subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1, nil
}

func (s *Store) BeginSession(ctx context.Context, streamID string, reconnectGrace time.Duration) (model.Session, error) {
	cutoff := time.Now().UTC().Add(-reconnectGrace).UnixMilli()
	var resume model.Session
	var start, ended int64
	err := s.db.QueryRowContext(ctx, `SELECT id,started_at_ms,COALESCE(ended_at_ms,0) FROM sessions WHERE stream_id=? AND status='disconnected' AND ended_at_ms>=? ORDER BY started_at_ms DESC LIMIT 1`, streamID, cutoff).Scan(&resume.ID, &start, &ended)
	if err == nil {
		resume.StreamID = streamID
		resume.StartedAt = time.UnixMilli(start).UTC()
		gapEnd := time.UnixMilli(ended).UTC()
		resume.EndedAt = &gapEnd
		resume.Status = "recording"
		if _, err = s.db.ExecContext(ctx, `UPDATE sessions SET status='recording',ended_at_ms=NULL WHERE id=? AND status='disconnected'`, resume.ID); err != nil {
			return model.Session{}, err
		}
		return resume, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Session{}, err
	}
	startedAt := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Session{}, err
	}
	defer tx.Rollback()
	id, err := nextSessionID(ctx, tx)
	if err != nil {
		return model.Session{}, err
	}
	ss := model.Session{ID: id, StreamID: streamID, StartedAt: startedAt, Status: "recording"}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,stream_id,started_at_ms,status) VALUES(?,?,?,?)`, ss.ID, ss.StreamID, ss.StartedAt.UnixMilli(), ss.Status); err != nil {
		return model.Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Session{}, err
	}
	return ss, nil
}

func nextSessionID(ctx context.Context, tx *sql.Tx) (string, error) {
	result, err := tx.ExecContext(ctx, `UPDATE session_counter SET value=value+1 WHERE singleton=1`)
	if err != nil {
		return "", err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if updated != 1 {
		return "", errors.New("global session counter is not initialized")
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `SELECT value FROM session_counter WHERE singleton=1`).Scan(&sequence); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", sequence), nil
}

func (s *Store) EndSession(ctx context.Context, id string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET ended_at_ms=?, status='disconnected' WHERE id=? AND status='recording'`, now.UnixMilli(), id)
	return err
}

func (s *Store) BeginEpoch(ctx context.Context, e model.Epoch) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO epochs(id,session_id,started_at_ms,init_path,codec_config_hash,video_timescale,audio_timescale) VALUES(?,?,?,?,?,?,?)`, e.ID, e.SessionID, e.StartedAt.UnixMilli(), e.InitPath, e.CodecConfigHash, e.VideoTimescale, e.AudioTimescale)
	return err
}

func (s *Store) EndEpoch(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE epochs SET ended_at_ms=? WHERE id=? AND ended_at_ms IS NULL`, at.UnixMilli(), id)
	return err
}

type PendingSegment struct {
	Segment           model.Segment
	Data              []byte
	ReservedFreeSpace int64
}

func (s *Store) Commit(ctx context.Context, p PendingSegment) (model.Segment, error) {
	v := p.Segment
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	if v.EndTime.IsZero() || !v.EndTime.After(v.StartTime) || len(p.Data) == 0 {
		return v, errors.New("invalid pending segment")
	}
	if _, err := s.EnsureFreeSpace(int64(len(p.Data)), p.ReservedFreeSpace); err != nil {
		return v, err
	}
	rel := filepath.Join("streams", v.StreamID, v.SessionID, v.EpochID, fmt.Sprintf("%08d.m4s", v.Sequence))
	path := filepath.Join(s.root, rel)
	if err := ensureSafeDir(s.root, filepath.Dir(path)); err != nil {
		return v, err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			_ = os.Remove(tmp)
			f, err = os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		}
		if err != nil {
			return v, err
		}
	}
	writeStarted := time.Now()
	_, err = f.Write(p.Data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return v, err
	}
	sum := sha256.Sum256(p.Data)
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return v, err
	}
	if d, e := os.Open(filepath.Dir(path)); e == nil {
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return v, err
		}
	}
	s.writeLatency.Store(time.Since(writeStarted).Nanoseconds())
	v.Path = rel
	v.SizeBytes = int64(len(p.Data))
	v.Checksum = hex.EncodeToString(sum[:])
	v.State = "COMMITTED"
	_, err = s.db.ExecContext(ctx, `INSERT INTO segments(id,stream_id,session_id,epoch_id,seq,start_ms,end_ms,first_pts,last_pts,first_dts,last_dts,timescale,size_bytes,path,random_access,checksum,state,pinned,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.StreamID, v.SessionID, v.EpochID, v.Sequence, v.StartTime.UnixMilli(), v.EndTime.UnixMilli(), v.FirstPTS, v.LastPTS, v.FirstDTS, v.LastDTS, v.TimeScale, v.SizeBytes, v.Path, boolInt(v.RandomAccess), v.Checksum, v.State, boolInt(v.Pinned), time.Now().UTC().UnixMilli())
	if err != nil {
		return v, fmt.Errorf("segment file published but index commit failed; recovery will reconcile it: %w", err)
	}
	return v, nil
}

func ensureSafeDir(root, dir string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator) {
		return errors.New("media path escaped storage root")
	}
	if err = os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("storage root must be a real directory")
	}
	cur := root
	for _, part := range splitPath(rel) {
		cur = filepath.Join(cur, part)
		if e := os.Mkdir(cur, 0o750); e != nil && !errors.Is(e, os.ErrExist) {
			return e
		}
		info, e := os.Lstat(cur)
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in media path")
		}
		if !info.IsDir() {
			return errors.New("media path component is not a directory")
		}
	}
	return nil
}
func splitPath(p string) []string {
	if p == "." || p == "" {
		return nil
	}
	out := []string{}
	for p != "." && p != "" {
		d, b := filepath.Split(p)
		if b != "" {
			out = append([]string{b}, out...)
		}
		p = filepath.Clean(d)
	}
	return out
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) GetSegment(ctx context.Context, id string) (model.Segment, error) {
	var v model.Segment
	var start, end int64
	var random, pinned int
	err := s.db.QueryRowContext(ctx, `SELECT id,stream_id,session_id,epoch_id,seq,start_ms,end_ms,first_pts,last_pts,first_dts,last_dts,timescale,size_bytes,path,random_access,checksum,state,pinned FROM segments WHERE id=? AND state='COMMITTED'`, id).Scan(&v.ID, &v.StreamID, &v.SessionID, &v.EpochID, &v.Sequence, &start, &end, &v.FirstPTS, &v.LastPTS, &v.FirstDTS, &v.LastDTS, &v.TimeScale, &v.SizeBytes, &v.Path, &random, &v.Checksum, &v.State, &pinned)
	v.StartTime = time.UnixMilli(start).UTC()
	v.EndTime = time.UnixMilli(end).UTC()
	v.RandomAccess = random != 0
	v.Pinned = pinned != 0
	return v, err
}

func (s *Store) Segments(ctx context.Context, streamID string, start, end time.Time) ([]model.Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,stream_id,session_id,epoch_id,seq,start_ms,end_ms,first_pts,last_pts,first_dts,last_dts,timescale,size_bytes,path,random_access,checksum,state,pinned FROM segments WHERE stream_id=? AND start_ms<? AND end_ms>? AND state='COMMITTED' ORDER BY start_ms,seq`, streamID, end.UnixMilli(), start.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Segment{}
	for rows.Next() {
		var v model.Segment
		var st, en int64
		var ra, pin int
		err = rows.Scan(&v.ID, &v.StreamID, &v.SessionID, &v.EpochID, &v.Sequence, &st, &en, &v.FirstPTS, &v.LastPTS, &v.FirstDTS, &v.LastDTS, &v.TimeScale, &v.SizeBytes, &v.Path, &ra, &v.Checksum, &v.State, &pin)
		if err != nil {
			return nil, err
		}
		v.StartTime = time.UnixMilli(st).UTC()
		v.EndTime = time.UnixMilli(en).UTC()
		v.RandomAccess = ra != 0
		v.Pinned = pin != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) PreviewSegments(ctx context.Context) ([]model.Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,stream_id,session_id,epoch_id,seq,start_ms,end_ms,first_pts,last_pts,first_dts,last_dts,timescale,size_bytes,path,random_access,checksum,state,pinned FROM segments WHERE state='COMMITTED' AND random_access=1 AND seq%3=0 ORDER BY start_ms,seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Segment{}
	for rows.Next() {
		v, scanErr := scanSegment(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) CreateGap(ctx context.Context, g model.Gap) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO recording_gaps(id,stream_id,session_id,start_ms,end_ms,reason) VALUES(?,?,?,?,?,?)`, uuid.NewString(), g.StreamID, nullString(g.SessionID), g.StartTime.UnixMilli(), g.EndTime.UnixMilli(), g.Reason)
	return err
}
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) Timeline(ctx context.Context, streamID string, start, end time.Time) ([]model.TimeRange, error) {
	segments, err := s.Segments(ctx, streamID, start, end)
	if err != nil {
		return nil, err
	}
	type mergedRange struct {
		sessionID string
		start     time.Time
		end       time.Time
		segments  []string
		pinned    bool
	}
	mergedRows, err := s.db.QueryContext(ctx, `SELECT session_id,start_ms,end_ms FROM merged_history_ranges WHERE stream_id=? AND start_ms<? AND end_ms>? ORDER BY start_ms`, streamID, end.UnixMilli(), start.UnixMilli())
	if err != nil {
		return nil, err
	}
	merged := make([]mergedRange, 0)
	for mergedRows.Next() {
		var v mergedRange
		var a, b int64
		if err = mergedRows.Scan(&v.sessionID, &a, &b); err != nil {
			mergedRows.Close()
			return nil, err
		}
		v.start, v.end, v.pinned = time.UnixMilli(a).UTC(), time.UnixMilli(b).UTC(), true
		merged = append(merged, v)
	}
	if err = mergedRows.Err(); err != nil {
		mergedRows.Close()
		return nil, err
	}
	if err = mergedRows.Close(); err != nil {
		return nil, err
	}
	gaps, err := s.db.QueryContext(ctx, `SELECT start_ms,end_ms,reason,COALESCE(session_id,'') FROM recording_gaps WHERE stream_id=? AND start_ms<? AND end_ms>? ORDER BY start_ms`, streamID, end.UnixMilli(), start.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer gaps.Close()
	available := make([]model.TimeRange, 0, len(segments))
	for _, seg := range segments {
		a, b := seg.StartTime, seg.EndTime
		if a.Before(start) {
			a = start
		}
		if b.After(end) {
			b = end
		}
		mergedIndex := -1
		for i := range merged {
			if merged[i].sessionID == seg.SessionID && seg.StartTime.Before(merged[i].end) && seg.EndTime.After(merged[i].start) {
				mergedIndex = i
				break
			}
		}
		if mergedIndex >= 0 {
			merged[mergedIndex].segments = append(merged[mergedIndex].segments, seg.ID)
			merged[mergedIndex].pinned = merged[mergedIndex].pinned && seg.Pinned
			continue
		}
		if len(available) > 0 && !a.After(available[len(available)-1].End) && available[len(available)-1].SessionID == seg.SessionID {
			if b.After(available[len(available)-1].End) {
				available[len(available)-1].End = b
			}
			available[len(available)-1].SegmentIDs = append(available[len(available)-1].SegmentIDs, seg.ID)
			available[len(available)-1].Pinned = available[len(available)-1].Pinned && seg.Pinned
			continue
		}
		available = append(available, model.TimeRange{Start: a, End: b, Available: true, SessionID: seg.SessionID, SegmentIDs: []string{seg.ID}, Pinned: seg.Pinned})
	}
	for _, v := range merged {
		if len(v.segments) == 0 {
			continue
		}
		a, b := v.start, v.end
		if a.Before(start) {
			a = start
		}
		if b.After(end) {
			b = end
		}
		if b.After(a) {
			available = append(available, model.TimeRange{Start: a, End: b, Available: true, SessionID: v.sessionID, SegmentIDs: v.segments, Pinned: v.pinned, Merged: true})
		}
	}
	unavailable := make([]model.TimeRange, 0, 4)
	for gaps.Next() {
		var a, b int64
		var reason, sid string
		if err = gaps.Scan(&a, &b, &reason, &sid); err != nil {
			return nil, err
		}
		gapStart, gapEnd := time.UnixMilli(a).UTC(), time.UnixMilli(b).UTC()
		if gapStart.Before(start) {
			gapStart = start
		}
		if gapEnd.After(end) {
			gapEnd = end
		}
		if gapEnd.After(gapStart) {
			unavailable = append(unavailable, model.TimeRange{Start: gapStart, End: gapEnd, Reason: reason, SessionID: sid})
		}
	}
	if err = gaps.Err(); err != nil {
		return nil, err
	}
	sort.Slice(available, func(i, j int) bool { return available[i].Start.Before(available[j].Start) })
	sort.Slice(unavailable, func(i, j int) bool { return unavailable[i].Start.Before(unavailable[j].Start) })
	// Return a non-overlapping partition of the requested window. Committed
	// media takes precedence over stale gap markers; uncovered time is still
	// exposed when retention removed media or a sub-frame timestamp gap was not
	// persisted as an explicit reconnect gap.
	ranges := make([]model.TimeRange, 0, len(available)*2+1)
	cursor := start
	appendUnavailable := func(a, b time.Time) {
		if !b.After(a) {
			return
		}
		v := model.TimeRange{Start: a, End: b, Reason: "no_recording"}
		for _, gap := range unavailable {
			if gap.End.After(a) && gap.Start.Before(b) {
				v.Reason = gap.Reason
				v.SessionID = gap.SessionID
				break
			}
		}
		ranges = append(ranges, v)
	}
	for _, media := range available {
		if media.End.Before(cursor) || media.End.Equal(cursor) {
			continue
		}
		if media.Start.After(cursor) {
			appendUnavailable(cursor, media.Start)
		}
		if media.Start.Before(cursor) {
			media.Start = cursor
		}
		if media.End.After(media.Start) {
			ranges = append(ranges, media)
			cursor = media.End
		}
	}
	appendUnavailable(cursor, end)
	return ranges, nil
}

// TimelineBuckets returns bounded, UTC-aligned summaries for long-range views.
// Segment overlap is clipped to each bucket so partial first and last days are accurate.
func (s *Store) TimelineBuckets(ctx context.Context, streamID string, start, end time.Time, width time.Duration) ([]model.TimelineBucket, error) {
	if width != time.Hour && width != 24*time.Hour {
		return nil, errors.New("timeline bucket width must be one hour or one day")
	}
	widthMS := width.Milliseconds()
	startMS, endMS := start.UnixMilli(), end.UnixMilli()
	firstMS := startMS - ((startMS%widthMS)+widthMS)%widthMS
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE buckets(bucket_start) AS (
  SELECT ?
  UNION ALL
  SELECT bucket_start + ? FROM buckets WHERE bucket_start + ? < ?
)
SELECT MAX(bucket_start, ?), MIN(bucket_start + ?, ?),
       COALESCE(SUM(MIN(s.end_ms, bucket_start + ?) - MAX(s.start_ms, bucket_start)), 0),
       COUNT(s.id)
FROM buckets
LEFT JOIN segments s ON s.stream_id=? AND s.state='COMMITTED'
  AND s.start_ms < bucket_start + ? AND s.end_ms > bucket_start
GROUP BY bucket_start ORDER BY bucket_start`, firstMS, widthMS, widthMS, endMS, startMS, widthMS, endMS, widthMS, streamID, widthMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.TimelineBucket, 0, int((endMS-firstMS+widthMS-1)/widthMS))
	for rows.Next() {
		var from, to, recorded, count int64
		if err = rows.Scan(&from, &to, &recorded, &count); err != nil {
			return nil, err
		}
		out = append(out, model.TimelineBucket{Start: time.UnixMilli(from).UTC(), End: time.UnixMilli(to).UTC(), RecordedMS: recorded, SegmentCount: count})
	}
	return out, rows.Err()
}

func (s *Store) SegmentPath(v model.Segment) string {
	return filepath.Join(s.root, filepath.Clean(v.Path))
}

func (s *Store) OpenSegment(v model.Segment) (*os.File, error) {
	rel := filepath.Clean(v.Path)
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, errors.New("invalid media index path")
	}
	r, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Open(rel)
}

func (s *Store) SavePreview(id string, data []byte) error {
	if id == "" || strings.ContainsAny(id, `/\\`) || len(data) == 0 {
		return errors.New("invalid preview image")
	}
	dir := filepath.Join(s.root, "previews")
	if err := ensureSafeDir(s.root, dir); err != nil {
		return err
	}
	path := filepath.Join(dir, id+".jpg")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Store) OpenPreview(id string) (*os.File, error) {
	if id == "" || strings.ContainsAny(id, `/\\`) {
		return nil, errors.New("invalid preview id")
	}
	r, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Open(filepath.Join("previews", id+".jpg"))
}

func (s *Store) HasPreview(id string) bool {
	f, err := s.OpenPreview(id)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func (s *Store) SetSegmentPinned(ctx context.Context, id string, pinned bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE segments SET pinned=? WHERE id=? AND state='COMMITTED'`, boolInt(pinned), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) SetClipPinned(ctx context.Context, id string, pinned bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE clips SET pinned=? WHERE job_id=?`, boolInt(pinned), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) OpenInit(ctx context.Context, epochID string) (*os.File, error) {
	var rel string
	if err := s.db.QueryRowContext(ctx, `SELECT init_path FROM epochs WHERE id=?`, epochID).Scan(&rel); err != nil {
		return nil, err
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(filepath.Clean(rel), ".."+string(os.PathSeparator)) {
		return nil, errors.New("invalid init segment path")
	}
	r, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Open(filepath.Clean(rel))
}

func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite integrity check: %s", result)
	}
	return nil
}

// Recover reconciles published media files and the committed index. Unindexed files are quarantined until a media parser can prove their metadata.
func (s *Store) Recover(ctx context.Context) error {
	return s.RecoverWithOptions(ctx, true, true)
}

func (s *Store) RecoverWithOptions(ctx context.Context, integrityCheck, quarantineInvalid bool) error {
	if integrityCheck {
		if err := s.IntegrityCheck(ctx); err != nil {
			return err
		}
	}
	if err := s.finalizeRetiring(ctx); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(s.root, "tmp", "exports"))
	type committedFile struct {
		path, checksum string
		size           int64
	}
	var rows []committedFile
	r, err := s.db.QueryContext(ctx, `SELECT path,checksum,size_bytes FROM segments WHERE state='COMMITTED'`)
	if err != nil {
		return err
	}
	for r.Next() {
		var item committedFile
		if err = r.Scan(&item.path, &item.checksum, &item.size); err != nil {
			r.Close()
			return err
		}
		rows = append(rows, item)
	}
	if err = r.Close(); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, item := range rows {
		rel := filepath.Clean(item.path)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("invalid committed media path %q", item.path)
		}
		root, openErr := os.OpenRoot(s.root)
		if openErr != nil {
			return openErr
		}
		file, openErr := root.Open(rel)
		if openErr != nil {
			_ = root.Close()
			if !quarantineInvalid {
				return fmt.Errorf("committed segment %s is missing: %w", rel, openErr)
			}
			if _, e := s.db.ExecContext(ctx, `UPDATE segments SET state='QUARANTINED' WHERE path=? AND state='COMMITTED'`, item.path); e != nil {
				return e
			}
			continue
		}
		info, statErr := file.Stat()
		var sum [32]byte
		if statErr == nil {
			h := sha256.New()
			_, statErr = io.Copy(h, file)
			copy(sum[:], h.Sum(nil))
		}
		_ = file.Close()
		_ = root.Close()
		actual := hex.EncodeToString(sum[:])
		if statErr != nil || info.Size() != item.size || actual != item.checksum {
			if !quarantineInvalid {
				return fmt.Errorf("committed segment %s failed size or checksum validation", rel)
			}
			if _, e := s.db.ExecContext(ctx, `UPDATE segments SET state='QUARANTINED' WHERE path=? AND state='COMMITTED'`, item.path); e != nil {
				return e
			}
			lost := filepath.Join(s.root, "lost+found")
			if e := os.MkdirAll(lost, 0o750); e != nil {
				return e
			}
			dst := filepath.Join(lost, filepath.Base(filepath.Dir(filepath.Dir(rel)))+"-"+filepath.Base(rel)+"-"+uuid.NewString())
			if e := os.Rename(filepath.Join(s.root, rel), dst); e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			continue
		}
		known[rel] = true
		s.recoveryTotal.Add(1)
	}
	base := filepath.Join(s.root, "streams")
	_ = os.MkdirAll(base, 0o750)
	lost := filepath.Join(s.root, "lost+found")
	_ = os.MkdirAll(lost, 0o750)
	return filepath.WalkDir(base, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(s.root, path)
		if e != nil {
			return e
		}
		if filepath.Ext(path) == ".tmp" {
			return os.Remove(path)
		}
		if filepath.Ext(path) == ".m4s" && !known[filepath.Clean(rel)] {
			if !quarantineInvalid {
				return nil
			}
			dst := filepath.Join(lost, filepath.Base(filepath.Dir(path))+"-"+filepath.Base(path))
			return os.Rename(path, dst)
		}
		return nil
	})
}

func (s *Store) finalizeRetiring(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,path FROM segments WHERE state='RETIRING'`)
	if err != nil {
		return err
	}
	type item struct{ id, path string }
	var items []item
	for rows.Next() {
		var x item
		if err = rows.Scan(&x.id, &x.path); err != nil {
			rows.Close()
			return err
		}
		items = append(items, x)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, x := range items {
		rel := filepath.Clean(x.path)
		if filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return errors.New("invalid retiring segment path")
		}
		e := root.Remove(rel)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE segments SET state='DELETED' WHERE id=? AND state='RETIRING'`, x.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EndIncompleteSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET status='interrupted',ended_at_ms=? WHERE status='recording'`, time.Now().UTC().UnixMilli())
	return err
}

func (s *Store) CreateClipJob(ctx context.Context, req model.ClipRequest) (model.ClipJob, error) {
	job := model.ClipJob{ID: uuid.NewString(), Request: req, Status: "QUEUED", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	b, err := json.Marshal(req)
	if err != nil {
		return job, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO clip_jobs(id,stream_id,request_json,status,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?)`, job.ID, req.StreamID, string(b), job.Status, job.CreatedAt.UnixMilli(), job.UpdatedAt.UnixMilli())
	return job, err
}

func (s *Store) ClipJobs(ctx context.Context) ([]model.ClipJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,request_json,status,progress,error,created_at_ms,updated_at_ms,COALESCE((SELECT pinned FROM clips WHERE job_id=clip_jobs.id),0) FROM clip_jobs ORDER BY created_at_ms DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ClipJob{}
	for rows.Next() {
		v, e := scanClipJob(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) ClipJob(ctx context.Context, id string) (model.ClipJob, error) {
	return scanClipJob(s.db.QueryRowContext(ctx, `SELECT id,request_json,status,progress,error,created_at_ms,updated_at_ms,COALESCE((SELECT pinned FROM clips WHERE job_id=clip_jobs.id),0) FROM clip_jobs WHERE id=?`, id))
}
func scanClipJob(row interface{ Scan(...any) error }) (model.ClipJob, error) {
	var v model.ClipJob
	var raw string
	var created, updated int64
	var pinned int
	err := row.Scan(&v.ID, &raw, &v.Status, &v.Progress, &v.Error, &created, &updated, &pinned)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(raw), &v.Request)
	v.CreatedAt = time.UnixMilli(created).UTC()
	v.UpdatedAt = time.UnixMilli(updated).UTC()
	v.Pinned = pinned != 0
	return v, err
}
func (s *Store) UpdateClipJob(ctx context.Context, id, status, message string, progress float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM clip_jobs WHERE id=?`, id).Scan(&previous); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE clip_jobs SET status=?,error=?,progress=?,updated_at_ms=? WHERE id=?`, status, message, progress, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	if status == "FAILED" && previous != "FAILED" {
		if err = incrementSettingTx(ctx, tx, "metrics.clip_jobs_failed_total", 1); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func incrementSettingTx(ctx context.Context, tx *sql.Tx, key string, delta int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO system_settings(key,value,updated_at_ms) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+CAST(excluded.value AS INTEGER),updated_at_ms=excluded.updated_at_ms`, key, fmt.Sprint(delta), time.Now().UTC().UnixMilli())
	return err
}
func (s *Store) CompleteClip(ctx context.Context, id, rel string, size int64, saved bool, expires time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO clips(id,job_id,path,size_bytes,saved_to_library,pinned,expires_at_ms) VALUES(?,?,?,?,?,?,?)`, uuid.NewString(), id, rel, size, boolInt(saved), boolInt(saved), nullableMS(expires)); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE clip_jobs SET status='COMPLETED',progress=1,error='',updated_at_ms=? WHERE id=? AND status NOT IN ('CANCELED','EXPIRED')`, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("clip job was canceled before publication")
	}
	return tx.Commit()
}

func (s *Store) ReplaceFavoriteClip(ctx context.Context, sourceID, taskID, rel string, size int64, settings model.ClipRequest) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var oldPath string
	var pinned int
	if err = tx.QueryRowContext(ctx, `SELECT path,pinned FROM clips WHERE job_id=?`, sourceID).Scan(&oldPath, &pinned); err != nil {
		return "", err
	}
	if pinned == 0 {
		return "", errors.New("clip is no longer in favorites")
	}
	var sourceRaw string
	if err = tx.QueryRowContext(ctx, `SELECT request_json FROM clip_jobs WHERE id=? AND status='COMPLETED'`, sourceID).Scan(&sourceRaw); err != nil {
		return "", err
	}
	var sourceRequest model.ClipRequest
	if err = json.Unmarshal([]byte(sourceRaw), &sourceRequest); err != nil {
		return "", err
	}
	sourceRequest.Mode = settings.Mode
	sourceRequest.QualityPreset = settings.QualityPreset
	sourceRequest.Resolution = settings.Resolution
	sourceRequest.SourceClipID = ""
	updatedRequest, err := json.Marshal(sourceRequest)
	if err != nil {
		return "", err
	}
	res, err := tx.ExecContext(ctx, `UPDATE clips SET path=?,size_bytes=?,expires_at_ms=NULL WHERE job_id=? AND pinned=1`, rel, size, sourceID)
	if err != nil {
		return "", err
	}
	if count, _ := res.RowsAffected(); count != 1 {
		return "", errors.New("favorite clip changed before replacement")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE clip_jobs SET request_json=?,updated_at_ms=? WHERE id=?`, string(updatedRequest), time.Now().UTC().UnixMilli(), sourceID); err != nil {
		return "", err
	}
	res, err = tx.ExecContext(ctx, `UPDATE clip_jobs SET status='COMPLETED',progress=1,error='',updated_at_ms=? WHERE id=? AND status NOT IN ('CANCELED','EXPIRED')`, time.Now().UTC().UnixMilli(), taskID)
	if err != nil {
		return "", err
	}
	if count, _ := res.RowsAffected(); count != 1 {
		return "", errors.New("re-encode task was canceled before replacement")
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return oldPath, nil
}

func nullableMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}
func (s *Store) ClipPath(ctx context.Context, id string) (string, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT path FROM clips WHERE job_id=?`, id).Scan(&p)
	return p, err
}

type ClipRetentionCandidate struct {
	JobID     string
	SizeBytes int64
}

func (s *Store) ClipRetentionCandidates(ctx context.Context, savedToLibrary bool) ([]ClipRetentionCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.job_id,c.size_bytes FROM clips c JOIN clip_jobs j ON j.id=c.job_id WHERE c.saved_to_library=? AND c.pinned=0 ORDER BY j.created_at_ms ASC`, boolInt(savedToLibrary))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClipRetentionCandidate
	for rows.Next() {
		var item ClipRetentionCandidate
		if err = rows.Scan(&item.JobID, &item.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) DeleteClip(ctx context.Context, id string) error {
	_, err := s.DeleteClipWithFreedBytes(ctx, id)
	return err
}

func (s *Store) PurgeClipJob(ctx context.Context, id string) error {
	if _, err := s.DeleteClipWithFreedBytes(ctx, id); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM segment_leases WHERE owner_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM clip_jobs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) DeleteClipWithFreedBytes(ctx context.Context, id string) (int64, error) {
	var status string
	_ = s.db.QueryRowContext(ctx, `SELECT status FROM clip_jobs WHERE id=?`, id).Scan(&status)
	var rel string
	err := s.db.QueryRowContext(ctx, `SELECT path FROM clips WHERE job_id=?`, id).Scan(&rel)
	var freedBytes int64
	if err == nil {
		rel = filepath.Clean(rel)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return 0, errors.New("invalid clip path")
		}
		root, e := os.OpenRoot(s.root)
		if e != nil {
			return 0, e
		}
		if file, openErr := root.Open(rel); openErr == nil {
			if info, statErr := file.Stat(); statErr == nil && info.Mode().IsRegular() {
				freedBytes = info.Size()
			}
			_ = file.Close()
		} else if !errors.Is(openErr, os.ErrNotExist) {
			_ = root.Close()
			return 0, openErr
		}
		e = root.Remove(rel)
		closeErr := root.Close()
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return freedBytes, errors.Join(e, closeErr)
		}
		if _, e = s.db.ExecContext(ctx, `DELETE FROM clips WHERE job_id=?`, id); e != nil {
			return freedBytes, errors.Join(e, closeErr)
		}
		if closeErr != nil {
			return freedBytes, closeErr
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	newStatus := "CANCELED"
	if status == "COMPLETED" {
		newStatus = "EXPIRED"
	}
	_, err = s.db.ExecContext(ctx, `UPDATE clip_jobs SET status=?,updated_at_ms=? WHERE id=?`, newStatus, time.Now().UTC().UnixMilli(), id)
	return freedBytes, err
}

func (s *Store) ExpiredClipJobs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id FROM clips WHERE pinned=0 AND expires_at_ms IS NOT NULL AND expires_at_ms<=?`, time.Now().UTC().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (s *Store) OpenClip(ctx context.Context, id string) (*os.File, error) {
	rel, err := s.ClipPath(ctx, id)
	if err != nil {
		return nil, err
	}
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, errors.New("invalid clip path")
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(rel)
}
func (s *Store) FailInterruptedClipJobs(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM clip_jobs WHERE status IN ('RESOLVING','RESERVED','RUNNING','VERIFYING')`).Scan(&count); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE clip_jobs SET status='FAILED',error='server restarted before task completion',updated_at_ms=? WHERE status IN ('RESOLVING','RESERVED','RUNNING','VERIFYING')`, time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	if count > 0 {
		if err = incrementSettingTx(ctx, tx, "metrics.clip_jobs_failed_total", count); err != nil {
			return err
		}
	}
	return tx.Commit()
}
