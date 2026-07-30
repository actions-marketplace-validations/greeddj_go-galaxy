package collections

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/metrics"
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

// Start installs collections according to the provided configuration.
func Start(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	err := runInstall(ctx, cfg, runtime)
	if err != nil {
		runtime.Output.Errorf("Error: %s", err.Error())
	}
	return err
}

// Warm resolves dependencies and ensures every artifact is downloaded into
// the cache and extracted into the content-addressable extracted store.
// It does not write anything to the install path - useful for baking CI
// images so subsequent installs hardlink instantly.
func Warm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	err := runWarm(ctx, cfg, runtime)
	if err != nil {
		runtime.Output.Errorf("Error: %s", err.Error())
	}
	return err
}

// runWarm owns the backend lifecycle for the warm command: it rejects
// --no-cache first, before anything is opened, then opens the backend, takes
// its exclusive lock, and registers the release/close defers that must run
// on every exit path - including one from warmWithState, which owns the
// actual work once state is initialized. This is the same lifecycle/work
// boundary runInstall already draws around prepareInstallPlan.
func runWarm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	// warm's entire output IS cache state: with --no-cache, initInstall would
	// build a nil extract store and every download would be thrown away
	// uncommitted (see helpers.ErrWarmCacheDisabled), so this is rejected
	// before the backend is opened or its lock taken - no lock, no network.
	if cfg.NoCache {
		return helpers.ErrWarmCacheDisabled
	}
	runtime.Output.Printf("🔥 Warming caches")
	start := time.Now()
	state, err := initInstall(ctx, cfg, runtime)
	if err != nil {
		return err
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

	return warmWithState(ctx, cfg, runtime, state, start)
}

// warmWithState performs warm's actual work against an already-initialized
// state: resolving requirements, warming every resolved collection into the
// cache, saving the resulting snapshot, and writing the run metrics report -
// in that order, since the report always runs regardless of whether the save
// succeeded. It assumes the backend is already open and locked - runWarm
// holds that lifecycle - so it never touches state.release or
// state.backend.Close itself.
func warmWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error {
	prep, err := loadRoots(cfg, runtime)
	if err != nil {
		return err
	}
	resolved, _, err := resolveOrLoadLockfile(ctx, cfg, runtime, state, prep)
	if err != nil {
		return err
	}
	collections, err := buildCollectionsMap(resolved)
	if err != nil {
		return err
	}

	if cfg.DryRun {
		return warmDryRun(ctx, cfg, runtime, state, collections, start)
	}

	summary := warmCollections(ctx, cfg, runtime, state, collections)
	// Same tail contract as finalizeInstall (see its doc comment): the save is
	// attempted first but does not short-circuit, writeRunMetrics always runs
	// regardless of whether it succeeded, and a nonzero collection-failure
	// count stays the primary error even when the save also failed - folded
	// together via annotateSaveFailure so errors.Is still matches
	// ErrInstallationFailed instead of degrading to the save error's own class.
	saveErr := state.backend.SaveStore(ctx, state.store)
	writeRunMetrics(cfg, runtime, "warm", start, len(collections), int(summary.count), cfg.Frozen)
	if summary.count > 0 {
		return annotateSaveFailure(summary.warmError(), saveErr)
	}
	if saveErr != nil {
		return saveErr
	}
	runtime.Output.PersistentPrintf("🔥 Warm complete: %d collections cached", len(collections))
	return nil
}

