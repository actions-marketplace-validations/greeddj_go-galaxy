package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
)

// SnapshotMeta holds metadata about the cached snapshot.
type SnapshotMeta struct {
	LastSnapshot     time.Time `json:"last_snapshot"`
	RequirementsHash string    `json:"requirements_hash"`
	Server           string    `json:"server"`
	SchemaVersion    int       `json:"schema_version"`
}

// APICacheEntry stores a cached API response and validation data.
type APICacheEntry struct {
	FetchedAt    time.Time     `json:"fetched_at"`
	URL          string        `json:"url"`
	ETag         string        `json:"etag"`
	LastModified string        `json:"last_modified"`
	Body         []byte        `json:"body"`
	TTL          time.Duration `json:"ttl"`
}

// VersionsEntry stores a cached versions list with the time it was written,
// so age-based eviction can prune it from the persisted snapshot.
type VersionsEntry struct {
	FetchedAt time.Time `json:"fetched_at"`
	List      []string  `json:"list"`
}

// DepsCacheEntry stores cached dependency constraints with the time they were
// written, so age-based eviction can prune them from the persisted snapshot.
type DepsCacheEntry struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Deps      map[string]string `json:"deps"`
}

// InstalledEntry records an installed collection entry.
type InstalledEntry struct {
	InstallPath    string    `json:"install_path"`
	Source         string    `json:"source"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
	InstalledAt    time.Time `json:"installed_at"`
	Deps           []string  `json:"deps"`
}

// Store holds cached state for collections and metadata.
type Store struct {
	APICache     map[string]APICacheEntry   `json:"api_cache"`
	DepsCache    map[string]DepsCacheEntry  `json:"deps_cache"`
	Installed    map[string]InstalledEntry  `json:"installed"`
	Graph        map[string][]string        `json:"graph"`
	Requirements map[string]RequirementSpec `json:"requirements"`
	Roots        map[string][]string        `json:"roots"`
	Resolved     map[string]ResolvedEntry   `json:"resolved"`
	Versions     map[string]VersionsEntry   `json:"versions_cache"`
	Meta         SnapshotMeta               `json:"meta"`
	mu           sync.RWMutex               `json:"-"`
}

// New creates an initialized Store with empty maps.
func New() *Store {
	return &Store{
		Meta: SnapshotMeta{
			SchemaVersion: helpers.StoreSnapshotSchemaVersion,
		},
		APICache:     make(map[string]APICacheEntry),
		DepsCache:    make(map[string]DepsCacheEntry),
		Installed:    make(map[string]InstalledEntry),
		Graph:        make(map[string][]string),
		Requirements: make(map[string]RequirementSpec),
		Roots:        make(map[string][]string),
		Resolved:     make(map[string]ResolvedEntry),
		Versions:     make(map[string]VersionsEntry),
	}
}

// ResolvedEntry stores a resolved collection version and source.
type ResolvedEntry struct {
	Version string `json:"version"`
	Source  string `json:"source"`
}

// RequirementSpec captures a requirement constraint and metadata.
type RequirementSpec struct {
	Constraint string   `json:"constraint"`
	Source     string   `json:"source"`
	Type       string   `json:"type,omitempty"`
	Signatures []string `json:"signatures,omitempty"`
}

// SetInstalled records an installed collection entry. The entry's Deps
// slice is cloned before storing, so a later caller mutation of its
// backing array cannot corrupt the stored snapshot state.
func (m *Store) SetInstalled(key string, entry InstalledEntry) {
	if m == nil {
		return
	}
	entry.Deps = slices.Clone(entry.Deps)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Installed[key] = entry
}

// DeleteInstalled removes an installed entry by key.
func (m *Store) DeleteInstalled(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Installed, key)
}

// GetInstalled returns an installed entry by key. Deps is cloned before
// returning, so a caller mutation of the returned slice cannot corrupt the
// stored snapshot state.
func (m *Store) GetInstalled(key string) (InstalledEntry, bool) {
	if m == nil {
		return InstalledEntry{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.Installed[key]
	entry.Deps = slices.Clone(entry.Deps)
	return entry, ok
}

// InstalledArtifactSHAByKey returns a fresh map from installed collection key
// (ns.name@version) to its ArtifactSHA256, omitting entries with an empty
// SHA. This is the persisted source of truth for which extracted artifact
// trees must be kept: unlike an on-disk workspace scan, it also covers
// projects whose workspace is currently absent (the normal ephemeral-CI
// state), since their installed entries are never pruned from the snapshot
// until their workspace is actually seen and scanned again.
func (m *Store) InstalledArtifactSHAByKey() map[string]string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.Installed))
	for key, entry := range m.Installed {
		if entry.ArtifactSHA256 == "" {
			continue
		}
		out[key] = entry.ArtifactSHA256
	}
	return out
}

// GetDepsCache returns cached dependency constraints for a key. This is a
// pure read under RLock: it does not bump the entry's FetchedAt, so a hit
// here never requires upgrading to the write lock.
func (m *Store) GetDepsCache(key string) (map[string]string, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.DepsCache[key]
	if !ok {
		return nil, false
	}
	clone := make(map[string]string, len(entry.Deps))
	maps.Copy(clone, entry.Deps)
	return clone, true
}

// SetDepsCache stores dependency constraints for a key, stamping the current
// time as the entry's FetchedAt. The stamp marks when the entry was written,
// not when it was last read: a key that stays referenced but is never
// rewritten still ages out of the persisted snapshot CacheEntryMaxAge after
// this write and gets refetched on a later run - an accepted, cheap
// consequence, and the reason GetDepsCache above stays a pure read instead
// of taking the write lock to bump FetchedAt on every hit. This differs from
// APICache, whose FetchedAt is also bumped on a successful 304 revalidation
// via refreshAPICacheEntry, not only on an initial fetch.
func (m *Store) SetDepsCache(key string, deps map[string]string) {
	if m == nil {
		return
	}
	clone := make(map[string]string, len(deps))
	maps.Copy(clone, deps)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DepsCache[key] = DepsCacheEntry{FetchedAt: time.Now().UTC(), Deps: clone}
}

// DeleteDepsCache removes cached dependency data for a key.
func (m *Store) DeleteDepsCache(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.DepsCache, key)
}

// GetAPICache returns a cached API entry by key. The returned entry shares
// its Body backing array with the stored snapshot: callers must treat Body
// as read-only and must not mutate it. Body is deliberately not cloned on
// read because this is the warm-cache hot path and Body can be a large
// response payload; the only caller unmarshals it without mutating.
func (m *Store) GetAPICache(key string) (APICacheEntry, bool) {
	if m == nil {
		return APICacheEntry{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.APICache[key]
	return entry, ok
}

// SetAPICache stores a cached API entry. The entry's Body is cloned before
// storing, so a later caller mutation (or reuse) of its backing buffer
// cannot corrupt the stored snapshot state.
func (m *Store) SetAPICache(key string, entry APICacheEntry) {
	if m == nil {
		return
	}
	entry.Body = slices.Clone(entry.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.APICache[key] = entry
}

// ClearCaches clears API, dependency, and versions caches.
func (m *Store) ClearCaches() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.APICache = make(map[string]APICacheEntry)
	m.DepsCache = make(map[string]DepsCacheEntry)
	m.Versions = make(map[string]VersionsEntry)
}

// GetVersionsCache returns cached versions for a key. This is a pure read
// under RLock: it does not bump the entry's FetchedAt, so a hit here never
// requires upgrading to the write lock.
func (m *Store) GetVersionsCache(key string) ([]string, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.Versions[key]
	if !ok {
		return nil, false
	}
	clone := make([]string, len(entry.List))
	copy(clone, entry.List)
	return clone, true
}

// SetVersionsCache stores cached versions for a key, stamping the current
// time as the entry's FetchedAt. The stamp marks when the entry was written,
// not when it was last read: a key that stays referenced but is never
// rewritten still ages out of the persisted snapshot CacheEntryMaxAge after
// this write and gets refetched on a later run - an accepted, cheap
// consequence, and the reason GetVersionsCache above stays a pure read
// instead of taking the write lock to bump FetchedAt on every hit. This
// differs from APICache, whose FetchedAt is also bumped on a successful 304
// revalidation via refreshAPICacheEntry, not only on an initial fetch.
func (m *Store) SetVersionsCache(key string, versions []string) {
	if m == nil {
		return
	}
	clone := make([]string, len(versions))
	copy(clone, versions)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Versions[key] = VersionsEntry{FetchedAt: time.Now().UTC(), List: clone}
}

// SetResolvedAll replaces the resolved entries map.
func (m *Store) SetResolvedAll(resolved map[string]ResolvedEntry) {
	if m == nil {
		return
	}
	clone := make(map[string]ResolvedEntry, len(resolved))
	maps.Copy(clone, resolved)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Resolved = clone
}

// ResolvedSnapshot returns a copy of resolved entries.
func (m *Store) ResolvedSnapshot() map[string]ResolvedEntry {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	clone := make(map[string]ResolvedEntry, len(m.Resolved))
	maps.Copy(clone, m.Resolved)
	return clone
}

// SetGraph records dependencies for a collection key. deps is cloned before
// storing, so a later caller mutation of its backing array cannot corrupt
// the stored snapshot state.
func (m *Store) SetGraph(key string, deps []string) {
	if m == nil {
		return
	}
	clone := slices.Clone(deps)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Graph[key] = clone
}

// DeleteGraph removes dependency data for a key.
func (m *Store) DeleteGraph(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Graph, key)
}

// SetGraphSnapshot replaces the dependency graph.
func (m *Store) SetGraphSnapshot(graph map[string][]string) {
	if m == nil {
		return
	}
	clone := make(map[string][]string, len(graph))
	for key, deps := range graph {
		out := make([]string, len(deps))
		copy(out, deps)
		clone[key] = out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Graph = clone
}

// GraphSnapshot returns a copy of the dependency graph.
func (m *Store) GraphSnapshot() map[string][]string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	clone := make(map[string][]string, len(m.Graph))
	for key, deps := range m.Graph {
		out := make([]string, len(deps))
		copy(out, deps)
		clone[key] = out
	}
	return clone
}

// SetRequirements stores a snapshot of requirement specs. maps.Copy alone
// would only shallow-copy each RequirementSpec, leaving its Signatures
// slice aliasing the caller's backing array, so each entry's Signatures is
// cloned individually before storing.
func (m *Store) SetRequirements(spec map[string]RequirementSpec) {
	if m == nil {
		return
	}
	clone := make(map[string]RequirementSpec, len(spec))
	for key, value := range spec {
		value.Signatures = slices.Clone(value.Signatures)
		clone[key] = value
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requirements = clone
}

// RequirementsSnapshot returns a fully independent deep copy of requirement
// specs: each entry's Signatures slice is cloned too, so mutating the
// returned map or any of its Signatures slices cannot corrupt the stored
// snapshot state.
func (m *Store) RequirementsSnapshot() map[string]RequirementSpec {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	clone := make(map[string]RequirementSpec, len(m.Requirements))
	for key, value := range m.Requirements {
		value.Signatures = slices.Clone(value.Signatures)
		clone[key] = value
	}
	return clone
}

// SetRoots stores root collection keys under a label. roots is cloned
// before storing, so a later caller mutation of its backing array cannot
// corrupt the stored snapshot state.
func (m *Store) SetRoots(key string, roots []string) {
	if m == nil {
		return
	}
	clone := slices.Clone(roots)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Roots[key] = clone
}

// MetaSnapshot returns the current snapshot metadata.
func (m *Store) MetaSnapshot() SnapshotMeta {
	if m == nil {
		return SnapshotMeta{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Meta
}

// SetMetaRequirements stores the requirements hash and server.
func (m *Store) SetMetaRequirements(hash, server string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Meta.RequirementsHash = hash
	m.Meta.Server = server
}

// snapshotData is a serialized view of Store contents.
type snapshotData struct {
	APICache     map[string]APICacheEntry
	DepsCache    map[string]DepsCacheEntry
	Installed    map[string]InstalledEntry
	Graph        map[string][]string
	Requirements map[string]RequirementSpec
	Roots        map[string][]string
	Resolved     map[string]ResolvedEntry
	Versions     map[string]VersionsEntry
	Meta         SnapshotMeta
}

// MarshalSnapshot returns a schema-stamped JSON encoding of the store,
// suitable for writing to a remote snapshot backend. It builds the payload
// from snapshotData's RLock-protected deep copy rather than marshaling the
// live store directly, so a concurrent writer goroutine cannot trip the
// race detector or produce a torn payload. The schema version and
// last-snapshot timestamp are stamped exactly as Save does.
func (m *Store) MarshalSnapshot() ([]byte, error) {
	data := m.snapshotData()
	data.Meta.SchemaVersion = helpers.StoreSnapshotSchemaVersion
	data.Meta.LastSnapshot = time.Now().UTC()

	// snapshotData has no json tags of its own; assign its fields onto a
	// throwaway Store so the encoding reuses Store's existing json tags and
	// the wire shape stays byte-identical to marshaling a *Store directly.
	snapshot := &Store{
		APICache:     data.APICache,
		DepsCache:    data.DepsCache,
		Installed:    data.Installed,
		Graph:        data.Graph,
		Requirements: data.Requirements,
		Roots:        data.Roots,
		Resolved:     data.Resolved,
		Versions:     data.Versions,
		Meta:         data.Meta,
	}
	return json.Marshal(snapshot)
}

// isStaleCacheEntry reports whether fetchedAt is older than cutoff. It uses
// strict Before so an entry written exactly at the cutoff instant is
// retained rather than pruned, and is factored out (rather than inlined at
// each of the three call sites below) so the exact-equality boundary
// behavior can be exercised directly by a test without racing the wall
// clock.
func isStaleCacheEntry(fetchedAt, cutoff time.Time) bool {
	return fetchedAt.Before(cutoff)
}

// snapshotData builds a snapshot payload from the store. Entries in
// APICache, DepsCache, and Versions that were last written before the
// CacheEntryMaxAge retention window are dropped from the payload: both
// persist entrypoints (local Bolt Save and the S3 MarshalSnapshot) build
// their payload through this method, so the age bound applies to both
// without either caller having to know about it. The live maps themselves
// are never pruned, only this RLock-protected copy - a still-warm entry
// stays available for reads until it is naturally overwritten or the store
// process restarts and reloads the (now-pruned) persisted snapshot.
func (m *Store) snapshotData() snapshotData {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cutoff := time.Now().UTC().Add(-helpers.CacheEntryMaxAge)

	data := snapshotData{
		Meta:         m.Meta,
		APICache:     make(map[string]APICacheEntry, len(m.APICache)),
		DepsCache:    make(map[string]DepsCacheEntry, len(m.DepsCache)),
		Installed:    make(map[string]InstalledEntry, len(m.Installed)),
		Graph:        make(map[string][]string, len(m.Graph)),
		Requirements: make(map[string]RequirementSpec, len(m.Requirements)),
		Roots:        make(map[string][]string, len(m.Roots)),
		Resolved:     make(map[string]ResolvedEntry, len(m.Resolved)),
		Versions:     make(map[string]VersionsEntry, len(m.Versions)),
	}

	for key, entry := range m.APICache {
		if isStaleCacheEntry(entry.FetchedAt, cutoff) {
			continue
		}
		data.APICache[key] = entry
	}
	for key, entry := range m.DepsCache {
		if isStaleCacheEntry(entry.FetchedAt, cutoff) {
			continue
		}
		clone := make(map[string]string, len(entry.Deps))
		maps.Copy(clone, entry.Deps)
		data.DepsCache[key] = DepsCacheEntry{FetchedAt: entry.FetchedAt, Deps: clone}
	}
	maps.Copy(data.Installed, m.Installed)
	for key, deps := range m.Graph {
		clone := make([]string, len(deps))
		copy(clone, deps)
		data.Graph[key] = clone
	}
	maps.Copy(data.Requirements, m.Requirements)
	for key, roots := range m.Roots {
		clone := make([]string, len(roots))
		copy(clone, roots)
		data.Roots[key] = clone
	}
	maps.Copy(data.Resolved, m.Resolved)
	for key, entry := range m.Versions {
		if isStaleCacheEntry(entry.FetchedAt, cutoff) {
			continue
		}
		clone := make([]string, len(entry.List))
		copy(clone, entry.List)
		data.Versions[key] = VersionsEntry{FetchedAt: entry.FetchedAt, List: clone}
	}

	return data
}

// Load reads cached state from the consolidated Bolt database. A schema
// version older than the current one causes the snapshot to be dropped and
// rebuilt (nil error, a fresh empty Store) rather than partially trusted; a
// newer version is reported as an error since this binary cannot safely
// interpret it.
func Load(dbs *DBs) (*Store, error) {
	store := New()
	if dbs == nil || dbs.db == nil {
		return store, nil
	}

	if err := dbs.db.View(func(tx *bolt.Tx) error {
		return loadMeta(tx, store)
	}); err != nil {
		return nil, err
	}

	if err := ValidateSchema(store.Meta.SchemaVersion); err != nil {
		if errors.Is(err, helpers.ErrOutdatedSchemaVersion) {
			return New(), nil
		}
		return nil, err
	}

	if err := dbs.db.View(func(tx *bolt.Tx) error {
		return runLoadSteps(tx, store)
	}); err != nil {
		return nil, err
	}
	return store, nil
}

// Save writes cached state to the consolidated Bolt database. The meta
// bucket and all eight data buckets are written inside a single Bolt
// transaction so a mid-save failure (e.g. a key or value exceeding Bolt's
// limits) leaves the previously committed snapshot fully intact instead of
// a partially overwritten mix of old and new data.
func Save(dbs *DBs, store *Store) error {
	if dbs == nil || dbs.db == nil {
		return helpers.ErrDbNil
	}
	if store == nil {
		return helpers.ErrStoreNil
	}

	data := store.snapshotData()
	data.Meta.SchemaVersion = helpers.StoreSnapshotSchemaVersion
	data.Meta.LastSnapshot = time.Now().UTC()

	return dbs.db.Update(func(tx *bolt.Tx) error {
		if err := saveMeta(tx, data.Meta); err != nil {
			return err
		}
		return runSaveSteps(tx, data)
	})
}

// ValidateSchema reports whether a stored snapshot schema version is
// compatible with this binary. A newer version means this build is too old
// to safely interpret the snapshot; an older version means the snapshot
// predates a breaking change and must be dropped and rebuilt rather than
// partially trusted.
func ValidateSchema(version int) error {
	switch {
	case version == helpers.StoreSnapshotSchemaVersion:
		return nil
	case version > helpers.StoreSnapshotSchemaVersion:
		return fmt.Errorf("%w: %d", helpers.ErrUnsupportedSchemaVersion, version)
	default:
		return fmt.Errorf("%w: %d", helpers.ErrOutdatedSchemaVersion, version)
	}
}

// runLoadSteps reads the eight data buckets in the given transaction.
func runLoadSteps(tx *bolt.Tx, store *Store) error {
	steps := []func() error{
		func() error { return loadAPICache(tx, store) },
		func() error { return loadInstalled(tx, store) },
		func() error { return loadDepsCache(tx, store) },
		func() error { return loadGraph(tx, store) },
		func() error { return loadRequirements(tx, store) },
		func() error { return loadRoots(tx, store) },
		func() error { return loadResolved(tx, store) },
		func() error { return loadVersions(tx, store) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// runSaveSteps writes the eight data buckets in the given transaction, in a
// fixed order (api_cache, deps_cache, installed, graph, requirements, roots,
// resolved, versions_cache) that callers rely on for fault injection tests.
func runSaveSteps(tx *bolt.Tx, data snapshotData) error {
	steps := []func() error{
		func() error { return saveAPICache(tx, data) },
		func() error { return saveDepsCache(tx, data) },
		func() error { return saveInstalled(tx, data) },
		func() error { return saveGraph(tx, data) },
		func() error { return saveRequirements(tx, data) },
		func() error { return saveRoots(tx, data) },
		func() error { return saveResolved(tx, data) },
		func() error { return saveVersions(tx, data) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// loadMeta reads the meta bucket into store.Meta. When the meta bucket does
// not exist at all (a brand-new file), the schema version keeps the current
// default set by New(). When the bucket exists but the schema key is
// missing, the version is treated as 0 (outdated) rather than silently
// keeping the current-version default, so a populated-but-unstamped
// database is dropped and rebuilt instead of passing validation.
func loadMeta(tx *bolt.Tx, store *Store) error {
	metaBucket := tx.Bucket([]byte(helpers.StoreBucketMeta))
	if metaBucket == nil {
		return nil
	}
	if v := metaBucket.Get([]byte(helpers.StoreMetaSchemaVersion)); v != nil {
		version, err := strconv.Atoi(string(v))
		if err != nil {
			return fmt.Errorf("invalid schema version: %w", err)
		}
		store.Meta.SchemaVersion = version
	} else {
		store.Meta.SchemaVersion = 0
	}
	if v := metaBucket.Get([]byte(helpers.StoreMetaLastSnapshot)); v != nil {
		t, err := time.Parse(time.RFC3339Nano, string(v))
		if err != nil {
			return fmt.Errorf("invalid snapshot time: %w", err)
		}
		store.Meta.LastSnapshot = t
	}
	if v := metaBucket.Get([]byte(helpers.StoreMetaRequirementsHash)); v != nil {
		store.Meta.RequirementsHash = string(v)
	}
	if v := metaBucket.Get([]byte(helpers.StoreMetaServer)); v != nil {
		store.Meta.Server = string(v)
	}
	return nil
}

func loadAPICache(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketAPICache, func(k, v []byte) error {
		var entry APICacheEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.APICache[string(k)] = entry
		return nil
	})
}

func loadInstalled(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketInstalled, func(k, v []byte) error {
		var entry InstalledEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.Installed[string(k)] = entry
		return nil
	})
}

func loadDepsCache(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketDepsCache, func(k, v []byte) error {
		var entry DepsCacheEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.DepsCache[string(k)] = entry
		return nil
	})
}

func loadGraph(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketGraph, func(k, v []byte) error {
		var deps []string
		if err := json.Unmarshal(v, &deps); err != nil {
			return err
		}
		store.Graph[string(k)] = deps
		return nil
	})
}

func loadRequirements(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketRequirements, func(k, v []byte) error {
		var spec RequirementSpec
		if err := json.Unmarshal(v, &spec); err != nil {
			return err
		}
		store.Requirements[string(k)] = spec
		return nil
	})
}

func loadRoots(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketRoots, func(k, v []byte) error {
		var roots []string
		if err := json.Unmarshal(v, &roots); err != nil {
			return err
		}
		store.Roots[string(k)] = roots
		return nil
	})
}

// loadResolved decodes the resolved bucket. Every current-schema value is
// written by saveResolved as a JSON-encoded ResolvedEntry, so an unmarshal
// failure means the value is genuinely corrupt: it is reported as an error
// rather than silently coerced into a garbage version string.
func loadResolved(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketResolved, func(k, v []byte) error {
		var entry ResolvedEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return fmt.Errorf("invalid resolved entry %q: %w", string(k), err)
		}
		store.Resolved[string(k)] = entry
		return nil
	})
}

func loadVersions(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketVersions, func(k, v []byte) error {
		var entry VersionsEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.Versions[string(k)] = entry
		return nil
	})
}

func saveMeta(tx *bolt.Tx, meta SnapshotMeta) error {
	metaBucket, err := ensureEmptyBucket(tx, helpers.StoreBucketMeta)
	if err != nil {
		return err
	}
	if err := metaBucket.Put([]byte(helpers.StoreMetaSchemaVersion), []byte(strconv.Itoa(meta.SchemaVersion))); err != nil {
		return err
	}
	if err := metaBucket.Put([]byte(helpers.StoreMetaLastSnapshot), []byte(meta.LastSnapshot.Format(time.RFC3339Nano))); err != nil {
		return err
	}
	if meta.RequirementsHash != "" {
		if err := metaBucket.Put([]byte(helpers.StoreMetaRequirementsHash), []byte(meta.RequirementsHash)); err != nil {
			return err
		}
	}
	if meta.Server != "" {
		if err := metaBucket.Put([]byte(helpers.StoreMetaServer), []byte(meta.Server)); err != nil {
			return err
		}
	}
	return nil
}

func saveAPICache(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketAPICache, data.APICache, func(entry APICacheEntry) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveDepsCache(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketDepsCache, data.DepsCache, func(entry DepsCacheEntry) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveInstalled(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketInstalled, data.Installed, func(entry InstalledEntry) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveGraph(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketGraph, data.Graph, func(entry []string) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveRequirements(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketRequirements, data.Requirements, func(entry RequirementSpec) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveRoots(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketRoots, data.Roots, func(entry []string) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveResolved(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketResolved, data.Resolved, func(entry ResolvedEntry) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

func saveVersions(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketVersions, data.Versions, func(entry VersionsEntry) ([]byte, error) {
		return json.Marshal(&entry)
	})
}

// ensureEmptyBucket recreates a bucket to ensure it is empty.
func ensureEmptyBucket(tx *bolt.Tx, name string) (*bolt.Bucket, error) {
	bucket := tx.Bucket([]byte(name))
	if bucket != nil {
		if err := tx.DeleteBucket([]byte(name)); err != nil {
			return nil, err
		}
	}
	return tx.CreateBucket([]byte(name))
}

// loadBucket iterates over a bucket and calls fn for each entry.
func loadBucket(tx *bolt.Tx, name string, fn func(k, v []byte) error) error {
	bucket := tx.Bucket([]byte(name))
	if bucket == nil {
		return nil
	}
	return bucket.ForEach(fn)
}

// saveBucket writes data to a bucket using the encode callback, within the
// caller's transaction.
func saveBucket[T any](tx *bolt.Tx, name string, data map[string]T, encode func(T) ([]byte, error)) error {
	bucket, err := ensureEmptyBucket(tx, name)
	if err != nil {
		return err
	}
	for key, entry := range data {
		encoded, err := encode(entry)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(key), encoded); err != nil {
			return err
		}
	}
	return nil
}
