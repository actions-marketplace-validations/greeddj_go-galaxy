package collections

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// artifactDeadlineError normalizes err into helpers.ErrArtifactDownloadDeadline
// when, and only when, this acquisition's own budget (dlCtx, built from
// context.WithTimeout(parent, budget)) is what ended the work - not the
// caller (parent). A live parent whose own context ended first, or expired on
// its own schedule, must surface as context.Canceled/context.DeadlineExceeded
// unaltered: parent.Err() != nil is checked first, and err passes through
// untouched in that case. dlCtx.Err() must also be exactly
// context.DeadlineExceeded: if dlCtx is still live, or if it was canceled for
// some other reason (which cannot happen here since only cancel() or its own
// deadline can end it, but the check stays exact rather than assuming), err
// again passes through unchanged.
//
// One exception is content-based rather than context-based: an error carrying
// helpers.ErrSHA256Mismatch passes through unchanged whatever the two contexts
// say, so an artifact-integrity failure keeps its ExitIntegrity classification
// and its evict-and-refetch recovery. See the comment at that check for why it
// can never suppress a genuine deadline.
//
// It is idempotent: a nil err, or one that already carries the sentinel, is
// returned as-is, so a call site may normalize once inside a retry attempt
// and again around the whole helpers.Retry loop without doubling the
// sentinel or its rendered cause into the message.
//
// The cause is rendered into the new error with %v, deliberately never %w -
// see helpers.ErrArtifactDownloadDeadline's own doc comment for why leaving
// context.DeadlineExceeded or context.Canceled reachable via errors.Is here
// would steal this failure's exit-code classification.
func artifactDeadlineError(parent, dlCtx context.Context, budget time.Duration, err error) error {
	if err == nil || errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		return err
	}
	// An artifact-integrity failure keeps its own identity even when the
	// budget expired in the same instant. helpers.ErrSHA256Mismatch means the
	// bytes do not authenticate - reported either by the S3 backend's
	// read-time check on a fetched object (verifyArtifactSHA) or by
	// verifyDownloadSHA on a freshly streamed one - and exitcode classifies it
	// ExitIntegrity, a stop-and-alert class deliberately kept separate from
	// network and install failures, while prepareWithRecovery recovers from it
	// by evicting and refetching. Relabeling it here would erase both, and the
	// %v rendering below would put it out of reach of errors.Is entirely. This
	// can never suppress a genuine deadline: every producer compares a digest
	// only after a complete copy (downloadToFile, writeDownloadToTemp, and
	// streamDownloadAndExtract all return on the copy error first), so a
	// deadline-aborted transfer never reaches a comparison at all.
	if errors.Is(err, helpers.ErrSHA256Mismatch) {
		return err
	}
	if parent.Err() != nil || !errors.Is(dlCtx.Err(), context.DeadlineExceeded) {
		return err
	}
	//nolint:errorlint // deliberately %v, not %w: see the doc comment above and helpers.ErrArtifactDownloadDeadline's own.
	return fmt.Errorf("%w after %s: %v", helpers.ErrArtifactDownloadDeadline, budget, err)
}
