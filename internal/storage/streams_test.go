package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSetStreamEnabledControlsPublishAuthentication(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "data"), filepath.Join(root, "data", "db", "dvr.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stream, err := store.CreateStream(ctx, "publisher", "publish-secret")
	if err != nil {
		t.Fatal(err)
	}
	if ok, authErr := store.AuthenticatePublish(ctx, stream.ID, "publish-secret"); authErr != nil || !ok {
		t.Fatalf("enabled stream authentication = %v, %v; want true, nil", ok, authErr)
	}
	if err = store.SetStreamEnabled(ctx, stream.ID, false); err != nil {
		t.Fatal(err)
	}
	if ok, authErr := store.AuthenticatePublish(ctx, stream.ID, "publish-secret"); authErr != nil || ok {
		t.Fatalf("disabled stream authentication = %v, %v; want false, nil", ok, authErr)
	}
	if err = store.SetStreamEnabled(ctx, stream.ID, true); err != nil {
		t.Fatal(err)
	}
	if ok, authErr := store.AuthenticatePublish(ctx, stream.ID, "publish-secret"); authErr != nil || !ok {
		t.Fatalf("re-enabled stream authentication = %v, %v; want true, nil", ok, authErr)
	}
}
