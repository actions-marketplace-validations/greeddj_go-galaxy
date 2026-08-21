package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// errTestResolvedBucketMissing is a static test-only error used when the
// resolved bucket is unexpectedly absent while corrupting a test fixture.
var errTestResolvedBucketMissing = errors.New("resolved bucket missing")

// errTestAPICacheBucketMissing is a static test-only error used when the
// api_cache bucket is unexpectedly absent while corrupting a test fixture.
var errTestAPICacheBucketMissing = errors.New("api_cache bucket missing")

// testDepsConstraint is the shared dependency constraint value seeded across
// several deps-cache test fixtures.
const testDepsConstraint = ">=1.0.0"

// testArtifactSHA is the shared artifact sha value seeded across several
// installed-entry test fixtures.
const testArtifactSHA = "abc"

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	mustSave(t, dbs, st)
	loaded := mustLoad(t, dbs)
	assertMeta(t, loaded)
	assertAPICache(t, loaded)
	assertDepsCache(t, loaded)
	assertInstalled(t, loaded)
	assertGraph(t, loaded)
	assertRequirements(t, loaded)
	assertResolved(t, loaded)
	assertVersions(t, loaded)
	assertWarmed(t, loaded)
	assertGitPin(t, loaded)
}

func openTestDBs(t *testing.T) *DBs {
	t.Helper()
	dir := t.TempDir()
	dbs, err := OpenDBs(dir, helpers.BoltOpenTimeout)
	if err != nil {
		t.Fatalf("OpenDBs error: %v", err)
	}
	t.Cleanup(func() {
		_ = dbs.Close()
	})
	return dbs
}

func buildTestStore(fixed time.Time) *Store {
	return populateTestStore(New(), fixed)
}

// populateTestStore writes a fixed, known fixture into every one of Store's
// nine map buckets via their normal mutators (SetAPICache, SetDepsCache,
// SetInstalled, SetGraph, SetRequirements, SetResolvedAll, SetVersionsCache,
// SetWarmed, SetGitPin). It is factored out of buildTestStore so a test can also apply
// this same fixture to a store obtained some other way (e.g. one just
// decoded from JSON), then reuse the assert* helpers below to verify it
// without duplicating the fixture values.
func populateTestStore(st *Store, fixed time.Time) *Store {
	st.SetMetaRequirements("req-hash", "https://example.com")
	st.SetAPICache("api", APICacheEntry{
		// FetchedAt uses the current time rather than the fixed fixture
		// clock: APICache is now subject to CacheEntryMaxAge pruning at
		// persist time, and fixed predates the retention window relative to
		// the current wall clock, which would make this entry vanish on
		// every Save/MarshalSnapshot round trip regardless of the behavior
		// under test.
		FetchedAt: time.Now().UTC(),
		URL:       "https://example.com/api",
		ETag:      "etag",
		TTL:       time.Minute,
		Body:      []byte(`{"ok":true}`),
	})
	st.SetDepsCache("deps", map[string]string{"a.b": testDepsConstraint})
	st.SetInstalled("a.b@1.0.0", InstalledEntry{
		InstallPath:    "/tmp/a/b",
		Source:         "https://example.com",
		ArtifactSHA256: testArtifactSHA,
		InstalledAt:    fixed,
		Deps:           []string{"c.d@1.2.3"},
	})
	st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
	st.SetRequirements(map[string]RequirementSpec{
		"a.b": {Constraint: "1.0.0", Source: "https://example.com", Type: "galaxy"},
	})
	st.SetResolvedAll(map[string]ResolvedEntry{
		"a.b": {Version: "1.0.0", Source: "https://example.com"},
	})
	st.SetVersionsCache("versions", []string{"1.0.0", "2.0.0"})
	st.SetWarmed("a.b@1.0.0", "warmed-sha")
	st.SetGitPin(testGitPinKey, GitPinEntry{
		Commit: testGitPinCommit,
		Collections: []GitPinCollection{{
			Namespace:    "acme",
			Name:         "app",
			Version:      "1.2.3",
			Subdir:       "collections/app",
			Dependencies: map[string]string{"a.b": testDepsConstraint},
		}},
	})
	return st
}

const (
	testGitPinKey    = "https://github.com/acme/app.git\nmain\n"
	testGitPinCommit = "0123456789abcdef0123456789abcdef01234567"
)

func assertGitPin(t *testing.T, loaded *Store) {
	t.Helper()
	pin, ok := loaded.GetGitPin(testGitPinKey)
	if !ok || pin.Commit != testGitPinCommit || len(pin.Collections) != 1 {
		t.Fatalf("unexpected git pin: %#v (ok=%t)", pin, ok)
	}
	c := pin.Collections[0]
	if c.Namespace != "acme" || c.Name != "app" || c.Version != "1.2.3" || c.Subdir != "collections/app" ||
		c.Dependencies["a.b"] != testDepsConstraint {
		t.Fatalf("unexpected git pin collection: %#v", c)
	}
	if pin.FetchedAt.IsZero() {
		t.Fatalf("git pin FetchedAt was not stamped")
	}
}

func mustSave(t *testing.T, dbs *DBs, st *Store) {
	t.Helper()
	if err := Save(dbs, st); err != nil {
		t.Fatalf("Save error: %v", err)
	}
}

func mustLoad(t *testing.T, dbs *DBs) *Store {
	t.Helper()
	loaded, err := Load(dbs)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	return loaded
}

