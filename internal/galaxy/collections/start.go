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
// It does not write anything to the install path — useful for baking CI
// images so subsequent installs hardlink instantly.
func Warm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	err := runWarm(ctx, cfg, runtime)
	if err != nil {
		runtime.Output.Errorf("Error: %s", err.Error())
	}
	return err
}

func runWarm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
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
	if err := state.backend.SaveStore(ctx, state.store); err != nil {
		return err
	}
	writeRunMetrics(cfg, runtime, "warm", start, len(collections), int(failures))
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
	depsCtx := newInstallDeps(cfg, runtime, state.store, state.backend.Artifacts(), nil, state.extractStore)
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.Workers)
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
	wg.Wait()
	return failures.Load()
}

func warmOne(ctx context.Context, deps installDeps, col collection) error {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)
	payload, err := prepareWithRecovery(ctx, deps, col, nil, filename, func(payload installPayload) error {
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

	prefetchStart := time.Now()
	prefetch := startPrefetcher(
		ctx,
		newPrefetchDeps(cfg, runtime, state.store, state.backend.Artifacts()),
		collections,
	)
	runtime.Output.DebugSincef(prefetchStart, "%s", "prefetch schedule")

	levelStart := time.Now()
	levels, err := buildInstallLevels(graph)
	if err != nil {
		// The prefetcher was already scheduled above; a level-build failure
		// here must not leak its workers, so join them before returning.
		prefetch.Close()
		return nil, err
	}
	runtime.Output.DebugSincef(levelStart, "%s", "build install levels")

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
func writeRunMetrics(cfg *config.Config, runtime *infra.Infra, command string, start time.Time, collections, failures int) {
	if cfg == nil || cfg.MetricsFile == "" {
		return
	}
	now := time.Now()
	report := metrics.Report{
		Command:      command,
		StartedAt:    start.UTC(),
		FinishedAt:   now.UTC(),
		Duration:     now.Sub(start),
		Collections:  collections,
		Failures:     failures,
		Server:       cfg.Server,
		Frozen:       cfg.Frozen,
		Offline:      cfg.Offline,
		LockfilePath: lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile),
		LockfileHash: tryLockfileHash(cfg),
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
		return nil, nil, fmt.Errorf("failed to resolve dependencies: %w", err)
	}
	runtime.Output.DebugSincef(resolveStart, "%s", "resolve dependencies")
	return resolved, graph, nil
}

func loadRoots(cfg *config.Config, runtime *infra.Infra) (*rootPreparation, error) {
	runtime.Output.Printf("🗂️ load collections from requirements file")
	collectionsDirect, rolesFound, err := loadRequirements(cfg.RequirementsFile, cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("failed to load requirements file: %w", err)
	}
	if rolesFound {
		runtime.Output.Printf("⚠️ requirements.yml contains roles, but roles are not supported.")
	}
	runtime.Output.Printf("🧩 prepare roots")
	prep, err := prepareRoots(cfg, collectionsDirect)
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
	depsCtx := newInstallDeps(cfg, runtime, st, artifacts, nil, extractStore)
	var failures int32
	for _, level := range levels {
		var wg sync.WaitGroup
		sem := make(chan struct{}, cfg.Workers)

		for _, key := range level {
			col, ok := collections[key]
			if !ok {
				return failures, fmt.Errorf("%w for: %s", helpers.ErrMissingCollection, key)
			}
			depKeys := graph[key]
			if depKeys == nil {
				depKeys = []string{}
			}
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				meta, ok, prefetchErr := prefetch.Wait(col.key())
				if ok && prefetchErr != nil {
					runtime.Output.Printf("⚠️ Prefetch failed for %s: %v", col.key(), prefetchErr)
				}
				if err := installCollection(ctx, col, depsCtx, depKeys, meta); err != nil {
					runtime.Output.Errorf("Failed: %s.%s error: %s", col.Namespace, col.Name, err)
					atomic.AddInt32(&failures, 1)
				} else {
					runtime.Output.Okf("Installed: %s.%s", col.Namespace, col.Name)
				}
			})
		}

		wg.Wait()
		if atomic.LoadInt32(&failures) > 0 {
			break
		}
	}
	return atomic.LoadInt32(&failures), nil
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
