package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAuthSessionRole(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, role := range []string{"admin", "viewer"} {
		token := "session-" + role
		if err := store.SaveAuthSession(ctx, token, role, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("save %s session: %v", role, err)
		}
		got, err := store.AuthSessionRole(ctx, token)
		if err != nil || got != role {
			t.Fatalf("role for %s session = %q, %v; want %q", role, got, err, role)
		}
	}

	if err := store.SaveAuthSession(ctx, "invalid", "owner", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("SaveAuthSession accepted an unsupported role")
	}
	if err := store.SaveAuthSession(ctx, "expired", "viewer", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := store.AuthSessionRole(ctx, "expired")
	if err != nil || got != "" {
		t.Fatalf("role for expired session = %q, %v; want empty role", got, err)
	}
}
