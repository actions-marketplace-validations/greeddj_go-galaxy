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
	return runInstall(ctx, cfg, runtime)
}

// Warm resolves dependencies and ensures every artifact is downloaded into
// the cache and extracted into the content-addressable extracted store.
// It does not write anything to the install path - useful for baking CI
// images so subsequent installs hardlink instantly.
func Warm(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runWarm(ctx, cfg, runtime)
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

	// The work runs under lockCtx and its outcome is judged against lockCtx,
	// as a direct expression rather than a defer: this function returns the
	// work's outcome from one place, so there is nothing a defer would buy,
	// and a named return value would be needed to make one work at all. See
	// runInstall for the full rationale behind this shape.
	return cacheManager.LockLostError(ctx, lockCtx, warmWithState(lockCtx, cfg, runtime, state, start))
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
	depsCtx := newInstallDeps(cfg, runtime, state.store, state.backend.Artifacts(), state.extractStore, nil)
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

// Lock resolves dependencies and writes a lockfile to disk. It is intended
// for the `lock` command and never installs anything; it does mutate the
// snapshot cache so that subsequent installs benefit from the work done.
func Lock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runLock(ctx, cfg, runtime)
}

// runLock owns the backend lifecycle for the lock command: it opens the
// backend, takes its exclusive lock, and registers the release/close defers
// that must run on every exit path - including one from lockWithState, which
// owns the actual work once state is initialized. Same lifecycle/work
// boundary runInstall and runWarm already draw.
func runLock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	// --frozen is registered on lock because it shares helpers.CollectionFlags
	// with install and warm, and on lock it means something coherent with
	// what it means on those two: the lockfile is law. install/warm --frozen
	// resolve FROM the lockfile instead of the network; lock --frozen instead
	// resolves fresh (lockWithState always does, --frozen or not) and refuses
	// to overwrite the file when that fresh resolve disagrees with what is
	// already there - see lockFrozen's own doc comment for the gate itself.
	// The banner names only the one thing every run does regardless of mode -
	// resolve - rather than what happens to the result, which is exactly
	// where "Checking" and "Generating" diverge: a plain run overwrites the
	// file, a frozen one only compares against it, and a single banner cannot
	// truthfully claim either without branching on cfg.Frozen.
	runtime.Output.Printf("🔒 Resolving for lockfile")
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

	// Same shape as runInstall and runWarm: the work runs under lockCtx and
	// its outcome is judged against lockCtx, as a direct expression.
	return cacheManager.LockLostError(ctx, lockCtx, lockWithState(lockCtx, cfg, runtime, state, start))
}

// lockWithState performs lock's actual work against an already-initialized
// state: resolve requirements, then either write the lockfile, preview it, or
// gate on it, depending on cfg.Frozen and cfg.DryRun. It assumes the backend
// is already open and locked - runLock holds that lifecycle - so it never
// touches state.release or state.backend.Close.
//
// cfg.Frozen is checked before cfg.DryRun, not merely in some order: the two
// flags are orthogonal rather than conflicting - both suppress the write, so
// they only ever disagree on the verdict a suppressed write would have had -
// and checking frozen first is what makes lockFrozen itself the one that
// decides whether to preview or really save its own snapshot (see
// saveLockSnapshot), the stricter of the two verdicts winning by construction
// rather than by which branch happens to run first.
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
	if cfg.Frozen {
		return lockFrozen(ctx, cfg, runtime, state, lf, path, start)
	}
	if cfg.DryRun {
		return lockDryRun(ctx, cfg, runtime, state, lf, path, start)
	}
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
	// install and warm - do not add one here. cfg.Frozen is passed rather than
	// a literal false purely for uniformity with lockFrozen's own call: this
	// line is reachable only when cfg.Frozen is already false (the branch
	// above returns through lockFrozen otherwise), so the value read here is
	// always false in practice, but it is read as the truth rather than
	// asserted as one.
	writeRunMetrics(cfg, runtime, "lock", start, len(lf.Collections), 0, cfg.Frozen)
	return saveErr
}

// lockDryRun substitutes for lockWithState's lockfile.Save and its
// "Lockfile written" announcement: it reports how lf - the fresh resolve's
// own result - differs from whatever is already on disk at path, and writes
// no lockfile at all. The announcement is not merely relocated but
// suppressed outright: under --dry-run no file was written, so printing that
// line would be a false statement about this run. Reached only when
// cfg.Frozen is false - lockWithState checks that flag first and returns
// through lockFrozen otherwise, which performs its own, stricter preview
// instead of this one.
//
// The snapshot is still saved, through saveLockSnapshot - the single rule
// this function shares with lockFrozen for what "save the snapshot" means
// under cfg.DryRun. See installDryRun's own doc comment for what a later
// cleanup run would read into a fabricated snapshot; the same reasoning
// applies here unchanged.
//
// writeRunMetrics is called unconditionally, matching every other command's
// tail; it self-suppresses under cfg.DryRun (see its own doc comment), so this
// call site does not need to know that. Its arguments are the same ones the
// real path passes: the resolved collection count, a truthful zero failure
// count (lock has no per-collection failure), and cfg.Frozen, which is always
// false here for the identical reason lockWithState's own call site is.
func lockDryRun(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	lf *lockfile.File,
	path string,
	start time.Time,
) error {
	reportLockfileDiff(runtime, lf, path, dryRunDiffPrefix, lockfile.Compare(lockDryRunBaseline(runtime, path), lf))
	saveErr := saveLockSnapshot(ctx, cfg, runtime, state)
	writeRunMetrics(cfg, runtime, "lock", start, len(lf.Collections), 0, cfg.Frozen)
	return saveErr
}

