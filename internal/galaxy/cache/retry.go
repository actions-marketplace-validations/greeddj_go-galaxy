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
	// Classify a stalled read before the context checks: the watchdog aborts a
	// stall by canceling its own derived context, so the error also matches
	// context.Canceled even though it is genuinely retryable. ErrReadStalled is
	// only produced while the caller's context is still live, so this never
	// masks a real caller cancellation (which arrives as a raw context.Canceled).
	if errors.Is(err, helpers.ErrReadStalled) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return helpers.IsRetryableHTTPStatus(statusErr.Code)
	}
	return false
}
