package collections_test

// This file (continued from e2e_test.go and retry_e2e_test.go) covers the
// artifact download watchdog end to end: a mid-body stall - the fake server
// writes a real prefix of the artifact, then blocks - must be caught by the
// fetch client's read watchdog, not by hanging forever, whether the stall
// recovers on retry or persists across every retry attempt.

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

// stallTimeout is the watchdog idle window (and http.Client timeout) every
// fixture in this file uses. It also bounds the fetch client's
// ResponseHeaderTimeout (see fetch.New), so the loopback first-byte and
// registered-metadata round trips - both orders of magnitude faster - never
// race against it; only a deliberately stalled artifact body read is meant
// to trip it.
const stallTimeout = 100 * time.Millisecond

// waitBoundStall is the generous upper bound TestParentCancelDuringStallExitsInterrupt
// gives itself to observe the first artifact request and, separately, Start's
// return after cancellation, so a genuine deadlock fails the test instead of
// hanging the suite.
const waitBoundStall = 5 * time.Second

// newStallFixture registers a single acme.solo collection (version "*", no
// dependencies, so exactly one artifact download ever runs) on a fresh fake
// server and returns a matching cold-cache config/runtime pair. NoCache is
// forced on to disable the background prefetcher: without it, a persistently
// stalled download would be attempted once by the prefetcher and again by
// installCollection's own fallback fetch, making the artifact request count
// ambiguous. Workers is forced to 1 since this fixture only ever installs one
// collection. Critically, the runtime's HTTP client is fetch.New(stallTimeout)
// - the real watchdog-wrapped client - not the fake server's own
// httptest.Server.Client(), which carries no read watchdog and would hang
// forever against a mid-body stall.
func newStallFixture(t *testing.T) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
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
		Timeout:          stallTimeout,
		NoCache:          true,
	}
	runtime := infra.New(noopPrinter{}, fetch.New(cfg.Timeout, nil))
	return cfg, runtime, s
}

// TestArtifactStallRecoversOnRetry asserts a one-shot mid-body stall on the
// artifact download is caught by the watchdog - which fires because the
// stalled read never makes progress - rather than hanging the install
// forever, and that the retried attempt completes the download and the
// install successfully.
func TestArtifactStallRecoversOnRetry(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newStallFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{StallAfterBytes: 8, Count: 1})

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "solo")
	// Two artifact requests prove the watchdog fired mid-body on the first
	// (stalled) attempt - otherwise that attempt would hang forever and this
	// test would never reach this assertion - and that the second attempt
	// streamed the full body, verified its sha256, and completed the install.
	if got := s.Count(fakegalaxy.EndpointArtifact); got != 2 {
		t.Errorf("EndpointArtifact count = %d, want 2 (1 stalled + 1 success)", got)
	}
}

// TestArtifactPersistentStallFailsBounded asserts a mid-body stall that
// recurs on every attempt is retried exactly helpers.FetchRetryMaxAttempts
// times - each caught by the watchdog rather than hanging - before the
// install fails closed through the generic aggregated-failure sentinel: the
// per-collection stall is logged and counted inside installLevels, then
// finalizeInstall reports it as helpers.ErrInstallationFailed rather than
// propagating helpers.ErrReadStalled itself.
//
// The context.Canceled and exitcode.FromError assertions below are
// genuinely falsifiable here, unlike their counterparts in
// deadline_e2e_test.go: this fixture's error tree has exactly one cause -
// the persistent stall - so there is no second, mutually exclusive cause
// tree for the fixture itself to rule out; reverting watchdog.go's %v back
// to %w makes both fire directly, with no fixture change needed to make
// that possible (see mixed_fault_e2e_test.go for the shape where a second
// failing collection is what makes the same pair of assertions meaningful).
func TestArtifactPersistentStallFailsBounded(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newStallFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{StallAfterBytes: 8, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a persistently stalled artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	// The watchdog, not the artifact download deadline, must be what ended
	// this run: every attempt here stalls well within the fixture's generous
	// runtime.ArtifactDeadline(), so a failing assertion here would mean the
	// deadline fired first and raced ahead of the watchdog's own bounded
	// retry loop, not that the watchdog itself is broken. Killing mutation:
	// setting this fixture's runtime.ArtifactDownloadDeadline below
	// stallTimeout makes the deadline win that race, and this assertion
	// fires.
	if errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected the watchdog, not the artifact download deadline, to end this run: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitInstall {
		t.Errorf("exitcode.FromError(err) = %d, want ExitInstall (%d)", got, exitcode.ExitInstall)
	}
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "solo"))
	// The test reaching this assertion at all already proves the watchdog
	// fired on every attempt: without it, the very first attempt would
	// deadlock and the test would time out instead of failing cleanly.
	if got := s.Count(fakegalaxy.EndpointArtifact); got != helpers.FetchRetryMaxAttempts {
		t.Errorf("EndpointArtifact count = %d, want helpers.FetchRetryMaxAttempts=%d", got, helpers.FetchRetryMaxAttempts)
	}
}

