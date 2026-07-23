package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// watchdogTransport wraps a base http.RoundTripper so that every response
// body read through it is guarded by a per-read inactivity timer: once idle
// elapses between two body reads (or before the first one), the stuck read
// is unblocked instead of hanging indefinitely. This replaces the whole
// response http.Client.Timeout, which bounded total transfer time rather
// than progress, and so truncated large-but-healthy artifact downloads.
type watchdogTransport struct {
	base http.RoundTripper
	idle time.Duration
}

// RoundTrip performs the request through the base transport on a
// cancelable context derived from the request's own context, then - on
// success - wraps the response body so every subsequent Read is guarded by
// the watchdog. The round trip itself runs against the derived context
// (not the caller's) because that is the context the watchdog timer
// cancels to unblock a stalled Read; net/http's response body reads honor
// the context the round trip was made with.
func (t watchdogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	wctx, cancel := context.WithCancel(req.Context())
	resp, err := t.base.RoundTrip(req.Clone(wctx))
	if err != nil {
		cancel()
		return resp, err
	}
	resp.Body = newWatchdogBody(req.Context(), resp.Body, cancel, t.idle)
	return resp, nil
}

// watchdogBody wraps a response body so that no single Read call - nor the
// gap before the first one - may block for longer than idle without
// making progress. It distinguishes a watchdog-triggered stall from a
// cancellation of the caller's own (parent) context: only the former is
// reported as helpers.ErrReadStalled, since a caller cancellation must
// surface as context.Canceled for callers that branch on it.
//
// watchdogBody is not safe for concurrent use. Like a bare io.Reader, Read
// must not be called concurrently with itself; and unlike a raw
// http.Response.Body - which conventionally allows a Close from another
// goroutine to abort a blocked Read - it must also not be Closed concurrently
// with an in-flight Read, because both touch the lazily-armed timer without
// synchronization. This is deliberate: a mutex on every read would tax the
// download hot path to defend a pattern no caller uses. The fetch client's
// callers read a body to completion and then Close it, aborting a stalled
// transfer via context cancellation rather than a concurrent Close, so the
// constraint holds; a stalled read is unblocked by the watchdog canceling the
// request context, not by Close.
type watchdogBody struct {
	body io.ReadCloser
	//nolint:containedctx // parentCtx is the caller's original request
	// context, kept only to distinguish "watchdog fired" from "caller
	// canceled" on a read error; it is never used to start work, dial, or
	// spawn goroutines, so the usual leak/propagation concerns do not apply.
	parentCtx context.Context
	cancel    context.CancelFunc
	timer     *time.Timer
	idle      time.Duration
	// fired is set by the timer goroutine and read by whichever goroutine
	// is inside Read; atomic because those are two different goroutines
	// touching it concurrently.
	fired atomic.Bool
}

// newWatchdogBody constructs a watchdogBody. The timer is armed lazily by
// the first Read, not here, so a body that is never read - a HEAD response,
// or a caller that discards the body outright - never pays for a timer it
// does not need.
func newWatchdogBody(parentCtx context.Context, body io.ReadCloser, cancel context.CancelFunc, idle time.Duration) *watchdogBody {
	return &watchdogBody{body: body, parentCtx: parentCtx, cancel: cancel, idle: idle}
}

// Read arms (or rearms) the inactivity timer, performs the underlying read
// - which may block until data arrives, the timer fires, or the request
// context ends - and then stops the timer before returning. A read that
// fails while the watchdog has fired and the caller's own context is still
// live is reported as helpers.ErrReadStalled; any other error, including
// one caused by the caller canceling its own context, propagates
// unchanged. Read must not be called concurrently with itself or with Close
// (see the watchdogBody type doc for the concurrency contract).
func (b *watchdogBody) Read(p []byte) (int, error) {
	if b.timer == nil {
		b.timer = time.AfterFunc(b.idle, b.onStall)
	} else {
		b.timer.Reset(b.idle)
	}
	n, err := b.body.Read(p)
	b.timer.Stop()
	if err != nil && b.fired.Load() && b.parentCtx.Err() == nil {
		return n, fmt.Errorf("%w: no data for %s: %w", helpers.ErrReadStalled, b.idle, err)
	}
	return n, err
}

// Close stops the timer, cancels the derived context, and closes the
// underlying body. Both the cancel and the timer Stop are idempotent, so a
// Close following an already-stalled or already-canceled body is safe. Close
// must not be called concurrently with an in-flight Read (see the
// watchdogBody type doc for the concurrency contract).
func (b *watchdogBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.cancel()
	return b.body.Close()
}

// onStall runs on the timer's own goroutine when idle elapses with no Read
// progress. It records that the watchdog - rather than the caller - is
// responsible for what happens next, then cancels the round trip's derived
// context to unblock whatever Read is currently in flight.
func (b *watchdogBody) onStall() {
	b.fired.Store(true)
	b.cancel()
}
