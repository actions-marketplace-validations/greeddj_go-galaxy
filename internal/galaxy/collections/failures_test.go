package collections

// This file pins failureRecorder/failureSummary/summaryError's contract in
// isolation, one layer below the end-to-end propagation covered in
// failure_propagation_test.go. Each test below was verified against
// the specific killing mutation named in its own doc comment, with the real
// observed failure output quoted:
//
//   - TestFailureRecorderIsConcurrencySafe: dropping the mutex in record
//     (appending to r.causes with no lock, leaving only n.Add(1) atomic)
//     makes `go test -race -run TestFailureRecorderIsConcurrencySafe` fail
//     with a real data race, reported by the race detector as:
//     "WARNING: DATA RACE
//     Write at 0x00c0000a9ce0 by goroutine 11:
//       github.com/greeddj/go-galaxy/internal/galaxy/collections.(*failureRecorder).record()
//           .../failures.go:30 +0x104
//     Previous read at 0x00c0000a9ce0 by goroutine 10:
//       github.com/greeddj/go-galaxy/internal/galaxy/collections.(*failureRecorder).record()
//           .../failures.go:30 +0x78"
//     (the race detector reports several overlapping read/write races across
//     the 64 goroutines; this is the first one it surfaces)
//   - TestSummaryErrorRendersHeadlineOnly: changing wrap to
//     `return errors.Join(headline, s.cause)` unconditionally (skipping the
//     one-line *summaryError wrapper) makes the test fail with:
//     "installError().Error() = \"installation failed for 3 collections\\ncause 0\\ncause 1\\ncause 2\",
//     want \"installation failed for 3 collections\""
//   - TestFailureRecorderSummaryIsEmptyWhenNothingRecorded: pre-sizing causes
//     with `make([]error, 0, 1)` in a zero-value recorder is not itself
//     observable through this test (errors.Join treats a slice of len 0 the
//     same regardless of capacity), which is exactly why this test exists as
//     the positive control instead: it is the one place a reader can confirm
//     the whole zero-value path returns nil/zero end to end before trusting
//     the concurrency and rendering tests below to mean anything.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errTestCause0/1/2 and errTestWarmCause0/1 stand in for distinct
// per-collection failure causes recorded by a failureRecorder in this file's
// tests. Declared as static package-level sentinels, rather than inline
// errors.New calls, purely to satisfy err113 - production code never compares
// against them.
var (
	errTestCause0 = errors.New("cause 0")
	errTestCause1 = errors.New("cause 1")
	errTestCause2 = errors.New("cause 2")

	errTestWarmCause0 = errors.New("warm cause 0")
	errTestWarmCause1 = errors.New("warm cause 1")

	errTestOutdatedCause0 = errors.New("outdated cause 0")
	errTestOutdatedCause1 = errors.New("outdated cause 1")

	errTestSaveFailure     = errors.New("simulated save failure")
	errTestCollectionCause = errors.New("collection cause")
)

// TestFailureRecorderSummaryIsEmptyWhenNothingRecorded is the positive
// control for the whole failureRecorder/failureSummary/summaryError chain: a
// zero-value recorder that never observed a single record call must report
// zero everywhere, including nil-valued headline errors, so the other tests
// in this file (which only check one failure mode each) are meaningful
// against a baseline that is known to succeed.
func TestFailureRecorderSummaryIsEmptyWhenNothingRecorded(t *testing.T) {
	t.Parallel()
	var r failureRecorder

	if got := r.count(); got != 0 {
		t.Fatalf("count() = %d, want 0", got)
	}

	summary := r.summary()
	if summary.count != 0 {
		t.Fatalf("summary.count = %d, want 0", summary.count)
	}
	if summary.cause != nil {
		t.Fatalf("summary.cause = %v, want nil", summary.cause)
	}
	if err := summary.installError(); err != nil {
		t.Fatalf("installError() = %v, want nil", err)
	}
	if err := summary.warmError(); err != nil {
		t.Fatalf("warmError() = %v, want nil", err)
	}
}

// TestFailureRecorderIsConcurrencySafe drives 64 goroutines that each record
// a distinct, wrapped sentinel concurrently, then asserts the recorder
// observed all 64 and that every one of them is still reachable via
// errors.Is through the summary's installError. Run with -race: the killing
// mutation is dropping the mutex from record, which turns the concurrent
// append into a data race the detector catches (see this file's header
// comment for the real observed race report).
func TestFailureRecorderIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	const workers = 64

	var r failureRecorder
	sentinels := make([]error, workers)
	for i := range sentinels {
		sentinels[i] = fmt.Errorf("worker %d failed: %w", i, helpers.ErrDownloadFailed)
	}

	var wg sync.WaitGroup
	for i := range sentinels {
		wg.Go(func() {
			r.record(sentinels[i])
		})
	}
	wg.Wait()

	if got := r.count(); got != workers {
		t.Fatalf("count() = %d, want %d", got, workers)
	}

	summary := r.summary()
	if summary.count != workers {
		t.Fatalf("summary.count = %d, want %d", summary.count, workers)
	}
	err := summary.installError()
	for i, sentinel := range sentinels {
		if !errors.Is(err, sentinel) {
			t.Fatalf("installError() does not match sentinels[%d] = %v", i, sentinel)
		}
	}
}

