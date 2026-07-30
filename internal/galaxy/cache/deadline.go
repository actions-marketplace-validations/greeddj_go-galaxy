package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// deadlineError normalizes err into a wrapped sentinel when, and only when,
// this operation's own budget (dlCtx, built from context.WithTimeout(parent,
// budget)) is what ended the work - not the caller (parent), and not some
// other context-unrelated failure. It is the shared normalizer behind both
// helpers.ErrMetadataFetchDeadline (internal/galaxy/cache's own metadata
// fetch) and helpers.ErrStateObjectDeadline (statedeadline.go's Backend
// decorator), taking the sentinel as a parameter instead of being duplicated
// once per surface.
//
// The checks run in this order, each guarding against a distinct
// misclassification:
//
//  1. A nil err, or one that already carries sentinel, is returned as-is:
//     idempotence, so a call site may normalize once inside a retry attempt
//     and again around the whole retry loop without doubling the sentinel or
//     its rendered cause into the message. This check is currently redundant
//     against step 4 for every shape production actually produces: the only
//     err that ever reaches a second normalization pass is one step 5 already
//     built, whose cause is rendered with %v and therefore carries no
//     context.DeadlineExceeded/context.Canceled reachable through errors.Is -
//     so step 4 already refuses to re-wrap it, independently of this check.
//     A test pinning this step is therefore synthetic by necessity, built
//     from a shape no producer here builds rather than from the real
//     double-call pattern. It would become load-bearing again the moment any
//     carrier joins a deadline-sentinel error with a context sentinel that is
//     still reachable through errors.Is - the two existing carriers that
//     would need to do that are internal/galaxy/collections/failures.go's
//     errors.Join of per-collection causes and annotateSaveFailure's "%w;
//     snapshot save failed: %w", and neither currently introduces a fresh,
//     still-live context.Context to re-derive a dlCtx from, so neither
//     reaches this function a second time today. The check stays regardless,
//     the same posture isLateTerminalDownloadError takes for its own explicit
//     checks behind a default-deny fallthrough: an invariant worth stating
//     directly rather than leaving to an incidental side effect of a
//     different step.
//
//  2. parent.Err() != nil returns err as-is: a caller cancellation, or the
//     caller's own deadline expiring first, must surface unaltered. dlCtx is
//     a child of parent and inherits its expiry, so dlCtx.Err() alone cannot
//     tell "my own budget expired" apart from "I merely inherited my
//     parent's expiry" - this check is what makes that distinction.
//
//  3. !errors.Is(dlCtx.Err(), context.DeadlineExceeded) returns err as-is:
//     dlCtx is still live, or (impossible in practice, since only cancel()
//     or its own deadline can end it, but checked exactly rather than
//     assumed) was canceled for some other reason.
//
//  4. !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err,
//     context.Canceled) returns err as-is. This is the causal precondition,
//     and the deliberate departure from artifactDeadlineError's ambient
//     shape (that function instead tests ambient context state and buys
//     back precision with a single content-based exclusion,
//     helpers.ErrSHA256Mismatch - adequate for a surface whose error
//     vocabulary is tiny). Both surfaces feeding this function have wide
//     error vocabularies, and one of them is load-bearing for routing:
//     tryServerRootMetadata/loadRootMetadataCached classify a root-metadata
//     fetch by HTTP status - a 404 advances the server-list walk, a 401/403
//     becomes helpers.ErrGalaxyAuthFailed, a retryable status becomes
//     helpers.ErrGalaxyServerUnavailable. A *HTTPStatusError returned at
//     T, with the budget expiring at T+epsilon, would - under the ambient
//     rule - be relabeled into the deadline sentinel and abort a walk that
//     should instead have advanced to the next server. On the state-object
//     side, the identical ambient rule would erase
//     helpers.ErrCorruptProjectRegistry's actionable message, the size-cap
//     diagnosis, and the schema-version verdicts, replacing all of them with
//     an undifferentiated deadline. This check costs two errors.Is calls on
//     an error path and needs no exclusion list a future error type could
//     fall outside of.
//
//     This cannot suppress a genuine budget expiry: every producer on both
//     paths surfaces one of the two context sentinels once the budget is
//     what ended the work. http.Client.Do returns a *url.Error wrapping the
//     context error; a body read aborted by the parent's own expiry is not
//     claimed by the watchdog (watchdogBody.Read's own guard fails when the
//     parent context, not the watchdog's derived one, is what ended the
//     read, so the raw error propagates), carrying context.DeadlineExceeded,
//     or context.Canceled when the watchdog's own cancel wins the race;
//     helpers.Retry's own backoff select returns a bare ctx.Err(). An error
//     carrying neither signal - a size-cap rejection, an HTTP status error,
//     a corrupt-JSON or corrupt-registry error, a disk error - was, by
//     construction, not ended by this budget.
//
//     This precondition is deliberately NOT retrofitted into
//     artifactDeadlineError (internal/galaxy/collections/deadline.go): doing
//     so would make that function's documented killing mutation for its
//     helpers.ErrSHA256Mismatch exclusion non-killing, since a sha mismatch
//     carries no context signal and would pass through this check either
//     way - turning a currently-pinned assertion into an unfalsifiable one.
//     The two functions are deliberately not unified.
//
//  5. Otherwise, err is wrapped: fmt.Errorf("%w after %s: %v", sentinel,
//     budget, err). The cause is rendered with %v, never %w - see
//     helpers.ErrMetadataFetchDeadline's and helpers.ErrStateObjectDeadline's
//     own doc comments for why leaving context.DeadlineExceeded or
//     context.Canceled reachable via errors.Is here would steal the
//     resulting failure's exit-code classification.
func deadlineError(parent, dlCtx context.Context, budget time.Duration, sentinel, err error) error {
	if err == nil || errors.Is(err, sentinel) {
		return err
	}
	if parent.Err() != nil {
		return err
	}
	if !errors.Is(dlCtx.Err(), context.DeadlineExceeded) {
		return err
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return err
	}
	//nolint:errorlint // deliberately %v, not %w: see the doc comment above and the sentinel's own.
	return fmt.Errorf("%w after %s: %v", sentinel, budget, err)
}

// MetadataDeadlineError is deadlineError bound to
// helpers.ErrMetadataFetchDeadline, exported for
// internal/galaxy/collections' versions-list paging loop
// (loadVersionsListCached), the one metadata call site outside this package
// that establishes its own budget around more than a single fetchJSONBody
// call.
func MetadataDeadlineError(parent, dlCtx context.Context, budget time.Duration, err error) error {
	return deadlineError(parent, dlCtx, budget, helpers.ErrMetadataFetchDeadline, err)
}
