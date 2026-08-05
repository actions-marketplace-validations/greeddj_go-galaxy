package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// ourToken is the token the tests below hand to reclaimIfExpired when
	// they drive it directly instead of going through Lock, standing in for
	// the one acquireLockLoop generates per acquisition.
	ourToken = "our-reclaimer-token"
	// competingToken stands in for a second acquirer that writes the same
	// lock object while this one is reclaiming it - the shape both
	// recheckBeforeDelete and claimReclaimed exist to notice.
	competingToken = "competing-reclaimer-token"
)

// TestReclaimDoesNotDeleteAFreshlyCreatedLock covers recheckBeforeDelete's
// answers against one fixture: a handler that, immediately after serving the
// first HEAD for the lock key, either replaces the stored object with one
// another acquirer might have written a moment later or arms a one-shot
// failure for the recheck HEAD itself. The seeded object is expired, so
// reclaimIfExpired's own HEAD decides "reclaim this" - and by the time its
// delete would be issued, the object that decision was taken about is gone,
// or the recheck that would confirm it never answers.
//
// The swap is installed inside the handler, before it returns, which is what
// makes the ordering exact rather than probable: the fake writes a HEAD
// response through net/http's own buffered writer with no flush of its own,
// so the bytes reach the client only once the handler returns, strictly after
// the swap. A failure armed at that same point is one-shot, so it is spent on
// the recheck HEAD and this test's own closing HEAD still reads the object.
//
// The rows are five reachable states, not five assertions about one:
//
//   - a live foreign lock: the delete must not be issued at all, and a live
//     holder is the ordinary contention signal, so observed is true.
//   - an expired foreign lock under a different token: still not ours to
//     delete, since the object is no longer the one the expiry decision was
//     taken about, but its writer holds nothing - so observed stays false.
//     This row is what pins recheckBeforeDelete reporting live rather than an
//     unconditional true.
//   - a recheck answered 404: the object vanished between the two HEADs, so
//     the create is worth attempting again at once. Nothing is deleted,
//     nothing is observed, and retryNow alone is set.
//   - a recheck answered 403: the recheck itself failed, so the attempt ends
//     rather than guessing at what the object holds. The status is chosen for
//     what it costs, not for its meaning: helpers.IsRetryableHTTPStatus
//     retries only 429/500/502/503/504, so 403 skips the client's retry
//     ladder that a 5xx would drag through this row's runtime. The error's
//     CLASS is asserted rather than its non-nilness - a status the remote
//     itself answered carries helpers.ErrCacheBackendUnavailable, per the
//     partition doc in variables.go - and so is the delete count, since "a
//     failed recheck deletes nothing" is what this row is worth.
//   - nothing racing in at all: the positive control, and the table's only
//     one, so nobody need add a second. The same fixture, with neither a swap
//     nor a failure armed, must still reclaim - delete issued, release
//     returned - or "it refused" here would be indistinguishable from "it
//     never got that far".
//
// KILLING MUTATIONS, run and reverted. Deleting reclaimIfExpired's call to
// recheckBeforeDelete, and the staleToken line that feeds it, fails all four
// rows that arm anything. Three fail on the delete this test exists to
// prevent, the two swapped rows reporting
//
//	lock_reclaim_test.go:108: DELETE count on the lock key = 1, want 0
//
// and the 403 row reporting 2 rather than 1, since the claim that then fails
// abandons the object it had just created. The 404 row fails a step earlier,
// on the vanished object no recheck is left to absorb:
//
//	lock_reclaim_test.go:108: reclaimIfExpired: s3 object not found
//
// Returning an unconditional observed: true from recheckBeforeDelete's
// mismatch arm, where it reports live, fails the expired row alone:
//
//	lock_reclaim_test.go:108: attempt.observed = true, want false
//
// Returning observed: true in place of retryNow: true from its vanished-object
// arm fails the 404 row alone:
//
//	lock_reclaim_test.go:108: attempt.retryNow = false, want true
//
// Swallowing the recheck's own error - reporting proceed instead of the
// failure - fails the 403 row alone:
//
//	lock_reclaim_test.go:108: reclaimIfExpired error = <nil>, want one matching cache backend unavailable
//
// All four are reported against the assertReclaimRecheck call below rather
// than inside that helper, or the helpers it calls, all of which call
// t.Helper().
func TestReclaimDoesNotDeleteAFreshlyCreatedLock(t *testing.T) {
	t.Parallel()

	for _, tc := range reclaimRecheckCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertReclaimRecheck(t, tc)
		})
	}
}

