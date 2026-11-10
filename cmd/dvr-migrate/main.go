// Command dvr-migrate copies a legacy Backtrack SQLite index into PostgreSQL.
//
// The media files themselves are not touched: segment, clip, and init paths are
// stored relative to storage.root and remain valid after the switch. Stop the
// Backtrack server before running the migration so the SQLite file is stable.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"backtrack/internal/storage"
	"github.com/jackc/pgx/v5"
	_ "modernc.org/sqlite"
)

type columnKind int

const (
	kindText columnKind = iota
	kindInt
	kindFloat
	kindBool
)

type column struct {
	name string
	kind columnKind
}

type table struct {
	name    string
	columns []column
}

var tables = []table{
	{name: "streams", columns: []column{
		{name: "id", kind: kindText},
		{name: "name", kind: kindText},
		{name: "enabled", kind: kindBool},
		{name: "publish_key_hash", kind: kindText},
		{name: "publish_key_encrypted", kind: kindText},
		{name: "created_at_ms", kind: kindInt},
	}},
	{name: "sessions", columns: []column{
		{name: "id", kind: kindText},
		{name: "stream_id", kind: kindText},
		{name: "started_at_ms", kind: kindInt},
		{name: "ended_at_ms", kind: kindInt},
		{name: "status", kind: kindText},
	}},
	{name: "epochs", columns: []column{
		{name: "id", kind: kindText},
		{name: "session_id", kind: kindText},
		{name: "started_at_ms", kind: kindInt},
		{name: "ended_at_ms", kind: kindInt},
		{name: "init_path", kind: kindText},
		{name: "codec_config_hash", kind: kindText},
		{name: "video_timescale", kind: kindInt},
		{name: "audio_timescale", kind: kindInt},
	}},
	{name: "segments", columns: []column{
		{name: "id", kind: kindText},
		{name: "stream_id", kind: kindText},
		{name: "session_id", kind: kindText},
		{name: "epoch_id", kind: kindText},
		{name: "seq", kind: kindInt},
		{name: "start_ms", kind: kindInt},
		{name: "end_ms", kind: kindInt},
		{name: "first_pts", kind: kindInt},
		{name: "last_pts", kind: kindInt},
		{name: "first_dts", kind: kindInt},
		{name: "last_dts", kind: kindInt},
		{name: "timescale", kind: kindInt},
		{name: "size_bytes", kind: kindInt},
		{name: "path", kind: kindText},
		{name: "random_access", kind: kindBool},
		{name: "checksum", kind: kindText},
		{name: "state", kind: kindText},
		{name: "pinned", kind: kindBool},
		{name: "created_at_ms", kind: kindInt},
	}},
	{name: "recording_gaps", columns: []column{
		{name: "id", kind: kindText},
		{name: "stream_id", kind: kindText},
		{name: "session_id", kind: kindText},
		{name: "start_ms", kind: kindInt},
		{name: "end_ms", kind: kindInt},
		{name: "reason", kind: kindText},
	}},
	{name: "merged_history_ranges", columns: []column{
		{name: "id", kind: kindText},
		{name: "stream_id", kind: kindText},
		{name: "session_id", kind: kindText},
		{name: "start_ms", kind: kindInt},
		{name: "end_ms", kind: kindInt},
		{name: "created_at_ms", kind: kindInt},
	}},
	{name: "clip_jobs", columns: []column{
		{name: "id", kind: kindText},
		{name: "stream_id", kind: kindText},
		{name: "request_json", kind: kindText},
		{name: "status", kind: kindText},
		{name: "progress", kind: kindFloat},
		{name: "error", kind: kindText},
		{name: "created_at_ms", kind: kindInt},
		{name: "updated_at_ms", kind: kindInt},
	}},
	{name: "clips", columns: []column{
		{name: "id", kind: kindText},
		{name: "job_id", kind: kindText},
		{name: "path", kind: kindText},
		{name: "size_bytes", kind: kindInt},
		{name: "saved_to_library", kind: kindBool},
		{name: "pinned", kind: kindBool},
		{name: "expires_at_ms", kind: kindInt},
	}},
	{name: "retention_policies", columns: []column{
		{name: "stream_id", kind: kindText},
		{name: "max_age_ms", kind: kindInt},
		{name: "max_size_bytes", kind: kindInt},
		{name: "updated_at_ms", kind: kindInt},
	}},
	{name: "segment_leases", columns: []column{
		{name: "id", kind: kindText},
		{name: "segment_id", kind: kindText},
		{name: "owner_id", kind: kindText},
		{name: "expires_at_ms", kind: kindInt},
	}},
	{name: "storage_volumes", columns: []column{
		{name: "id", kind: kindText},
		{name: "root_path", kind: kindText},
		{name: "enabled", kind: kindBool},
		{name: "updated_at_ms", kind: kindInt},
	}},
	{name: "auth_sessions", columns: []column{
		{name: "id", kind: kindText},
		{name: "token_hash", kind: kindText},
		{name: "expires_at_ms", kind: kindInt},
		{name: "role", kind: kindText},
	}},
	{name: "system_settings", columns: []column{
		{name: "key", kind: kindText},
		{name: "value", kind: kindText},
		{name: "updated_at_ms", kind: kindInt},
	}},
	{name: "session_counter", columns: []column{
		{name: "singleton", kind: kindInt},
		{name: "value", kind: kindInt},
	}},
}