func assertMeta(t *testing.T, loaded *Store) {
	t.Helper()
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("unexpected schema version: %d", loaded.Meta.SchemaVersion)
	}
	if loaded.Meta.RequirementsHash != "req-hash" {
		t.Fatalf("unexpected requirements hash: %q", loaded.Meta.RequirementsHash)
	}
	if loaded.Meta.Server != "https://example.com" {
		t.Fatalf("unexpected server: %q", loaded.Meta.Server)
	}
	if loaded.Meta.LastSnapshot.IsZero() {
		t.Fatalf("expected LastSnapshot to be set")
	}
}

func assertAPICache(t *testing.T, loaded *Store) {
	t.Helper()
	entry, ok := loaded.GetAPICache("api")
	if !ok {
		t.Fatalf("expected API cache entry")
	}
	if entry.URL != "https://example.com/api" {
		t.Fatalf("unexpected api cache url: %q", entry.URL)
	}
	if string(entry.Body) != `{"ok":true}` {
		t.Fatalf("unexpected api cache body: %s", string(entry.Body))
	}
}

func assertDepsCache(t *testing.T, loaded *Store) {
	t.Helper()
	deps, ok := loaded.GetDepsCache("deps")
	if !ok || deps["a.b"] != testDepsConstraint {
		t.Fatalf("unexpected deps cache: %#v", deps)
	}
}

func assertInstalled(t *testing.T, loaded *Store) {
	t.Helper()
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v", installed)
	}
}

func assertGraph(t *testing.T, loaded *Store) {
	t.Helper()
	graph := loaded.GraphSnapshot()
	if len(graph["a.b@1.0.0"]) != 1 || graph["a.b@1.0.0"][0] != "c.d@1.2.3" {
		t.Fatalf("unexpected graph: %#v", graph)
	}
}

func assertRequirements(t *testing.T, loaded *Store) {
	t.Helper()
	reqs := loaded.RequirementsSnapshot()
	if reqs["a.b"].Constraint != "1.0.0" {
		t.Fatalf("unexpected requirements: %#v", reqs)
	}
}

func assertResolved(t *testing.T, loaded *Store) {
	t.Helper()
	resolved := loaded.ResolvedSnapshot()
	if resolved["a.b"].Version != "1.0.0" {
		t.Fatalf("unexpected resolved: %#v", resolved)
	}
}

func assertVersions(t *testing.T, loaded *Store) {
	t.Helper()
	versions, ok := loaded.GetVersionsCache("versions")
	if !ok || len(versions) != 2 {
		t.Fatalf("unexpected versions cache: %#v", versions)
	}
}

func assertWarmed(t *testing.T, loaded *Store) {
	t.Helper()
	warmed := loaded.WarmedArtifactSHAByKey()
	if got := warmed["a.b@1.0.0"]; got != "warmed-sha" {
		t.Fatalf("unexpected warmed entry: %#v", warmed)
	}
}

// TestSaveRollsBackWholeTransactionOnMidSaveFailure proves that Save writes
// the meta bucket and all eight data buckets inside a single Bolt
// transaction: a failure partway through (here, an oversized key in the
// installed bucket, which the fixed save order writes after api_cache)
// must roll back the entire attempt, leaving the previously committed
// snapshot exactly as it was rather than a mix of old and new data.
func TestSaveRollsBackWholeTransactionOnMidSaveFailure(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)

	st := New()
	// FetchedAt must be within the retention window, since api_cache is now
	// subject to CacheEntryMaxAge pruning at persist time: a zero-value
	// FetchedAt would make this entry vanish on every Save regardless of the
	// rollback behavior under test.
	st.SetAPICache("api", APICacheEntry{URL: "v1", FetchedAt: time.Now().UTC()})
	st.SetInstalled("normal.key", InstalledEntry{Source: "v1"})
	mustSave(t, dbs, st)

	// Mutate api_cache (written before installed) and inject a key that
	// exceeds bolt.MaxKeySize into installed (written after api_cache),
	// forcing a mid-transaction failure in a non-first bucket.
	st.SetAPICache("api", APICacheEntry{URL: "v2", FetchedAt: time.Now().UTC()})
	giantKey := strings.Repeat("k", 40000)
	st.SetInstalled(giantKey, InstalledEntry{Source: "v2"})

	err := Save(dbs, st)
	if err == nil {
		t.Fatalf("expected Save to fail on oversized key")
	}
	if !errors.Is(err, bolterrors.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge, got %v", err)
	}

	loaded := mustLoad(t, dbs)
	entry, ok := loaded.GetAPICache("api")
	if !ok || entry.URL != "v1" {
		t.Fatalf("expected api cache to remain at v1 after rollback, got %#v (ok=%v)", entry, ok)
	}
	if _, ok := loaded.GetInstalled(giantKey); ok {
		t.Fatalf("expected giant key to be absent after rolled-back save")
	}
	if _, ok := loaded.GetInstalled("normal.key"); !ok {
		t.Fatalf("expected normal.key from the first save to survive")
	}
}

// TestLoadRejectsNewerSchemaAndDropsOlderSchema exercises ValidateSchema
// through Load: a newer-than-current schema version is reported as an
// error (this binary cannot safely interpret it), while an older-than
// -current version causes Load to drop the snapshot and return a fresh
// empty Store with a nil error rather than partially trusting stale data.
func TestLoadRejectsNewerSchemaAndDropsOlderSchema(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	mustSave(t, dbs, st)

	newerVersion := helpers.StoreSnapshotSchemaVersion + 1
	stampSchemaVersion(t, dbs, newerVersion)
	if _, err := Load(dbs); !errors.Is(err, helpers.ErrUnsupportedSchemaVersion) {
		t.Fatalf("expected ErrUnsupportedSchemaVersion, got %v", err)
	}

	olderVersion := helpers.StoreSnapshotSchemaVersion - 1
	stampSchemaVersion(t, dbs, olderVersion)
	loaded, err := Load(dbs)
	if err != nil {
		t.Fatalf("expected nil error for outdated schema, got %v", err)
	}
	fresh := New()
	if loaded.Meta.SchemaVersion != fresh.Meta.SchemaVersion {
		t.Fatalf("expected fresh schema version %d, got %d", fresh.Meta.SchemaVersion, loaded.Meta.SchemaVersion)
	}
	if len(loaded.APICache) != 0 || len(loaded.Installed) != 0 {
		t.Fatalf("expected an empty store after dropping an outdated schema, got %#v", loaded)
	}
}