// reclaimRecheckCase is one row of TestReclaimDoesNotDeleteAFreshlyCreatedLock:
// swapToken and swapAhead describe the object installed behind
// reclaimIfExpired's own HEAD - an empty swapToken arms no swap at all, and a
// negative swapAhead makes the swapped-in object expired - failRecheckStatus
// arms a one-shot status for the recheck HEAD instead, and the want fields
// describe the lockAttempt, the error, and the stored object that must follow.
type reclaimRecheckCase struct {
	name              string
	wantErr           error
	swapToken         string
	wantToken         string
	swapAhead         time.Duration
	failRecheckStatus int
	wantDeletes       int
	wantObserved,
	wantRetryNow,
	wantRelease bool
}

// reclaimRecheckCases returns the five states reachable between
// reclaimIfExpired's expiry decision and the delete it would otherwise issue.
func reclaimRecheckCases() []reclaimRecheckCase {
	return []reclaimRecheckCase{
		{
			name:         "a live lock written after the expiry decision is left alone",
			swapToken:    competingToken,
			swapAhead:    time.Hour,
			wantToken:    competingToken,
			wantDeletes:  0,
			wantObserved: true,
		},
		{
			name:        "an expired lock under another token is left alone and is not an observation",
			swapToken:   competingToken,
			swapAhead:   -time.Hour,
			wantToken:   competingToken,
			wantDeletes: 0,
		},
		{
			name:              "a lock that vanished before the recheck is retried immediately",
			wantToken:         foreignToken,
			failRecheckStatus: http.StatusNotFound,
			wantDeletes:       0,
			wantRetryNow:      true,
		},
		{
			name:              "a recheck the backend refuses ends the attempt and deletes nothing",
			wantToken:         foreignToken,
			wantErr:           helpers.ErrCacheBackendUnavailable,
			failRecheckStatus: http.StatusForbidden,
			wantDeletes:       0,
		},
		{
			name:        "the same fixture with nothing racing in reclaims",
			wantToken:   ourToken,
			wantDeletes: 1,
			wantRelease: true,
		},
	}
}