// TestArtifactStallBytesCountedPerAttemptWithNoMissOnFailure pins two
// metrics invariants together against the same persistent-stall scenario as
// TestArtifactPersistentStallFailsBounded: first, that AddBytesDownloaded is
// recorded once per download attempt - including a failed attempt's partial
// read - so a persistent stall's total is the per-attempt stalled prefix
// times the number of attempts, not the bytes of just one attempt and not
// zero; second, that a download acquisition which never completes
// successfully records no cache miss at all, since AddCacheMiss lives
// outside the retry loop in downloadCollectionToCache and only fires once its
// helpers.Retry call returns with no error - which it never does here.
func TestArtifactStallBytesCountedPerAttemptWithNoMissOnFailure(t *testing.T) {
	t.Parallel()
	const stallAfterBytes = 8 // same K as TestArtifactStallRecoversOnRetry above
	cfg, runtime, s := newStallFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{StallAfterBytes: stallAfterBytes, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a persistently stalled artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}

	totals := runtime.Metrics.Totals()
	wantBytes := int64(stallAfterBytes) * int64(helpers.FetchRetryMaxAttempts)
	if totals.BytesDownloaded != wantBytes {
		t.Errorf("BytesDownloaded = %d, want %d (%d attempts, each stalling after exactly %d real bytes)",
			totals.BytesDownloaded, wantBytes, helpers.FetchRetryMaxAttempts, stallAfterBytes)
	}
	if totals.CacheMisses != 0 {
		t.Errorf("CacheMisses = %d, want 0 (the acquisition never completed successfully, so it never reaches AddCacheMiss)",
			totals.CacheMisses)
	}
}

// TestParentCancelDuringStallExitsInterrupt is the positive control paired
// with TestArtifactPersistentStallFailsBounded above (named there too): the
// same persistent mid-body stall, but this time it is the CALLER's own
// context that ends the run, not the watchdog exhausting its retry budget.
// The result must be a genuine context.Canceled, classifying as
// exitcode.ExitInterrupt - the one case a stalled read must still produce
// that classification, exactly because it was a real cancellation and not
// the watchdog's.
//
// This is not flaky despite racing cancel() against the fake server's own
// goroutine: every interleaving between the poll below observing the first
// artifact request and cancel() actually running agrees on the outcome.
//   - cancel() lands while the artifact body read is already blocked inside
//     serveArtifactStall: this poll fires within ~1-2ms of the request
//     landing, well inside the fixture's 100ms idle window, so the watchdog
//     timer has not fired yet and b.fired is still false. wctx is a child of
//     parentCtx, so canceling parentCtx cancels wctx too, unblocking the
//     read with a raw context.Canceled; Read's error branch short-circuits
//     at `b.fired.Load()` before it ever reaches the
//     `b.parentCtx.Err() == nil` guard, so the raw error passes through
//     unchanged regardless of the guard's presence. (The cell where the
//     guard itself is load-bearing - the watchdog has already fired AND the
//     parent is already canceled - is not reachable from this timing and is
//     pinned separately; see below.)
//   - cancel() lands during a backoff sleep between retry attempts:
//     helpers.Retry's own ctx.Done() select case returns the bare ctx.Err()
//     (context.Canceled) immediately, without ever calling attempt() again.
//   - cancel() lands before the next request is even issued: the collection
//     fails immediately against an already-dead context, which surfaces the
//     same way.
//
// All three converge on the same observable error class, so which one this
// run happens to hit on a given machine does not matter to the assertions
// below. What this test pins is the run-level property: a genuine
// cancellation survives helpers.Retry, the per-collection failure
// aggregation, and exitcode.FromError's ordering, all the way out to
// ExitInterrupt. The `b.parentCtx.Err() == nil` guard itself - the one
// interleaving where the watchdog has already fired before the parent is
// canceled - is pinned by
// TestWatchdogBody_FiredWatchdogYieldsToParentCancel in
// internal/galaxy/fetch/watchdog_test.go, deterministically, with no timing
// race at all.
func TestParentCancelDuringStallExitsInterrupt(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newStallFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "solo", fakegalaxy.Fault{StallAfterBytes: 8, Count: -1})

	ctx, cancel := context.WithCancel(context.Background())
	// Deliberately in addition to the explicit cancel() call below, not
	// instead of it: without this defer, a t.Fatal above the explicit cancel()
	// would exit this test via runtime.Goexit with the stall-faulted request
	// still parked and nothing left to ever cancel it, so fakegalaxy's own
	// t.Cleanup (httptest.Server.Close) would then block forever waiting for
	// that request to finish.
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- collections.Start(ctx, cfg, runtime)
	}()

	// dispatchArtifact increments the endpoint counter before
	// serveArtifactStall ever blocks, so polling Count is a reliable signal
	// that a request has actually reached the fake server - unlike, say,
	// polling for a fixed elapsed duration.
	deadline := time.Now().Add(waitBoundStall)
	for s.Count(fakegalaxy.EndpointArtifact) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Count(fakegalaxy.EndpointArtifact) < 1 {
		t.Fatal("artifact endpoint never received a request before the poll deadline")
	}
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(waitBoundStall):
		t.Fatal("Start did not return after the parent context was canceled")
	}
	if err == nil {
		t.Fatal("expected an error from a canceled stalled artifact download, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is context.Canceled, got %v", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitInterrupt {
		t.Errorf("exitcode.FromError(err) = %d, want ExitInterrupt (%d)", got, exitcode.ExitInterrupt)
	}
}
