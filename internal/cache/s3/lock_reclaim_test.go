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
	// lock object while this one is reclaiming it - the shape the swap's own
	// If-Match condition exists to lose against.
	competingToken = "competing-reclaimer-token"
)

// TestReclaimSwapsOnlyTheVersionItRead covers what reclaimIfExpired does when
// the object changes between the HEAD that judged it expired and the
// compare-and-swap that would take it over. The seeded object is expired, so
// that HEAD decides "reclaim this" and reads the ETag the swap is conditioned
// on; each row then installs a different state behind that HEAD.
//
// The interference is installed inside the handler, before it returns, which
// is what makes the ordering exact rather than probable: the fake writes a HEAD
// response through net/http's own buffered writer with no flush of its own, so
// the bytes reach the client only once the handler returns, strictly after the
// interference.
//
// The rows are five reachable states, not five assertions about one:
//
//   - a rewrite that changes ONLY the version: the same foreign token, the
//     same expired deadline, a new ETag. Nothing the protocol reads by name
//     differs, so a swap that still lands could only have ignored the
//     condition - which makes this row, alone in the table, proof that the
//     ETag is what arbitrates. The writer that produced it is another
//     acquirer, so observed is true.
//   - a rewrite under a competing acquirer's token: the realistic shape of the
//     same loss, and the one that pins whose write is left standing.
//   - the object deleted behind the HEAD: a swap has no version left to match,
//     which S3 answers with 404 rather than 412. Every deleter of this key is a
//     holder releasing it, so the create is worth retrying at once: retryNow
//     alone is set, and nothing is observed.
//   - a swap the backend refuses outright (403): the attempt ends rather than
//     guessing at what the object holds. The status is chosen for what it
//     costs, not for its meaning: helpers.IsRetryableHTTPStatus retries only
//     429/500/502/503/504, so 403 skips the client's retry ladder that a 5xx
//     would drag through this row's runtime. The error's CLASS is asserted
//     rather than its non-nilness - a status the remote itself answered
//     carries helpers.ErrCacheBackendUnavailable, per the partition doc in
//     variables.go.
//   - nothing racing in at all: the positive control, and the table's only
//     one, so nobody need add a second. The same fixture, with nothing armed,
//     must still reclaim (release returned, our token standing on the object),
//     since otherwise "it refused" here would be indistinguishable from "it
//     never got that far".
//
// Every row also asserts that not one DELETE was issued against the lock key,
// which is the property the whole compare-and-swap change is for: the reclaim
// used to delete the object and recreate it, and two reclaimers could both
// come out of that believing they had won.
//
// KILLING MUTATIONS, run and reverted. Replacing the swap's condition with an
// unconditional write - putCondition{ifMatch: etag} to putCondition{} - fails
// three rows, which is the whole point of the change. Both rewrite rows report
//
//	lock_reclaim_test.go:101: attempt.observed = false, want true
//
// and the deleted row reports what the next mutation does, since an
// unconditional write simply recreates the object it found gone.
//
// Dropping the 404 arm, so a vanished object falls through to the arm that
// abandons and returns, fails the deleted row alone, one assertion earlier -
// on the error, before any field of the attempt is read:
//
//	lock_reclaim_test.go:101: reclaimIfExpired: s3 object not found
//
// Answering that same arm with observed instead of retryNow fails the same row
// on the first field checked:
//
//	lock_reclaim_test.go:101: attempt.retryNow = false, want true
//
// All are reported against the assertReclaimSwap call below rather than inside
// that helper, or the helpers it calls, all of which call t.Helper().
func TestReclaimSwapsOnlyTheVersionItRead(t *testing.T) {
	t.Parallel()

	for _, tc := range reclaimSwapCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertReclaimSwap(t, tc)
		})
	}
}

// reclaimSwapCase is one row of TestReclaimSwapsOnlyTheVersionItRead:
// rewriteToken and rewriteAhead describe an object installed behind
// reclaimIfExpired's own HEAD - an empty rewriteToken arms no rewrite, and a
// negative rewriteAhead makes the rewritten object expired - dropObject
// deletes it there instead, failSwapStatus arms a one-shot status for the swap
// PUT, and the want fields describe the lockAttempt, the error, and the stored
// object that must follow.
type reclaimSwapCase struct {
	name           string
	wantErr        error
	rewriteToken   string
	wantToken      string
	rewriteAhead   time.Duration
	failSwapStatus int
	dropObject     bool
	wantGone       bool
	wantObserved,
	wantRetryNow,
	wantRelease bool
}