// assertReclaimRecheck runs one reclaimRecheckCase end to end against a real
// fake S3 server.
func assertReclaimRecheck(t *testing.T, tc reclaimRecheckCase) {
	t.Helper()
	ctx := context.Background()
	key := path.Join(locksPrefix, lockObject)
	b, fake := newReclaimRecheckFixture(t, tc, key)

	// Open's conditional-PUT probe writes under the locks prefix too, so the
	// baseline is asserted rather than assumed: a probe that ever started
	// HEADing this key would make the swap fire behind the wrong request and
	// quietly turn every row below into a different test.
	if got := fake.requestCount(key, http.MethodHead); got != 0 {
		t.Fatalf("HEAD count on the lock key before the reclaim = %d, want 0", got)
	}

	attempt, err := b.reclaimIfExpired(ctx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	assertReclaimError(t, tc, err)
	// The delete count comes next because it is the outcome this test is
	// named for, and because it is the one every row can fail on: the two
	// swapped rows would each fail it for the same reason, while the checks
	// below separate them from one another.
	if got := fake.requestCount(key, http.MethodDelete); got != tc.wantDeletes {
		t.Fatalf("DELETE count on the lock key = %d, want %d", got, tc.wantDeletes)
	}
	assertReclaimOutcome(t, tc, attempt)

	headers, headErr := b.client.headObject(ctx, key)
	if headErr != nil {
		t.Fatalf("headObject after the reclaim: %v", headErr)
	}
	// Documentary, not pinned: every reachable state that leaves a different
	// token on the object fails the delete count or the release check above
	// first, so nothing can reach this line and fail only it. It is kept
	// because it names what the rows are ultimately about - which acquirer's
	// write is standing on the lock object when the attempt is over.
	if got := headers.Get("X-Amz-Meta-Token"); got != tc.wantToken {
		t.Fatalf("lock object token after the reclaim = %q, want %q", got, tc.wantToken)
	}
}

// assertReclaimError checks the error one reclaimRecheckCase expects. A row
// naming no error requires a nil one; a row naming one requires that CLASS
// rather than merely something non-nil, since what the 403 row is worth is
// which of the cache-backend classes a status the remote itself answered
// lands in (see variables.go's partition doc).
func assertReclaimError(t *testing.T, tc reclaimRecheckCase, err error) {
	t.Helper()
	if tc.wantErr == nil {
		if err != nil {
			t.Fatalf("reclaimIfExpired: %v", err)
		}
		return
	}
	if !errors.Is(err, tc.wantErr) {
		t.Fatalf("reclaimIfExpired error = %v, want one matching %v", err, tc.wantErr)
	}
}

// newReclaimRecheckFixture builds the fake S3 server one reclaimRecheckCase
// runs against: its handler installs tc's swapped-in object immediately after
// the first HEAD for the lock key has been served, and the lock object is
// seeded as an expired foreign holder so reclaimIfExpired's own HEAD decides
// to reclaim it. It returns the Backend pointed at that server and the fake
// behind it.
func newReclaimRecheckFixture(t *testing.T, tc reclaimRecheckCase, key string) (*Backend, *fakeS3) {
	t.Helper()
	ctx := context.Background()
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key

	var heads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.ServeHTTP(w, r)
		if r.Method != http.MethodHead || r.URL.Path != lockPath || heads.Add(1) != 1 {
			return
		}
		if tc.swapToken != "" {
			fake.storeLockObject(key, tc.swapToken, time.Now().UTC().Add(tc.swapAhead))
		}
		if tc.failRecheckStatus != 0 {
			// One use only, so it is spent on the recheck HEAD - the next
			// request for this key - and every later HEAD, this test's own
			// included, reads the stored object normally.
			fake.failNext(key, http.MethodHead, tc.failRecheckStatus, 1)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	b := newLockBackendAt(t, srv.URL, srv.Client(), testLockTiming(time.Minute))
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(-time.Hour), false); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}
	return b, fake
}

// assertReclaimOutcome checks the lockAttempt one reclaimRecheckCase expects.
// It is split out only to keep assertReclaimRecheck's own fixture setup
// readable; all three checks here are on the value reclaimIfExpired returned.
//
// retryNow is checked before observed because the two fields are how the
// vanished-object answer differs from the contention one, and a row expecting
// retryNow reports the missing field rather than the spurious one it was
// swapped for.
func assertReclaimOutcome(t *testing.T, tc reclaimRecheckCase, attempt lockAttempt) {
	t.Helper()
	if attempt.retryNow != tc.wantRetryNow {
		t.Fatalf("attempt.retryNow = %v, want %v", attempt.retryNow, tc.wantRetryNow)
	}
	if attempt.observed != tc.wantObserved {
		t.Fatalf("attempt.observed = %v, want %v", attempt.observed, tc.wantObserved)
	}
	if got := attempt.release != nil; got != tc.wantRelease {
		t.Fatalf("attempt holds the lock = %v, want %v", got, tc.wantRelease)
	}
}

const (
	// reclaimTheftSettle is the settle interval the settled row below runs
	// with.
	reclaimTheftSettle = 500 * time.Millisecond
	// reclaimTheftDelay is how long after the reclaim's PUT has been SERVED
	// the theft is written. The anchor is an event - the fake finishing that
	// PUT - rather than a guess at when the reclaim gets there; the offset
	// from that anchor is what separates the two verifications, and it is
	// needed because the settle's whole observable effect is wall time in
	// which nothing else happens, leaving no event between the two to trigger
	// on. Measured on this fixture, an unsettled verification reaches the
	// fake 87-169us after the PUT was served (eight runs), so this offset is
	// ~300x later than the verification it must not disturb and 10x earlier
	// than the one it must be seen by.
	reclaimTheftDelay = 50 * time.Millisecond
)

