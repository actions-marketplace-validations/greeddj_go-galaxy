package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// s3RetryableCase is one row of TestS3Retryable's table. canceled selects
// between a live and an already-canceled context inside the test loop,
// rather than a func(error) bool row storing a context.Context directly -
// which containedctx flags on a struct field, and which every row here would
// otherwise share the same live background context anyway except the one
// pair that deliberately does not.
type s3RetryableCase struct {
	err      error
	name     string
	canceled bool
	want     bool
}

// newS3RetryableCases builds TestS3Retryable's table, split out from the test
// function itself purely to stay under the funlen budget - mirrors
// cache_backend_classification_test.go's own
// assertLiveTimeoutClassifiesAsCacheBackendUnavailable split for the
// identical reason.
func newS3RetryableCases() []s3RetryableCase {
	// stalledProduction mirrors the real shape watchdogBody.Read builds: the
	// cause rendered with %v, not wrapped with %w, so it does not carry
	// context.Canceled through errors.Is (see helpers.ErrReadStalled's doc
	// comment). This is the shape s3Retryable actually receives today.
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stalledProduction := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, time.Second, context.Canceled)
	// stalledSynthetic is deliberately NOT the production shape: it
	// double-wraps context.Canceled with %w, a signature the current producer
	// never builds. It is kept to pin s3Retryable's ordering guard
	// (ErrReadStalled classified before the errS3TransportFailed arm)
	// independently of how the producer happens to render its cause.
	stalledSynthetic := fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, time.Second, context.Canceled)
	// transportFailure mirrors the exact shape Client.do produces: a dial or
	// response-header timeout, wrapped in errS3TransportFailed, still
	// carrying context.DeadlineExceeded through errors.Is - the shape the
	// classifier must retry when the caller's own context is live, and refuse
	// when it is not.
	transportFailure := fmt.Errorf("%w: %w", errS3TransportFailed,
		&url.Error{Op: "Get", URL: "https://example.invalid", Err: context.DeadlineExceeded})

	return []s3RetryableCase{
		{name: "nil is not retryable", err: nil, want: false},
		{name: "offline is not retryable", err: helpers.ErrOfflineMode, want: false},
		{name: "stalled read in its production shape is retryable", err: stalledProduction, want: true},
		{
			name: "a stall signature that also carries context.Canceled is still retryable",
			err:  stalledSynthetic,
			want: true,
		},
		{name: "raw context.Canceled is not retryable", err: context.Canceled, want: false},
		{name: "raw context.DeadlineExceeded is not retryable", err: context.DeadlineExceeded, want: false},
		{
			name: "retryable status is retryable",
			err:  wrapRetryableStatus(http.StatusServiceUnavailable, errS3BucketRequestFailed),
			want: true,
		},
		{name: "not found is not retryable", err: errS3NotFound, want: false},
		{name: "precondition failed is not retryable", err: errS3PreconditionFailed, want: false},
		{name: "an oversized artifact download is never retried", err: helpers.ErrArtifactTooLarge, want: false},
		// Positive control and refusal on one fixture: the identical
		// errS3TransportFailed-wrapped error is retryable under a live ctx and
		// refused under a canceled one, proving the gate discriminates on ctx
		// rather than on the error's own shape.
		{name: "transport failure is retryable while the caller's ctx is live", err: transportFailure, want: true},
		{
			name:     "the identical transport failure is not retried once the caller's ctx is canceled",
			err:      transportFailure,
			canceled: true,
			want:     false,
		},
		{
			name: "an unmarked raw transport error is never retried",
			err:  &url.Error{Op: "Get", URL: "https://example.invalid", Err: context.DeadlineExceeded},
			want: false,
		},
		{
			name: "a status sentinel carrying ErrCacheBackendUnavailable but no retryable status is not retried",
			err:  errS3GetFailed,
			want: false,
		},
	}
}

// TestS3Retryable pins the retry classification, in particular that a
// stalled read is retryable in both its real production rendering and a
// deliberately synthetic one that still carries context.Canceled, while a
// genuine caller cancellation - which arrives as a raw context.Canceled,
// never wrapped in ErrReadStalled - is not, and that errS3TransportFailed's
// retryability depends on the caller's own ctx rather than on the error's
// shape.
func TestS3Retryable(t *testing.T) {
	t.Parallel()
	for _, tc := range newS3RetryableCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.canceled {
				canceledCtx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = canceledCtx
			}
			if got := s3Retryable(ctx, tc.err); got != tc.want {
				t.Errorf("s3Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestWrapRetryableStatusFollowsTheSharedSet pins that this package retries
// exactly the statuses helpers.IsRetryableHTTPStatus names, with no set of
// its own. It sweeps both classes, so it fails in either direction: a status
// this package wrapped but the shared predicate rejects, or one the shared
// predicate accepts but this package left unwrapped. A private copy that
// drifted by even a single status - the failure mode that made consolidating
// on one definition worth doing - shows up here rather than as two
// subsystems quietly disagreeing about whether a run retries.
//
// Verified against a real drift: making wrapRetryableStatus skip 429 while
// the shared predicate still accepts it fails this test with
// "s3Retryable(wrapRetryableStatus(429, err)) = false, want true".
func TestWrapRetryableStatusFollowsTheSharedSet(t *testing.T) {
	t.Parallel()

	statuses := []int{
		http.StatusOK,
		http.StatusMovedPermanently,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusPreconditionFailed,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusNotImplemented,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	}
	// Both classes must be exercised for the sweep to mean anything: an
	// all-retryable or all-terminal list would pass against a predicate stuck
	// at a constant.
	var retryable, terminal int
	for _, status := range statuses {
		if helpers.IsRetryableHTTPStatus(status) {
			retryable++
		} else {
			terminal++
		}
	}
	if retryable == 0 || terminal == 0 {
		t.Fatalf("sweep must cover both classes, got %d retryable and %d terminal", retryable, terminal)
	}

	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			wrapped := wrapRetryableStatus(status, errS3BucketRequestFailed)
			want := helpers.IsRetryableHTTPStatus(status)
			if got := s3Retryable(context.Background(), wrapped); got != want {
				t.Errorf("s3Retryable(wrapRetryableStatus(%d, err)) = %v, want %v", status, got, want)
			}
			// The wrapper must stay transparent either way: callers outside
			// this package keep matching the underlying sentinel.
			if !errors.Is(wrapped, errS3BucketRequestFailed) {
				t.Errorf("wrapRetryableStatus(%d, err) lost the underlying sentinel", status)
			}
		})
	}
}
