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
//
// The checks below are split into isEarlyTerminalDownloadError,
// isLateTerminalDownloadError, and isRetryableAttemptError purely to stay
// under the cyclomatic-complexity budget; together with the inline
// ErrReadStalled/context checks between them, they implement one
// classification in one fixed order, not three independent ones.
func downloadRetryable(err error) bool {
	if err == nil {
		return false
	}
	if isEarlyTerminalDownloadError(err) {
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
	if isLateTerminalDownloadError(err) {
		return false
	}
	return isRetryableAttemptError(err)
}

// isEarlyTerminalDownloadError reports whether err is one of the two
// sentinels checked ahead of ErrReadStalled: offline mode, and this
// acquisition's own artifact download deadline. The deadline specifically
// must be checked here, ahead of ErrReadStalled, so a watchdog stall that
// raced the deadline resolves deterministically toward terminal rather than
// burning a backoff on a context that is already gone. Like
// isLateTerminalDownloadError below, the default-deny fallthrough in
// downloadRetryable would already cover both of these; the checks are
// explicit so a future reordering cannot start retrying either one.
func isEarlyTerminalDownloadError(err error) bool {
	return errors.Is(err, helpers.ErrOfflineMode) || errors.Is(err, helpers.ErrArtifactDownloadDeadline)
}

// isLateTerminalDownloadError reports whether err is a terminal artifact
// content failure discovered only after a complete read: a sha256 mismatch,
// an oversized body, bytes that are not a gzip-compressed tar at all, or a
// tar stream that presented no header inside the shape probe's scan bound.
// All four are terminal by the same reasoning - re-requesting the same URL
// turns a corrupt artifact into a correct one no more than it turns an error
// page into an archive, and the fourth is a deterministic property of the
// bytes delivered, so the same URL yields the same unbounded meta-header
// chain - and all four are checked explicitly, rather than relying on the
// default-deny fallthrough, so a future reordering of the classifier cannot
// accidentally start retrying a corrupt, tampered, hostile, oversized, or
// shapeless artifact.
func isLateTerminalDownloadError(err error) bool {
	return errors.Is(err, helpers.ErrSHA256Mismatch) ||
		errors.Is(err, helpers.ErrResponseTooLarge) ||
		errors.Is(err, helpers.ErrArtifactNotTarGz) ||
		errors.Is(err, helpers.ErrArtifactTarHeaderNotFound)
}

// isRetryableAttemptError reports whether err is a *downloadAttemptError
// worth retrying: a transport-level failure (status 0, no HTTP response at
// all) or a retryable HTTP status. Any other error - including one that is
// not a *downloadAttemptError at all - is not retryable.
func isRetryableAttemptError(err error) bool {
	if attemptErr, ok := errors.AsType[*downloadAttemptError](err); ok {
		if attemptErr.status == 0 {
			return true
		}
		return helpers.IsRetryableHTTPStatus(attemptErr.status)
	}
	return false
}
