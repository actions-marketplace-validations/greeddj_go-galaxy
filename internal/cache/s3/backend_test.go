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

// testArtifactSHA is the shared artifact sha value seeded across several
// installed-entry test fixtures.
const testArtifactSHA = "abc"

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

// TestLoadStoreCurrentSchemaCorruptDataErrors locks in the boundary between
// LoadStore's two decode stages. The lightweight meta-only probe only ever
// authorizes the drop-and-rebuild path for an OUTDATED schema (see
// TestLoadStoreDropsV3ShapeAndRebuilds, which plants this exact corrupt
// versions_cache shape under schema version 3). Here the same corrupt shape
// is stamped with the CURRENT schema version instead: the probe reports a
// match, so the full json.Unmarshal into *store.Store runs, and it must fail
// on the malformed bucket. That failure has to surface as an error rather
// than being swallowed into a silent, empty store.New() - a future edit that
// widened the drop-and-rebuild path to cover decode failures at any schema
// version would make genuine current-schema corruption indistinguishable
// from a legitimately empty cache, silently masking data loss.
func TestLoadStoreCurrentSchemaCorruptDataErrors(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := fmt.Appendf(nil, `{
		"meta": {"schema_version": %d, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": {},
		"deps_cache": {},
		"installed": {},
		"graph": {},
		"requirements": {},
		"roots": {},
		"resolved": {},
		"versions_cache": {"a.b": ["1.0.0", "2.0.0"]}
	}`, helpers.StoreSnapshotSchemaVersion)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err == nil {
		t.Fatalf("expected an error decoding a current-schema snapshot with a corrupt versions_cache bucket, got a store: %#v", loaded)
	}
	if loaded != nil {
		t.Fatalf("expected a nil store alongside the decode error, got %#v", loaded)
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
		st.SetInstalled("a.b@1.0.0", store.InstalledEntry{ArtifactSHA256: testArtifactSHA})
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
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}
}

// TestLoadStoreToleratesLegacyRootsKey confirms LoadStore ignores an unknown
// "roots" key left over from before the field was removed from Store: a
// current-schema payload carrying a populated legacy roots bucket alongside
// real data must decode successfully, with the real data intact. json.Decode
// has no DisallowUnknownFields call anywhere on this path, so an unrecognized
// key is silently skipped rather than rejected - this test locks in that a
// removed-but-still-present key does not turn into a decode error.
func TestLoadStoreToleratesLegacyRootsKey(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := fmt.Appendf(nil, `{
		"meta": {"schema_version": %d, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": {},
		"deps_cache": {},
		"installed": {"a.b@1.0.0": {"install_path": "/tmp/a/b", "artifact_sha256": "abc"}},
		"graph": {},
		"requirements": {},
		"roots": {"last_run": ["a.b@1.0.0"]},
		"resolved": {},
		"versions_cache": {},
		"warmed": {}
	}`, helpers.StoreSnapshotSchemaVersion)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}
}

// TestLoadStoreToleratesNullRootsKey pins that an explicit "roots": null is
// tolerated as an unknown key and decodes with the real data intact.
//
// It exists separately from TestLoadStoreToleratesLegacyRootsKey because null
// and {} are different decoder inputs: only an explicit null overwrites a
// pre-initialized map with a nil one, while {} and an absent key leave it
// alone.
//
// This test does not, and cannot, exercise a panic from a nilled Roots map:
// Store has no Roots field and no SetRoots mutator (see
// TestLoadStoreToleratesLegacyRootsKey for why "roots" itself is still a
// recognized-but-unknown key) for "roots": null to nil out, so there is no
// production call site left for a nil map to reach. This test only pins
// decode tolerance for the null shape itself.
func TestLoadStoreToleratesNullRootsKey(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := fmt.Appendf(nil, `{
		"meta": {"schema_version": %d, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": {},
		"deps_cache": {},
		"installed": {"a.b@1.0.0": {"install_path": "/tmp/a/b", "artifact_sha256": "abc"}},
		"graph": {},
		"requirements": {},
		"roots": null,
		"resolved": {},
		"versions_cache": {},
		"warmed": {}
	}`, helpers.StoreSnapshotSchemaVersion)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}
}

