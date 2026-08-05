package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// installedCollection tracks an installed collection discovered on disk.
// Namespace and Name are the two ansible_collections/<namespace>/<name>
// directory components the scan walked through to find this record's
// manifest, never a value the manifest itself declared, so no MANIFEST.json
// content can ever redirect a path this package builds from Namespace/Name
// at a different collection's directory. See buildInstalledRecord's doc
// comment for the one field that is still manifest-derived (Version) and
// for why its own blast radius stays bounded to this same Namespace/Name
// pair rather than ever reaching a different collection.
type installedCollection struct {
	Parsed         *semver.Version
	Key            string
	FQDN           string
	Namespace      string
	Name           string
	Version        string
	InstallPath    string
	CollectionsDir string
}

type cleanupState struct {
	backend  cacheManager.Backend
	store    *store.Store
	registry *store.ProjectRegistry
	release  func() error
}

// Start runs the cleanup process for unused collections.
func Start(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	err := runCleanup(ctx, cfg, runtime)
	if err != nil {
		runtime.Output.Errorf("Error: %s", err.Error())
	}
	return err
}

// runCleanup owns the backend lifecycle for the cleanup command: it opens the
// backend, takes its exclusive lock, and registers the release/close defers
// that must run on every exit path - including one from cleanupWithState,
// which owns the actual work once state is initialized. This is the same
// lifecycle/work split collections.runInstall/installWithState already draw,
// and it exists here for one concrete reason: the lock-loss verdict
// (cacheManager.LockLostError) needs exactly ONE place to wrap this command's
// outcome. The work half returns from several places and nonamedreturns is
// enabled, so a defer cannot wrap them, and a wrap repeated at every return
// site would silently drift apart.
//
// lockCtx is the backend's holder context (see cacheManager.Backend's Lock
// contract): the work runs under it, so a run whose lock is stolen mid-flight
// stops deleting, and both the init error and the work's return are judged
// against it here. "Stops" has a stated granularity, and it is the only thing
// this pipeline can honestly claim: the work reads the holder context before
// each next collection in removeUnused, once more between that pass and the
// sweeps, before each next legacy artifact key in sweepLegacyArtifacts, and
// before each next entry in the extracted-store sweep (extracted.Store.Sweep).
// The residual is one unit of work in flight: a RemoveAll already walking a
// collection's tree, and an ArtifactStore.Delete already issued, both run to
// completion - neither is interruptible, and abandoning a tree half-removed
// would be worse than finishing it. Those same checks are what makes cleanup
// answer an operator's Ctrl-C between collections rather than only once every
// project has been walked; that path stays a plain context cancellation and
// keeps its own exit class, since only a genuine lock-loss cause reaches the
// verdict.
func runCleanup(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	lockCtx, state, err := initCleanup(ctx, cfg, runtime)
	if err != nil {
		return cacheManager.LockLostError(ctx, lockCtx, err)
	}
	if state == nil {
		// initCleanup returns a non-nil state on success; this guard is
		// defensive and has nothing acquired to release.
		return nil
	}
	// Register the lock release and backend close before any work: initCleanup
	// has already acquired the lock and opened the backend by this point, so
	// even cleanupWithState's empty-registry no-op must still release both
	// instead of leaking them.
	defer func() {
		if state.release != nil {
			if err := state.release(); err != nil {
				runtime.Output.Errorf("lock release: %v", err)
			}
		}
	}()
	defer func() {
		if state.backend != nil {
			_ = state.backend.Close(ctx)
		}
	}()

	return cacheManager.LockLostError(ctx, lockCtx, cleanupWithState(lockCtx, cfg, runtime, state))
}

// cleanupWithState performs cleanup's actual work against an
// already-initialized state: compute reachability across every recorded
// project, remove what nothing reaches, sweep the legacy artifact keys and
// the extracted store, then save. It assumes the backend is already open and
// locked - runCleanup holds that lifecycle - so it never touches
// state.release or state.backend.Close itself.
func cleanupWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *cleanupState) error {
	if state.registry == nil || len(state.registry.Projects) == 0 {
		runtime.Output.Printf("ℹ️ No projects recorded for GC.")
		return nil
	}
	warnIfSnapshotNotPersisted(runtime, state.store)

	reachable, installedByKey, err := buildReachable(runtime, state.registry)
	if err != nil {
		return err
	}
	removed, err := removeUnused(ctx, cfg, runtime, state.backend, state.store, reachable, installedByKey)
	if err != nil {
		return err
	}
	// removeUnused returning cleanly means it deleted every unreachable
	// collection it found, not that this run still owns the cache: the last
	// key's own check passed before that key was removed, and the holder
	// context can have ended during the removal itself. Re-read it here so a
	// run that lost the lock during the final collection does not go on to
	// sweep artifact keys under a cache another holder is already writing.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cleanup stopped before sweeping cached artifacts: %w", err)
	}
	sweepLegacyArtifacts(ctx, cfg, runtime, state.backend, installedByKey)
	sweepExtractedStore(ctx, cfg, runtime, state.store, reachable, installedByKey)
	return finalizeCleanup(ctx, cfg, runtime, state.backend, state.store, removed)
}

// warnIfSnapshotNotPersisted emits a single operator-facing warning when st
// carries no evidence of a persisted snapshot (a fresh cache dir, a schema
// bump that dropped it, or a missing/expired S3 state object): the
// extracted-cache sweep (sweepExtractedStore) and the snapshot save
// (finalizeCleanup) both act on evidence this store does not have, so both
// are skipped this run and the persisted snapshot - if any exists in the
// backend - is left untouched. This is the only place that message is printed; the two
// guarded functions themselves stay silent about why they no-op.
func warnIfSnapshotNotPersisted(runtime *infra.Infra, st *store.Store) {
	if st.HasRecordedContent() {
		return
	}
	if st.WasPersisted() {
		runtime.Output.Warnf(
			"the persisted snapshot records nothing about what is installed or warmed; skipping the extracted-cache sweep this run",
		)
		return
	}
	runtime.Output.Warnf(
		"no persisted snapshot was found; skipping the extracted-cache sweep and leaving the snapshot untouched this run",
	)
}

// initCleanup opens the cache backend, takes its exclusive lock, and loads
// the snapshot and the project registry this run reasons about, releasing
// and closing whatever it already acquired on any failure after that point.
//
// The returned context is the backend's HOLDER CONTEXT (see
// cacheManager.Backend's own Lock contract): every step below that runs after
// the lock is taken uses it rather than ctx, and it is returned on the
// post-lock failure paths too, not only on success - so a LoadStore or
// LoadProjectRegistry failure caused by this run's lock being stolen is
// reported as exactly that instead of as an ordinary backend failure.
// backend.Open runs before the lock exists and backend.Close must still run
// after ownership is gone, so both keep the caller's own ctx.
func initCleanup(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (context.Context, *cleanupState, error) {
	runtime.Output.Printf("🚀 init cache backend")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		return nil, nil, err
	}
	// Every persisted cache-state operation this run makes (LoadStore and
	// LoadProjectRegistry below) runs under its own bounded budget from here
	// on: both happen after the exclusive lock below is acquired, and a
	// stalled read there would otherwise hold that lock - and block every
	// other runner sharing this backend - for as long as the caller's own
	// context allows. See cacheManager.WithStateDeadline's own doc comment
	// for why this wraps the backend once here rather than at each of its
	// several call sites (this file's own finalizeCleanup among them).
	backend = cacheManager.WithStateDeadline(backend, runtime.StateDeadline())
	if err := backend.Open(ctx); err != nil {
		return nil, nil, err
	}
	lockCtx, releaseLock, err := backend.Lock(ctx)
	if err != nil {
		_ = backend.Close(ctx)
		return nil, nil, err
	}
	runtime.Output.Printf("🚀 load storage")
	st, err := backend.LoadStore(lockCtx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return lockCtx, nil, err
	}
	runtime.Output.Printf("🚀 load projects registry")
	registry, err := backend.LoadProjectRegistry(lockCtx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return lockCtx, nil, err
	}
	return lockCtx, &cleanupState{
		backend:  backend,
		store:    st,
		registry: registry,
		release:  releaseLock,
	}, nil
}

