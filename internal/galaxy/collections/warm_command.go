package collections

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// Warm resolves dependencies and ensures every artifact is downloaded into
// the cache and extracted into the content-addressable extracted store.
// It does not write anything to the install path - useful for baking CI
// images so subsequent installs hardlink instantly.
func Warm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runWarm(ctx, cfg, runtime)
}

// runWarm drives the warm command through the backend lifecycle every
// collection command shares (see withBackend, which owns that lifecycle and
// the lock-loss verdict), with warmWithState as its work half. The one thing
// warm adds is its own refusal of --no-cache, made ahead of that lifecycle.
func runWarm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	// warm's entire output IS cache state: with --no-cache, initInstall would
	// build a nil extract store and every download would be thrown away
	// uncommitted (see helpers.ErrWarmCacheDisabled), so this is rejected
	// before the backend is opened or its lock taken - no lock, no network.
	if cfg.NoCache {
		return helpers.ErrWarmCacheDisabled
	}
	return withBackend(ctx, cfg, runtime, "🔥 Warming caches", warmWithState)
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
// The error deliberately reuses failureSummary.warmError - the identical
// headline warmWithState's own real failure wrap builds - through the same
// annotateSaveFailure helper, rather than a bespoke fmt.Errorf: the summary's
// recorded causes are joined behind that headline exactly as they are on a
// real run (see failureSummary.wrap), which is what lets the preview's exit
// code track the real run's per underlying cause instead of a single fixed
// class - ExitIntegrity when a recorded cause is helpers.ErrSHA256Mismatch
// (dryRunPinVerdict's own verdict), ExitInstall otherwise (the offline case),
// matching exactly what a real --frozen --offline warm would exit with for
// the identical cause.
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
	summary := classifyDryRun(ctx, runtime, cfg, collections, warmDryRunVerbs, probe)
	saveErr := saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	writeRunMetrics(cfg, runtime, "warm", start, len(collections), int(summary.count), cfg.Frozen)
	if summary.count > 0 {
		return annotateSaveFailure(summary.warmError(), saveErr)
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
	depsCtx := newInstallDeps(cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore, nil, nil)
	var wg sync.WaitGroup
	// max(cfg.Workers, 1): a zero Workers would make sem unbuffered, and the
	// first send would block forever since no worker has started to drain it
	// yet. Matches the same guard the prefetcher already applies (see
	// buildPrefetchTasks / startPrefetchWorkers).
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for _, col := range collections {
		// Once ctx is canceled, no further collection is handed to the worker
		// pool: the loop stops dispatching here, on this iteration, while every
		// worker already started keeps running to completion under the inline
		// wg.Wait below, so no in-flight download or extract is abandoned
		// mid-write. break rather than return: a bare return would still reach
		// the same wg.Wait immediately below (it is not deferred here, see that
		// call's own comment), so nothing is skipped either way, but break
		// keeps this function's single return-through-summary() exit point
		// instead of adding a second one. This function still returns
		// failures.summary() on this path, exactly as it does when the map
		// finishes normally - not an error derived from ctx.Err() - since
		// warmWithState's own tail (the snapshot save and writeRunMetrics)
		// needs to run for an interrupted warm exactly as it does for one that
		// finished on its own. The process's own exit code is decided
		// separately, by cmd/go-galaxy/main.go's handleResult reading the
		// caught signal, so it does not depend on what this function returns.
		//
		// Residual: if cancellation lands exactly between two dispatches and
		// every worker already started still finishes without error, the
		// returned summary's count stays zero and warmWithState reports its
		// ordinary "Warm complete" line even though the run was interrupted -
		// the exit code still comes from the caught signal, not from that
		// count.
		if ctx.Err() != nil {
			break
		}
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
		return warmVerifyAndEnsure(ctx, deps, col, payload)
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
func warmVerifyAndEnsure(ctx context.Context, deps installDeps, col collection, payload installPayload) error {
	if err := verifyPinnedSHA(col, payload.artifactSHA); err != nil {
		return err
	}
	if deps.extractStore == nil {
		return nil
	}
	_, err := deps.extractStore.Ensure(ctx, payload.artifactSHA, payload.artifact.Path)
	return err
}