// stampSchemaVersion directly overwrites the meta bucket's schema version
// key, bypassing Save, to simulate a snapshot written by a different
// binary version.
func stampSchemaVersion(t *testing.T, dbs *DBs, version int) {
	t.Helper()
	err := dbs.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketMeta))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(helpers.StoreMetaSchemaVersion), []byte(strconv.Itoa(version)))
	})
	if err != nil {
		t.Fatalf("failed to stamp schema version: %v", err)
	}
}

// TestWasPersistedFalseForFreshStore proves a brand-new store built via New()
// - never saved to or loaded from any backend - reports no persisted
// snapshot, since New() leaves Meta.LastSnapshot at its zero value.
func TestWasPersistedFalseForFreshStore(t *testing.T) {
	t.Parallel()
	if New().WasPersisted() {
		t.Fatal("expected a fresh store to report WasPersisted() == false")
	}
}

// TestWasPersistedFalseForNilStore proves WasPersisted follows the package's
// nil-receiver convention (and is the conservative answer here): a nil
// *Store carries no evidence of anything, so it must never be read as "yes,
// a snapshot was persisted".
func TestWasPersistedFalseForNilStore(t *testing.T) {
	t.Parallel()
	var st *Store
	if st.WasPersisted() {
		t.Fatal("expected a nil store to report WasPersisted() == false")
	}
}

// TestWasPersistedTrueAfterLocalSaveLoadRoundTrip proves a store that went
// through a real local Save -> Load round trip reports a persisted snapshot,
// since Save unconditionally stamps Meta.LastSnapshot before writing.
func TestWasPersistedTrueAfterLocalSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	mustSave(t, dbs, st)

	loaded := mustLoad(t, dbs)
	if !loaded.WasPersisted() {
		t.Fatal("expected a store loaded after a real Save to report WasPersisted() == true")
	}
}

// TestWasPersistedTrueAfterMarshalSnapshotUnmarshal pins the S3 wire path:
// MarshalSnapshot stamps Meta.LastSnapshot exactly as Save does, so a store
// decoded from its JSON output - the shape the S3 backend's LoadStore
// produces on a successful fetch - must also report a persisted snapshot.
func TestWasPersistedTrueAfterMarshalSnapshotUnmarshal(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)

	data, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot error: %v", err)
	}

	decoded := New()
	if err := json.Unmarshal(data, decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if !decoded.WasPersisted() {
		t.Fatal("expected a store decoded from MarshalSnapshot's output to report WasPersisted() == true")
	}
}

// TestWasPersistedFalseAfterOutdatedSchemaLoad is the exact post-schema-bump
// scenario: a Bolt DB was genuinely saved by an older binary (so its meta
// bucket carries a real, non-zero last_snapshot), but its schema_version is
// then downgraded below the current one - simulating a snapshot schema bump
// landing on a cache directory a previous binary version already wrote to.
// Load drops it and returns a fresh store (New()) rather than partially
// trusting it, so WasPersisted must report false even though the underlying
// Bolt bytes do contain a non-zero last_snapshot value - that stale value
// belongs to the dropped store, not to the one Load actually returned.
func TestWasPersistedFalseAfterOutdatedSchemaLoad(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	st := buildTestStore(fixed)
	mustSave(t, dbs, st)

	olderVersion := helpers.StoreSnapshotSchemaVersion - 1
	stampSchemaVersion(t, dbs, olderVersion)

	loaded := mustLoad(t, dbs)
	if loaded.WasPersisted() {
		t.Fatal("expected a store dropped for an outdated schema to report WasPersisted() == false")
	}
}

// TestLoadRejectsCorruptResolvedEntry proves Load reports an error
// instead of silently coercing a genuinely corrupt resolved value into a
// garbage version string. Every current-schema value is written as valid
// JSON by saveJSONBucket, so an unmarshal failure here can only mean the
// stored bytes are corrupt.
func TestLoadRejectsCorruptResolvedEntry(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	st := New()
	st.SetResolvedAll(map[string]ResolvedEntry{
		"a.b": {Version: "1.0.0"},
	})
	mustSave(t, dbs, st)

	err := dbs.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(helpers.StoreBucketResolved))
		if bucket == nil {
			return errTestResolvedBucketMissing
		}
		return bucket.Put([]byte("a.b"), []byte("{not-json"))
	})
	if err != nil {
		t.Fatalf("failed to corrupt resolved entry: %v", err)
	}

	_, err = Load(dbs)
	if err == nil {
		t.Fatalf("expected Load to reject a corrupt resolved entry")
	}
	if !strings.Contains(err.Error(), "a.b") {
		t.Fatalf("expected error to mention the corrupt key, got %v", err)
	}
}

