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

// TestWatchdogBody_ParentCancelPropagatesContextCanceled covers the
// opposite case: the CALLER cancels its own (parent) context while the
// watchdog's idle window is far too long to have fired. The resulting error
// must be exactly context.Canceled, never helpers.ErrReadStalled, since
// callers branch on that distinction (e.g. Ctrl-C exit-code handling).
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
