package cache

// This file covers helpers.MetadataFetchDeadline end to end against a plain
// httptest byte-drip fixture: a JSON response that stays syntactically
// in-progress forever, making genuine (if glacial) progress the whole time,
// so it never trips a read-inactivity watchdog and can only be caught by a
// whole-request deadline.
//
// Each test below was verified against a real revert of the production
// change it pins, and this comment quotes the actual observed output:
//
//   - TestMetadataByteDripFailsAtTheFetchDeadline, reverting fetchJSONBody to
//     pass ctx straight through to helpers.Retry/fetchJSONBodyOnce (dropping
//     the context.WithTimeout and both deadlineError calls), hangs rather
//     than fails - that hang is the defect itself, and is the strongest
//     evidence in this change. Run with a bounded -timeout so the harness
//     kills it rather than blocking the suite forever, the observed output
//     is:
//     "panic: test timed out after 5s
//     running tests:
//     TestMetadataByteDripFailsAtTheFetchDeadline (5s)"
//     with the stuck goroutine's frame at
//     "github.com/greeddj/go-galaxy/internal/galaxy/cache.
//     fetchJSONBodyOnce(...)", reading the never-ending drip body, directly
//     beneath "github.com/greeddj/go-galaxy/internal/galaxy/helpers.Retry(...)"
//     in the same trace.
// The count assertion does NOT pin where the budget is established: a budget
// established fresh per attempt (inside fetchJSONBodyOnce instead of around
// the whole helpers.Retry loop) would also yield a count of 1, since the
// sentinel is terminal either way per fetchRetryable's
// isEarlyTerminalFetchError. That the budget is one shared acquisition
// budget, not one per attempt, is enforced by construction at its single
// establishment point (fetchJSONBody) and documented there.
//
// One claim this file deliberately does NOT make, having actually tried it:
// forcing fetchRetryable to answer true for helpers.ErrMetadataFetchDeadline
// does not raise the request count above 1 against this fixture, unlike the
// artifact-download deadline's own byte-drip test. In this deterministic
// fixture, the only way an attempt can ever fail is the budget itself
// expiring - the drip never stops making progress, so nothing else ends the
// read - which means dlCtx (passed as helpers.Retry's own ctx) is already
// closed by the time fetchRetryable's answer would matter. helpers.Retry's
// backoff wait races its own ctx.Done() against the jittered timer before a
// second attempt ever starts, and an already-closed channel always wins that
// select, so it returns a bare ctx.Err() instead of calling attempt() again
// - regardless of what fetchRetryable says. fetchRetryable's classification
// of this sentinel is instead pinned directly and unambiguously by
// TestFetchRetryableTreatsTheMetadataDeadlineAsTerminal in retry_test.go,
// which does not depend on this timing coincidence.
//
// The count assertion's near-documentary status goes further than that one
// tried mutation: no SINGLE-part mutation moves it off 1, verified against
// each of the following in turn. Deleting isEarlyTerminalFetchError's check
// from fetchRetryable leaves the sentinel classified by the default-deny
// fallthrough at the bottom of that function, still false. Moving the budget
// to be established fresh per attempt (inside fetchJSONBodyOnce) instead of
// once around the whole helpers.Retry loop still yields a count of 1 on its
// own, since the sentinel is terminal either way once it reaches
// fetchRetryable. Even removing helpers.Retry's own ctx.Done() case from its
// backoff-wait select leaves the count at 1 in this fixture, because
// fetchRetryable's false answer makes Retry return at the `!retryable(err)`
// check, before that select is ever reached - the select's own ctx.Done()
// case is not what is holding the count down here. The count is only
// reachable above 1 through the COMPOUND mutation "establish the budget fresh
// per attempt AND make fetchRetryable classify the sentinel retryable"
// together: only then does each attempt get its own fresh, not-yet-expired
// dlCtx to retry against. The design forbids that compound in two
// independent places at once (fetchJSONBody's single shared-budget
// establishment point, and fetchRetryable's terminal classification of this
// sentinel), which is exactly what TestFetchRetryableTreatsTheMetadataDeadlineAsTerminal
// pins directly. The count assertion here is a tripwire on that compound
// state, not an independent pin of either half.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// metadataDripInterval is the drip cadence used by the fault fixture in this
// file: fast enough that several bytes are written well inside the 200ms
// budget every test here uses, slow enough that the request never legitimately
// completes on its own within that budget.
const metadataDripInterval = 5 * time.Millisecond

// metadataDripBudget is the fetchJSONBody budget every test in this file
// passes.
const metadataDripBudget = 200 * time.Millisecond

// newMetadataDripServer starts an httptest server whose single handler
// writes a JSON content type and an opening "{" byte, then - when drip is
// true - one ASCII space per metadataDripInterval, flushed onto the wire,
// forever, until the request's context ends: a JSON document that stays
// syntactically in-progress forever and can never be parsed. When drip is
// false, it instead writes one small, complete, valid JSON body. requests
// counts every request the handler receives.
func newMetadataDripServer(t *testing.T, drip bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !drip {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if _, err := w.Write([]byte("{")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(metadataDripInterval):
			}
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestMetadataByteDripFailsAtTheFetchDeadline asserts a byte-drip response -
// which always makes progress and so never trips a read-inactivity watchdog
// - is caught by the whole-request metadata fetch deadline instead, failing
// closed with helpers.ErrMetadataFetchDeadline (matching neither
// context.DeadlineExceeded nor context.Canceled through errors.Is, per the
// sentinel's own %v-not-%w rule), with the endpoint hit exactly once.
func TestMetadataByteDripFailsAtTheFetchDeadline(t *testing.T) {
	t.Parallel()
	srv, requests := newMetadataDripServer(t, true)

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, nil, &out, Policy{}, metadataDripBudget)
	if err == nil {
		t.Fatal("expected an error from a byte-dripped metadata response, got nil")
	}
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("expected errors.Is ErrMetadataFetchDeadline, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("must not match context.DeadlineExceeded: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (the deadline is terminal: no retry follows it)", got)
	}
}

// TestMetadataDripFixturePositiveControl is the positive control for
// TestMetadataByteDripFailsAtTheFetchDeadline: the identical fixture with
// the drip disabled fetches and decodes successfully under the same budget,
// proving the 200ms budget used above is not itself what would fail an
// ordinary fetch against this fixture.
func TestMetadataDripFixturePositiveControl(t *testing.T) {
	t.Parallel()
	srv, requests := newMetadataDripServer(t, false)

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, nil, &out, Policy{}, metadataDripBudget)
	if err != nil {
		t.Fatalf("FetchJSONWithCachePolicy: %v", err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected out to decode {ok:true}, got %v", out)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}