// buildReachable computes, for every project recorded in the registry, the
// set of installed collection keys (ns.name@version) reachable from that
// project's requirements.yml, transitively through each installed
// collection's own declared dependencies, plus installedByKey - the full
// index of every on-disk copy that removeUnused and sweepLegacyArtifacts
// iterate afterward.
//
// This runs as two full passes over one sorted project-path list
// (projectPaths), never interleaved per project:
//
//   - Phase 1 scans every recorded project's on-disk workspace into the
//     shared installedIndex/installedByKey/depsByKey maps, before any
//     project's requirements are resolved against them.
//   - Phase 2 resolves every recorded project's requirements roots
//     (projectRequirementRoots) against that now-complete index.
//
// The split is load-bearing, not cosmetic. A single-phase implementation
// that scans one project's workspace and immediately resolves that same
// project's roots against the index built so far would only ever see
// collections scanned by projects processed earlier: project A's
// requirements.yml naming a collection only project B installs would find
// nothing whenever B happens to be scanned after A - purely a function of
// iteration order, not of what either project actually declares. Splitting
// into two full passes removes that ordering dependency entirely: every
// root, from every project, is resolved against every project's scanned
// collections, regardless of which project the registry yields first.
//
// projectPaths is sorted (slices.Sorted over the registry's map keys) for
// three reasons, all load-bearing:
//
//   - it makes a phase-2 abort deterministic - the same unreadable project's
//     requirements file names itself in the returned error on every run,
//     rather than whichever project the map's randomized iteration order
//     happened to yield first;
//   - it makes warning order deterministic for the identical reason; and
//   - it lets a test fixture pin a genuine ordering bug with a single run
//     instead of a statistical loop that only sometimes iterates the
//     registry in the order that exposes it.
func buildReachable(runtime *infra.Infra, registry *store.ProjectRegistry) (map[string]bool, map[string][]installedCollection, error) {
	reachable := make(map[string]bool)
	installedIndex := make(map[string][]installedCollection)
	depsByKey := make(map[string]map[string]string)
	// installedByKey accumulates every on-disk copy of a given key
	// (ns.name@version): the same collection can be installed under more
	// than one project's collections path, and every copy must be found so
	// removeUnused can remove all of them in a single run rather than
	// overwriting earlier copies and only shedding one per run.
	installedByKey := make(map[string][]installedCollection)
	// constraints caches each raw requirement constraint string's parsed
	// *semver.Constraints (or nil for an unparseable one) across the whole
	// BFS below, so the same constraint evaluated on many edges is parsed
	// at most once instead of once per edge.
	constraints := make(map[string]*semver.Constraints)

	projectPaths := slices.Sorted(maps.Keys(registry.Projects))

	// Phase 1: every project's workspace into the shared index, before any
	// root resolves against it.
	for _, projectPath := range projectPaths {
		if err := scanProjectWorkspace(
			runtime.Output, projectPath, registry.Projects[projectPath], installedIndex, installedByKey, depsByKey,
		); err != nil {
			return nil, nil, err
		}
	}

	// Phase 2: every recorded project's roots, against the complete index.
	for _, projectPath := range projectPaths {
		roots, err := projectRequirementRoots(runtime.Output, projectPath, registry.Projects[projectPath])
		if err != nil {
			return nil, nil, err
		}
		for _, root := range roots {
			fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
			for _, inst := range selectInstalled(installedIndex, constraints, fqdn, root.Version) {
				markReachable(inst.Key, reachable, depsByKey, installedIndex, constraints)
			}
		}
	}
	return reachable, installedByKey, nil
}

// projectRequirementRoots loads projectPath's requirements roots for
// reachability. It runs for every recorded project in phase 2, regardless of
// whether that project's own workspace was scanned in phase 1: phase 1 and
// phase 2 are deliberately independent (see buildReachable's own doc
// comment), so a project skipped in phase 1 - an absent or escaping
// workspace - still contributes whatever roots its own requirements.yml
// declares here, potentially keeping another project's on-disk copy alive.
//
// Only two states about a project's requirements are honest, and this
// function distinguishes exactly those two by testing the load failure
// against fs.ErrNotExist: "this project declares nothing" (loadRequirements
// failed with a plain fs.ErrNotExist - zero roots, no error) and "this
// project's declarations are unknown" (loadRequirements failed with
// anything else - the roots it would have contributed cannot be
// determined). The first is a single warning naming the project,
// contributing no roots and letting the run proceed; the second aborts the
// whole run before removeUnused deletes anything, since an unknown set of
// roots could have been protecting any project's on-disk copies, not only
// this one's.
func projectRequirementRoots(
	out output.Printer, projectPath string, project store.ProjectRecord,
) ([]requirements.CollectionRequirement, error) {
	roots, err := loadRequirements(project.RequirementsFile, "")
	if err == nil {
		return roots, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		out.Warnf("project %q: requirements file %q no longer exists; it contributes no reachability roots this run",
			projectPath, project.RequirementsFile)
		return nil, nil
	}
	return nil, fmt.Errorf("%w: %s: %w", helpers.ErrProjectRequirementsUnreadable, project.RequirementsFile, err)
}

func removeUnused(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) (int, error) {
	var removed int
	// Sorted rather than ranged directly: this loop returns on the first
	// removal failure below, so which subset of installedByKey was already
	// deleted before that return must not depend on Go's randomized map
	// iteration order - the same failure, on the same recorded keys, must
	// leave the same partial result on disk every time.
	for _, key := range slices.Sorted(maps.Keys(installedByKey)) {
		// Read before the reachable skip below, deliberately, and this is the
		// difference between a claim that holds and one that only looks like
		// it does: placed after the skip, the loop would stop only once it
		// reached a key it was actually going to remove, so a cancellation
		// landing in a long run of reachable keys would be honored an
		// arbitrary number of iterations late. Placed here, "this loop stops
		// at the next collection" is a property of every iteration.
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("cleanup stopped at %s: %w", key, err)
		}
		insts := installedByKey[key]
		if reachable[key] {
			continue
		}
		removed++
		// key is "<ns>.<name>@<version>", built by buildInstalledRecord only
		// after ns, name, and version each pass helpers.IsPathElement - which
		// rejects every control character, \n and \t included (see
		// IsPathElement's own doc comment). key therefore can never carry a
		// character able to forge an extra line or a terminal command, so
		// this line, the "removed" line below, and the stop error above all
		// print it with a bare %s rather than %q: quoting would change the
		// exact, greppable shape CI tooling matches against (e.g.
		// "🧹 removed ns.name@1.0.0").
		if cfg.DryRun {
			runtime.Output.Printf("🧹 would remove %s", key)
			continue
		}
		// The persisted InstalledEntry's own Source - not any field on the
		// on-disk installedCollection scan, which never recorded a server at
		// all - is the only place the server this collection actually
		// resolved from is available, and it is what the current, scoped
		// artifact key (helpers.ArtifactKey) must be built from.
		source := installedSource(st, key)
		// The same key can be installed under more than one project's
		// collections path; every on-disk copy is removed in this single
		// run, and the snapshot is pruned exactly once afterward rather
		// than once per copy.
		for _, inst := range insts {
			if err := removeInstalled(ctx, inst, backend.Artifacts(), source); err != nil {
				return removed, err
			}
		}
		runtime.Output.Printf("🧹 removed %s", key)
		if st != nil {
			st.DeleteInstalled(key)
			st.DeleteGraph(key)
			// The deps-cache entry for this key, if any, lives under the
			// server-scoped key (helpers.ScopedDepsCacheKey) the resolve path
			// wrote it under, not under key itself - a bare "ns.name@version"
			// delete would never match anything once every live entry carries
			// the server-base prefix. Without source (e.g. this collection's
			// InstalledEntry predates the multi-server work, or was never
			// recorded), there is no way to know which scoped key to target,
			// so the stale entry is simply left for CacheEntryMaxAge to evict.
			if source != "" {
				st.DeleteDepsCache(helpers.ScopedDepsCacheKey(source, key))
			}
		}
	}
	return removed, nil
}