// TestReclaimSettleRefusesAStolenClaim drives the one thing the settle
// changes: a second acquirer's write landing after this run's reclaim PUT and
// before its ownership check. The theft is written 50ms after the fake has
// finished serving that PUT, and the row differs only in whether a settle
// stands between the PUT and the check.
//
// It is an implication test in both directions, not a timing assumption. With
// the settle at 500ms the theft is 10x earlier than the verification, so the
// verification sees it and the claim is refused. With the settle disabled the
// verification is one loopback round trip after the PUT - measured at 87-169us
// on this fixture - so it is roughly 300x earlier than the theft, and the same
// run claims successfully. The disabled row is also this test's positive
// control: without it, "the claim was refused" would not distinguish the
// settle working from the fixture never letting any claim through.
//
// A settleReclaim mutated into a no-op needs no separate paragraph here: it
// is exactly the disabled row, which already asserts the opposite outcome.
func TestReclaimSettleRefusesAStolenClaim(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		settle      time.Duration
		wantRelease bool
	}{
		{name: "a settled verification sees the theft", settle: reclaimTheftSettle},
		{name: "the same theft is missed with no settle", settle: 0, wantRelease: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertStolenClaim(t, tc.settle, tc.wantRelease)
		})
	}
}

// assertStolenClaim reclaims an expired lock with the given settle while a
// competing write lands reclaimTheftDelay after the reclaim's PUT was served,
// and checks whether the resulting attempt holds the lock.
func assertStolenClaim(t *testing.T, settle time.Duration, wantRelease bool) {
	t.Helper()
	ctx := context.Background()
	fake := newFakeS3()
	key := path.Join(locksPrefix, lockObject)
	lockPath := "/" + fake.bucket + "/" + key

	// Counted after the fake has served the request, so the count moving
	// means the object is already stored: counting before it - which is what
	// the fake's own requestCount does - would leave this test's write racing
	// the very PUT that triggered it, and losing that race would silently
	// restore our own token.
	var putsServed atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.ServeHTTP(w, r)
		if r.Method == http.MethodPut && r.URL.Path == lockPath {
			putsServed.Add(1)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	timing := testLockTiming(time.Minute)
	timing.reclaimSettle = settle
	b := newLockBackendAt(t, srv.URL, srv.Client(), timing)
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(-time.Hour), false); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}
	served := putsServed.Load()

	holderCancel := testHolderCancel(t)
	done := make(chan lockAttempt, 1)
	errCh := make(chan error, 1)
	go func() {
		attempt, err := b.reclaimIfExpired(ctx, key, ourToken, holderCancel)
		errCh <- err
		done <- attempt
	}()

	waitForLockEvent(t, "the reclaim's own create-if-absent PUT", func() bool {
		return putsServed.Load() > served
	})
	time.Sleep(reclaimTheftDelay)
	fake.storeLockObject(key, competingToken, time.Now().UTC().Add(time.Hour))

	if err := <-errCh; err != nil {
		t.Fatalf("reclaimIfExpired: %v", err)
	}
	attempt := <-done
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	if got := attempt.release != nil; got != wantRelease {
		t.Fatalf("attempt holds the lock = %v, want %v", got, wantRelease)
	}
	// Documentary, not pinned: claim reports observed exactly when it returns
	// no release, so no reachable state fails this line while passing the one
	// above. It is kept because it names what a refusal has to be worth - an
	// observation of another acquirer, which is what classifies the whole wait
	// as contention instead of as a backend that never answered.
	if got := attempt.observed; got == wantRelease {
		t.Fatalf("attempt.observed = %v alongside holding the lock = %v; a claim is one or the other", got, wantRelease)
	}
}