// TestLoadNamesBucketAndKeyOfCorruptEntry proves the error Load reports for a
// corrupt entry names both the bucket the value came from and the key it was
// stored under. The bucket is api_cache rather than resolved, so the two tests
// together cover a wrapper shared across buckets instead of one loader whose
// message happens to be special-cased to a single bucket.
func TestLoadNamesBucketAndKeyOfCorruptEntry(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	st := New()
	// FetchedAt is sampled fresh rather than left zero: SetAPICache does not
	// stamp it, and snapshotData prunes api_cache against CacheEntryMaxAge, so
	// a zero stamp drops the entry at save time and the corruption below would
	// have no stored value to land on.
	st.SetAPICache("a.b", APICacheEntry{
		URL:       "https://example.com/api",
		FetchedAt: time.Now().UTC(),
		Body:      []byte(`{"ok":true}`),
	})
	mustSave(t, dbs, st)

	// Positive control on this same fixture, before it is corrupted: the entry
	// has to be shown loading cleanly, otherwise the refusal below cannot be
	// told apart from a fixture whose value never reaches the decode at all.
	if _, ok := mustLoad(t, dbs).GetAPICache("a.b"); !ok {
		t.Fatal("expected the intact fixture to load its api_cache entry")
	}

	err := dbs.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(helpers.StoreBucketAPICache))
		if bucket == nil {
			return errTestAPICacheBucketMissing
		}
		return bucket.Put([]byte("a.b"), []byte("{not-json"))
	})
	if err != nil {
		t.Fatalf("failed to corrupt api_cache entry: %v", err)
	}

	_, err = Load(dbs)
	// This guard is documentary rather than pinned: it exists so the two
	// assertions below can call err.Error() at all. Neither dropping the
	// bucket nor dropping the key from loadJSONBucket's format string reaches
	// it, since both still return an error - the runs quoted below stop lower.
	if err == nil {
		t.Fatal("expected Load to reject a corrupt api_cache entry")
	}
	// Replacing loadJSONBucket's wrapped return with a bare `return err` stops
	// here, and fails TestLoadRejectsCorruptResolvedEntry as well:
	// expected error to name the bucket, got invalid character 'n' looking for beginning of object key string
	//
	// Dropping the bucket from that format string ("invalid entry %q: %w")
	// stops here too, while TestLoadRejectsCorruptResolvedEntry keeps passing,
	// since its own assertion is on the key alone:
	// expected error to name the bucket, got invalid entry "a.b": invalid character 'n' looking for beginning of object key string
	if !strings.Contains(err.Error(), helpers.StoreBucketAPICache) {
		t.Fatalf("expected error to name the bucket, got %v", err)
	}
	// Dropping the key from that format string ("invalid %s entry: %w") leaves
	// both assertions above satisfied and stops here, and it fails
	// TestLoadRejectsCorruptResolvedEntry, whose sole assertion is the key:
	// expected error to name the corrupt key, got invalid api_cache entry: invalid character 'n' looking for beginning of object key string
	if !strings.Contains(err.Error(), "a.b") {
		t.Fatalf("expected error to name the corrupt key, got %v", err)
	}
}

// TestSnapshotV4RoundTripLocal proves the schema-4 wire shape - APICache,
// DepsCache, and Versions each carrying a FetchedAt stamp - round-trips
// through Save/Load intact. The wire shape has carried forward unchanged
// through later schema bumps (a bump gates key-format changes, not this
// struct shape), so the assertion compares against the live
// helpers.StoreSnapshotSchemaVersion rather than a stale literal.
func TestSnapshotV4RoundTripLocal(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)
	// recent, not a fixed historical date: APICache is now subject to
	// CacheEntryMaxAge pruning at persist time, so a stamp must fall within
	// the retention window for the entry to survive the round trip this
	// test exercises.
	recent := time.Now().UTC().Add(-time.Hour)

	st := New()
	st.SetAPICache("api", APICacheEntry{URL: "https://example.com/api", FetchedAt: recent, Body: []byte("body")})
	st.SetDepsCache("deps", map[string]string{"a.b": testDepsConstraint})
	st.SetVersionsCache("versions", []string{"1.0.0", "2.0.0"})
	mustSave(t, dbs, st)

	loaded := mustLoad(t, dbs)
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", helpers.StoreSnapshotSchemaVersion, loaded.Meta.SchemaVersion)
	}
	assertV4APICacheEntry(t, loaded, recent)
	assertV4DepsCacheEntry(t, loaded)
	assertV4VersionsEntry(t, loaded)
}

// assertV4APICacheEntry confirms the "api" key survived a round trip with
// its FetchedAt and Body intact.
func assertV4APICacheEntry(t *testing.T, loaded *Store, wantFetchedAt time.Time) {
	t.Helper()
	apiEntry, ok := loaded.GetAPICache("api")
	if !ok || !apiEntry.FetchedAt.Equal(wantFetchedAt) || string(apiEntry.Body) != "body" {
		t.Fatalf("unexpected api cache entry after round trip: %#v (ok=%v)", apiEntry, ok)
	}
}

// assertV4DepsCacheEntry confirms the "deps" key survived a round trip as a
// DepsCacheEntry with a non-zero FetchedAt stamp.
func assertV4DepsCacheEntry(t *testing.T, loaded *Store) {
	t.Helper()
	depsEntry, ok := loaded.DepsCache["deps"]
	if !ok || depsEntry.Deps["a.b"] != testDepsConstraint || depsEntry.FetchedAt.IsZero() {
		t.Fatalf("unexpected deps cache entry after round trip: %#v (ok=%v)", depsEntry, ok)
	}
}

// assertV4VersionsEntry confirms the "versions" key survived a round trip as
// a VersionsEntry with a non-zero FetchedAt stamp.
func assertV4VersionsEntry(t *testing.T, loaded *Store) {
	t.Helper()
	versionsEntry, ok := loaded.Versions["versions"]
	if !ok || len(versionsEntry.List) != 2 || versionsEntry.FetchedAt.IsZero() {
		t.Fatalf("unexpected versions cache entry after round trip: %#v (ok=%v)", versionsEntry, ok)
	}
}

