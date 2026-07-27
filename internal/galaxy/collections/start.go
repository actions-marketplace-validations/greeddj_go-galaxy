package collections

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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
// --no-cache before anything is opened, then opens the backend, takes its
// exclusive lock, and registers the release/close defers that must run on
// every exit path - including one from warmWithState, which owns the actual
// work once state is initialized. This is the same lifecycle/work boundary
// runInstall already draws around prepareInstallPlan.
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

	failures := warmCollections(ctx, cfg, runtime, state, collections)
	// Mirror runInstall's precedence: writeRunMetrics always runs, even when
	// SaveStore fails, and a save error wins over a nonzero failure count.
	saveErr := state.backend.SaveStore(ctx, state.store)
	writeRunMetrics(cfg, runtime, "warm", start, len(collections), int(failures))
	if saveErr != nil {
		return saveErr
	}
	if failures > 0 {
		return fmt.Errorf("%w: warm failed for %d collections", helpers.ErrInstallationFailed, failures)
	}
	runtime.Output.PersistentPrintf("🔥 Warm complete: %d collections cached", len(collections))
	return nil
}

func warmCollections(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	collections map[string]collection,
) int32 {
	var failures atomic.Int32
	depsCtx := newInstallDeps(cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore)
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
				failures.Add(1)
			} else {
				runtime.Output.Okf("Cached: %s.%s@%s", col.Namespace, col.Name, col.Version)
			}
		})
	}
	// wg.Wait is inline, not deferred: this function returns failures.Load(),
	// and a deferred wait would evaluate that return value before the workers
	// finish, silently under-reporting failures. runInstallLevel can defer its
	// wait only because it returns an error and reports failures through a
	// caller-owned *int32 instead of a return value - do not "fix" this to
	// match that shape.
	wg.Wait()
	return failures.Load()
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
	return nil
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

func runLock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
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
	if err := state.backend.SaveStore(ctx, state.store); err != nil {
		return err
	}
	writeRunMetrics(cfg, runtime, "lock", start, len(lf.Collections), 0)
	runtime.Output.PersistentPrintf("✅ Lockfile written to %s (%d collections)", path, len(lf.Collections))
	return nil
}

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

	plan, err := prepareInstallPlan(ctx, cfg, runtime, state)
	if err != nil {
		return err
	}
	// Registered after the lock-release and backend-close defers above, so by
	// LIFO it runs first: every prefetch worker is canceled and joined before
	// the backend is closed and the lock released, so a late worker can never
	// call backend.Artifacts().Commit (or mutate the Store) after the lock is
	// gone.
	defer plan.prefetch.Close()
	failures, err := installLevels(
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
	)
	if err != nil {
		return err
	}

	finalErr := finalizeInstall(ctx, runtime, state.backend, state.store, failures, start)
	writeRunMetrics(cfg, runtime, "install", start, len(plan.collections), int(failures))
	return finalErr
}

func prepareInstallPlan(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) (*installPlan, error) {
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

	roots, err := buildRootKeys(prep, resolved)
	if err != nil {
		return nil, err
	}
	state.store.SetRoots("last_run", roots)

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
		newPrefetchDeps(cfg, runtime, state.store, state.backend.Artifacts()),
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

func initInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (*installState, error) {
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
	if cfg.ClearCache {
		st.ClearCaches()
		if err := backend.ClearFiles(ctx); err != nil {
			_ = releaseLock()
			_ = backend.Close(ctx)
			return nil, err
		}
	}
	if err := backend.RecordProject(ctx, cfg.RequirementsFile, cfg.DownloadPath); err != nil {
		runtime.Output.Printf("⚠️ Failed to record project: %v", err)
	}

	return &installState{
		backend:      backend,
		store:        st,
		release:      releaseLock,
		extractStore: extractStore,
	}, nil
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
func writeRunMetrics(cfg *config.Config, runtime *infra.Infra, command string, start time.Time, collections, failures int) {
	if cfg == nil || cfg.MetricsFile == "" {
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
		Frozen:          cfg.Frozen,
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

func buildCollectionsMap(resolved map[string]collection) (map[string]collection, error) {
	collections := make(map[string]collection, len(resolved))
	for _, col := range resolved {
		key := col.key()
		if _, ok := collections[key]; ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrDuplicateCollectionKey, key)
		}
		collections[key] = col
	}
	return collections, nil
}

func buildRootKeys(prep *rootPreparation, resolved map[string]collection) ([]string, error) {
	roots := make([]string, 0, len(prep.AllRoots))
	for _, col := range prep.AllRoots {
		fqdn := fmt.Sprintf("%s.%s", col.Namespace, col.Name)
		resolvedCol, ok := resolved[fqdn]
		if !ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrMissingResolvedRoot, fqdn)
		}
		roots = append(roots, resolvedCol.key())
	}
	return roots, nil
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
) (int32, error) {
	depsCtx := newInstallDeps(cfg, runtime, st, artifacts, extractStore)
	var failures int32
	for _, level := range levels {
		if err := runInstallLevel(ctx, depsCtx, collections, graph, level, prefetch, &failures); err != nil {
			return atomic.LoadInt32(&failures), err
		}
		if atomic.LoadInt32(&failures) > 0 {
			break
		}
	}
	return atomic.LoadInt32(&failures), nil
}

// runInstallLevel dispatches installs for one level's keys onto a
// Workers-bounded pool and joins them before returning. wg.Wait is deferred
// ahead of the loop so every exit - including the ErrMissingCollection guard,
// which can trip after earlier keys in this level were already dispatched -
// waits the in-flight workers rather than leaking them past installLevels (and
// past runInstall's backend-lock release). failures is shared across levels and
// updated atomically by the workers.
func runInstallLevel(
	ctx context.Context,
	depsCtx installDeps,
	collections map[string]collection,
	graph map[string][]string,
	level []string,
	prefetch *prefetcher,
	failures *int32,
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
				atomic.AddInt32(failures, 1)
			} else {
				depsCtx.runtime.Output.Okf("Installed: %s.%s", col.Namespace, col.Name)
			}
		})
	}
	return nil
}

func finalizeInstall(
	ctx context.Context,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	failures int32,
	start time.Time,
) error {
	saveStart := time.Now()
	if err := backend.SaveStore(ctx, st); err != nil {
		return err
	}
	runtime.Output.DebugSincef(saveStart, "%s", "save snapshot")
	if failures > 0 {
		runtime.Output.PersistentPrintf("⚠️ Completed with errors: %d failed. Took %s", failures, time.Since(start).Round(time.Second))
		return fmt.Errorf("%w for %d collections", helpers.ErrInstallationFailed, failures)
	}
	runtime.Output.PersistentPrintf("🤩 All done. Took %s", time.Since(start).Round(time.Second))
	return nil
}
