package collections_test

// This file covers the artifact download deadline end to end, using a
// byte-drip fault (fakegalaxy.Fault.DripInterval): a body that keeps making
// genuine progress forever, so the read-inactivity watchdog - which bounds
// the gap between two reads, not total transfer time - never trips on it.
// Only the whole-acquisition helpers.ArtifactDownloadDeadline can end such a
// run. This is modeled on newStallFixture in stall_e2e_test.go, but the
// watchdog's own idle window (Timeout) is set deliberately far above the
// artifact deadline, so only the deadline can be the reason the run ends.
//
// Each test below was verified against a real revert of the production
// change it pins, and this comment quotes the actual output observed:
//
//   - TestArtifactByteDripFailsAtTheDownloadDeadline, reverting
//     downloadCollectionToCache to pass ctx straight through (dropping the
//     context.WithTimeout and both artifactDeadlineError calls), hangs
//     rather than fails - that hang is the defect itself, and is the
//     strongest evidence that the deadline is what prevents it:
//     "panic: test timed out after 30s
//     running tests:
//     TestArtifactByteDripFailsAtTheDownloadDeadline (30s)"
//     with the stuck goroutine's frame at
//     "github.com/greeddj/go-galaxy/internal/galaxy/collections.
//     downloadCollectionToCache.func1()" (the retried closure, blocked
//     inside io.Copy reading the never-ending drip) directly beneath
//     "github.com/greeddj/go-galaxy/internal/galaxy/helpers.Retry(...)" in
//     the same trace.
//   - The same test, dropping newDripFixture's watchdogTimeout to 1ms so the
//     read-inactivity watchdog fires on nearly every dripped byte and races
//     the acquisition deadline, is a genuine kill filed under the COUNT
//     assertion, not under the watchdog-race assertions (c)/(e): at this
//     timescale the watchdog aborts each dripped-byte read within 1ms and
//     ErrReadStalled is retryable, so several attempts complete inside the
//     500ms budget before it expires. The returned error still carries the
//     deadline sentinel, so (c)/(e) still pass; the count assertion - the
//     one actually checking the deadline is terminal, spending the budget
//     once per acquisition rather than once per retry - fails instead,
//     observed as:
//     "EndpointArtifact count = 3, want 1 (the deadline is terminal: no
//     retry follows it)"
//     (a repeated run of the same mutation was also observed failing one
//     assertion earlier, on the missing-deadline-sentinel check itself, as:
//     "expected errors.Is ErrArtifactDownloadDeadline, got installation
//     failed for 1 collections"
//     - both outcomes are real and each on its own confirms the mutation is
//     caught; which one fires first is a race inside this MUTATED build
//     - not flakiness in the test itself, which is deterministic on HEAD,
//     where no retryable failure exists for the loop to act on in the first
//     place).
//   - Assertion (e), BytesDownloaded >= 2, IS falsifiable, and it is the one
//     that actually separates a byte-drip (progress throughout) from a stall
//     (no progress at all): swapping this test's fault to
//     fakegalaxy.Fault{Hang: true, Count: -1} makes the fake block before
//     writing any header, status, or body, so the acquisition deadline
//     cancels the round trip during the response-header phase instead. (a)
//     through (d) and the count assertion all still pass - the error is still
//     normalized to the deadline sentinel - and (e) is the first and only
//     line to fail, deterministically, since nothing is ever written and
//     there is no race to lose:
//     "BytesDownloaded = 0, want >= 2 (progress made throughout, the
//     byte-drip signature)"
//   - Assertions (c) and (d) - "expected the drip to defeat the watchdog, not
//     trip it" and "must not match context.Canceled" - are DOCUMENTARY rather
//     than pinned, and no mutation can change that: they are structurally
//     unfalsifiable in this fixture, in the same sense the outer
//     artifactDeadlineError call in downloadCollectionToCache is deliberately
//     uncovered. For either to be the first failing line, the run's error
//     would have to carry helpers.ErrArtifactDownloadDeadline (so (b) passes)
//     AND ErrReadStalled or context.Canceled (so (c) or (d) fires). That
//     combination cannot occur here: this fixture fails exactly one
//     collection, so failureSummary.wrap joins exactly one cause tree, and a
//     single cause carries one signature or the other, never both -
//     artifactDeadlineError renders its cause with %v, so the sentinel's tree
//     holds only the sentinel, while a watchdog-terminated acquisition
//     returns ErrReadStalled (rendering context.Canceled with %v) with no
//     sentinel in it at all. Every mutation that makes the watchdog win
//     therefore fails at (b) first, confirmed by running exactly that
//     (StallAfterBytes: 8, watchdog 100ms, deadline 10s; deterministic across
//     8 runs):
//     "expected errors.Is ErrArtifactDownloadDeadline, got installation
//     failed for 1 collections"
//     What (c) and (d) do buy is a tripwire on the FIXTURE rather than on
//     production: they fire if a future edit here lowers dripWatchdogTimeout,
//     changes the fault, or adds a second failing collection - that last one
//     would join two cause trees and break the mutual exclusion above,
//     turning both assertions falsifiable. mixed_fault_e2e_test.go is exactly
//     that fixture: it adds a second, stalled collection alongside a dripped
//     one, and its own errors.Is(err, helpers.ErrReadStalled)/errors.Is(err,
//     context.Canceled) assertions are the falsifiable versions of (c)/(d)
//     that this fixture cannot produce on its own. The reciprocal proof that
//     the two mechanisms really are distinct lives in the PAIR, not in (c):
//     this file proves a drip ends on the deadline and not the watchdog, and
//     stall_e2e_test.go:111 proves a stall ends on the watchdog and not the
//     deadline. Each half is falsifiable only through its own sentinel check,
//     never through (c).

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// dripWatchdogTimeout is the watchdog idle window (and http.Client timeout)
// every fixture in this file uses: deliberately far above the deadline these
// tests arm, so the watchdog can never be the reason a drip-faulted run ends
// - only the artifact download deadline can.
const dripWatchdogTimeout = 10 * time.Second

