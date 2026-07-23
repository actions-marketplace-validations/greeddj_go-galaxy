package cache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errTestTransport is a static stand-in for a raw transport-level failure
// (e.g. a dial or connection reset), used only to exercise fetchRetryable's
// classification without ever being returned by production code.
var errTestTransport = errors.New("dial tcp: connection refused")

// TestFetchRetryable pins the retry classification, in particular the
// ordering that a stalled read is retryable even though the read-inactivity
// watchdog aborts it by canceling its own derived context (so the error
// also matches context.Canceled), while a genuine caller cancellation -
// which arrives as a raw context.Canceled, never wrapped in ErrReadStalled -
// is not, and that a raw transport error (no HTTPStatusError at all) is
// treated as non-retryable for a Galaxy API GET.
func TestFetchRetryable(t *testing.T) {
	t.Parallel()

	// stalled mirrors the exact shape the fetch watchdog produces: an
	// ErrReadStalled wrapping the context.Canceled its own derived-context
	// cancel raised. If fetchRetryable checked context.Canceled before
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
			name: "retryable status 503 is retryable",
			err:  &HTTPStatusError{URL: "https://example.com", Status: "503 Service Unavailable", Code: http.StatusServiceUnavailable},
			want: true,
		},
		{
			name: "not found 404 is not retryable",
			err:  &HTTPStatusError{URL: "https://example.com", Status: "404 Not Found", Code: http.StatusNotFound},
			want: false,
		},
		{name: "a raw transport error is not retryable for an API GET", err: errTestTransport, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fetchRetryable(tc.err); got != tc.want {
				t.Errorf("fetchRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
