package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// foreignToken stands in for a lock token belonging to some other acquirer
// across the tests that seed a lock object out-of-band to simulate
// contention, reclaim, or takeover scenarios.
const foreignToken = "foreign-token"

// errRawInFlightPlaceholder stands in for tryAcquireOnce's raw in-flight S3
// transport failure in TestWaitCeilingErrClassification's table: only its
// non-nilness matters to waitCeilingErr, so one static sentinel serves every
// row that needs a placeholder, rather than a fresh dynamic error per row.
var errRawInFlightPlaceholder = errors.New("raw in-flight transport failure")

// testLockTiming returns a lockTiming with ttl set to the given value and
// every other interval shrunk to make the lock's state machine fast and
// deterministic in tests. Callers needing a non-default waitCeiling,
// heartbeatInterval, etc. can copy the returned value and override fields.
//
// These shrunken intervals sit on top of the client's fixed retry policy
// (s3RetryPolicy, base s3RetryBackoffBase = 200ms, 4 attempts), which tests
// cannot shrink; heartbeatOpTimeout and releaseTimeout are sized for clean
// round trips, so a single jittered retry backoff can consume a whole op
// budget. Therefore any test that injects a retryable 5xx on a lock-path key
// must synchronize on the fault having been consumed, never sleep a number
// of intervals.
func testLockTiming(ttl time.Duration) lockTiming {
	return lockTiming{
		ttl:                ttl,
		heartbeatInterval:  30 * time.Millisecond,
		heartbeatOpTimeout: 200 * time.Millisecond,
		releaseTimeout:     200 * time.Millisecond,
		waitCeiling:        time.Second,
		backoffBase:        10 * time.Millisecond,
		backoffCap:         50 * time.Millisecond,
	}
}

// lockEventWaitCeiling is a LIVENESS ceiling, not a timing margin - the
// tests using waitForLockEvent assert on observed events, so a slow machine
// only makes them slower, never wrong, and the ceiling fires only when the
// event genuinely never happens.
const lockEventWaitCeiling = 10 * time.Second

// lockEventPollInterval is how often waitForLockEvent re-checks its
// condition while waiting.
const lockEventPollInterval = 2 * time.Millisecond

// waitForLockEvent blocks until cond reports true, polling every
// lockEventPollInterval, and fails the test once lockEventWaitCeiling
// elapses without cond becoming true. It lets tests synchronize on something
// the heartbeat observably did - a request the fake served, a deadline the
// lock object now records - instead of sleeping a fixed number of heartbeat
// intervals, which only ever buys a probabilistic head start.
func waitForLockEvent(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(lockEventWaitCeiling)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", lockEventWaitCeiling, what)
		}
		time.Sleep(lockEventPollInterval)
	}
}

// newLockFake starts a fake S3 server and returns its endpoint and client,
// so multiple Backends can be built against the same in-memory bucket to
// exercise cross-backend lock contention.
func newLockFake(t *testing.T) (string, *http.Client) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

// newLockBackendAt builds a Backend pointed at an existing fake S3 endpoint
// (as returned by newLockFake), with timing set to timing.
func newLockBackendAt(t *testing.T, endpoint string, client *http.Client, timing lockTiming) *Backend {
	t.Helper()
	cfg := config.S3CacheConfig{
		Endpoint:  endpoint,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	b, err := New(cfg, client, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.lock = timing
	return b
}

// newLockBackendWithFake starts a fresh fake S3 server and returns a Backend
// pointed at it alongside the underlying fake, so a test can reach into the
// fake's test-only knobs (e.g. deleteDelay) that Backend itself exposes no
// way to set.
func newLockBackendWithFake(t *testing.T, timing lockTiming) (*Backend, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return newLockBackendAt(t, srv.URL, srv.Client(), timing), fake
}

// testHolderCancel returns a holder-context cancel function shaped exactly
// like the one acquireLock threads through the acquisition protocol, for the
// tests below that call one step of that protocol directly instead of going
// through Lock. The context it belongs to is discarded: those tests assert on
// the returned lockAttempt, while the holder context's own behavior is
// covered end to end by TestLockHolderContextCanceledWhenOwnershipLost and
// its positive control. t.Cleanup cancels it so nothing outlives the test.
func testHolderCancel(t *testing.T) context.CancelCauseFunc {
	t.Helper()
	_, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return cancel
}

// seedLockObject writes the lock object directly (bypassing acquireLock),
// under foreignToken, to set up contention/reclaim scenarios ahead of a real
// Lock call - every caller in this suite simulates a different acquirer, so
// the token is fixed rather than taken as a parameter.
func seedLockObject(ctx context.Context, t *testing.T, b *Backend, deadline time.Time) {
	t.Helper()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	key := b.key(locksPrefix, lockObject)
	if err := b.putLock(ctx, key, foreignToken, deadline, putCondition{}); err != nil {
		t.Fatalf("seed lock object: %v", err)
	}
}

// TestLockAcquiresOnEmptyBucket confirms a fresh bucket lets Lock succeed
// immediately and stamps the new wire format's token and deadline metadata.
func TestLockAcquiresOnEmptyBucket(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(time.Minute)
	ctx := context.Background()

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	key := b.key(locksPrefix, lockObject)
	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if token := headers.Get("X-Amz-Meta-Token"); token == "" {
		t.Fatalf("expected a non-empty lock token")
	}
	deadline, err := time.Parse(time.RFC3339, headers.Get("X-Amz-Meta-Deadline"))
	if err != nil {
		t.Fatalf("parse deadline: %v", err)
	}
	if !deadline.After(time.Now()) {
		t.Fatalf("expected a future deadline, got %v", deadline)
	}
}

// TestLockConcurrentFreshAcquireSingleWinner confirms that when two Backends
// race to create the same absent lock, exactly one wins and the other times
// out waiting rather than both succeeding or both failing outright. The
// winner's release is deliberately deferred until after both Lock calls
// have returned: releasing it immediately would legitimately free the lock
// for the loser to acquire afterward, which is correct behavior but would
// make this particular assertion (a single winner from the race itself)
// meaningless.
func TestLockConcurrentFreshAcquireSingleWinner(t *testing.T) {
	t.Parallel()
	endpoint, client := newLockFake(t)

	timing := testLockTiming(time.Minute)
	timing.waitCeiling = 300 * time.Millisecond
	b1 := newLockBackendAt(t, endpoint, client, timing)
	b2 := newLockBackendAt(t, endpoint, client, timing)

	type lockResult struct {
		release func() error
		err     error
	}
	results := make([]lockResult, 2)
	var wg sync.WaitGroup
	for i, b := range []*Backend{b1, b2} {
		wg.Go(func() {
			_, release, err := b.Lock(context.Background())
			results[i] = lockResult{release: release, err: err}
		})
	}
	wg.Wait()

	var successes, timeouts, otherErrs int
	for _, r := range results {
		switch {
		case r.err == nil:
			successes++
			_ = r.release()
		case errors.Is(r.err, errS3LockWaitTimeout):
			timeouts++
		default:
			otherErrs++
		}
	}

	if successes != 1 {
		t.Fatalf("expected exactly one winner, got %d successes (timeouts=%d, other=%d)", successes, timeouts, otherErrs)
	}
	if timeouts != 1 {
		t.Fatalf("expected exactly one timeout, got %d (successes=%d, other=%d)", timeouts, successes, otherErrs)
	}
}

// TestLockReclaimsExpiredLock seeds a lock object whose TTL has elapsed
// (judged by Last-Modified staleness, which is what lockExpired checks)
// under a foreign token, then confirms a single Backend
// reclaims it: the stored token becomes the reclaimer's and the deadline
// moves forward from the seeded (deliberately far-past) one. The comparison
// is against the seeded deadline rather than time.Now(): RFC3339 only
// carries second resolution, so a deadline computed a mere ttl (tens of
// milliseconds) beyond "now" can round-trip to a timestamp that is not
// reliably after a later call to time.Now() taken outside the test's
// control; an hour-old seeded deadline avoids that flakiness entirely.
func TestLockReclaimsExpiredLock(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(20 * time.Millisecond)
	ctx := context.Background()

	pastDeadline := time.Now().UTC().Add(-time.Hour)
	seedLockObject(ctx, t, b, pastDeadline)

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	key := b.key(locksPrefix, lockObject)
	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got == foreignToken {
		t.Fatalf("expected the reclaimer's token, still foreign-token")
	}
	deadline, err := time.Parse(time.RFC3339, headers.Get("X-Amz-Meta-Deadline"))
	if err != nil {
		t.Fatalf("parse deadline: %v", err)
	}
	if !deadline.After(pastDeadline) {
		t.Fatalf("expected the reclaim to move the deadline forward from %v, got %v", pastDeadline, deadline)
	}
}

// TestLockWaitsThenTimesOutOnLiveLock seeds a live (freshly-written, so not
// yet TTL-expired) foreign lock and confirms Lock backs off at least once
// before giving up with errS3LockWaitTimeout once waitCeiling elapses.
func TestLockWaitsThenTimesOutOnLiveLock(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	timing := testLockTiming(5 * time.Second) // stays live for the whole test window
	timing.waitCeiling = 150 * time.Millisecond
	b.lock = timing
	ctx := context.Background()

	seedLockObject(ctx, t, b, time.Now().UTC().Add(b.lock.ttl))

	start := time.Now()
	_, _, err := b.Lock(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, errS3LockWaitTimeout) {
		t.Fatalf("expected errS3LockWaitTimeout, got %v", err)
	}
	if elapsed < b.lock.backoffBase {
		t.Fatalf("expected at least one backoff sleep before timing out, elapsed %v", elapsed)
	}
	// errS3LockWaitTimeout carries helpers.ErrCacheBusy (see variables.go's
	// partition doc): a live foreign lock that outlasts the wait ceiling is
	// exactly the contention shape cmd/go-galaxy/exitcode's ExitCacheBusy
	// exists for. TestLockAcquirePropagatesCallerCancellation is this
	// assertion's negative control on the same acquireLock loop: a caller
	// cancellation racing the identical contention must NOT match
	// helpers.ErrCacheBusy.
	if !errors.Is(err, helpers.ErrCacheBusy) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBusy), got %v", err)
	}
}

