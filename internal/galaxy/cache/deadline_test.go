package cache

// This file pins deadlineError's classification table (run against both
// sentinels this package produces), MetadataDeadlineError's HTTP-status
// pass-through property, and idempotence.
//
// Each test below was verified against a real revert of the production
// change it pins, and this comment quotes the actual observed output:
//
//   - TestDeadlineErrorClassification, changing the %v render to %w in
//     deadlineError's final fmt.Errorf, makes both "cause wraps
//     context.DeadlineExceeded" rows fail with:
//     "deadlineError = galaxy metadata fetch deadline exceeded after 1s:
//     client.Do: context deadline exceeded, must not match
//     context.DeadlineExceeded"
//     (and the ErrStateObjectDeadline sibling row fails identically with
//     that sentinel's own text).
//   - The same test, deleting the causal precondition (step 4: neither
//     context.DeadlineExceeded nor context.Canceled in the cause), makes the
//     "cause is an HTTP status error: unchanged" row normalize instead,
//     failing with:
//     "deadlineError = galaxy metadata fetch deadline exceeded after 1s:
//     failed to fetch metadata: 404 Not Found (https://example.com), want
//     unchanged failed to fetch metadata: 404 Not Found (https://example.com)"
//   - The same test, deleting the parent.Err() != nil guard, makes the
//     "parent's own deadline (not this operation's budget) expired first"
//     row normalize instead of passing through unchanged, failing with:
//     "deadlineError = galaxy metadata fetch deadline exceeded after 1s:
//     client.Do: context deadline exceeded, want unchanged client.Do:
//     context deadline exceeded"
//   - The same test, deleting the dlCtx.Err() check (step 3), makes the
//     "dlCtx still live: unchanged" row normalize instead, failing with:
//     "deadlineError = galaxy metadata fetch deadline exceeded after 1s:
//     client.Do: context deadline exceeded, want unchanged client.Do:
//     context deadline exceeded"
//   - TestMetadataDeadlineErrorPreservesHTTPStatusRouting, deleting the
//     causal precondition the same way, makes the HTTP-status case fail
//     with:
//     "MetadataDeadlineError(failed to fetch metadata: 404 Not Found
//     (https://example.com)) = galaxy metadata fetch deadline exceeded
//     after 1s: failed to fetch metadata: 404 Not Found
//     (https://example.com), must not carry the sentinel"
//   - TestDeadlineErrorIsIdempotent, deleting the errors.Is(err, sentinel)
//     early return, makes the isolating-fixture pass fail with:
//     "re-normalizing the isolating fixture changed the message:
//     once=\"galaxy metadata fetch deadline exceeded after 1s: context
//     deadline exceeded\" twice=\"galaxy metadata fetch deadline exceeded
//     after 1s: galaxy metadata fetch deadline exceeded after 1s: context
//     deadline exceeded\""
//     The real-shape pass in the same test (the one built the way
//     fetchJSONBody's own retry loop actually produces one) does NOT fail
//     under this mutation: once wrapped, that shape's cause is rendered with
//     %v, so it no longer carries a raw context.DeadlineExceeded/Canceled
//     reachable through errors.Is, and step 4's causal precondition alone
//     already refuses to re-wrap it - independent of step 1. That is exactly
//     why the isolating fixture (deliberately double-%w, a shape no real
//     producer here builds - mirroring retry_test.go's own "stalledSynthetic"
//     precedent) exists: it is what actually separates step 1 from step 4.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// deadlineTestBudget is the fixed budget every deadlineError case in this
// file passes; its value is irrelevant to the classification, only its
// presence in the rendered message matters.
const deadlineTestBudget = time.Second

// errDeadlineCauseWrapsDeadlineExceeded and errDeadlineCauseWrapsCanceled
// mirror the two real shapes a stalled http.Client.Do or body read produce
// once dlCtx expires: one wrapping context.DeadlineExceeded (the ordinary
// case), the other context.Canceled (the watchdog's own derived-context
// cancel racing the deadline). Declared as static package-level vars, per
// err113, rather than inline errors.New/fmt.Errorf calls.
var (
	errDeadlineCauseWrapsDeadlineExceeded = fmt.Errorf("client.Do: %w", context.DeadlineExceeded)
	errDeadlineCauseWrapsCanceled         = fmt.Errorf("body read: %w", context.Canceled)
)

// deadlineErrorCase is one deadlineErrorCases table row, run against every
// sentinel in deadlineErrorSentinels.
type deadlineErrorCase struct {
	buildParent func() (context.Context, context.CancelFunc)
	buildDl     func(parent context.Context) (context.Context, context.CancelFunc)
	err         error
	name        string
	wantSame    bool
}

