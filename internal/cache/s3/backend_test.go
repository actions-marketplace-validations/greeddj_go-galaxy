package s3

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

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

// TestLoadStoreDropsV3ShapeAndRebuilds proves LoadStore's schema check
// happens before the full payload is unmarshaled into a *store.Store: a
// genuine v3-shape object (versions_cache as bare arrays, deps_cache as bare
// maps, neither wrapped with a fetched_at stamp) is dropped and rebuilt with
// a nil error, rather than failing json.Unmarshal against the current
// wrapper structs. putStoreObject cannot exercise this: it always marshals
// the current Store type (only the schema_version number overridden), so
// its versions_cache/deps_cache are always already in the current shape.
func TestLoadStoreDropsV3ShapeAndRebuilds(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := []byte(`{
		"meta": {"schema_version": 3, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": {},
		"deps_cache": {"a.b": {"c.d": ">=1.0.0"}},
		"installed": {},
		"graph": {},
		"requirements": {},
		"roots": {},
		"resolved": {},
		"versions_cache": {"a.b": ["1.0.0", "2.0.0"]}
	}`)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("expected nil error dropping a v3-shape snapshot, got %v", err)
	}
	fresh := store.New()
	if loaded.Meta.SchemaVersion != fresh.Meta.SchemaVersion {
		t.Fatalf("expected fresh schema version %d, got %d", fresh.Meta.SchemaVersion, loaded.Meta.SchemaVersion)
	}
	if len(loaded.DepsCache) != 0 || len(loaded.Versions) != 0 {
		t.Fatalf("expected an empty store after dropping a v3-shape snapshot, got %#v", loaded)
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

// TestSaveStoreConcurrentMutationIsRaceFree proves SaveStore never touches
// the live store's maps directly: a goroutine hammers SetInstalled and
// SetAPICache in a tight loop while SaveStore runs repeatedly on the same
// *store.Store from the test goroutine. Before MarshalSnapshot existed,
// SaveStore's json.Marshal(st) read the live maps without a lock and this
// tripped -race; MarshalSnapshot's RLock deep-copy must be clean.
func TestSaveStoreConcurrentMutationIsRaceFree(t *testing.T) {
	b := newTestBackend(t)
	ctx := t.Context()
	st := store.New()

	// Reuse a small, fixed set of keys so the store's maps stay bounded:
	// an ever-growing key set would make each SaveStore's deep copy
	// progressively more expensive, snowballing into a runaway loop.
	const mutatorKeys = 8

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			key := fmt.Sprintf("mutator.key%d", i%mutatorKeys)
			st.SetInstalled(key, store.InstalledEntry{ArtifactSHA256: "sha"})
			st.SetAPICache("api", store.APICacheEntry{URL: fmt.Sprintf("https://example.com/%d", i)})
		}
	})

	const saveCount = 20
	var lastErr error
	for range saveCount {
		lastErr = b.SaveStore(ctx, st)
		if lastErr != nil {
			break
		}
	}
	close(done)
	wg.Wait()

	if lastErr != nil {
		t.Fatalf("expected SaveStore to succeed under concurrent mutation, got %v", lastErr)
	}
}

// TestSaveStoreStampsSchemaAndTimestamp confirms SaveStore stamps the
// current schema version and a non-zero last-snapshot time via
// MarshalSnapshot, exactly as the local backend's Save does.
func TestSaveStoreStampsSchemaAndTimestamp(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	st := store.New()
	st.SetAPICache("api", store.APICacheEntry{URL: "https://example.com/api"})

	if err := b.SaveStore(ctx, st); err != nil {
		t.Fatalf("SaveStore error: %v", err)
	}

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", helpers.StoreSnapshotSchemaVersion, loaded.Meta.SchemaVersion)
	}
	if loaded.Meta.LastSnapshot.IsZero() {
		t.Fatalf("expected a non-zero last_snapshot timestamp")
	}
}

