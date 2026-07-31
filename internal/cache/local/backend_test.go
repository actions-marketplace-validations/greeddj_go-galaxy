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
