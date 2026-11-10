// Package testdb provides isolated PostgreSQL schemas for integration tests.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var schemaCounter atomic.Uint64

// DSN returns a PostgreSQL connection string whose search_path points at a
// fresh schema. The schema and its data are removed when the test completes.
//
// Tests are skipped when DVR_TEST_DATABASE is not set. Point it at a database
// where the test user may create and drop schemas, for example:
//
//	DVR_TEST_DATABASE='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable'
func DSN(t *testing.T) string {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("DVR_TEST_DATABASE"))
	if base == "" {
		t.Skip("DVR_TEST_DATABASE is not set; skipping PostgreSQL integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open DVR_TEST_DATABASE: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("connect to DVR_TEST_DATABASE: %v", err)
	}
	schema := fmt.Sprintf("dvr_test_%d_%d", os.Getpid(), schemaCounter.Add(1))
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+quoteIdentifier(schema)); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA `+quoteIdentifier(schema)+` CASCADE`); err != nil {
			t.Logf("drop test schema %s: %v", schema, err)
		}
		_ = admin.Close()
	})
	dsn, err := withSearchPath(base, schema)
	if err != nil {
		t.Fatalf("build test DSN: %v", err)
	}
	return dsn
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func withSearchPath(base, schema string) (string, error) {
	if u, err := url.Parse(base); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return strings.TrimSpace(base) + " search_path=" + schema, nil
}
