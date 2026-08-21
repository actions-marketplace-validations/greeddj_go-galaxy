package collections

import (
	"context"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/psvmcc/hub/pkg/types"
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
	roots, roleRoots, err := loadRoots(cfg, runtime)
	if err != nil {
		return err
	}
	// Returned as it came rather than folded into the failure summary below: a
	// keyring that cannot be read, or requirements declaring signatures with
	// none configured, is a configuration error this run never started work
	// under, and joining it behind helpers.ErrInstallationFailed would exit it
	// as a failed install rather than as the usage error it is.
	verify, err := newVerifyContext(cfg, runtime, roots)
	if err != nil {
		return err
	}
	resolved, _, err := resolveOrLoadLockfile(ctx, cfg, runtime, state, roots)
	if err != nil {
		return err
	}
	collections, err := buildCollectionsMap(resolved)
	if err != nil {
		return err
	}
	roles, err := resolveOrLoadRoles(ctx, cfg, runtime, state, roleRoots)
	if err != nil {
		return err
	}
	counts := runCounts{Collections: len(collections), Roles: len(roles.roles)}

	if cfg.DryRun {
		return warmDryRun(ctx, cfg, runtime, state, collections, roles, start)
	}

	summary := warmCollections(ctx, cfg, runtime, state, collections, verify)
	if summary.count == 0 {
		summary = warmRoles(ctx, cfg, runtime, state, roles)
	}
	// Same tail contract as finalizeInstall (see its doc comment): the save is
	// attempted first but does not short-circuit, writeRunMetrics always runs
	// regardless of whether it succeeded, and a nonzero collection-failure
	// count stays the primary error even when the save also failed - folded
	// together via annotateSaveFailure so errors.Is still matches
	// ErrInstallationFailed instead of degrading to the save error's own class.
	saveErr := state.backend.SaveStore(ctx, state.store)
	counts.Failures = int(summary.count)
	writeRunMetrics(cfg, runtime, "warm", start, counts, cfg.Frozen)
	if summary.count > 0 {
		return annotateSaveFailure(summary.warmError(), saveErr)
	}
	if saveErr != nil {
		return saveErr
	}
	runtime.Output.PersistentPrintf("🔥 Warm complete: %s cached", counts.describe())
	return nil
}

// warmRoles fills the artifact cache and the extracted store with every
// resolved role, on the Workers-bounded pool: the same two halves warm
// produces for a collection, with no roles tree touched (rolesRoot stays
// nil). It runs after the collections, so a collection failure's summary is
// not diluted by roles that were never attempted.
func warmRoles(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState, roles roleResolution) failureSummary {
	var failures failureRecorder
	depsCtx := newInstallDeps(cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore, nil, nil, nil)
	depsCtx.collectionDeps = depsCtx.withGit(state.backend.Artifacts(), state.gitMemo, state.roleMemo)
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for _, name := range roles.order {
		if ctx.Err() != nil {
			break
		}
		role := roles.roles[name]
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := warmRole(ctx, depsCtx, role); err != nil {
				runtime.Output.Errorf("Failed: role %s error: %s", role.Name, err)
				failures.record(err)
			} else {
				runtime.Output.Okf("Cached: role %s", role.key())
			}
		})
	}
	wg.Wait()
	return failures.summary()
}