// installedSource returns the server the collection recorded under key
// actually resolved from - the persisted InstalledEntry's own Source, the
// only place that survives from resolve time, since the on-disk manifest
// scan (installedCollection) never records a server at all. It returns ""
// when st is nil or has no entry for key, in which case the caller cannot
// build a correctly scoped artifact or deps-cache key and must skip that
// part of the purge rather than guess.
func installedSource(st *store.Store, key string) string {
	if st == nil {
		return ""
	}
	entry, ok := st.GetInstalled(key)
	if !ok {
		return ""
	}
	return entry.Source
}

func finalizeCleanup(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	removed int,
) error {
	// A store that was never persisted (st.WasPersisted() false) never had a
	// registry-driven pass run against it either: with no persisted snapshot,
	// removeUnused's DeleteInstalled/DeleteGraph/DeleteDepsCache calls were all
	// no-ops against st's empty in-memory maps, so there is nothing this save
	// would actually preserve. Saving anyway would not preserve state - it
	// would fabricate a persisted-and-empty snapshot, stamping LastSnapshot
	// for the first time, that the next run reads as positive evidence that
	// nothing is installed or warmed anywhere. Skipping the save leaves the
	// backend's snapshot exactly as untouched as it was before this run.
	if !cfg.DryRun && st.WasPersisted() {
		if err := backend.SaveStore(ctx, st); err != nil {
			return err
		}
	}
	if cfg.DryRun {
		runtime.Output.PersistentPrintf("🫡 Dry-run cleanup complete. Candidates: %d", removed)
		return nil
	}
	runtime.Output.PersistentPrintf("✨ Cleanup complete. Removed: %d", removed)
	return nil
}

// errWorkspaceUnrooted names a project's ansible_collections entry whose
// probe (openProjectWorkspace's root.Stat call) returned a non-nil error
// other than fs.ErrNotExist, as distinct from the entry simply not existing
// or existing as something other than a directory - both ordinary skips,
// neither evidence of an escape. That covers more than os.Root's own
// refusal to resolve an escaping symlink: it also covers any other stat
// failure the probe does not distinguish from one, such as a symlink loop
// (ELOOP) reported directly by the kernel. It never leaves buildReachable:
// openProjectWorkspace's caller renders it into an operator warning and
// moves on, so it carries no exit code and is deliberately not
// helpers.ErrCollectionsPathEscape, which is reserved for install-side
// writes and is mapped to a real exit class in cmd/go-galaxy/exitcode.
var errWorkspaceUnrooted = errors.New("ansible_collections does not resolve inside the project collections path")

// workspace is one project's opened, rooted collections workspace: root and
// fsys are both anchored at the collections path itself, never one level
// down at its ansible_collections subdirectory, because os.OpenRoot follows
// a symlink when establishing the root - rooting at ansible_collections
// would simply adopt whatever it points at, leaving nothing left to refuse.
// This is the same boundary openCollectionsRoot and removeWorkspaceFiles
// already draw on the install and removal sides respectively.
//
// path stays an OS-native absolute string: it becomes an installedCollection's
// CollectionsDir and is what removeWorkspaceFiles later opens its own,
// separate os.Root at. Every path handed to root or fsys, by contrast, is
// slash-separated (built with path.Join, never filepath.Join), matching the
// io/fs convention both APIs use regardless of GOOS.
//
// That handoff is a string, not a live root, and the gap between the two
// opens is real: ws.root itself is closed per project at the end of
// scanProjectWorkspace, well before removeUnused ever runs, while
// removeWorkspaceFiles re-opens its own, separate os.Root from this path
// string alone, only once buildReachable has finished scanning every
// project. os.OpenRoot follows a symlink when establishing a root - the same
// property that lets a symlinked collections path work at all - so a local
// writer able to replace the collections path itself in that window
// redirects both of removeWorkspaceFiles's RemoveAll calls into a tree of
// the writer's own choosing; the deletion stays contained relative to
// whatever that second os.OpenRoot resolves to, but the scan no longer
// controls which tree that turns out to be. This is the same class of
// live-writer residual already accepted for a symlink planted deeper in the
// tree (see removeInstalled's own doc comment below): the attacker already
// needs write access to the collections path to win this race, and gains
// nothing from it that writing there directly would not. Threading ws.root
// through to the removal side instead of re-opening it would trade this
// residual for a wider file-descriptor lifetime and a scan/removal ownership
// split this package does not otherwise have.
type workspace struct {
	root *os.Root
	fsys fs.FS
	path string
}

// maxCollectionsPathCandidates is the number of collections-path candidates
// collectionsPathCandidates can ever produce for one project: the recorded
// CollectionsPath, plus a project-relative ".collections" and "collections"
// fallback.
const maxCollectionsPathCandidates = 3

// collectionsPathCandidates builds the ordered list of collections-path
// candidates for a project: its recorded CollectionsPath first (when set),
// then the project-relative ".collections" and "collections" fallbacks
// (when projectPath is known) - independently of each other, so a project
// can contribute anywhere from zero to three candidates. Which candidate, if
// any, is actually usable is openProjectWorkspace's decision, not this
// function's.
func collectionsPathCandidates(projectPath string, project store.ProjectRecord) []string {
	candidates := make([]string, 0, maxCollectionsPathCandidates)
	if project.CollectionsPath != "" {
		candidates = append(candidates, project.CollectionsPath)
	}
	if projectPath != "" {
		candidates = append(candidates, filepath.Join(projectPath, ".collections"), filepath.Join(projectPath, "collections"))
	}
	return candidates
}

