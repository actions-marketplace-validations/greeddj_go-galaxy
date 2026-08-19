package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// Lock resolves dependencies and writes a lockfile to disk. It is intended
// for the `lock` command and never installs anything; it does mutate the
// snapshot cache so that subsequent installs benefit from the work done.
func Lock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return runLock(ctx, cfg, runtime)
}

// runLock drives the lock command through the backend lifecycle every
// collection command shares (see withBackend, which owns that lifecycle and
// the lock-loss verdict), with lockWithState as its work half. lock adds
// nothing of its own ahead of it.
//
// --frozen is registered on lock because it shares helpers.CollectionFlags
// with install and warm, and on lock it means something coherent with what it
// means on those two: the lockfile is law. install/warm --frozen resolve FROM
// the lockfile instead of the network; lock --frozen instead resolves fresh
// (lockWithState always does, --frozen or not) and refuses to overwrite the
// file when that fresh resolve disagrees with what is already there - see
// lockFrozen's own doc comment for the gate itself. The banner below names
// only the one thing every run does regardless of mode - resolve - rather
// than what happens to the result, which is exactly where "Checking" and
// "Generating" diverge: a plain run overwrites the file, a frozen one only
// compares against it, and a single banner cannot truthfully claim either
// without branching on cfg.Frozen.
func runLock(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	return withBackend(ctx, cfg, runtime, "🔒 Resolving for lockfile", lockWithState)
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
	roots, err := loadRoots(cfg, runtime)
	if err != nil {
		return err
	}
	deps := newCollectionDeps(cfg, runtime, state.store)
	resolved, graph, err := resolveCollectionsInternal(ctx, deps, roots, resolveTopLevel)
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
