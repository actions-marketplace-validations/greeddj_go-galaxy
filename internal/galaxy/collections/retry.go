package collections

import (
	"context"
	"errors"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// downloadAttemptError marks one artifact-download attempt's failure with
// enough information for downloadRetryable to classify it, while leaving the
// original sentinel-wrapped error reachable through Unwrap so every existing
// errors.Is/errors.As caller (helpers.ErrDownloadFailed, exitcode's
// classifier) keeps matching exactly as before. status is 0 when the
// failure happened before any HTTP response was received - a
// transport-level failure such as a dial or TLS handshake error - and the
// response's numeric status code otherwise.
type downloadAttemptError struct {
	err    error
	status int
}

// Error returns the wrapped error's message unchanged.
func (e *downloadAttemptError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the wrapped sentinel-carrying error.
func (e *downloadAttemptError) Unwrap() error {
	return e.err
}

// downloadRetryable classifies whether a full establish+stream+verify
// artifact download attempt is worth retrying in its entirety. Offline mode
// and a context that is already canceled or expired are never retried. A
// sha256 mismatch discovered after a complete read is a terminal,
// non-retryable outcome: the artifact was fully read and simply does not
// match its expected checksum, so repeating the identical request would
// only spend the retry budget on a corrupt or tampered artifact rather than
// a transient fault. Otherwise a *downloadAttemptError is retried when it
// carries a retryable HTTP status, or when it marks a transport-level
// failure (status 0, i.e. the request never got as far as an HTTP
// response) - unlike the Galaxy API GET predicate (fetchRetryable in
// package cache), which leaves a bare transport error non-retryable, an
// artifact download streams a much larger body over a longer-lived
// connection and is materially more likely to hit a transient network
// fault that never produces a response at all, so retrying the whole
// attempt here is worthwhile. Any other error - a local filesystem failure
// while writing the temp file or extracting the archive, for instance - is
// left non-retryable, since repeating the same local operation would not be
// expected to succeed where it just failed.
func downloadRetryable(err error) bool {
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
	if errors.Is(err, helpers.ErrSHA256Mismatch) {
		return false
	}
	// An oversized artifact is terminal by the same reasoning as a sha256
	// mismatch, but the default-deny fallthrough below would already cover
	// it: this check is explicit so a future reordering of the classifier
	// cannot accidentally start retrying a hostile or broken oversized body.
	if errors.Is(err, helpers.ErrArtifactTooLarge) {
		return false
	}
	var attemptErr *downloadAttemptError
	if errors.As(err, &attemptErr) {
		if attemptErr.status == 0 {
			return true
		}
		return helpers.IsRetryableHTTPStatus(attemptErr.status)
	}
	return false
}