// openProjectWorkspace opens the first usable collections-path candidate for
// a project, rooted at that candidate via os.Root, and returns exactly one of
// three outcomes distinguished without inspecting any error string:
//
//   - (workspace{}, nil): no candidate has an ansible_collections directory -
//     the project has no workspace this run, the ordinary skip case.
//   - (workspace{}, err): a candidate has an ansible_collections entry whose
//     resolution os.Root refuses (errWorkspaceUnrooted) - a symlink escaping
//     that candidate's root, most commonly. The caller must warn and skip the
//     whole project rather than fall through to a later candidate: falling
//     through to, say, a valid sibling ".collections" would silently retarget
//     cleanup at a directory the operator never configured for this project.
//   - (ws, nil): a usable workspace. The caller owns ws.root and must close
//     it once done scanning.
//
// A non-nil statErr other than fs.ErrNotExist - including a symlink loop -
// lands in the escape outcome: os.Root's own refusal is an unexported error
// value matched by no exported sentinel, which is exactly why the split is
// made on the probe's outcome (a non-nil, non-ENOENT statErr, or not) rather
// than by inspecting the error's shape. A successful Stat on an
// ansible_collections entry that turns out not to be a directory is the
// ordinary skip instead, exactly like fs.ErrNotExist: neither is evidence of
// an escape, so this function falls through and tries the next candidate for
// both. This also means the escape is only caught statically, at this
// probe: a local writer that swaps ansible_collections for an escaping
// symlink between this probe and the later directory read
// (scanInstalledCollections) makes that later read fail instead, and that
// failure still aborts the whole run (wrapped by
// scanProjectWorkspace as "failed to scan <path>") rather than being
// downgraded to a skip - separating os.Root's static refusal from a genuine
// IO failure at the read site would require matching that same unexported
// error value, and treating every non-ENOENT read failure as a skip would
// silently swallow real IO errors instead. Aborting on evidence of an active
// local writer is the conservative outcome; the ansible_collections
// misconfiguration this function exists to catch is closed deterministically
// right here. The manifest leaf, further down the same walk, has its own
// static checkpoint for the identical reason (scanCollectionDir's
// manifestIsRegularFile gate) - what still aborts past either checkpoint is
// always a genuine IO failure or a live-writer swap, never a static shape
// either gate was built to catch.
func openProjectWorkspace(projectPath string, project store.ProjectRecord) (workspace, error) {
	for _, candidate := range collectionsPathCandidates(projectPath, project) {
		root, err := os.OpenRoot(candidate)
		if err != nil {
			continue
		}
		info, statErr := root.Stat("ansible_collections")
		switch {
		case statErr == nil && info.IsDir():
			return workspace{root: root, fsys: root.FS(), path: candidate}, nil
		case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
			_ = root.Close()
			return workspace{}, fmt.Errorf("%w: %q: %w", errWorkspaceUnrooted, filepath.Join(candidate, "ansible_collections"), statErr)
		}
		_ = root.Close()
	}
	return workspace{}, nil
}

// scanProjectWorkspace opens projectPath's collections workspace and, if
// usable, scans it into index/byKey/deps. It is buildReachable's phase-1
// per-project step, and is deliberately independent of whether this
// project's own requirements load successfully - see projectRequirementRoots,
// buildReachable's phase-2 step, for that half. Its only signal to the
// caller is success or failure: it does not report whether anything was
// actually scanned, since phase 2 resolves every recorded project's roots
// regardless of what phase 1 found for that same project.
//
//   - No candidate has an ansible_collections directory: nil error, nothing
//     scanned - the ordinary skip, identical to today's absent-workspace
//     behavior.
//   - A candidate's ansible_collections entry escapes its root: a single
//     operator warning naming the project is emitted and nil is returned, so
//     nothing under this project is scanned or removed this run but every
//     other project's cleanup proceeds unaffected. This is phase 1 only: the
//     project's own requirements.yml is still resolved in phase 2
//     (projectRequirementRoots runs for every recorded project regardless of
//     this outcome), so its roots can still keep another project's on-disk
//     copy reachable even though nothing under this project itself was
//     scanned or removed.
//   - A usable workspace fails to scan (a genuine IO error, not an escape):
//     the error is wrapped with the collections path and returned, aborting
//     the whole run. Every path inside the scan is root-relative once ws is
//     in play, so the raw error carries no indication of which project it
//     came from without this wrap.
//
// ws.root is closed before returning in every case, releasing its file
// descriptor before the next project's workspace is opened rather than
// holding one open per project for the whole run.
//
// projectPath always derives from filepath.Dir of a requirements file path
// (store.RecordProject) that can itself sit inside a directory whose name a
// hostile checkout chose - a project subdirectory name is ordinary git tree
// content, not a value this tool ever validates. The collections path
// carries the same exposure whenever it too derives from projectPath - true
// of both scan fallback candidates and of the default, relative
// download-path, though not of an operator-configured absolute one.
//
// Every line this package emits goes through internal/progress, which
// applies safeout.Clean on every tier, so no control character except \n
// and \t reaches a terminal from either the Warnf here or the error wrapped
// further down this function. The %q on projectPath here, and on the path
// in openProjectWorkspace's own wrapped error, additionally escapes \n for
// those two operands specifically, before Clean ever sees it - the same
// overlap reportOutdated's doc (outdated.go) already describes for its own
// rendering. The residual that remains is Clean's own documented one: a
// *fs.PathError surfacing from the scan carries
// ansible_collections/<ns>/<name>/MANIFEST.json with ns and name straight
// off fs.ReadDir, and a \n in either survives Clean, so such a path can
// claim one extra plain-text line - never overwrite one already emitted,
// since \r does not survive. See safeout.Clean's own doc comment for why.
func scanProjectWorkspace(
	out output.Printer,
	projectPath string,
	project store.ProjectRecord,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	ws, err := openProjectWorkspace(projectPath, project)
	if err != nil {
		out.Warnf("skipping project %q: %v; nothing under it was scanned or removed", projectPath, err)
		return nil
	}
	if ws.root == nil {
		return nil
	}
	defer func() { _ = ws.root.Close() }()

	if err := scanInstalledCollections(out, ws, index, byKey, deps); err != nil {
		return fmt.Errorf("failed to scan %q: %w", ws.path, err)
	}
	return nil
}

// scanInstalledCollections indexes installed collections under ws. Installed
// collections only ever live at the fixed
// <ws.path>/ansible_collections/<ns>/<name>/MANIFEST.json depth, so this
// walks exactly those two directory levels rather than the whole tree: a
// MANIFEST.json nested deeper (e.g. inside a collection's own test fixtures)
// is never mistaken for an installed collection, and the scan does not pay
// for descending into every file of every installed collection. Directory
// reads (ansible_collections itself, and each namespace/name listing) go
// through ws.fsys; the manifest file itself is read through ws.root directly
// (scanCollectionDir's call to readManifest). Both are bound by the same
// os.Root openProjectWorkspace established, so a symlink swap planted after
// that root was opened cannot make the scan follow it out of ws.path. Every
// component on this walk is closed by a checkpoint before it is ever opened,
// though the checkpoint's shape differs by how the scan reaches that
// component. ansible_collections is closed by openProjectWorkspace's own
// static probe, which already found it clean before this function ever ran.
// A namespace or a name component is closed by the parent fs.ReadDir call
// that listed it as a real directory: a component that is already a symlink
// at listing time is skipped by !IsDir() and never reaches a rooted read in
// the first place, so openProjectWorkspace never needs to inspect those
// components itself. The manifest leaf is the one component nothing lists on
// the way in - scanCollectionDir reaches it directly by name, not through a
// parent directory listing - so it carries its own static checkpoint
// instead: manifestIsRegularFile's Lstat gate. See scanProjectWorkspace's
// doc comment for what happens when a rooted read fails for a reason other
// than a symlink swap.
//
// Manifests whose namespace/name/version cannot be safely used as filesystem
// path elements are rejected at ingestion (see buildInstalledRecord): a
// warning is emitted via out and the scan continues rather than aborting the
// whole run or indexing the tainted record.
func scanInstalledCollections(
	out output.Printer,
	ws workspace,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	nsEntries, err := fs.ReadDir(ws.fsys, "ansible_collections")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, nsEntry := range nsEntries {
		if !nsEntry.IsDir() {
			continue
		}
		if err := scanNamespaceDir(out, ws, nsEntry.Name(), index, byKey, deps); err != nil {
			return err
		}
	}
	return nil
}

