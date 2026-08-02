package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
)

func TestBackendOpenDoesNotOpenBolt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	metaPath := filepath.Join(dir, helpers.StoreSnapshotMeta)

	// Hold the meta DB's file lock externally. If Open still touched Bolt,
	// it would either fail (timeout) or hang - here it must do neither.
	holder, err := bolt.Open(metaPath, helpers.FileMod, nil)
	if err != nil {
		t.Fatalf("failed to open holder DB: %v", err)
	}
	defer func() {
		_ = holder.Close()
	}()

	b := New(dir)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("expected Open to succeed without touching Bolt, got %v", err)
	}
}

// TestBackendOpenRejectsEmptyCacheDir confirms ensureDir's guard surfaces the
// shared helpers.ErrCacheDirEmpty sentinel - not a package-private
// duplicate - so cmd/go-galaxy/exitcode's isConfigUsageError check (which
// matches on helpers.ErrCacheDirEmpty alone) still classifies an empty
// cfg.CacheDir as a usage error for the local backend.
func TestBackendOpenRejectsEmptyCacheDir(t *testing.T) {
	t.Parallel()

	b := New("")
	if err := b.Open(context.Background()); !errors.Is(err, helpers.ErrCacheDirEmpty) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheDirEmpty), got %v", err)
	}
}

// TestBackendClearFilesRejectsEmptyCacheDir confirms ClearFiles' own empty-
// cacheDir guard, mirroring TestBackendOpenRejectsEmptyCacheDir above. This
// is a uniformity guard across the Backend surface, not a path a real run
// can reach: backend.Open already fails first via ensureDir for an empty
// cfg.CacheDir, so no caller reaches ClearFiles with one in practice. What
// it buys is specific: without it, store.ClearCacheFiles("") would reach
// os.ReadDir(""), whose resulting ErrNotExist is swallowed by
// ClearCacheFiles' own not-exist arm - so a Backend constructed directly
// with an empty cacheDir (bypassing Open) would report silent success on a
// destructive operation instead of failing loudly.
func TestBackendClearFilesRejectsEmptyCacheDir(t *testing.T) {
	t.Parallel()

	t.Run("empty cache dir", func(t *testing.T) {
		t.Parallel()
		b := New("")
		if err := b.ClearFiles(context.Background()); !errors.Is(err, helpers.ErrCacheDirEmpty) {
			t.Fatalf("expected errors.Is(err, helpers.ErrCacheDirEmpty), got %v", err)
		}
	})

	// The positive control: a real cacheDir reaches and passes ClearFiles,
	// proving "empty cache dir" above is refused by the guard and not by
	// some unrelated failure that would refuse any cacheDir.
	t.Run("real cache dir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCacheFile(t, dir, "acme-widgets-1.0.0.tar.gz", []byte("tarball"))

		b := New(dir)
		if err := b.ClearFiles(context.Background()); err != nil {
			t.Fatalf("ClearFiles: %v", err)
		}
		assertFileAbsent(t, dir, "acme-widgets-1.0.0.tar.gz")
	})
}

func TestBackendLockFailsFastWhenHeld(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := context.Background()

	b1 := openedBackend(t, dir)
	release1 := lockOrFatal(t, b1)

	b2 := openedBackend(t, dir)
	assertLockFailsFast(t, b2)

	if err := release1(); err != nil {
		t.Fatalf("release1 failed: %v", err)
	}

	release2 := lockOrFatal(t, b2)
	if err := release2(); err != nil {
		t.Fatalf("release2 failed: %v", err)
	}

	if err := b1.Close(ctx); err != nil {
		t.Fatalf("b1.Close failed: %v", err)
	}
	if err := b2.Close(ctx); err != nil {
		t.Fatalf("b2.Close failed: %v", err)
	}
}

// TestBackendSweepTempRemovesDownloadOrphansKeepsRealFiles proves SweepTemp
// removes only leftover download-temp files: a committed tarball, its sha256
// sidecar, and the consolidated Bolt database must all survive.
func TestBackendSweepTempRemovesDownloadOrphansKeepsRealFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeCacheFile(t, dir, helpers.ArtifactDownloadTempPrefix+"abc", []byte("orphan"))
	writeCacheFile(t, dir, helpers.ArtifactDownloadTempPrefix+"xyz", []byte("orphan"))
	writeCacheFile(t, dir, "acme-widgets-1.0.0.tar.gz", []byte("tarball"))
	writeCacheFile(t, dir, "acme-widgets-1.0.0.tar.gz"+helpers.ArtifactSHASidecarSuffix, []byte("sha256"))
	writeCacheFile(t, dir, helpers.StoreDBLocal, []byte("bolt"))

	b := New(dir)
	if err := b.SweepTemp(context.Background()); err != nil {
		t.Fatalf("SweepTemp error: %v", err)
	}

	assertFileAbsent(t, dir, helpers.ArtifactDownloadTempPrefix+"abc")
	assertFileAbsent(t, dir, helpers.ArtifactDownloadTempPrefix+"xyz")
	assertFileExists(t, dir, "acme-widgets-1.0.0.tar.gz")
	assertFileExists(t, dir, "acme-widgets-1.0.0.tar.gz"+helpers.ArtifactSHASidecarSuffix)
	assertFileExists(t, dir, helpers.StoreDBLocal)
}

