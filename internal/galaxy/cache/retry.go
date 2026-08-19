package cache

import (
	"context"
	"errors"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// fetchRetryable classifies whether a Galaxy API GET's failure is worth
// retrying. Offline mode, this request's own metadata fetch deadline, and a
// context that is already canceled or expired are never retried - repeating
// the same call against a dead context or a transport that rejects every
// request outright would only spend the backoff budget for nothing. Every
// other error is terminal by default (this covers a 404 and any other
// non-retryable HTTPStatusError) except the two transient shapes
// fetchJSONBody marks explicitly: a retryable HTTP status carried by
// *HTTPStatusError, or a stalled body read (helpers.ErrReadStalled). A raw
// transport-level failure (client.Do returning an error before any response
// is received, e.g. a dial or TLS failure) is deliberately NOT retried here:
// unlike the artifact download's predicate, a small metadata GET is cheap
// enough to fail fast and let its caller (candidate fallback, resolution)
// react, rather than spend the retry budget on a connection that may never
// come up.
//
// Split into isEarlyTerminalFetchError and the body below purely to stay
// under the cyclomatic-complexity budget; the two together still cover the
// exact same classification.
func fetchRetryable(err error) bool {
	if err == nil {
		return false
	}
	if isEarlyTerminalFetchError(err) {
		return false
	}
	// Classify a stalled read before the context checks. The production stall
	// error no longer carries context.Canceled through errors.Is (see
	// helpers.ErrReadStalled's doc comment: the watchdog renders its
	// cancellation cause with %v, not %w), so this check and the context check
	// below no longer overlap in practice. It is kept ahead of the context
	// check anyway as defense in depth, so this classifier stays correct
	// regardless of how the producer renders its cause.
	if errors.Is(err, helpers.ErrReadStalled) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A response that overran its size cap is terminal: retrying would just
	// re-fetch the same oversized body.
	if errors.Is(err, helpers.ErrResponseTooLarge) {
		return false
	}
	if statusErr, ok := errors.AsType[*HTTPStatusError](err); ok {
		return helpers.IsRetryableHTTPStatus(statusErr.Code)
	}
	return false
}

// isEarlyTerminalFetchError reports whether err is one of the two sentinels
// checked ahead of ErrReadStalled: offline mode, and this request's own
// metadata fetch deadline. The deadline specifically must be checked here,
// ahead of ErrReadStalled, so a watchdog stall that raced the deadline
// resolves deterministically toward terminal rather than burning a backoff on
// a context that is already gone - mirroring
// isEarlyTerminalDownloadError's identical placement and reasoning in
// internal/galaxy/collections/retry.go. The default-deny fallthrough in
// fetchRetryable would already cover both of these; the check is explicit so
// a future reordering cannot start retrying either one.
func isEarlyTerminalFetchError(err error) bool {
	return errors.Is(err, helpers.ErrOfflineMode) || errors.Is(err, helpers.ErrMetadataFetchDeadline)
}
