package cache

import (
	"context"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// cleanSaveSkipBackend is a Backend decorator that turns SaveStore into a
// no-op whenever the store it is handed carries no write this process made,
// while passing every other method through unmodified. See WithCleanSaveSkip
// for the argument behind its shape.
type cleanSaveSkipBackend struct {
	inner Backend
}

// WithCleanSaveSkip wraps b so its SaveStore call becomes a no-op returning
// nil whenever the store it is handed is non-nil and reports
// Store.Dirty() == false: this process wrote nothing into it since it was
// loaded, so there is nothing for a save to persist. b == nil returns nil
// rather than wrapping a nil interface into a non-nil one, which would
// otherwise make every method call on the result panic on a nil pointer
// dereference instead of behaving like an absent backend - the identical
// guard WithStateDeadline carries.
//
// Every Backend method is implemented here by hand, over the named inner
// field rather than an embedded interface, for the identical reason
// stateDeadlineBackend in statedeadline.go is: a method added to the Backend
// interface later cannot pass through this decorator unnoticed, because the
// build stops on this function's own return statement until that method is
// written here too. Adding a Ping(ctx context.Context) error to Backend, for
// instance, fails with "cannot use &cleanSaveSkipBackend{…} (value of type
// *cleanSaveSkipBackend) as Backend value in return statement:
// *cleanSaveSkipBackend does not implement Backend (missing method Ping)".
// Whoever adds that method then has to decide here, at this call site,
// whether it needs its own dirty-aware override or is left passing through
// unmodified - the way every method but SaveStore already is.
//
// What the skip gives up, and why none of it costs a verdict:
//
//   - Persist-time age eviction (snapshotData's CacheEntryMaxAge and
//     WarmedEntryMaxAge windows) does not run on a save this decorator
//     skips, so an entry that would otherwise have aged out of the
//     persisted snapshot lingers there until the next run that actually
//     writes it. This is safe: a clean run added nothing new for that
//     eviction to prune in the first place, and the one pass that depends
//     on the warmed window - the extracted-store keep set cleanup builds -
//     reads that window at call time through
//     Store.WarmedArtifactSHAByKey rather than trusting whatever the last
//     save happened to prune, so a keep set built against a store whose
//     save was skipped is exactly as fresh as one built against a store
//     that just saved.
//   - For an entry read under an exact-version policy (Policy.TTL == 0) -
//     cachedVersionsList and cachedDeps in internal/galaxy/collections, and
//     this package's own tryServeFromCache when its TTL check is
//     structurally skipped - persist-time eviction is the ONLY mechanism
//     that ever forces a refetch, since none of the three re-checks the
//     entry's age at read time. Skipping a save therefore lengthens how
//     long such an entry can outlive CacheEntryMaxAge. That reach is bounded
//     on purpose: an exact-version policy only ever names a key for one
//     immutable published fact (a specific collection version's dependency
//     map, or a versions listing read while already pinned to one version),
//     so what outlives its window is a fact this program already treats as
//     unable to change out from under the cache regardless of this
//     decorator - see resolveCollectionsInternal's own doc comment
//     (internal/galaxy/collections/resolve.go) for why --refresh itself
//     leaves exactly this class of entry alone. None of the operator escape
//     hatches that exist to force a fresh look are defeated by this skip:
//     --refresh still redoes the version-free half of resolution and
//     records the result through the normal setters, dirtying the store
//     regardless of whether any individual exact-version entry was itself
//     bypassed; --no-cache disables per-request cache read/write but the
//     resolved-snapshot record a completed resolve writes still runs and
//     still dirties the store; and --clear-cache calls Store.ClearCaches,
//     itself one of the 14 mutators that sets Dirty.
//   - lock on a cold cache still saves, which is the first thing a reader of
//     this decorator should check: with no prior snapshot,
//     Meta.RequirementsHash is empty, so snapshotMatchesRequirements is
//     false, the fresh resolve genuinely runs, recordResolution's
//     SetResolvedAll dirties the store, and the persisted-and-empty
//     snapshot still gets created.
//   - The one genuine residual: an abandoned prefetch worker can still call
//     SetAPICache between a SaveStore call's own Dirty() check here and
//     finalizeInstall's own copy of the state it is about to save,
//     reachable only after installLevels has already broken out of a
//     failing level - plan.prefetch.Close() is deferred below
//     finalizeInstall on purpose, so a worker mid-flight when a level fails
//     is allowed to finish rather than being torn down - which means it is
//     reachable only on a run that has already failed. What is lost in that
//     narrow window is one reconstructible metadata entry: exactly what an
//     unconditional save racing the same worker would also have lost, since
//     neither a save nor this decorator's check synchronizes with that
//     goroutine at all. This is disclosed, not fixed: fixing it would mean
//     restructuring the defer order finalizeInstall relies on for an
//     unrelated reason, and this decorator deliberately leaves that order
//     alone.
func WithCleanSaveSkip(b Backend) Backend {
	if b == nil {
		return nil
	}
	return &cleanSaveSkipBackend{inner: b}
}

// SaveStore skips b.inner.SaveStore entirely when st is non-nil and reports
// Store.Dirty() == false.
//
// The st != nil guard is load-bearing, not defensive boilerplate: Dirty on a
// nil *Store returns false through its own nil-receiver guard, so without
// this check a nil store would read as "clean" and this decorator would
// silently turn a nil SaveStore call into success - swallowing
// local.Backend's helpers.ErrStoreNil, which local.Backend returns for
// exactly that call today. The S3 backend, by contrast, already returns nil
// for a nil store on its own. The two backends disagree with each other
// today on what a nil store means, and this decorator must not invent a
// third answer by quietly making local.Backend agree with the S3 backend's
// nil-is-fine contract through a side door.
func (b *cleanSaveSkipBackend) SaveStore(ctx context.Context, st *store.Store) error {
	if st != nil && !st.Dirty() {
		return nil
	}
	return b.inner.SaveStore(ctx, st)
}

// Open passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) Open(ctx context.Context) error {
	return b.inner.Open(ctx)
}

// Close passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) Close(ctx context.Context) error {
	return b.inner.Close(ctx)
}

// Lock passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) Lock(ctx context.Context) (context.Context, func() error, error) {
	return b.inner.Lock(ctx)
}

// LoadStore passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) LoadStore(ctx context.Context) (*store.Store, error) {
	return b.inner.LoadStore(ctx)
}

// ClearFiles passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) ClearFiles(ctx context.Context) error {
	return b.inner.ClearFiles(ctx)
}

// RecordProject passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) RecordProject(ctx context.Context, requirementsFile, downloadPath string) error {
	return b.inner.RecordProject(ctx, requirementsFile, downloadPath)
}

// LoadProjectRegistry passes through unmodified; only SaveStore is decided
// here.
func (b *cleanSaveSkipBackend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	return b.inner.LoadProjectRegistry(ctx)
}

// Artifacts passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) Artifacts() ArtifactStore {
	return b.inner.Artifacts()
}

// SweepTemp passes through unmodified; only SaveStore is decided here.
func (b *cleanSaveSkipBackend) SweepTemp(ctx context.Context) error {
	return b.inner.SweepTemp(ctx)
}
