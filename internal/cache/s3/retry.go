package s3

import (
	"context"
	"errors"
	"net/http"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// retryableStatusError marks a per-verb HTTP failure as safe to retry, while
// still carrying the original sentinel-wrapped error (e.g. errS3GetFailed
// with its S3 <Error> code/message folded in) through Unwrap. s3Retryable
// recognizes it via errors.As; every caller and test outside this package
// keeps matching the underlying sentinel via errors.Is exactly as before,
// unaware that a retry ever happened.
type retryableStatusError struct {
	err    error
	status int
}

// Error returns the wrapped error's message unchanged.
func (e *retryableStatusError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the wrapped sentinel-carrying error.
func (e *retryableStatusError) Unwrap() error {
	return e.err
}

// isRetryableStatus reports whether status is one of the small set of
// transient HTTP statuses (rate limiting and server-side failures) that are
// safe to retry on an idempotent S3 verb.
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// wrapRetryableStatus wraps err in a *retryableStatusError when status is
// one of the retryable HTTP statuses, so s3Retryable knows to retry it; any
// other status (including a nil err) is returned unchanged, since it is
// never retried.
func wrapRetryableStatus(status int, err error) error {
	if err == nil || !isRetryableStatus(status) {
		return err
	}
	return &retryableStatusError{status: status, err: err}
}

// s3RetryPolicy is the fixed retry policy shared by every idempotent S3 verb
// (GET, HEAD, DELETE, list, and an unconditional PUT): a small bounded
// number of attempts with a full-jitter exponential backoff between them.
// It is never configurable per call site - unlike the distributed lock's
// own backoff, which callers may shrink for tests - since these verbs run on
// the ordinary read/write path rather than the lock's own contention loop.
func s3RetryPolicy() helpers.RetryPolicy {
	return helpers.RetryPolicy{
		Base:        s3RetryBackoffBase,
		Cap:         s3RetryBackoffCap,
		MaxAttempts: s3RetryMaxAttempts,
	}
}

// s3Retryable classifies whether an idempotent S3 verb's failure is worth
// retrying. Offline mode and a context that is already canceled or expired
// are never retried - repeating the same call against a dead context or a
// transport that rejects every request outright would only spend the
// backoff budget for nothing. Every other error is terminal by default
// (this covers errS3NotFound, errS3PreconditionFailed, and the artifact
// sha256-mismatch sentinel) except the two transient shapes a verb marks
// explicitly: a retryable HTTP status carried by *retryableStatusError, or a
// stalled body read (helpers.ErrReadStalled).
func s3Retryable(err error) bool {
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
	var statusErr *retryableStatusError
	return errors.As(err, &statusErr)
}
