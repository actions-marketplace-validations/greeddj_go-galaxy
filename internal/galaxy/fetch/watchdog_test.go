package fetch

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// testIdle is the inactivity window used by every test in this file. It is
// small enough that a watchdog-fired case resolves quickly, but every
// assertion still waits on a channel rather than on elapsed wall-clock time,
// so a slow CI runner cannot make these tests flaky.
const testIdle = 20 * time.Millisecond

// waitBound is the generous upper bound every test gives itself to observe
// a Read (or a goroutine) complete, so a genuine deadlock fails the test
// instead of hanging the suite.
const waitBound = 2 * time.Second

// blockingReadCloser is a synthetic io.ReadCloser whose Read blocks until
// ctx is done, then returns ctx.Err(). It closes started right before
// blocking, letting a test wait deterministically until the read is
// actually in flight instead of racing a goroutine against a sleep.
type blockingReadCloser struct {
	ctx     context.Context //nolint:containedctx // test double: ctx is what the blocked Read waits on, not a stored request context.
	started chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func newBlockingReadCloser(ctx context.Context) *blockingReadCloser {
	return &blockingReadCloser{ctx: ctx, started: make(chan struct{})}
}

// Read signals started, then blocks until ctx is done and returns its error.
func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// Close records that it was called; it never errors.
func (r *blockingReadCloser) Close() error {
	r.closed.Store(true)
	return nil
}

// gatedReadCloser blocks every Read until release is closed, then returns
// context.Canceled - the error a real body read returns once its request
// context ends. Unlike blockingReadCloser it watches no context at all, which
// is what lets a test hold a read still long enough to drive the one
// interleaving watchdogBody.Read's b.parentCtx.Err() == nil guard exists to
// resolve: the watchdog timer has already fired AND the caller's parent
// context has already been canceled, both before the read returns.
type gatedReadCloser struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedReadCloser() *gatedReadCloser {
	return &gatedReadCloser{started: make(chan struct{}), release: make(chan struct{})}
}

// Read signals started, then blocks until release is closed and returns
// context.Canceled, mirroring what a real body read returns once its request
// context ends.
func (r *gatedReadCloser) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, context.Canceled
}

// Close is a no-op; nothing in this file asserts on whether it was called
// for a gatedReadCloser.
func (r *gatedReadCloser) Close() error {
	return nil
}

// closeTrackingReadCloser adapts a plain io.Reader (e.g. strings.NewReader)
// into an io.ReadCloser that records whether Close was called.
type closeTrackingReadCloser struct {
	io.Reader

	closed atomic.Bool
}

// Close records that it was called; it never errors.
func (c *closeTrackingReadCloser) Close() error {
	c.closed.Store(true)
	return nil
}

// readResult carries a Read call's outcome across a goroutine boundary.
// err is ordered first so the struct's pointer-containing prefix is as
// short as possible for the garbage collector to scan.
type readResult struct {
	err error
	n   int
}

// TestWatchdogBody_StallReportsErrReadStalled arms a body whose underlying
// Read never returns on its own; only the watchdog timer firing cancels the
// context it is blocked on. This is the "the watchdog itself is the reason
// the read ended" case, which must surface as helpers.ErrReadStalled while
// the parent context is still live.
func TestWatchdogBody_StallReportsErrReadStalled(t *testing.T) {
	t.Parallel()

	parentCtx, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	// wctx mirrors what watchdogTransport.RoundTrip derives from the
	// request context: a child the watchdog timer can cancel on its own,
	// independent of the parent.
	wctx, wcancel := context.WithCancel(parentCtx)
	defer wcancel()

	body := newBlockingReadCloser(wctx)
	wb := newWatchdogBody(parentCtx, body, wcancel, testIdle)

	done := make(chan readResult, 1)
	go func() {
		n, err := wb.Read(make([]byte, 8))
		done <- readResult{err: err, n: n}
	}()

	select {
	case res := <-done:
		if !errors.Is(res.err, helpers.ErrReadStalled) {
			t.Fatalf("Read error = %v, want errors.Is(err, helpers.ErrReadStalled)", res.err)
		}
		if res.n != 0 {
			t.Fatalf("Read n = %d, want 0", res.n)
		}
	case <-time.After(waitBound):
		t.Fatal("Read did not return once the watchdog should have fired; the timer never unblocked it")
	}

	if parentCtx.Err() != nil {
		t.Fatalf("parent context Err() = %v, want nil: the watchdog must not touch the parent context", parentCtx.Err())
	}
}