// newDripFixture registers a single acme.solo collection (version "*", no
// dependencies) on a fresh fake server and returns a matching config/runtime
// pair. noCache controls whether the background prefetcher runs: disabled
// (true), the artifact request count from a single acquisition is
// unambiguous; enabled (false), exactly two acquisitions run (one prefetch,
// one install-path), which TestByteDripCostsExactlyTwoAcquisitionsPerCollection
// depends on. watchdogTimeout is baked into the runtime's HTTP client at
// construction time (fetch.New reads it once), so - unlike the runtime's
// ArtifactDownloadDeadline, itself mutable after construction - it must be a
// parameter here rather than a field a caller overrides afterward: this is
// what lets a test deliberately race the watchdog against the deadline (see
// this file's own header comment for the observed result of doing so).
func newDripFixture(
	t *testing.T,
	noCache bool,
	deadline time.Duration,
	watchdogTimeout time.Duration,
) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.solo")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "solo", "1.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
		Timeout:          watchdogTimeout,
		NoCache:          noCache,
	}
	runtime := infra.New(noopPrinter{}, fetch.New(cfg.Timeout, nil))
	runtime.ArtifactDownloadDeadline = deadline
	return cfg, runtime, s
}

// TestArtifactByteDripFailsAtTheDownloadDeadline asserts a byte-drip fault -
// which always makes progress and so never trips the read-inactivity
// watchdog - is caught by the whole-acquisition download deadline instead,
// failing the run closed rather than hanging it forever, with the artifact
// endpoint hit exactly once.
func TestArtifactByteDripFailsAtTheDownloadDeadline(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newDripFixture(t, true, 500*time.Millisecond, dripWatchdogTimeout)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{DripInterval: 5 * time.Millisecond, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a byte-dripped artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected errors.Is ErrArtifactDownloadDeadline, got %v", err)
	}
	// (c) and (d) are documentary, not pinned: they are structurally
	// unfalsifiable in this fixture - see this file's header for why, and for
	// what would make them falsifiable again.
	if errors.Is(err, helpers.ErrReadStalled) {
		t.Fatalf("expected the drip to defeat the watchdog, not trip it: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}

	if got := runtime.Metrics.Totals().BytesDownloaded; got < 2 {
		t.Errorf("BytesDownloaded = %d, want >= 2 (progress made throughout, the byte-drip signature)", got)
	}
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "solo"))
	// Exactly one artifact request pins terminality: once the acquisition
	// deadline fires, downloadRetryable classifies the sentinel non-retryable
	// and helpers.Retry returns instead of spending the remaining
	// helpers.FetchRetryMaxAttempts-1 attempts. This is deterministic on HEAD
	// - with a watchdog window far above the deadline, a drip can only ever
	// end an attempt by the deadline firing, so no retryable failure exists
	// for the loop to act on.
	//
	// It deliberately does NOT pin where the budget is established. Moving the
	// context.WithTimeout from downloadCollectionToCache into
	// attemptDownloadToCache - a fresh budget per attempt - would still yield
	// a count of 1, because the sentinel is terminal either way. That the
	// budget is one shared acquisition budget is enforced by construction at
	// its single establishment point and documented there; the only behavior
	// that would distinguish the two placements is total elapsed wall clock,
	// which this suite deliberately does not assert on.
	if got := s.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Errorf("EndpointArtifact count = %d, want 1 (the deadline is terminal: no retry follows it)", got)
	}
}