// scanNamespaceDir scans every ansible_collections/<ns>/<name> directory for
// a MANIFEST.json, one namespace at a time.
func scanNamespaceDir(
	out output.Printer,
	ws workspace,
	ns string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	nameEntries, err := fs.ReadDir(ws.fsys, path.Join("ansible_collections", ns))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The namespace directory vanished between the parent ReadDir
			// and this one (e.g. a concurrent cleanup or install run) -
			// skip it rather than aborting the whole scan.
			return nil
		}
		return err
	}
	for _, nameEntry := range nameEntries {
		if !nameEntry.IsDir() {
			continue
		}
		if err := scanCollectionDir(out, ws, ns, nameEntry.Name(), index, byKey, deps); err != nil {
			return err
		}
	}
	return nil
}

// manifestIsRegularFile reports whether ansible_collections/<ns>/<name>/
// MANIFEST.json at rel is a regular file - the only shape scanCollectionDir
// ever treats as an installed collection's manifest. Its error return is the
// raw Lstat error, unwrapped: the caller distinguishes fs.ErrNotExist (no
// MANIFEST.json here, the existing silent skip) from every other Lstat
// failure (a genuine IO error, which aborts) the same way it already
// distinguishes those two outcomes for readManifest's own error below.
//
// IsRegular is deliberately a predicate, not a symlink blocklist: it rejects
// a symlink, a directory named MANIFEST.json, a fifo, a socket, and a device
// in the same check, instead of enumerating shapes one at a time and risking
// a future addition being missed. The fifo case is not academic -
// root.ReadFile opens with O_RDONLY, which blocks on a fifo until a writer
// appears, so a symlink-only blocklist would trade an abort for a hang. Lstat
// (not Stat) is what makes a symlink itself the thing being classified rather
// than whatever it points at.
func manifestIsRegularFile(root *os.Root, rel string) (bool, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// scanCollectionDir probes ansible_collections/<ns>/<name>/MANIFEST.json and,
// if present, regular, and parseable, indexes the installed collection it
// describes.
func scanCollectionDir(
	out output.Printer,
	ws workspace,
	ns, name string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	rel := path.Join("ansible_collections", ns, name, "MANIFEST.json")
	manifestPath := filepath.Join(ws.path, filepath.FromSlash(rel))

	regular, err := manifestIsRegularFile(ws.root, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A <ns>/<name> directory without a MANIFEST.json is not an
			// installed collection - skip it silently rather than aborting.
			return nil
		}
		return err
	}
	if !regular {
		// A MANIFEST.json entry that is not a regular file - a symlink, a
		// directory, a fifo, or anything else - identifies no collection at
		// all: it is neither a reachability source nor a deletion candidate,
		// so it is reported (visible warning) but never opened, and the scan
		// continues rather than aborting.
		out.Warnf("skipping non-regular manifest at %q", manifestPath)
		return nil
	}

	manifest, err := readManifest(ws.root, rel, manifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The entry vanished between the Lstat gate above and this read
			// (e.g. a concurrent cleanup or install run) - skip it silently
			// rather than aborting, the same benign race scanNamespaceDir's
			// own vanished-namespace comment describes.
			//
			// Documented uncovered: reaching this arm requires winning a
			// race between manifestIsRegularFile's Lstat above and this
			// ReadFile - the entry must exist as a regular file at the
			// first syscall and be gone by the second - and this test suite
			// has no seam to force that timing. If this arm were removed, a
			// manifest that vanishes mid-scan would abort the whole run
			// instead of being skipped, which is exactly the "one bad
			// project kills every project" class the rest of this scan is
			// written to avoid.
			return nil
		}
		if errors.Is(err, helpers.ErrCorruptManifest) {
			// A manifest that cannot be parsed identifies no collection at
			// all: it is neither a reachability source nor a deletion
			// candidate, so it is reported (visible warning) but its
			// on-disk tree is left untouched rather than aborting the scan.
			// manifestPath is built from ns/name straight off fs.ReadDir,
			// before either has ever reached an IsPathElement check (that
			// check runs inside buildInstalledRecord, which this branch
			// never calls), so it is rendered %q rather than %s: unlike
			// the identifiers this package prints elsewhere, it is not yet
			// known to be free of a line-forging character.
			out.Warnf("skipping corrupt manifest at %q: %v", manifestPath, err)
			return nil
		}
		return err
	}
	record, key, ok, err := buildInstalledRecord(ws.path, manifestPath, ns, name, manifest)
	if err != nil {
		// manifestPath carries the identical exposure the ErrCorruptManifest
		// branch above documents - built from raw, unvalidated ns/name - and
		// err's own %q rendering of ns/name/version (buildInstalledRecord's
		// own error) does not cover manifestPath, a separately built string.
		// %q for the same reason.
		out.Warnf("skipping install with unsafe identifier at %q: %v", manifestPath, err)
		return nil
	}
	if !ok {
		return nil
	}
	index[record.FQDN] = append(index[record.FQDN], record)
	byKey[key] = append(byKey[key], record)
	deps[key] = extractDeps(manifest)
	return nil
}

// readManifest reads and parses a MANIFEST.json at rel (slash-separated,
// relative to root) through root. A read failure (including fs.ErrNotExist
// for a missing file, which the caller checks for) is returned as-is.
// display is the same location rendered as a plain, OS-native absolute
// string, used only in the helpers.ErrCorruptManifest message: rel alone
// would not tell an operator which project's manifest failed to parse. A
// file that exists but fails to parse as JSON is reported as
// helpers.ErrCorruptManifest wrapping the underlying decode error, rather
// than silently discarding it: the caller decides how to surface that.
func readManifest(root *os.Root, rel, display string) (types.GalaxyCollectionVersionInfoManifest, error) {
	data, err := root.ReadFile(rel)
	if err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, err
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, fmt.Errorf("%w at %s: %w", helpers.ErrCorruptManifest, display, err)
	}
	return manifest, nil
}