// warmDryRun substitutes for warmCollections wholesale on a dry-run warm,
// rather than passing a flag into it: a boolean inside the worker deciding
// whether to download hundreds of megabytes is exactly the wrong seam - it
// would leave cfg.DryRun readable from warmOne downward, exactly what the
// prohibition on a mode parameter in that call chain exists to prevent.
//
// It reuses saveDryRunSnapshotIfPersisted unchanged because warm's exposure
// to a fabricated snapshot is identical to install's and in fact sharper: a
// warm-only machine's extracted trees are protected solely by the warmed set
// (recordWarmed), since such a machine has no ansible_collections workspace
// at all and so never contributes an installed entry either. A fabricated
// persisted-and-empty snapshot on that machine would hand a later cleanup run
// the evidence to wipe the whole content-addressable store, with nothing on
// disk to re-derive it from.
//
// The error deliberately matches warmWithState's real failure wrap
// (ErrInstallationFailed, mapping to exitcode.ExitInstall) rather than
// inventing a new class, so the preview and the real run exit identically.
//
// SaveStore on this path still age-evicts expired APICache/DepsCache/
// Versions/Warmed entries inside snapshotData(), exactly as every other
// command's save does: that is shared retention maintenance on
// reconstructible/expired state, not warm's product, and special-casing it
// here would mean a second save path for no gain.
func warmDryRun(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	collections map[string]collection,
	start time.Time,
) error {
	warmed := state.store.WarmedArtifactSHAByKey()
	probe := warmDryRunProbe(cfg, state.backend.Artifacts(), state.extractStore, warmed)
	wouldFail := classifyDryRun(ctx, runtime, cfg, collections, warmDryRunVerbs, probe)
	saveErr := saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	writeRunMetrics(cfg, runtime, "warm", start, len(collections), wouldFail, cfg.Frozen)
	if wouldFail > 0 {
		return annotateSaveFailure(
			fmt.Errorf("%w: %d collections cannot be warmed offline: %w", helpers.ErrInstallationFailed, wouldFail, helpers.ErrOfflineMode),
			saveErr,
		)
	}
	return saveErr
}

func warmCollections(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	collections map[string]collection,
) failureSummary {
	var failures failureRecorder
	// warm never touches the collections tree at all - it only downloads and
	// extracts into the content-addressable extracted store - so its
	// installDeps carries a nil root; newInstallTarget's own nil-root guard
	// then makes any accidental collections-tree call from this path fail
	// closed rather than by convention.
	depsCtx := newInstallDeps(cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore, nil)
	var wg sync.WaitGroup
	// max(cfg.Workers, 1): a zero Workers would make sem unbuffered, and the
	// first send would block forever since no worker has started to drain it
	// yet. Matches the same guard the prefetcher already applies (see
	// buildPrefetchTasks / startPrefetchWorkers).
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for _, col := range collections {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := warmOne(ctx, depsCtx, col); err != nil {
				runtime.Output.Errorf("Failed: %s.%s error: %s", col.Namespace, col.Name, err)
				failures.record(err)
			} else {
				runtime.Output.Okf("Cached: %s.%s@%s", col.Namespace, col.Name, col.Version)
			}
		})
	}
	// wg.Wait is inline, not deferred: this function returns failures.summary(),
	// and a deferred wait would evaluate that return value before the workers
	// finish, silently under-reporting failures. runInstallLevel can defer its
	// wait only because it returns an error and reports failures through a
	// caller-owned *failureRecorder instead of a return value - do not "fix"
	// this to match that shape.
	//
	// A per-collection failure's cause is reported to the operator on the
	// error-tier "Failed:" line above, and is now also recorded into the
	// summary this function returns - not because the operator needs to see
	// it twice (summaryError's own doc comment explains why the returned
	// error's message stays one line), but because the causes are what let
	// the run's final error carry a more specific classification than
	// helpers.ErrInstallationFailed alone, such as exiting with the dedicated
	// integrity exit code when the recorded cause is a checksum mismatch.
	// Joining N causes behind the count is what makes that possible; it does
	// not, by itself, change the message length the operator sees.
	wg.Wait()
	return failures.summary()
}

func warmOne(ctx context.Context, deps installDeps, col collection) error {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)
	// warm has no prefetcher, so there is never a prefetched temp to hand off.
	payload, err := prepareWithRecovery(ctx, deps, col, nil, downloadResult{}, filename, func(payload installPayload) error {
		return warmVerifyAndEnsure(deps, col, payload)
	})
	if err != nil {
		return err
	}
	if payload.artifact.Cleanup != nil {
		defer payload.artifact.Cleanup()
	}
	recordWarmed(deps, col, payload.artifactSHA)
	return nil
}