// lockFrozen implements lock --frozen's drift gate: instead of writing lf -
// the fresh resolve's own result, identical to what a plain `lock` run would
// have written - it compares lf against whatever lockfile already exists at
// path and fails the run when they differ, rather than overwriting the file.
// This is "the lockfile is law" applied to the one command that would
// otherwise rewrite it: install/warm --frozen already refuse to install
// anything the lockfile does not name; lock --frozen refuses to change what
// the lockfile names in the first place.
//
// A missing lockfile is not drift and is reported as a different sentinel,
// helpers.ErrLockfileMissing: lockfile.Compare(nil, lf) would report every
// collection as Added, which reads exactly like "every pin just changed"
// even though the real fact is "this project has never run `lock`" - and
// Compare's own doc comment requires a caller to draw that distinction
// itself before ever calling it with a nil before, rather than trying to
// recover it from a nil argument after the fact. This is the identical
// sentinel resolveOrLoadLockfile already raises for install/warm --frozen
// against an absent lockfile, so the same flag fails the same way across all
// three commands. A lockfile that exists but fails to load (a bad schema
// version, unparseable YAML, or any other lockfile.Load failure) fails closed
// with helpers.ErrLockfileInvalid via that same Load call - deliberately not
// through lockDryRunBaseline's lenient "treat as no baseline" policy. That
// policy is sound only for a command whose product is to overwrite this file
// and therefore never reads it for real; lockFrozen's entire verdict is what
// is already on disk, so treating an unloadable file as "no baseline" here
// would let a corrupt lockfile pass the gate it exists to enforce.
//
// The gate deliberately mirrors lock's own resolution rather than checking
// against upstream publication: lf comes from the identical
// resolveCollectionsInternal call lockWithState always makes, which reuses
// the persisted resolve snapshot whenever requirements.yml and the effective
// server list are unchanged since the last resolve. So publishing a newer
// upstream version without touching requirements.yml leaves this gate
// reporting "up to date" - deliberately: this gate's contract is "running
// `lock` right now would not change this file", not "every locked collection
// is still the newest one available", which is what the outdated command
// answers instead. A policy that instead forced a network re-resolve here
// would be a gate `lock` itself could not satisfy, since reusing an
// unchanged resolve is lock's own established behavior, not a bug this gate
// exists to route around.
//
// The reported drift error carries no counts of its own, matching
// reportLockfileDiff's own summary (see its doc comment): a file-level server
// change alone leaves every per-collection count at zero, and a message built
// from counts would misreport that as "nothing changed" even though it would
// still rewrite the file. reportLockfileDiff and its Okf/PersistentPrintf
// lines already state what changed in full; this error only states that
// something did, plus how to fix it.
func lockFrozen(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	state *installState,
	lf *lockfile.File,
	path string,
	start time.Time,
) error {
	existing, err := lockfile.LoadRequired(path)
	if err != nil {
		return err
	}

	diff := lockfile.Compare(existing, lf)
	reportLockfileDiff(runtime, lf, path, frozenDiffPrefix, diff)
	saveErr := saveLockSnapshot(ctx, cfg, runtime, state)
	writeRunMetrics(cfg, runtime, "lock", start, len(lf.Collections), 0, cfg.Frozen)
	if !diff.Empty() {
		return annotateSaveFailure(
			fmt.Errorf("%w: %s: run `go-galaxy lock` to update it", helpers.ErrLockfileDrift, path),
			saveErr,
		)
	}
	return saveErr
}

// saveLockSnapshot saves state.store the same way lockDryRun and lockFrozen
// both need it saved: through saveDryRunSnapshotIfPersisted under cfg.DryRun,
// or a plain SaveStore otherwise. It exists as one rule with two callers
// specifically so they cannot diverge: lockFrozen can itself run under
// --dry-run (the two flags are orthogonal - see lockWithState's own doc
// comment), and its save behavior under that combination must be identical
// to lockDryRun's own, by construction, not by two call sites happening to
// agree today.
func saveLockSnapshot(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error {
	if cfg.DryRun {
		return saveDryRunSnapshotIfPersisted(ctx, runtime, state)
	}
	return state.backend.SaveStore(ctx, state.store)
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
	backend = cacheManager.WithStateDeadline(backend, runtime.StateDeadline())
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

// writeRunMetrics persists a JSON metrics report when cfg.MetricsFile is set.
// Best-effort: failures are logged but never fail the run.
//
// CacheHits/CacheMisses/BytesDownloaded report 0/0/0 for any run that never
// touched an ArtifactStore, which is every "lock" and "outdated" run by
// construction: neither command opens one - runLock resolves and writes a
// lockfile, and Outdated only reads the lockfile and queries each server's
// metadata, so runtime.Metrics genuinely accumulates nothing during either
// path. This is a truthful zero, not a gap in this report.
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
// through; lock honors the flag too - lockFrozen gates a fresh resolve
// against the on-disk lockfile instead of consuming it over the network the
// way install/warm do, but it is still --frozen actually changing what this
// run does - so its call sites pass cfg.Frozen through as well, and honored
// and configured coincide there exactly as they do for install and warm.
// Outdated is the opposite case: it passes a literal false unconditionally,
// because it never honors --frozen at all - the lockfile is already its only
// source of the locked side and the server is always asked for the latest,
// with or without the flag - so honored and configured genuinely differ
// there, unlike every other caller of this function. Offline stays
// cfg-derived ON PURPOSE - it governs the HTTP transport for every command,
// lock and outdated included - so cfg.Offline is always the truth there.
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