// deadlineErrorSentinels is every sentinel deadlineError normalizes into,
// each exercised against the identical table below.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var deadlineErrorSentinels = []error{helpers.ErrMetadataFetchDeadline, helpers.ErrStateObjectDeadline}

// deadlineErrorCases is TestDeadlineErrorClassification's table, hoisted to
// package level so the test function itself stays within the linter's
// length budget. The first two rows are the positive control every
// "unchanged" row below depends on: they prove this exact dlCtx/parent
// fixture is capable of normalizing at all, so an "unchanged" result on a
// later row is a real refusal, not evidence of a fixture that can never
// accept.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var deadlineErrorCases = []deadlineErrorCase{
	{
		name: "own deadline fired, parent live, cause wraps context.DeadlineExceeded: normalized",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err: errDeadlineCauseWrapsDeadlineExceeded,
	},
	{
		name: "own deadline fired, parent live, cause wraps context.Canceled: normalized",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err: errDeadlineCauseWrapsCanceled,
	},
	{
		// The causal precondition: an HTTP status error carries no context
		// signal, so it must pass through unchanged even though the budget
		// expired in the same instant. Losing this would relabel a 404 (or a
		// 401/403, or a retryable status) into the deadline sentinel and abort
		// a server-list walk that should instead advance or classify by
		// status.
		name: "own deadline fired, parent live, cause is an HTTP status error: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      &HTTPStatusError{URL: "https://example.com", Status: "404 Not Found", Code: http.StatusNotFound},
		wantSame: true,
	},
	{
		// The state-object side of the identical precondition: a corrupt
		// registry's actionable message must survive even though the budget
		// expired in the same instant.
		name: "own deadline fired, parent live, cause is a corrupt project registry error: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      helpers.ErrCorruptProjectRegistry,
		wantSame: true,
	},
	{
		name: "parent explicitly canceled: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			return parent, cancel
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, deadlineTestBudget)
		},
		err:      context.Canceled,
		wantSame: true,
	},
	{
		// dlCtx is a child of parent, so once parent's own deadline expires
		// first, dlCtx inherits context.DeadlineExceeded from it too:
		// dlCtx.Err() alone cannot tell "my own budget expired" apart from "I
		// merely inherited my parent's expiry". Without the parent.Err() != nil
		// check ahead of it, this would be misclassified as this operation's
		// own deadline firing.
		name: "parent's own deadline (not this operation's budget) expired first: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			<-parent.Done()
			return parent, cancel
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, deadlineTestBudget)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      errDeadlineCauseWrapsDeadlineExceeded,
		wantSame: true,
	},
	{
		name: "dlCtx still live: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, time.Hour)
		},
		err:      errDeadlineCauseWrapsDeadlineExceeded,
		wantSame: true,
	},
	{
		name: "nil error stays nil",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      nil,
		wantSame: true,
	},
}

// TestDeadlineErrorClassification pins deadlineError's normalization table,
// run against every sentinel it is used with.
func TestDeadlineErrorClassification(t *testing.T) {
	t.Parallel()
	for _, sentinel := range deadlineErrorSentinels {
		for _, tc := range deadlineErrorCases {
			t.Run(sentinel.Error()+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				parent, parentCancel := tc.buildParent()
				defer parentCancel()
				dlCtx, dlCancel := tc.buildDl(parent)
				defer dlCancel()

				got := deadlineError(parent, dlCtx, deadlineTestBudget, sentinel, tc.err)
				if tc.wantSame {
					assertDeadlineErrorUnchanged(t, got, tc.err)
					return
				}
				assertDeadlineErrorNormalized(t, got, sentinel, tc.err)
			})
		}
	}
}

// TestMetadataDeadlineErrorPreservesHTTPStatusRouting asserts the property
// the root-metadata server walk actually consumes, not bare identity: an
// *HTTPStatusError surviving deadlineError still satisfies errors.As with its
// original Code intact, and the result does not carry
// helpers.ErrMetadataFetchDeadline. The positive control on the same
// dlCtx/parent fixture - a cause that does carry a context signal - DOES
// produce the sentinel, proving the causal precondition is a real refusal
// rather than a fixture incapable of ever normalizing.
func TestMetadataDeadlineErrorPreservesHTTPStatusRouting(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	dlCtx, dlCancel := context.WithTimeout(parent, time.Nanosecond)
	defer dlCancel()
	<-dlCtx.Done()

	statusErr := &HTTPStatusError{URL: "https://example.com", Status: "404 Not Found", Code: http.StatusNotFound}
	got := MetadataDeadlineError(parent, dlCtx, deadlineTestBudget, statusErr)

	var route *HTTPStatusError
	if !errors.As(got, &route) {
		t.Fatalf("errors.As(got, &*HTTPStatusError) failed on %v, want success (the server walk consumes this property)", got)
	}
	if route.Code != http.StatusNotFound {
		t.Fatalf("route.Code = %d, want %d", route.Code, http.StatusNotFound)
	}
	if errors.Is(got, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("MetadataDeadlineError(%v) = %v, must not carry the sentinel", statusErr, got)
	}

	positiveControl := MetadataDeadlineError(parent, dlCtx, deadlineTestBudget, errDeadlineCauseWrapsDeadlineExceeded)
	if !errors.Is(positiveControl, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("positive control: MetadataDeadlineError = %v, want errors.Is ErrMetadataFetchDeadline", positiveControl)
	}
}

