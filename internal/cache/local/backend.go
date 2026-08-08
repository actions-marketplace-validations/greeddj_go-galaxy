// Package local implements the cache backend backed by the local filesystem.
// Snapshot state lives in a single BoltDB file under the configured cache
// directory, the project registry is a JSON file beside it, a cached artifact
// is one plain file whose key is a single path element joined onto its own
// directory, and exclusive access is an advisory flock. That flock cannot be
// taken from a live holder, so Lock returns the caller's own context as the
// holder context rather than deriving one.
//
// A failure this package produces is labeled by classifyCacheFailure, which
// puts it into the same cache-backend classes the S3 backend speaks so that
// one operator mistake yields one exit code whichever backend was configured.
// Its own doc comment holds the two classes it assigns and the three shapes it
// deliberately passes through unlabeled.
package local

import (
	"context"
	"os"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// Backend provides a filesystem-backed cache backend.
type Backend struct {
	dbs       *store.DBs
	artifacts *Artifacts
	cacheDir  string
}

// New creates a Backend rooted at cacheDir.
func New(cacheDir string) *Backend {
	return &Backend{
		cacheDir:  cacheDir,
		artifacts: NewArtifacts(cacheDir),
	}
}

// Open ensures the cache directory exists. Bolt files are opened lazily by
// ensureOpen so the instance lock can be taken first, keeping a second
// process from hanging on an unbounded Bolt file lock.
func (b *Backend) Open(_ context.Context) error {
	return classifyCacheFailure(b.ensureDir())
}

// Close releases any open resources.
func (b *Backend) Close(_ context.Context) error {
	if b.dbs == nil {
		return nil
	}
	err := b.dbs.Close()
	b.dbs = nil
	return classifyCacheFailure(err)
}

// Lock obtains an exclusive lock for the cache directory. It ensures the
// directory exists first so the lock file has a parent even if Open was
// never called, then acquires the lock before any Bolt file is opened.
//
// The holder context is ctx itself, unchanged. That is a property of a
// flock(2) rather than an unimplemented half of the Backend contract: the
// kernel holds the advisory lock for as long as this process holds the
// descriptor open, and no other process can take it away: a contender's own
// non-blocking flock fails immediately (helpers.ErrAnotherInstanceIsRunning)
// instead of displacing this holder.
// There is therefore no moment at which this backend could learn it had lost
// ownership, and the holder context coincides with the parent by
// construction. cacheManager.LockLostError reads context.Cause on whatever
// this returns, so returning ctx also means a caller's own cancellation is
// reported as cancellation rather than as a lost lock.
func (b *Backend) Lock(ctx context.Context) (context.Context, func() error, error) {
	if err := b.ensureDir(); err != nil {
		return nil, nil, classifyCacheFailure(err)
	}
	release, err := store.AcquireLock(b.cacheDir)
	if err != nil {
		return nil, nil, classifyCacheFailure(err)
	}
	return ctx, release, nil
}

// LoadStore loads the persistent snapshot store.
func (b *Backend) LoadStore(_ context.Context) (*store.Store, error) {
	if err := b.ensureOpen(); err != nil {
		return nil, classifyCacheFailure(err)
	}
	st, err := store.Load(b.dbs)
	if err != nil {
		return nil, classifyCacheFailure(err)
	}
	return st, nil
}

// SaveStore persists the snapshot store.
func (b *Backend) SaveStore(_ context.Context, st *store.Store) error {
	if err := b.ensureOpen(); err != nil {
		return classifyCacheFailure(err)
	}
	return classifyCacheFailure(store.Save(b.dbs, st))
}

// ClearFiles removes cached artifact files from disk.
func (b *Backend) ClearFiles(_ context.Context) error {
	if b.cacheDir == "" {
		return helpers.ErrCacheDirEmpty
	}
	return classifyCacheFailure(store.ClearCacheFiles(b.cacheDir))
}

// RecordProject records the project in the local registry.
func (b *Backend) RecordProject(_ context.Context, requirementsFile, downloadPath string) error {
	if b.cacheDir == "" {
		return helpers.ErrCacheDirEmpty
	}
	return classifyCacheFailure(store.RecordProject(b.cacheDir, requirementsFile, downloadPath))
}

// LoadProjectRegistry loads the local project registry.
func (b *Backend) LoadProjectRegistry(_ context.Context) (*store.ProjectRegistry, error) {
	if b.cacheDir == "" {
		return nil, helpers.ErrCacheDirEmpty
	}
	registry, err := store.LoadProjectRegistry(b.cacheDir)
	if err != nil {
		return nil, classifyCacheFailure(err)
	}
	return registry, nil
}

// Artifacts returns the artifact store for the backend.
func (b *Backend) Artifacts() cacheManager.ArtifactStore {
	return b.artifacts
}

// SweepTemp removes download-temp files left in the cache directory by a
// previously killed run. Safe only under the backend's exclusive lock, which
// the caller holds; a killed run is the only way these outlive their per-run
// cleanup.
func (b *Backend) SweepTemp(_ context.Context) error {
	if b.cacheDir == "" {
		return helpers.ErrCacheDirEmpty
	}
	return classifyCacheFailure(store.SweepDownloadTemps(b.cacheDir))
}

// ensureOpen lazily opens the Bolt snapshot files. Deferring this past
// directory creation lets callers take the instance lock first, so a
// second process on the same cache dir fails fast via BoltOpenTimeout
// instead of blocking forever on Bolt's own file lock.
func (b *Backend) ensureOpen() error {
	if b.dbs != nil {
		return nil
	}
	if err := b.ensureDir(); err != nil {
		return err
	}
	dbs, err := store.OpenDBs(b.cacheDir, helpers.BoltOpenTimeout)
	if err != nil {
		return err
	}
	b.dbs = dbs
	return nil
}

// ensureDir validates the cache directory is configured and creates it if
// missing, without touching any Bolt file.
func (b *Backend) ensureDir() error {
	if b.cacheDir == "" {
		return helpers.ErrCacheDirEmpty
	}
	return os.MkdirAll(b.cacheDir, helpers.DirMod)
}
