package collections

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// Start installs collections according to the provided configuration.
func Start(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runInstall(ctx, cfg, runtime)
}

// runInstall owns the backend lifecycle for the install command: it opens
// the backend, takes its exclusive lock, and registers the release/close
// defers that must run on every exit path - including one from
// installWithState, which owns the actual work once state is initialized.
// This is what makes the save/metrics tail reachable from a test with an
// already-initialized state and no production seam.
//
// It also owns the lock-loss verdict for this command. lockCtx is the
// backend's holder context (see cacheManager.Backend's Lock contract): every
// piece of real work runs under it, so a run whose lock is stolen mid-flight
// stops rather than continuing to install, commit, and persist
// non-exclusively, and both the init error and the work's own return are
// judged against it through cacheManager.LockLostError.
//
// "Stops" has a granularity, and it is one unit of work per worker - the same
// shape runCleanup states for its own loops. A collection whose artifact
// bytes are already in hand finishes extracting into the collections tree,
// since neither the untar nor the extracted store's rename is interruptible.
// What does stop is every write to the shared cache: on the S3 backend, the
// only one whose lock can be taken away, the artifact commit and the tail
// SaveStore both run under this context and fail once it ends.
//
// Judging through LockLostError is a direct expression rather than a defer
// for two reasons: nonamedreturns is enabled, so a defer would need a named
// return this function does not have, and both call sites are single returns
// where a defer buys nothing anyway. The release defers are deliberately left
// alone: releasing and closing must happen regardless of the verdict, and the
// lock-loss error a release closure returns stays a logged line rather than
// becoming the run's error, since by then the verdict has already been made
// from the same fact.
func runInstall(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	runtime.Output.Printf("🚀 Starting installation process")
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

	return cacheManager.LockLostError(ctx, lockCtx, installWithState(lockCtx, cfg, runtime, state, start))
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
// The failure error reuses failureSummary.installError - the identical
// headline finalizeInstall's own real failure wrap builds - through the same
// annotateSaveFailure helper, rather than a bespoke fmt.Errorf: the
// summary's recorded causes are joined behind that headline exactly as they
// are on a real run (see failureSummary.wrap), which is what lets the
// preview's exit code track the real run's per underlying cause instead of a
// single fixed class - ExitIntegrity when a recorded cause is
// helpers.ErrSHA256Mismatch (dryRunPinVerdict's own verdict), ExitInstall
// otherwise (the offline case, or a collections-tree write a real install
// would refuse), matching exactly what a real --frozen --offline install
// would exit with for the identical cause.
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
	summary := classifyDryRun(
		ctx, runtime, cfg, plan.collections, installDryRunVerbs, installDryRunProbe(cfg, state.store, state.backend.Artifacts(), root),
	)
	saveErr := saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	writeRunMetrics(cfg, runtime, "install", start, len(plan.collections), int(summary.count), cfg.Frozen)
	if summary.count > 0 {
		return annotateSaveFailure(summary.installError(), saveErr)
	}
	return saveErr
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
	depsCtx := newInstallDeps(cfg, runtime, st, artifacts, extractStore, root, prefetch.cachedArtifacts())
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
		// Once ctx is canceled, no further key in this level is handed to the
		// worker pool: the loop stops dispatching here, on this iteration,
		// while every worker already started keeps running to completion under
		// the deferred wg.Wait above, so no in-flight install is abandoned
		// mid-write. break rather than return: a bare return would still run
		// that same deferred wait, so nothing is skipped either way, but break
		// keeps this function's single nil-returning exit point instead of
		// adding a second one. This function still returns nil on this path,
		// exactly as it does when a level finishes normally - not ctx.Err() -
		// because installLevels propagates a non-nil return straight up past
		// finalizeInstall, and an interrupted run must still reach it to save
		// its snapshot. The process's own exit code is decided separately, by
		// cmd/go-galaxy/main.go's handleResult reading the caught signal, so it
		// does not depend on what this function returns.
		//
		// Residual: if cancellation lands exactly between two dispatches and
		// every worker already started still finishes without error, this
		// level's failure count stays zero and finalizeInstall reports its
		// ordinary success line even though the run was interrupted - the exit
		// code still comes from the caught signal, not from that count.
		if ctx.Err() != nil {
			break
		}
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
// interrupted run should report.
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
