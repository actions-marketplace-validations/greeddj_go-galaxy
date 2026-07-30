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

// TestFetchRetryable pins the retry classification, in particular that a
// stalled read is retryable in both its real production rendering and a
// deliberately synthetic one that still carries context.Canceled, while a
// genuine caller cancellation - which arrives as a raw context.Canceled,
// never wrapped in ErrReadStalled - is not, and that a raw transport error
// (no HTTPStatusError at all) is treated as non-retryable for a Galaxy API
// GET.
func TestFetchRetryable(t *testing.T) {
	t.Parallel()

	// stalledProduction mirrors the real shape watchdogBody.Read builds: the
	// cause rendered with %v, not wrapped with %w, so it does not carry
	// context.Canceled through errors.Is (see helpers.ErrReadStalled's doc
	// comment). This is the shape fetchRetryable actually receives today.
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stalledProduction := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, time.Second, context.Canceled)
	// stalledSynthetic is deliberately NOT the production shape: it
	// double-wraps context.Canceled with %w, a signature the current producer
	// never builds. It is kept to pin fetchRetryable's ordering guard
	// (ErrReadStalled classified before the context.Canceled check)
	// independently of how the producer happens to render its cause.
	stalledSynthetic := fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, time.Second, context.Canceled)

	cases := []struct {
		err  error
		name string
		want bool
	}{
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