// TestWatchdogBody_StallErrorDoesNotMatchContextCanceled uses the identical
// construction as TestWatchdogBody_StallReportsErrReadStalled (its positive
// control on the same fixture: that test proves the acceptance side,
// errors.Is(err, helpers.ErrReadStalled); this test proves the refusal side,
// !errors.Is(err, context.Canceled)) but additionally asserts that the
// watchdog's cancellation cause - context.Canceled, raised by the watchdog
// canceling its own derived context to unblock the stuck read - is not
// reachable through errors.Is on the returned error, even though the read's
// underlying error was genuinely context.Canceled. This is what makes a
// persistently stalled read classify as a network failure (exitcode.FromError
// checks context.Canceled ahead of every other class) instead of being
// mistaken for a caught Ctrl-C. The text assertion answers a different
// question - "does rendering the cause with %v instead of wrapping it with %w
// lose diagnostic information?" - and it does not: the identical text still
// renders into the message, only matchability through errors.Is is removed;
// that assertion uses t.Errorf, not t.Fatalf, so a failure there is reported
// without masking the two errors.Is assertions above it.
func TestWatchdogBody_StallErrorDoesNotMatchContextCanceled(t *testing.T) {
	t.Parallel()

	parentCtx, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	wctx, wcancel := context.WithCancel(parentCtx)
	defer wcancel()

	body := newBlockingReadCloser(wctx)
	wb := newWatchdogBody(parentCtx, body, wcancel, testIdle)

	done := make(chan readResult, 1)
	go func() {
		n, err := wb.Read(make([]byte, 8))
		done <- readResult{err: err, n: n}
	}()

	select {
	case res := <-done:
		if !errors.Is(res.err, helpers.ErrReadStalled) {
			t.Fatalf("Read error = %v, want errors.Is(err, helpers.ErrReadStalled)", res.err)
		}
		if errors.Is(res.err, context.Canceled) {
			t.Fatalf("Read error = %v, must not match context.Canceled: the stall error must render "+
				"its cancellation cause with %%v, not wrap it with %%w, or it steals the exit-code "+
				"classification of a genuine Ctrl-C", res.err)
		}
		if !strings.Contains(res.err.Error(), "context canceled") {
			t.Errorf("Read error = %q, want it to still contain %q: rendering the cause with %%v "+
				"must not drop it from the message, only from errors.Is matchability", res.err.Error(), "context canceled")
		}
	case <-time.After(waitBound):
		t.Fatal("Read did not return once the watchdog should have fired; the timer never unblocked it")
	}
}

// TestWatchdogBody_ParentCancelPropagatesContextCanceled covers the
// opposite case: the CALLER cancels its own (parent) context while the
// watchdog's idle window is far too long to have fired. The resulting error
// must be exactly context.Canceled, never helpers.ErrReadStalled, since
// callers branch on that distinction (e.g. Ctrl-C exit-code handling).
//
// This fixture's own idle window (an hour) is deliberately too long for the
// watchdog timer to ever fire during the test, so fired stays false and the
// ErrReadStalled branch in Read is never reached here at all - neither the
// %v-vs-%w rendering choice nor the parentCtx.Err() == nil guard changes this
// test's outcome, since both only matter once fired is true. The %v-vs-%w
// rendering distinction is instead pinned by
// TestWatchdogBody_StallErrorDoesNotMatchContextCanceled above, which does
// drive the watchdog to fire.
func TestWatchdogBody_ParentCancelPropagatesContextCanceled(t *testing.T) {
	t.Parallel()

	parentCtx, parentCancel := context.WithCancel(t.Context())
	wctx, wcancel := context.WithCancel(parentCtx)
	defer wcancel()

	body := newBlockingReadCloser(wctx)
	// An idle window generous enough that this test's own runtime can
	// never make the watchdog itself fire; only parentCancel below can
	// unblock the read.
	wb := newWatchdogBody(parentCtx, body, wcancel, time.Hour)
	defer func() { _ = wb.Close() }()

	done := make(chan readResult, 1)
	go func() {
		n, err := wb.Read(make([]byte, 8))
		done <- readResult{err: err, n: n}
	}()

	// Wait until the read has actually entered its blocking phase before
	// canceling, so the cancellation is guaranteed to be what unblocks it
	// rather than racing a goroutine that has not started yet.
	select {
	case <-body.started:
	case <-time.After(waitBound):
		t.Fatal("Read never started")
	}
	parentCancel()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("Read error = %v, want errors.Is(err, context.Canceled)", res.err)
		}
		if errors.Is(res.err, helpers.ErrReadStalled) {
			t.Fatalf("Read error = %v, must not be helpers.ErrReadStalled: the parent context, not the watchdog, ended this read", res.err)
		}
	case <-time.After(waitBound):
		t.Fatal("Read did not return once the parent context was canceled")
	}
}

