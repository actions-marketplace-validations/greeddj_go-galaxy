package s3

// This file covers cacheManager.WithStateDeadline against this package's own
// backend and fake: a numeric relation pin between helpers.StateObjectDeadline
// and this package's lock timings, and the only end-to-end evidence
// available that the decorator actually bounds a real *Backend.LoadStore
// against a dripping GET.
//
// Each test below was verified against a real revert of the production
// change it pins, and this comment quotes the actual observed output:
//
//   - TestStateDeadlineBoundsADrippingSnapshotRead, calling b.LoadStore(ctx)
//     directly instead of through cacheManager.WithStateDeadline, hangs
//     rather than fails - run with a bounded -timeout so the harness kills
//     it instead of blocking the suite forever, observed as:
//     "panic: test timed out after 5s
//     running tests:
//     TestStateDeadlineBoundsADrippingSnapshotRead (5s)"
//     with the stuck goroutine's frame at
//     "github.com/greeddj/go-galaxy/internal/cache/s3.(*fakeS3).driveDripGet(...)"
//     serving the never-ending drip, reached through
//     "github.com/greeddj/go-galaxy/internal/cache/s3.(*Backend).readObject(...)"
//     and "github.com/greeddj/go-galaxy/internal/cache/s3.(*Backend).LoadStore(...)"
//     in the same trace.
//
// DELIBERATELY NOT TESTED here, and said so rather than hidden: the
// fleet-level consequence of a stalled state read (every other runner
// sharing the bucket locked out until its own lockWaitCeiling wait expires)
// is the motivation for this whole item, not an assertable behavior in a unit
// or integration test - TestStateObjectDeadlineFitsInsideTheLockTimings pins
// its numeric form instead. Also deliberately not tested: whether
// s3Retryable needs a change for a budget expiry inside getObject's own
// retry loop. It does not, and this was verified by reading rather than by
// adding a duplicate test: a budget expiry there surfaces as
// context.DeadlineExceeded (client.Do wraps a canceled-context dial/read in a
// *url.Error carrying it), and s3Retryable already classifies
// context.DeadlineExceeded as terminal ahead of its *retryableStatusError
// check (see s3Retryable's own doc comment), so a state-object deadline
// expiring mid-retry cannot cause getObject to spend a second attempt.

import (
	"context"
	"errors"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// stateDeadlineBudget is the fixed cacheManager.WithStateDeadline budget
// every test in this file uses.
const stateDeadlineBudget = 300 * time.Millisecond

// TestStateObjectDeadlineFitsInsideTheLockTimings is a relation pin, not a
// behavior test: it asserts the numeric ordering
// helpers.StateObjectDeadline's own doc comment claims relative to this
// package's lock timings - StateObjectDeadline < heartbeatInterval < lockTTL,
// and 3*StateObjectDeadline < lockWaitCeiling, where 3 is the number of state
// operations one run performs while holding the lock on either path: install
// spends LoadStore (initInstall) + RecordProject (recordProjectUnlessDryRun)
// + SaveStore (finalizeInstall); cleanup spends LoadStore + LoadProjectRegistry
// (both in initCleanup) + SaveStore (finalizeCleanup) - three either way. This
// undercounts one verb, deliberately: Backend.RecordProject itself calls
// LoadProjectRegistry internally before its own PUT, so the single
// WithStateDeadline budget wrapping one RecordProject call actually covers a
// GET+PUT pair, not one verb - correct as designed (the whole point of one
// budget per Backend-seam call, not per HTTP verb), but worth remembering
// here since it means the real per-call-site cost this 3x/60s arithmetic
// bounds can include two S3 round trips under a single "operation". A
// violation of the assertions below would mean the state-object budget could
// itself starve the lock protocol it exists to protect.
func TestStateObjectDeadlineFitsInsideTheLockTimings(t *testing.T) {
	t.Parallel()
	if helpers.StateObjectDeadline >= heartbeatInterval {
		t.Fatalf("helpers.StateObjectDeadline (%s) must be less than heartbeatInterval (%s)",
			helpers.StateObjectDeadline, heartbeatInterval)
	}
	if heartbeatInterval >= lockTTL {
		t.Fatalf("heartbeatInterval (%s) must be less than lockTTL (%s)", heartbeatInterval, lockTTL)
	}
	if 3*helpers.StateObjectDeadline >= lockWaitCeiling {
		t.Fatalf("3*helpers.StateObjectDeadline (%s) must be less than lockWaitCeiling (%s)",
			3*helpers.StateObjectDeadline, lockWaitCeiling)
	}
}

// TestStateDeadlineBoundsADrippingSnapshotRead stands up a real *Backend
// against this package's in-memory fake, seeds a real store object, then
// arms a drip on that object's GET body: cacheManager.WithStateDeadline
// turns the resulting hang into helpers.ErrStateObjectDeadline (matching
// neither context sentinel through errors.Is) instead of blocking for as
// long as the caller's own context allows. This is the only end-to-end
// evidence available for this decorator against a real S3 backend, since
// this package's fake is unexported and unreachable from
// internal/galaxy/collections - the same limitation
// s3_cache_recovery_test.go records for its own fixture.
func TestStateDeadlineBoundsADrippingSnapshotRead(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion, nil)
	fake.dripGet(b.key(statePrefix, storeObject), 5*time.Millisecond)

	wrapped := cacheManager.WithStateDeadline(b, stateDeadlineBudget)
	_, err := wrapped.LoadStore(ctx)
	if !errors.Is(err, helpers.ErrStateObjectDeadline) {
		t.Fatalf("LoadStore error = %v, want errors.Is ErrStateObjectDeadline", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("must not match context.DeadlineExceeded: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
}

// TestStateDeadlineBoundsADrippingSnapshotReadPositiveControl is the
// positive control for the test above: the identical fake with no drip
// armed decodes a real store through the same wrapper and the same budget,
// proving the budget is not itself what would fail an ordinary load against
// this fixture.
func TestStateDeadlineBoundsADrippingSnapshotReadPositiveControl(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion, nil)

	wrapped := cacheManager.WithStateDeadline(b, stateDeadlineBudget)
	st, err := wrapped.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if st == nil {
		t.Fatal("LoadStore returned a nil store with a nil error")
	}
}