// reclaimSwapCases returns the five states reachable between reclaimIfExpired's
// expiry decision and the swap it conditions on that same read.
func reclaimSwapCases() []reclaimSwapCase {
	return []reclaimSwapCase{
		{
			name:         "a rewrite that changes only the version loses the swap",
			rewriteToken: foreignToken,
			rewriteAhead: -time.Hour,
			wantToken:    foreignToken,
			wantObserved: true,
		},
		{
			name:         "a competing acquirer's write is left standing",
			rewriteToken: competingToken,
			rewriteAhead: time.Hour,
			wantToken:    competingToken,
			wantObserved: true,
		},
		{
			name:         "a lock deleted before the swap is retried immediately",
			dropObject:   true,
			wantGone:     true,
			wantRetryNow: true,
		},
		{
			name:           "a swap the backend refuses ends the attempt",
			wantToken:      foreignToken,
			wantErr:        helpers.ErrCacheBackendUnavailable,
			failSwapStatus: http.StatusForbidden,
		},
		{
			name:        "the same fixture with nothing racing in reclaims",
			wantToken:   ourToken,
			wantRelease: true,
		},
	}
}

// assertReclaimSwap runs one reclaimSwapCase end to end against a real fake S3
// server.
func assertReclaimSwap(t *testing.T, tc reclaimSwapCase) {
	t.Helper()
	ctx := context.Background()
	key := path.Join(locksPrefix, lockObject)
	b, fake := newReclaimSwapFixture(t, tc, key)

	// Open's conditional-write probe writes under the locks prefix too, so the
	// baseline is asserted rather than assumed: a probe that ever started
	// HEADing this key would make the interference fire behind the wrong
	// request and quietly turn every row below into a different test.
	if got := fake.requestCount(key, http.MethodHead); got != 0 {
		t.Fatalf("HEAD count on the lock key before the reclaim = %d, want 0", got)
	}

	attempt, err := b.reclaimIfExpired(ctx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	assertReclaimError(t, tc.wantErr, err)
	// The delete count comes first among the outcome checks because it is the
	// one every row shares: a compare-and-swap reclaim has no delete step at
	// all, on any path, including the one that abandons an object it may have
	// written (which declines the delete for a foreign token).
	if got := fake.requestCount(key, http.MethodDelete); got != 0 {
		t.Fatalf("DELETE count on the lock key = %d, want 0: a swap reclaim never deletes", got)
	}
	assertReclaimOutcome(t, tc, attempt)
	assertReclaimSwapObject(t, tc, b, key)
}

// assertReclaimError checks the error one row expects. A row naming no error
// requires a nil one; a row naming one requires that CLASS rather than merely
// something non-nil, since what the 403 row is worth is which of the
// cache-backend classes a status the remote itself answered lands in (see
// variables.go's partition doc).
func assertReclaimError(t *testing.T, want, err error) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("reclaimIfExpired: %v", err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("reclaimIfExpired error = %v, want one matching %v", err, want)
	}
}

// newReclaimSwapFixture builds the fake S3 server one reclaimSwapCase runs
// against: its handler applies tc's interference immediately after the first
// HEAD for the lock key has been served, and the lock object is seeded as an
// expired foreign holder so reclaimIfExpired's own HEAD decides to reclaim it.
// It returns the Backend pointed at that server and the fake behind it.
func newReclaimSwapFixture(t *testing.T, tc reclaimSwapCase, key string) (*Backend, *fakeS3) {
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
		switch {
		case tc.rewriteToken != "":
			fake.storeLockObject(key, tc.rewriteToken, time.Now().UTC().Add(tc.rewriteAhead))
		case tc.dropObject:
			fake.dropObject(key)
		}
		if tc.failSwapStatus != 0 {
			// One use only, so it is spent on the swap PUT and no later write
			// this row makes is affected.
			fake.failNext(key, http.MethodPut, tc.failSwapStatus, 1)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	b := newLockBackendAt(t, srv.URL, srv.Client(), testLockTiming(time.Minute))
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(-time.Hour), putCondition{}); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}
	return b, fake
}

