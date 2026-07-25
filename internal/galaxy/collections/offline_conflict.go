package collections

import (
	"errors"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// offlineConflictNote is appended to a resolution conflict's own message
// when it happened under --offline: an offline run can only ever see
// whatever metadata is already cached, so a conflict there may just be an
// artifact of stale or incomplete cache coverage rather than a genuine,
// permanent unsatisfiability.
const offlineConflictNote = "note: offline mode restricts resolution to cached metadata; retry without --offline to fetch fresh metadata"

// offlineConflictError wraps a *solver.ConflictError with offlineConflictNote
// appended to its rendered message, while staying fully transparent to
// errors.Is/errors.As through Unwrap: callers that check for
// helpers.ErrNoVersionSatisfiesConstraints or *solver.ConflictError, or that
// classify it via exitcode.FromError, see exactly the same result as they
// would for the unwrapped error.
type offlineConflictError struct {
	inner error
}

// Error renders the inner conflict's own message followed by the offline
// note on its own line.
func (e *offlineConflictError) Error() string {
	return e.inner.Error() + "\n" + offlineConflictNote
}

// Unwrap exposes the inner error so errors.Is/errors.As - and therefore
// exitcode.FromError - classify offlineConflictError identically to the
// *solver.ConflictError it wraps.
func (e *offlineConflictError) Unwrap() error {
	return e.inner
}

// annotateOfflineConflict returns err unchanged unless cfg is running in
// --offline mode and err is a *solver.ConflictError, in which case it
// returns err wrapped in offlineConflictError. Any other error, or a
// conflict that happened online, passes through untouched.
func annotateOfflineConflict(cfg *config.Config, err error) error {
	if err == nil || !cfg.Offline {
		return err
	}
	var conflictErr *solver.ConflictError
	if !errors.As(err, &conflictErr) {
		return err
	}
	return &offlineConflictError{inner: err}
}
