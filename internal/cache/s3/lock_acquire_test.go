package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"sync/atomic"
	"testing"
	"time"
)

// freshCreateOrphanBudget bounds the acquisition of the row that must fail
// after its create-if-absent PUT landed. It has to clear the one loopback round
// trip ahead of that PUT - measured at 87-169us on this fixture, the
// measurement recorded on reclaimSwapHold - by enough that the row never fails
// before reaching the state it is about, and it is also the entire time the
// fixture then holds the ownership check open, so it is what that row waits.
// Well past the former, small enough to be a cheap test.
const freshCreateOrphanBudget = 300 * time.Millisecond

// TestFreshCreateAbandonsALockItNeverHeld is the fresh-create counterpart of
// TestReclaimAbandonsALockItNeverHeld: the same orphan, on the branch that
// creates the lock object rather than the one that takes it over. Once the
// create-if-absent PUT has landed and the acquisition then ends anyway, the
// object records this run's token with no heartbeat behind it - and every other
// acquirer waits out a full lock TTL against a holder that does not exist,
// which is twice the ceiling each of them waits before giving up.
//
// The two branches reach that state by different routes and share one cleanup,
// which is why this test exists separately rather than as a row of the reclaim
// one: the reclaim writes with If-Match and this one with If-None-Match, so no
// fixture drives both.
//
// The failure row builds the state exactly: the budget is wide enough for the
// create to land, and the fixture then holds the ownership check that follows
// it until that budget is gone, so the run always fails at the same point
// rather than racing a loopback round trip it would win.
//
// The second row is the positive control on the same fixture, with the block
// disarmed: the acquisition holds the lock and the object still records our
// token, so the failure row's "the object is gone" cannot be the fixture never
// letting an acquisition through in the first place.
//
// The failure row asserts the object is GONE rather than merely foreign, since
// foreign is what a competing acquirer would leave and this cleanup must never
// delete that.
//
// KILLING MUTATION, run and reverted: deleting the abandonLockObject call from
// tryAcquireOnce's claim-error arm. The failure row then leaves the orphan
// behind:
//
//	lock_acquire_test.go:69: lock object after an abandoned acquisition: headObject err = <nil>, want errS3NotFound
func TestFreshCreateAbandonsALockItNeverHeld(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		blockPostPutHead bool
		wantRelease      bool
	}{
		{name: "a budget that fits the create but not the ownership check", blockPostPutHead: true},
		{name: "the same fixture with nothing cutting the acquisition short", wantRelease: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertFreshCreateOrphanCleanup(t, tc.blockPostPutHead, tc.wantRelease)
		})
	}
}

// assertFreshCreateOrphanCleanup runs one row end to end against a real fake S3
// server, driving tryAcquireOnce directly rather than Lock: Lock would retry the
// whole acquisition loop after the failure this row induces, spending the
// budget on attempts that are not what is being measured.
func assertFreshCreateOrphanCleanup(t *testing.T, blockPostPutHead, wantRelease bool) {
	t.Helper()
	key := path.Join(locksPrefix, lockObject)
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key
	opCtx, cancel := context.WithTimeout(context.Background(), freshCreateOrphanBudget)
	t.Cleanup(cancel)

	var putsServed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The ownership check that follows the create PUT is held until the
		// caller's budget is gone, so the failure row fails on that budget every
		// time instead of racing a loopback round trip it would win.
		if blockPostPutHead && r.Method == http.MethodHead && r.URL.Path == lockPath && putsServed.Load() > 0 {
			<-opCtx.Done()
		}
		fake.ServeHTTP(w, r)
		if r.Method == http.MethodPut && r.URL.Path == lockPath {
			putsServed.Add(1)
		}
	}))
	t.Cleanup(srv.Close)

	b := newLockBackendAt(t, srv.URL, srv.Client(), testLockTiming(time.Minute))
	// Open runs before the budget matters and writes only its own probe key, so
	// the lock key is untouched when tryAcquireOnce reaches it: its
	// create-if-absent PUT is the first write this key ever sees, which is what
	// makes the anchor above count that PUT alone.
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	attempt, err := b.tryAcquireOnce(opCtx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	assertFreshCreateOutcome(t, wantRelease, attempt, err)
	assertFreshCreateObject(t, wantRelease, b, key)
}