// TestSnapshotV4RoundTripJSON proves MarshalSnapshot's schema-4 wire shape
// round-trips through a plain json.Unmarshal, and that the raw JSON encodes
// versions_cache and deps_cache values as objects (fetched_at plus the
// payload), not the bare arrays/maps of the pre-v4 shape. As with
// TestSnapshotV4RoundTripLocal, this shape is unchanged by later schema
// bumps, so the version assertion compares against the live
// helpers.StoreSnapshotSchemaVersion.
func TestSnapshotV4RoundTripJSON(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetAPICache("api", APICacheEntry{URL: "https://example.com/api", Body: []byte("body")})
	st.SetDepsCache("deps", map[string]string{"a.b": testDepsConstraint})
	st.SetVersionsCache("versions", []string{"1.0.0", "2.0.0"})

	payload, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot error: %v", err)
	}

	var loaded Store
	if err := json.Unmarshal(payload, &loaded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if loaded.Meta.SchemaVersion != helpers.StoreSnapshotSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", helpers.StoreSnapshotSchemaVersion, loaded.Meta.SchemaVersion)
	}
	depsEntry, ok := loaded.DepsCache["deps"]
	if !ok || depsEntry.Deps["a.b"] != testDepsConstraint || depsEntry.FetchedAt.IsZero() {
		t.Fatalf("unexpected deps cache entry after JSON round trip: %#v (ok=%v)", depsEntry, ok)
	}
	versionsEntry, ok := loaded.Versions["versions"]
	if !ok || len(versionsEntry.List) != 2 || versionsEntry.FetchedAt.IsZero() {
		t.Fatalf("unexpected versions cache entry after JSON round trip: %#v (ok=%v)", versionsEntry, ok)
	}

	assertObjectShape(t, payload, "versions_cache", "versions", "fetched_at", "list")
	assertObjectShape(t, payload, "deps_cache", "deps", "fetched_at", "deps")
}

// assertObjectShape confirms that raw[bucket][key] decodes as a JSON object
// carrying both expected field names, rather than a bare array or map (the
// pre-v4 shape for versions_cache and deps_cache respectively).
func assertObjectShape(t *testing.T, payload []byte, bucket, key, firstField, secondField string) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("json.Unmarshal raw payload error: %v", err)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw[bucket], &entries); err != nil {
		t.Fatalf("json.Unmarshal %s error: %v", bucket, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entries[key], &fields); err != nil {
		t.Fatalf("%s[%q] is not a JSON object: %v", bucket, key, err)
	}
	if _, ok := fields[firstField]; !ok {
		t.Fatalf("%s[%q] missing field %q: %s", bucket, key, firstField, entries[key])
	}
	if _, ok := fields[secondField]; !ok {
		t.Fatalf("%s[%q] missing field %q: %s", bucket, key, secondField, entries[key])
	}
}

// TestSnapshotPrunesStaleAndFutureAcrossAllBuckets proves entries outside
// their retention window - both a stale entry last written before the window
// and a future entry stamped ahead of the sampling instant - are dropped
// from the persisted snapshot in all four age-eviction buckets (APICache,
// DepsCache, and Versions against CacheEntryMaxAge; Warmed against its own
// separate helpers.WarmedEntryMaxAge window), while entries within the
// window survive, across both persist paths (local Bolt Save/Load and
// MarshalSnapshot/json.Unmarshal).
func TestSnapshotPrunesStaleAndFutureAcrossAllBuckets(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	stale := now.Add(-40 * 24 * time.Hour)
	fresh := now.Add(-1 * 24 * time.Hour)
	future := now.Add(365 * 24 * time.Hour)

	build := func() *Store {
		st := New()
		st.APICache["api-stale"] = APICacheEntry{URL: "stale", FetchedAt: stale}
		st.APICache["api-fresh"] = APICacheEntry{URL: "fresh", FetchedAt: fresh}
		st.APICache["api-future"] = APICacheEntry{URL: "future", FetchedAt: future}
		st.DepsCache["deps-stale"] = DepsCacheEntry{FetchedAt: stale, Deps: map[string]string{"a.b": "1"}}
		st.DepsCache["deps-fresh"] = DepsCacheEntry{FetchedAt: fresh, Deps: map[string]string{"a.b": "1"}}
		st.DepsCache["deps-future"] = DepsCacheEntry{FetchedAt: future, Deps: map[string]string{"a.b": "1"}}
		st.Versions["versions-stale"] = VersionsEntry{FetchedAt: stale, List: []string{"1.0.0"}}
		st.Versions["versions-fresh"] = VersionsEntry{FetchedAt: fresh, List: []string{"1.0.0"}}
		st.Versions["versions-future"] = VersionsEntry{FetchedAt: future, List: []string{"1.0.0"}}
		st.Warmed["warmed-stale"] = WarmedEntry{WarmedAt: stale, ArtifactSHA256: "sha-stale"}
		st.Warmed["warmed-fresh"] = WarmedEntry{WarmedAt: fresh, ArtifactSHA256: "sha-fresh"}
		st.Warmed["warmed-future"] = WarmedEntry{WarmedAt: future, ArtifactSHA256: "sha-future"}
		return st
	}

	t.Run("bolt", func(t *testing.T) {
		t.Parallel()
		dbs := openTestDBs(t)
		mustSave(t, dbs, build())
		assertStalePruned(t, mustLoad(t, dbs))
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		payload, err := build().MarshalSnapshot()
		if err != nil {
			t.Fatalf("MarshalSnapshot error: %v", err)
		}
		var loaded Store
		if err := json.Unmarshal(payload, &loaded); err != nil {
			t.Fatalf("json.Unmarshal error: %v", err)
		}
		assertStalePruned(t, &loaded)
	})
}