// TestLoadStoreToleratesNullBuckets pins that LoadStore survives a payload
// where all eight of Store's nil-able map buckets are an explicit JSON null,
// and that the loaded store's mutators for those buckets remain usable
// afterward.
//
// Unlike TestLoadStoreToleratesNullRootsKey above - which documents that it
// does NOT pin the panic, because Roots and its only mutator were deleted
// entirely - this test DOES pin the panic: Installed, Warmed, GitPins and the
// other six map fields, and their mutators, still exist on Store, so without
// store.Store.UnmarshalJSON re-allocating a decode-nilled map, the SetInstalled
// / SetWarmed calls below panic with "assignment to entry in nil map" inside
// LoadStore's caller, exactly as production code would when the next install
// or warm worker writes to that bucket.
func TestLoadStoreToleratesNullBuckets(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	rawJSON := fmt.Appendf(nil, `{
		"meta": {"schema_version": %d, "last_snapshot": "2024-01-02T03:04:05Z"},
		"api_cache": null,
		"deps_cache": null,
		"installed": null,
		"graph": null,
		"requirements": null,
		"resolved": null,
		"versions_cache": null,
		"warmed": null,
		"git_pins": null
	}`, helpers.StoreSnapshotSchemaVersion)
	putRawStoreObject(ctx, t, b, rawJSON)

	loaded, err := b.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore error: %v", err)
	}

	loaded.SetInstalled("a.b@1.0.0", store.InstalledEntry{ArtifactSHA256: testArtifactSHA})
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}

	loaded.SetWarmed("a.b@1.0.0", "sha-1")
	if warmed := loaded.WarmedArtifactSHAByKey(); warmed["a.b@1.0.0"] != "sha-1" {
		t.Fatalf("unexpected warmed entry: %#v", warmed)
	}

	const pinCommit = "0123456789abcdef0123456789abcdef01234567"
	loaded.SetGitPin("url\nref\n", store.GitPinEntry{Commit: pinCommit})
	if pin, ok := loaded.GetGitPin("url\nref\n"); !ok || pin.Commit != pinCommit {
		t.Fatalf("unexpected git pin: %#v (ok=%v)", pin, ok)
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
// SaveStore and read back through LoadStore preserves its data exactly,
// including a warmed entry - the S3 backend's gzip-on-the-wire round trip
// must not drop the warmed bucket any more than it drops the others.
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
	st.SetInstalled("a.b@1.0.0", store.InstalledEntry{ArtifactSHA256: testArtifactSHA})
	st.SetWarmed("c.d@1.0.0", "warmed-sha")

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
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry after round trip: %#v (ok=%v)", installed, ok)
	}
	if got := loaded.WarmedArtifactSHAByKey()["c.d@1.0.0"]; got != "warmed-sha" {
		t.Fatalf("unexpected warmed entry after round trip: %q", got)
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
// fail with helpers.ErrResponseTooLarge before it is fully buffered.
func TestReadAllCappedRejectsOversizedRaw(t *testing.T) {
	t.Parallel()

	const compressedCap = 16
	data := bytes.Repeat([]byte("x"), compressedCap*4)

	_, err := readAllCapped(t.Context(), bytes.NewReader(data), http.Header{}, "state/store.json",
		compressedCap, helpers.StateObjectMaxDecompressedSize)
	if !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("readAllCapped() error = %v, want ErrResponseTooLarge", err)
	}
}