// buildInstalledRecord builds an installedCollection from ns and name - the
// ansible_collections/<ns>/<name> directory pair scanCollectionDir just
// walked to reach manifestPath - and the version parsed out of manifest.
// This is the governing identity invariant for this package: a scanned
// collection's namespace and name are always the two path components the
// scan walked through, never a value the manifest declares, so no MANIFEST.json
// content - however it got there - can ever redirect this record, or any
// deletion built from it, at a different collection's directory. ns and name
// are single filesystem path elements by construction, since fs.ReadDir
// never yields an entry containing "/" or a bare "." / "..", so the
// IsPathElement checks below are defensive-only for those two arguments in
// every call this package itself makes; they are kept anyway as
// belt-and-suspenders, and TestBuildInstalledRecordRejectsSeparators is what
// actually exercises that arm, by calling this function directly with values
// the real scan could never produce.
//
// version is the one identity component still read from the manifest:
// nothing else on disk records it. Recovering it from the persisted
// snapshot's InstalledEntry.InstallPath instead was considered and rejected:
// Store.SetInstalled keys by ns.name@version and never prunes an older entry
// on upgrade, so two entries can legitimately share one InstallPath, making
// the reverse (path -> version) lookup ambiguous. The .info sidecar
// directory name was rejected too, as a third source of truth for a value
// the manifest already names.
//
// A lying version's blast radius is bounded to a fixed prefix, not to a
// unique target: ns and name are fixed by the walk before version is ever
// consulted, so the two names a lying version can mistarget are always
// <ns>.<name>-<lying-version>.info (the sidecar) and
// <ns>-<name>-<lying-version>.tar.gz (the artifact filename) - never some
// other collection's own ns/name. But "-" and "." are both legal inside a
// walked path element, which makes both of those concatenations ambiguous:
// walking namespace "a", name "b", with a manifest lying that its version is
// "c-1.0.0" produces the sidecar name "a.b-c-1.0.0.info" and the artifact
// filename "a-b-c-1.0.0.tar.gz" - byte-identical to what a genuinely,
// legitimately installed a.b-c@1.0.0 would itself produce. The impact stays
// narrow regardless: the sidecar RemoveAll failure this could cause is
// already ignored (removeInfoDir is best-effort), and an artifact cache-slot
// eviction self-heals on the next refetch. Containment is unaffected either
// way - removeInstallPath joins only Namespace and Name, so the version
// never participates in a directory path at all.
//
// An incomplete manifest (missing namespace, name, or version) is a benign
// skip: ok is false and err is nil. A manifest whose namespace, name, or
// version cannot be safely used as a single filesystem path element (e.g. it
// contains "/" or is "..") is rejected with
// helpers.ErrUnsafeCollectionIdentifier rather than silently building a
// record whose Version would later escape the collections tree in
// removeInstalled.
func buildInstalledRecord(
	collectionsPath string,
	manifestPath string,
	ns, name string,
	manifest types.GalaxyCollectionVersionInfoManifest,
) (installedCollection, string, bool, error) {
	version := manifest.CollectionInfo.Version
	if ns == "" || name == "" || version == "" {
		return installedCollection{}, "", false, nil
	}
	if !helpers.IsPathElement(ns) || !helpers.IsPathElement(name) || !helpers.IsPathElement(version) {
		return installedCollection{}, "", false, fmt.Errorf(
			"%w: ns=%q name=%q version=%q", helpers.ErrUnsafeCollectionIdentifier, ns, name, version,
		)
	}
	installPath := filepath.Dir(manifestPath)
	key := fmt.Sprintf("%s.%s@%s", ns, name, version)
	fqdn := fmt.Sprintf("%s.%s", ns, name)
	// A parse failure here is not this function's concern to reject: an
	// identifier that passed the path-element safety check above can still
	// be non-semver (e.g. a git ref), and selectInstalled's existing
	// unparseable-version handling (skip the item under a real constraint)
	// is preserved by simply caching nil in that case.
	parsed, _ := semver.NewVersion(version)
	return installedCollection{
		Key:            key,
		FQDN:           fqdn,
		Namespace:      ns,
		Name:           name,
		Version:        version,
		InstallPath:    installPath,
		CollectionsDir: collectionsPath,
		Parsed:         parsed,
	}, key, true, nil
}

func extractDeps(manifest types.GalaxyCollectionVersionInfoManifest) map[string]string {
	if manifest.CollectionInfo.Dependencies != nil {
		return manifest.CollectionInfo.Dependencies
	}
	return map[string]string{}
}

// selectInstalled filters installed collections by constraint. constraints
// caches each raw constraint string's parsed *semver.Constraints (or nil for
// an unparseable one) across the whole BFS, so the same requirement string
// evaluated against many edges is parsed at most once. A cache miss is
// distinguished from a cached-nil (parse failure) via the two-value map
// read, so an unparseable constraint is itself parsed only once too.
func selectInstalled(
	index map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
	fqdn, constraint string,
) []installedCollection {
	items := index[fqdn]
	if len(items) == 0 {
		return nil
	}
	normalized := helpers.NormalizeConstraint(constraint)
	if normalized == "" {
		return items
	}
	c, ok := constraints[normalized]
	if !ok {
		c, _ = semver.NewConstraint(normalized)
		constraints[normalized] = c
	}
	if c == nil {
		return items
	}
	out := make([]installedCollection, 0, len(items))
	for _, item := range items {
		if item.Parsed == nil {
			continue
		}
		if c.Check(item.Parsed) {
			out = append(out, item)
		}
	}
	return out
}

// markReachable marks all reachable dependencies starting at key.
func markReachable(
	key string,
	reachable map[string]bool,
	deps map[string]map[string]string,
	index map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
) {
	queue := []string{key}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if reachable[current] {
			continue
		}
		reachable[current] = true
		for depFQDN, constraint := range deps[current] {
			for _, inst := range selectInstalled(index, constraints, depFQDN, constraint) {
				if !reachable[inst.Key] {
					queue = append(queue, inst.Key)
				}
			}
		}
	}
}

// removeInstalled deletes collection files and cached artifacts.
//
// namespace and name below are read directly from inst.Namespace/inst.Name -
// the ansible_collections/<ns>/<name> directory pair the scan walked through
// to find this record's manifest, never anything the manifest itself
// declared - so nothing a hostile MANIFEST.json contains can retarget this
// removal at a different collection's directory.
//
// The WithinDir checks in removeInstallPath/removeInfoDir are
// defense-in-depth: buildInstalledRecord already rejects any
// namespace/name/version that cannot be safely used as a path element at
// ingestion, so these containment failures should never trigger on a record
// built through the normal scan. If they do, the in-memory state is
// anomalous (e.g. constructed directly rather than scanned), and the safest
// action is to abort rather than guess which part of the path is untrusted.
//
// The load-bearing escape prevention is os.Root, opened in
// removeWorkspaceFiles: both on-disk deletions are rooted at
// inst.CollectionsDir (the operator-configured, trusted collections_path)
// rather than at its ansible_collections subdirectory, so a local attacker
// who swaps either the ansible_collections directory or the namespace
// component for a symlink between the scan and this call cannot make either
// RemoveAll follow it out of the tree - os.Root refuses to traverse a
// symlink that escapes its root, closing a TOCTOU that a purely lexical
// WithinDir check cannot catch. A symlink placed deeper - e.g. redirecting
// the collection's own <ns>/<name> directory to another location still
// inside collections_path - is not defended here: the attacker already needs
// workspace write access to plant it, and could just as easily delete that
// same target directly, so os.Root's guarantee (no escape outside the root)
// is the property that actually matters.
//
// The artifact purge runs after removeWorkspaceFiles regardless of whether
// the workspace itself was present: the artifact store is independent of the
// on-disk collections tree, so an absent (or already-swept) workspace must
// not gate it - otherwise a cached tarball for an unreachable collection
// would leak whenever its project's workspace happens to be gone this run.
//
// source is the server this collection actually resolved from (the
// persisted InstalledEntry's own Source, via installedSource) - possibly ""
// when unknown - and is used to purge its current, server-scoped artifact
// cache entry (helpers.ArtifactKey). It does not purge the pre-multi-server
// flat-keyed entry (legacyArtifactKey): that purge runs unconditionally for
// every scanned collection, reachable or not, via sweepLegacyArtifacts,
// since a legacy-keyed entry can never be reached by a fresh install again
// regardless of whether its collection is being removed.
func removeInstalled(ctx context.Context, inst installedCollection, artifacts cacheManager.ArtifactStore, source string) error {
	namespace := inst.Namespace
	name := inst.Name
	if !helpers.IsPathElement(namespace) || !helpers.IsPathElement(name) || !helpers.IsPathElement(inst.Version) {
		return fmt.Errorf("%w: ns=%q name=%q version=%q", helpers.ErrUnsafeRemovalPath, namespace, name, inst.Version)
	}

	if err := removeWorkspaceFiles(inst, namespace, name); err != nil {
		return err
	}

	if artifacts != nil && strings.TrimSpace(source) != "" {
		filename := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, inst.Version)
		_ = artifacts.Delete(ctx, helpers.ArtifactKey(source, filename))
	}
	return nil
}