// TestReclaimSettleIsChargedAgainstTheWaitCeiling pins settleReclaim's
// fit-or-skip rule: the wait ceiling is 300ms and the settle 5s, so the
// settle does not fit, and the acquisition finishes in milliseconds instead
// of waiting for either.
//
// The nil error is the assertion that separates skipping from truncating.
// Truncating the settle to whatever the ceiling has left would end the wait
// with waitCtx already expired, and the ownership check that follows the
// reclaim PUT would then fail against a dead context - reporting the ceiling
// while an object carrying this run's token sits in the bucket with no
// heartbeat behind it, blocking every other acquirer for a full TTL.
// TestReclaimSettleIsPaidWhenItFits is the other half of this pair: without
// it, a settleReclaim that returned immediately every time would pass here.
//
// KILLING MUTATION, run and reverted: deleting settleReclaim's deadline
// check, so the settle is truncated to whatever the ceiling has left instead
// of skipped. The acquisition then spends the whole ceiling inside the settle
// and fails on the ownership check that follows it, against a context that
// has already expired (one line, wrapped here; the port is the fake's own):
//
//	lock_reclaim_test.go:461: Lock: cache backend unavailable: s3 lock wait ceiling
//	elapsed without ever observing a lock holder: Head "http://127.0.0.1:62936/test/locks/cache.lock":
//	context deadline exceeded
func TestReclaimSettleIsChargedAgainstTheWaitCeiling(t *testing.T) {
	t.Parallel()

	timing := testLockTiming(time.Minute)
	timing.waitCeiling = 300 * time.Millisecond
	timing.reclaimSettle = 5 * time.Second
	b := newTestBackend(t)
	b.lock = timing
	ctx := context.Background()

	seedLockObject(ctx, t, b, time.Now().UTC().Add(-time.Hour))

	start := time.Now()
	_, release, err := b.Lock(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })
	// A liveness bound, not a measurement: anything at all close to the
	// settle means it was paid, in full or truncated.
	if bound := timing.reclaimSettle / 2; elapsed > bound {
		t.Fatalf("Lock took %v with a settle that cannot fit the wait ceiling, want under %v", elapsed, bound)
	}
}

// TestReclaimSettleIsPaidWhenItFits is the other half of the pair above: with
// a wait ceiling well clear of the settle, a reclaiming acquisition waits the
// whole settle before it reports holding the lock. Without this, the
// fit-or-skip test would pass just as happily against a settle that never ran.
func TestReclaimSettleIsPaidWhenItFits(t *testing.T) {
	t.Parallel()

	timing := testLockTiming(time.Minute)
	timing.waitCeiling = 10 * time.Second
	timing.reclaimSettle = 400 * time.Millisecond
	b := newTestBackend(t)
	b.lock = timing
	ctx := context.Background()

	seedLockObject(ctx, t, b, time.Now().UTC().Add(-time.Hour))

	start := time.Now()
	_, release, err := b.Lock(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })
	if elapsed < timing.reclaimSettle {
		t.Fatalf("reclaiming acquisition took %v, want at least the settle interval %v", elapsed, timing.reclaimSettle)
	}
}

const (
	// reclaimOrphanSettle is the settle the orphan-cleanup rows below run
	// with. It is long enough that a cancellation aimed at its middle lands
	// there with room on both sides, and it is what the deadline row's own
	// budget is built from.
	reclaimOrphanSettle = 400 * time.Millisecond
	// reclaimOrphanMargin is how far the deadline row's budget exceeds the
	// settle: wide enough for settleReclaim's fit check to pass, which needs
	// the four loopback round trips ahead of the settle to complete inside it,
	// and it is also everything the ownership check afterwards is left with -
	// a check this row holds open until the budget is gone, so what it would
	// otherwise have cost never decides anything. That the fit check passes is
	// a property of these two constants rather than something the row's own
	// assertions show: the held HEAD carries the run to its budget whether the
	// settle was paid or skipped.
	reclaimOrphanMargin = 150 * time.Millisecond
	// reclaimOrphanCancelDelay is how long after the reclaim's PUT has been
	// SERVED the cancellation row cancels. The anchor is the same event
	// reclaimTheftDelay uses, and so is the reasoning: the fake finishing that
	// PUT is an observable event, and an unsettled follow-up request reaches
	// the fake 87-169us after it (the measurement recorded on reclaimTheftDelay
	// above), so this offset is far past the PUT completing and far short of
	// the settle ending.
	//
	// The cancellation is deliberately NOT issued from inside the handler the
	// way the swap in the recheck fixture is. The response bytes leave the
	// handler only once it returns, so canceling there would race the very PUT
	// this row needs to have landed, and the row would sometimes exercise the
	// failed-PUT arm instead of the settle it is named for.
	reclaimOrphanCancelDelay = 50 * time.Millisecond
)