// TestReadAllCappedRejectsGzipBomb confirms the decompressed-size ceiling
// applies on the gzip path independently of the compressed-size ceiling: a
// gzip stream well under compressedCap that inflates past a tiny
// decompressedCap must fail with helpers.ErrResponseTooLarge.
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
	_, err := readAllCapped(t.Context(), bytes.NewReader(gz), header, "state/store.json.gz",
		helpers.StateObjectMaxCompressedSize, decompressedCap)
	if !errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("readAllCapped() error = %v, want ErrResponseTooLarge", err)
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
		got, err := readAllCapped(t.Context(), bytes.NewReader(gz), header, "state/store.json.gz", 64, 64)
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

		got, err := readAllCapped(t.Context(), bytes.NewReader(want), http.Header{}, "state/store.json", 64, 64)
		if err != nil {
			t.Fatalf("readAllCapped() error = %v, want nil", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("readAllCapped() = %q, want %q", got, want)
		}
	})
}

// TestReadObjectReclassifiesOversizedStateObject proves readObject's own
// reclassification of a size-ceiling failure, not readAllCapped's: a state
// object that overruns helpers.StateObjectMaxCompressedSize fails with
// helpers.ErrStateObjectTooLarge and NOT helpers.ErrResponseTooLarge, even
// though readAllCapped's own cap failure - the one readObject wraps - always
// carries the latter (TestReadAllCappedRejectsOversizedRaw pins that shape
// directly). This exercises readObject through a real HTTP round trip
// against the fake S3 server, at the actual production ceiling, rather than
// readAllCapped's own custom-cap unit tests above.
//
// The positive control lives in the same test, against the same key prefix
// and the same readObject call: a within-cap object at a neighboring key
// round-trips its exact bytes with a nil error, proving the oversized case
// above is a real refusal readObject's success path could otherwise have
// taken, not evidence the fixture never reaches that path at all. That half
// stays a real stored object and a real upload; only the oversized half is
// generated.
func TestReadObjectReclassifiesOversizedStateObject(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// One byte past the compressed-size ceiling, on the non-gzip path, so the
	// fixture needs no gzip compression/decompression work to trip the cap.
	// The body is generated as it is served rather than stored: the ceiling is
	// 256 MiB, so materializing it would cost that twice - once in this test
	// and once in the fake - plus an upload of the same size that proves
	// nothing. Nothing about the ceiling itself changes, so this still
	// exercises readObject at the production constants rather than at a
	// custom cap.
	oversizedKey := b.key(statePrefix, "oversized-state-object.json")
	fake.serveSyntheticBody(oversizedKey, helpers.StateObjectMaxCompressedSize+1)

	_, err := b.readObject(ctx, oversizedKey)
	if !errors.Is(err, helpers.ErrStateObjectTooLarge) {
		t.Fatalf("readObject(oversized) error = %v, want errors.Is(err, ErrStateObjectTooLarge) = true", err)
	}
	// The load-bearing partition check: readAllCapped's own cap failure
	// always carries helpers.ErrResponseTooLarge, since it is built on the
	// same sizeLimitedReader every capped response uses. This must be false
	// only because readObject deliberately breaks that errors.Is chain by
	// rendering the cause with %v instead of %w.
	if errors.Is(err, helpers.ErrResponseTooLarge) {
		t.Fatalf("readObject(oversized) error = %v, want errors.Is(err, ErrResponseTooLarge) = false", err)
	}

	withinCapKey := b.key(statePrefix, "within-cap-state-object.json")
	want := []byte(`{"projects":{}}`)
	if err := b.client.putObject(ctx, withinCapKey, bytes.NewReader(want), int64(len(want)),
		putObjectAttrs{contentType: "application/json"}, putCondition{}); err != nil {
		t.Fatalf("putObject(within cap): %v", err)
	}
	got, err := b.readObject(ctx, withinCapKey)
	if err != nil {
		t.Fatalf("readObject(within cap) error = %v, want nil", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readObject(within cap) = %q, want %q", got, want)
	}
}

// emptyGzipMember is the smallest gzip member there is - the ten-byte header,
// one fixed-Huffman final block carrying nothing, and an eight-byte trailer
// over zero bytes - hand-spelled so the fixture is exactly the shape a hostile
// writer of this bucket would put there rather than whatever a writer in this
// process happens to emit.
func emptyGzipMember() []byte {
	member := make([]byte, 0, 20)
	member = append(member, "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff"...)
	member = append(member, 0x03, 0x00)
	return append(member, 0, 0, 0, 0, 0, 0, 0, 0)
}

// TestReadObjectReclassifiesAStateObjectThatWillNotInflate proves readObject's
// second reclassification: a state object whose gzip stream produces no bytes
// is helpers.ErrCorruptStateObject, so a pipeline branching on
// ExitCacheCorrupt sees the object as one to discard rather than as the
// unclassified generic failure the bare inflate verdict would have been.
//
// The bare verdict is what makes the wrap load-bearing rather than cosmetic:
// helpers.ErrEmptyGzipMember belongs to no cmd/go-galaxy/exitcode predicate -
// deliberately, since its other readers classify through their own wraps - so
// without this one the same failure reaches FromError as ExitError. The cause
// stays reachable underneath, which the second assertion pins: the wrap
// reclassifies without hiding what happened.
//
// The positive control lives in the same test, against the same key suffix and
// the same readObject call: an ordinary gzipped object at a neighboring key
// round-trips its exact bytes with a nil error, so the refusal above is a real
// one on the gzip path rather than evidence the fixture never reached it.
//
// Killing mutation, run: deleting the helpers.ErrEmptyGzipMember arm from
// readObject, leaving the failure to be returned as it arrives, fails this
// test with
//
//	backend_test.go:661: readObject(empty member) = gzip stream carries a member that produces no bytes, want ErrCorruptStateObject
func TestReadObjectReclassifiesAStateObjectThatWillNotInflate(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackendAndFake(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The .gz suffix is what routes readAllCapped down its inflating arm, the
	// same way the real snapshot object's own key does.
	emptyMemberKey := b.key(statePrefix, "empty-member-state-object.json.gz")
	member := emptyGzipMember()
	if err := b.client.putObject(ctx, emptyMemberKey, bytes.NewReader(member), int64(len(member)),
		putObjectAttrs{contentType: "application/gzip"}, putCondition{}); err != nil {
		t.Fatalf("putObject(empty member): %v", err)
	}

	_, err := b.readObject(ctx, emptyMemberKey)
	if !errors.Is(err, helpers.ErrCorruptStateObject) {
		t.Fatalf("readObject(empty member) = %v, want ErrCorruptStateObject", err)
	}
	if !errors.Is(err, helpers.ErrEmptyGzipMember) {
		t.Fatalf("readObject(empty member) = %v, want its cause reachable", err)
	}

	inflatableKey := b.key(statePrefix, "inflatable-state-object.json.gz")
	want := []byte(`{"projects":{}}`)
	body := gzipBytes(t, want)
	if err := b.client.putObject(ctx, inflatableKey, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip"}, putCondition{}); err != nil {
		t.Fatalf("putObject(inflatable): %v", err)
	}
	got, err := b.readObject(ctx, inflatableKey)
	if err != nil {
		t.Fatalf("readObject(inflatable) error = %v, want nil", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readObject(inflatable) = %q, want %q", got, want)
	}
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
	attrs := putObjectAttrs{contentType: "application/json"}
	if err := b.client.putObject(ctx, key, reader, int64(len(data)), attrs, putCondition{}); err != nil {
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
	attrs := putObjectAttrs{contentType: "application/json", contentEncoding: "gzip"}
	if err := b.client.putObject(ctx, key, reader, int64(buf.Len()), attrs, putCondition{}); err != nil {
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
	attrs := putObjectAttrs{contentType: "application/json", contentEncoding: "gzip"}
	if err := b.client.putObject(ctx, key, reader, int64(buf.Len()), attrs, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}
}
