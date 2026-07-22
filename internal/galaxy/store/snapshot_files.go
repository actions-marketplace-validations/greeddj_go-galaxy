package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// DBs holds BoltDB handles for snapshot storage buckets.
type DBs struct {
	meta         *bolt.DB
	apiCache     *bolt.DB
	depsCache    *bolt.DB
	installed    *bolt.DB
	graph        *bolt.DB
	requirements *bolt.DB
	roots        *bolt.DB
	resolved     *bolt.DB
	versions     *bolt.DB
}

// OpenDBs opens all snapshot BoltDB files under cacheDir. Each file is
// opened with timeout bounding how long it waits on another process's
// flock, so a busy cache fails fast instead of hanging forever.
func OpenDBs(cacheDir string, timeout time.Duration) (*DBs, error) {
	dbs := &DBs{}
	var err error

	dbs.meta, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotMeta), timeout)
	if err != nil {
		return nil, err
	}
	dbs.apiCache, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotAPICache), timeout)
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.depsCache, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotDepsCache), timeout)
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.installed, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotInstalled), timeout)
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.graph, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotGraph), timeout)
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.requirements, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotRequirements), timeout)
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.roots, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotRoots), timeout)
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.resolved, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotResolved), timeout)
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}
	dbs.versions, err = openBolt(filepath.Join(cacheDir, helpers.StoreSnapshotVersions), timeout)
	if err != nil {
		_ = dbs.Close()
		return nil, err
	}

	return dbs, nil
}

// Close closes all open BoltDB handles.
func (s *DBs) Close() error {
	if s == nil {
		return nil
	}
	var firstErr error
	closeDB := func(db *bolt.DB) {
		if db == nil {
			return
		}
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	closeDB(s.meta)
	closeDB(s.apiCache)
	closeDB(s.depsCache)
	closeDB(s.installed)
	closeDB(s.graph)
	closeDB(s.requirements)
	closeDB(s.roots)
	closeDB(s.resolved)
	closeDB(s.versions)
	return firstErr
}

// openBolt opens a Bolt database at the given path, bounding how long it
// waits to acquire the file's flock. A timeout means another process
// currently holds the file, so it is reported as a busy cache rather than
// left to hang.
func openBolt(path string, timeout time.Duration) (*bolt.DB, error) {
	db, err := bolt.Open(path, helpers.FileMod, &bolt.Options{Timeout: timeout})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, fmt.Errorf("%w: %w", helpers.ErrCacheBusy, err)
		}
		return nil, err
	}
	return db, nil
}