// TestArtifactDripFixtureInstallsWithoutTheFault is the positive control for
// TestArtifactByteDripFailsAtTheDownloadDeadline: the identical fixture, with
// no fault armed, installs successfully - proving the 500ms deadline used
// above is not itself what would fail an ordinary install against this
// fixture.
func TestArtifactDripFixtureInstallsWithoutTheFault(t *testing.T) {
	t.Parallel()
	cfg, runtime, _ := newDripFixture(t, true, 500*time.Millisecond, dripWatchdogTimeout)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "solo")
}

// TestByteDripCostsExactlyTwoAcquisitionsPerCollection pins the disclosed
// residual that one collection against a hostile server costs two full
// acquisition budgets on this path, not one: the prefetcher acquires the
// artifact under its own budget, and - since a prefetch failure is
// deliberately non-fatal (runInstallLevel logs it and proceeds) - the install
// worker then acquires it again under a fresh budget of its own. That
// doubling is exactly why helpers.ArtifactDownloadDeadline is 15 minutes
// rather than 30; see its doc comment. The prefetcher is left enabled here
// (unlike the NoCache fixture above) specifically to exercise both
// acquisitions in the same run.
//
// Like the count assertion in TestArtifactByteDripFailsAtTheDownloadDeadline,
// this pins the number of acquisitions and the terminality of each, not where
// the budget is established: a per-attempt budget would also yield 2, since
// the sentinel is terminal either way.
//
// The three-budget corner - a cache-hit Fetch failing the S3 read-time
// integrity check and driving an evict-and-refetch - is not exercised here:
// this fixture uses the local backend with a cold cache, so the install
// worker's own probe finds nothing to Fetch. It is recorded on
// helpers.ArtifactDownloadDeadline instead.
func TestByteDripCostsExactlyTwoAcquisitionsPerCollection(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newDripFixture(t, false, 400*time.Millisecond, dripWatchdogTimeout)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{DripInterval: 5 * time.Millisecond, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a byte-dripped artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected errors.Is ErrArtifactDownloadDeadline, got %v", err)
	}

	if got := s.Count(fakegalaxy.EndpointArtifact); got != 2 {
		t.Errorf("EndpointArtifact count = %d, want exactly 2 (one prefetch acquisition, one install-path acquisition, "+
			"each terminal once its deadline fires), not 2*helpers.FetchRetryMaxAttempts=%d", got, 2*helpers.FetchRetryMaxAttempts)
	}
}
