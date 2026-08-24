// Package cleanup implements the cleanup command. It walks every project the
// cache backend's registry records, works out which installed collections some
// project's requirements file still reaches, and removes the ones nothing
// does - the on-disk copy, its cached artifact, and its snapshot entry - then
// sweeps the extracted store and the artifact keys written under the
// pre-multi-server shape. Start is the entry point; runCleanup owns the
// backend lifecycle and the lock-loss verdict, and cleanupWithState owns the
// work once the snapshot and the registry are loaded.
//
// Reachability is computed over the whole registry before anything is deleted,
// so one project's requirement keeping another project's copy alive never
// depends on iteration order. Every removal resolves through an os.Root opened
// at the owning project's own collections path, and --dry-run reports exactly
// the candidates a real run would act on without deleting any of them.
package cleanup

import (
	"context"
	"fmt"

	"github.com/Masterminds/semver/v3"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
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
	return runCleanup(ctx, cfg, runtime)
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
				runtime.Output.Errorf("Lock release: %v", err)
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
		runtime.Output.Printf("No projects recorded for GC.")
		return nil
	}
	warnIfSnapshotNotPersisted(runtime, state.store)

	reachable, installedByKey, roles, err := buildReachable(runtime, state.registry, state.store)
	if err != nil {
		return err
	}
	removed, err := removeUnused(ctx, cfg, runtime, state.backend, state.store, reachable, installedByKey)
	if err != nil {
		return err
	}
	removedRoles, err := removeUnusedRoles(ctx, cfg, runtime, state.backend, state.store, roles.reachable, roles.byName)
	if err != nil {
		return err
	}
	removed += removedRoles
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
	sweepExtractedStore(ctx, cfg, runtime, state.store, reachable, installedByKey, roles)
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
	runtime.Output.Printf("Init cache backend")
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
	// hand-written pair at each of them, exactly as collections.initInstall
	// does it: a return added later between the Lock below and the commit at
	// the end gives back whatever this function had already taken, by
	// construction rather than by the author having remembered to.
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
	runtime.Output.Printf("Load storage")
	st, err := backend.LoadStore(lockCtx)
	if err != nil {
		return lockCtx, nil, err
	}
	runtime.Output.Printf("Load projects registry")
	registry, err := backend.LoadProjectRegistry(lockCtx)
	if err != nil {
		return lockCtx, nil, err
	}
	committed = true
	return lockCtx, &cleanupState{
		backend:  backend,
		store:    st,
		registry: registry,
		release:  releaseLock,
	}, nil
}

// unwindBackend gives back what initCleanup had already taken when it fails
// after the backend was opened: the exclusive lock first, when one was
// granted, and the backend itself second. It is a local twin of
// collections.unwindBackend rather than something both packages import: the
// two lifecycles they belong to are not interchangeable (runCleanup keeps a
// defensive nil-state guard and a nil-backend check its collections
// counterpart has no use for), and one shared helper would invite unifying
// those too.
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
		runtime.Output.Okf("Dry-run cleanup complete. Candidates: %d", removed)
		return nil
	}
	runtime.Output.Okf("Cleanup complete. Removed: %d", removed)
	return nil
}
