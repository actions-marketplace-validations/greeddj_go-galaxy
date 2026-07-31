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

// TestDownloadRetryable pins the artifact-download retry classification: a
// stalled read is retryable in both its real production rendering (the cause
// left unreachable through errors.Is, see helpers.ErrReadStalled's doc
// comment) and a deliberately synthetic one that still carries
// context.Canceled, since downloadRetryable classifies ErrReadStalled before
// the context.Canceled check; a terminal sha256 mismatch after a complete
// read is never retried; and - unlike the Galaxy API GET predicate
// (fetchRetryable in package cache) - a bare transport-level failure (no
// HTTP response at all) is retryable here.
func TestDownloadRetryable(t *testing.T) {
	t.Parallel()

	// stalledProduction mirrors the real shape watchdogBody.Read builds: the
	// cause rendered with %v, not wrapped with %w, so it does not carry
	// context.Canceled through errors.Is (see helpers.ErrReadStalled's doc
	// comment). This is the shape downloadRetryable actually receives today.
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stalledProduction := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, time.Second, context.Canceled)
	// stalledSynthetic is deliberately NOT the production shape: it
	// double-wraps context.Canceled with %w, a signature the current producer
	// never builds. It is kept to pin downloadRetryable's ordering guard
	// (ErrReadStalled classified before the context.Canceled check)
	// independently of how the producer happens to render its cause, so that
	// guard stays tested even if a future call site reintroduces %w somewhere.
	stalledSynthetic := fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, time.Second, context.Canceled)
	shaMismatch := fmt.Errorf("%w: aaaa != bbbb", helpers.ErrSHA256Mismatch)

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
		{name: "sha256 mismatch after a complete read is terminal", err: shaMismatch, want: false},
		{name: "an oversized artifact download is never retried", err: helpers.ErrResponseTooLarge, want: false},
		{
			// This case is DELIBERATELY a non-killing regression pin, exactly
			// like the ErrResponseTooLarge case above: helpers.ErrArtifactDownloadDeadline
			// never wraps its cause with %w (see its own doc comment), so
			// this error does not match context.Canceled or
			// context.DeadlineExceeded either, and the default-deny
			// fallthrough at the bottom of downloadRetryable already returns
			// false for it even with the explicit arm deleted. Do not
			// fabricate a killing mutation for this case; there isn't one.
			name: "an expired artifact download deadline is never retried",
			err:  fmt.Errorf("%w after 1s: boom", helpers.ErrArtifactDownloadDeadline),
			want: false,
		},
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
