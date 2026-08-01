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
}

// ArtifactStore provides access to cached collection artifacts.
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
type Backend interface {
	Open(ctx context.Context) error
	Close(ctx context.Context) error
	Lock(ctx context.Context) (func() error, error)
	LoadStore(ctx context.Context) (*store.Store, error)
	SaveStore(ctx context.Context, st *store.Store) error
	ClearFiles(ctx context.Context) error
	RecordProject(ctx context.Context, requirementsFile, downloadPath string) error
	LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error)
	Artifacts() ArtifactStore
	// SweepTemp deletes temporary artifact files left by a previously killed
	// run. It must be called only while the caller holds the backend's
	// exclusive lock, so every matching entry is provably a dead-run orphan and
	// no live writer's in-flight temp can be removed.
	SweepTemp(ctx context.Context) error
}