// recordWarmed records that col's artifact has been materialized in the
// extracted store, so cleanup keeps its tree even though warm never calls
// recordInstall. It runs unconditionally on every warmOne call, including a
// cache hit, which is what re-stamps a periodically re-warmed key and keeps
// its WarmedEntryMaxAge retention window meaningful rather than frozen at the
// key's first warm.
//
// install must never call this: recordInstall already writes an
// InstalledEntry whose sha InstalledArtifactSHAByKey feeds into the very same
// keep set (see extractedKeepSet), so a warmed entry there would be
// redundant - and worse than redundant, since it would keep the extracted
// tree alive for up to WarmedEntryMaxAge past the moment cleanup legitimately
// garbage-collected the install that produced it, actively defeating
// cleanup. lock never touches an artifact at all, so it has nothing to
// record either.
func recordWarmed(deps installDeps, col collection, artifactSHA string) {
	if deps.st == nil || deps.extractStore == nil {
		return
	}
	deps.st.SetWarmed(col.key(), artifactSHA)
}

// warmVerifyAndEnsure enforces col's pin (if any) and then populates the
// extracted store for the artifact, mirroring the install path's verify+extract
// so a corrupt, drifted, or poisoning cached tarball drives the same bounded
// evict-and-refetch-once through prepareWithRecovery.
func warmVerifyAndEnsure(deps installDeps, col collection, payload installPayload) error {
	if err := verifyPinnedSHA(col, payload.artifactSHA); err != nil {
		return err
	}
	if deps.extractStore == nil {
		return nil
	}
	_, err := deps.extractStore.Ensure(payload.artifactSHA, payload.artifact.Path)
	return err
}

// Lock resolves dependencies and writes a lockfile to disk. It is intended
// for the `lock` command and never installs anything; it does mutate the
// snapshot cache so that subsequent installs benefit from the work done.
func Lock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	err := runLock(ctx, cfg, runtime)
	if err != nil {
		runtime.Output.Errorf("Error: %s", err.Error())
	}
	return err
}

// runLock owns the backend lifecycle for the lock command: it opens the
// backend, takes its exclusive lock, and registers the release/close defers
// that must run on every exit path - including one from lockWithState, which
// owns the actual work once state is initialized. Same lifecycle/work
// boundary runInstall and runWarm already draw.
func runLock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	// lock does not implement --dry-run: it would still overwrite the
	// lockfile from a fresh resolve, with nothing in its output marking the
	// run as a dry run at all. Checked first, ahead of the --frozen warning
	// below, so a run that cannot proceed emits nothing else - and before
	// anything is opened, locked, or printed, matching runWarm's identical
	// guard.
	if cfg.DryRun {
		return fmt.Errorf("%w: lock would still overwrite the lockfile", helpers.ErrDryRunUnsupported)
	}
	// --frozen is registered on lock only because lock shares
	// helpers.CollectionFlags with install and warm; lock never consumes a
	// lockfile - it always rewrites one from a fresh resolve - so the flag
	// cannot be honored here. This warns rather than failing the run: the
	// realistic way the flag reaches a lock invocation is an ambient
	// GO_GALAXY_FROZEN in a CI environment block shared by every job, and
	// failing a lock job that is otherwise exactly right is a worse trade
	// than one line on stderr. Warnf, not Printf: it must survive --quiet
	// and must not touch stdout.
	if cfg.Frozen {
		runtime.Output.Warnf("--frozen has no effect on lock: the lockfile is always regenerated from a fresh resolve. " +
			"Use install --frozen or warm --frozen to consume an existing lockfile.")
	}
	runtime.Output.Printf("🔒 Generating lockfile")
	start := time.Now()
	state, err := initInstall(ctx, cfg, runtime)
	if err != nil {
		return err
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

	return lockWithState(ctx, cfg, runtime, state, start)
}