// newSilentEndpointBackend builds a Backend pointed at a live TCP listener
// that accepts every connection and never answers a single request (see
// newAcceptingNeverRespondingListener). client is built and assigned to the
// Backend directly, bypassing Open's ensureBucket/probeConditionalPut calls:
// both would otherwise run on the raw ctx Backend.Lock passes to Open, before
// acquireLockLoop ever constructs waitCtx, so they are unbounded by waitCeiling -
// calling them against a listener that never answers would hang the test
// itself rather than exercising acquireLockLoop's wait ceiling, which is
// this helper's entire point.
func newSilentEndpointBackend(t *testing.T, waitCeiling time.Duration) *Backend {
	t.Helper()
	ln, _ := newAcceptingNeverRespondingListener(t)
	cfg := config.S3CacheConfig{
		Endpoint:  "http://" + ln.Addr().String(),
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	client, err := newClient(cfg, http.DefaultClient)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	timing := testLockTiming(time.Minute)
	timing.waitCeiling = waitCeiling
	return &Backend{client: client, lock: timing}
}

// TestLockWaitCeilingDistinguishesSilentBackendFromContention pins
// waitCeilingErr's discriminator between the two failures the S3 lock's
// wait ceiling can produce, under an identical wait ceiling for both rows so
// neither timing difference can explain the result: a silent endpoint that
// never answers a single request must classify as an unreached backend
// (helpers.ErrCacheBackendUnavailable), never as contention
// (helpers.ErrCacheBusy) - the defect this test guards against is exactly a
// silent, reachable endpoint being reported as "another process holds the
// cache". The second row is this table's positive control: a live foreign
// lock that outlasts the identical ceiling proves the fixture family (and
// waitCeilingErr itself) still produces genuine contention when contention is
// what actually happened, so the first row's refusal is not merely a
// classifier that never fires at all.
func TestLockWaitCeilingDistinguishesSilentBackendFromContention(t *testing.T) {
	t.Parallel()

	const waitCeiling = 200 * time.Millisecond

	tests := []struct {
		build    func(t *testing.T) *Backend
		name     string
		wantBusy bool
	}{
		{
			name: "silent endpoint never answers a request",
			build: func(t *testing.T) *Backend {
				t.Helper()
				return newSilentEndpointBackend(t, waitCeiling)
			},
			wantBusy: false,
		},
		{
			name: "live foreign lock outlasts the ceiling",
			build: func(t *testing.T) *Backend {
				t.Helper()
				b := newTestBackend(t)
				timing := testLockTiming(5 * time.Second) // stays live for the whole test window
				timing.waitCeiling = waitCeiling
				b.lock = timing
				seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))
				return b
			},
			wantBusy: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.build(t)

			_, _, err := b.Lock(context.Background())
			if err == nil {
				t.Fatalf("expected Lock to fail, got nil")
			}

			if got := errors.Is(err, helpers.ErrCacheBusy); got != tc.wantBusy {
				t.Fatalf("errors.Is(err, helpers.ErrCacheBusy) = %v, want %v (err: %v)", got, tc.wantBusy, err)
			}
			if tc.wantBusy {
				return
			}
			if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
				t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
			}
		})
	}
}