// bucketHasKey reports whether key is present in m. It is generic purely so
// assertStalePruned can drive one loop over all four differently-typed
// buckets instead of repeating the same three-way presence check per bucket,
// which would push that function over its cyclomatic complexity budget.
func bucketHasKey[T any](m map[string]T, key string) bool {
	_, ok := m[key]
	return ok
}

// assertStalePruned checks that every "-stale" and "-future" entry seeded by
// TestSnapshotPrunesStaleAndFutureAcrossAllBuckets's build helper is gone and
// every "-fresh" entry survived, across all four age-eviction buckets.
func assertStalePruned(t *testing.T, loaded *Store) {
	t.Helper()
	cases := []struct {
		bucket                        string
		hasStale, hasFresh, hasFuture bool
	}{
		{
			"api cache",
			bucketHasKey(loaded.APICache, "api-stale"),
			bucketHasKey(loaded.APICache, "api-fresh"),
			bucketHasKey(loaded.APICache, "api-future"),
		},
		{
			"deps cache",
			bucketHasKey(loaded.DepsCache, "deps-stale"),
			bucketHasKey(loaded.DepsCache, "deps-fresh"),
			bucketHasKey(loaded.DepsCache, "deps-future"),
		},
		{
			"versions cache",
			bucketHasKey(loaded.Versions, "versions-stale"),
			bucketHasKey(loaded.Versions, "versions-fresh"),
			bucketHasKey(loaded.Versions, "versions-future"),
		},
		{
			"warmed",
			bucketHasKey(loaded.Warmed, "warmed-stale"),
			bucketHasKey(loaded.Warmed, "warmed-fresh"),
			bucketHasKey(loaded.Warmed, "warmed-future"),
		},
	}
	for _, c := range cases {
		assertBucketRetention(t, c.bucket, c.hasStale, c.hasFresh, c.hasFuture)
	}
}

// assertBucketRetention fails (via Error, not Fatal) unless a single bucket's
// stale entry was pruned, its fresh entry survived, and its future entry was
// pruned. It never stops early, so a single call to assertStalePruned reports
// every failing bucket in one test run instead of only the first.
func assertBucketRetention(t *testing.T, bucket string, hasStale, hasFresh, hasFuture bool) {
	t.Helper()
	if hasStale {
		t.Errorf("expected stale %s entry to be pruned", bucket)
	}
	if !hasFresh {
		t.Errorf("expected fresh %s entry to survive", bucket)
	}
	if hasFuture {
		t.Errorf("expected future %s entry to be pruned", bucket)
	}
}

// TestSnapshotBoundaryEntryIsKept proves retentionWindow.isStale's
// exact-equality boundary on both ends of the closed [oldest, newest]
// interval: an entry stamped exactly at oldest is kept (strict Before is
// false at equality), an entry one nanosecond before oldest is pruned, an
// entry stamped exactly at newest is kept (strict After is false at
// equality), and an entry one nanosecond after newest - the future case - is
// pruned. This tests the comparison directly against a constructed window
// rather than through a full Save/MarshalSnapshot round trip, since the real
// window is sampled from the wall clock at persist time and cannot be
// predicted precisely enough from a test goroutine to hit either boundary
// deterministically. The zero time.Time needs no separate rule: it is
// already stale via the oldest bound, since it falls before any window built
// from time.Now().
func TestSnapshotBoundaryEntryIsKept(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	w := newRetentionWindow(now, time.Hour)

	if w.isStale(w.oldest) {
		t.Fatalf("expected an entry stamped exactly at the oldest bound to be kept")
	}
	if !w.isStale(w.oldest.Add(-time.Nanosecond)) {
		t.Fatalf("expected an entry stamped one nanosecond before the oldest bound to be pruned")
	}
	if w.isStale(w.newest) {
		t.Fatalf("expected an entry stamped exactly at the newest bound to be kept")
	}
	if !w.isStale(w.newest.Add(time.Nanosecond)) {
		t.Fatalf("expected an entry stamped one nanosecond after the newest bound (future) to be pruned")
	}
}

// TestLoadDropsV3BoltAndRebuilds proves Load's schema check happens before
// any data bucket is decoded: a v3-stamped snapshot whose versions_cache and
// deps_cache buckets still hold the pre-schema-4 bare shapes (JSON arrays
// and plain maps, without a fetched_at wrapper) is dropped and rebuilt
// rather than failing to decode into the current wrapper structs.
func TestLoadDropsV3BoltAndRebuilds(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)

	err := dbs.db.Update(func(tx *bolt.Tx) error {
		metaBucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketMeta))
		if err != nil {
			return err
		}
		if err := metaBucket.Put([]byte(helpers.StoreMetaSchemaVersion), []byte(strconv.Itoa(3))); err != nil {
			return err
		}

		versionsBucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketVersions))
		if err != nil {
			return err
		}
		if err := versionsBucket.Put([]byte("versions"), []byte(`["1.0.0","2.0.0"]`)); err != nil {
			return err
		}

		depsBucket, err := tx.CreateBucketIfNotExists([]byte(helpers.StoreBucketDepsCache))
		if err != nil {
			return err
		}
		return depsBucket.Put([]byte("deps"), []byte(`{"a.b":">=1.0.0"}`))
	})
	if err != nil {
		t.Fatalf("failed to seed a v3-shape bolt file: %v", err)
	}

	loaded, err := Load(dbs)
	if err != nil {
		t.Fatalf("expected nil error dropping a v3 snapshot, got %v", err)
	}
	if !reflect.DeepEqual(loaded, New()) {
		t.Fatalf("expected a fresh store after dropping a v3 snapshot, got %#v", loaded)
	}
}

