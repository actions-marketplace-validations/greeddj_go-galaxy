package store

import (
	"path/filepath"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// AcquireLock creates a lock file in cacheDir to prevent concurrent writers.
func AcquireLock(cacheDir string) (func() error, error) {
	if cacheDir == "" {
		return nil, helpers.ErrCacheDirEmpty
	}

	return flockFile(filepath.Join(cacheDir, helpers.StoreDBLock))
}