// lockWithState performs lock's actual work against an already-initialized
// state: resolve requirements, build and write the lockfile, save the
// resulting snapshot, and write the run metrics report. It assumes the
// backend is already open and locked - runLock holds that lifecycle - so it
// never touches state.release or state.backend.Close.
func lockWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error {
	prep, err := loadRoots(cfg, runtime)
	if err != nil {
		return err
	}
	deps := newCollectionDeps(cfg, runtime, state.store)
	resolved, graph, err := resolveCollectionsInternal(ctx, deps, prep.AllRoots, true, true)
	if err != nil {
		return fmt.Errorf("failed to resolve dependencies: %w", err)
	}
	lf, err := buildLockfile(ctx, deps, resolved, graph)
	if err != nil {
		return err
	}
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	if err := lockfile.Save(path, lf); err != nil {
		return err
	}
	// The lockfile is this command's entire product and stays valid whether or
	// not the snapshot save below succeeds - a failed save costs the next run
	// a cold metadata cache, it does not invalidate the file just written.
	// This is the single emission point for the announcement, placed right
	// after the write it describes so the operator is told the file landed
	// instead of guessing from a nonzero exit code alone; the run still exits
	// nonzero when the save fails.
	runtime.Output.PersistentPrintf("✅ Lockfile written to %s (%d collections)", path, len(lf.Collections))
	saveErr := state.backend.SaveStore(ctx, state.store)
	// writeRunMetrics runs unconditionally, after the save, so its recorded
	// duration includes the save: the report describes the run's work, not
	// its verdict. Failures is a truthful zero here, not a placeholder: across
	// commands the field counts collections that failed to install or warm,
	// and lock has no per-collection failure that could ever reach this line -
	// a resolve or lockfile-build failure returns before writeRunMetrics runs
	// at all. A save failure is not encoded in this field either, matching
	// install and warm - do not add one here. The literal false is not a
	// placeholder: it is the truth for this command, which never reads
	// cfg.Frozen (see runLock's warning above, printed instead of honoring it).
	writeRunMetrics(cfg, runtime, "lock", start, len(lf.Collections), 0, false)
	return saveErr
}

// runInstall owns the backend lifecycle for the install command: it opens
// the backend, takes its exclusive lock, and registers the release/close
// defers that must run on every exit path - including one from
// installWithState, which owns the actual work once state is initialized.
// This is what makes the save/metrics tail reachable from a test with an
// already-initialized state and no production seam.
func runInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	runtime.Output.Printf("🚀 Starting installation process")
	start := time.Now()
	state, err := initInstall(ctx, cfg, runtime)
	if err != nil {
		return err
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

	return installWithState(ctx, cfg, runtime, state, start)
}

// installWithState performs install's actual work against an
// already-initialized state: build the plan, install every level, save the
// snapshot, write the run metrics report - in that order, since the report
// always runs regardless of whether the save succeeded. It assumes the
// backend is already open and locked - runInstall holds that lifecycle - so
// it never touches state.release or state.backend.Close.
func installWithState(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, start time.Time) error {
	// Opened once, here, before requirements are even loaded, so every
	// downstream consumer - the prefetcher and every install worker across
	// every level - shares the same os.Root instead of each racing to open its
	// own. create is tied to !cfg.DryRun: a dry run must never create the
	// directory it is only describing (see openCollectionsRoot's own doc
	// comment). Opening it here, once, rather than once per collection, is
	// also what turns a symlinked ansible_collections into exactly one
	// operator-facing failure for the whole run instead of one per collection
	// in the level - see openCollectionsRoot's own doc comment for why.
	root, err := openCollectionsRoot(cfg.DownloadPath, !cfg.DryRun)
	if err != nil {
		return err
	}
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()

	plan, err := prepareInstallPlan(ctx, cfg, runtime, state, root)
	if err != nil {
		return err
	}
	// A callee's defers always run before its caller's: installWithState is a
	// callee of runInstall, so this defer - registered here, inside
	// installWithState - is guaranteed to run and finish (canceling and
	// joining every prefetch worker) before installWithState returns control
	// to runInstall, which only then unwinds its own lock-release and
	// backend-close defers. A late prefetch worker can therefore never call
	// backend.Artifacts().Commit (or mutate the Store) after the lock is gone
	// - structurally, by Go's defer-then-return ordering across this call
	// boundary, not by convention of registering this defer after two other
	// defers within one function. On the dry-run path below, this defer is
	// still harmless: prepareInstallPlan never started the prefetcher (see
	// startPrefetcher's own cfg.DryRun guard), so plan.prefetch.Close() is a
	// no-op against a prefetcher that was never armed.
	defer plan.prefetch.Close()

	if cfg.DryRun {
		return installDryRun(ctx, cfg, runtime, state, plan, start, root)
	}

	summary, err := installLevels(
		ctx,
		cfg,
		runtime,
		state.store,
		state.backend.Artifacts(),
		state.extractStore,
		plan.collections,
		plan.graph,
		plan.levels,
		plan.prefetch,
		root,
	)
	if err != nil {
		return err
	}

	finalErr := finalizeInstall(ctx, runtime, state.backend, state.store, summary, start)
	writeRunMetrics(cfg, runtime, "install", start, len(plan.collections), int(summary.count), cfg.Frozen)
	return finalErr
}

