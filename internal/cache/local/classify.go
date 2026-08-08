package local

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// classifyCacheFailure puts this backend's failures into the same
// cache-backend classes the S3 backend already speaks, so one operator
// mistake produces one exit code regardless of which backend was configured.
//
// The split is deliberately two-way rather than per-errno, because a table of
// error numbers is a second rule shape nobody would keep current:
//
//   - A permission failure is helpers.ErrCacheBackendUnusable. It is a durable
//     property of the configured location: no retry repairs it, and the remedy
//     is to fix the location or point somewhere else - the same predicate that
//     class already carries for an S3 endpoint that cannot be addressed.
//   - Anything else is helpers.ErrCacheBackendUnavailable. The store was there
//     and the operation still failed for a reason that is not this program's
//     own doing - a full disk, an I/O error, a vanished mount.
//
// Three kinds of error pass through untouched, and each for its own reason.
// An error already carrying one of the cache classes keeps it, so a producer
// that knows more than this function does is never overruled. An absent file
// keeps its fs.ErrNotExist identity alone, because absence is a normal outcome
// several callers branch on, and because it already classifies as a usage
// error - adding a class here would silently move it. helpers.ErrCacheDirEmpty
// is an operator's own configuration mistake and is deliberately a usage error
// on both backends.
func classifyCacheFailure(err error) error {
	if err == nil || alreadyClassified(err) {
		return err
	}
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %w", helpers.ErrCacheBackendUnusable, err)
	}
	return fmt.Errorf("%w: %w", helpers.ErrCacheBackendUnavailable, err)
}

// alreadyClassified reports whether err carries a verdict this function must
// not overrule or move. See classifyCacheFailure for why each member is here.
func alreadyClassified(err error) bool {
	return errors.Is(err, helpers.ErrCacheBackendUnavailable) ||
		errors.Is(err, helpers.ErrCacheBackendUnusable) ||
		errors.Is(err, helpers.ErrCacheBusy) ||
		errors.Is(err, helpers.ErrAnotherInstanceIsRunning) ||
		errors.Is(err, helpers.ErrCorruptSnapshotStore) ||
		errors.Is(err, helpers.ErrCorruptProjectRegistry) ||
		errors.Is(err, helpers.ErrUnsupportedSchemaVersion) ||
		errors.Is(err, helpers.ErrCacheDirEmpty) ||
		errors.Is(err, fs.ErrNotExist)
}
