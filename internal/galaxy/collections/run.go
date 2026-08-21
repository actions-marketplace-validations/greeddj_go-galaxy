// Package collections implements the commands that resolve Galaxy collections
// and put them where they are wanted: install, warm, lock, and outdated.
// Resolution turns a requirements file into an exact version per transitive
// collection, and the install side then downloads, verifies, extracts, and
// records each one under the configured collections path.
//
// install, warm and lock reach the cache backend through one funnel,
// withBackend: it opens the backend, takes its exclusive lock, and runs the
// command's own work half under the holder context that lock returns, so a run
// that stops owning the cache stops writing to it. Splitting each command into
// a lifecycle half and a work half is also what makes its save-and-report tail
// reachable from a test holding an already-initialized state, with no
// production seam. outdated deliberately opens no backend at all, and
// therefore takes no lock and serves no cached metadata.
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
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

type installState struct {
	backend      cacheManager.Backend
	store        *store.Store
	release      func() error
	extractStore *extracted.Store
	// gitMemo is the run-wide table of discovered git collections; see
	// gitDiscoveryMemo. It lives on the state rather than on a phase's deps
	// because the install phase reads what the resolve phase wrote.
	gitMemo *gitDiscoveryMemo
	// roleMemo is gitMemo's counterpart for roles: what the resolve phase
	// learned about each role, read by the install phase.
	roleMemo *roleDiscoveryMemo
}

// resolveDeps builds the dependency set a resolve runs with: the store, and
// the git store and memo every phase shares.
func (s *installState) resolveDeps(cfg *config.Config, runtime *infra.Infra) collectionDeps {
	return newCollectionDeps(cfg, runtime, s.store).withGit(s.backend.Artifacts(), s.gitMemo, s.roleMemo)
}

type installPlan struct {
	collections map[string]collection
	graph       map[string][]string
	// roles is every role the requirements file and their dependencies
	// resolved to, keyed by install name; see resolveRoles.
	roles    roleResolution
	prefetch *prefetcher
	// verify is this run's signature verification state, nil when the run
	// verifies nothing. It is resolved from the requirements roots, so it
	// belongs to the plan rather than to the state initInstall builds before
	// any requirements file has been read.
	verify *verifyContext
	levels [][]string
}

// stateWork is the work half of a collection command's lifecycle/work split:
// it runs against an already-initialized installState and never touches the
// backend lifecycle itself - not state.release, not state.backend.Close.
type stateWork func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error

// withBackend owns the backend lifecycle every collection command shares: it
// opens the backend, takes its exclusive lock, and registers the release and
// close defers that must run on every exit path - including one from work,
// which owns the actual work once state is initialized. That split is what
// makes each command's save/metrics tail reachable from a test holding an
// already-initialized state, with no production seam.
//
// It also owns the lock-loss verdict. lockCtx is the backend's holder context
// (see cacheManager.Backend's Lock contract): every piece of real work runs
// under it, so a run whose lock is stolen mid-flight stops rather than
// continuing to install, commit, and persist non-exclusively, and both the
// init error and the work's own return are judged against it through
// cacheManager.LockLostError. The Close defer keeps the caller's own ctx
// instead, since it must still run once ownership is gone.
//
// "Stops" has a granularity, and on the two commands that have workers it is
// one unit of work per worker - the same shape runCleanup states for its own
// loops. A collection whose artifact bytes are already in hand finishes
// extracting into the collections tree, since neither the untar nor the
// extracted store's rename is interruptible. What does stop is every write to
// the shared cache: on the S3 backend, the only one whose lock can be taken
// away, an artifact commit and the tail SaveStore both run under this context
// and fail once it ends.
//
// Judging through LockLostError is a direct expression rather than a defer
// for two reasons: nonamedreturns is enabled, so a defer would need a named
// return this function does not have, and both call sites are single returns
// where a defer buys nothing anyway. The release defers are deliberately left
// alone: releasing and closing must happen regardless of the verdict, and the
// lock-loss error a release closure returns stays a logged line rather than
// becoming the run's error, since by then the verdict has already been made
// from the same fact.
//
// banner is passed through a constant format string rather than used as one:
// go vet's printf check refuses a non-constant format, and every caller hands
// a plain percent-free announcement here rather than something to expand.
func withBackend(ctx context.Context, cfg *config.Config, runtime *infra.Infra, banner string, work stateWork) error {
	runtime.Output.Printf("%s", banner)
	start := time.Now()
	lockCtx, state, err := initInstall(ctx, cfg, runtime)
	if err != nil {
		return cacheManager.LockLostError(ctx, lockCtx, err)
	}
	defer func() {
		if state.release != nil {
			if err := state.release(); err != nil {
				runtime.Output.Errorf("lock release: %v", err)
			}
		}
	}()
	defer func() {
		_ = state.backend.Close(ctx)
	}()
	// A --no-cache run hands git builds from discovery to the install phase
	// through the memo; whatever no install worker took is removed here.
	defer state.gitMemo.cleanup()
	defer state.roleMemo.cleanup()

	return cacheManager.LockLostError(ctx, lockCtx, work(lockCtx, cfg, runtime, state, start))
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
	// One unwind covering every failure path from here on, in place of a
	// hand-written pair at each of them: a return added later between the
	// Lock below and the commit at the end gives back whatever this function
	// had already taken, by construction rather than by the author having
	// remembered to.
	//
	// Registered after a successful Open, deliberately: an Open that failed
	// closes nothing today, and this unwind keeps that property rather than
	// quietly changing it.
	var releaseLock func() error
	committed := false
	defer func() {
		if committed {
			return
		}
		unwindBackend(ctx, backend, releaseLock)
	}()

	lockCtx, releaseLock, err := backend.Lock(ctx)
	if err != nil {
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
		return lockCtx, nil, err
	}
	runtime.Output.DebugSincef(snapshotStart, "%s", "load snapshot")
	if err := clearCacheIfRequested(lockCtx, cfg, runtime, backend, st); err != nil {
		return lockCtx, nil, err
	}
	recordProjectUnlessDryRun(lockCtx, cfg, runtime, backend)

	committed = true
	return lockCtx, &installState{
		backend:      backend,
		store:        st,
		release:      releaseLock,
		extractStore: extractStore,
		gitMemo:      newGitDiscoveryMemo(),
		roleMemo:     newRoleDiscoveryMemo(),
	}, nil
}