// TestSaveStoreRoundTripsDataShape confirms MarshalSnapshot's JSON shape is
// unchanged from a direct json.Marshal(st): a populated store saved through
// SaveStore and read back through LoadStore preserves its data exactly.
func TestSaveStoreRoundTripsDataShape(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	st := store.New()
	// FetchedAt must be within the retention window, since api_cache is now
	// subject to CacheEntryMaxAge pruning at persist time: a zero-value
	// FetchedAt would make this entry vanish from SaveStore's payload
	// regardless of the shape-preservation behavior under test.
	st.SetAPICache("api", store.APICacheEntry{URL: "https://example.com/api", ETag: "etag", FetchedAt: time.Now().UTC()})
	st.SetInstalled("a.b@1.0.0", store.InstalledEntry{ArtifactSHA256: "abc"})

	if err := b.SaveStore(ctx, st); err != nil {
		t.Fatalf("SaveStore error: %v", err)
	}

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	entry, ok := loaded.GetAPICache("api")
	if !ok || entry.URL != "https://example.com/api" || entry.ETag != "etag" {
		t.Fatalf("unexpected api cache entry after round trip: %#v (ok=%v)", entry, ok)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != "abc" {
		t.Fatalf("unexpected installed entry after round trip: %#v (ok=%v)", installed, ok)
	}
}

// TestLoadProjectRegistryRejectsCorruptObject confirms a project registry
// object that fails to decode is reported as an error rather than silently
// replaced by an empty registry. Cleanup relies on the registry to compute
// which installed collections are still reachable, so an empty registry
// would make it believe nothing is reachable and delete everything.
func TestLoadProjectRegistryRejectsCorruptObject(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putProjectsObject(ctx, t, b, []byte("{invalid"))

	_, err := b.LoadProjectRegistry(ctx)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
}

// TestLoadProjectRegistryMissingObjectReturnsEmpty is a regression guard: a
// bucket that has never recorded a project must still return an empty,
// initialized registry with a nil error.
func TestLoadProjectRegistryMissingObjectReturnsEmpty(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	registry, err := b.LoadProjectRegistry(ctx)
	if err != nil {
		t.Fatalf("expected nil error for a missing registry object, got %v", err)
	}
	if registry == nil || registry.Projects == nil {
		t.Fatalf("expected an initialized empty registry, got %#v", registry)
	}
	if len(registry.Projects) != 0 {
		t.Fatalf("expected no projects, got %#v", registry.Projects)
	}
}

// TestBackendSweepTempNoOp confirms the S3 backend's SweepTemp is a genuine
// no-op: its download temps live under the OS temp directory rather than the
// shared S3 storage, so there is nothing for the backend itself to sweep.
func TestBackendSweepTempNoOp(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	if err := b.SweepTemp(ctx); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// TestReadAllCappedRejectsOversizedRaw confirms the compressed-size ceiling
// applies on the non-gzip path: a plain body longer than compressedCap must
// fail with helpers.ErrArtifactTooLarge before it is fully buffered.
func TestReadAllCappedRejectsOversizedRaw(t *testing.T) {
	t.Parallel()

	const compressedCap = 16
	data := bytes.Repeat([]byte("x"), compressedCap*4)

	_, err := readAllCapped(bytes.NewReader(data), http.Header{}, "state/store.json", compressedCap, helpers.StateObjectMaxDecompressedSize)
	if !errors.Is(err, helpers.ErrArtifactTooLarge) {
		t.Fatalf("readAllCapped() error = %v, want ErrArtifactTooLarge", err)
	}
}

// TestReadAllCappedRejectsGzipBomb confirms the decompressed-size ceiling
// applies on the gzip path independently of the compressed-size ceiling: a
// gzip stream well under compressedCap that inflates past a tiny
// decompressedCap must fail with helpers.ErrArtifactTooLarge.
func TestReadAllCappedRejectsGzipBomb(t *testing.T) {
	t.Parallel()

	const decompressedCap = 64
	// A few KB of zeros compresses to well under any reasonable compressedCap,
	// while comfortably exceeding decompressedCap once inflated, so the
	// decompressed ceiling - not the compressed one - is what trips.
	payload := make([]byte, 8<<10)
	gz := gzipBytes(t, payload)
	if int64(len(gz)) >= helpers.StateObjectMaxCompressedSize {
		t.Fatalf("test setup: gzip payload (%d bytes) is not comfortably under the compressed cap", len(gz))
	}

	header := http.Header{"Content-Encoding": []string{"gzip"}}
	_, err := readAllCapped(bytes.NewReader(gz), header, "state/store.json.gz", helpers.StateObjectMaxCompressedSize, decompressedCap)
	if !errors.Is(err, helpers.ErrArtifactTooLarge) {
		t.Fatalf("readAllCapped() error = %v, want ErrArtifactTooLarge", err)
	}
}

// TestReadAllCappedAcceptsNormal confirms both the gzip and non-gzip paths
// still round-trip their exact bytes through readAllCapped when the input
// stays comfortably under both ceilings.
func TestReadAllCappedAcceptsNormal(t *testing.T) {
	t.Parallel()

	t.Run("gzip", func(t *testing.T) {
		t.Parallel()
		want := []byte("a small cache-state payload")
		gz := gzipBytes(t, want)

		header := http.Header{"Content-Encoding": []string{"gzip"}}
		got, err := readAllCapped(bytes.NewReader(gz), header, "state/store.json.gz", 64, 64)
		if err != nil {
			t.Fatalf("readAllCapped() error = %v, want nil", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("readAllCapped() = %q, want %q", got, want)
		}
	})

	t.Run("raw", func(t *testing.T) {
		t.Parallel()
		want := []byte("a small non-gzip payload")

		got, err := readAllCapped(bytes.NewReader(want), http.Header{}, "state/store.json", 64, 64)
		if err != nil {
			t.Fatalf("readAllCapped() error = %v, want nil", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("readAllCapped() = %q, want %q", got, want)
		}
	})
}

// gzipBytes gzip-encodes data using the standard library's compress/gzip,
// which produces the same on-the-wire format klauspost/pgzip reads, so it
// stands in for a real state object without pulling the production gzip
// writer into the test.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// putProjectsObject seeds the state/projects.json object with raw bytes,
// mirroring saveProjectRegistry's plain (non-gzipped) JSON encoding so
// LoadProjectRegistry's decoding path is exercised the same way it would be
// against a real registry object.
func putProjectsObject(ctx context.Context, t *testing.T, b *Backend, data []byte) {
	t.Helper()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(statePrefix, projectsObject)
	reader := bytes.NewReader(data)
	if err := b.client.putObject(ctx, key, reader, int64(len(data)), "application/json", "", nil, false, ""); err != nil {
		t.Fatalf("putObject: %v", err)
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

// putRawStoreObject seeds the state/store.json.gz object with raw JSON
// bytes, gzipped exactly as SaveStore would, without going through
// store.New()/json.Marshal(*store.Store) - letting a test inject a wire
// shape (such as a genuine pre-schema-4 snapshot) that the current Store
// type could never produce by construction.
func putRawStoreObject(ctx context.Context, t *testing.T, b *Backend, rawJSON []byte) {
	t.Helper()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(rawJSON); err != nil {
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
