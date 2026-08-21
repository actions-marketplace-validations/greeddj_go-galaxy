// Package store is go-galaxy's persisted cache state in memory - cached API
// responses, versions lists, dependency edges, resolved versions, and what is
// installed or warmed - together with the local plumbing behind it: the BoltDB
// snapshot file, the project registry, the instance lock, and the
// cache-directory sweeps. Store is the value both cache backends move: the
// local one bucket-maps it into Bolt, the S3 one marshals it as gzipped JSON.
//
// Store guards its own maps with an internal RWMutex, so callers go through
// the methods rather than reading or writing a field across goroutines.
// Persisting goes through snapshotData, the single copy path where age
// eviction, a signature source's query cut, and the meta stamps are applied,
// so both backends inherit one set of retention and redaction rules;
// helpers.StoreSnapshotSchemaVersion decides what a binary will read back,
// and ValidateSchema refuses anything newer than it understands.
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

// GitPinCollection is one collection a pinned git commit carried: its
// identity, the subdir it was built from, and its raw galaxy.yml
// dependencies, which is everything the solver needs to answer for it
// without touching the remote again.
type GitPinCollection struct {
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Namespace    string            `json:"namespace"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Subdir       string            `json:"subdir,omitempty"`
}

// GitPinEntry records what a (url, ref, subdir) git requirement resolved to:
// the commit, and the collections discovered at it. Keyed by
// gitsource.PinKey. A pin carries no retention window: it is invalidated by
// the requirements signature (a changed ref re-resolves) and by --refresh,
// never by the clock, because a branch that has not moved is the same answer
// a month later and re-advertising it would cost a round trip for nothing.
type GitPinEntry struct {
	FetchedAt   time.Time          `json:"fetched_at"`
	Commit      string             `json:"commit"`
	Collections []GitPinCollection `json:"collections"`
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
	GitPins      map[string]GitPinEntry     `json:"git_pins"`
	Meta         SnapshotMeta               `json:"meta"`
	mu           sync.RWMutex               `json:"-"`
	// dirty records whether this process has written something into the
	// store since it was loaded (or since New built a fresh one); see Dirty
	// for the full contract.
	dirty bool `json:"-"`
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
		GitPins:      make(map[string]GitPinEntry),
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
// The local Bolt path never needed this: loadJSONBucket only ever populates an
// already-initialized map key by key and never assigns a whole map field, so
// there is no decode step there that can replace a map with nil.
func (s *Store) UnmarshalJSON(data []byte) error {
	// storeJSON strips the json.Unmarshaler method set, so the decode below
	// cannot recurse back into this method. The conversion is on the pointer,
	// so the RWMutex is never copied.
	type storeJSON Store

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.Unmarshal(data, (*storeJSON)(s)); err != nil {
		return err
	}
	s.ensureMaps()
	return nil
}

// ResolvedEntry stores a resolved collection version and source. Ref is set
// only for a collection resolved from a git source: the branch, tag or
// commit the requirements file asked for, kept so a lockfile built from a
// replayed resolution records the same ref a fresh one would.
type ResolvedEntry struct {
	Version string `json:"version"`
	Source  string `json:"source"`
	Ref     string `json:"ref,omitempty"`
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
func (s *Store) SetInstalled(key string, entry InstalledEntry) {
	if s == nil {
		return
	}
	entry.Deps = slices.Clone(entry.Deps)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Installed[key] = entry
	s.dirty = true
}

// DeleteInstalled removes an installed entry by key.
func (s *Store) DeleteInstalled(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Installed, key)
	s.dirty = true
}

// GetInstalled returns an installed entry by key. Deps is cloned before
// returning, so a caller mutation of the returned slice cannot corrupt the
// stored snapshot state.
func (s *Store) GetInstalled(key string) (InstalledEntry, bool) {
	if s == nil {
		return InstalledEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.Installed[key]
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
func (s *Store) InstalledArtifactSHAByKey() map[string]string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.Installed))
	for key, entry := range s.Installed {
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
func (s *Store) SetWarmed(key, artifactSHA string) {
	if s == nil || key == "" || artifactSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Warmed[key] = WarmedEntry{WarmedAt: time.Now().UTC(), ArtifactSHA256: artifactSHA}
	s.dirty = true
}

// GetGitPin returns the pin recorded under key, deep-copied so a caller can
// neither observe nor cause a later mutation.
func (s *Store) GetGitPin(key string) (GitPinEntry, bool) {
	if s == nil {
		return GitPinEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.GitPins[key]
	if !ok {
		return GitPinEntry{}, false
	}
	return cloneGitPin(entry), true
}

// SetGitPin records entry under key, deep-copying it and stamping
// FetchedAt with the current time. An empty key or commit is ignored: a pin
// that names no commit protects nothing and would only ever be replayed into
// a failure.
func (s *Store) SetGitPin(key string, entry GitPinEntry) {
	if s == nil || key == "" || entry.Commit == "" {
		return
	}
	clone := cloneGitPin(entry)
	clone.FetchedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.GitPins[key] = clone
	s.dirty = true
}

// cloneGitPin copies a pin and every reference-type field inside it.
func cloneGitPin(entry GitPinEntry) GitPinEntry {
	clone := entry
	clone.Collections = make([]GitPinCollection, len(entry.Collections))
	for i, c := range entry.Collections {
		cc := c
		if c.Dependencies != nil {
			cc.Dependencies = make(map[string]string, len(c.Dependencies))
			maps.Copy(cc.Dependencies, c.Dependencies)
		}
		clone.Collections[i] = cc
	}
	return clone
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
func (s *Store) WarmedArtifactSHAByKey() map[string]string {
	if s == nil {
		return nil
	}
	window := newRetentionWindow(time.Now().UTC(), helpers.WarmedEntryMaxAge)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.Warmed))
	for key, entry := range s.Warmed {
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
func (s *Store) GetDepsCache(key string) (map[string]string, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.DepsCache[key]
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
func (s *Store) SetDepsCache(key string, deps map[string]string) {
	if s == nil {
		return
	}
	clone := make(map[string]string, len(deps))
	maps.Copy(clone, deps)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DepsCache[key] = DepsCacheEntry{FetchedAt: time.Now().UTC(), Deps: clone}
	s.dirty = true
}

// DeleteDepsCache removes cached dependency data for a key.
func (s *Store) DeleteDepsCache(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.DepsCache, key)
	s.dirty = true
}

// GetAPICache returns a cached API entry by key. The returned entry shares
// its Body backing array with the stored snapshot: callers must treat Body
// as read-only and must not mutate it. Body is deliberately not cloned on
// read because this is the warm-cache hot path and Body can be a large
// response payload; the only caller unmarshals it without mutating.
func (s *Store) GetAPICache(key string) (APICacheEntry, bool) {
	if s == nil {
		return APICacheEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.APICache[key]
	return entry, ok
}

// SetAPICache stores a cached API entry. The entry's Body is cloned before
// storing, so a later caller mutation (or reuse) of its backing buffer
// cannot corrupt the stored snapshot state.
func (s *Store) SetAPICache(key string, entry APICacheEntry) {
	if s == nil {
		return
	}
	entry.Body = slices.Clone(entry.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.APICache[key] = entry
	s.dirty = true
}

// ClearCaches clears the API, dependency, versions and git pin caches: every
// bucket that is an answer from a remote rather than a record of content on
// disk.
func (s *Store) ClearCaches() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.APICache = make(map[string]APICacheEntry)
	s.DepsCache = make(map[string]DepsCacheEntry)
	s.Versions = make(map[string]VersionsEntry)
	s.GitPins = make(map[string]GitPinEntry)
	s.dirty = true
}

// GetVersionsCache returns cached versions for a key. This is a pure read
// under RLock: it does not bump the entry's FetchedAt, so a hit here never
// requires upgrading to the write lock.
func (s *Store) GetVersionsCache(key string) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.Versions[key]
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
func (s *Store) SetVersionsCache(key string, versions []string) {
	if s == nil {
		return
	}
	clone := make([]string, len(versions))
	copy(clone, versions)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Versions[key] = VersionsEntry{FetchedAt: time.Now().UTC(), List: clone}
	s.dirty = true
}

// SetResolvedAll replaces the resolved entries map.
func (s *Store) SetResolvedAll(resolved map[string]ResolvedEntry) {
	if s == nil {
		return
	}
	clone := make(map[string]ResolvedEntry, len(resolved))
	maps.Copy(clone, resolved)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Resolved = clone
	s.dirty = true
}

// ResolvedSnapshot returns a copy of resolved entries.
func (s *Store) ResolvedSnapshot() map[string]ResolvedEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone := make(map[string]ResolvedEntry, len(s.Resolved))
	maps.Copy(clone, s.Resolved)
	return clone
}

// SetGraph records dependencies for a collection key. deps is cloned before
// storing, so a later caller mutation of its backing array cannot corrupt
// the stored snapshot state.
func (s *Store) SetGraph(key string, deps []string) {
	if s == nil {
		return
	}
	clone := slices.Clone(deps)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Graph[key] = clone
	s.dirty = true
}

// DeleteGraph removes dependency data for a key.
func (s *Store) DeleteGraph(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Graph, key)
	s.dirty = true
}

// SetGraphSnapshot replaces the dependency graph.
func (s *Store) SetGraphSnapshot(graph map[string][]string) {
	if s == nil {
		return
	}
	clone := make(map[string][]string, len(graph))
	for key, deps := range graph {
		out := make([]string, len(deps))
		copy(out, deps)
		clone[key] = out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Graph = clone
	s.dirty = true
}

// GraphSnapshot returns a copy of the dependency graph.
func (s *Store) GraphSnapshot() map[string][]string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone := make(map[string][]string, len(s.Graph))
	for key, deps := range s.Graph {
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
func (s *Store) SetRequirements(spec map[string]RequirementSpec) {
	if s == nil {
		return
	}
	clone := make(map[string]RequirementSpec, len(spec))
	for key, value := range spec {
		value.Signatures = slices.Clone(value.Signatures)
		clone[key] = value
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requirements = clone
	s.dirty = true
}

// RequirementsSnapshot returns a fully independent deep copy of requirement
// specs: each entry's Signatures slice is cloned too, so mutating the
// returned map or any of its Signatures slices cannot corrupt the stored
// snapshot state.
func (s *Store) RequirementsSnapshot() map[string]RequirementSpec {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone := make(map[string]RequirementSpec, len(s.Requirements))
	for key, value := range s.Requirements {
		value.Signatures = slices.Clone(value.Signatures)
		clone[key] = value
	}
	return clone
}

// MetaSnapshot returns the current snapshot metadata.
func (s *Store) MetaSnapshot() SnapshotMeta {
	if s == nil {
		return SnapshotMeta{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Meta
}

// WasPersisted reports whether this store was ever actually loaded from a
// persisted snapshot, as opposed to being a fresh, never-saved store. It is
// equivalent to !s.Meta.LastSnapshot.IsZero(): LastSnapshot is zero exactly
// when no persisted snapshot was loaded - New() leaves it zero, Load returns
// New() outright for ErrOutdatedSchemaVersion (a dropped, pre-migration
// snapshot), the S3 backend's LoadStore returns store.New() for both
// errS3NotFound and a failed outdated-schema probe, and an absent meta
// bucket leaves LastSnapshot zero on an otherwise normally-loaded store -
// loadMeta returns early on a missing bucket, so the data buckets still
// load as usual; that combination only a hand-edited database can produce,
// and skipping is the conservative answer there anyway - while Save and
// MarshalSnapshot both stamp it unconditionally on every persisted write.
// It reads s.Meta.LastSnapshot directly under the read lock rather than
// through MetaSnapshot, which would copy the whole SnapshotMeta struct just
// to check one field.
//
// Callers need this because a store that was never persisted carries no
// evidence about what is actually in use anywhere: any pass that deletes
// content or narrows state on the strength of such a store is acting on
// ignorance, not on evidence, and must not treat an empty in-memory map as
// proof that nothing is installed or warmed.
func (s *Store) WasPersisted() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.Meta.LastSnapshot.IsZero()
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
func (s *Store) HasRecordedContent() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.Meta.ContentRecorded.IsZero()
}

// SetMetaRequirements stores the requirements hash and server.
func (s *Store) SetMetaRequirements(hash, server string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Meta.RequirementsHash = hash
	s.Meta.Server = server
	s.dirty = true
}

// Dirty reports whether this process has written something into the store
// since it was loaded (or since New built a fresh one). It is what lets a
// caller decide whether a save has anything to persist at all.
//
// The predicate is "this process wrote something into the store since it was
// loaded", never "the store differs from what the backend holds": nothing
// here compares against what was last persisted, and nothing here consults
// the backend to find out - it is a pure read of a flag this process set
// itself.
//
// The flag is set unconditionally by any mutator call, never conditioned on
// whether the value actually changed and never cleared, because a false
// positive costs one redundant save - exactly today's behavior - while a
// false negative silently drops persisted state. The two are not symmetric,
// so the design errs toward saving.
//
// A load never sets it, for the identical reason in both cases: store.Load's
// loadJSONBucket populates Store's map fields directly, key by key,
// rather than through a setter, and UnmarshalJSON decodes straight onto the
// struct the same way - so setting the flag from either path would make
// every run against a backend that already holds a snapshot dirty on
// arrival, which defeats the point of this flag entirely. Every read method
// takes only the read lock and never reaches a mutator, so none of them set
// it either.
func (s *Store) Dirty() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
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
	GitPins      map[string]GitPinEntry
	Meta         SnapshotMeta
}

// MarshalSnapshot returns a schema-stamped JSON encoding of the store,
// suitable for writing to a remote snapshot backend. It builds the payload
// from snapshotData's RLock-protected deep copy rather than marshaling the
// live store directly, so a concurrent writer goroutine cannot trip the
// race detector or produce a torn payload. The schema version and
// last-snapshot timestamp are stamped exactly as Save does.
func (s *Store) MarshalSnapshot() ([]byte, error) {
	data := s.snapshotData()
	stampSaveMeta(&data, s.hasContentEntries())

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
		GitPins:      data.GitPins,
		Meta:         data.Meta,
	}
	return json.Marshal(snapshot)
}

// hasContentEntries reports whether the live store currently holds any record
// of on-disk content. It is what a save consults to decide whether to stamp
// Meta.ContentRecorded; see stampSaveMeta for why it is read here, from the
// store itself, rather than from the payload that save is about to write.
func (s *Store) hasContentEntries() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Installed) > 0 || len(s.Warmed) > 0
}

// ensureMaps re-allocates every map a decode may have nilled. The caller must
// hold the write lock. Every map New() initializes is listed here; a new map
// on Store must be added to both.
func (s *Store) ensureMaps() {
	s.APICache = ensureMap(s.APICache)
	s.DepsCache = ensureMap(s.DepsCache)
	s.Installed = ensureMap(s.Installed)
	s.Graph = ensureMap(s.Graph)
	s.Requirements = ensureMap(s.Requirements)
	s.Resolved = ensureMap(s.Resolved)
	s.Versions = ensureMap(s.Versions)
	s.Warmed = ensureMap(s.Warmed)
	s.GitPins = ensureMap(s.GitPins)
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

// snapshotData builds a snapshot payload from the store, applying two
// transformations neither persist entrypoint has to know about on its own:
// both the local Bolt Save and the S3 MarshalSnapshot build their payload
// through this one method, so both bounds apply to both without either
// caller repeating them.
//
// First, entries in APICache, DepsCache, and Versions that were last written
// before the CacheEntryMaxAge retention window are dropped from the payload.
// The live maps themselves are never pruned, only this RLock-protected copy
// - a still-warm entry stays available for reads until it is naturally
// overwritten or the store process restarts and reloads the (now-pruned)
// persisted snapshot.
//
// Second, every Requirements entry's Signatures sources have their query cut
// on the way out, via copyRequirementsCutQuery/signaturesWithoutQuery below.
// See normalizeSignatures' own doc comment
// (internal/galaxy/collections/resolve.go) for why a signature source's
// query must never reach persisted state, and for the mixed-fleet
// consequences of the two binaries either side of this cut disagreeing about
// it. The residual here is a predicate, not a gap: a run that persists
// nothing does not strip, for the identical reason cache.WithCleanSaveSkip's
// own doc comment (internal/galaxy/cache/dirtyskip.go) gives for the age
// eviction just above - this method's cut runs only inside a call this
// decorator lets through, and a call it skips carries no bytes to strip in
// the first place.
func (s *Store) snapshotData() snapshotData {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now().UTC()
	window := newRetentionWindow(now, helpers.CacheEntryMaxAge)
	warmedWindow := newRetentionWindow(now, helpers.WarmedEntryMaxAge)

	data := snapshotData{
		Meta:         s.Meta,
		APICache:     make(map[string]APICacheEntry, len(s.APICache)),
		DepsCache:    make(map[string]DepsCacheEntry, len(s.DepsCache)),
		Installed:    make(map[string]InstalledEntry, len(s.Installed)),
		Graph:        make(map[string][]string, len(s.Graph)),
		Requirements: make(map[string]RequirementSpec, len(s.Requirements)),
		Resolved:     make(map[string]ResolvedEntry, len(s.Resolved)),
		Versions:     make(map[string]VersionsEntry, len(s.Versions)),
		Warmed:       make(map[string]WarmedEntry, len(s.Warmed)),
		GitPins:      make(map[string]GitPinEntry, len(s.GitPins)),
	}

	for key, entry := range s.APICache {
		if window.isStale(entry.FetchedAt) {
			continue
		}
		data.APICache[key] = entry
	}
	for key, entry := range s.DepsCache {
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
	maps.Copy(data.Installed, s.Installed)
	for key, deps := range s.Graph {
		clone := make([]string, len(deps))
		copy(clone, deps)
		data.Graph[key] = clone
	}
	copyRequirementsCutQuery(data.Requirements, s.Requirements)
	maps.Copy(data.Resolved, s.Resolved)
	for key, entry := range s.Versions {
		if window.isStale(entry.FetchedAt) {
			continue
		}
		clone := make([]string, len(entry.List))
		copy(clone, entry.List)
		data.Versions[key] = VersionsEntry{FetchedAt: entry.FetchedAt, List: clone}
	}
	copyFreshWarmed(data.Warmed, s.Warmed, warmedWindow)
	// Git pins are copied whole: see GitPinEntry for why they carry no
	// retention window.
	for key, entry := range s.GitPins {
		data.GitPins[key] = cloneGitPin(entry)
	}

	return data
}

// copyRequirementsCutQuery copies every entry from src into dst, cutting each
// entry's Signatures sources down to their query-free form via
// signaturesWithoutQuery. It exists so the persisted spec can never carry a
// query a run's own producer-side cut (normalizeSignatures,
// internal/galaxy/collections/resolve.go) failed to strip before an entry
// reached the store. The shape that reaches this in production is a
// snapshot predating this cut, loaded by a run that never rebuilds the
// spec: install --frozen takes resolveOrLoadLockfile's lockfile branch,
// which never calls buildRequirementsSpec/recordResolution - the only path
// that ever runs a signature source through normalizeSignatures' own query
// cut - so an entry an older binary, from before this cut existed, once
// wrote unstripped, or any entry set directly
// onto the map bypassing SetRequirements (the shape a test seeds when it
// cannot depend on an older binary having run first), survives untouched
// until this backstop runs; see normalizeSignatures' own doc comment for
// why the persist-side cut has to exist at all rather than trusting every
// write path to have already applied it.
//
// It is factored out of snapshotData, rather than inlined there as a loop
// like the other buckets above, to keep snapshotData under its cyclomatic
// complexity budget: inlining this helper's own loop there alone raises
// snapshotData's complexity from 8 to 9, inlining copyFreshWarmed's below
// alone raises it to 10, and inlining both at once raises it to 11 - the
// only one of those three shapes that breaches cyclop's default limit of
// 10 (measured with the pinned golangci-lint release; .golangci.yml sets no
// cyclop override, and loadMeta/saveMeta already sit at exactly 10 and
// pass). The caller holds the store's read lock for the duration of the
// call.
func copyRequirementsCutQuery(dst, src map[string]RequirementSpec) {
	for key, entry := range src {
		entry.Signatures = signaturesWithoutQuery(entry.Signatures)
		dst[key] = entry
	}
}

// signaturesWithoutQuery returns sources with every entry's query cut via
// helpers.WithoutQuery, or sources itself unchanged when no entry's cut form
// differs from its own - the common case, since a run's own producer-side
// cut normally already stripped every source before it ever reached the
// store. Only a genuine hit allocates: the scan finds the first element
// WithoutQuery would change, and only then is one slice built, copying the
// untouched prefix verbatim and cutting the remainder.
//
// Returning sources itself on the no-hit path preserves the exact aliasing
// the maps.Copy this function replaced already had for this field, rather
// than introducing new aliasing: the caller holds only the store's read
// lock, so the returned slice must never be written through, and nothing
// here does. The one path that could, Store.SetRequirements, never mutates
// an existing backing array in place - it always replaces the whole map with
// freshly cloned per-entry Signatures slices (see its own doc comment) - so
// an alias handed out from here can never be observed changing under a
// concurrent reader.
func signaturesWithoutQuery(sources []string) []string {
	cut := -1
	for i, source := range sources {
		if helpers.WithoutQuery(source) != source {
			cut = i
			break
		}
	}
	if cut < 0 {
		return sources
	}

	out := make([]string, len(sources))
	copy(out, sources[:cut])
	for i := cut; i < len(sources); i++ {
		out[i] = helpers.WithoutQuery(sources[i])
	}
	return out
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
// bucket and all nine data buckets are written inside a single Bolt
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

// runSteps runs each step in order and returns the first error, leaving the
// remaining steps unrun. Both bucket runners below share it, so the rule that
// a failing bucket aborts the rest is stated once rather than per runner.
func runSteps(steps []func() error) error {
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// jsonBucketIO is one data bucket bound to the maps it is mirrored in, in
// both directions at once: load fills the live store's map, save writes the
// snapshot copy's. Binding both together means a bucket cannot be listed for
// one direction and forgotten in the other.
type jsonBucketIO struct {
	load func(*bolt.Tx) error
	save func(*bolt.Tx) error
}

// bindJSONBucket pairs the named bucket with the store map it loads into and
// the snapshot map it saves from.
func bindJSONBucket[T any](name string, dst, src map[string]T) jsonBucketIO {
	return jsonBucketIO{
		load: func(tx *bolt.Tx) error { return loadJSONBucket(tx, name, dst) },
		save: func(tx *bolt.Tx) error { return saveJSONBucket(tx, name, src) },
	}
}

// jsonBuckets lists the nine data buckets in the one fixed order both
// runners follow (api_cache, deps_cache, installed, graph, requirements,
// resolved, versions_cache, warmed, git_pins), which fault injection tests
// rely on for the save side.
func jsonBuckets(store *Store, data snapshotData) []jsonBucketIO {
	return []jsonBucketIO{
		bindJSONBucket(helpers.StoreBucketAPICache, store.APICache, data.APICache),
		bindJSONBucket(helpers.StoreBucketDepsCache, store.DepsCache, data.DepsCache),
		bindJSONBucket(helpers.StoreBucketInstalled, store.Installed, data.Installed),
		bindJSONBucket(helpers.StoreBucketGraph, store.Graph, data.Graph),
		bindJSONBucket(helpers.StoreBucketRequirements, store.Requirements, data.Requirements),
		bindJSONBucket(helpers.StoreBucketResolved, store.Resolved, data.Resolved),
		bindJSONBucket(helpers.StoreBucketVersions, store.Versions, data.Versions),
		bindJSONBucket(helpers.StoreBucketWarmed, store.Warmed, data.Warmed),
		bindJSONBucket(helpers.StoreBucketGitPins, store.GitPins, data.GitPins),
	}
}

// runLoadSteps reads the nine data buckets in the given transaction.
func runLoadSteps(tx *bolt.Tx, store *Store) error {
	buckets := jsonBuckets(store, snapshotData{})
	steps := make([]func() error, 0, len(buckets))
	for _, b := range buckets {
		steps = append(steps, func() error { return b.load(tx) })
	}
	return runSteps(steps)
}

// runSaveSteps writes the nine data buckets in the given transaction, in the
// fixed order jsonBuckets states.
func runSaveSteps(tx *bolt.Tx, data snapshotData) error {
	buckets := jsonBuckets(&Store{}, data)
	steps := make([]func() error, 0, len(buckets))
	for _, b := range buckets {
		steps = append(steps, func() error { return b.save(tx) })
	}
	return runSteps(steps)
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

// loadJSONBucket decodes every value in the named bucket as a JSON-encoded T
// and stores it in dst under that entry's bucket key. A bucket that does not
// exist is not an error; loadBucket above owns that contract.
//
// A value that does not decode is reported as an error rather than stored as a
// zero T: every current-schema value is written as valid JSON by
// saveJSONBucket, so a decode failure means the stored bytes are genuinely
// corrupt, and coercing them into a zero entry would hand a caller a garbage
// record it cannot tell from a real one.
//
// The error names both the bucket and the key because a caller of Load
// otherwise gets a bare encoding/json message identifying neither, leaving an
// operator with a corrupt cache and nothing to look at. The key renders through
// %q because it is a byte string read back from persisted cache state, so an
// unprintable or empty key still renders unambiguously.
//
// The destination is the map rather than the *Store, so a decoded entry is
// written into it directly and this function has no receiver through which to
// reach a mutator - which is what leaves Store.Dirty false after a load (see
// Store.Dirty, and TestDirtyIsFalseAfterEveryLoadPath for the pin).
func loadJSONBucket[T any](tx *bolt.Tx, name string, dst map[string]T) error {
	return loadBucket(tx, name, func(k, v []byte) error {
		var entry T
		if err := json.Unmarshal(v, &entry); err != nil {
			return fmt.Errorf("invalid %s entry %q: %w", name, string(k), err)
		}
		dst[string(k)] = entry
		return nil
	})
}

// saveJSONBucket replaces the named bucket's contents with data, within the
// caller's transaction, encoding every value as JSON under its map key.
//
// The encoding is not a parameter because every bucket of this snapshot is
// JSON, and the load side assumes exactly that: loadJSONBucket above decodes
// whatever it finds as JSON and treats anything else as corruption.
func saveJSONBucket[T any](tx *bolt.Tx, name string, data map[string]T) error {
	bucket, err := ensureEmptyBucket(tx, name)
	if err != nil {
		return err
	}
	for key, entry := range data {
		encoded, err := json.Marshal(&entry)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(key), encoded); err != nil {
			return err
		}
	}
	return nil
}
