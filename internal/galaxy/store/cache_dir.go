package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ClearCacheFiles removes cache files that are safe to delete.
func ClearCacheFiles(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !shouldDeleteCacheFile(name) {
			continue
		}
		if err := removeCacheFile(cacheDir, name); err != nil {
			return err
		}
	}
	return nil
}

func shouldDeleteCacheFile(name string) bool {
	if isDeleteCacheName(name) {
		return true
	}
	if isKeepCacheName(name) {
		return false
	}
	return strings.HasSuffix(name, ".tar.gz") || strings.HasPrefix(name, ".download-") || strings.HasSuffix(name, ".tmp")
}

// isDeleteCacheName reports whether name is a file that is always safe to
// remove on --clear-cache. This is the nine per-bucket snapshot files from
// the pre-consolidation layout: the snapshot store now lives entirely in
// the single Bolt database named by helpers.StoreDBLocal, so these nine
// legacy files are never opened anymore and any that remain on disk are
// orphans from an older binary that can be reclaimed unconditionally. The
// active lock file is deliberately excluded: unlinking a lock file while
// another process still holds its flock lets a third process create a new
// inode at the same path and flock it too, so two processes would both
// believe they hold the lock.
func isDeleteCacheName(name string) bool {
	deleteList := []string{
		helpers.StoreSnapshotMeta,
		helpers.StoreSnapshotAPICache,
		helpers.StoreSnapshotDepsCache,
		helpers.StoreSnapshotInstalled,
		helpers.StoreSnapshotGraph,
		helpers.StoreSnapshotRequirements,
		helpers.StoreSnapshotRoots,
		helpers.StoreSnapshotResolved,
		helpers.StoreSnapshotVersions,
	}
	return slices.Contains(deleteList, name)
}

// isKeepCacheName reports whether name is a live artifact of the current
// (consolidated) cache layout that --clear-cache must never remove: the
// single Bolt snapshot database, the active lock file, and the project
// registry that cleanup relies on.
func isKeepCacheName(name string) bool {
	keepList := []string{
		helpers.StoreDBLocal,
		helpers.StoreDBLock,
		helpers.StoreDBProjects,
	}

	return slices.Contains(keepList, name)
}

func removeCacheFile(cacheDir, name string) error {
	if err := os.Remove(filepath.Join(cacheDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
