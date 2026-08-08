package collections

import (
	"context"
	"fmt"
	"os"
	"time"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

type installState struct {
	backend      cacheManager.Backend
	store        *store.Store
	release      func() error
	extractStore *extracted.Store
}

type installPlan struct {
	collections map[string]collection
	graph       map[string][]string
	prefetch    *prefetcher
	levels      [][]string
}

// initInstall is the single function every collection command that can be in
// dry-run mode must pass through, so emitting dryRunBanner here makes "no
// command is in dry-run mode silently" a structural property rather than a
// per-call-site convention - install, warm, and lock all reach the banner
// through this one call site, with nothing per-command to remember. The
// --refresh/--offline warning right below sits on the identical argument:
// --offline outranks --refresh (see refreshBypassesSnapshot's own doc
// comment for why), and emitting the disclosure from this one funnel is what
// makes "no run silently drops a flag" structural here too, rather than a
// convention install/warm/lock would each have to remember on their own.
//
// The returned context is the backend's HOLDER CONTEXT (see
// cacheManager.Backend's own Lock contract): every step below that runs after
// the lock is taken uses it rather than ctx, and it is returned on the
// post-lock failure paths too, not only on success. That matters because the
// window between acquiring the lock and returning covers sweepDeadRunTemps,
// LoadStore, clearCacheIfRequested and recordProjectUnlessDryRun - a
// --clear-cache bulk delete against a large bucket is exactly the kind of
// work long enough for a heartbeat tick to land inside it - so discarding the
// holder context there would report a failure CAUSED by the lock being stolen
// as an ordinary backend failure. backend.Open runs before the lock exists
// and backend.Close must still run after ownership is gone, so both keep the
// caller's own ctx.
func initInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (context.Context, *installState, error) {
	if cfg.DryRun {
		dryRunBanner(runtime)
	}
	// This is not a usage error: --refresh (GO_GALAXY_REFRESH) and --offline
	// (GO_GALAXY_OFFLINE) both realistically arrive from an ambient CI
	// environment block, and failing an otherwise-correct offline run over a
	// flag combination that resolves unambiguously is a worse trade than one
	// line on stderr. The condition below reads --refresh and --offline only,
	// deliberately never cfg.Frozen, because --frozen --refresh (without
	// --offline) must never warn here, for two different reasons depending on
	// which command it reaches: on install/warm, resolveOrLoadLockfile takes
	// the cfg.Frozen branch straight to the lockfile and never calls
	// resolveCollectionsInternal, so --refresh truly has nothing to affect
	// there - not a case this warning needs to disclose, since nothing is
	// being silently dropped. On lock, lockWithState always calls
	// resolveCollectionsInternal regardless of cfg.Frozen, so --refresh keeps
	// vetoing the resolve snapshot exactly as it does unfrozen - warning
	// "skipping --refresh" there would be an outright false statement about
	// what lock --frozen --refresh does.
	if cfg.Refresh && cfg.Offline {
		runtime.Output.Warnf("--offline: skipping --refresh; cached state is the only source of truth offline")
	}
	runtime.Output.Printf("🚀 init cache backend")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		return nil, nil, err
	}
	// Every persisted cache-state operation this run makes (LoadStore and
	// SaveStore below, plus RecordProject inside recordProjectUnlessDryRun)
	// runs under its own bounded budget from here on: both happen after the
	// exclusive lock below is acquired, and a stalled read or write there
	// would otherwise hold that lock - and block every other runner sharing
	// this backend - for as long as the caller's own context allows. See
	// cacheManager.WithStateDeadline's own doc comment for why this wraps the
	// backend once here rather than at each of its several call sites.
	//
	// WithCleanSaveSkip wraps outermost, so a save this run never needed
	// skips before WithStateDeadline would even construct a timer for it: see
	// its own doc comment (internal/galaxy/cache/dirtyskip.go) for what it
	// decides and what it deliberately leaves alone.
	backend = cacheManager.WithCleanSaveSkip(cacheManager.WithStateDeadline(backend, runtime.StateDeadline()))
	if err := backend.Open(ctx); err != nil {
		return nil, nil, err
	}
	lockCtx, releaseLock, err := backend.Lock(ctx)
	if err != nil {
		_ = backend.Close(ctx)
		return nil, nil, err
	}

	extractStore := newExtractStore(cfg)
	// The exclusive backend lock is held now, so no other go-galaxy process can
	// be mid-write: every leftover download-temp and extract-temp is a dead-run
	// orphan and is safe to reclaim.
	sweepDeadRunTemps(lockCtx, runtime, backend, extractStore)

	snapshotStart := time.Now()
	runtime.Output.Printf("🚀 load storage")
	st, err := backend.LoadStore(lockCtx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return lockCtx, nil, err
	}
	runtime.Output.DebugSincef(snapshotStart, "%s", "load snapshot")
	if err := clearCacheIfRequested(lockCtx, cfg, runtime, backend, st); err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return lockCtx, nil, err
	}
	recordProjectUnlessDryRun(lockCtx, cfg, runtime, backend)

	return lockCtx, &installState{
		backend:      backend,
		store:        st,
		release:      releaseLock,
		extractStore: extractStore,
	}, nil
}