// TestBackendSweepTempMissingDirIsNoError proves SweepTemp treats a
// not-yet-created cache directory as nothing to sweep, matching
// store.SweepDownloadTemps' own ErrNotExist handling.
func TestBackendSweepTempMissingDirIsNoError(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	b := New(dir)
	if err := b.SweepTemp(context.Background()); err != nil {
		t.Fatalf("expected nil error for a missing cache dir, got %v", err)
	}
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

// openedBackend creates and opens a Backend rooted at dir, failing the test
// on any error.
func openedBackend(t *testing.T, dir string) *Backend {
	t.Helper()
	b := New(dir)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return b
}

// lockOrFatal acquires b's lock, failing the test on any error.
func lockOrFatal(t *testing.T, b *Backend) func() error {
	t.Helper()
	release, err := b.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}
	return release
}

// assertLockFailsFast asserts that b.Lock returns ErrAnotherInstanceIsRunning
// promptly instead of blocking, proving the lock is taken before any Bolt
// file is opened.
func assertLockFailsFast(t *testing.T, b *Backend) {
	t.Helper()

	type lockResult struct {
		release func() error
		err     error
	}
	resultCh := make(chan lockResult, 1)
	go func() {
		release, err := b.Lock(context.Background())
		resultCh <- lockResult{release: release, err: err}
	}()

	select {
	case res := <-resultCh:
		if !errors.Is(res.err, helpers.ErrAnotherInstanceIsRunning) {
			t.Fatalf("expected ErrAnotherInstanceIsRunning, got %v", res.err)
		}
		if res.release != nil {
			t.Fatalf("expected nil release on failed lock, got non-nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lock hung instead of failing fast")
	}
}

// TestBackendClassifiesItsOwnFailures pins that this backend now speaks the
// same cache-backend classes the S3 one does. Before it, a filesystem failure
// came back bare and exited 1, while the S3 backend against a store it could
// not reach exited 4 - one operator mistake, two exit codes, depending on a
// backend choice a CI branching on the exit code does not know about.
//
// The rows are the two halves of the split and the two verdicts that must not
// move. Contention is the sharpest of those: it is the one class this backend
// always had, and a classifier that swallowed it would turn "another process
// holds the cache" into "the cache is unusable", which is exactly the wrong
// advice.
func TestBackendClassifiesItsOwnFailures(t *testing.T) {
	t.Parallel()

	t.Run("permission failure is unusable", func(t *testing.T) {
		t.Parallel()
		dir := readOnlyDir(t)
		// Lock, not Open: os.MkdirAll returns nil for a directory that already
		// exists whatever its mode, so Open on a read-only cache directory
		// succeeds and the permission failure lands where the lock file is
		// created - which is exactly where a real run meets it.
		_, err := New(dir).Lock(context.Background())
		if !errors.Is(err, helpers.ErrCacheBackendUnusable) {
			t.Fatalf("Lock on a read-only cache dir = %v, want errors.Is helpers.ErrCacheBackendUnusable", err)
		}
	})

	t.Run("other filesystem failure is unavailable", func(t *testing.T) {
		t.Parallel()
		// A regular file where a parent directory belongs: the resulting
		// ENOTDIR is neither a permission problem nor an absence, which is
		// the whole "everything else" half of the split.
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), helpers.FileMod); err != nil {
			t.Fatalf("write blocker: %v", err)
		}
		err := New(filepath.Join(blocker, "cache")).Open(context.Background())
		if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
			t.Fatalf("Open under a regular file = %v, want errors.Is helpers.ErrCacheBackendUnavailable", err)
		}
	})

	t.Run("an empty cache dir stays a usage error", func(t *testing.T) {
		t.Parallel()
		err := New("").Open(context.Background())
		if !errors.Is(err, helpers.ErrCacheDirEmpty) {
			t.Fatalf("Open with no cache dir = %v, want errors.Is helpers.ErrCacheDirEmpty", err)
		}
		if errors.Is(err, helpers.ErrCacheBackendUnusable) || errors.Is(err, helpers.ErrCacheBackendUnavailable) {
			t.Errorf("an operator's own configuration mistake must not be reclassified: %v", err)
		}
	})

	t.Run("contention keeps its own class", testContentionKeepsItsOwnClass)
}

// testContentionKeepsItsOwnClass is the row above, split out to keep its
// parent within the cyclomatic-complexity budget. It is the sharpest of the
// four: contention is the one class this backend always spoke, and a
// classifier that swallowed it would turn "another process holds the cache"
// into "the cache is unusable", which is the wrong advice entirely.
func testContentionKeepsItsOwnClass(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	release, err := New(dir).Lock(context.Background())
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	defer func() { _ = release() }()

	_, err = New(dir).Lock(context.Background())
	if !errors.Is(err, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("second Lock = %v, want errors.Is helpers.ErrAnotherInstanceIsRunning", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnusable) || errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Errorf("contention must not be reclassified as a backend problem: %v", err)
	}
}

// readOnlyDir returns a directory this process cannot write to, restoring its
// mode afterwards so t.TempDir's own cleanup can remove it.
func readOnlyDir(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	//nolint:gosec // 0o555 is a directory mode (read+traverse, no write), which is the condition under test.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, helpers.DirMod); err != nil {
			t.Errorf("restoring directory mode: %v", err)
		}
	})
	// A process running as root ignores the mode entirely, which would make
	// the caller assert against a directory it can still write to. Prove the
	// condition holds before returning it.
	probe := filepath.Join(dir, "writable-probe")
	if err := os.WriteFile(probe, []byte("x"), helpers.FileMod); err == nil {
		_ = os.Remove(probe)
		t.Skip("this process can write to a 0o555 directory (running as root?), so the permission case is unreachable here")
	}
	return dir
}
