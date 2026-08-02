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
//
// LastSnapshot and ContentRecorded answer two different questions and must
// not be conflated. LastSnapshot is stamped by every persisted write and says
// only that a snapshot exists. ContentRecorded is stamped only by a write
// that carried records of what is on disk, and is what a destructive pass has
// to consult - see HasRecordedContent.
type SnapshotMeta struct {
	LastSnapshot     time.Time `json:"last_snapshot"`
	ContentRecorded  time.Time `json:"content_recorded"`
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

// WarmedEntry records that the warm command materialized a collection's
// artifact into the content-addressable extracted store. Unlike
// InstalledEntry, it has no install path and no project behind it: a
// warm-only machine's project is recorded but its workspace never exists, so
// this entry - and the age it was written - is the only signal cleanup has
// that the extracted tree it names is still wanted (see
// Store.WarmedArtifactSHAByKey and helpers.WarmedEntryMaxAge).
//
// The key is the plain ns.name@version collection key, deliberately not
// scoped by server the way the deps cache and the artifact store are
// (helpers.ScopedDepsCacheKey, helpers.ArtifactKey). Those two are lookup
// keys, where a collision would serve one server's bytes in place of
// another's; this map is never looked up by key at all - extractedKeepSet
// discards the key and unions only the values - so the worst a collision can
// do is under-protect. Two servers publishing the same ns.name@version with
// different bytes resolve to different shas, the second warm overwrites the
// first's entry, and the first tree loses its keep-set protection. That
// self-heals with no network round trip: the artifact itself is server-scoped
// and is never reclaimed on a warm-only machine, so the next warm or install
// for that server re-extracts the tree from the still-cached tarball through
// extracted.Store.Ensure.
type WarmedEntry struct {
	WarmedAt       time.Time `json:"warmed_at"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
}

// Store holds cached state for collections and metadata.
type Store struct {
	APICache     map[string]APICacheEntry   `json:"api_cache"`
	DepsCache    map[string]DepsCacheEntry  `json:"deps_cache"`
	Installed    map[string]InstalledEntry  `json:"installed"`
	Graph        map[string][]string        `json:"graph"`
	Requirements map[string]RequirementSpec `json:"requirements"`
	Resolved     map[string]ResolvedEntry   `json:"resolved"`
	Versions     map[string]VersionsEntry   `json:"versions_cache"`
	Warmed       map[string]WarmedEntry     `json:"warmed"`
	Meta         SnapshotMeta               `json:"meta"`
	mu           sync.RWMutex               `json:"-"`
}

// New creates an initialized Store with empty maps. UnmarshalJSON is the
// method that restores this same all-maps-non-nil invariant after a decode,
// since a decode can nil a map in a way this constructor never does.
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
		Resolved:     make(map[string]ResolvedEntry),
		Versions:     make(map[string]VersionsEntry),
		Warmed:       make(map[string]WarmedEntry),
	}
}

// UnmarshalJSON decodes a Store, then re-allocates any map field an explicit
// JSON null nilled. An explicit `null` in the payload differs from both an
// absent key and `{}`: only `null` nils a pre-initialized map field during
// decode, while an absent key or `{}` leaves it alone (or empty). Without
// this guard, the next write into that map panics with "assignment to entry
// in nil map"; nothing in this program calls recover, and both the install
// and warm pipelines run their per-collection work inside wg.Go goroutines,
// so that panic kills the whole process before state.release runs. On the S3
// backend that means the exclusive lock object is never released and
// survives to its own 10-minute TTL, stalling every other run against that
// bucket for the whole window.
//
// The guard lives here, on the type, rather than being called explicitly at
// each decode site (e.g. LoadStore): every present and future decode path -
// including one nobody has written yet - inherits it automatically, and a
// ninth map added to Store later cannot reopen this hole just by a call site
// forgetting to guard it.
//
// The local Bolt path never needed this: loadBucket only ever populates an
// already-initialized map key by key and never assigns a whole map field, so
// there is no decode step there that can replace a map with nil.
func (m *Store) UnmarshalJSON(data []byte) error {
	// storeJSON strips the json.Unmarshaler method set, so the decode below
	// cannot recurse back into this method. The conversion is on the pointer,
	// so the RWMutex is never copied.
	type storeJSON Store

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := json.Unmarshal(data, (*storeJSON)(m)); err != nil {
		return err
	}
	m.ensureMaps()
	return nil
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

// SetWarmed records that key's artifact (identified by artifactSHA) has been
// materialized in the extracted store, stamping the current time. It returns
// early on a nil receiver, an empty key, or an empty sha, so it can never
// persist an entry that protects nothing. There is nothing to clone here,
// unlike SetInstalled: WarmedEntry holds no reference-type field.
func (m *Store) SetWarmed(key, artifactSHA string) {
	if m == nil || key == "" || artifactSHA == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Warmed[key] = WarmedEntry{WarmedAt: time.Now().UTC(), ArtifactSHA256: artifactSHA}
}

// WarmedArtifactSHAByKey returns a fresh map from warmed collection key to
// artifact sha, excluding entries with an empty sha and entries outside the
// WarmedEntryMaxAge retention window. The window is enforced here, not just
// at persist time: cleanup builds its keep set from this call and then saves
// the snapshot in the same run, and the save prunes on the same window (see
// snapshotData/copyFreshWarmed) - filtering here is what keeps the keep set
// and the snapshot it is about to write in agreement, rather than leaving
// every expiry lagging one whole cleanup cycle. The shared thing is
// helpers.WarmedEntryMaxAge, passed to newRetentionWindow at both sites; each
// site samples its own now, so the two windows share a width but not an
// instant.
func (m *Store) WarmedArtifactSHAByKey() map[string]string {
	if m == nil {
		return nil
	}
	window := newRetentionWindow(time.Now().UTC(), helpers.WarmedEntryMaxAge)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.Warmed))
	for key, entry := range m.Warmed {
		if entry.ArtifactSHA256 == "" || window.isStale(entry.WarmedAt) {
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

// MetaSnapshot returns the current snapshot metadata.
func (m *Store) MetaSnapshot() SnapshotMeta {
	if m == nil {
		return SnapshotMeta{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Meta
}

// WasPersisted reports whether this store was ever actually loaded from a
// persisted snapshot, as opposed to being a fresh, never-saved store. It is
// equivalent to !m.Meta.LastSnapshot.IsZero(): LastSnapshot is zero exactly
// when no persisted snapshot was loaded - New() leaves it zero, Load returns
// New() outright for ErrOutdatedSchemaVersion (a dropped, pre-migration
// snapshot), the S3 backend's LoadStore returns store.New() for both
// errS3NotFound and a failed outdated-schema probe, and an absent meta
// bucket leaves LastSnapshot zero on an otherwise normally-loaded store -
// loadMeta returns early on a missing bucket, so the data buckets still
// load as usual; that combination only a hand-edited database can produce,
// and skipping is the conservative answer there anyway - while Save and
// MarshalSnapshot both stamp it unconditionally on every persisted write.
// It reads m.Meta.LastSnapshot directly under the read lock rather than
// through MetaSnapshot, which would copy the whole SnapshotMeta struct just
// to check one field.
//
// Callers need this because a store that was never persisted carries no
// evidence about what is actually in use anywhere: any pass that deletes
// content or narrows state on the strength of such a store is acting on
// ignorance, not on evidence, and must not treat an empty in-memory map as
// proof that nothing is installed or warmed.
func (m *Store) WasPersisted() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.Meta.LastSnapshot.IsZero()
}

// HasRecordedContent reports whether any run has ever written records of
// on-disk content into this cache - an installed entry or a warmed one. It is
// the predicate a pass that deletes on-disk content must consult, and it is
// deliberately not WasPersisted.
//
// The two differ on exactly one shape, and that shape is reachable: `lock` is
// a real command that writes neither installed nor warmed entries, so on a
// cache carrying no persisted snapshot it produces one whose two content sets
// are empty and whose LastSnapshot is stamped. WasPersisted reads that as
// evidence, and the emptiness then reads as "nothing is installed or warmed
// anywhere" - which is how a plain `lock` run on a machine whose snapshot was
// dropped by a schema bump, but whose extracted trees survived on disk, hands
// the next cleanup the evidence to wipe the whole content-addressable store.
//
// The stamp is derived from the data rather than passed in by each command,
// so no command has to remember to set it and none can set it wrongly: a save
// stamps ContentRecorded when the store holds at least one installed or
// warmed entry, and otherwise carries forward whatever was already recorded.
// Carrying forward is what keeps the predicate true for a cache whose last
// collection was legitimately cleaned up - the snapshot genuinely knows
// nothing is installed there, which is evidence, not ignorance - while
// leaving it false for a cache no content-recording command has ever
// written.
func (m *Store) HasRecordedContent() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.Meta.ContentRecorded.IsZero()
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

// stampSaveMeta applies the metadata every persisted write stamps, and is the
// one place either backend's save decides what the snapshot claims about
// itself: the local Bolt Save and the S3 MarshalSnapshot both go through it,
// so the two cannot drift on what a snapshot asserts.
//
// SchemaVersion and LastSnapshot are unconditional - the snapshot was written,
// by this binary, now. ContentRecorded is not: it is stamped only when the
// data being written actually holds records of on-disk content, and otherwise
// carries forward whatever the loaded snapshot already had. See
// Store.HasRecordedContent for why that distinction exists and which pass
// depends on it.
//
// hasContent is read from the live store BEFORE snapshotData's age eviction,
// not from the payload after it, and the difference is load-bearing rather
// than incidental: a cache whose only content record ages out on this very
// save would otherwise have that record dropped and the evidence it ever
// existed erased in the same write, leaving the extracted tree it named
// unreclaimable forever. Recording it was a fact about the past; the eviction
// does not unmake it.
func stampSaveMeta(data *snapshotData, hasContent bool) {
	data.Meta.SchemaVersion = helpers.StoreSnapshotSchemaVersion
	data.Meta.LastSnapshot = time.Now().UTC()
	if hasContent {
		data.Meta.ContentRecorded = data.Meta.LastSnapshot
	}
}

// snapshotData is a serialized view of Store contents.
type snapshotData struct {
	APICache     map[string]APICacheEntry
	DepsCache    map[string]DepsCacheEntry
	Installed    map[string]InstalledEntry
	Graph        map[string][]string
	Requirements map[string]RequirementSpec
	Resolved     map[string]ResolvedEntry
	Versions     map[string]VersionsEntry
	Warmed       map[string]WarmedEntry
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
	stampSaveMeta(&data, m.hasContentEntries())

	// snapshotData has no json tags of its own; assign its fields onto a
	// throwaway Store so the encoding reuses Store's existing json tags and
	// the wire shape stays byte-identical to marshaling a *Store directly.
	snapshot := &Store{
		APICache:     data.APICache,
		DepsCache:    data.DepsCache,
		Installed:    data.Installed,
		Graph:        data.Graph,
		Requirements: data.Requirements,
		Resolved:     data.Resolved,
		Versions:     data.Versions,
		Warmed:       data.Warmed,
		Meta:         data.Meta,
	}
	return json.Marshal(snapshot)
}

// hasContentEntries reports whether the live store currently holds any record
// of on-disk content. It is what a save consults to decide whether to stamp
// Meta.ContentRecorded; see stampSaveMeta for why it is read here, from the
// store itself, rather than from the payload that save is about to write.
func (m *Store) hasContentEntries() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Installed) > 0 || len(m.Warmed) > 0
}

// ensureMaps re-allocates every map a decode may have nilled. The caller must
// hold the write lock. Every map New() initializes is listed here; a new map
// on Store must be added to both.
func (m *Store) ensureMaps() {
	m.APICache = ensureMap(m.APICache)
	m.DepsCache = ensureMap(m.DepsCache)
	m.Installed = ensureMap(m.Installed)
	m.Graph = ensureMap(m.Graph)
	m.Requirements = ensureMap(m.Requirements)
	m.Resolved = ensureMap(m.Resolved)
	m.Versions = ensureMap(m.Versions)
	m.Warmed = ensureMap(m.Warmed)
}

// ensureMap returns m when it is non-nil and a fresh empty map otherwise.
func ensureMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return make(map[K]V)
	}
	return m
}

// retentionWindow is the closed interval of write stamps a persist pass or a
// keep-set pass accepts: [oldest, newest]. Both bounds come from one wall-clock
// sample, so every entry judged against a given window is judged against the
// same instant - two entries in one save can never be classified by two
// different clocks.
type retentionWindow struct {
	oldest time.Time
	newest time.Time
}

// newRetentionWindow builds the window [now-maxAge, now] for a single
// wall-clock sample now, shared by every entry classified against the
// returned window.
func newRetentionWindow(now time.Time, maxAge time.Duration) retentionWindow {
	return retentionWindow{oldest: now.Add(-maxAge), newest: now}
}

// isStale reports whether stampedAt falls outside the window.
//
// Both bounds are inclusive. Strict Before keeps an entry stamped exactly at
// oldest (see TestSnapshotBoundaryEntryIsKept); strict After keeps an entry
// stamped exactly at newest, so an entry written in the same instant the
// window was sampled never expires the moment it is written.
//
// A future stamp is dropped, not clamped and not fatal. Clamping to newest
// would launder an invalid stamp into a valid one and hand it a fresh
// full-length lease; rejecting the whole snapshot would turn a droppable
// cache into an outage and would diverge from the local Bolt path. Dropping
// is the only option that both stops the entry surviving forever - a stamp
// like "9999-01-01" compared only against the oldest bound is never older
// than that bound, so it would be rewritten into every later save - and
// self-heals, since every bucket this guards is rebuildable.
//
// No skew tolerance, deliberately. With several machines on one cache, an
// entry written by a machine whose clock runs ahead looks future to the
// others and is refetched (or, for a warmed entry, re-extracted from a
// tarball that is still cached locally) - bounded, network-cheap,
// self-correcting. A tolerance constant would be an unfalsifiable number and
// a second definition of "now". Do not add one.
func (w retentionWindow) isStale(stampedAt time.Time) bool {
	return stampedAt.Before(w.oldest) || stampedAt.After(w.newest)
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

	now := time.Now().UTC()
	window := newRetentionWindow(now, helpers.CacheEntryMaxAge)
	warmedWindow := newRetentionWindow(now, helpers.WarmedEntryMaxAge)

	data := snapshotData{
		Meta:         m.Meta,
		APICache:     make(map[string]APICacheEntry, len(m.APICache)),
		DepsCache:    make(map[string]DepsCacheEntry, len(m.DepsCache)),
		Installed:    make(map[string]InstalledEntry, len(m.Installed)),
		Graph:        make(map[string][]string, len(m.Graph)),
		Requirements: make(map[string]RequirementSpec, len(m.Requirements)),
		Resolved:     make(map[string]ResolvedEntry, len(m.Resolved)),
		Versions:     make(map[string]VersionsEntry, len(m.Versions)),
		Warmed:       make(map[string]WarmedEntry, len(m.Warmed)),
	}

	for key, entry := range m.APICache {
		if window.isStale(entry.FetchedAt) {
			continue
		}
		data.APICache[key] = entry
	}
	for key, entry := range m.DepsCache {
		if window.isStale(entry.FetchedAt) {
			continue
		}
		clone := make(map[string]string, len(entry.Deps))
		maps.Copy(clone, entry.Deps)
		data.DepsCache[key] = DepsCacheEntry{FetchedAt: entry.FetchedAt, Deps: clone}
	}
	// Installed is copied whole, with no retention window, and that asymmetry
	// with Warmed just below is a decision rather than an oversight.
	//
	// Its cost is known and bounded: a project whose workspace is gone is
	// skipped by every cleanup run, so its keys never reach that pass's
	// would-remove set, so the shas its installed entries name are kept in the
	// extracted store forever. The consequence is disk growth in a
	// reconstructible layer - nothing anyone else owns is deleted - and the
	// existing remedy for an operator who wants that space back is
	// `--clear-cache`.
	//
	// An age window here would cost more than it recovers. An installed entry
	// is written by an install that actually did work; a collection already
	// present is skipped before recordInstall is ever reached, so a stable
	// cache in its steady state - every collection installed, every run a
	// no-op - stops refreshing its entries entirely. Under a window they would
	// age out from under a live project and take its extracted trees with
	// them, so the common case pays a full re-extraction on a timer to reclaim
	// space from the rare one. Making it safe would mean re-recording on the
	// skip path too, which turns every no-op run into a snapshot write.
	//
	// What would change this: a rule keyed on evidence rather than the clock.
	// A project whose registry entry names a requirements file that is gone
	// AND a workspace that is gone is dead by two independent signals, and its
	// installed entries could be dropped on that basis without a window and
	// without touching a live project. That is a cleanup-pass design, not a
	// snapshot-schema one, and it is what a revival of this should build.
	maps.Copy(data.Installed, m.Installed)
	for key, deps := range m.Graph {
		clone := make([]string, len(deps))
		copy(clone, deps)
		data.Graph[key] = clone
	}
	maps.Copy(data.Requirements, m.Requirements)
	maps.Copy(data.Resolved, m.Resolved)
	for key, entry := range m.Versions {
		if window.isStale(entry.FetchedAt) {
			continue
		}
		clone := make([]string, len(entry.List))
		copy(clone, entry.List)
		data.Versions[key] = VersionsEntry{FetchedAt: entry.FetchedAt, List: clone}
	}
	copyFreshWarmed(data.Warmed, m.Warmed, warmedWindow)

	return data
}

// copyFreshWarmed copies every entry from src into dst that falls inside
// window. It is factored out of snapshotData, rather than inlined there as a
// loop like the other buckets above, purely to keep snapshotData under its
// cyclomatic complexity budget; the caller holds the store's read
// lock for the duration of the call.
func copyFreshWarmed(dst, src map[string]WarmedEntry, window retentionWindow) {
	for key, entry := range src {
		if window.isStale(entry.WarmedAt) {
			continue
		}
		dst[key] = entry
	}
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
	stampSaveMeta(&data, store.hasContentEntries())

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
		func() error { return loadResolved(tx, store) },
		func() error { return loadVersions(tx, store) },
		func() error { return loadWarmed(tx, store) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// runSaveSteps writes the eight data buckets in the given transaction, in a
// fixed order (api_cache, deps_cache, installed, graph, requirements,
// resolved, versions_cache, warmed) that callers rely on for fault injection
// tests.
func runSaveSteps(tx *bolt.Tx, data snapshotData) error {
	steps := []func() error{
		func() error { return saveAPICache(tx, data) },
		func() error { return saveDepsCache(tx, data) },
		func() error { return saveInstalled(tx, data) },
		func() error { return saveGraph(tx, data) },
		func() error { return saveRequirements(tx, data) },
		func() error { return saveResolved(tx, data) },
		func() error { return saveVersions(tx, data) },
		func() error { return saveWarmed(tx, data) },
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
	// An absent key leaves the zero value, which HasRecordedContent reads as
	// "no run has recorded on-disk content here". That is the conservative
	// answer and it is also what a snapshot written by a binary predating this
	// key produces, so an older binary sharing the cache can only make a
	// destructive pass do less, never more.
	if v := metaBucket.Get([]byte(helpers.StoreMetaContentRecorded)); v != nil {
		t, err := time.Parse(time.RFC3339Nano, string(v))
		if err != nil {
			return fmt.Errorf("invalid content-recorded time: %w", err)
		}
		store.Meta.ContentRecorded = t
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

func loadWarmed(tx *bolt.Tx, store *Store) error {
	return loadBucket(tx, helpers.StoreBucketWarmed, func(k, v []byte) error {
		var entry WarmedEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		store.Warmed[string(k)] = entry
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
	// Written only when set, so a cache no content-recording run has ever
	// touched carries no key at all rather than a zero timestamp - the same
	// shape a binary predating this key leaves behind, which is what keeps
	// loadMeta's absent-key path the one both produce.
	if !meta.ContentRecorded.IsZero() {
		if err := metaBucket.Put([]byte(helpers.StoreMetaContentRecorded), []byte(meta.ContentRecorded.Format(time.RFC3339Nano))); err != nil {
			return err
		}
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

func saveWarmed(tx *bolt.Tx, data snapshotData) error {
	return saveBucket(tx, helpers.StoreBucketWarmed, data.Warmed, func(entry WarmedEntry) ([]byte, error) {
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