// TestWatchdogBody_FiredWatchdogYieldsToParentCancel pins the one cell of the
// truth table Read's guard at
// `err != nil && b.fired.Load() && b.parentCtx.Err() == nil` decides that no
// other test in this file independently covers: fired=true (the watchdog
// already fired) crossed with parent already canceled. Read renders this
// cell's error with %v, not %w, so deleting the parentCtx guard would
// silently turn a genuine operator Ctrl-C landing in this exact interleaving
// into helpers.ErrReadStalled - a network failure, not an interrupt - rather
// than leaving context.Canceled reachable through errors.Is the way %w
// rendering would. This cell is exactly where the guard is load-bearing,
// which is why it earns its own pin.
//
// gatedReadCloser (not blockingReadCloser) is required here: blockingReadCloser
// unblocks the instant onStall cancels wctx, before a test could ever also
// cancel the parent first. gatedReadCloser watches no context at all, so a
// test can hold the read blocked through both events - the watchdog firing,
// then (in one subtest) the parent being canceled - and only then let it
// return, deterministically producing the interleaving this guard exists for.
//
// The wait for <-wctx.Done() is the deterministic barrier for "the watchdog
// has fired": onStall calls b.fired.Store(true) before b.cancel(), and the
// channel close/receive gives the happens-before edge that guarantees fired
// is visible as true once wctx.Done() is observed. This is not a sleep-based
// fixture on purpose - that class of fixture is exactly what would let the
// cell pinned above go unverified.
//
// "parent still live" is the positive control on this exact construction: it
// proves gatedReadCloser can still produce the ordinary fired-and-stalled
// label, so "parent canceled"'s refusal to produce that label is not vacuous.
//
// Killing mutation, run for real: deleting `&& b.parentCtx.Err() == nil` from
// Read. Both assertions in the "parent canceled" subtest fail while "parent
// still live" keeps passing - a discriminating kill, not a fixture-wide
// breakage (both lines report line 314, the "parent canceled" t.Run
// closure's own runFiredWatchdogCase call site, since both
// runFiredWatchdogCase and checkFiredWatchdogResult call t.Helper()):
//
//	watchdog_test.go:314: Read error = network read stalled: no data for 20ms: context canceled,
//	want errors.Is(err, context.Canceled)
//	watchdog_test.go:314: Read error = network read stalled: no data for 20ms: context canceled,
//	must not match helpers.ErrReadStalled: the parent was already canceled before the read
//	returned, so the guard must yield to it even though the watchdog had already fired
//	--- PASS: TestWatchdogBody_FiredWatchdogYieldsToParentCancel/parent_still_live (0.02s)
//	--- FAIL: TestWatchdogBody_FiredWatchdogYieldsToParentCancel/parent_canceled_before_the_read_returns (0.02s)
func TestWatchdogBody_FiredWatchdogYieldsToParentCancel(t *testing.T) {
	t.Parallel()

	t.Run("parent still live", func(t *testing.T) {
		t.Parallel()
		runFiredWatchdogCase(t, false)
	})
	t.Run("parent canceled before the read returns", func(t *testing.T) {
		t.Parallel()
		runFiredWatchdogCase(t, true)
	})
}