// assertFreshCreateOutcome checks the attempt and the error one row expects.
func assertFreshCreateOutcome(t *testing.T, wantRelease bool, attempt lockAttempt, err error) {
	t.Helper()
	if got := attempt.release != nil; got != wantRelease {
		t.Fatalf("attempt holds the lock = %v, want %v (error: %v)", got, wantRelease, err)
	}
	// The first arm is documentary, not pinned: reaching it needs a release AND
	// an error, which lockAttempt's own contract forbids and no path produces.
	// The second is the real one - a failure row that quietly reported nothing
	// wrong is exactly what an abandoned lock would look like to a caller.
	switch {
	case wantRelease && err != nil:
		t.Fatalf("tryAcquireOnce: %v", err)
	case !wantRelease && err == nil:
		t.Fatalf("tryAcquireOnce reported no error for an acquisition cut short after its create PUT")
	}
}

// assertFreshCreateObject checks what the lock object holds once the attempt is
// over: nothing at all for a row whose acquisition was cut short, and this run's
// own token for the control.
func assertFreshCreateObject(t *testing.T, wantRelease bool, b *Backend, key string) {
	t.Helper()
	headers, err := b.client.headObject(context.Background(), key)
	if !wantRelease {
		if !errors.Is(err, errS3NotFound) {
			t.Fatalf("lock object after an abandoned acquisition: headObject err = %v, want errS3NotFound", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("headObject after the acquisition: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != ourToken {
		t.Fatalf("lock object token after the acquisition = %q, want %q", got, ourToken)
	}
}

// TestConditionalConflictIsRetriedNotFatal drives a 409 answering each of the
// two conditional writes the lock protocol makes. S3 documents the status for a
// concurrent request racing a conditional write - a delete completing first -
// and documents the write as safe to retry afterwards, so an acquisition that
// meets one must back off and try again rather than end.
//
// Both rows assert the acquisition SUCCEEDS, which alone would pass just as
// happily against a fixture that never served a 409 at all. The PUT count is
// what rules that out: it is asserted to have grown past the number a
// conflict-free acquisition makes, so the row fails if the conflict was never
// delivered or never retried. That count is also this test's positive control -
// the same fixture is shown taking the lock, so "it retried" cannot be "it
// failed differently".
//
// The two rows arm the conflict at different points because the two writes sit
// at different points: the create-if-absent PUT is the first request an
// acquisition makes against an empty bucket, while the swap only happens after
// a create has been refused and a HEAD has judged the object expired, so its
// row arms the conflict behind that HEAD.
//
// KILLING MUTATIONS, run and reverted. Each fails its own row, and the two
// report the identical message, so the row name is what tells them apart.
// Deleting tryAcquireOnce's errS3ConditionalConflict arm fails the create row:
//
//	lock_acquire_test.go:199: Lock: cache backend unavailable: s3 conditional write conflicted with a concurrent request
//
// Deleting reclaimIfExpired's own arm fails the reclaim row:
//
//	lock_acquire_test.go:199: Lock: cache backend unavailable: s3 conditional write conflicted with a concurrent request
func TestConditionalConflictIsRetriedNotFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		onReclaim bool
		minPuts   int
	}{
		{name: "a conflict answering the create is retried", minPuts: 2},
		{name: "a conflict answering the swap is retried", onReclaim: true, minPuts: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertConditionalConflictRetried(t, tc.onReclaim, tc.minPuts)
		})
	}
}

// assertConditionalConflictRetried arms a single 409 against whichever
// conditional write the row names, then requires the acquisition to complete
// anyway and to have issued more PUTs than an unobstructed one would.
func assertConditionalConflictRetried(t *testing.T, onReclaim bool, minPuts int) {
	t.Helper()
	ctx := context.Background()
	fake := newFakeS3()
	key := path.Join(locksPrefix, lockObject)
	lockPath := "/" + fake.bucket + "/" + key

	var heads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.ServeHTTP(w, r)
		// The swap is the first PUT after the reclaim's own HEAD, so arming the
		// conflict behind that HEAD lands it on the swap rather than on the
		// create-if-absent PUT that precedes both.
		if onReclaim && r.Method == http.MethodHead && r.URL.Path == lockPath && heads.Add(1) == 1 {
			fake.failNext(key, http.MethodPut, http.StatusConflict, 1)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	b := newLockBackendAt(t, srv.URL, srv.Client(), testLockTiming(time.Minute))
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if onReclaim {
		// An expired holder, so the acquisition reaches the swap at all.
		fake.storeLockObject(key, foreignToken, time.Now().UTC().Add(-time.Hour))
	} else {
		fake.failNext(key, http.MethodPut, http.StatusConflict, 1)
	}

	_, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })
	if got := fake.requestCount(key, http.MethodPut); got < minPuts {
		t.Fatalf("PUT count on the lock key = %d, want at least %d: the conflict was never served or never retried", got, minPuts)
	}
}