// installDryRun is installLevels' dry-run substitute: instead of downloading,
// extracting, writing GALAXY.yml, or calling recordInstall for any
// collection, it reports what install would do (classifyDryRun) and stops
// there. writeRunMetrics is still called, unconditionally, matching every
// other command's tail - it self-suppresses under cfg.DryRun (see its own
// doc comment), so this call site does not need to know that.
//
// The snapshot is saved only when state.store.WasPersisted() was already
// true when this run loaded it - i.e. only when a persisted snapshot already
// existed. When it did, the resolve-side caches this run's fresh solve wrote
// (Meta.RequirementsHash/Meta.Server, Requirements, the resolved/graph snapshot, plus
// APICache/DepsCache/Versions picked up along the way) are pure,
// reconstructible cache, not this command's product, so saving them is free
// value - exactly the half of this write --dry-run must not suppress. When
// no persisted snapshot existed, the save is skipped, mirroring
// finalizeCleanup's identical guard in internal/galaxy/cleanup: a fresh
// snapshot's Save/MarshalSnapshot both stamp Meta.LastSnapshot
// unconditionally, and doing that here would manufacture the exact
// persisted-and-empty-installed shape sweepExtractedStore's own
// WasPersisted() guard exists to distinguish from "nothing is installed or
// warmed anywhere" - handing a later cleanup run false positive evidence to
// wipe the whole extracted store on a cold-cache preview that touched
// nothing.
func installDryRun(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	plan *installPlan,
	start time.Time,
	root *os.Root,
) error {
	wouldFail := classifyDryRun(
		ctx, runtime, cfg, plan.collections, installDryRunVerbs, installDryRunProbe(cfg, state.store, state.backend.Artifacts(), root),
	)
	saveErr := saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	writeRunMetrics(cfg, runtime, "install", start, len(plan.collections), wouldFail, cfg.Frozen)
	if wouldFail > 0 {
		return annotateSaveFailure(
			fmt.Errorf("%w: %d collections cannot be installed offline: %w", helpers.ErrInstallationFailed, wouldFail, helpers.ErrOfflineMode),
			saveErr,
		)
	}
	return saveErr
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

// initInstall is the single function every collection command that can be in
// dry-run mode must pass through, so emitting dryRunBanner here makes "no
// command is in dry-run mode silently" a structural property rather than a
// per-call-site convention - a future lock --dry-run inherits it by deleting
// its own guard in runLock, with nothing else to remember.
func initInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (*installState, error) {
	if cfg.DryRun {
		dryRunBanner(runtime)
	}
	runtime.Output.Printf("🚀 init cache backend")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		return nil, err
	}
	if err := backend.Open(ctx); err != nil {
		return nil, err
	}
	releaseLock, err := backend.Lock(ctx)
	if err != nil {
		_ = backend.Close(ctx)
		return nil, err
	}

	extractStore := newExtractStore(cfg)
	// The exclusive backend lock is held now, so no other go-galaxy process can
	// be mid-write: every leftover download-temp and extract-temp is a dead-run
	// orphan and is safe to reclaim.
	sweepDeadRunTemps(ctx, runtime, backend, extractStore)

	snapshotStart := time.Now()
	runtime.Output.Printf("🚀 load storage")
	st, err := backend.LoadStore(ctx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return nil, err
	}
	runtime.Output.DebugSincef(snapshotStart, "%s", "load snapshot")
	if err := clearCacheIfRequested(ctx, cfg, runtime, backend, st); err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return nil, err
	}
	recordProjectUnlessDryRun(ctx, cfg, runtime, backend)

	return &installState{
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

// writeRunMetrics persists a JSON metrics report when cfg.MetricsFile is set.
// Best-effort: failures are logged but never fail the run.
//
// For the "lock" command, CacheHits/CacheMisses/BytesDownloaded always report
// 0/0/0: runLock resolves and writes a lockfile without ever touching an
// ArtifactStore, so runtime.Metrics genuinely accumulates nothing during that
// path - a truthful zero, not a gap in this report.
//
// The totals read here are best-effort on a failed run. runInstall registers
// `defer plan.prefetch.Close()`, so on a successful run every prefetch task
// has already been joined through Wait before this function runs, and the
// totals are complete. On a failed run, though, installLevels breaks out of
// its level loop before scheduling any later level, while the prefetcher may
// already have background workers in flight for that unscheduled level; such
// a worker can still land its own miss and bytes after Totals() is read here
// (Close only cancels and joins it afterward, once runInstall unwinds), so a
// failed run's counters in this report are a lower bound, not an exact count.
//
// frozen is passed by the caller rather than read from cfg because the
// report's frozen field states what the run HONORED, not what was
// CONFIGURED: install and warm route resolution through
// resolveOrLoadLockfile, which branches on cfg.Frozen, so they pass it
// through; lock accepts the flag (it shares the collection flag set) but
// never consumes a lockfile, so it passes false. Offline stays cfg-derived ON
// PURPOSE - it governs the HTTP transport for every command, lock included -
// so cfg.Offline is always the truth there.
func writeRunMetrics(
	cfg *config.Config,
	runtime *infra.Infra,
	command string,
	start time.Time,
	collections, failures int,
	frozen bool,
) {
	if cfg == nil || cfg.MetricsFile == "" {
		return
	}
	// metrics.Report has no field distinguishing a dry run from a real one, so
	// a dry run's report is indistinguishable from an install that actually
	// happened - the same class of untruth as a report claiming a run honored
	// a flag it never honored. Worse, tryLockfileHash below reads the
	// lockfile fresh off disk, so a dry run's report would pair an OLD
	// lockfile hash with a NEW resolve's counts. This guard lives inside
	// writeRunMetrics itself, not at each call site, so install, warm, and
	// lock all inherit it for free.
	if cfg.DryRun {
		runtime.Output.Warnf("--dry-run: skipping metrics report to %s", cfg.MetricsFile)
		return
	}
	now := time.Now()
	totals := runtime.Metrics.Totals()
	report := metrics.Report{
		Command:         command,
		StartedAt:       start.UTC(),
		FinishedAt:      now.UTC(),
		Duration:        now.Sub(start),
		CacheHits:       totals.CacheHits,
		CacheMisses:     totals.CacheMisses,
		BytesDownloaded: totals.BytesDownloaded,
		Collections:     collections,
		Failures:        failures,
		Server:          cfg.Server,
		Frozen:          frozen,
		Offline:         cfg.Offline,
		LockfilePath:    lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile),
		LockfileHash:    tryLockfileHash(cfg),
	}
	if err := metrics.Write(cfg.MetricsFile, report); err != nil {
		runtime.Output.Printf("⚠️ Failed to write metrics %s: %v", cfg.MetricsFile, err)
	}
}