// assertReclaimOutcome checks the lockAttempt one reclaimSwapCase expects. It
// is split out only to keep assertReclaimSwap's own fixture setup readable; all
// three checks here are on the value reclaimIfExpired returned.
//
// retryNow is checked before observed because the two fields are how the
// vanished-object answer differs from the contention one, and a row expecting
// retryNow reports the missing field rather than the spurious one it was
// swapped for.
func assertReclaimOutcome(t *testing.T, tc reclaimSwapCase, attempt lockAttempt) {
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

// assertReclaimSwapObject checks which write is standing on the lock object
// once the attempt is over - the question every row is ultimately about.
func assertReclaimSwapObject(t *testing.T, tc reclaimSwapCase, b *Backend, key string) {
	t.Helper()
	headers, err := b.client.headObject(context.Background(), key)
	if tc.wantGone {
		if !errors.Is(err, errS3NotFound) {
			t.Fatalf("lock object after a reclaim of a deleted lock: headObject err = %v, want errS3NotFound", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("headObject after the reclaim: %v", err)
	}
	if got := headers.Get("X-Amz-Meta-Token"); got != tc.wantToken {
		t.Fatalf("lock object token after the reclaim = %q, want %q", got, tc.wantToken)
	}
}

// TestReclaimRefusesAnUnversionedHead drives the one runtime path that reaches
// errS3CompareAndSwapUnsupported after Open's probe has already passed: a
// backend that stores the object and then answers a HEAD without naming its
// version, leaving the swap nothing to be conditioned on.
//
// The refusal is not merely an error but a REFUSAL, which is why the PUT count
// is asserted alongside it: the alternative implementation - falling back to an
// unconditional overwrite - would also return the object to this run, and the
// only visible difference is that a write was issued at all.
//
// The ETag is withheld only after Open, because Open's own probe requires one:
// arming it earlier would fail there instead, and this test would then prove
// nothing about the reclaim path. Its own positive control is
// TestReclaimSwapsOnlyTheVersionItRead's last row, which shows this same
// fixture shape reclaiming when the ETag is present.
//
// KILLING MUTATION, run and reverted: deleting the empty-ETag refusal, so the
// swap is reached with etag == "" - which putCondition treats as no condition
// at all, making it the unconditional overwrite this branch exists to refuse:
//
//	lock_reclaim_test.go:339: reclaimIfExpired error = <nil>, want one matching
//	cache backend cannot be used as configured
func TestReclaimRefusesAnUnversionedHead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	key := path.Join(locksPrefix, lockObject)
	b, fake := newTestBackendAndFake(t)
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(-time.Hour), putCondition{}); err != nil {
		t.Fatalf("seed expired lock: %v", err)
	}
	puts := fake.requestCount(key, http.MethodPut)
	fake.setSuppressETag(true)

	attempt, err := b.reclaimIfExpired(ctx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnusable) {
		t.Fatalf("reclaimIfExpired error = %v, want one matching %v", err, helpers.ErrCacheBackendUnusable)
	}
	if attempt.release != nil {
		t.Fatalf("the reclaim held the lock against a backend that names no version")
	}
	if got := fake.requestCount(key, http.MethodPut) - puts; got != 0 {
		t.Fatalf("PUT count on the lock key = %d, want 0: an unversioned HEAD must be refused, not overwritten", got)
	}
}

// reclaimCutShortBudget bounds the acquisition of the row that must fail after
// its swap PUT landed. It has to clear the two loopback round trips ahead of
// the swap - measured at 87-169us each on this fixture - by enough that the row
// never fails before reaching the state it is about, and it is also the entire
// time the fixture then holds the ownership check open, so it is what that row
// waits. Well past the former, small enough to be a cheap test.
const reclaimCutShortBudget = 300 * time.Millisecond

// reclaimCompletionMargin is the deadline the control row hands
// reclaimIfExpired. That row's client is wrapped, so it is not a margin the row
// bets on; freshCreateCompletionMargin is its twin on the create branch and
// holds both why the two rows cannot share one constant and what the wrapper
// changed about this one.
const reclaimCompletionMargin = 10 * time.Second

// TestReclaimAbandonsALockItNeverHeld drives what happens after a reclaim's own
// swap PUT has landed and the acquisition then ends anyway. The object at that
// moment records this run's token with no heartbeat behind it: left there, it
// blocks every other acquirer for a full lock TTL, which is twice the ceiling
// each of them waits before giving up.
//
// The failure row builds that state exactly: the budget is wide enough for the
// swap to land and the fixture then holds the ownership check that follows it
// until that budget is gone, so the run always fails at the same point rather
// than racing a loopback round trip it would win.
//
// The second row is the positive control on the same fixture, with the block
// disarmed: the reclaim holds the lock and the object still records our token,
// so the failure row's "the object is gone" cannot be the fixture never letting
// a reclaim through in the first place.
//
// The failure row asserts the object is GONE rather than merely foreign, since
// foreign is what a competing acquirer would leave and this cleanup must never
// delete that.
//
// The two rows are not protected by the same thing - the control row cannot be
// cut short by a clock at all, while the failure row is defined by its budget
// firing - and that asymmetry is deliberate rather than something to be tidied
// away. orphanRowClient is where it is decided, and why.
//
// KILLING MUTATION, run and reverted: deleting the abandonLockObject call from
// reclaimIfExpired's claim-error arm. The failure row then leaves the orphan
// behind:
//
//	lock_reclaim_test.go:408: lock object after an abandoned reclaim: headObject err = <nil>, want errS3NotFound
func TestReclaimAbandonsALockItNeverHeld(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		blockPostPutHead bool
		wantRelease      bool
	}{
		{name: "a budget that fits the swap but not the ownership check", blockPostPutHead: true},
		{name: "the same fixture with nothing cutting the acquisition short", wantRelease: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertReclaimOrphanCleanup(t, tc.blockPostPutHead, tc.wantRelease)
		})
	}
}