// TestReclaimAbandonsALockItNeverHeld drives what happens after a reclaim's
// own create-if-absent PUT has landed and the acquisition then ends anyway.
// The object at that moment records this run's token with no heartbeat behind
// it: left there, it blocks every other acquirer for a full lock TTL, which is
// twice the ceiling each of them waits before giving up.
//
// The two failure rows drive two bands of that state rather than one shape
// twice; they are not an enumeration of the ways to reach it:
//
//   - a cancellation inside the settle: the caller stops while settleReclaim
//     is waiting, so the ownership check that follows runs against a dead
//     context. Its elapsed time stays under the settle, which is what
//     distinguishes it from the row below.
//   - a budget that fits the settle but not the check: the budget is the settle
//     plus a margin the ownership check is never allowed to use, since the
//     post-PUT HEAD is held until that budget is gone. That places the row in
//     the band where settleReclaim's fit rule passes, the settle is paid in
//     full, and the check afterwards fails on what is left of the budget. The
//     elapsed assertion is what makes this row that band and not the one above:
//     the fit rule guarantees the settle completes, never that the check after
//     it does.
//
// The third row is the positive control on the same fixture, with neither the
// cancel nor the block armed: the reclaim holds the lock and the object still
// records our token, so a failure row's "the object is gone" cannot be the
// fixture never letting a reclaim through in the first place.
//
// Both failure rows assert the object is GONE rather than merely foreign,
// since foreign is what a competing reclaimer would leave and this cleanup
// must never delete that.
//
// KILLING MUTATION, run and reverted: deleting the abandonReclaimedLock call
// from reclaimIfExpired's claim-error arm. Both failure rows then leave the
// orphan behind and fail identically:
//
//	lock_reclaim_test.go:573: lock object after an abandoned reclaim: headObject err = <nil>, want errS3NotFound
func TestReclaimAbandonsALockItNeverHeld(t *testing.T) {
	t.Parallel()

	for _, tc := range reclaimOrphanCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertReclaimOrphanCleanup(t, tc)
		})
	}
}

// reclaimOrphanCase is one row of TestReclaimAbandonsALockItNeverHeld: budget
// bounds the acquisition context (zero leaves it unbounded), cancelOnPut and
// blockPostPutHead select which failure the row drives, and the want fields
// describe the attempt and the timing that must follow.
type reclaimOrphanCase struct {
	name             string
	budget           time.Duration
	cancelOnPut      bool
	blockPostPutHead bool
	wantSettlePaid   bool
	wantRelease      bool
}

// reclaimOrphanCases returns the two bands these rows drive after a reclaim PUT
// has landed, plus the positive control the two are read against.
func reclaimOrphanCases() []reclaimOrphanCase {
	return []reclaimOrphanCase{
		{
			name:        "a cancellation inside the settle",
			cancelOnPut: true,
		},
		{
			name:             "a budget that fits the settle but not the check",
			budget:           reclaimOrphanSettle + reclaimOrphanMargin,
			blockPostPutHead: true,
			wantSettlePaid:   true,
		},
		{
			name:           "the same fixture with nothing cutting the acquisition short",
			wantSettlePaid: true,
			wantRelease:    true,
		},
	}
}

// assertReclaimOrphanCleanup runs one reclaimOrphanCase end to end against a
// real fake S3 server, driving reclaimIfExpired directly: the create-PUT Lock
// issues first would poison the putsServed anchor and spend the row's budget.
func assertReclaimOrphanCleanup(t *testing.T, tc reclaimOrphanCase) {
	t.Helper()
	key := path.Join(locksPrefix, lockObject)
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key
	opCtx, cancel := reclaimOrphanContext(tc)
	t.Cleanup(cancel)

	var putsServed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The ownership check that follows the reclaim PUT is held until the
		// caller's budget is gone, so the deadline row fails on that budget
		// every time instead of racing a loopback round trip it would win.
		if tc.blockPostPutHead && r.Method == http.MethodHead && r.URL.Path == lockPath && putsServed.Load() > 0 {
			<-opCtx.Done()
		}
		fake.ServeHTTP(w, r)
		if r.Method == http.MethodPut && r.URL.Path == lockPath {
			putsServed.Add(1)
		}
	}))
	t.Cleanup(srv.Close)

	timing := testLockTiming(time.Minute)
	timing.reclaimSettle = reclaimOrphanSettle
	b := newLockBackendAt(t, srv.URL, srv.Client(), timing)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Seeded straight into the fake rather than over HTTP, so the reclaim's
	// own create-if-absent PUT is the first one this key ever sees and the
	// cancel below cannot be triggered by the setup.
	fake.storeLockObject(key, foreignToken, time.Now().UTC().Add(-time.Hour))
	if tc.cancelOnPut {
		go cancelAfterLockPut(&putsServed, cancel)
	}

	start := time.Now()
	attempt, err := b.reclaimIfExpired(opCtx, key, ourToken, testHolderCancel(t))
	elapsed := time.Since(start)
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	assertReclaimOrphanOutcome(t, tc, attempt, err, elapsed)
	assertReclaimOrphanObject(t, tc, b, key)
}

