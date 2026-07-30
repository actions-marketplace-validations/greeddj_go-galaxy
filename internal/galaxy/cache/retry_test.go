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

// TestFetchRetryableTreatsTheMetadataDeadlineAsTerminal pins that
// isEarlyTerminalFetchError's helpers.ErrMetadataFetchDeadline check runs
// ahead of the ErrReadStalled check: an error carrying both - a watchdog
// stall that raced the deadline - must resolve deterministically toward
// terminal, mirroring isEarlyTerminalDownloadError's identical placement and
// reasoning for the artifact download path. The bare helpers.ErrReadStalled
// row is the control proving the table can produce true at all, so the
// combined row's false is a real refusal, not a fixture that can never
// accept.
//
// A row carrying only the sentinel would not be killable on its own: moving
// isEarlyTerminalFetchError below the ErrReadStalled check would still leave
// a bare-sentinel-only error unclassified by ErrReadStalled (it does not
// match) and unclassified by every check below it, falling through to the
// same "return false" default-deny outcome fetchRetryable already reaches
// today - so a bare-sentinel row is default-deny-covered and cannot
// distinguish the two orderings. The combined-error row is what makes this
// killable: only when both signatures are present does the ordering actually
// decide the answer.
func TestFetchRetryableTreatsTheMetadataDeadlineAsTerminal(t *testing.T) {
	t.Parallel()

	combined := fmt.Errorf("%w: %w", helpers.ErrMetadataFetchDeadline, helpers.ErrReadStalled)
	if got := fetchRetryable(combined); got {
		t.Errorf("fetchRetryable(%v) = %v, want false (the deadline must resolve deterministically toward terminal)", combined, got)
	}

	// Control: a bare ErrReadStalled, with no deadline sentinel in the tree,
	// is retryable - proving this table can produce true at all.
	if got := fetchRetryable(helpers.ErrReadStalled); !got {
		t.Errorf("fetchRetryable(%v) = %v, want true (control: proves the table can produce true)", helpers.ErrReadStalled, got)
	}
}
