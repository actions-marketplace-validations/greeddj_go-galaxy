package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errTestTransport and errTestLocalIO are static stand-ins used only to
// exercise downloadRetryable's classification: the former for a raw
// transport-level failure (e.g. a dial or connection reset), the latter for
// an unrelated local filesystem failure. Neither is ever returned by
// production code.
var (
	errTestTransport = errors.New("dial tcp: connection refused")
	errTestLocalIO   = errors.New("write /tmp/x: no space left on device")
)

// TestDownloadRetryable pins the artifact-download retry classification: the
// ordering that a stalled read is retryable even though the read-inactivity
// watchdog aborts it by canceling its own derived context (so the error also
// matches context.Canceled), a terminal sha256 mismatch after a complete
// read is never retried, and - unlike the Galaxy API GET predicate
// (fetchRetryable in package cache) - a bare transport-level failure (no
// HTTP response at all) is retryable here.
func TestDownloadRetryable(t *testing.T) {
	t.Parallel()

	// stalled mirrors the exact shape the fetch watchdog produces: an
	// ErrReadStalled wrapping the context.Canceled its own derived-context
	// cancel raised. If downloadRetryable checked context.Canceled before
	// ErrReadStalled, this would be misclassified as non-retryable.
	stalled := fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, time.Second, context.Canceled)
	shaMismatch := fmt.Errorf("%w: aaaa != bbbb", helpers.ErrSHA256Mismatch)

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
		{name: "sha256 mismatch after a complete read is terminal", err: shaMismatch, want: false},
		{
			name: "retryable status is retryable",
			err:  &downloadAttemptError{err: fmt.Errorf("%w: boom", helpers.ErrDownloadFailed), status: http.StatusServiceUnavailable},
			want: true,
		},
		{
			name: "not found status is not retryable",
			err:  &downloadAttemptError{err: fmt.Errorf("%w: boom", helpers.ErrDownloadFailed), status: http.StatusNotFound},
			want: false,
		},
		{
			name: "a bare transport-level failure (no response at all) is retryable",
			err:  &downloadAttemptError{err: errTestTransport},
			want: true,
		},
		{name: "an unclassified local error is not retryable", err: errTestLocalIO, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := downloadRetryable(tc.err); got != tc.want {
				t.Errorf("downloadRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
