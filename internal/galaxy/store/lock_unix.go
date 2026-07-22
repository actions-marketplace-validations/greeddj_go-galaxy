//go:build unix

package store

import (
	"errors"
	"os"
	"sync"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// flockFile opens lockPath and acquires a non-blocking exclusive advisory
// lock on it via flock(2). The lock file itself carries no content - it is
// a pure anchor for the kernel-held lock, so stale content left behind by a
// crashed process (or restored from a cache archive) can never be mistaken
// for a live lock. O_NOFOLLOW rejects a symlink planted at the lock path
// (e.g. inside a restored cache archive) rather than following it to another
// target. The returned closure releases the lock and closes the file
// descriptor exactly once.
func flockFile(lockPath string) (func() error, error) {
	//nolint:gosec // G304: lockPath is derived from the configured cache dir and names the cache lock file.
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, helpers.FileMod)
	if err != nil {
		return nil, err
	}

	fd := fd(f)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, helpers.ErrAnotherInstanceIsRunning
		}
		return nil, err
	}

	var once sync.Once
	release := func() error {
		var releaseErr error
		once.Do(func() {
			releaseErr = syscall.Flock(fd, syscall.LOCK_UN)
			if closeErr := f.Close(); releaseErr == nil {
				releaseErr = closeErr
			}
		})
		return releaseErr
	}

	return release, nil
}

// fd narrows an os.File descriptor to the int type syscall.Flock expects.
// File descriptors are small non-negative values on unix, so this
// conversion never overflows in practice despite the uintptr source type.
func fd(f *os.File) int {
	//nolint:gosec // G115: file descriptors are small non-negative values on unix.
	return int(f.Fd())
}