// TestLockAcquireTimesOutAfterObservationThenSilence pins acquireLockLoop's
// accumulation of observedHolder across attempts, not just the attempt in
// flight when the wait ceiling fires: a live foreign lock is seeded so the
// very first HEAD observes it, and every HEAD after that one hangs - never
// answering at all - until the wait ceiling itself ends the wait. The
// attempt actually in flight when acquireLockLoop gives up therefore observed
// nothing; Lock must still report errS3LockWaitTimeout, proving the
// accumulation is a logical OR across the whole wait, not the last attempt's
// own answer. This is also waitCeilingErr's own "stale evidence" residual
// (see its doc comment) made executable: the backend genuinely goes silent,
// rather than merely failing, for the remainder of the wait.
func TestLockAcquireTimesOutAfterObservationThenSilence(t *testing.T) {
	t.Parallel()

	fake := newFakeS3()
	key := path.Join(locksPrefix, lockObject)
	lockPath := "/" + fake.bucket + "/" + key

	var headCount atomic.Int32
	// hangGate lets every HEAD to the lock key past the first block
	// indefinitely, simulating a backend that goes silent for the rest of
	// the wait. It is closed, and only then is the server closed, in a
	// single t.Cleanup - the same ordering newAcceptingNeverRespondingListener
	// uses for its own held connections: closing the server first would
	// block in Close() waiting for this still-blocked handler to return,
	// the exact teardown hang this ordering avoids.
	hangGate := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == lockPath {
			if headCount.Add(1) == 1 {
				fake.ServeHTTP(w, r)
				return
			}
			select {
			case <-r.Context().Done():
				http.Error(w, r.Context().Err().Error(), http.StatusGatewayTimeout)
			case <-hangGate:
			}
			return
		}
		fake.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		close(hangGate)
		srv.Close()
	})

	timing := testLockTiming(5 * time.Second) // stays live for the whole test window
	timing.waitCeiling = 300 * time.Millisecond
	b := newLockBackendAt(t, srv.URL, srv.Client(), timing)

	seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))

	_, _, err := b.Lock(context.Background())
	if !errors.Is(err, errS3LockWaitTimeout) {
		t.Fatalf("expected errS3LockWaitTimeout, got %v", err)
	}
	// lockPath is assembled here rather than asked of the backend, and it
	// equals the client's real request path only while the backend prefix is
	// empty. Should the two ever diverge, no HEAD is gated, every attempt
	// observes the holder, and the assertion above passes for the wrong
	// reason - as a duplicate of TestLockWaitsThenTimesOutOnLiveLock.
	if got := headCount.Load(); got < 2 {
		t.Fatalf("HEAD count on the lock key = %d, want at least 2: the gate never matched, so no attempt was ever silenced", got)
	}
}

// TestLockAcquireSurfacesAHardFailureOverTheCeiling pins
// acquireLockAttemptErr's live-waitCtx branch: an attempt that comes back
// with its own failure while the wait ceiling is still live ends the whole
// acquisition immediately, carrying that call's own sentinel, rather than
// being held until the ceiling and reclassified as a wait-ceiling outcome.
// It is the executable proof of waitCeilingErr's "a backend that answers
// only with failures is a different case" paragraph.
//
// The first HEAD is served by the fake on purpose, so this wait genuinely
// observes a live foreign holder before the failures start: that is what
// makes the interesting half observable - a hard failure is not converted
// into errS3LockWaitTimeout even when this wait already has the observation
// that would otherwise justify it.
//
// waitCeiling is set far above the client's own retry budget so the ceiling
// cannot be what ends the wait; it is a liveness margin, not a timing
// assertion, and nothing here asserts elapsed time.
func TestLockAcquireSurfacesAHardFailureOverTheCeiling(t *testing.T) {
	t.Parallel()

	fake := newFakeS3()
	key := path.Join(locksPrefix, lockObject)
	lockPath := "/" + fake.bucket + "/" + key

	var headCount atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == lockPath && headCount.Add(1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fake.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	timing := testLockTiming(30 * time.Second) // stays live for the whole test window
	timing.waitCeiling = 5 * time.Second
	b := newLockBackendAt(t, srv.URL, srv.Client(), timing)

	seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))

	_, _, err := b.Lock(context.Background())
	if !errors.Is(err, errS3HeadFailed) {
		t.Fatalf("expected errS3HeadFailed, got %v", err)
	}
	// Documentary, not pinned: neither can be the first failing line here.
	// errS3HeadFailed and the two wait-ceiling sentinels are disjoint error
	// trees, so any state satisfying the assertion above already fails both
	// of these. They are kept because they name the confusion this test
	// exists to rule out.
	if errors.Is(err, errS3LockWaitTimeout) || errors.Is(err, errS3LockWaitNoHolderObserved) {
		t.Fatalf("a hard failure must not be reclassified as a wait-ceiling outcome, got %v", err)
	}
	// Same reasoning as its sibling above: without this, a diverged lockPath
	// would leave every HEAD served normally, and the run would fail on
	// something other than the branch under test.
	if got := headCount.Load(); got < 2 {
		t.Fatalf("HEAD count on the lock key = %d, want at least 2: the gate never matched, so no attempt ever failed hard", got)
	}
}

// TestLockAcquirePropagatesCallerCancellation confirms that when the
// caller's own context is canceled while Lock is still contending for a
// live foreign lock, the caller cancellation is what surfaces - not
// errS3LockWaitTimeout - even though both errors originate from the same
// waitCtx.Done() signal internally. waitCeiling and the foreign lock's ttl
// are both set far longer than the test can possibly run, so the wait
// ceiling itself cannot fire; only the explicit cancel() can end the wait.
func TestLockAcquirePropagatesCallerCancellation(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	timing := testLockTiming(10 * time.Second)
	timing.waitCeiling = 10 * time.Second
	b.lock = timing

	seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := b.Lock(ctx)
		errCh <- err
	}()

	time.Sleep(2 * b.lock.backoffBase)
	cancel()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Lock did not return after caller cancellation")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if errors.Is(err, errS3LockWaitTimeout) {
		t.Fatalf("expected the caller cancellation, not errS3LockWaitTimeout, got %v", err)
	}
	// Negative control for TestLockWaitsThenTimesOutOnLiveLock, whose positive
	// assertion this test mirrors: the identical live-foreign-lock contention,
	// ended by the caller's own cancellation instead of the wait ceiling
	// firing on its own, must NOT match helpers.ErrCacheBusy - waitCeilingErr
	// propagates parent.Err() unchanged in that branch, never
	// errS3LockWaitTimeout.
	if errors.Is(err, helpers.ErrCacheBusy) {
		t.Fatalf("expected the caller cancellation, not helpers.ErrCacheBusy, got %v", err)
	}
}