// runFiredWatchdogCase drives the shared sequence
// TestWatchdogBody_FiredWatchdogYieldsToParentCancel needs for both of its
// subtests: start a read on a gated body, wait for the watchdog to fire,
// optionally cancel the parent context while the read is still blocked, then
// release the read and check what error it returned.
func runFiredWatchdogCase(t *testing.T, cancelParent bool) {
	t.Helper()

	parentCtx, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	wctx, wcancel := context.WithCancel(parentCtx)
	defer wcancel()

	body := newGatedReadCloser()
	wb := newWatchdogBody(parentCtx, body, wcancel, testIdle)

	done := make(chan readResult, 1)
	go func() {
		n, err := wb.Read(make([]byte, 8))
		done <- readResult{err: err, n: n}
	}()

	select {
	case <-body.started:
	case <-time.After(waitBound):
		t.Fatal("Read never started")
	}

	// The read is now blocked inside gatedReadCloser.Read, with the watchdog
	// timer armed. Wait for the watchdog to actually fire - see this test's
	// doc comment for why this channel wait, not a sleep, is what makes fired
	// deterministically true by the time this select returns.
	select {
	case <-wctx.Done():
	case <-time.After(waitBound):
		t.Fatal("watchdog never fired (wctx was never canceled)")
	}

	if cancelParent {
		parentCancel()
	}
	// Only now does the blocked read actually return, with both preconditions
	// (fired, and - in the cancelParent case - a dead parent) already true.
	close(body.release)

	select {
	case res := <-done:
		checkFiredWatchdogResult(t, cancelParent, res.err)
	case <-time.After(waitBound):
		t.Fatal("Read did not return after release was closed")
	}
}

// checkFiredWatchdogResult asserts runFiredWatchdogCase's expected outcome
// for one of its two subtests, split out from runFiredWatchdogCase purely to
// stay under the cyclomatic-complexity budget.
func checkFiredWatchdogResult(t *testing.T, cancelParent bool, err error) {
	t.Helper()

	if cancelParent {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Read error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, helpers.ErrReadStalled) {
			t.Errorf("Read error = %v, must not match helpers.ErrReadStalled: the parent was already "+
				"canceled before the read returned, so the guard must yield to it even though the "+
				"watchdog had already fired", err)
		}
		return
	}
	if !errors.Is(err, helpers.ErrReadStalled) {
		t.Errorf("Read error = %v, want errors.Is(err, helpers.ErrReadStalled)", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Read error = %v, must not match context.Canceled", err)
	}
}

// TestWatchdogBody_NormalReadAndClose covers the common, non-stalled path:
// data is available immediately, Read returns it with no error, and Close
// closes the underlying body cleanly.
func TestWatchdogBody_NormalReadAndClose(t *testing.T) {
	t.Parallel()

	const want = "hello, watchdog"
	body := &closeTrackingReadCloser{Reader: strings.NewReader(want)}
	wb := newWatchdogBody(t.Context(), body, func() {}, time.Second)

	got, err := io.ReadAll(wb)
	if err != nil {
		t.Fatalf("ReadAll error = %v, want nil", err)
	}
	if string(got) != want {
		t.Fatalf("ReadAll = %q, want %q", got, want)
	}

	if err := wb.Close(); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
	if !body.closed.Load() {
		t.Fatal("Close did not close the underlying body")
	}
}

// TestWatchdogBody_CloseWithoutRead ensures Close is safe even when no Read
// ever ran, so the timer was never armed (it is nil), exercising the guard
// in Close that skips Stop in that case.
func TestWatchdogBody_CloseWithoutRead(t *testing.T) {
	t.Parallel()

	body := &closeTrackingReadCloser{Reader: strings.NewReader("unread")}
	wb := newWatchdogBody(t.Context(), body, func() {}, time.Second)

	if err := wb.Close(); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
	if !body.closed.Load() {
		t.Fatal("Close did not close the underlying body")
	}
}
