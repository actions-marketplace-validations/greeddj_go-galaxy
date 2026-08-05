package s3

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// lockRaceIterations is how many acquire/steal/release cycles the race
	// below runs. It is a budget for hitting a window, not a duration: the
	// whole loop takes well under a second because every cycle is bounded by
	// one heartbeat interval plus a handful of in-process round trips.
	lockRaceIterations = 300
	// lockRaceSpreadSteps and lockRaceSpreadUnit sweep the release call
	// across the window between the heartbeat's HEAD leaving the fake and
	// that tick acting on the answer. Measured on this repository's fake
	// (in-process httptest, no network), that gap ran 18-74us with a median
	// near 35us, so 48 steps of 2us cover it end to end with margin at both
	// ends. Both numbers are a sweep range, never a timing assumption: a
	// machine where the gap sits outside this range loses detections, which
	// the positive control below turns into a loud failure rather than a
	// silently vacuous pass.
	lockRaceSpreadSteps = 48
	lockRaceSpreadUnit  = 2 * time.Microsecond
	// lockRaceEventCeiling is a LIVENESS bound on waiting for a heartbeat
	// HEAD, matching lockEventWaitCeiling's own contract: a slow machine only
	// makes this slower, never wrong.
	lockRaceEventCeiling = 2 * time.Second
)

// lockRaceTiming is deliberately not testLockTiming: that one's 30ms
// heartbeat interval would make 300 cycles take ten seconds, and its
// intervals are sized for tests that synchronize on whole heartbeat ticks
// rather than on the inside of one. ttl is short so the loop is
// self-healing - a cycle that somehow leaves a live lock object behind is
// reclaimed by the next acquisition instead of stalling it until the wait
// ceiling.
func lockRaceTiming() lockTiming {
	return lockTiming{
		ttl:                100 * time.Millisecond,
		heartbeatInterval:  time.Millisecond,
		heartbeatOpTimeout: 200 * time.Millisecond,
		releaseTimeout:     200 * time.Millisecond,
		waitCeiling:        5 * time.Second,
		backoffBase:        time.Millisecond,
		backoffCap:         5 * time.Millisecond,
	}
}

// TestReleaseRacingTheTickKeepsTheLossCause pins the one ordering inside
// startHeartbeat's release closure that no other test in this package can
// see: release must join the heartbeat goroutine (<-done) BEFORE it cancels
// the holder context, so that a tick which is mid-decision has already
// finished - and already recorded its loss cause - by the time release's own
// nil cause reaches a first-cancel-wins context.
//
// The property is stated as an implication plus a positive control, which is
// what makes it neither flaky nor vacuous:
//
//   - whenever release() reports errS3LockLost, context.Cause(holderCtx) must
//     match helpers.ErrCacheLockLost. A cycle where release reports no loss is
//     not a failure and asserts nothing: the release's own hbCancel can abort
//     the heartbeat's in-flight HEAD, in which case that tick reports a
//     transient error and never declares anything, which is correct behavior.
//   - at least one cycle must have reported a loss. Without this clause a
//     build where NO cycle ever detects would pass while proving nothing.
//     Measured here: 45 of 300 cycles detected.
//
// The window is reached through the fake's own request counter. countRequest
// runs at the top of handleHead, before the response is built, so observing
// that counter move puts this goroutine a full HTTP round trip ahead of the
// tick's decision - the only signal available that is EARLIER than the
// decision rather than simultaneous with it. Each cycle then spins a
// different slice of that gap (see lockRaceSpreadSteps) before releasing, so
// the loop sweeps the whole window instead of sampling one point in it.
//
// One fake and one backend serve every cycle: the foreign token is seeded
// with an already-elapsed deadline, so the next acquisition reclaims it
// immediately rather than contending, and the test needs no second httptest
// server per cycle.
//
// KILLING MUTATION, run and reverted: moving `holderCancel(nil)` above the
// `<-done` join in startHeartbeat's release closure - the edit that removes
// the happens-before while leaving first-cancel-wins intact, so every
// existing test in this package stays green. Nine runs failed, at cycles 12,
// 20, 23, 29, 30, 32, 35, 41 and 66 - what was measured, never a bound; one:
//
//	lock_race_test.go:104: cycle 30: release reported the lock lost, context.Cause(holderCtx) = context canceled
func TestReleaseRacingTheTickKeepsTheLossCause(t *testing.T) {
	t.Parallel()
	fake := newFakeS3()
	b := newTestBackendWithFake(t, fake)
	b.lock = lockRaceTiming()
	key := b.key(locksPrefix, lockObject)

	detections := 0
	for cycle := range lockRaceIterations {
		if lockRaceCycle(t, b, fake, key, cycle) {
			detections++
		}
	}
	if detections == 0 {
		t.Fatalf("no cycle of %d reported a lost lock; the implication above held vacuously", lockRaceIterations)
	}
}