// TestWaitCeilingErrClassification directly unit-tests waitCeilingErr over
// all three of its parameters: a canceled parent wins even over an observed
// holder; observed alone decides contention versus no-observation, regardless
// of inFlight; and inFlight only matters, as a diagnostic cause, once
// observed is false. This is the table that pins observed's priority over
// inFlight: observed is checked first, so a non-nil inFlight - which ordinary
// contention produces about as often as a nil one, since the wait ceiling
// lands inside an in-flight attempt about as often as inside a backoff sleep
// - never overrides a genuine observation.
func TestWaitCeilingErrClassification(t *testing.T) {
	t.Parallel()

	canceledParent, cancel := context.WithCancel(context.Background())
	cancel()

	// noHolderSynthetic is deliberately NOT a shape tryAcquireOnce's own
	// in-flight transport errors ever take: it double-wraps context.Canceled
	// with %w, a signature no producer in this package builds for an
	// in-flight S3 call's failure. It exists to pin that waitCeilingErr's %v
	// rendering of inFlight keeps context.Canceled unreachable through
	// errors.Is regardless of what inFlight itself happens to wrap, not only
	// for the shapes a real producer builds today - the same reason
	// retry_test.go's stalledSynthetic exists for s3Retryable.
	noHolderSynthetic := fmt.Errorf("synthetic in-flight failure: %w", context.Canceled)

	tests := []waitCeilingErrCase{
		{
			name:         "a canceled parent wins even over an observed holder",
			parent:       canceledParent,
			observed:     true,
			wantCanceled: true,
		},
		{
			name:     "an observed holder is contention regardless of a non-nil inFlight",
			parent:   context.Background(),
			observed: true,
			inFlight: errRawInFlightPlaceholder,
			wantBusy: true,
		},
		{
			name:            "no observed holder with a non-nil cause is unavailable",
			parent:          context.Background(),
			observed:        false,
			inFlight:        errRawInFlightPlaceholder,
			wantUnavailable: true,
		},
		{
			name:            "a synthetic cause wrapping context.Canceled still classifies as unavailable, not canceled",
			parent:          context.Background(),
			observed:        false,
			inFlight:        noHolderSynthetic,
			wantUnavailable: true,
		},
		{
			name:            "no observed holder with a nil cause is still unavailable",
			parent:          context.Background(),
			observed:        false,
			inFlight:        nil,
			wantUnavailable: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertWaitCeilingErrClassification(t, tc)
		})
	}
}

// waitCeilingErrCase is one row of TestWaitCeilingErrClassification's table.
type waitCeilingErrCase struct {
	parent          context.Context //nolint:containedctx // a direct table-driven parameter to waitCeilingErr, not a stored request context.
	inFlight        error
	name            string
	observed        bool
	wantBusy        bool
	wantUnavailable bool
	wantCanceled    bool
}

// assertWaitCeilingErrClassification runs one row's body: call waitCeilingErr
// with tc's parameters and assert the result's class membership. Split out of
// TestWaitCeilingErrClassification purely to stay under the funlen budget;
// the two together still cover the same five rows.
func assertWaitCeilingErrClassification(t *testing.T, tc waitCeilingErrCase) {
	t.Helper()
	err := waitCeilingErr(tc.parent, tc.observed, tc.inFlight)
	if err == nil {
		t.Fatalf("expected a non-nil error")
	}
	if got := errors.Is(err, helpers.ErrCacheBusy); got != tc.wantBusy {
		t.Errorf("errors.Is(err, helpers.ErrCacheBusy) = %v, want %v (err: %v)", got, tc.wantBusy, err)
	}
	if got := errors.Is(err, helpers.ErrCacheBackendUnavailable); got != tc.wantUnavailable {
		t.Errorf("errors.Is(err, helpers.ErrCacheBackendUnavailable) = %v, want %v (err: %v)", got, tc.wantUnavailable, err)
	}
	if got := errors.Is(err, context.Canceled); got != tc.wantCanceled {
		t.Errorf("errors.Is(err, context.Canceled) = %v, want %v (err: %v)", got, tc.wantCanceled, err)
	}
}

