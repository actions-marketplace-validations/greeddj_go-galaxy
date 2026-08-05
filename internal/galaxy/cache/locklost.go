package cache

import (
	"context"
	"errors"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// LockLostError turns a run's own outcome into a lock-loss verdict whenever
// the holder context Backend.Lock returned says this run stopped being the
// exclusive holder. holder is that context; parent is the caller's own; err
// is whatever the run itself produced, nil included.
//
// The cause is rendered with %v, deliberately never %w - the same rule
// artifactDeadlineError (internal/galaxy/collections/deadline.go) and
// deadlineError (internal/galaxy/cache/deadline.go) already follow for their
// own sentinels. A backend cancels the holder context to signal the loss, so
// err on a canceled run carries context.Canceled, and
// cmd/go-galaxy/exitcode's FromError checks context.Canceled ahead of every
// other class: wrapping the cause with %w would report a lock stolen by
// another holder as an operator's own Ctrl-C (exit 130).
//
// A lock-loss verdict SUPERSEDES whatever the run itself concluded, and the
// %v rendering is what enforces that: err's whole error tree is flattened
// into the message rather than left reachable through errors.Is, so a run
// that both lost the lock and failed an integrity check reports the lock-loss
// class and not the integrity one. That is deliberate, not a rounding error.
// Once another holder is writing the same cache, this run's own verdicts stop
// being trustworthy on their own terms - the sha256 mismatch it reports may
// simply be the other holder rewriting an artifact underneath it - so the
// actionable fact is that exclusivity was lost, and every other conclusion is
// evidence rather than a diagnosis. Any single sentinel a caller genuinely
// needs to keep matchable would have to be excluded here explicitly, the way
// artifactDeadlineError excludes helpers.ErrSHA256Mismatch; nothing is
// excluded today, on purpose.
//
// A run that finished all its work with no error at all is still an error
// once the lock was lost. Detection is late by construction - the heartbeat
// runs every heartbeatInterval and the lock's TTL is longer still - so by the
// time a tick observes a foreign token, this run has already been installing,
// extracting, committing artifacts, and persisting a snapshot without
// exclusivity. Reporting exit 0 there would certify work whose exclusivity
// this program knows was violated.
//
// The three passes above the verdict each protect a distinct
// misclassification:
//
//  1. holder == nil returns err unchanged. It is required rather than
//     defensive: context.Cause(nil) panics on a nil interface, and a nil
//     holder is a real shape - Backend.Lock returns one on every acquisition
//     failure, and initInstall propagates it from the arm that never reached
//     the lock at all.
//
//  2. parent.Err() != nil returns err unchanged. The caller's own
//     cancellation outranks a lock-loss verdict: an operator's Ctrl-C must
//     stay ExitInterrupt, and a holder context derived from a canceled parent
//     is canceled too, so without this check every interrupted run against a
//     backend would be reclassified.
//
//  3. A holder that is canceled for any other reason - or not canceled at all
//     - returns err unchanged. This is what makes the local backend, which
//     returns the parent context itself, inert here by construction rather
//     than by a per-backend branch.
func LockLostError(parent, holder context.Context, err error) error {
	if holder == nil {
		return err
	}
	if parent.Err() != nil {
		return err
	}
	if !errors.Is(context.Cause(holder), helpers.ErrCacheLockLost) {
		return err
	}
	if err == nil {
		return helpers.ErrCacheLockLost
	}
	//nolint:errorlint // deliberately %v, not %w: see the doc comment above and the sentinel's own.
	return fmt.Errorf("%w: %v", helpers.ErrCacheLockLost, err)
}
