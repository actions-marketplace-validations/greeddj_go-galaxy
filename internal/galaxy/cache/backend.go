// Package cache declares the persistence seam every implementation under
// internal/cache satisfies - Backend for state and locking, ArtifactStore for
// cached tarballs - and holds the behavior that belongs to the seam rather
// than to either side of it: the Backend decorators WithStateDeadline and
// WithCleanSaveSkip, the LockLostError verdict a caller turns a run's outcome
// into, and the cache-policy-aware JSON fetch a Galaxy metadata request goes
// through, with the Policy deciding whether it may read or write the
// snapshot's response cache.
//
// The concurrency contract splits along those two interfaces: a Backend value
// is used by one goroutine at a time, while the ArtifactStore it hands back is
// shared by every worker. Both interfaces state it in full; a new
// implementation is bound by what they say, not by what today's two do.
package cache

import (
	"context"
	"os"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// ArtifactFile describes a cached artifact file on disk.
type ArtifactFile struct {
	Cleanup func()
	Meta    map[string]string
	Path    string
	// SHA is the hex sha256 the backend itself computed over the returned
	// file's bytes while producing it. It is empty when the backend has only
	// sidecar-derived knowledge of the digest: the local backend leaves it
	// empty by design, reporting the sidecar's value through Meta instead.
	SHA string
}

// ArtifactStore provides access to cached collection artifacts.
//
// Every method is safe to call concurrently from multiple goroutines: install
// workers, warm workers, and the prefetcher all share one ArtifactStore value
// for the run's whole duration. That safety is scoped to distinct keys,
// though, not to any key at all: the caller is responsible for there being at
// most one goroutine working on any single key at a time, and a concurrent
// Commit and Delete of the same key is undefined by this contract.
type ArtifactStore interface {
	Has(ctx context.Context, key string) (bool, error)
	// Meta reports key's cached metadata without ever reading the artifact
	// body, under a tri-state contract every implementation must uphold
	// exactly: found=false, err=nil means key is not cached at all - the
	// identical meaning as Has's own false, nil; found=true with a nil or
	// empty map means key is cached but carries no recorded metadata; a
	// non-nil err means the store could not be consulted, and found carries
	// no meaning in that case. Has stays a separate method rather than being
	// replaced everywhere by this one: it sits on the install hot path
	// (isCacheHit) and the prefetch scan, and on the local backend Meta costs
	// an extra sidecar read those two callers would only discard.
	Meta(ctx context.Context, key string) (map[string]string, bool, error)
	Fetch(ctx context.Context, key string) (ArtifactFile, error)
	TempFile(ctx context.Context, prefix string) (*os.File, func(), error)
	Commit(ctx context.Context, key, tmpPath string, meta map[string]string) (ArtifactFile, error)
	Delete(ctx context.Context, key string) error
}

// Backend defines a cache backend for state and artifacts.
//
// A Backend value is not safe for concurrent use: its caller serializes
// access to it, so at most one goroutine calls any of its ten methods -
// Artifacts included - at any moment. Both implementations depend on that
// property rather than merely tolerating it, because each initializes its
// own mutable state lazily and writes it without synchronization: the local
// backend's ensureOpen writes its Bolt handle, and the S3 backend's Open
// writes its client and its artifact store. A caller that needs to hand a
// backend to multiple workers must add its own serialization around it; the
// interface guarantees none.
//
// Splitting Backend into a role per access model is deliberately not the
// answer here: the split a difference in access model would motivate already
// exists - the concurrent surface is already carved out as the separate
// ArtifactStore interface reached through Artifacts() - and Backend already
// has two implementations and two decorators (stateDeadlineBackend, in
// statedeadline.go, and cleanSaveSkipBackend, in dirtyskip.go), so splitting
// it into roles would mean either a decorator per role or a composite type
// assembling both, enlarging exactly the drift surface each decorator's own
// hand-written-methods shape works to shrink.
type Backend interface {
	Open(ctx context.Context) error
	Close(ctx context.Context) error
	// Lock takes the backend's exclusive lock and returns the HOLDER CONTEXT
	// alongside the release closure. The holder context is derived from ctx
	// and is canceled the moment the backend can no longer guarantee this run
	// still holds the lock; context.Cause of it then satisfies errors.Is(cause,
	// helpers.ErrCacheLockLost), which is what lets a caller turn a run's
	// outcome into that verdict through LockLostError. A backend whose lock
	// cannot be taken away from a live holder returns ctx unchanged, so a
	// caller needs no per-backend branch.
	//
	// The caller never cancels the returned context: it may be ctx itself, and
	// canceling it would then tear down the caller's own context out from
	// under everything else derived from it. Ownership of the cancellation
	// belongs to the release closure, and a backend that DOES derive a context
	// must cancel it from that closure, so a clean release leaves no child
	// hanging off the parent for the rest of the process's life.
	Lock(ctx context.Context) (context.Context, func() error, error)
	LoadStore(ctx context.Context) (*store.Store, error)
	SaveStore(ctx context.Context, st *store.Store) error
	ClearFiles(ctx context.Context) error
	// RecordProject enrolls the project behind requirementsFile in the
	// registry cleanup walks, with its collections path and its roles path;
	// an empty rolesPath records no roles path, which cleanup reads as
	// "do not scan" (see store.ProjectRecord).
	RecordProject(ctx context.Context, requirementsFile, downloadPath, rolesPath string) error
	LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error)
	// Artifacts returns the backend's artifact store. Call it only after a
	// successful Open: an implementation may build its artifact store inside
	// Open (the S3 backend does), so a call made before Open has succeeded can
	// return a store that is not yet usable. Unlike the Backend value itself,
	// the returned ArtifactStore is intended for concurrent use - see its own
	// doc comment.
	Artifacts() ArtifactStore
	// SweepTemp deletes temporary artifact files left by a previously killed
	// run. It must be called only while the caller holds the backend's
	// exclusive lock, so every matching entry is provably a dead-run orphan and
	// no live writer's in-flight temp can be removed.
	SweepTemp(ctx context.Context) error
}