// unwindBackend gives back what initInstall had already taken when it fails
// after the backend was opened: the exclusive lock first, when one was
// granted, and the backend itself second. That order is the one the
// hand-written pairs at each failure arm used, and it is deliberately the
// reverse of the order withBackend's own two defers unwind in - both are kept
// as they are, since unifying them is a decision about observable behavior
// rather than about removing duplication.
//
// releaseLock is nil when backend.Lock is what failed: there is no lock to
// give back then, only the backend to close. Every failure of both calls is
// swallowed on purpose - this runs while a run is already failing, and the
// error it is failing with is the one the operator needs.
func unwindBackend(ctx context.Context, backend cacheManager.Backend, releaseLock func() error) {
	if releaseLock != nil {
		_ = releaseLock()
	}
	_ = backend.Close(ctx)
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
	if err := backend.RecordProject(ctx, cfg.RequirementsFile, cfg.DownloadPath, cfg.RolesPath); err != nil {
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
	roots, roleRoots, err := loadRoots(cfg, runtime)
	if err != nil {
		return nil, err
	}

	// Resolved from the roots the requirements file declares, which survive
	// under --frozen too, since loadRoots runs before resolveOrLoadLockfile
	// branches. It runs ahead of the prefetcher rather
	// than beside the install workers so that an unreadable keyring, or
	// requirements declaring signatures with none configured, fails the run
	// before a single background download has been scheduled.
	verify, err := newVerifyContext(cfg, runtime, roots)
	if err != nil {
		return nil, err
	}

	resolved, graph, err := resolveOrLoadLockfile(ctx, cfg, runtime, state, roots)
	if err != nil {
		return nil, err
	}

	// Roles are resolved here, beside the collections and ahead of the
	// prefetcher, for the reason newVerifyContext runs where it does: a role
	// that does not exist, or a repository that refuses, fails the run before
	// a background download is scheduled. Under --frozen they come from the
	// lockfile, with no network, as the collections did just above.
	roles, err := resolveOrLoadRoles(ctx, cfg, runtime, state, roleRoots)
	if err != nil {
		return nil, err
	}

	collections, err := buildCollectionsMap(resolved)
	if err != nil {
		return nil, err
	}

	if err := verifyRootsResolved(roots, resolved); err != nil {
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
	prefetchDeps := newPrefetchDeps(cfg, runtime, state.store, state.backend.Artifacts(), root)
	prefetchDeps.collectionDeps = prefetchDeps.withGit(state.backend.Artifacts(), state.gitMemo, state.roleMemo)
	prefetch := startPrefetcher(ctx, prefetchDeps, collections, levels)
	runtime.Output.DebugSincef(prefetchStart, "%s", "prefetch schedule")

	return &installPlan{
		collections: collections,
		graph:       graph,
		roles:       roles,
		levels:      levels,
		prefetch:    prefetch,
		verify:      verify,
	}, nil
}

// resolveOrLoadRoles is resolveOrLoadLockfile for the roles list: under
// --frozen every role comes from the lockfile, else from discovery.
func resolveOrLoadRoles(
	ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, roots []requirements.RoleRequirement,
) (roleResolution, error) {
	if cfg.Frozen {
		lf, err := lockfile.LoadRequired(lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile))
		if err != nil {
			return roleResolution{}, err
		}
		return resolveRolesFromLockfile(lf, roots)
	}
	return resolveRoles(ctx, state.resolveDeps(cfg, runtime), roots)
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
	roots []collection,
) (map[string]collection, map[string][]string, error) {
	if cfg.Frozen {
		path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
		runtime.Output.Printf("🔒 frozen: using lockfile %s", path)
		lf, err := lockfile.LoadRequired(path)
		if err != nil {
			return nil, nil, err
		}
		return resolveFromLockfile(cfg, lf, roots)
	}

	resolveStart := time.Now()
	runtime.Output.Printf("🧩 resolve dependencies")
	resolved, graph, err := resolveCollectionsInternal(
		ctx,
		state.resolveDeps(cfg, runtime),
		roots,
		resolveTopLevel,
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
func loadRoots(cfg *config.Config, runtime *infra.Infra) ([]collection, []requirements.RoleRequirement, error) {
	runtime.Output.Printf("🗂️ load collections from requirements file")
	collectionsDirect, file, err := loadRequirements(cfg.RequirementsFile, "")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load requirements file: %w", err)
	}
	for _, w := range file.Warnings {
		runtime.Output.Warnf("%s", w)
	}
	runtime.Output.Printf("🧩 prepare roots")
	roots, err := prepareRoots(collectionsDirect)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare requirements: %w", err)
	}
	return roots, file.Roles, nil
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
func verifyRootsResolved(roots []collection, resolved map[string]collection) error {
	for _, col := range roots {
		// A git root without an explicit name is verified by discovery (its
		// collections were produced from the repository, or the resolve
		// failed) and, under --frozen, by verifyRootsAgainstLockfile; it has
		// no fqdn of its own to look up here.
		if col.isGit() && col.Namespace == "" && col.Name == "" {
			continue
		}
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