// TestDeadlineErrorIsIdempotent asserts a second normalization pass over an
// already-normalized error returns an identical message rather than doubling
// the sentinel or its rendered cause into it - the contract fetchJSONBody
// relies on when it may normalize once inside a retry attempt and again
// around the whole helpers.Retry loop.
//
// This uses two fixtures, not one, and the reason is load-bearing. The real
// shape fetchJSONBody's own retry loop produces - deadlineError's step 5
// renders the cause with %v, never %w - already cannot carry a raw
// context.DeadlineExceeded/context.Canceled through errors.Is once wrapped,
// so a second pass over that real shape is ALSO refused by the causal
// precondition (step 4) even with the err == nil || errors.Is(err, sentinel)
// early return deleted: that mutation does not double the message here,
// because step 4 already blocks it independently. alreadyNormalized below is
// the fixture that actually isolates the idempotence check: it deliberately
// double-wraps the sentinel AND context.DeadlineExceeded with %w - a shape
// no real producer in this codebase builds, mirroring
// internal/galaxy/cache/retry_test.go's own "stalledSynthetic" precedent,
// which pins an ordering guard independently of how the real producer
// happens to render its cause. Against that fixture, step 4's causal
// precondition passes (the raw signal IS reachable), so only the idempotence
// early return stands between it and a doubled message.
func TestDeadlineErrorIsIdempotent(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	dlCtx, dlCancel := context.WithTimeout(parent, time.Nanosecond)
	defer dlCancel()
	<-dlCtx.Done()

	once := deadlineError(parent, dlCtx, deadlineTestBudget, helpers.ErrMetadataFetchDeadline, errDeadlineCauseWrapsDeadlineExceeded)
	twice := deadlineError(parent, dlCtx, deadlineTestBudget, helpers.ErrMetadataFetchDeadline, once)
	if twice.Error() != once.Error() {
		t.Fatalf("re-normalizing the real shape changed the message: once=%q twice=%q", once.Error(), twice.Error())
	}

	alreadyNormalized := fmt.Errorf("%w after 1s: %w", helpers.ErrMetadataFetchDeadline, context.DeadlineExceeded)
	reNormalized := deadlineError(parent, dlCtx, deadlineTestBudget, helpers.ErrMetadataFetchDeadline, alreadyNormalized)
	if reNormalized.Error() != alreadyNormalized.Error() {
		t.Fatalf("re-normalizing the isolating fixture changed the message: once=%q twice=%q",
			alreadyNormalized.Error(), reNormalized.Error())
	}
	if n := strings.Count(reNormalized.Error(), helpers.ErrMetadataFetchDeadline.Error()); n != 1 {
		t.Fatalf("sentinel text appears %d times in %q, want exactly 1 (idempotency must not double-wrap)", n, reNormalized.Error())
	}
}

// assertDeadlineErrorUnchanged fails the test unless got is want, left
// untouched by deadlineError.
func assertDeadlineErrorUnchanged(t *testing.T, got, want error) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("deadlineError = %v, want nil", got)
		}
		return
	}
	if !errors.Is(got, want) {
		t.Fatalf("deadlineError = %v, want unchanged %v", got, want)
	}
}

// assertDeadlineErrorNormalized fails the test unless got is cause normalized
// into sentinel: it matches sentinel, it does not also match
// context.DeadlineExceeded or context.Canceled (the %v-not-%w contract), and
// its message contains cause's own text.
func assertDeadlineErrorNormalized(t *testing.T, got, sentinel, cause error) {
	t.Helper()
	if !errors.Is(got, sentinel) {
		t.Fatalf("deadlineError = %v, want errors.Is %v", got, sentinel)
	}
	if errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("deadlineError = %v, must not match context.DeadlineExceeded", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("deadlineError = %v, must not match context.Canceled", got)
	}
	if !strings.Contains(got.Error(), cause.Error()) {
		t.Fatalf("deadlineError = %q, want it to contain the cause %q", got.Error(), cause.Error())
	}
}
