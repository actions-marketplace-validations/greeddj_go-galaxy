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
