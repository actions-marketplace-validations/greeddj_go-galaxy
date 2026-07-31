package collections_test

// This file (continued from e2e_test.go, deadline_e2e_test.go, and
// stall_e2e_test.go) covers the shape a security audit measured: one run
// installing two collections, where one is byte-dripped (ending on
// helpers.ArtifactDownloadDeadline) and the other is mid-body stalled (ending
// on helpers.ErrReadStalled) at the same time. The watchdog renders the
// stalled collection's cause with %v, not %w, so its context.Canceled - the
// cause of the watchdog canceling its own derived context to unblock the
// stuck read - does not reach errors.Is; wrapping it with %w instead would
// let exitcode.FromError's context.Canceled case (checked ahead of every
// other class) match it and misclassify the whole run as ExitInterrupt (130)
// even though the run also carries helpers.ErrArtifactDownloadDeadline, a
// sentinel that has never been reachable via context.Canceled. With %v
// rendering, the mixed run correctly classifies as ExitInstall (5), matching
// every other per-collection failure joined behind
// helpers.ErrInstallationFailed.
//
// Three fixture parameters are load-bearing:
//
//   - DripInterval is 2ms against a 100ms watchdog window (stallTimeout-shaped
//     Timeout here): a 50x margin, so the drip's per-byte progress can never
//     be mistaken for a stall even under -race, where goroutine scheduling is
//     materially slower and jitterier than under a normal build.
//   - The deadline is 3s against the stall's HARD worst case: four attempts
//     (helpers.FetchRetryMaxAttempts) each stalling for the full 100ms idle
//     window before the watchdog trips it, plus a full-jitter backoff sum
//     strictly bounded by its three ceilings, 200+400+800ms (jitter is
//     uniform on [0, ceiling), so each term is a strict upper bound, never an
//     expectation) - 4*100ms + (200+400+800)ms = 400ms + 1400ms = 1800ms
//     absolute maximum. 3s over that 1.8s hard bound is a 1.67x margin.
//   - The stalled collection uses StallAfterBytes, not Hang.
//     deadline_e2e_test.go's header notes Hang also lands on the deadline
//     sentinel, but only in that file's own fixture, where Timeout (10s) is
//     set deliberately far above the deadline. Here Timeout must stay small
//     (100ms) for the stall half of this fixture to ever trip the watchdog
//     at all, so a Hang fault would instead trip fetch.New's
//     ResponseHeaderTimeout (bounded by the same 100ms Timeout) before ever
//     reaching the artifact download deadline. Do not "simplify" this
//     fixture to Hang.
//
// TestMixedDripAndStallDoesNotClassifyAsInterrupt was run against a real
// revert of watchdog.go:102 back to wrapping its cause with %w, and the
// observed failure was:
//
//	mixed_fault_e2e_test.go:146: !errors.Is(err, context.Canceled) failed: err = installation failed for 2 collections
//	mixed_fault_e2e_test.go:149: exitcode.FromError(err) = 130, want != ExitInterrupt (130)
//	mixed_fault_e2e_test.go:152: exitcode.FromError(err) = 130, want ExitInstall (5)
//
// (the summary error's Error() renders only the one-line headline -
// "installation failed for 2 collections" - never the per-collection causes;
// those were already logged to stderr in real time by the worker that hit
// each one, and are reachable programmatically through Unwrap/errors.Is,
// which is exactly what the errors.Is assertions above this file's mutation
// output prove). This confirms both the wrong classification and that it
// fires specifically because the stalled cause, not the deadline cause,
// carries the reachable context.Canceled.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// mixedFaultTimeout is this fixture's watchdog idle window (and http.Client
// timeout). It must stay small enough that the stalled collection's watchdog
// fires well inside the deadline below - see this file's header for the exact
// arithmetic - unlike deadline_e2e_test.go's dripWatchdogTimeout, which is
// deliberately large.
const mixedFaultTimeout = 100 * time.Millisecond

// mixedFaultDeadline is this fixture's artifact download deadline, set with a
// 1.67x margin over the stalled collection's hard worst-case duration; see
// this file's header for the arithmetic.
const mixedFaultDeadline = 3 * time.Second

// newMixedFaultFixture registers two independent collections, acme.drip and
// acme.stall, on a fresh fake server and returns a matching cold-cache
// config/runtime pair with the prefetcher disabled (NoCache: true), so each
// collection is acquired exactly once - the same reasoning newStallFixture
// documents. Workers: 2 lets both collections' installs run concurrently,
// which is what actually produces the mixed error tree this file exists to
// cover: a sequential run would still join both causes, but concurrency is
// what a real CI worker pool actually does.
func newMixedFaultFixture(t *testing.T) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirementsMulti(t, reqPath, "acme.drip", "acme.stall")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "drip", "1.0.0", nil)
	s.AddVersion("acme", "stall", "1.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          2,
		Timeout:          mixedFaultTimeout,
		NoCache:          true,
	}
	runtime := infra.New(noopPrinter{}, fetch.New(cfg.Timeout, nil))
	runtime.ArtifactDownloadDeadline = mixedFaultDeadline
	return cfg, runtime, s
}

// TestMixedDripAndStallDoesNotClassifyAsInterrupt is the mandatory regression
// test for the defect this file's header describes: one byte-dripped
// collection (ends on helpers.ErrArtifactDownloadDeadline) and one
// persistently stalled collection (ends on helpers.ErrReadStalled), run
// together, must classify as ExitInstall, never ExitInterrupt, even though
// the stalled cause's underlying error genuinely is context.Canceled.
func TestMixedDripAndStallDoesNotClassifyAsInterrupt(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newMixedFaultFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "drip", fakegalaxy.Fault{DripInterval: 2 * time.Millisecond, Count: -1})
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "stall", fakegalaxy.Fault{StallAfterBytes: 8, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a mixed drip/stall run, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	// These two prove the fixture actually produced the mixed shape - one
	// cause tree per fault kind - rather than a degenerate run where only one
	// collection's failure survived to the joined error. Without both of
	// these holding, the assertions below would not be falsifiable: a
	// single-cause run could pass them by accident.
	if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected errors.Is ErrArtifactDownloadDeadline, got %v", err)
	}
	if !errors.Is(err, helpers.ErrReadStalled) {
		t.Fatalf("expected errors.Is ErrReadStalled, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("!errors.Is(err, context.Canceled) failed: err = %v", err)
	}
	// The == ExitInterrupt check below is documentary, not an independent
	// pin: it is implied by the != ExitInstall check right after it on the
	// same value. Both are kept - matching exitcode_test.go's own convention
	// for this pair - because together they name the security property this
	// file exists to cover, even though only one of them is load-bearing.
	if got := exitcode.FromError(err); got == exitcode.ExitInterrupt {
		t.Errorf("exitcode.FromError(err) = %d, want != ExitInterrupt (%d)", got, exitcode.ExitInterrupt)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitInstall {
		t.Errorf("exitcode.FromError(err) = %d, want ExitInstall (%d)", got, exitcode.ExitInstall)
	}
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "drip"))
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "stall"))
}

// TestMixedFaultFixtureInstallsWithoutFaults is the positive control for
// TestMixedDripAndStallDoesNotClassifyAsInterrupt: the identical fixture,
// with no fault armed, installs both collections successfully - proving the
// timeout/deadline/worker parameters above are not themselves what would fail
// an ordinary install against this fixture.
func TestMixedFaultFixtureInstallsWithoutFaults(t *testing.T) {
	t.Parallel()
	cfg, runtime, _ := newMixedFaultFixture(t)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "drip")
	assertManifestInstalled(t, cfg.DownloadPath, "stall")
}
