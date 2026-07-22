package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

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
	assertRoots(t, loaded)
	assertResolved(t, loaded)
	assertVersions(t, loaded)
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
	st := New()
	st.SetMetaRequirements("req-hash", "https://example.com")
	st.SetAPICache("api", APICacheEntry{
		URL:       "https://example.com/api",
		ETag:      "etag",
		FetchedAt: fixed,
		TTL:       time.Minute,
		Body:      []byte(`{"ok":true}`),
	})
	st.SetDepsCache("deps", map[string]string{"a.b": ">=1.0.0"})
	st.SetInstalled("a.b@1.0.0", InstalledEntry{
		InstallPath:    "/tmp/a/b",
		Source:         "https://example.com",
		ArtifactSHA256: "abc",
		InstalledAt:    fixed,
		Deps:           []string{"c.d@1.2.3"},
	})
	st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
	st.SetRequirements(map[string]RequirementSpec{
		"a.b": {Constraint: "1.0.0", Source: "https://example.com", Type: "galaxy"},
	})
	st.SetRoots("last_run", []string{"a.b@1.0.0"})
	st.SetResolvedAll(map[string]ResolvedEntry{
		"a.b": {Version: "1.0.0", Source: "https://example.com"},
	})
	st.SetVersionsCache("versions", []string{"1.0.0", "2.0.0"})
	return st
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
	if !ok || deps["a.b"] != ">=1.0.0" {
		t.Fatalf("unexpected deps cache: %#v", deps)
	}
}

func assertInstalled(t *testing.T, loaded *Store) {
	t.Helper()
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != "abc" {
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

func assertRoots(t *testing.T, loaded *Store) {
	t.Helper()
	roots := loaded.Roots["last_run"]
	if len(roots) != 1 || roots[0] != "a.b@1.0.0" {
		t.Fatalf("unexpected roots: %#v", roots)
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
	st.SetAPICache("api", APICacheEntry{URL: "v1"})
	st.SetInstalled("normal.key", InstalledEntry{Source: "v1"})
	mustSave(t, dbs, st)

	// Mutate api_cache (written before installed) and inject a key that
	// exceeds bolt.MaxKeySize into installed (written after api_cache),
	// forcing a mid-transaction failure in a non-first bucket.
	st.SetAPICache("api", APICacheEntry{URL: "v2"})
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