// clearCacheIfRequested honors --clear-cache by wiping the in-memory caches
// and the cached artifact files on disk, unless a dry run is in effect:
// --clear-cache is a destructive mutation, so a dry run must never honor it,
// on top of everything else --dry-run already suppresses. Factored out of
// initInstall to keep its own branching under the cyclomatic complexity
// budget.
func clearCacheIfRequested(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
) error {
	if !cfg.ClearCache {
		return nil
	}
	if cfg.DryRun {
		runtime.Output.Warnf("--dry-run: skipping --clear-cache")
		return nil
	}
	st.ClearCaches()
	return backend.ClearFiles(ctx)
}

// recordProjectUnlessDryRun records this project in the persistent project
// registry, unless a dry run is in effect. initInstall is shared by install,
// warm, and lock, so this guard applies identically to all three - not just
// to install. RecordProject is the only persistent, non-cache,
// non-reconstructible write in initInstall, and it feeds a DESTRUCTIVE
// command: cleanup walks every registered project's requirements.yml and
// aborts its whole run if one is unreadable or unparseable. `install
// --dry-run -r broken.yml` enrolling that project would then abort every
// cleanup run on every machine sharing this cache, for a project that never
// actually installed anything. ProjectRecord.LastRun is read by nobody, so
// there is no cost to simply not writing it here. Factored out of initInstall
// to keep its own branching under the cyclomatic complexity budget.
func recordProjectUnlessDryRun(ctx context.Context, cfg *config.Config, runtime *infra.Infra, backend cacheManager.Backend) {
	if cfg.DryRun {
		return
	}
	if err := backend.RecordProject(ctx, cfg.RequirementsFile, cfg.DownloadPath); err != nil {
		runtime.Output.Printf("⚠️ Failed to record project: %v", err)
	}
}

// newExtractStore returns a content-addressable extraction store, or nil
// when caching is disabled or no cache directory is configured.
func newExtractStore(cfg *config.Config) *extracted.Store {
	if cfg == nil || cfg.NoCache {
		return nil
	}
	return extracted.NewStore(cfg.CacheDir)
}

// sweepDeadRunTemps reclaims temporary files and directories left behind by a
// previously killed run, safe to delete because the caller holds the backend's
// exclusive lock. It is best-effort: each failure is logged and the install
// proceeds, since leaked temp space is not worth failing an otherwise-valid
// install over.
//
// This runs unconditionally, even under --dry-run, unlike clearCacheIfRequested
// - deliberately, not by oversight. What it deletes is provably a dead-run
// orphan (the exclusive lock rules out any live writer), never something a
// later read treats as an assertion about reality, so it is not the kind of
// write --dry-run exists to suppress in the first place.
func sweepDeadRunTemps(ctx context.Context, runtime *infra.Infra, backend cacheManager.Backend, extractStore *extracted.Store) {
	if err := backend.SweepTemp(ctx); err != nil {
		runtime.Output.Printf("⚠️ Failed to sweep leftover download temps: %v", err)
	}
	if err := extractStore.SweepTemp(); err != nil {
		runtime.Output.Printf("⚠️ Failed to sweep leftover extract temps: %v", err)
	}
}