// TestHeartbeatRefreshesDeadline confirms the background heartbeat advances
// the lock object's deadline while the holder keeps it, without any
// explicit refresh call from the caller.
//
// Rather than sleeping past several heartbeat ticks and comparing two reads
// (the old approach needed a 1.5s sleep just to guarantee the two RFC3339,
// second-resolution timestamps differed), the test rolls the deadline an
// hour into the past under the holder's own token - the same unconditional
// same-token write the heartbeat itself performs - and waits for the
// heartbeat to observably push it forward again. That is unambiguous at any
// resolution and needs no scheduling margin.
func TestHeartbeatRefreshesDeadline(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	readLock := func() (string, time.Time) {
		t.Helper()
		headers, err := b.client.headObject(ctx, key)
		if err != nil {
			t.Fatalf("headObject: %v", err)
		}
		deadline, err := time.Parse(time.RFC3339, headers.Get("X-Amz-Meta-Deadline"))
		if err != nil {
			t.Fatalf("parse deadline: %v", err)
		}
		return headers.Get("X-Amz-Meta-Token"), deadline
	}

	token, acquiredDeadline := readLock()
	// The same unconditional same-token write the heartbeat itself performs,
	// with the deadline moved an hour into the past, so the refresh that
	// follows is observable without waiting for the wall clock to cross the
	// second boundary RFC3339 resolution otherwise requires.
	rolledBack := time.Now().UTC().Add(-time.Hour)
	if err := b.putLock(ctx, key, token, rolledBack, putCondition{}); err != nil {
		t.Fatalf("roll the deadline back: %v", err)
	}

	var refreshed time.Time
	waitForLockEvent(t, "the heartbeat to advance the rolled-back deadline", func() bool {
		holder, deadline := readLock()
		if holder != token {
			t.Fatalf("expected the holder token to stay %q, got %q", token, holder)
		}
		refreshed = deadline
		return deadline.After(rolledBack)
	})
	// The refresh writes a full ttl ahead of its own now, so it can never
	// land before the deadline the acquisition recorded.
	if refreshed.Before(acquiredDeadline) {
		t.Fatalf("expected the refresh to be at least the acquired deadline (%v), got %v", acquiredDeadline, refreshed)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestHeartbeatDetectsLostOwnershipAndReleaseSkipsDelete overwrites the lock
// object out-of-band with a foreign token while a Backend holds it, and
// confirms the heartbeat detects the takeover: release reports
// errS3LockLost and, critically, never deletes the foreign holder's object.
//
// It waits on the holder context closing. The two alternatives an earlier
// revision recorded here are both answered rather than sidestepped:
//
//   - the request stream is genuinely unusable, and that reasoning stands: the
//     fake counts a request when it is SERVED, before the holder has processed
//     the response, so waiting on its HEAD counter lets release's hbCancel
//     abort the very HEAD that would have revealed the takeover. Polled
//     tightly, that variant fails outright. Nothing a server observes can
//     report what the client did with the answer.
//   - the holder context was refused on the ground that consuming it makes
//     this test assert through the mechanism
//     TestLockHolderContextCanceledWhenOwnershipLost exists to pin. That
//     conflates synchronizing with asserting. What this test asserts is
//     unchanged and still exclusively its own: that release REPORTS the loss,
//     and that the foreign object survives. Deleting lost.Store(true) - the
//     flag half, which no other test covers - still fails it, and fails it on
//     its own assertion rather than on the wait, because holderCancel keeps
//     running and the wait keeps completing. What the wait does add is a
//     second way to fail if holderCancel is deleted, which costs exclusive
//     attribution and buys determinism; the fact stays covered either way,
//     since that deletion fails the context test too.
//
// The wait is exact rather than probabilistic because heartbeatTick stores the
// flag BEFORE it cancels: a closed holder context therefore proves the flag is
// already set. That is a happens-before, not a margin - which is what the
// three-heartbeat-interval sleep it replaces never was, and why that sleep
// failed roughly one full package run in eighty-five under -race.
//
// KILLING MUTATION, run and reverted: deleting lost.Store(true) from
// heartbeatTick, leaving the cancel in place. The wait still completes, and the
// failure lands on this test's own assertion, which is the claim above:
//
//	lock_test.go:831: expected errS3LockLost, got <nil>
func TestHeartbeatDetectsLostOwnershipAndReleaseSkipsDelete(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	holderCtx, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	// Simulate a foreign acquirer reclaiming the lock out-of-band (e.g.
	// after this holder stalled past its TTL from S3's point of view).
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(b.lock.ttl), putCondition{}); err != nil {
		t.Fatalf("seed foreign takeover: %v", err)
	}

	select {
	case <-holderCtx.Done():
	case <-time.After(lockEventWaitCeiling):
		t.Fatalf("the heartbeat did not observe the takeover within %v; if holderCancel was removed, "+
			"TestLockHolderContextCanceledWhenOwnershipLost is the test that owns that fact", lockEventWaitCeiling)
	}

	if err := release(); !errors.Is(err, errS3LockLost) {
		t.Fatalf("expected errS3LockLost, got %v", err)
	}

	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != foreignToken {
		t.Fatalf("expected the foreign lock to remain in place, got token %q", got)
	}
}

// TestLockHolderContextCanceledWhenOwnershipLost pins the other consumer of
// the same takeover TestHeartbeatDetectsLostOwnershipAndReleaseSkipsDelete
// covers: the holder context Lock returns must close, with a cause matching
// helpers.ErrCacheLockLost, so the run itself stops working under a lock it
// no longer holds instead of finishing and reporting the loss in one line
// afterward. The takeover is seeded out-of-band with putLock, the same way
// that test does it, rather than with raceTokenOnNextHead: that knob is a
// one-shot swap armed for a specific HEAD and exists to race claim's own
// verification during acquisition, not to model an acquirer that arrives
// while a lock is already held.
//
// TestLockHolderContextStaysLiveWhileOwned is this test's positive control on
// the identical fixture: without it, "the context closed" would be
// indistinguishable from a holder context that is simply never live.
//
// What release does to that same cause on its way out is deliberately not
// re-asserted below: a cause is immutable once set, so a second read after
// release could not differ from the one above whatever release did. That
// ordering is pinned by TestReleaseRacingTheTickKeepsTheLossCause instead,
// which races release against a tick that has not decided yet.
func TestLockHolderContextCanceledWhenOwnershipLost(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	holderCtx, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(b.lock.ttl), putCondition{}); err != nil {
		t.Fatalf("seed foreign takeover: %v", err)
	}

	// A bounded select, not a sleep: the closing of the holder context IS the
	// event this test is about, so it can be waited on directly. The ceiling
	// is a liveness bound - a slow machine only makes this slower, never
	// wrong - matching waitForLockEvent's own contract.
	select {
	case <-holderCtx.Done():
	case <-time.After(lockEventWaitCeiling):
		t.Fatalf("holder context still live %v after a takeover; the run would keep writing without the lock", lockEventWaitCeiling)
	}
	if cause := context.Cause(holderCtx); !errors.Is(cause, helpers.ErrCacheLockLost) {
		t.Fatalf("context.Cause(holderCtx) = %v, want errors.Is helpers.ErrCacheLockLost", cause)
	}

	assertReleaseReportsLossAndKeepsForeignLock(t, b, release, key)
}

