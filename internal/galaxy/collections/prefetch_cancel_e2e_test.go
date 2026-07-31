package collections_test

// This file (continued from e2e_test.go and stall_e2e_test.go) covers the
// prefetcher's cancellable lifecycle: on any early return from runInstall -
// in particular, a level failing before the next level is scheduled - every
// in-flight prefetch download must be canceled and every prefetch worker
// joined before Start returns, so a prefetch worker can never keep running
// (and, worse, keep mutating the shared Store or calling
// backend.Artifacts().Commit) after the backend lock has been released.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// countingRoundTripper wraps an underlying transport and tracks in-flight
// requests: gauge is the number of RoundTrip calls that have entered but not
// yet returned, whether they succeeded, failed, or were aborted by a
// canceled context (the increment happens on entry, the decrement is a
// deferred call so it runs on every exit path). It is the deterministic
// signal these tests use to prove that no HTTP request - in particular, a
// prefetch worker's artifact download - outlives collections.Start; a
// runtime.NumGoroutine() count would be flaky and is deliberately avoided.
type countingRoundTripper struct {
	rt    http.RoundTripper
	gauge atomic.Int32
}

// RoundTrip delegates to rt, bracketing the call with gauge's increment and
// (deferred) decrement.
func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.gauge.Add(1)
	defer c.gauge.Add(-1)
	return c.rt.RoundTrip(req)
}

// newPrefetchCancelFixture registers acme.app@1.0.0 (depending on
// acme.lib>=1.0.0) and acme.lib@1.0.0 (no dependencies) on a fresh fake
// server, matching newE2EFixture's dependency shape, and returns a matching
// cold-cache config/runtime pair whose HTTP client is wrapped by a
// countingRoundTripper. The client deliberately carries no Timeout: it must
// be Close's cancel, not a client-side deadline, that ever unblocks a
// prefetch worker parked in a hung download.
func newPrefetchCancelFixture(t *testing.T) (*config.Config, *infra.Infra, *fakegalaxy.Server, *countingRoundTripper) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.app")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	s.AddVersion("acme", "lib", "1.0.0", nil)

	countingRT := &countingRoundTripper{rt: s.Client().Transport}
	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          4,
	}
	runtime := infra.New(noopPrinter{}, &http.Client{Transport: countingRT})
	return cfg, runtime, s, countingRT
}

// TestPrefetchWorkersJoinedOnLevelFailure is the load-bearing regression
// test for the prefetcher's cancellable lifecycle. acme.lib (install level
// 0, no dependencies) fails every artifact download attempt with a 500,
// exhausting its retries; installLevels breaks before scheduling level 1 and
// so never waits on acme.app's install worker. Meanwhile acme.app's
// prefetch worker - scheduled up front, concurrently with level 0 - has
// already loaded acme.app's metadata successfully and is parked
// indefinitely in its artifact GET (a Hang fault). Only a canceled context
// can ever unblock that worker, since nothing else in this test ever cancels
// or times out the request.
//
// A naive implementation (a bare `go func()` with no cancel and no join)
// would let Start return once level 0's failure is reported, while
// acme.app's prefetch worker is still blocked mid-request: the gauge would
// read 1, not 0, and (worse) t.Cleanup's srv.Close would then block forever
// waiting for that still in-flight request to finish, since nothing would
// ever cancel it. This test discriminates exactly that: it can only pass if
// runInstall's defer chain cancels the prefetcher's context and joins every
// worker before returning.
func TestPrefetchWorkersJoinedOnLevelFailure(t *testing.T) {
	t.Parallel()
	cfg, runtime, s, countingRT := newPrefetchCancelFixture(t)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusInternalServerError, Count: -1})
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "app", fakegalaxy.Fault{Hang: true, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from acme.lib's persistently failing artifact download, got nil")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	// The load-bearing assertion: proves no prefetch HTTP request - in
	// particular, acme.app's hung one - outlived runInstall. Deterministic
	// on the correct implementation, since Close cancels the prefetcher's
	// context and then joins its workers: the canceled worker's RoundTrip
	// returns (running the gauge's deferred decrement) before that worker
	// itself returns, before wg.Wait returns, before Start returns.
	if got := countingRT.gauge.Load(); got != 0 {
		t.Errorf("in-flight HTTP requests immediately after Start returned = %d, want 0", got)
	}
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "app"))
	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "lib"))
}

// TestPrefetchWorkersJoinedOnSuccess asserts the same zero-in-flight-requests
// invariant on a normal, fault-free two-collection install. Unlike
// TestPrefetchWorkersJoinedOnLevelFailure, this test would also pass against
// a naive bare-goroutine implementation with no cancel and no join: with
// nothing hung or canceled, every prefetch worker's own request completes
// and decrements the gauge well before installLevels (let alone Start)
// returns. It is included only as a sanity check on the happy path; the
// level-failure test above is the real guard against a leaked or unjoined
// prefetch worker.
func TestPrefetchWorkersJoinedOnSuccess(t *testing.T) {
	t.Parallel()
	cfg, runtime, _, countingRT := newPrefetchCancelFixture(t)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "app")
	assertManifestInstalled(t, cfg.DownloadPath, "lib")
	if got := countingRT.gauge.Load(); got != 0 {
		t.Errorf("in-flight HTTP requests immediately after Start returned = %d, want 0", got)
	}
}
