package s3

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestLoadStoreRejectsNewerSchema mirrors the local backend's contract: a
// snapshot stamped with a schema version newer than this binary supports
// must be reported as an error rather than partially trusted.
func TestLoadStoreRejectsNewerSchema(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion+1, nil)

	_, err := b.LoadStore(ctx)
	if !errors.Is(err, helpers.ErrUnsupportedSchemaVersion) {
		t.Fatalf("expected ErrUnsupportedSchemaVersion, got %v", err)
	}
}

// TestLoadStoreDropsOlderSchema mirrors the local backend's contract: a
// snapshot stamped with a schema version older than current must be
// dropped and rebuilt, returning a fresh empty Store with a nil error.
func TestLoadStoreDropsOlderSchema(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion-1, func(st *store.Store) {
		st.SetAPICache("api", store.APICacheEntry{URL: "https://example.com/api"})
	})

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("expected nil error for outdated schema, got %v", err)
	}
	fresh := store.New()
	if loaded.Meta.SchemaVersion != fresh.Meta.SchemaVersion {
		t.Fatalf("expected fresh schema version %d, got %d", fresh.Meta.SchemaVersion, loaded.Meta.SchemaVersion)
	}
	if len(loaded.APICache) != 0 {
		t.Fatalf("expected an empty store after dropping an outdated schema, got %#v", loaded)
	}
}

// TestLoadStoreLoadsCurrentSchema confirms a snapshot stamped with the
// current schema version round-trips its data unchanged through LoadStore.
func TestLoadStoreLoadsCurrentSchema(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion, func(st *store.Store) {
		st.SetAPICache("api", store.APICacheEntry{URL: "https://example.com/api"})
		st.SetInstalled("a.b@1.0.0", store.InstalledEntry{ArtifactSHA256: "abc"})
	})

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	entry, ok := loaded.GetAPICache("api")
	if !ok || entry.URL != "https://example.com/api" {
		t.Fatalf("unexpected api cache entry: %#v (ok=%v)", entry, ok)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != "abc" {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}
}

// putStoreObject seeds the state/store.json.gz object with a store stamped
// at schemaVersion, mirroring exactly what SaveStore produces (marshal the
// store JSON, gzip it) so LoadStore's decoding path is exercised the same
// way it would be against a real snapshot.
func putStoreObject(ctx context.Context, t *testing.T, b *Backend, schemaVersion int, mutate func(*store.Store)) {
	t.Helper()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	st := store.New()
	st.Meta.SchemaVersion = schemaVersion
	if mutate != nil {
		mutate(st)
	}

	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	key := b.key(statePrefix, storeObject)
	reader := bytes.NewReader(buf.Bytes())
	if err := b.client.putObject(ctx, key, reader, int64(buf.Len()), "application/json", "gzip", nil, false, ""); err != nil {
		t.Fatalf("putObject: %v", err)
	}
}