// assertReleaseReportsLossAndKeepsForeignLock runs the release half of a
// takeover scenario: release must report errS3LockLost and must leave the
// foreign holder's object in place. Split out of
// TestLockHolderContextCanceledWhenOwnershipLost purely to stay under the
// funlen budget.
func assertReleaseReportsLossAndKeepsForeignLock(t *testing.T, b *Backend, release func() error, key string) {
	t.Helper()
	if err := release(); !errors.Is(err, errS3LockLost) {
		t.Fatalf("release = %v, want errS3LockLost", err)
	}
	headers, err := b.client.headObject(context.Background(), key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != foreignToken {
		t.Fatalf("expected the foreign lock to remain in place, got token %q", got)
	}
}

// TestLockHolderContextStaysLiveWhileOwned is
// TestLockHolderContextCanceledWhenOwnershipLost's positive control on the
// identical acquisition, with the one difference that matters: nobody takes
// the lock away. The holder context must still be live after several
// heartbeat ticks have observably happened, proving the cancellation that
// test observes is a response to the takeover rather than a context that was
// never usable in the first place.
//
// It also pins the no-leak half of the Backend.Lock contract: a clean release
// must end the holder context too, so a long-lived parent does not accumulate
// a child per run - and must end it with a plain cancellation, never a
// lock-loss cause, or every successful run would classify as exit 8.
func TestLockHolderContextStaysLiveWhileOwned(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	holderCtx, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	putsAtAcquire := fake.requestCount(key, http.MethodPut)
	// Three refresh PUTs the fake actually served, rather than three sleeps:
	// each one proves a whole heartbeat tick ran to completion under this
	// token, which is exactly the window a takeover would have been noticed
	// in.
	waitForLockEvent(t, "three heartbeat refresh PUTs under our own token", func() bool {
		return fake.requestCount(key, http.MethodPut) >= putsAtAcquire+3
	})
	if err := holderCtx.Err(); err != nil {
		t.Fatalf("holderCtx.Err() = %v, want nil while this run still holds the lock", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := b.client.headObject(ctx, key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected the lock object to be deleted, headObject error = %v", err)
	}
	assertHolderContextEndedCleanly(t, holderCtx.Err(), context.Cause(holderCtx))
}

// assertHolderContextEndedCleanly asserts the three things a clean release
// owes the holder context, in an order where each is reachable while every
// check above it passes: it ended at all (no leaked child), it did not end
// with a lock-loss cause (which would classify a successful run as exit 8),
// and the cause it did end with is plain cancellation. It takes the two
// already-read values rather than the context itself, so the helper keeps
// *testing.T first without tripping revive's context-as-argument rule.
func assertHolderContextEndedCleanly(t *testing.T, ctxErr, cause error) {
	t.Helper()
	if ctxErr == nil {
		t.Fatalf("holder context still live after a clean release; the parent leaks a child per run")
	}
	if errors.Is(cause, helpers.ErrCacheLockLost) {
		t.Fatalf("holder context cause after a clean release = %v, must not be a lock-loss cause", cause)
	}
	if !errors.Is(cause, context.Canceled) {
		t.Fatalf("holder context cause after a clean release = %v, want context.Canceled", cause)
	}
}

// TestHeartbeatSurvivesTransientHeadFailures forces the heartbeat's HEAD
// (verifyOwner) calls to fail a bounded number of times with a transient
// server error, then recover, while the holder keeps the lock. It confirms
// heartbeatTick's error branch never flips the lost flag on its own: only a
// definitive token mismatch may do that. Release afterward must succeed
// (not errS3LockLost) and must actually delete the lock object, proving the
// heartbeat kept refreshing normally once the forced failures were spent.
//
// The fault is sized to the client's whole retry budget
// (s3RetryMaxAttempts) on purpose: a 500 is retryable, so a smaller fault is
// absorbed by headObject's own retry and heartbeatTick never sees an error
// at all - the branch this test is named for would go untested while the
// test still passed. A full budget guarantees the tick that runs first
// exhausts its retries and hands verifyOwner a real error. How long that
// takes is not predictable (a single full-jitter backoff draw can outlast a
// whole heartbeat interval), so the test waits for the refresh PUT that only
// follows a successful verifyOwner HEAD, rather than sleeping a number of
// ticks.
func TestHeartbeatSurvivesTransientHeadFailures(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	headsAtArm := fake.requestCount(key, http.MethodHead)
	putsAtArm := fake.requestCount(key, http.MethodPut)
	fake.failNext(key, http.MethodHead, http.StatusInternalServerError, s3RetryMaxAttempts)

	// The heartbeat only issues a refresh PUT after a verifyOwner HEAD
	// succeeded, and a HEAD can only succeed once every armed failure has
	// been served - so one new PUT proves the forced failures were spent and
	// the heartbeat recovered, and proves nothing is left armed to ambush
	// release's own HEAD.
	waitForLockEvent(t, "a heartbeat refresh PUT after the forced HEAD failures", func() bool {
		return fake.requestCount(key, http.MethodPut) > putsAtArm
	})

	// Every forced failure plus the HEAD that finally succeeded: proof the
	// fault was really injected and really exhausted, not silently skipped.
	if heads := fake.requestCount(key, http.MethodHead) - headsAtArm; heads < s3RetryMaxAttempts+1 {
		t.Fatalf("expected %d forced failures and a successful HEAD, got %d HEADs after arming", s3RetryMaxAttempts, heads)
	}

	if err := release(); err != nil {
		t.Fatalf("expected release to succeed after transient HEAD failures, got %v", err)
	}
	if _, err := b.client.headObject(ctx, key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected the lock object to be deleted, headObject error = %v", err)
	}
}

// TestReclaimIfExpiredLosesRaceOnRecreate directly unit-tests
// reclaimIfExpired's losing-the-recreate-race branch: the existing lock is
// expired (so this call deletes it), but a forced failure makes the
// following create-if-absent PUT report precondition-failed - exactly as
// real S3 would if another acquirer's create landed first. reclaimIfExpired
// must report this as "not acquired, no immediate retry" (nil release,
// retryNow=false) and, since that other creator is an acquirer this call did
// observe, observed=true - see TestTryAcquireOnceReportsObservationPerBranch
// for the same branch exercised through tryAcquireOnce's full entry point.
func TestReclaimIfExpiredLosesRaceOnRecreate(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = testLockTiming(time.Minute)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	pastDeadline := time.Now().UTC().Add(-time.Hour)
	if err := b.putLock(ctx, key, foreignToken, pastDeadline, putCondition{}); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}

	// The next PUT to this key (the create-if-absent recreate that follows
	// our delete) reports precondition-failed, simulating another
	// acquirer's create winning the race for the just-deleted key.
	fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, 1)

	attempt, err := b.reclaimIfExpired(ctx, key, "our-token", testHolderCancel(t))
	if err != nil {
		t.Fatalf("reclaimIfExpired: %v", err)
	}
	if attempt.release != nil {
		t.Fatalf("expected no release function for a lost recreate race")
	}
	if attempt.retryNow {
		t.Fatalf("expected retryNow=false for a lost recreate race (back off, don't retry immediately)")
	}
	if !attempt.observed {
		t.Fatalf("expected observed=true: another creator winning the race is an acquirer this call saw")
	}
}

// TestReclaimIfExpiredRetriesImmediatelyWhenObjectVanished directly
// unit-tests reclaimIfExpired's vanished-object branch: HEAD on a key that
// was never written returns not-found, which must be reported as an
// immediate-retry signal (retryNow=true) rather than an error, and as
// observed=false: a vanished object proves nothing about another acquirer.
func TestReclaimIfExpiredRetriesImmediatelyWhenObjectVanished(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "never-written-lock")
	attempt, err := b.reclaimIfExpired(ctx, key, "token", testHolderCancel(t))
	if err != nil {
		t.Fatalf("reclaimIfExpired: %v", err)
	}
	if attempt.release != nil {
		t.Fatalf("expected no release function when the object has vanished")
	}
	if !attempt.retryNow {
		t.Fatalf("expected retryNow=true when the object has vanished")
	}
	if attempt.observed {
		t.Fatalf("expected observed=false: a vanished object is not evidence of another acquirer")
	}
}