// removeWorkspaceFiles removes the collection's on-disk install directory
// and .info sidecar, both rooted at inst.CollectionsDir via a single
// os.Root (see removeInstalled's doc comment for why that root prevents a
// symlink-swap escape). namespace and name are the already-validated path
// elements from removeInstalled's caller.
func removeWorkspaceFiles(inst installedCollection, namespace, name string) error {
	root, err := os.OpenRoot(inst.CollectionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No on-disk workspace files to remove: the collections dir this
			// record refers to is already gone. The caller still proceeds to
			// purge the cached artifact - that store is independent of this
			// workspace's presence.
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()

	if err := removeInstallPath(root, inst, namespace, name); err != nil {
		return err
	}
	return removeInfoDir(root, inst, namespace, name)
}

// removeInstallPath removes the collection's own install directory
// (ansible_collections/<namespace>/<name>) through root, rooted at
// inst.CollectionsDir, so a symlink swap of either the ansible_collections
// directory or the namespace component cannot make the removal escape the
// collections tree. namespace and name are the already-validated path
// elements from removeInstalled's caller - the same ansible_collections/<ns>/
// <name> pair buildInstalledRecord read inst.InstallPath from in the first
// place (installPath := filepath.Dir(manifestPath), where manifestPath is
// exactly ansible_collections/<ns>/<name>/MANIFEST.json). The walked identity
// and the recorded InstallPath name the identical on-disk location by
// construction, not two independently-derived values that merely happen to
// agree.
//
// The InstallPath == "" and WithinDir guards below are belt-and-suspenders
// over that construction, not the load-bearing check: they exist for a
// record that never went through the normal scan at all - one built
// directly, as several tests in this package do - where InstallPath could
// disagree with namespace/name.
// TestRemoveInstalledRejectsInstallPathEscape pins exactly that
// directly-constructed case: the last lexical check before a RemoveAll that
// a record produced by the real scan can never actually trigger.
func removeInstallPath(root *os.Root, inst installedCollection, namespace, name string) error {
	if inst.InstallPath == "" {
		return nil
	}
	if !helpers.WithinDir(inst.CollectionsDir, inst.InstallPath) {
		return fmt.Errorf("%w: install path %q escapes %q", helpers.ErrUnsafeRemovalPath, inst.InstallPath, inst.CollectionsDir)
	}
	installRel := filepath.Join("ansible_collections", namespace, name)
	return root.RemoveAll(installRel)
}

// removeInfoDir best-effort removes the collection's .info sidecar
// directory (ansible_collections/<namespace>.<name>-<version>.info) through
// root, the same way removeInstallPath does. Only the WithinDir containment
// failure is surfaced as an error; a RemoveAll failure itself is ignored,
// preserving this call's pre-existing best-effort semantics.
func removeInfoDir(root *os.Root, inst installedCollection, namespace, name string) error {
	acRoot := filepath.Join(inst.CollectionsDir, "ansible_collections")
	infoName := fmt.Sprintf("%s.%s-%s.info", namespace, name, inst.Version)
	infoDir := filepath.Join(acRoot, infoName)
	// Unreachable by construction (belt-and-suspenders): namespace, name, and
	// inst.Version were already IsPathElement-checked by removeInstalled, so
	// infoDir is always a clean single element under acRoot; kept because a
	// future refactor breaking that containment right before RemoveAll would
	// be catastrophic.
	if !helpers.WithinDir(acRoot, infoDir) {
		return fmt.Errorf("%w: info dir %q escapes %q", helpers.ErrUnsafeRemovalPath, infoDir, acRoot)
	}
	_ = root.RemoveAll(filepath.Join("ansible_collections", infoName))
	return nil
}

// legacyArtifactKey builds the pre-multi-server-scoped artifact cache key:
// the flat, percent-encoded-filename-only shape every artifactKey in this
// codebase used to build before it gained a server-fingerprint prefix (see
// helpers.ArtifactKey). No code path ever builds this shape for a fresh
// install anymore - collections.artifactKey and this package's own current
// key both go through helpers.ArtifactKey now - so a cache entry still
// living under this key can never again be reached by a cache-hit lookup,
// regardless of whether its collection is still reachable. sweepLegacyArtifacts
// is the only remaining caller, purging it unconditionally as a one-time
// migration cleanup rather than leaving it as permanent orphaned disk usage.
func legacyArtifactKey(namespace, name, version string) string {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
	return url.QueryEscape(filename)
}

// sweepLegacyArtifacts removes every discovered installed collection's
// artifact cached under the pre-multi-server flat key shape
// (legacyArtifactKey), independent of reachability: a still-reachable
// collection keeps its workspace and its current, server-scoped artifact
// cache entry (helpers.ArtifactKey) untouched, but any copy still cached
// under the retired flat key is unconditionally dead weight, since nothing
// will ever look it up again. removeUnused's own artifact purge only ever
// runs for a collection it is also removing, so without this separate pass a
// still-reachable collection's legacy-keyed tarball would never be reclaimed
// by any code path. In a dry run this only reports candidates that actually
// exist on disk, matching sweepExtractedStore's own dry-run reporting
// convention.
func sweepLegacyArtifacts(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	installedByKey map[string][]installedCollection,
) {
	artifacts := backend.Artifacts()
	if artifacts == nil {
		return
	}
	// Sorted rather than ranged directly, mirroring removeUnused's own
	// ordering fix: a Delete failure is ignored below rather than propagated,
	// so map order cannot randomize which entries a completed pass sweeps -
	// only the order dry-run report lines are printed in, and the order a
	// test fixture asserting on those lines would otherwise have to tolerate.
	// A pass cut short by the context check below stops on a prefix of that
	// same order rather than on an arbitrary subset.
	for _, mapKey := range slices.Sorted(maps.Keys(installedByKey)) {
		// Stopping silently suffices for a pass that is void by design: on
		// the common path the caller has already returned an error
		// (cleanupWithState reads the same context immediately before
		// calling this), and a context that ends inside this loop instead is
		// still judged once the work returns - runCleanup hands
		// cleanupWithState's outcome to cacheManager.LockLostError, which
		// turns even a nil into the lock-loss verdict whenever the holder
		// context ended because another holder took the cache. A plain
		// cancellation costs at most a deferred purge: what this pass
		// reclaims is disk under a key shape nothing can look up again,
		// never correctness.
		if ctx.Err() != nil {
			return
		}
		insts := installedByKey[mapKey]
		if len(insts) == 0 {
			continue
		}
		// Every on-disk copy of the same key shares the same namespace/name/
		// version, and therefore the same legacy key, regardless of which
		// project installed it - so only the first copy needs inspecting.
		inst := insts[0]
		legacyKey := legacyArtifactKey(inst.Namespace, inst.Name, inst.Version)
		// A walked namespace directory may legally contain "." (nothing in
		// this codebase's scan rejects it - see buildInstalledRecord's
		// IsPathElement check, which permits it), and legacyArtifactKey's
		// url.QueryEscape leaves both "." and "-" unescaped. A namespace like
		// "<12-hex>.acme" therefore forges a legacyArtifactKey byte-identical
		// to a genuine, current helpers.ArtifactKey entry scoped to a server
		// whose fingerprint happens to be that same 12-hex prefix - a
		// collision this pass has no reachability check or Source
		// requirement to catch, since sweepLegacyArtifacts runs
		// unconditionally over every scanned collection. Skipping any
		// candidate that already carries ArtifactKey's own fingerprint prefix
		// closes that: this pass only ever purges a key that could not also
		// be a live, server-scoped cache slot.
		if helpers.IsScopedArtifactKey(legacyKey) {
			continue
		}
		if cfg.DryRun {
			reportLegacyArtifactSweepCandidate(ctx, runtime, artifacts, legacyKey)
			continue
		}
		_ = artifacts.Delete(ctx, legacyKey)
	}
}

// reportLegacyArtifactSweepCandidate prints a dry-run sweep line for key only
// when it actually exists, so a dry-run report is not flooded with a line for
// every installed collection regardless of whether it ever had a
// legacy-keyed cache entry in the first place. A Has error is treated as "not
// present" - conservative for a report that must never claim more than it
// can verify.
func reportLegacyArtifactSweepCandidate(ctx context.Context, runtime *infra.Infra, artifacts cacheManager.ArtifactStore, key string) {
	has, err := artifacts.Has(ctx, key)
	if err != nil || !has {
		return
	}
	// key needs no quoting here: legacyArtifactKey builds it via
	// url.QueryEscape(filename), which percent-encodes every control byte
	// (measured: url.QueryEscape("a\nb.tar.gz") == "a%0Ab.tar.gz") on top of
	// namespace/name/version already being IsPathElement-validated by
	// buildInstalledRecord before a key is ever built from them. A future
	// author must not "fix" this into %q to match the extracted-entry line
	// below: that line's name comes from a raw directory listing with no
	// escaping step of its own, which is exactly what makes it different.
	runtime.Output.Printf("🧹 would sweep legacy artifact %s", key)
}

// sweepExtractedStore drops content-addressable extracted entries whose SHA
// is not referenced by any entry in the persisted snapshot's Installed set,
// nor by a still-fresh entry in its Warmed set. The snapshot - not an on-disk
// workspace scan - is the correct source of truth here: in a real run,
// removeUnused has already pruned it down to installed entries that are
// either still reachable or belong to a project whose workspace was absent
// this run (and so was never scanned or pruned at all). An on-disk scan
// would see an empty keep set for every absent workspace, which is the
// normal ephemeral-CI state, and would wipe the entire extracted cache. A
// warmed entry is the only evidence a warm-only machine - no project
// workspace exists at all, so scanProjectWorkspace skips the project entirely
// and it never contributes to installedByKey/reachable either - still wants
// its extracted trees; it expires purely by age (helpers.WarmedEntryMaxAge).
//
// In a dry run, removeUnused does not prune the snapshot (it only reports
// what it would remove), so the snapshot still contains the about-to-be-
// removed keys. To report the sweep accurately, their SHAs are excluded from
// keep here via reachable/installedByKey - the same two values removeUnused
// used to decide what it would remove - so the reported plan matches what a
// real run would actually do. This exclusion is a no-op in a real run, since
// those keys are already absent from the snapshot by the time this runs.
//
// The !st.WasPersisted() guard lives here, inside the function, rather than
// at the Start call site, so that a future second caller of sweepExtractedStore
// inherits it automatically instead of having to remember to repeat it. It
// runs before the cfg.DryRun branch so a dry run reports no sweep candidates
// that a real run would not act on either. Without it, an absent workspace
// plus a never-loaded (or dropped/expired) snapshot hands this function an
// empty keep set indistinguishable from "nothing is installed or warmed
// anywhere", which would wipe the entire extracted store on the strength of
// having no evidence at all.
func sweepExtractedStore(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) {
	if cfg == nil || cfg.CacheDir == "" || st == nil || !st.HasRecordedContent() {
		return
	}
	extractedStore := extracted.NewStore(cfg.CacheDir)
	if extractedStore == nil {
		return
	}
	keep := extractedKeepSet(st, reachable, installedByKey)

	if cfg.DryRun {
		reportExtractedSweepPlan(runtime, extractedStore, keep)
		return
	}
	// Reported, not discarded, and still not fatal. The sweep reclaims disk in
	// a rebuildable layer, so one unreadable entry must not fail a cleanup run
	// that has already done its real work; but the same return also carries
	// the containment root's refusal when the store directory itself leads out
	// of the cache directory, and ctx's own error when this run stopped owning
	// the cache partway through the entries - and silence for either would
	// leave an operator with a run that reports success while reclaiming
	// nothing.
	if err := extractedStore.Sweep(ctx, keep); err != nil {
		runtime.Output.Errorf("failed to sweep the extracted cache: %v", err)
	}
}

// extractedKeepSet builds the set of extracted-store SHAs to keep: installed-
// and-still-referenced union warmed-and-still-fresh. The installed half
// excludes any key that removeUnused would remove (or already removed, in a
// real run) so a dry-run report matches what a real run would actually
// sweep. The warmed half is unioned in unconditionally: it is deliberately not
// subject to the installed half's wouldRemove exclusion, since a key that is
// both installed-unreachable and warmed must still keep its tree - warm's
// intent is independent of install reachability. The two loops below are pure
// additions to the same set, so their relative order is irrelevant; what is
// load-bearing is that the warmed loop never consults wouldRemove.
func extractedKeepSet(
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) map[string]bool {
	wouldRemove := make(map[string]bool, len(installedByKey))
	for key := range installedByKey {
		if !reachable[key] {
			wouldRemove[key] = true
		}
	}

	shaByKey := st.InstalledArtifactSHAByKey()
	warmedByKey := st.WarmedArtifactSHAByKey()
	keep := make(map[string]bool, len(shaByKey)+len(warmedByKey))
	for key, sha := range shaByKey {
		if wouldRemove[key] {
			continue
		}
		keep[sha] = true
	}
	for _, sha := range warmedByKey {
		keep[sha] = true
	}
	return keep
}

// reportExtractedSweepPlan prints, without deleting anything, the extracted
// entries a real run would sweep given keep.
func reportExtractedSweepPlan(runtime *infra.Infra, extractedStore *extracted.Store, keep map[string]bool) {
	plan, err := extractedStore.SweepPlan(keep)
	if err != nil {
		runtime.Output.Errorf("failed to plan extracted cache sweep: %v", err)
		return
	}
	for _, name := range plan {
		// name is entry.Name() from a raw directory listing of the extracted
		// store root (extracted.Store.SweepPlan) - content this program did
		// not itself validate, unlike a legacy artifact key (see
		// reportLegacyArtifactSweepCandidate) or an install key (see
		// removeUnused), both of which are assembled solely from
		// IsPathElement-validated components. A local writer able to plant a
		// directory there controls this string outright, so it is rendered
		// %q rather than %s.
		runtime.Output.Printf("🧹 would sweep extracted %q", name)
	}
}