// TestLoadToleratesLegacyRootsBucket proves Load tolerates a leftover "roots"
// Bolt bucket from before the field was removed from Store: a current-schema
// snapshot with real installed data, plus a legacy roots bucket planted
// directly (bypassing Save, which never writes one), must still load
// successfully with the installed entry intact, and a subsequent Save must
// still succeed. This deliberately does not assert that the roots bucket
// still exists after Load/Save: whether a future sweep drops that leftover
// bucket is an incidental detail this test is not pinning down, only that its
// mere presence does not break loading or saving.
func TestLoadToleratesLegacyRootsBucket(t *testing.T) {
	t.Parallel()
	dbs := openTestDBs(t)

	st := New()
	st.SetInstalled("a.b@1.0.0", InstalledEntry{InstallPath: "/tmp/a/b", ArtifactSHA256: testArtifactSHA})
	mustSave(t, dbs, st)

	err := dbs.db.Update(func(tx *bolt.Tx) error {
		// "roots" is the literal legacy bucket name, spelled out here
		// because Store carries no roots field and therefore no named
		// constant for it.
		rootsBucket, err := tx.CreateBucketIfNotExists([]byte("roots"))
		if err != nil {
			return err
		}
		return rootsBucket.Put([]byte("last_run"), []byte(`["a.b@1.0.0"]`))
	})
	if err != nil {
		t.Fatalf("failed to plant a legacy roots bucket: %v", err)
	}

	loaded := mustLoad(t, dbs)
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != testArtifactSHA {
		t.Fatalf("unexpected installed entry: %#v (ok=%v)", installed, ok)
	}

	if err := Save(dbs, loaded); err != nil {
		t.Fatalf("expected Save to succeed with a legacy roots bucket present, got %v", err)
	}
}

// TestSetWarmedIgnoresEmptyKeyOrSHA proves SetWarmed can never persist an
// entry that would protect nothing: an empty key or an empty artifact sha is
// silently ignored rather than stored, since a key or sha string that no
// caller can ever look up again would just be dead weight in the snapshot.
func TestSetWarmedIgnoresEmptyKeyOrSHA(t *testing.T) {
	t.Parallel()
	st := New()

	st.SetWarmed("", "sha-1")
	st.SetWarmed("a.b@1.0.0", "")

	if got := st.WarmedArtifactSHAByKey(); len(got) != 0 {
		t.Fatalf("expected no warmed entries after empty-key/empty-sha calls, got %#v", got)
	}
}

// TestWarmedArtifactSHAByKeyExcludesStaleEntry proves WarmedArtifactSHAByKey
// applies the same WarmedEntryMaxAge retention window SetWarmed's persisted
// data is later pruned against, rather than returning every entry ever
// written regardless of age.
func TestWarmedArtifactSHAByKeyExcludesStaleEntry(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetWarmed("fresh.key@1.0.0", "sha-fresh")
	// Seed the stale entry directly: SetWarmed always stamps time.Now, so a
	// genuinely stale WarmedAt can only be produced by writing the map field.
	st.Warmed["stale.key@1.0.0"] = WarmedEntry{
		WarmedAt:       time.Now().UTC().Add(-40 * 24 * time.Hour),
		ArtifactSHA256: "sha-stale",
	}

	got := st.WarmedArtifactSHAByKey()
	if _, ok := got["stale.key@1.0.0"]; ok {
		t.Fatalf("expected the stale warmed entry to be excluded, got %#v", got)
	}
	if got["fresh.key@1.0.0"] != "sha-fresh" {
		t.Fatalf("expected the fresh warmed entry to survive, got %#v", got)
	}
}

// TestWarmedArtifactSHAByKeyExcludesFutureEntry proves WarmedArtifactSHAByKey
// treats a warmed entry stamped ahead of the current window's sampling
// instant as stale too, mirroring TestWarmedArtifactSHAByKeyExcludesStaleEntry
// for the opposite (future) edge: a keep-set built from a corrupt or
// clock-skewed future stamp must not protect its extracted tree forever.
func TestWarmedArtifactSHAByKeyExcludesFutureEntry(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetWarmed("fresh.key@1.0.0", "sha-fresh")
	// Seed the future entry directly: SetWarmed always stamps time.Now, so a
	// future WarmedAt can only be produced by writing the map field.
	st.Warmed["future.key@1.0.0"] = WarmedEntry{
		WarmedAt:       time.Now().UTC().Add(365 * 24 * time.Hour),
		ArtifactSHA256: "sha-future",
	}

	got := st.WarmedArtifactSHAByKey()
	if _, ok := got["future.key@1.0.0"]; ok {
		t.Fatalf("expected the future warmed entry to be excluded, got %#v", got)
	}
	if got["fresh.key@1.0.0"] != "sha-fresh" {
		t.Fatalf("expected the fresh warmed entry to survive, got %#v", got)
	}
}

// TestWarmedArtifactSHAByKeyReturnsIndependentMap proves the map
// WarmedArtifactSHAByKey returns is a fresh copy: mutating it must never
// corrupt the store's own Warmed state, mirroring the same guarantee
// InstalledArtifactSHAByKey already provides.
func TestWarmedArtifactSHAByKeyReturnsIndependentMap(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetWarmed("a.b@1.0.0", "sha-1")

	got := st.WarmedArtifactSHAByKey()
	got["a.b@1.0.0"] = "tampered"
	got["c.d@2.0.0"] = "injected"

	fresh := st.WarmedArtifactSHAByKey()
	if fresh["a.b@1.0.0"] != "sha-1" {
		t.Fatalf("expected the store's own warmed entry to be unaffected by mutating a returned map, got %#v", fresh)
	}
	if _, ok := fresh["c.d@2.0.0"]; ok {
		t.Fatalf("expected an injected key in a returned map to never appear in the store, got %#v", fresh)
	}
}