// warmRole acquires one role's artifact into the cache and its tree into the
// extracted store, recording the warmed entry under a "role:" key so a role
// and a collection that happen to share a name@version never overwrite each
// other's record.
func warmRole(ctx context.Context, deps installDeps, r resolvedRole) error {
	artifact, err := fetchRoleArtifact(ctx, deps, r)
	if err != nil {
		return err
	}
	defer cleanupIfNeeded(artifact.Cleanup)
	sha, computed, err := resolveArtifactSHA(artifact.Path, nil, artifact.Meta, artifact.SHA, "")
	if err != nil {
		return err
	}
	if deps.extractStore == nil {
		return nil
	}
	if _, err := deps.extractStore.Ensure(ctx, sha, artifact.Path, shaProvenance(computed)); err != nil {
		return err
	}
	if deps.st != nil {
		deps.st.SetWarmed("role:"+r.key(), sha)
	}
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
	roles roleResolution,
	start time.Time,
) error {
	warmed := state.store.WarmedArtifactSHAByKey()
	probe := warmDryRunProbe(cfg, state.backend.Artifacts(), state.extractStore, warmed)
	summary := classifyDryRun(ctx, runtime, cfg, collections, warmDryRunVerbs, probe)
	summary = summary.join(classifyRolesDryRun(ctx, runtime, cfg, roles, warmRolesDryRunVerbs,
		warmRoleDryRunProbe(state.backend.Artifacts(), state.extractStore, warmed)))
	saveErr := saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	counts := runCounts{Collections: len(collections), Roles: len(roles.roles), Failures: int(summary.count)}
	writeRunMetrics(cfg, runtime, "warm", start, counts, cfg.Frozen)
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
	verify *verifyContext,
) failureSummary {
	var failures failureRecorder
	// The same prefetcher install runs (see prepareInstallPlan): warm's cold
	// keys download on the cfg.DownloadWorkers-bounded network pool while the
	// cfg.Workers-bounded workers below consume the handoff via Wait, instead
	// of each worker downloading inline at the smaller pool's concurrency.
	// What that trades away is the fused single-pass streamDownloadAndExtract
	// a worker-inline download would take - the identical trade install makes,
	// and with the handoff's stream-computed hash provenance riding into
	// Ensure, one that re-hashes nothing. The prefetcher's root is nil, like
	// warm's own installDeps root below: shouldSchedulePrefetch reads a nil
	// root as "not installed" and schedules on the cache probe alone, which is
	// exactly warm's question. levels is nil too - warm has no install order -
	// so the task queue falls back to plain key order (see sortTasksByLevel).
	// Close before returning (deferred, ahead of the inline wg.Wait below)
	// joins every prefetch worker and reclaims any unclaimed temp inside
	// warmWithState's own frame, so no download outlives the backend lock.
	prefetchDeps := newPrefetchDeps(cfg, runtime, state.store, state.backend.Artifacts(), nil)
	prefetchDeps.collectionDeps = prefetchDeps.withGit(state.backend.Artifacts(), state.gitMemo, state.roleMemo)
	prefetch := startPrefetcher(ctx, prefetchDeps, collections, nil)
	defer prefetch.Close()
	// warm never touches the collections tree at all - it only downloads and
	// extracts into the content-addressable extracted store - so its
	// installDeps carries a nil root; newInstallTarget's own nil-root guard
	// then makes any accidental collections-tree call from this path fail
	// closed rather than by convention.
	depsCtx := newInstallDeps(
		cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore, nil, prefetch.cachedArtifacts(), verify,
	)
	depsCtx.collectionDeps = depsCtx.withGit(state.backend.Artifacts(), state.gitMemo, state.roleMemo)
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
			meta, prefetched, ok, prefetchErr := prefetch.Wait(col.key())
			if ok && prefetchErr != nil {
				runtime.Output.Printf("⚠️ Prefetch failed for %s: %v", col.key(), prefetchErr)
			}
			if err := warmOne(ctx, depsCtx, col, meta, prefetched); err != nil {
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

// warmOne materializes col into the artifact cache and the extracted store.
// meta and prefetched carry warmCollections's prefetch handoff, consumed
// exactly the way runInstallLevel's workers hand theirs to installCollection:
// meta is the version metadata the key's prefetch worker already loaded (nil
// when the key was never scheduled, or its prefetch failed), and prefetched
// is the artifact temp that worker already downloaded and committed (zero
// when there is nothing to hand off, in which case prepareWithRecovery
// resolves and fetches on its own, as it does for every cache hit).
func warmOne(
	ctx context.Context,
	deps installDeps,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
) error {
	filename := helpers.ArtifactFilename(col.Namespace, col.Name, col.Version)
	payload, err := prepareWithRecovery(ctx, deps, col, meta, prefetched, filename, func(payload installPayload) error {
		return warmVerifyAndEnsure(ctx, deps, col, payload)
	})
	if err != nil {
		return err
	}
	defer cleanupIfNeeded(payload.artifact.Cleanup)
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

// warmVerifyAndEnsure enforces col's pin (if any), verifies its signatures, and
// then populates the extracted store for the artifact, mirroring the install
// path's verify+extract so a corrupt, drifted, or poisoning cached tarball
// drives the same bounded evict-and-refetch-once through prepareWithRecovery.
//
// The order is verifyAndExtract's own, and for the same reason: the pin proves
// these are the bytes the lockfile names, the signature proves who published
// them, and both precede the artifact being materialized anywhere a later
// install would hardlink from.
func warmVerifyAndEnsure(ctx context.Context, deps installDeps, col collection, payload installPayload) error {
	if err := verifyPinnedSHA(col, payload.artifactSHA); err != nil {
		return err
	}
	if err := verifyCollectionSignatures(ctx, deps, col, payload); err != nil {
		return err
	}
	if deps.extractStore == nil {
		return nil
	}
	_, err := deps.extractStore.Ensure(ctx, payload.artifactSHA, payload.artifact.Path, shaProvenance(payload.artifactSHAComputed))
	return err
}
