package s3

import (
	"context"
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