// TestTryAcquireOnceReportsObservationPerBranch pins lockAttempt.observed at
// each of tryAcquireOnce's branches, against the real fake backend rather
// than by constructing a lockAttempt value directly: the evidence a wait
// ultimately reports as contention (see waitCeilingErr) must come from these
// functions actually answering the backend's responses, not from a value a
// test merely asserts about in isolation.
func TestTryAcquireOnceReportsObservationPerBranch(t *testing.T) {
	t.Parallel()

	tests := []tryAcquireOnceObservationCase{
		{
			name: "a live unexpired foreign holder is observed",
			setup: func(t *testing.T, b *Backend, _ *fakeS3, _ string) {
				t.Helper()
				seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))
			},
			wantObserved: true,
		},
		{
			// The object is absent when this call's own create-if-absent PUT
			// runs, so it succeeds; the armed swap then simulates another
			// acquirer's write landing before this call's own follow-up
			// ownership-verifying HEAD, in the same generation of the object
			// this call itself just created.
			name: "a foreign token racing in right after our own create is observed",
			setup: func(t *testing.T, b *Backend, fake *fakeS3, key string) {
				t.Helper()
				if err := b.Open(context.Background()); err != nil {
					t.Fatalf("Open: %v", err)
				}
				fake.raceTokenOnNextHead(key, foreignToken)
			},
			wantObserved: true,
		},
		{
			// Two real PUTs happen inside this one tryAcquireOnce call: the
			// initial create-if-absent (which fails against the genuinely
			// present, expired seeded object) and the post-delete recreate.
			// Both are forced to precondition-failed here, so the second -
			// the recreate - loses the race exactly as reclaimIfExpired's own
			// final branch expects, regardless of which PUT the fault budget
			// happens to land on first.
			name: "another creator winning the race for the just-deleted object is observed",
			setup: func(t *testing.T, b *Backend, fake *fakeS3, key string) {
				t.Helper()
				seedLockObject(context.Background(), t, b, time.Now().UTC().Add(-time.Hour))
				fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, 2)
			},
			wantObserved: true,
		},
		{
			name: "the object vanishing between our failed create and our HEAD is not observed",
			setup: func(t *testing.T, b *Backend, fake *fakeS3, key string) {
				t.Helper()
				seedLockObject(context.Background(), t, b, time.Now().UTC().Add(b.lock.ttl))
				fake.failNext(key, http.MethodHead, http.StatusNotFound, 1)
			},
			wantRetryNow: true,
		},
		{
			name: "a successful claim on an empty bucket is not observed",
			setup: func(t *testing.T, b *Backend, _ *fakeS3, _ string) {
				t.Helper()
				if err := b.Open(context.Background()); err != nil {
					t.Fatalf("Open: %v", err)
				}
			},
			wantRelease: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertTryAcquireOnceObservation(t, tc)
		})
	}
}

// tryAcquireOnceObservationCase is one row of
// TestTryAcquireOnceReportsObservationPerBranch's table: setup arranges the
// fake and the Backend's lock object into the state that reaches one
// tryAcquireOnce branch, and the want fields describe that branch's expected
// lockAttempt.
type tryAcquireOnceObservationCase struct {
	setup        func(t *testing.T, b *Backend, fake *fakeS3, key string)
	name         string
	wantObserved bool
	wantRetryNow bool
	wantRelease  bool
}

// assertTryAcquireOnceObservation runs one row's body: build a fresh Backend
// and fake, arrange tc's precondition via tc.setup, call tryAcquireOnce once,
// and assert every field of the returned lockAttempt against tc's want
// fields, releasing the lock afterward if one was acquired. Split out of
// TestTryAcquireOnceReportsObservationPerBranch purely to stay under the
// funlen budget; the two together still cover the same five rows.
func assertTryAcquireOnceObservation(t *testing.T, tc tryAcquireOnceObservationCase) {
	t.Helper()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = testLockTiming(time.Minute)
	key := b.key(locksPrefix, lockObject)

	tc.setup(t, b, fake, key)

	attempt, err := b.tryAcquireOnce(context.Background(), key, "our-token", testHolderCancel(t))
	if err != nil {
		t.Fatalf("tryAcquireOnce: %v", err)
	}
	if attempt.observed != tc.wantObserved {
		t.Errorf("attempt.observed = %v, want %v", attempt.observed, tc.wantObserved)
	}
	if attempt.retryNow != tc.wantRetryNow {
		t.Errorf("attempt.retryNow = %v, want %v", attempt.retryNow, tc.wantRetryNow)
	}
	if gotRelease := attempt.release != nil; gotRelease != tc.wantRelease {
		t.Errorf("attempt.release != nil = %v, want %v", gotRelease, tc.wantRelease)
	}
	if attempt.release != nil {
		if err := attempt.release(); err != nil {
			t.Errorf("release: %v", err)
		}
	}
}

// TestLockAcquireBoundsImmediateRetrySpin forces the pathological PUT/HEAD
// inconsistency that acquireLockLoop's retryNow handoff exists to survive: a
// misbehaving S3-compatible backend that answers the create-if-absent PUT
// with 412 (precondition failed) while a follow-up HEAD on the very same key
// keeps reporting the object as missing. Before maxImmediateLockRetries
// bounded this, acquireLockLoop would spin PUT+HEAD with no backoff sleep at all
// for the entire waitCeiling window; this test shrinks waitCeiling so the
// spin (if unbounded) would produce a very large number of requests in a
// short, deterministic window, and confirms both that Lock still terminates
// (not a raw transport/context error leaking out) and that the number of PUT
// attempts against the lock key stays in the tens rather than growing to the
// thousands an unbounded spin would produce over the same window.
//
// The terminal error is errS3LockWaitNoHolderObserved, not errS3LockWaitTimeout:
// this fixture never produces a completed observation of another acquirer -
// every create sees the object present, every HEAD sees it missing, a
// self-contradicting sequence a conforming backend never sustains - so there
// is no holder for "retry when the other run finishes" to be advice about.
func TestLockAcquireBoundsImmediateRetrySpin(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	timing := testLockTiming(time.Minute)
	timing.waitCeiling = 300 * time.Millisecond
	b.lock = timing
	ctx := context.Background()

	// Arm both halves of the inconsistency indefinitely: every create
	// attempt sees the key as already present (412), while every
	// follow-up HEAD sees it as absent (404). A conforming backend can
	// never produce this combination in steady state, only transiently;
	// this fake sustains it for the whole test to exercise the acquirer's
	// own bound rather than relying on the backend to behave.
	key := b.key(locksPrefix, lockObject)
	fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, -1)
	fake.failNext(key, http.MethodHead, http.StatusNotFound, -1)

	start := time.Now()
	_, _, err := b.Lock(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, errS3LockWaitNoHolderObserved) {
		t.Fatalf("expected errS3LockWaitNoHolderObserved, got %v", err)
	}
	// A generous upper margin over waitCeiling: it only needs to catch a
	// genuine hang regression, not pin down the exact backoff-jitter tail.
	if elapsed > timing.waitCeiling+2*time.Second {
		t.Fatalf("expected Lock to terminate close to the wait ceiling (%v), took %v", timing.waitCeiling, elapsed)
	}

	// maxRequestBudget is deliberately generous - order-of-magnitude
	// headroom above what the bounded spin plus its subsequent
	// backoff-gated attempts could plausibly produce in waitCeiling - so
	// the assertion is about the spin being bounded at all (tens, not the
	// thousands an unbounded immediate-retry loop would rack up in the
	// same 300ms window), not about pinning an exact count.
	const maxRequestBudget = 300
	if puts := fake.requestCount(key, http.MethodPut); puts > maxRequestBudget {
		t.Fatalf("expected the PUT spin to stay bounded (budget %d), got %d requests", maxRequestBudget, puts)
	}
}