// assertReclaimOrphanCleanup runs one orphan-cleanup row end to end against a
// real fake S3 server, driving reclaimIfExpired directly: the create-PUT Lock
// issues first would poison the putsServed anchor and spend the row's budget.
func assertReclaimOrphanCleanup(t *testing.T, blockPostPutHead, wantRelease bool) {
	t.Helper()
	key := path.Join(locksPrefix, lockObject)
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key
	// The blocked row is the one whose budget must fire; the control row's has
	// to fit a whole reclaim inside it instead. See freshCreateCompletionMargin.
	budget := reclaimCompletionMargin
	if blockPostPutHead {
		budget = reclaimCutShortBudget
	}
	opCtx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)

	var putsServed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The ownership check that follows the swap PUT is held until the
		// caller's budget is gone, so the failure row fails on that budget
		// every time instead of racing a loopback round trip it would win.
		if blockPostPutHead && r.Method == http.MethodHead && r.URL.Path == lockPath && putsServed.Load() > 0 {
			<-opCtx.Done()
		}
		fake.ServeHTTP(w, r)
		if r.Method == http.MethodPut && r.URL.Path == lockPath {
			putsServed.Add(1)
		}
	}))
	t.Cleanup(srv.Close)

	b := newLockBackendAt(t, srv.URL, orphanRowClient(srv, blockPostPutHead), testLockTiming(time.Minute))
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Seeded straight into the fake rather than over HTTP, so the reclaim's own
	// swap PUT is the first one this key ever sees and the anchor above counts
	// only it.
	fake.storeLockObject(key, foreignToken, time.Now().UTC().Add(-time.Hour))

	attempt, err := b.reclaimIfExpired(opCtx, key, ourToken, testHolderCancel(t))
	if attempt.release != nil {
		t.Cleanup(func() { _ = attempt.release() })
	}
	assertReclaimOrphanOutcome(t, wantRelease, attempt, err)
	assertReclaimOrphanObject(t, wantRelease, b, key)
}

// assertReclaimOrphanOutcome checks the attempt and the error one
// orphan-cleanup row expects.
func assertReclaimOrphanOutcome(t *testing.T, wantRelease bool, attempt lockAttempt, err error) {
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
		t.Fatalf("reclaimIfExpired: %v", err)
	case !wantRelease && err == nil:
		t.Fatalf("reclaimIfExpired reported no error for an acquisition cut short after its swap PUT")
	}
}