// TestSummaryErrorRendersHeadlineOnly asserts installError's message stays
// exactly the one-line headline - "installation failed for 3 collections",
// with no cause text appended - while errors.Is still reaches
// helpers.ErrInstallationFailed and every recorded cause through Unwrap. The
// killing mutation is building wrap's non-nil-cause branch as
// errors.Join(headline, s.cause) directly instead of through *summaryError:
// see this file's header comment for the real observed message that
// mutation produces.
func TestSummaryErrorRendersHeadlineOnly(t *testing.T) {
	t.Parallel()
	var r failureRecorder
	causes := []error{errTestCause0, errTestCause1, errTestCause2}
	for _, c := range causes {
		r.record(c)
	}

	summary := r.summary()
	err := summary.installError()

	const want = "installation failed for 3 collections"
	if got := err.Error(); got != want {
		t.Fatalf("installError().Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	for i, c := range causes {
		if !errors.Is(err, c) {
			t.Fatalf("expected errors.Is causes[%d] = %v, got %v", i, c, err)
		}
	}
}

// TestWarmErrorKeepsItsOwnHeadline pins warmError's distinct headline
// wording, byte for byte, alongside the same one-line-message /
// full-cause-tree contract TestSummaryErrorRendersHeadlineOnly pins for
// installError.
func TestWarmErrorKeepsItsOwnHeadline(t *testing.T) {
	t.Parallel()
	var r failureRecorder
	causes := []error{errTestWarmCause0, errTestWarmCause1}
	for _, c := range causes {
		r.record(c)
	}

	summary := r.summary()
	err := summary.warmError()

	const want = "installation failed: warm failed for 2 collections"
	if got := err.Error(); got != want {
		t.Fatalf("warmError().Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	for i, c := range causes {
		if !errors.Is(err, c) {
			t.Fatalf("expected errors.Is causes[%d] = %v, got %v", i, c, err)
		}
	}
}

// TestOutdatedErrorKeepsItsOwnHeadline pins outdatedError's distinct
// headline wording, byte for byte, alongside the same one-line-message /
// full-cause-tree contract TestSummaryErrorRendersHeadlineOnly pins for
// installError and TestWarmErrorKeepsItsOwnHeadline pins for warmError.
func TestOutdatedErrorKeepsItsOwnHeadline(t *testing.T) {
	t.Parallel()
	var r failureRecorder
	causes := []error{errTestOutdatedCause0, errTestOutdatedCause1}
	for _, c := range causes {
		r.record(c)
	}

	summary := r.summary()
	err := summary.outdatedError()

	const want = "latest version lookup failed for 2 collections"
	if got := err.Error(); got != want {
		t.Fatalf("outdatedError().Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, helpers.ErrLatestVersionLookupFailed) {
		t.Fatalf("expected errors.Is helpers.ErrLatestVersionLookupFailed, got %v", err)
	}
	for i, c := range causes {
		if !errors.Is(err, c) {
			t.Fatalf("expected errors.Is causes[%d] = %v, got %v", i, c, err)
		}
	}
}

// TestFailureSummarySurvivesSaveAnnotation proves the summary's joined error
// still passes through annotateSaveFailure without losing any of its three
// independently matchable errors: the ErrInstallationFailed headline, the
// recorded per-collection cause, and the save failure itself - and that the
// combined message stays one line containing the save-failure annotation.
func TestFailureSummarySurvivesSaveAnnotation(t *testing.T) {
	t.Parallel()

	var r failureRecorder
	r.record(errTestCollectionCause)
	summary := r.summary()

	err := annotateSaveFailure(summary.installError(), errTestSaveFailure)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, errTestCollectionCause) {
		t.Fatalf("expected errors.Is cause, got %v", err)
	}
	if !errors.Is(err, errTestSaveFailure) {
		t.Fatalf("expected errors.Is errSaveSentinel, got %v", err)
	}

	const wantSubstring = "; snapshot save failed:"
	got := err.Error()
	if !strings.Contains(got, wantSubstring) {
		t.Fatalf("Error() = %q, want it to contain %q", got, wantSubstring)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("Error() = %q, want a single line", got)
	}
}
