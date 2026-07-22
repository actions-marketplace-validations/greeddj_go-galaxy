//go:build !unix

package store

import "errors"

// errLockUnsupportedPlatform indicates advisory flock-based locking is not
// available on the current platform.
var errLockUnsupportedPlatform = errors.New("advisory file locking is not supported on this platform")

// flockFile is the non-unix stub: this platform has no flock(2) equivalent
// wired up, so lock acquisition always fails rather than silently proceeding
// without mutual exclusion.
func flockFile(_ string) (func() error, error) {
	return nil, errLockUnsupportedPlatform
}
