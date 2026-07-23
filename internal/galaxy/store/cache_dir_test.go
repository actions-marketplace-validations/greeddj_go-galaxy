package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestClearCacheFilesPreservesLiveState proves --clear-cache never touches
// the live consolidated snapshot database: a store saved before the clear
// must still load with its data intact afterward, while an unrelated
// tarball is swept away.
func TestClearCacheFilesPreservesLiveState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	dbs, err := OpenDBs(dir, helpers.BoltOpenTimeout)
	if err != nil {
		t.Fatalf("OpenDBs error: %v", err)
	}
	st := New()
	st.SetInstalled("a.b@1.0.0", InstalledEntry{ArtifactSHA256: "abc"})
	if err := Save(dbs, st); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	if err := dbs.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	writeCacheFile(t, dir, "x.tar.gz", []byte("tarball"))
	writeCacheFile(t, dir, helpers.StoreDBLock, []byte("lock"))

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles error: %v", err)
	}

	assertFileAbsent(t, dir, "x.tar.gz")
	assertFileExists(t, dir, helpers.StoreDBLock)

	reopened, err := OpenDBs(dir, helpers.BoltOpenTimeout)
	if err != nil {
		t.Fatalf("reopen OpenDBs error: %v", err)
	}
	t.Cleanup(func() {
		_ = reopened.Close()
	})
	loaded, err := Load(reopened)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	installed, ok := loaded.GetInstalled("a.b@1.0.0")
	if !ok || installed.ArtifactSHA256 != "abc" {
		t.Fatalf("expected installed entry to survive clear, got %#v (ok=%v)", installed, ok)
	}
}

// TestIsDeleteCacheNameNeverDeletesLiveArtifacts is a focused unit check on
// the delete predicate: it must never mark the active lock file or the live
// consolidated database as safe to remove, since deleting either would open
// the flock inode-reuse hole (a second process re-creating the lock path
// while the first still holds a flock on the unlinked inode) or destroy the
// only copy of cached state.
func TestIsDeleteCacheNameNeverDeletesLiveArtifacts(t *testing.T) {
	t.Parallel()
	if isDeleteCacheName(helpers.StoreDBLock) {
		t.Fatalf("expected the lock file to never be in the delete set")
	}
	if isDeleteCacheName(helpers.StoreDBLocal) {
		t.Fatalf("expected the consolidated database to never be in the delete set")
	}
}

// TestClearCacheFilesPreservesLockFile confirms a plain lock file (not a
// real flock, so the test stays platform-neutral) survives ClearCacheFiles.
func TestClearCacheFilesPreservesLockFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCacheFile(t, dir, helpers.StoreDBLock, []byte("lock"))

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles error: %v", err)
	}
	assertFileExists(t, dir, helpers.StoreDBLock)
}

// TestClearCacheFilesReclaimsLegacySnapshotFiles confirms the nine
// pre-consolidation per-bucket snapshot files are reclaimed as orphans,
// since they are no longer opened by the consolidated single-file layout.
func TestClearCacheFilesReclaimsLegacySnapshotFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacyNames := []string{
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
	for _, name := range legacyNames {
		writeCacheFile(t, dir, name, []byte("legacy"))
	}

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles error: %v", err)
	}
	for _, name := range legacyNames {
		assertFileAbsent(t, dir, name)
	}
}

// TestClearCacheFilesRemovesArtifactSHASidecar confirms --clear-cache sweeps
// a sha256 sidecar left next to a cached tarball, so a stale digest can
// never survive a clear and be picked up against different bytes later.
func TestClearCacheFilesRemovesArtifactSHASidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCacheFile(t, dir, "x.tar.gz"+helpers.ArtifactSHASidecarSuffix, []byte(strings.Repeat("a", 64)))

	if err := ClearCacheFiles(dir); err != nil {
		t.Fatalf("ClearCacheFiles error: %v", err)
	}
	assertFileAbsent(t, dir, "x.tar.gz"+helpers.ArtifactSHASidecarSuffix)
}

// writeCacheFile writes a small file under dir, failing the test on error.
func writeCacheFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, helpers.FileMod); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
}

// assertFileExists fails the test unless the named file is present.
func assertFileExists(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("expected %s to exist, stat error: %v", name, err)
	}
}

// assertFileAbsent fails the test unless the named file is gone.
func assertFileAbsent(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, stat error: %v", name, err)
	}
}
