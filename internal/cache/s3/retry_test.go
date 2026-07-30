package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestS3Retryable pins the retry classification, in particular the ordering
// that a stalled read is retryable even though the read-inactivity watchdog
// aborts it by canceling its own derived context (so the error also matches
// context.Canceled), while a genuine caller cancellation - which arrives as a
// raw context.Canceled, never wrapped in ErrReadStalled - is not.
func TestS3Retryable(t *testing.T) {
	t.Parallel()

	// stalled mirrors the exact shape the fetch watchdog produces: an
	// ErrReadStalled wrapping the context.Canceled its own derived-context
	// cancel raised. If s3Retryable checked context.Canceled before
	// ErrReadStalled, this would be misclassified as non-retryable.
	stalled := fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, time.Second, context.Canceled)

	cases := []struct {
		err  error
		name string
		want bool
	}{
		{name: "nil is not retryable", err: nil, want: false},
		{name: "offline is not retryable", err: helpers.ErrOfflineMode, want: false},
		{name: "stalled read matches both ErrReadStalled and context.Canceled", err: stalled, want: true},
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := s3Retryable(tc.err); got != tc.want {
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
			if got := s3Retryable(wrapped); got != want {
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