// assertReclaimOrphanObject checks what the lock object holds once the attempt
// is over: nothing at all for a row whose reclaim was cut short, and this run's
// own token for the control.
func assertReclaimOrphanObject(t *testing.T, wantRelease bool, b *Backend, key string) {
	t.Helper()
	headers, err := b.client.headObject(context.Background(), key)
	if !wantRelease {
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

// reclaimSwapHold is how long the first swap PUT to reach the lock key is held
// before the fake sees it. It only has to exceed the round trip the other
// acquirer needs to get its own swap in - measured at 87-169us on this fixture
// - and every millisecond beyond that is time this test simply waits, so it is
// two orders of magnitude above that round trip and nothing more.
const reclaimSwapHold = 50 * time.Millisecond

// TestTwoReclaimersEndWithOneHolder is the whole defect in one test: two
// acquirers reading the same expired lock object, each judging it expired, each
// taking it over under its own token.
//
// The interleaving is forced, not sampled. The first swap PUT to reach the lock
// key is held before it is served, so the other acquirer's swap completes first
// and changes the object's version; the held one then arrives conditioned on a
// version that no longer exists and is refused. Under the delete-and-recreate
// reclaim this replaces, that same ordering left both acquirers holding: each
// verified a token it had itself just written, in a window neither could see.
// The hold is a fixed duration rather than anything derived from the other
// acquirer's progress, so no assertion here depends on how fast either one
// runs.
//
// Either acquirer may win. At delays this far below a round trip both orderings
// are legitimate, so the test asserts the count of holders and the loser's
// error class, never which Backend ends up with the lock.
//
// Both acquirers share one client wrapped in answeredDespiteCancelTransport,
// which closes this test's wall-clock exposures rather than widening them:
// neither the winner's swap and its ownership HEAD nor the loser's own
// observation of that winner can be cut short by the 600ms ceiling, so a
// starved goroutine costs time here and never a verdict. The ceiling still ends
// the loser's wait - lockBackoff selects on waitCtx, which no transport
// touches. The wrapper is admissible because newHeldFirstSwapServer answers
// every request it is given, holding one PUT and then serving it; a request
// that hangs, added to that fixture later, takes the admissibility away.
//
// Its reach is the whole run rather than the acquisition, and that is what
// covers the last of the three exposures rather than any budget of this test's
// own: the winner's heartbeat and both releases go through it too, so a release
// here is not bounded by testLockTiming's short default for as long as the
// client is wrapped. Unwrapping it hands all three exposures back at once - the
// two ceilings, and that default over a release's own HEAD and DELETE.
//
// KILLING MUTATION, run and reverted: replacing the swap's condition with an
// unconditional write, putCondition{ifMatch: etag} to putCondition{}. Five
// runs, five failures, all identical:
//
//	lock_reclaim_test.go:580: 2 of 2 reclaimers ended up holding the lock, want exactly 1 (errors: [<nil> <nil>])
//
// The two nil errors are the sharp end of it: both acquisitions reported
// success, so neither run learns anything is wrong until a heartbeat tick - and
// a run shorter than one interval never learns at all.
func TestTwoReclaimersEndWithOneHolder(t *testing.T) {
	t.Parallel()

	key := path.Join(locksPrefix, lockObject)
	timing := testLockTiming(time.Minute)
	timing.waitCeiling = 600 * time.Millisecond

	srv := newHeldFirstSwapServer(t, key, reclaimSwapHold)
	// A copy of the server's own client rather than a bare http.Client, so
	// whatever else httptest configured on it survives the wrapping.
	answering := *srv.Client()
	answering.Transport = answeredDespiteCancelTransport{base: srv.Client().Transport}
	b1 := newLockBackendAt(t, srv.URL, &answering, timing)
	b2 := newLockBackendAt(t, srv.URL, &answering, timing)
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

// newHeldFirstSwapServer starts a fake S3 server that holds the FIRST take-over
// PUT for key - and only that one - for hold before serving it, leaving every
// other request untouched.
//
// A take-over is identified by its position in the request sequence rather than
// by its If-Match header, and that is load-bearing rather than stylistic: the
// mutation this fixture exists to catch is the removal of that very header, and
// a fixture keyed on it would stop forcing any interleaving at all under the
// mutation and pass. Position identifies it just as exactly: each acquirer
// issues its create-if-absent PUT before it ever HEADs, so the first HEAD to
// reach this key marks the point after which the only PUTs left are take-overs.
// The seeded write is ahead of all of it.
func newHeldFirstSwapServer(t *testing.T, key string, hold time.Duration) *httptest.Server {
	t.Helper()
	fake := newFakeS3()
	lockPath := "/" + fake.bucket + "/" + key

	var heads, swaps atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		onLock := r.URL.Path == lockPath
		if onLock && r.Method == http.MethodHead {
			heads.Add(1)
		}
		if onLock && r.Method == http.MethodPut && heads.Load() > 0 && swaps.Add(1) == 1 {
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