// assertStoreMapsNonNil fails (via Error, not Fatal) for every one of
// Store's eight map fields that is still nil. It never stops early: the
// caller relies on every field being checked even if an earlier one already
// failed, since the point of the test calling this is to then go on and
// exercise the mutators regardless.
func assertStoreMapsNonNil(t *testing.T, st *Store) {
	t.Helper()
	fields := []struct {
		name  string
		isNil bool
	}{
		{"APICache", st.APICache == nil},
		{"DepsCache", st.DepsCache == nil},
		{"Installed", st.Installed == nil},
		{"Graph", st.Graph == nil},
		{"Requirements", st.Requirements == nil},
		{"Resolved", st.Resolved == nil},
		{"Versions", st.Versions == nil},
		{"GitPins", st.GitPins == nil},
		{"Warmed", st.Warmed == nil},
	}
	for _, f := range fields {
		if f.isNil {
			t.Errorf("expected %s to be non-nil after decode", f.name)
		}
	}
}

// TestUnmarshalJSONRestoresEveryNilMap proves that decoding a payload where
// every one of Store's eight map fields is an explicit JSON null still
// leaves every field writable afterward. It reuses populateTestStore - the
// exact same mutator calls (SetAPICache, SetDepsCache, SetInstalled,
// SetGraph, SetVersionsCache, SetWarmed, plus the two wholesale replacers
// SetRequirements and SetResolvedAll) TestSaveLoadRoundTrip exercises against
// a freshly constructed Store - against the just-decoded one instead, then
// reads every value back through the matching assert* helpers. Without
// Store.ensureMaps running after the decode, populateTestStore's first
// map-indexing call panics with "assignment to entry in nil map".
func TestUnmarshalJSONRestoresEveryNilMap(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	payload := fmt.Sprintf(`{
		"meta": {"schema_version": %d},
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

	st := New()
	if err := json.Unmarshal([]byte(payload), st); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	assertStoreMapsNonNil(t, st)

	populateTestStore(st, fixed)

	assertAPICache(t, st)
	assertDepsCache(t, st)
	assertInstalled(t, st)
	assertGraph(t, st)
	assertRequirements(t, st)
	assertResolved(t, st)
	assertVersions(t, st)
	assertWarmed(t, st)
	assertGitPin(t, st)
}

// TestUnmarshalJSONKeepsDecodedData proves ensureMaps only fills in a field a
// decode actually nilled: a bucket with real decoded entries must survive
// untouched, and Meta must come from the payload rather than being reset to
// New()'s defaults, while the one nulled bucket (warmed) ends up non-nil and
// empty rather than losing the rest of the payload. The payload mirrors
// populateTestStore's fixture values exactly, so this reuses the same
// assertMeta/assertAPICache/... helpers TestSaveLoadRoundTrip uses to verify
// a Bolt round trip, here verifying a JSON decode instead.
func TestUnmarshalJSONKeepsDecodedData(t *testing.T) {
	t.Parallel()
	payload := fmt.Sprintf(`{
		"meta": {
			"schema_version": %d,
			"requirements_hash": "req-hash",
			"server": "https://example.com",
			"last_snapshot": "2024-01-02T03:04:05Z"
		},
		"api_cache": {"api": {"url": "https://example.com/api", "etag": "etag", "body": "eyJvayI6dHJ1ZX0="}},
		"deps_cache": {"deps": {"fetched_at": "2024-01-02T03:04:05Z", "deps": {"a.b": ">=1.0.0"}}},
		"installed": {"a.b@1.0.0": {"artifact_sha256": "abc"}},
		"graph": {"a.b@1.0.0": ["c.d@1.2.3"]},
		"requirements": {"a.b": {"constraint": "1.0.0"}},
		"resolved": {"a.b": {"version": "1.0.0"}},
		"versions_cache": {"versions": {"list": ["1.0.0", "2.0.0"]}},
		"warmed": null
	}`, helpers.StoreSnapshotSchemaVersion)

	st := New()
	if err := json.Unmarshal([]byte(payload), st); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	assertMeta(t, st)
	assertAPICache(t, st)
	assertDepsCache(t, st)
	assertInstalled(t, st)
	assertGraph(t, st)
	assertRequirements(t, st)
	assertResolved(t, st)
	assertVersions(t, st)

	if st.Warmed == nil {
		t.Fatal("expected Warmed to be non-nil after decoding an explicit null")
	}
	if len(st.Warmed) != 0 {
		t.Fatalf("expected Warmed to be empty, got %#v", st.Warmed)
	}
}

// TestUnmarshalJSONPropagatesDecodeError proves UnmarshalJSON does not
// swallow a genuine decode failure: ensureMaps must never mask bad input
// into a silently-empty, well-formed Store.
//
// The payload must be syntactically valid but type-invalid. encoding/json
// validates the whole document before it dispatches to an Unmarshaler, so a
// malformed payload is rejected without this method ever being entered, and
// the test would then pass no matter what the method did with the error.
func TestUnmarshalJSONPropagatesDecodeError(t *testing.T) {
	t.Parallel()
	st := New()
	err := json.Unmarshal([]byte(`{"installed": 42}`), st)
	if err == nil {
		t.Fatal("expected an error decoding a type-invalid installed bucket, got nil")
	}
}
