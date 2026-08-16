package collections

// This file pins signatureDeadlineError's classification table, and one thing
// above all: the %v-not-%w rendering that keeps a spent signature budget from
// classifying as a caller's own Ctrl-C. It is modeled on
// internal/galaxy/cache/deadline_test.go's TestDeadlineErrorClassification,
// which pins the identical rule for the metadata and state-object budgets.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// signatureBudget is the budget every row below passes. Its value never
// decides a verdict; it only has to appear in the rendered message.
const signatureBudget = time.Second

// The two shapes a gather really produces once its budget expires: the
// signature fetcher wraps whatever ended the transfer behind
// helpers.ErrSignatureSourceUnavailable, and what it wraps is
// context.DeadlineExceeded in the ordinary case or context.Canceled when a
// watchdog's own derived cancel wins the race. Declared as package-level vars,
// per err113, rather than built inline in the table.
var (
	errSignatureCauseDeadline = fmt.Errorf("%w: %q: %w",
		helpers.ErrSignatureSourceUnavailable, "https://sigs.example/a.asc", context.DeadlineExceeded)
	errSignatureCauseCanceled = fmt.Errorf("%w: %q: %w",
		helpers.ErrSignatureSourceUnavailable, "https://sigs.example/a.asc", context.Canceled)
)

// signatureDeadlineCase is one row of the table below.
type signatureDeadlineCase struct {
	buildParent func() (context.Context, context.CancelFunc)
	buildSig    func(parent context.Context) (context.Context, context.CancelFunc)
	err         error
	name        string
	wantSame    bool
}

// expiredSigCtx builds a signature budget that has already expired under a live
// parent, which is the state every normalizing row needs.
func expiredSigCtx(parent context.Context) (context.Context, context.CancelFunc) {
	sigCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
	<-sigCtx.Done()
	return sigCtx, cancel
}

// liveParent builds a parent context nothing has ended.
func liveParent() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// signatureDeadlineCases is TestSignatureFetchDeadlineClassification's table,
// hoisted to package level so the test function stays within the length
// budget. The first two rows are the positive control every "unchanged" row
// below depends on: they prove this exact parent/sigCtx pair is capable of
// normalizing at all, so an "unchanged" verdict later is a real refusal rather
// than a fixture that could never accept.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var signatureDeadlineCases = []signatureDeadlineCase{
	{
		name:        "own budget fired, parent live, cause wraps context.DeadlineExceeded: normalized",
		buildParent: liveParent,
		buildSig:    expiredSigCtx,
		err:         errSignatureCauseDeadline,
	},
	{
		name:        "own budget fired, parent live, cause wraps context.Canceled: normalized",
		buildParent: liveParent,
		buildSig:    expiredSigCtx,
		err:         errSignatureCauseCanceled,
	},
	{
		// The caller's own cancellation outranks the budget: a Ctrl-C during a
		// signature fetch has to stay reachable as one, which is the single
		// exception cmd/go-galaxy/exitcode's isCanceled documents.
		name: "parent canceled first: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			return parent, cancel
		},
		buildSig: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, signatureBudget)
		},
		err:      errSignatureCauseCanceled,
		wantSame: true,
	},
	{
		// sigCtx inherits its parent's expiry, so sigCtx.Err() alone cannot tell
		// "my own budget expired" from "my parent's did". The parent check is
		// what separates them.
		name: "parent's own deadline expired first: unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			<-parent.Done()
			return parent, cancel
		},
		buildSig: func(parent context.Context) (context.Context, context.CancelFunc) {
			sigCtx, cancel := context.WithTimeout(parent, signatureBudget)
			<-sigCtx.Done()
			return sigCtx, cancel
		},
		err:      errSignatureCauseDeadline,
		wantSame: true,
	},
	{
		name:        "sigCtx still live: unchanged",
		buildParent: liveParent,
		buildSig: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, time.Hour)
		},
		err:      errSignatureCauseDeadline,
		wantSame: true,
	},
	{
		name:        "nil error stays nil",
		buildParent: liveParent,
		buildSig:    expiredSigCtx,
		err:         nil,
		wantSame:    true,
	},
}

// TestSignatureFetchDeadlineClassification walks signatureDeadlineCases,
// asserting for every normalized row that the sentinel is reachable and that
// neither context sentinel is - the %v-not-%w contract, which is what keeps a
// hostile or degraded signature host from being reported as a caught Ctrl-C.
//
// KILLING MUTATION, run and reverted: the %v in signatureDeadlineError's final
// fmt.Errorf changed to %w. Both normalized rows fail; the first reads:
//
//	verify_deadline_test.go:171: signatureDeadlineError = collection
//	signature fetch deadline exceeded after 1s: collection signature source
//	unavailable: "https://sigs.example/a.asc": context deadline exceeded,
//	must not match context.DeadlineExceeded
func TestSignatureFetchDeadlineClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range signatureDeadlineCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent, parentCancel := tc.buildParent()
			defer parentCancel()
			sigCtx, sigCancel := tc.buildSig(parent)
			defer sigCancel()

			got := signatureDeadlineError(parent, sigCtx, signatureBudget, tc.err)
			if tc.wantSame {
				if tc.err == nil {
					if got != nil {
						t.Fatalf("signatureDeadlineError = %v, want nil", got)
					}
					return
				}
				if !errors.Is(got, tc.err) {
					t.Fatalf("signatureDeadlineError = %v, want unchanged %v", got, tc.err)
				}
				return
			}
			if !errors.Is(got, helpers.ErrSignatureFetchDeadline) {
				t.Fatalf("signatureDeadlineError = %v, want errors.Is helpers.ErrSignatureFetchDeadline", got)
			}
			if errors.Is(got, context.DeadlineExceeded) {
				t.Fatalf("signatureDeadlineError = %v, must not match context.DeadlineExceeded", got)
			}
			if errors.Is(got, context.Canceled) {
				t.Fatalf("signatureDeadlineError = %v, must not match context.Canceled", got)
			}
			if !strings.Contains(got.Error(), tc.err.Error()) {
				t.Fatalf("signatureDeadlineError = %q, want it to carry the cause %q", got.Error(), tc.err.Error())
			}
		})
	}
}

// TestSignatureDeadlineErrorIsIdempotent asserts a second normalization pass
// over an already-normalized error returns the same message rather than
// doubling the sentinel into it.
//
// The fixture deliberately re-wraps the sentinel AND context.DeadlineExceeded
// with %w - a shape no producer here builds, mirroring
// internal/galaxy/cache/deadline_test.go's own isolating fixture - because that
// is what actually isolates the idempotence check: the real shape's cause is
// rendered with %v, so a second pass over it carries no reachable context
// sentinel to re-wrap in the first place.
func TestSignatureDeadlineErrorIsIdempotent(t *testing.T) {
	t.Parallel()
	parent, parentCancel := liveParent()
	defer parentCancel()
	sigCtx, sigCancel := expiredSigCtx(parent)
	defer sigCancel()

	alreadyNormalized := fmt.Errorf("%w after 1s: %w", helpers.ErrSignatureFetchDeadline, context.DeadlineExceeded)
	got := signatureDeadlineError(parent, sigCtx, signatureBudget, alreadyNormalized)
	if got.Error() != alreadyNormalized.Error() {
		t.Fatalf("re-normalizing changed the message: once=%q twice=%q", alreadyNormalized.Error(), got.Error())
	}
	if n := strings.Count(got.Error(), helpers.ErrSignatureFetchDeadline.Error()); n != 1 {
		t.Fatalf("sentinel text appears %d times in %q, want exactly 1", n, got.Error())
	}
}