func tryLockfileHash(cfg *config.Config) string {
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(path)
	if err != nil {
		return ""
	}
	hash, err := lf.Hash()
	if err != nil {
		return ""
	}
	return hash
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
		lf, err := lockfile.Load(path)
		if err != nil {
			if lockfile.IsNotExist(err) {
				return nil, nil, fmt.Errorf("%w: %s", helpers.ErrLockfileMissing, path)
			}
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
// map, rejecting two failure shapes before any install work starts: a
// duplicate key (ErrDuplicateCollectionKey, pre-existing) and, now, an unsafe
// identifier (ErrUnsafeCollectionIdentifier). The latter guard exists because
// helpers.SplitFQDN performs no path-safety validation of its own - see
// TestSplitFQDNDoesNotValidatePathSafety in helpers - so a namespace or name
// like "foo/../.." reaches here unvalidated from every caller that only
// checked it parses as a two-part FQDN. newInstallTarget validates the same
// three components again, later, per collection - keeping this guard here as
// well is deliberate, matching the pattern writeExtractMarker's own doc
// comment already documents for this file: a caller's check does not make a
// callee's own guard redundant.
func buildCollectionsMap(resolved map[string]collection) (map[string]collection, error) {
	collections := make(map[string]collection, len(resolved))
	for _, col := range resolved {
		if !helpers.IsPathElement(col.Namespace) || !helpers.IsPathElement(col.Name) || !helpers.IsPathElement(col.Version) {
			return nil, fmt.Errorf("%w: ns=%q name=%q version=%q",
				helpers.ErrUnsafeCollectionIdentifier, col.Namespace, col.Name, col.Version)
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

func installLevels(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	extractStore *extracted.Store,
	collections map[string]collection,
	graph map[string][]string,
	levels [][]string,
	prefetch *prefetcher,
	root *os.Root,
) (failureSummary, error) {
	depsCtx := newInstallDeps(cfg, runtime, st, artifacts, extractStore, root)
	var failures failureRecorder
	for _, level := range levels {
		if err := runInstallLevel(ctx, depsCtx, collections, graph, level, prefetch, &failures); err != nil {
			// err here is helpers.ErrMissingCollection, a usage-class plan bug in
			// the level/collections map built before any level ran - not an
			// install failure. installWithState returns this err directly,
			// never reaching finalizeInstall, so the summary's causes (if any
			// were already recorded by workers dispatched earlier in this same
			// level) are deliberately discarded rather than joined into it:
			// doing so would let a plan bug misclassify as an install/integrity
			// failure instead of the usage error it is.
			return failures.summary(), err
		}
		if failures.count() > 0 {
			break
		}
	}
	return failures.summary(), nil
}

// runInstallLevel dispatches installs for one level's keys onto a
// Workers-bounded pool and joins them before returning. wg.Wait is deferred
// ahead of the loop so every exit - including the ErrMissingCollection guard,
// which can trip after earlier keys in this level were already dispatched -
// waits the in-flight workers rather than leaking them past installLevels (and
// past runInstall's backend-lock release). failures is shared across levels
// and is safe for concurrent use by every worker, recording each one's own
// cause alongside its count.
func runInstallLevel(
	ctx context.Context,
	depsCtx installDeps,
	collections map[string]collection,
	graph map[string][]string,
	level []string,
	prefetch *prefetcher,
	failures *failureRecorder,
) error {
	var wg sync.WaitGroup
	// max(depsCtx.cfg.Workers, 1): see warmCollections's identical guard - a
	// zero Workers would make sem unbuffered and deadlock the first send.
	sem := make(chan struct{}, max(depsCtx.cfg.Workers, 1))
	defer wg.Wait()

	for _, key := range level {
		col, ok := collections[key]
		if !ok {
			return fmt.Errorf("%w for: %s", helpers.ErrMissingCollection, key)
		}
		depKeys := graph[key]
		if depKeys == nil {
			depKeys = []string{}
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			meta, prefetched, ok, prefetchErr := prefetch.Wait(col.key())
			if ok && prefetchErr != nil {
				depsCtx.runtime.Output.Printf("⚠️ Prefetch failed for %s: %v", col.key(), prefetchErr)
			}
			if err := installCollection(ctx, col, depsCtx, depKeys, meta, prefetched); err != nil {
				depsCtx.runtime.Output.Errorf("Failed: %s.%s error: %s", col.Namespace, col.Name, err)
				failures.record(err)
			} else {
				depsCtx.runtime.Output.Okf("Installed: %s.%s", col.Namespace, col.Name)
			}
		})
	}
	return nil
}

// finalizeInstall saves the run's snapshot and reports the run's outcome.
// The save is attempted first but does not short-circuit: whether it
// succeeds or fails, the returned error still classifies by the
// collection-failure count, so the exit class a CI script branches on does
// not depend on whether the tail also happened to hit a full disk. The one
// classification that still outranks it is cancellation - cmd/go-galaxy/exitcode
// checks context.Canceled ahead of every class - so a save that fails because
// the run was interrupted still exits as interrupted, which is what an
// interrupted run should report and is what the pre-fix code did too.
func finalizeInstall(
	ctx context.Context,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	summary failureSummary,
	start time.Time,
) error {
	saveStart := time.Now()
	saveErr := backend.SaveStore(ctx, st)
	if saveErr == nil {
		runtime.Output.DebugSincef(saveStart, "%s", "save snapshot")
	}
	if summary.count > 0 {
		runtime.Output.PersistentPrintf("⚠️ Completed with errors: %d failed. Took %s", summary.count, time.Since(start).Round(time.Second))
		return annotateSaveFailure(summary.installError(), saveErr)
	}
	if saveErr != nil {
		return saveErr
	}
	runtime.Output.PersistentPrintf("🤩 All done. Took %s", time.Since(start).Round(time.Second))
	return nil
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