// TestReleaseLockOnMissingObjectReturnsNil directly unit-tests releaseLock's
// not-found branch: a key that was never written is treated as an already
// released lock, not an error.
func TestReleaseLockOnMissingObjectReturnsNil(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "never-written-lock")
	if err := b.releaseLock(ctx, key, "token"); err != nil {
		t.Fatalf("expected nil for a missing object, got %v", err)
	}
}

// TestReleaseLockOnForeignTokenDoesNotDelete directly unit-tests
// releaseLock's token-mismatch branch: releasing with a token that does not
// match the object's recorded owner must return nil without deleting the
// object, since it belongs to a different holder.
func TestReleaseLockOnForeignTokenDoesNotDelete(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "foreign-lock")
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(time.Hour), putCondition{}); err != nil {
		t.Fatalf("seed foreign lock: %v", err)
	}

	if err := b.releaseLock(ctx, key, "our-token"); err != nil {
		t.Fatalf("expected nil for a foreign-token release, got %v", err)
	}

	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != foreignToken {
		t.Fatalf("expected the foreign lock to remain in place, got token %q", got)
	}
}

// TestReleaseLockPropagatesHeadError directly unit-tests releaseLock's
// non-not-found HEAD error branch: a genuine S3 failure (as opposed to a
// 404) must propagate to the caller rather than being swallowed. The
// failure is armed indefinitely (500 is a retryable status, and
// headObject now retries it internally) so it survives long enough to be
// the terminal error rather than being consumed by a retry that then
// observes the never-created key as a plain 404, which releaseLock treats
// as an already-released lock.
func TestReleaseLockPropagatesHeadError(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "lock-with-head-error")
	fake.failNext(key, http.MethodHead, http.StatusInternalServerError, -1)

	if err := b.releaseLock(ctx, key, "token"); !errors.Is(err, errS3HeadFailed) {
		t.Fatalf("expected errS3HeadFailed to propagate, got %v", err)
	}
}

// TestReleaseDeletesOwnedLock confirms the ordinary path: releasing a lock
// this Backend still owns deletes the object.
func TestReleaseDeletesOwnedLock(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(time.Second)
	ctx := context.Background()

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	if _, err := b.client.headObject(ctx, key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected the lock object to be deleted, headObject error = %v", err)
	}
}

// TestReleaseUsesFreshContextAfterInstallCancel is the core regression test
// for the lock-leak bug: it cancels the caller's context (simulating an
// interrupted or failed install run) before calling release, then confirms
// release still succeeds and the lock object is actually gone from the
// fake - proving its DELETE ran on a fresh context, not the canceled one.
func TestReleaseUsesFreshContextAfterInstallCancel(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	cancel() // simulate the install run this lock belongs to being canceled

	if err := release(); err != nil {
		t.Fatalf("expected release to succeed despite the canceled caller context, got %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	if _, err := b.client.headObject(context.Background(), key); !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected the lock object to be deleted despite the canceled caller context, headObject error = %v", err)
	}
}

// TestReleaseTimeoutIsBounded confirms release's fresh context is itself
// bounded by releaseTimeout: a DELETE that hangs well past releaseTimeout
// causes release to give up with a deadline error rather than block
// indefinitely, and it does so comfortably before the hang would resolve
// on its own.
func TestReleaseTimeoutIsBounded(t *testing.T) {
	t.Parallel()
	timing := testLockTiming(time.Minute)
	timing.releaseTimeout = 50 * time.Millisecond
	b, fake := newLockBackendWithFake(t, timing)

	_, release, err := b.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	const hangDelay = 2 * time.Second
	fake.deleteDelay = hangDelay // far above releaseTimeout

	start := time.Now()
	releaseErr := release()
	elapsed := time.Since(start)

	if !errors.Is(releaseErr, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline-related error, got %v", releaseErr)
	}
	if elapsed >= hangDelay {
		t.Fatalf("expected release to return well before the %v DELETE hang, took %v", hangDelay, elapsed)
	}
}

// TestLockExpiredUsesDeadline confirms the writer-recorded X-Amz-Meta-Deadline
// header is authoritative: a past deadline is reclaimable regardless of when
// the object was actually written, and a future deadline is not.
func TestLockExpiredUsesDeadline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		deadline time.Time
		name     string
		want     bool
	}{
		{name: "past deadline is expired", deadline: time.Now().Add(-time.Hour), want: true},
		{name: "future deadline is not expired", deadline: time.Now().Add(time.Hour), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := http.Header{}
			headers.Set("X-Amz-Meta-Deadline", tt.deadline.UTC().Format(time.RFC3339))

			if got := lockExpired(headers, time.Minute); got != tt.want {
				t.Fatalf("lockExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLockExpiredMalformedDeadlineFallsBackToAge confirms a deadline header
// that fails to parse does not hard-fail the check: it falls back to
// age-based staleness judged by Last-Modified against ttl, exactly as if no
// deadline had been recorded at all.
func TestLockExpiredMalformedDeadlineFallsBackToAge(t *testing.T) {
	t.Parallel()

	const ttl = time.Minute

	tests := []struct {
		lastModified time.Time
		name         string
		want         bool
	}{
		{name: "recent Last-Modified is not expired", lastModified: time.Now(), want: false},
		{name: "old Last-Modified is expired", lastModified: time.Now().Add(-2 * ttl), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := http.Header{}
			headers.Set("X-Amz-Meta-Deadline", "not-a-valid-timestamp")
			headers.Set("Last-Modified", tt.lastModified.UTC().Format(http.TimeFormat))

			if got := lockExpired(headers, ttl); got != tt.want {
				t.Fatalf("lockExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLockExpiredNoTimingIsReclaimable confirms that a lock object carrying
// neither a deadline nor a Last-Modified header is treated as reclaimable
// rather than as an unresolvable error: uninterpretable timing must never
// permanently block acquisition.
func TestLockExpiredNoTimingIsReclaimable(t *testing.T) {
	t.Parallel()

	if got := lockExpired(http.Header{}, time.Minute); !got {
		t.Fatalf("lockExpired() = %v, want true (reclaimable) for headers with no timing information", got)
	}
}
