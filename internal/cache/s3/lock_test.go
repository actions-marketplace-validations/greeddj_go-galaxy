package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// foreignToken stands in for a lock token belonging to some other acquirer
// across the tests that seed a lock object out-of-band to simulate
// contention, reclaim, or takeover scenarios.
const foreignToken = "foreign-token"

// testLockTiming returns a lockTiming with ttl set to the given value and
// every other interval shrunk to make the lock's state machine fast and
// deterministic in tests. Callers needing a non-default waitCeiling,
// heartbeatInterval, etc. can copy the returned value and override fields.
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
		SecretKey: "y",
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

// seedLockObject writes the lock object directly (bypassing acquireLock) to
// set up contention/reclaim scenarios ahead of a real Lock call.
func seedLockObject(ctx context.Context, t *testing.T, b *Backend, token string, deadline time.Time) {
	t.Helper()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	key := b.key(locksPrefix, lockObject)
	if err := b.putLock(ctx, key, token, deadline, false); err != nil {
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

	release, err := b.Lock(ctx)
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
			release, err := b.Lock(context.Background())
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
// (judged by Last-Modified staleness, since lockExpired is not rewritten in
// this commit) under a foreign token, then confirms a single Backend
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
	seedLockObject(ctx, t, b, foreignToken, pastDeadline)
	time.Sleep(2 * b.lock.ttl) // let the seeded lock's TTL elapse per Last-Modified

	release, err := b.Lock(ctx)
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

	seedLockObject(ctx, t, b, foreignToken, time.Now().UTC().Add(b.lock.ttl))

	start := time.Now()
	_, err := b.Lock(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, errS3LockWaitTimeout) {
		t.Fatalf("expected errS3LockWaitTimeout, got %v", err)
	}
	if elapsed < b.lock.backoffBase {
		t.Fatalf("expected at least one backoff sleep before timing out, elapsed %v", elapsed)
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

	seedLockObject(context.Background(), t, b, foreignToken, time.Now().UTC().Add(b.lock.ttl))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := b.Lock(ctx)
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
}

// TestHeartbeatRefreshesDeadline confirms the background heartbeat advances
// the lock object's deadline while the holder keeps it, without any
// explicit refresh call from the caller.
func TestHeartbeatRefreshesDeadline(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	firstHeaders, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	firstDeadline, err := time.Parse(time.RFC3339, firstHeaders.Get("X-Amz-Meta-Deadline"))
	if err != nil {
		t.Fatalf("parse first deadline: %v", err)
	}

	// Sleep well past several heartbeat ticks. 1.5s guarantees the real
	// wall-clock gap between the two deadline reads exceeds a full second,
	// so their RFC3339 (second-resolution) values are certain to differ.
	time.Sleep(1500 * time.Millisecond)

	secondHeaders, err := b.client.headObject(ctx, key)
	if err != nil {
		t.Fatalf("headObject: %v", err)
	}
	secondDeadline, err := time.Parse(time.RFC3339, secondHeaders.Get("X-Amz-Meta-Deadline"))
	if err != nil {
		t.Fatalf("parse second deadline: %v", err)
	}
	if !secondDeadline.After(firstDeadline) {
		t.Fatalf("expected the heartbeat to advance the deadline, first=%v second=%v", firstDeadline, secondDeadline)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestHeartbeatDetectsLostOwnershipAndReleaseSkipsDelete overwrites the lock
// object out-of-band with a foreign token while a Backend holds it, and
// confirms the heartbeat detects the takeover: release reports
// errS3LockLost and, critically, never deletes the foreign holder's object.
func TestHeartbeatDetectsLostOwnershipAndReleaseSkipsDelete(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.lock = testLockTiming(10 * time.Second)
	ctx := context.Background()

	release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	// Simulate a foreign acquirer reclaiming the lock out-of-band (e.g.
	// after this holder stalled past its TTL from S3's point of view).
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(b.lock.ttl), false); err != nil {
		t.Fatalf("seed foreign takeover: %v", err)
	}

	time.Sleep(3 * b.lock.heartbeatInterval)

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

// TestHeartbeatSurvivesTransientHeadFailures forces the heartbeat's HEAD
// (verifyOwner) calls to fail a bounded number of times with a transient
// server error, then recover, while the holder keeps the lock. It confirms
// heartbeatTick's error branch never flips the lost flag on its own: only a
// definitive token mismatch may do that. Release afterward must succeed
// (not errS3LockLost) and must actually delete the lock object, proving the
// heartbeat kept refreshing normally once the forced failures were spent.
func TestHeartbeatSurvivesTransientHeadFailures(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	timing := testLockTiming(10 * time.Second)
	timing.heartbeatInterval = 20 * time.Millisecond
	b.lock = timing
	ctx := context.Background()

	release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	key := b.key(locksPrefix, lockObject)
	// Fail the next two HEAD requests on the lock key (two heartbeat
	// ticks' worth), then let the fake behave normally again.
	fake.failNext(key, http.MethodHead, http.StatusInternalServerError, 2)

	// Give the heartbeat enough ticks to hit both forced failures and then
	// tick again successfully at least once before we release.
	time.Sleep(6 * b.lock.heartbeatInterval)

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
// retryNow=false, nil error) rather than as an error or a retryNow signal.
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
	if err := b.putLock(ctx, key, foreignToken, pastDeadline, false); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}

	// The next PUT to this key (the create-if-absent recreate that follows
	// our delete) reports precondition-failed, simulating another
	// acquirer's create winning the race for the just-deleted key.
	fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, 1)

	release, retryNow, err := b.reclaimIfExpired(ctx, key, "our-token")
	if err != nil {
		t.Fatalf("reclaimIfExpired: %v", err)
	}
	if release != nil {
		t.Fatalf("expected no release function for a lost recreate race")
	}
	if retryNow {
		t.Fatalf("expected retryNow=false for a lost recreate race (back off, don't retry immediately)")
	}
}

// TestReclaimIfExpiredRetriesImmediatelyWhenObjectVanished directly
// unit-tests reclaimIfExpired's vanished-object branch: HEAD on a key that
// was never written returns not-found, which must be reported as an
// immediate-retry signal (retryNow=true) rather than an error, since
// acquireLock's caller loop uses it to retry the create without waiting
// out a backoff interval.
func TestReclaimIfExpiredRetriesImmediatelyWhenObjectVanished(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := b.key(locksPrefix, "never-written-lock")
	release, retryNow, err := b.reclaimIfExpired(ctx, key, "token")
	if err != nil {
		t.Fatalf("reclaimIfExpired: %v", err)
	}
	if release != nil {
		t.Fatalf("expected no release function when the object has vanished")
	}
	if !retryNow {
		t.Fatalf("expected retryNow=true when the object has vanished")
	}
}

// TestLockAcquireBoundsImmediateRetrySpin forces the pathological PUT/HEAD
// inconsistency that acquireLock's retryNow handoff exists to survive: a
// misbehaving S3-compatible backend that answers the create-if-absent PUT
// with 412 (precondition failed) while a follow-up HEAD on the very same key
// keeps reporting the object as missing. Before maxImmediateLockRetries
// bounded this, acquireLock would spin PUT+HEAD with no backoff sleep at all
// for the entire waitCeiling window; this test shrinks waitCeiling so the
// spin (if unbounded) would produce a very large number of requests in a
// short, deterministic window, and confirms both that Lock still terminates
// with errS3LockWaitTimeout (not a raw transport/context error leaking out)
// and that the number of PUT attempts against the lock key stays in the tens
// rather than growing to the thousands an unbounded spin would produce over
// the same window.
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
	_, err := b.Lock(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, errS3LockWaitTimeout) {
		t.Fatalf("expected errS3LockWaitTimeout, got %v", err)
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
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(time.Hour), false); err != nil {
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

	release, err := b.Lock(ctx)
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
	release, err := b.Lock(ctx)
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

	release, err := b.Lock(context.Background())
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