// reclaimOrphanContext builds the acquisition context one reclaimOrphanCase
// runs under: a wall-clock budget when the row names one, an ordinary
// cancelable context otherwise.
func reclaimOrphanContext(tc reclaimOrphanCase) (context.Context, context.CancelFunc) {
	if tc.budget > 0 {
		return context.WithTimeout(context.Background(), tc.budget)
	}
	return context.WithCancel(context.Background())
}

// cancelAfterLockPut cancels the acquisition reclaimOrphanCancelDelay after
// the reclaim's PUT has been served, i.e. inside the settle that follows it.
// It polls rather than blocking on a channel so that a run where that PUT
// never happens still ends: the cancel fires on the ceiling instead, and the
// row fails on its assertions rather than by hanging. Nothing here touches
// *testing.T, since it runs on its own goroutine.
func cancelAfterLockPut(putsServed *atomic.Int32, cancel context.CancelFunc) {
	deadline := time.Now().Add(lockEventWaitCeiling)
	for putsServed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(lockEventPollInterval)
	}
	time.Sleep(reclaimOrphanCancelDelay)
	cancel()
}

// assertReclaimOrphanOutcome checks the attempt, the error, and the elapsed
// time one reclaimOrphanCase expects. The elapsed check is two-sided on
// purpose: a row that must have paid the settle and one that must have been
// cut short inside it are otherwise indistinguishable from their return
// values alone.
func assertReclaimOrphanOutcome(
	t *testing.T, tc reclaimOrphanCase, attempt lockAttempt, err error, elapsed time.Duration,
) {
	t.Helper()
	if got := attempt.release != nil; got != tc.wantRelease {
		t.Fatalf("attempt holds the lock = %v, want %v (error: %v)", got, tc.wantRelease, err)
	}
	// The first arm is documentary, not pinned: reaching it needs a release
	// AND an error, which lockAttempt's own contract forbids and no path
	// produces. The second is the real one - a failure row that quietly
	// reported nothing wrong is exactly what an abandoned lock would look like
	// to a caller.
	switch {
	case tc.wantRelease && err != nil:
		t.Fatalf("reclaimIfExpired: %v", err)
	case !tc.wantRelease && err == nil:
		t.Fatalf("reclaimIfExpired reported no error for an acquisition cut short after its reclaim PUT")
	}
	if tc.wantSettlePaid && elapsed < reclaimOrphanSettle {
		t.Fatalf("the reclaim ended after %v, want at least the settle %v", elapsed, reclaimOrphanSettle)
	}
	if !tc.wantSettlePaid && elapsed >= reclaimOrphanSettle {
		t.Fatalf("the reclaim ended after %v, want less than the settle %v", elapsed, reclaimOrphanSettle)
	}
}

