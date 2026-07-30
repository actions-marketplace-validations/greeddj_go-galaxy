package cache

import (
	"context"
	"errors"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// fetchRetryable classifies whether a Galaxy API GET's failure is worth
// retrying. Offline mode and a context that is already canceled or expired
// are never retried - repeating the same call against a dead context or a
// transport that rejects every request outright would only spend the
// backoff budget for nothing. Every other error is terminal by default
// (this covers a 404 and any other non-retryable HTTPStatusError) except
// the two transient shapes fetchJSONBody marks explicitly: a retryable HTTP
// status carried by *HTTPStatusError, or a stalled body read
// (helpers.ErrReadStalled). A raw transport-level failure (client.Do
// returning an error before any response is received, e.g. a dial or TLS
// failure) is deliberately NOT retried here: unlike the artifact download's
// predicate, a small metadata GET is cheap enough to fail fast and let its
// caller (candidate fallback, resolution) react, rather than spend the
// retry budget on a connection that may never come up.
func fetchRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, helpers.ErrOfflineMode) {
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
	if errors.Is(err, helpers.ErrArtifactTooLarge) {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return helpers.IsRetryableHTTPStatus(statusErr.Code)
	}
	return false
}