// lockRaceCycle runs one acquire, steal, release cycle and reports whether
// release detected the loss. It fails the test only on the implication this
// file exists for, or on a fixture step that did not do what it was asked.
func lockRaceCycle(t *testing.T, b *Backend, fake *fakeS3, key string, cycle int) bool {
	t.Helper()
	ctx := context.Background()
	holderCtx, release, err := b.Lock(ctx)
	if err != nil {
		t.Fatalf("cycle %d: Lock: %v", cycle, err)
	}
	// An unconditional overwrite carrying another acquirer's token and an
	// already-elapsed deadline: the heartbeat's next verifyOwner sees a
	// foreign token and declares the loss, and whatever this cycle leaves
	// behind is reclaimable by the next one.
	if err := b.putLock(ctx, key, foreignToken, time.Now().UTC().Add(-b.lock.ttl), false); err != nil {
		t.Fatalf("cycle %d: seed foreign takeover: %v", cycle, err)
	}
	waitForHeartbeatHead(t, fake, key, cycle, holderCtx.Err)
	spinFor(time.Duration(cycle%lockRaceSpreadSteps) * lockRaceSpreadUnit)

	if !errors.Is(release(), errS3LockLost) {
		return false
	}
	if cause := context.Cause(holderCtx); !errors.Is(cause, helpers.ErrCacheLockLost) {
		t.Fatalf("cycle %d: release reported the lock lost, context.Cause(holderCtx) = %v", cycle, cause)
	}
	return true
}

// waitForHeartbeatHead blocks until the fake has begun serving a heartbeat
// HEAD issued after the seed above, busy-polling rather than sleeping because
// the whole point is to act inside a window measured in microseconds - a
// sleep with a millisecond floor would step straight over it.
//
// It also returns once the holder context has ended, and that arm is
// required rather than defensive: the tick that detects the loss may be one
// whose HEAD was already counted before this function read its baseline, in
// which case no further HEAD is ever served (the heartbeat goroutine exits on
// a detected loss) and waiting for one would hang until the ceiling.
//
// holderErr is passed as a function rather than the context itself so
// *testing.T stays the first parameter without tripping revive's
// context-as-argument rule.
func waitForHeartbeatHead(t *testing.T, fake *fakeS3, key string, cycle int, holderErr func() error) {
	t.Helper()
	before := fake.requestCount(key, http.MethodHead)
	deadline := time.Now().Add(lockRaceEventCeiling)
	for {
		if fake.requestCount(key, http.MethodHead) > before || holderErr() != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cycle %d: no heartbeat HEAD within %v", cycle, lockRaceEventCeiling)
		}
		runtime.Gosched()
	}
}

// spinFor busy-waits for d, yielding rather than sleeping: the durations here
// are single-digit microseconds, well below what a timer can resolve, and the
// yield keeps the heartbeat goroutine and the fake's own handler runnable
// while this one waits.
func spinFor(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		runtime.Gosched()
	}
}