// assertReclaimOrphanObject checks what the lock object holds once the attempt
// is over: nothing at all for a row whose reclaim was cut short, and this
// run's own token for the control.
func assertReclaimOrphanObject(t *testing.T, tc reclaimOrphanCase, b *Backend, key string) {
	t.Helper()
	headers, err := b.client.headObject(context.Background(), key)
	if !tc.wantRelease {
		if !errors.Is(err, errS3NotFound) {
			t.Fatalf("lock object after an abandoned reclaim: headObject err = %v, want errS3NotFound", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("headObject after the reclaim: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != ourToken {
		t.Fatalf("lock object token after the reclaim = %q, want %q", got, ourToken)
	}
}

// TestFreshCreatePathPaysNoSettle pins the settle to the reclaim path alone.
// A create-if-absent PUT that succeeded against an empty bucket proves the
// object was absent when it landed - a fact Open's own conditional-PUT probe
// has already established the backend enforces - so there is no second writer
// to wait for and nothing a settle here would buy.
//
// The wait ceiling is deliberately far above the settle: with the ceiling
// below it, settleReclaim's own fit-or-skip rule would skip the wait anyway
// and this test would pass without ever depending on where the settle is
// applied.
func TestFreshCreatePathPaysNoSettle(t *testing.T) {
	t.Parallel()

	timing := testLockTiming(time.Minute)
	timing.waitCeiling = 10 * time.Second
	timing.reclaimSettle = 3 * time.Second
	b := newTestBackend(t)
	b.lock = timing

	start := time.Now()
	_, release, err := b.Lock(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })
	if bound := timing.reclaimSettle / 2; elapsed > bound {
		t.Fatalf("fresh acquisition took %v, want under %v: the settle belongs to the reclaim path", elapsed, bound)
	}
}

// TestTwoReclaimersEndWithOneHolder is the whole defect in one test: two
// acquirers reading the same expired lock object, each passing its own expiry
// check, each deleting unconditionally and recreating under its own token.
//
// The interleaving is forced, not sampled. The first DELETE to reach the lock
// key is held for a tenth of the settle before it is served, so the second
// acquirer's delete-and-recreate completes first and the first acquirer's
// delete then removes the object the second just wrote. That is the ordering
// in which both used to end up holding: each verified a token it had itself
// just written, in a window neither could see. The hold is a fixed duration
// rather than anything derived from the other acquirer's progress, so no
// assertion here depends on how fast either one runs.
//
// Either acquirer may win. At delays this far below a round trip both
// orderings are legitimate, so the test asserts the count of holders and the
// loser's error class, never which Backend ends up with the lock.
//
// KILLING MUTATION, run and reverted: deleting claimReclaimed's call to
// settleReclaim, which is the defect this test describes. Five runs, five
// failures, all identical:
//
//	lock_reclaim_test.go:821: 2 of 2 reclaimers ended up holding the lock, want exactly 1 (errors: [<nil> <nil>])
//
// The two nil errors are the sharp end of it: both acquisitions reported
// success, so neither run learns anything is wrong until a heartbeat tick -
// and a run shorter than one interval never learns at all.
func TestTwoReclaimersEndWithOneHolder(t *testing.T) {
	t.Parallel()

	key := path.Join(locksPrefix, lockObject)
	timing := testLockTiming(time.Minute)
	timing.reclaimSettle = 100 * time.Millisecond
	timing.waitCeiling = 600 * time.Millisecond

	srv := newHeldFirstDeleteServer(t, key, timing.reclaimSettle/10)
	b1 := newLockBackendAt(t, srv.URL, srv.Client(), timing)
	b2 := newLockBackendAt(t, srv.URL, srv.Client(), timing)
	seedLockObject(context.Background(), t, b1, time.Now().UTC().Add(-time.Hour))

	releases := make([]func() error, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, b := range []*Backend{b1, b2} {
		wg.Go(func() {
			_, release, err := b.Lock(context.Background())
			releases[i], errs[i] = release, err
		})
	}
	wg.Wait()

	if holders := releaseHolders(t, releases); holders != 1 {
		t.Fatalf("%d of 2 reclaimers ended up holding the lock, want exactly 1 (errors: %v)", holders, errs)
	}
	for i, err := range errs {
		if err != nil && !errors.Is(err, helpers.ErrCacheBusy) {
			t.Fatalf("acquirer %d lost the reclaim with %v, want an error matching helpers.ErrCacheBusy", i, err)
		}
	}
}

// newHeldFirstDeleteServer starts a fake S3 server that holds the FIRST
// DELETE for key - and only that one - for hold before serving it, leaving
// every other request untouched. Holding it before the fake sees it, rather
// than through the fake's own deleteDelay, is what keeps the delay on one
// request instead of on every delete the run makes.
func newHeldFirstDeleteServer(t *testing.T, key string, hold time.Duration) *httptest.Server {
	t.Helper()
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key

	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == lockPath && deletes.Add(1) == 1 {
			time.Sleep(hold)
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// releaseHolders releases every acquirer that ended up holding the lock and
// returns how many there were. A release that reports an error is a fixture
// failure rather than the property under test, so it is reported with Errorf
// and the count is still returned.
func releaseHolders(t *testing.T, releases []func() error) int {
	t.Helper()
	holders := 0
	for i, release := range releases {
		if release == nil {
			continue
		}
		holders++
		if err := release(); err != nil {
			t.Errorf("acquirer %d: release: %v", i, err)
		}
	}
	return holders
}
