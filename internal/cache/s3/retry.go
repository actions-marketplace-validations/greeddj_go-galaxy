package s3

import (
	"context"
	"errors"

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

// wrapRetryableStatus wraps err in a *retryableStatusError when status is
// one of the retryable HTTP statuses, so s3Retryable knows to retry it; any
// other status (including a nil err) is returned unchanged, since it is
// never retried.
//
// The retryable set comes from helpers.IsRetryableHTTPStatus, the single
// definition shared with the Galaxy API and artifact-download paths. This
// package deliberately keeps no set of its own: the two are the same class
// of transient server-side failure, and two independent copies could drift
// into disagreeing about which statuses a run retries depending only on
// which subsystem issued the request.
func wrapRetryableStatus(status int, err error) error {
	if err == nil || !helpers.IsRetryableHTTPStatus(status) {
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

// s3RetryableFor binds ctx to s3Retryable, producing the func(error) bool
// helpers.Retry itself expects. Every call site in this file passes the same
// ctx it also threads into newRequest for the attempt being judged, so the
// classifier and the request it is classifying always share one context.
func s3RetryableFor(ctx context.Context) func(error) bool {
	return func(err error) bool { return s3Retryable(ctx, err) }
}

// s3Retryable classifies whether an idempotent S3 verb's failure is worth
// retrying. Offline mode is never retried - repeating the same call against
// a transport that rejects every request outright would only spend the
// backoff budget for nothing. Every other error is terminal by default
// (this covers errS3NotFound, errS3PreconditionFailed, and the artifact
// sha256-mismatch sentinel) except three transient shapes a verb or
// Client.do marks explicitly: a stalled body read (helpers.ErrReadStalled),
// a transport-level failure that never produced a response
// (errS3TransportFailed, the sentinel Client.do alone produces), or a
// retryable HTTP status carried by *retryableStatusError.
//
// errS3TransportFailed is retried only while ctx.Err() == nil. Measured on
// go1.26.5 with this project's transport settings, both a net.Dialer timeout
// (helpers.FetchDialContextTimeout) and http.Transport's own
// ResponseHeaderTimeout satisfy errors.Is(err, context.DeadlineExceeded)
// while the caller's own context is still live, so classifying by the
// caller's context - not by the error's shape - is what keeps the two
// commonest outage shapes (a black-holed endpoint, and one that accepts a
// connection and then never answers) retryable at all; Client.do's own doc
// comment carries the full measurement this classifier relies on.
//
// That ctx check is not what stops a retry after the caller's own
// cancellation. Two backstops already do that, and neither is here:
// Client.do declines to label a transport failure the caller's cancellation
// raced (see its "one race is resolved deliberately" paragraph), and
// helpers.Retry re-checks the context before every backoff sleep and returns
// ctx.Err() from there. What this check decides is narrower and is only
// about which error surfaces: when a cancellation lands after Client.do has
// labeled a genuine transport failure but before this classification reads
// it, the run reports that transport failure rather than ctx.Err().
//
// The check sits on the transport arm alone, never as an early blanket gate
// on the whole function, because the two arms describe different events. A
// transport failure is terminal in its own right - nothing answered - so
// reporting it reports what actually happened. A retryable status is an
// answer this loop was about to act on, so turning it terminal would
// attribute to the backend a failure that never ended the run: for a status
// arriving in the same instant the caller cancels, a blanket gate would turn
// "return ctx.Err()" into "return the status error", the inversion
// Client.do's own doctrine forbids.
func s3Retryable(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, helpers.ErrOfflineMode) {
		return false
	}
	if errors.Is(err, helpers.ErrReadStalled) {
		return true
	}
	// An oversized S3 listing or batch-delete response - the two S3 verbs in
	// this package whose body read is capped and reached through this
	// classifier - is terminal by the same reasoning as a sha256 mismatch,
	// but the default-deny fallthrough below would already cover it: this
	// check is explicit so a future reordering of the classifier cannot
	// accidentally start retrying a hostile or broken oversized response.
	if errors.Is(err, helpers.ErrResponseTooLarge) {
		return false
	}
	if errors.Is(err, errS3TransportFailed) {
		return ctx.Err() == nil
	}
	var statusErr *retryableStatusError
	return errors.As(err, &statusErr)
}
