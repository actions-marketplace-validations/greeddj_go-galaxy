package cleanup

import (
	"errors"
	"fmt"
	"os"

	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

// errRequirementsNotRegular names a recorded project's requirements file
// whose path Stat succeeds against, but which is not a regular file - a
// fifo, a directory, a socket, or any other non-regular shape sitting at
// that path. It never leaves this package: loadRequirements's only caller,
// projectRequirementRoots, folds it into helpers.ErrProjectRequirementsUnreadable
// exactly like any other non-fs.ErrNotExist load failure, the same pattern
// errWorkspaceUnrooted already follows for the workspace side (cleanup.go).
// It is deliberately distinct from fs.ErrNotExist so a non-regular entry
// aborts the run instead of being tolerated as a stale registry entry - the
// two states genuinely differ: a missing file means "this project declares
// nothing", while a fifo or directory in its place means the file this
// project actually recorded cannot be read at all.
var errRequirementsNotRegular = errors.New("requirements file is not a regular file")

// loadRequirements reads requirements for cleanup scope.
//
// buildReachable runs while initCleanup's exclusive backend lock is still
// held, with no context or per-call deadline of its own. A recorded
// requirements file that turns out to be a fifo rather than a real file
// would otherwise reach requirements.LoadCollections's os.ReadFile
// unguarded: ReadFile opens with O_RDONLY, which blocks in open() until a
// writer appears - potentially forever, on an attacker-planted or merely
// misconfigured pipe. That stall does not stay local to this run: on the S3
// backend, the lock's own heartbeat keeps renewing its TTL for as long as
// this call blocks, so it locks out every other runner sharing the bucket
// for the same duration. os.Stat gates against exactly that shape before
// ever delegating to requirements.LoadCollections, mirroring
// manifestIsRegularFile's identical reasoning for the MANIFEST.json leaf
// (cleanup.go).
//
// Stat, not Lstat, is deliberate: a symlinked requirements.yml is a
// legitimate, already-supported shape (store.RecordProject records whatever
// path a project's own requirements file sits at, symlink or not), and
// Lstat would reject it outright - a real regression, not a hardening. What
// this gate classifies is the shape Stat resolves to - the fifo itself,
// following the symlink - not the entry's own on-disk shape at path.
//
// A path that does not exist at all - including a dangling symlink -
// returns the Stat error unchanged, still satisfying errors.Is(err,
// fs.ErrNotExist), so projectRequirementRoots keeps routing it to its
// tolerated stale-entry arm exactly as it did before this gate existed.
func loadRequirements(path, defaultSource string) ([]requirements.CollectionRequirement, error) {
	// #nosec G703 -- path is a registry-recorded requirements file path (a
	// fixed set of candidates this program itself wrote via
	// store.RecordProject), not a value taken directly from an external
	// request; this is a read-only shape check before any read is attempted.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q", errRequirementsNotRegular, path)
	}
	reqs, _, err := requirements.LoadCollections(path, defaultSource)
	return reqs, err
}