// Tables that are intentionally not copied: schema_migrations is created by the
// target schema and already reflects the current schema version.
var truncateTables = []string{
	"segment_leases", "segments", "clips", "clip_jobs", "merged_history_ranges",
	"recording_gaps", "epochs", "sessions", "retention_policies", "streams",
	"storage_volumes", "auth_sessions", "system_settings", "session_counter",
}

func main() {
	sqlitePath := flag.String("sqlite", "", "path to the legacy SQLite database (for example ./data/db/dvr.sqlite)")
	pgDSN := flag.String("pg", "", "target PostgreSQL connection string")
	force := flag.Bool("force", false, "delete existing rows in the target database before copying")
	flag.Parse()
	if *sqlitePath == "" || *pgDSN == "" {
		fmt.Fprintln(os.Stderr, "usage: dvr-migrate -sqlite <path> -pg <postgres-dsn> [-force]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := run(ctx, *sqlitePath, *pgDSN, *force); err != nil {
		fmt.Fprintf(os.Stderr, "migration failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, sqlitePath, pgDSN string, force bool) error {
	if _, err := os.Stat(sqlitePath); err != nil {
		return fmt.Errorf("open sqlite database: %w", err)
	}
	src, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return fmt.Errorf("open sqlite database: %w", err)
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	if err = src.PingContext(ctx); err != nil {
		return fmt.Errorf("open sqlite database: %w", err)
	}
	if _, err = src.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		return fmt.Errorf("set sqlite query_only: %w", err)
	}
	if err = verifySourceTables(ctx, src); err != nil {
		return err
	}

	// Open the store once to create the PostgreSQL schema, then use a native
	// pgx connection for COPY FROM.
	schemaStore, err := storage.Open(os.TempDir(), pgDSN)
	if err != nil {
		return fmt.Errorf("prepare postgres schema: %w", err)
	}
	if err = schemaStore.Close(); err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, pgDSN)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		return err
	}
	empty, err := targetIsEmpty(ctx, tx)
	if err != nil {
		return err
	}
	if !empty && !force {
		return errors.New("target database already contains data; pass -force to replace it")
	}
	if force && !empty {
		if _, err = tx.Exec(ctx, `TRUNCATE TABLE `+strings.Join(quotedNames(truncateTables), ", ")+` CASCADE`); err != nil {
			return fmt.Errorf("truncate target tables: %w", err)
		}
	}
	// The schema bootstrap inserts the global session counter singleton. Remove
	// the placeholder so the source row can be copied verbatim.
	if _, err = tx.Exec(ctx, `DELETE FROM session_counter`); err != nil {
		return err
	}

	counts := make(map[string]int64, len(tables))
	for _, spec := range tables {
		n, copyErr := copyTable(ctx, tx, src, spec)
		if copyErr != nil {
			return copyErr
		}
		counts[spec.name] = n
	}
	for _, spec := range tables {
		var got int64
		if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM `+quoteIdent(spec.name)).Scan(&got); err != nil {
			return fmt.Errorf("verify %s: %w", spec.name, err)
		}
		if got != counts[spec.name] {
			return fmt.Errorf("verify %s: copied %d rows, found %d in target", spec.name, counts[spec.name], got)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	for _, spec := range tables {
		fmt.Printf("%-24s %d\n", spec.name, counts[spec.name])
	}
	fmt.Println("migration complete; media files were not modified")
	return nil
}

func verifySourceTables(ctx context.Context, src *sql.DB) error {
	rows, err := src.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return err
		}
		found[name] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, spec := range tables {
		if !found[spec.name] {
			return fmt.Errorf("sqlite database is missing table %q; run the current Backtrack version once to apply schema migrations, then retry", spec.name)
		}
	}
	return nil
}

func targetIsEmpty(ctx context.Context, tx pgx.Tx) (bool, error) {
	var empty bool
	err := tx.QueryRow(ctx, `SELECT NOT (EXISTS(SELECT 1 FROM streams) OR EXISTS(SELECT 1 FROM sessions) OR EXISTS(SELECT 1 FROM segments) OR EXISTS(SELECT 1 FROM clip_jobs) OR EXISTS(SELECT 1 FROM system_settings))`).Scan(&empty)
	return empty, err
}

func copyTable(ctx context.Context, tx pgx.Tx, src *sql.DB, spec table) (int64, error) {
	available, err := sourceColumns(ctx, src, spec.name)
	if err != nil {
		return 0, err
	}
	selectColumns := make([]string, len(spec.columns))
	insertColumns := make([]string, len(spec.columns))
	for i, col := range spec.columns {
		if !available[col.name] {
			return 0, fmt.Errorf("sqlite table %s is missing column %q; run the current Backtrack version once to apply schema migrations, then retry", spec.name, col.name)
		}
		selectColumns[i] = quoteIdent(col.name)
		insertColumns[i] = col.name
	}
	rows, err := src.QueryContext(ctx, `SELECT `+strings.Join(selectColumns, ",")+` FROM `+quoteIdent(spec.name))
	if err != nil {
		return 0, fmt.Errorf("read sqlite %s: %w", spec.name, err)
	}
	defer rows.Close()
	source := &rowsCopySource{rows: rows, table: spec.name, columns: spec.columns}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{spec.name}, insertColumns, source); err != nil {
		return 0, fmt.Errorf("copy %s: %w", spec.name, err)
	}
	return source.count, nil
}

func sourceColumns(ctx context.Context, src *sql.DB, tableName string) (map[string]bool, error) {
	rows, err := src.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(tableName)+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspect sqlite %s: %w", tableName, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err = rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("inspect sqlite %s: %w", tableName, err)
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

type rowsCopySource struct {
	rows    *sql.Rows
	table   string
	columns []column
	count   int64
	err     error
}

func (s *rowsCopySource) Next() bool {
	if s.err != nil || !s.rows.Next() {
		return false
	}
	s.count++
	return true
}

func (s *rowsCopySource) Values() ([]any, error) {
	raw := make([]any, len(s.columns))
	targets := make([]any, len(s.columns))
	for i := range raw {
		targets[i] = &raw[i]
	}
	if err := s.rows.Scan(targets...); err != nil {
		s.err = err
		return nil, err
	}
	values := make([]any, len(s.columns))
	for i, col := range s.columns {
		v, err := convertValue(raw[i], col.kind)
		if err != nil {
			s.err = err
			return nil, fmt.Errorf("%s.%s: %w", s.table, col.name, err)
		}
		values[i] = v
	}
	return values, nil
}

func (s *rowsCopySource) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.rows.Err()
}

func convertValue(v any, kind columnKind) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch kind {
	case kindBool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			return x != 0, nil
		case float64:
			return x != 0, nil
		case string:
			return x == "1" || strings.EqualFold(x, "true"), nil
		}
	case kindInt:
		switch x := v.(type) {
		case int64:
			return x, nil
		case int:
			return int64(x), nil
		case float64:
			return int64(x), nil
		case []byte:
			return strconv.ParseInt(string(x), 10, 64)
		case string:
			return strconv.ParseInt(x, 10, 64)
		}
	case kindFloat:
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case []byte:
			return strconv.ParseFloat(string(x), 64)
		case string:
			return strconv.ParseFloat(x, 64)
		}
	default:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		}
	}
	return nil, fmt.Errorf("unsupported %T value", v)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quotedNames(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = quoteIdent(name)
	}
	return out
}