func prepareInstallPlan(
	ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, root *os.Root,
) (*installPlan, error) {
	prep, err := loadRoots(cfg, runtime)
	if err != nil {
		return nil, err
	}

	resolved, graph, err := resolveOrLoadLockfile(ctx, cfg, runtime, state, prep)
	if err != nil {
		return nil, err
	}

	collections, err := buildCollectionsMap(resolved)
	if err != nil {
		return nil, err
	}

	if err := verifyRootsResolved(prep, resolved); err != nil {
		return nil, err
	}

	// Compute install levels before scheduling the prefetcher: a level-build
	// failure (a dependency cycle) now surfaces before any prefetch worker
	// exists, and the level assignment lets the prefetch queue be ordered to
	// match the level-ordered install consumer.
	levelStart := time.Now()
	levels, err := buildInstallLevels(graph)
	if err != nil {
		return nil, err
	}
	runtime.Output.DebugSincef(levelStart, "%s", "build install levels")

	prefetchStart := time.Now()
	prefetch := startPrefetcher(
		ctx,
		newPrefetchDeps(cfg, runtime, state.store, state.backend.Artifacts(), root),
		collections,
		levels,
	)
	runtime.Output.DebugSincef(prefetchStart, "%s", "prefetch schedule")

	return &installPlan{
		collections: collections,
		graph:       graph,
		levels:      levels,
		prefetch:    prefetch,
	}, nil
}

// resolveOrLoadLockfile chooses between lockfile-driven resolution (when
// --frozen is set) and the regular API-based resolver. With --frozen, the
// lockfile is the source of truth: resolved/graph are built from its
// entries with no network calls.
func resolveOrLoadLockfile(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	prep *rootPreparation,
) (map[string]collection, map[string][]string, error) {
	if cfg.Frozen {
		path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
		runtime.Output.Printf("🔒 frozen: using lockfile %s", path)
		lf, err := lockfile.LoadRequired(path)
		if err != nil {
			return nil, nil, err
		}
		return resolveFromLockfile(cfg, lf, prep)
	}

	resolveStart := time.Now()
	runtime.Output.Printf("🧩 resolve dependencies")
	resolved, graph, err := resolveCollectionsInternal(
		ctx,
		newCollectionDeps(cfg, runtime, state.store),
		prep.AllRoots,
		true,
		true,
	)
	if err != nil {
		return nil, nil, annotateOfflineConflict(cfg, fmt.Errorf("failed to resolve dependencies: %w", err))
	}
	runtime.Output.DebugSincef(resolveStart, "%s", "resolve dependencies")
	return resolved, graph, nil
}

// loadRoots parses requirements.yml and normalizes its entries into roots.
// A collection with no explicit source: field is passed through with an
// empty Source ("" for defaultSource below), deliberately not defaulted to
// cfg.Server here: an unpinned root walks the whole configured server list
// at resolve time (see serverCandidates) instead of being nailed to one.
func loadRoots(cfg *config.Config, runtime *infra.Infra) (*rootPreparation, error) {
	runtime.Output.Printf("🗂️ load collections from requirements file")
	collectionsDirect, rolesFound, err := loadRequirements(cfg.RequirementsFile, "")
	if err != nil {
		return nil, fmt.Errorf("failed to load requirements file: %w", err)
	}
	if rolesFound {
		runtime.Output.Printf("⚠️ requirements.yml contains roles, but roles are not supported.")
	}
	runtime.Output.Printf("🧩 prepare roots")
	prep, err := prepareRoots(collectionsDirect)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare requirements: %w", err)
	}
	return prep, nil
}

