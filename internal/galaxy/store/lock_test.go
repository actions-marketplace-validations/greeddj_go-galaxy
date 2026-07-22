package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestAcquireLockRejectsSymlinkedLockFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), helpers.FileMod); err != nil {
		t.Fatalf("failed to create symlink target: %v", err)
	}
	// Plant a symlink where the lock file would be, as a poisoned cache
	// archive might. O_NOFOLLOW must make the open fail rather than redirect
	// it to the target.
	lockPath := filepath.Join(dir, helpers.StoreDBLock)
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatalf("failed to plant symlink: %v", err)
	}

	release, err := AcquireLock(dir)
	if err == nil {
		if release != nil {
			_ = release()
		}
		t.Fatal("expected AcquireLock to reject a symlinked lock path, got nil error")
	}
	if errors.Is(err, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("expected an open error for the symlink, got contention error: %v", err)
	}
}

func TestAcquireLockEmptyCacheDir(t *testing.T) {
	t.Parallel()

	release, err := AcquireLock("")
	if !errors.Is(err, helpers.ErrCacheDirEmpty) {
		t.Fatalf("expected ErrCacheDirEmpty, got %v", err)
	}
	if release != nil {
		t.Fatalf("expected nil release closure on error, got non-nil")
	}
}

func TestAcquireLockContention(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock failed: %v", err)
	}
	defer func() {
		if err := first(); err != nil {
			t.Errorf("first release failed: %v", err)
		}
	}()

	second, err := AcquireLock(dir)
	if !errors.Is(err, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("expected ErrAnotherInstanceIsRunning, got %v", err)
	}
	if second != nil {
		t.Fatalf("expected nil release closure on contention, got non-nil")
	}
}

func TestAcquireLockRecoversAfterRelease(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock failed: %v", err)
	}
	// Releasing closes the held FD, which is exactly what the kernel does
	// when the owning process dies - the lock must become acquirable again.
	if err := release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	second, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after release failed: %v", err)
	}
	defer func() {
		if err := second(); err != nil {
			t.Errorf("second release failed: %v", err)
		}
	}()
}

func TestAcquireLockIgnoresStaleRestoredLockFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, helpers.StoreDBLock)

	// Simulate a lock file restored from a CI cache archive: it names a
	// live-looking PID but no flock is actually held on it by anyone.
	content := fmt.Appendf(nil, `{"pid":%d}`, os.Getpid())
	if err := os.WriteFile(lockPath, content, helpers.FileMod); err != nil {
		t.Fatalf("failed to seed stale lock file: %v", err)
	}

	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("expected AcquireLock to succeed over stale content, got %v", err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release failed: %v", err)
		}
	}()
}

func TestAcquireLockReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock failed: %v", err)
	}

	if err := release(); err != nil {
		t.Fatalf("first release failed: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("second release failed: %v", err)
	}
}
