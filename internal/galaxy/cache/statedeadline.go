package cache

import (
	"context"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// stateDeadlineBackend is a Backend decorator that bounds the four
// persisted cache-state operations - LoadStore, SaveStore,
// LoadProjectRegistry, RecordProject - with a shared wall-clock budget,
// while passing every other method through unmodified. See WithStateDeadline
// for why a decorator, rather than nine separate context.WithTimeout call
// sites, is the right shape for this.
type stateDeadlineBackend struct {
	Backend

	budget time.Duration
}

// WithStateDeadline wraps b so its four persisted cache-state operations -
// LoadStore, SaveStore, LoadProjectRegistry, RecordProject - each run under
// their own context.WithTimeout(parent, budget), with a stalled operation
// normalized into helpers.ErrStateObjectDeadline instead of hanging (or
// blocking) for as long as the caller's own context allows. budget <= 0
// falls back to helpers.StateObjectDeadline. b == nil returns nil rather than
// wrapping a nil interface into a non-nil one, which would otherwise make
// every method call on the result panic on a nil pointer dereference instead
// of behaving like an absent backend.
//
// Every other Backend method - Open, Close, Lock, ClearFiles, SweepTemp,
// Artifacts - passes through to b verbatim; see the doc comments on the
// pass-through methods below for why each one is deliberately left
// unbounded by this budget.
//
// This is a decorator rather than nine separate context.WithTimeout pairs at
// each call site because SaveStore alone has five call sites across two
// packages (finalizeInstall, warmWithState, lockWithState,
// saveDryRunSnapshotIfPersisted, finalizeCleanup): with the decorator, the
// entire production diff outside this file is the two call sites that
// construct it (initInstall, initCleanup), finalizeInstall's
// annotateSaveFailure handling is untouched, and "every state operation is
// bounded" becomes a property of the Backend value itself rather than a
// convention nine call sites must each remember to honor. Nothing in this
// codebase type-asserts a Backend to a concrete type, which is what makes
// wrapping it in a decorator safe here.
//
// The budget is applied at this seam, the caller's side of Backend, and
// never inside internal/cache/s3 or internal/cache/local, for the identical
// reason helpers.ArtifactDownloadDeadline is applied in
// internal/galaxy/collections rather than inside a cache backend: no
// constant, sentinel, or policy from internal/galaxy/{cache,helpers} may
// cross the Backend seam. This is structural, not just stylistic, for the
// local backend specifically: local.Backend's LoadStore, SaveStore,
// LoadProjectRegistry, and RecordProject all take a context parameter named
// "_" and ignore it entirely, so wrapping local.Backend in this decorator
// makes the budget inert for it BY CONSTRUCTION - exactly as
// ArtifactDownloadDeadline is inert for local.Artifacts.Fetch, which also
// ignores the context deadline a caller layers on top of it.
//
// The drift hazard this shape introduces: stateDeadlineBackend embeds
// Backend rather than implementing every method by hand, so a method added
// to the Backend interface later passes through this decorator unbounded by
// default, with no compile error to flag it. Any future Backend method that
// touches a persisted state object must be given its own explicit bounded
// override here, the same way LoadStore/SaveStore/LoadProjectRegistry/
// RecordProject already are; it does not become bounded on its own just by
// virtue of being added to the interface.
func WithStateDeadline(b Backend, budget time.Duration) Backend {
	if b == nil {
		return nil
	}
	if budget <= 0 {
		budget = helpers.StateObjectDeadline
	}
	return &stateDeadlineBackend{Backend: b, budget: budget}
}

// LoadStore bounds b.LoadStore with this decorator's budget.
func (b *stateDeadlineBackend) LoadStore(ctx context.Context) (*store.Store, error) {
	dlCtx, cancel := context.WithTimeout(ctx, b.budget)
	defer cancel()
	st, err := b.Backend.LoadStore(dlCtx)
	return st, deadlineError(ctx, dlCtx, b.budget, helpers.ErrStateObjectDeadline, err)
}

// SaveStore bounds b.SaveStore with this decorator's budget. This is the
// largest legitimate operation the budget bounds: for the S3 backend, it
// covers Store.MarshalSnapshot's own copy+encode alongside the gzip and the
// PUT, all inside the same budget LoadStore spends on the GET and inflate
// alone.
func (b *stateDeadlineBackend) SaveStore(ctx context.Context, st *store.Store) error {
	dlCtx, cancel := context.WithTimeout(ctx, b.budget)
	defer cancel()
	err := b.Backend.SaveStore(dlCtx, st)
	return deadlineError(ctx, dlCtx, b.budget, helpers.ErrStateObjectDeadline, err)
}

// LoadProjectRegistry bounds b.LoadProjectRegistry with this decorator's
// budget.
func (b *stateDeadlineBackend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	dlCtx, cancel := context.WithTimeout(ctx, b.budget)
	defer cancel()
	registry, err := b.Backend.LoadProjectRegistry(dlCtx)
	return registry, deadlineError(ctx, dlCtx, b.budget, helpers.ErrStateObjectDeadline, err)
}

// RecordProject bounds b.RecordProject with this decorator's budget.
func (b *stateDeadlineBackend) RecordProject(ctx context.Context, requirementsFile, downloadPath string) error {
	dlCtx, cancel := context.WithTimeout(ctx, b.budget)
	defer cancel()
	err := b.Backend.RecordProject(dlCtx, requirementsFile, downloadPath)
	return deadlineError(ctx, dlCtx, b.budget, helpers.ErrStateObjectDeadline, err)
}

// Open passes through unmodified: it is ensureBucket plus
// probeConditionalPut, both tiny fixed-size bodies with no drip surface
// worth a dedicated budget. A caller that skips calling Open explicitly
// still gets it run inside LoadStore/SaveStore/etc.'s own lazy Open, which is
// then bounded by that operation's budget; both production call sites in
// this codebase call Open explicitly before any state operation, so that lazy
// path is a no-op by the time it would matter.
func (b *stateDeadlineBackend) Open(ctx context.Context) error {
	return b.Backend.Open(ctx)
}

// Close passes through unmodified: it releases in-process resources and
// issues no network call in either backend.
func (b *stateDeadlineBackend) Close(ctx context.Context) error {
	return b.Backend.Close(ctx)
}

// Lock passes through unmodified: the distributed lock protocol carries its
// own timings (lockWaitCeiling, heartbeatOpTimeout, lockReleaseTimeout), and
// bounding the whole acquisition with a 60-second state-object budget would
// break legitimate contention outright - a lock wait can and does take
// longer than that while a live holder finishes its own work.
func (b *stateDeadlineBackend) Lock(ctx context.Context) (func() error, error) {
	return b.Backend.Lock(ctx)
}

// ClearFiles passes through unmodified: --clear-cache's bulk delete is
// legitimate work proportional to how much the cache holds, which is
// unbounded, so no fixed budget is defensible here. The residual is
// disclosed rather than hidden: a drip on one ListObjectsV2 page during
// --clear-cache still holds the backend lock for as long as that page's
// drip runs, reachable only when the operator explicitly asked for this
// destructive bulk operation.
func (b *stateDeadlineBackend) ClearFiles(ctx context.Context) error {
	return b.Backend.ClearFiles(ctx)
}

// SweepTemp passes through unmodified: it is local-only work (the S3
// backend's implementation is a no-op), with no state-object read or write
// for a budget to bound.
func (b *stateDeadlineBackend) SweepTemp(ctx context.Context) error {
	return b.Backend.SweepTemp(ctx)
}

// Artifacts passes through unmodified, returning the underlying store
// unwrapped: an artifact read or write gets its own budget
// (helpers.ArtifactDownloadDeadline) in internal/galaxy/collections, not this
// one.
func (b *stateDeadlineBackend) Artifacts() ArtifactStore {
	return b.Backend.Artifacts()
}