// buildCollectionsMap folds the resolved requirements into a key-addressed
// map, rejecting three failure shapes before any install work starts: an
// unsafe namespace/name identifier (ErrUnsafeCollectionIdentifier), a version
// that is not helpers.IsExactVersion (ErrInvalidCollectionVersion), and a
// duplicate key (ErrDuplicateCollectionKey). The namespace/name guard exists
// because helpers.SplitFQDN performs no path-safety validation of its own -
// see TestSplitFQDNDoesNotValidatePathSafety in helpers - so a namespace or
// name like "foo/../.." reaches here unvalidated from every caller that only
// checked it parses as a two-part FQDN. newInstallTarget validates all three
// components again, later, per collection - keeping this guard here as well
// is deliberate, matching the pattern writeExtractMarker's own doc comment
// already documents for this file: a caller's check does not make a callee's
// own guard redundant.
//
// The version check is IsExactVersion rather than a second IsPathElement
// call, replacing it rather than stacking alongside it:
// TestIsExactVersionImpliesIsPathElementExhaustive (internal/galaxy/helpers)
// proves every value IsExactVersion accepts also satisfies IsPathElement -
// exhaustively over an alphabet covering every character class either
// vendored semver grammar treats specially, under both settings of
// semver.CoerceNewVersion, not merely a handful of sampled fixtures - so an
// IsPathElement check on a version that already passed IsExactVersion could
// never fire; keeping it would only cost a redundant call, never add
// coverage. Rejecting it under its own sentinel rather than folding it into
// ErrUnsafeCollectionIdentifier also gives a poisoned or lockfile-sourced
// constraint string like "*" its own classification instead of reading as a
// path-traversal attempt, which it is not: it is syntactically safe as a
// path element and still not a version anything could install.
func buildCollectionsMap(resolved map[string]collection) (map[string]collection, error) {
	collections := make(map[string]collection, len(resolved))
	for _, col := range resolved {
		if !helpers.IsPathElement(col.Namespace) || !helpers.IsPathElement(col.Name) {
			// version is reported here as identity context - which collection
			// this is - never as a checked field: this branch's own condition
			// never looks at col.Version, so an unsafe namespace or name is
			// what triggers it regardless of what the version says.
			return nil, fmt.Errorf("%w: ns=%q name=%q version=%q",
				helpers.ErrUnsafeCollectionIdentifier, col.Namespace, col.Name, col.Version)
		}
		if !helpers.IsExactVersion(col.Version) {
			return nil, fmt.Errorf("%w: %s.%s: %q", helpers.ErrInvalidCollectionVersion, col.Namespace, col.Name, col.Version)
		}
		key := col.key()
		if _, ok := collections[key]; ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrDuplicateCollectionKey, key)
		}
		collections[key] = col
	}
	return collections, nil
}

// verifyRootsResolved is a post-condition on resolution: it asserts that
// every requirements root came back with a resolved version, returning
// helpers.ErrMissingResolvedRoot naming the first one that did not.
//
// This check is redundant on two of the three resolution paths -
// verifyRootsAgainstLockfile plus materializeLockfile already guarantee it
// under --frozen, and rootsMatchSnapshot guarantees it on the snapshot-reuse
// path - but it is load-bearing on the third: solverResultToResolvedGraph
// builds resolved purely from result.Versions and never cross-checks it
// against the requirements, so this is the only place a solver that silently
// drops a root is caught on a fresh solve.
func verifyRootsResolved(prep *rootPreparation, resolved map[string]collection) error {
	for _, col := range prep.AllRoots {
		fqdn := fmt.Sprintf("%s.%s", col.Namespace, col.Name)
		if _, ok := resolved[fqdn]; !ok {
			return fmt.Errorf("%w: %s", helpers.ErrMissingResolvedRoot, fqdn)
		}
	}
	return nil
}

// saveDryRunSnapshotIfPersisted saves state.store only when it was already
// persisted before this run - see installDryRun's own doc comment for why an
// unpersisted store must not be saved here. The warning is this guard's only
// output; the save path itself (SaveStore, or a save failure) speaks for
// itself the same way it does on every other command.
func saveDryRunSnapshotIfPersisted(ctx context.Context, runtime *infra.Infra, state *installState) error {
	if !state.store.WasPersisted() {
		runtime.Output.Warnf(
			"no persisted snapshot was found; a dry run will not create one, so the metadata caches this run built are discarded",
		)
		return nil
	}
	return state.backend.SaveStore(ctx, state.store)
}

// annotateSaveFailure folds a snapshot-save failure into a run's primary
// error. The primary error keeps the classification - errors.Is still matches
// it, so cmd/go-galaxy/exitcode maps a partially failed run to the install
// exit class instead of degrading it to the generic one just because the disk
// also filled up at the tail - while the save failure stays in the message and
// matchable via errors.Is. Returns primary unchanged when the save succeeded.
//
// primary may itself already be a *summaryError joining a headline with N
// per-collection causes (see failureSummary.wrap); fmt.Errorf's "%w; ...: %w"
// still folds that in losslessly, since errors.Is walks Unwrap() []error trees
// depth-first with no first-match-wins shortcut - primary's own headline, its
// causes, and saveErr all remain independently matchable afterward.
func annotateSaveFailure(primary, saveErr error) error {
	if saveErr == nil {
		return primary
	}
	return fmt.Errorf("%w; snapshot save failed: %w", primary, saveErr)
}
