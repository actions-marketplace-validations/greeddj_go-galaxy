package collections_test

// This file (continued from e2e_test.go) covers end-to-end retry behavior
// for the Galaxy API GET and the artifact download, exercised against the
// fake Galaxy server's scripted fault injection (fakegalaxy.Fault) rather
// than against the internal predicates directly.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// newRetryFixture registers a single acme.<name> collection (version "*",
// no dependencies) on a fresh fake server and returns a matching cold-cache
// config/runtime pair. When noCache is true, the background prefetcher (see
// prefetch.go's startPrefetcher) and the local artifact/API cache are
// disabled, so the artifact download has exactly one call site with its own
// internal retry loop; without it, a persistent download failure would be
// attempted once by the prefetcher and then again by installCollection's
// own fallback fetch, making an exact EndpointArtifact request count
// ambiguous. Metadata fixtures instead need caching enabled: dependency
// resolution (resolveCollectionsInternal) fetches each candidate's version
// metadata to build the graph, and installCollection fetches the same URL
// again afterward - with the API cache enabled, that second fetch is served
// from the snapshot populated by the first, keeping the EndpointVersionDetail
// count from this fixture's single fault to a single retry cycle.
func newRetryFixture(t *testing.T, name string, noCache bool) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme."+name)

	s := fakegalaxy.New(t)
	s.AddVersion("acme", name, "1.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
		NoCache:          noCache,
	}
	return cfg, infra.New(noopPrinter{}, s.Client()), s
}

// TestMetadataFetchRetriesTransientFailureThenSucceeds asserts that a Galaxy
// API GET (here, the version-detail fetch) transparently recovers from a
// bounded run of transient 503s, completing the install after exactly three
// requests to the faulted endpoint (two failures plus the succeeding one).
func TestMetadataFetchRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "metaretry", false)
	s.Fail(fakegalaxy.EndpointVersionDetail, "acme", "metaretry", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: 2})

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "metaretry")
	if got := s.Count(fakegalaxy.EndpointVersionDetail); got != 3 {
		t.Errorf("EndpointVersionDetail count = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestArtifactDownloadRetriesTransientFailureThenSucceeds mirrors
// TestMetadataFetchRetriesTransientFailureThenSucceeds for the artifact
// download endpoint.
func TestArtifactDownloadRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "artretry", true)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "artretry", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: 2})

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "artretry")
	if got := s.Count(fakegalaxy.EndpointArtifact); got != 3 {
		t.Errorf("EndpointArtifact count = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestArtifactDownloadRetrySuccessCountsOneMissNotOnePerAttempt pins "one
// miss per acquisition, not per attempt": downloadCollectionToCache retries
// the whole establish+stream+verify attempt inside attemptDownloadToCache up
// to helpers.FetchRetryMaxAttempts times, but AddCacheMiss is called exactly
// once, outside that retry loop, only after the whole retry-bounded
// acquisition finally succeeds. The EndpointArtifact-count-of-3-vs-
// CacheMisses-of-1 relationship asserted below is the whole point of this
// test: it proves the increment lives in downloadCollectionToCache, not in
// attemptDownloadToCache - if AddCacheMiss were moved into
// attemptDownloadToCache (incrementing once per HTTP attempt instead of once
// per successful acquisition), this run would report CacheMisses == 3, not 1.
func TestArtifactDownloadRetrySuccessCountsOneMissNotOnePerAttempt(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "artretry", true)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "artretry", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: 2})

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "artretry")
	if got := s.Count(fakegalaxy.EndpointArtifact); got != 3 {
		t.Errorf("EndpointArtifact count = %d, want 3 (2 failures + 1 success)", got)
	}
	totals := runtime.Metrics.Totals()
	if totals.CacheMisses != 1 {
		t.Errorf("CacheMisses = %d, want 1 (one per retry-bounded acquisition, not one per the 3 HTTP attempts above)", totals.CacheMisses)
	}
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (a fresh origin download is never a cache hit)", totals.CacheHits)
	}
}

// TestMetadataFetchExhaustsRetriesAndFails asserts that an indefinitely
// failing Galaxy API GET is attempted exactly helpers.FetchRetryMaxAttempts
// times before the run fails closed. The fault is armed with a negative
// Count so it fires on every attempt, including the last: a bounded Count
// would be consumed by the retries themselves and could let a later attempt
// succeed, masking the exhaustion this test means to exercise.
//
// Unlike an artifact download (which always fails inside installLevels,
// surfacing as helpers.ErrInstallationFailed), a persistently failing
// metadata GET aborts during dependency resolution - resolveCollectionsInternal
// must fetch each candidate's version metadata to build the graph before
// installLevels ever runs - so the run fails with the underlying
// *cacheManager.HTTPStatusError still reachable via errors.As, wrapped only
// in "failed to resolve dependencies", never helpers.ErrInstallationFailed.
func TestMetadataFetchExhaustsRetriesAndFails(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "metafail", false)
	s.Fail(fakegalaxy.EndpointVersionDetail, "acme", "metafail", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	var statusErr *cacheManager.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected errors.As to *cacheManager.HTTPStatusError, got %v", err)
	}
	if statusErr.Code != http.StatusServiceUnavailable {
		t.Errorf("HTTPStatusError.Code = %d, want %d", statusErr.Code, http.StatusServiceUnavailable)
	}
	if got := s.Count(fakegalaxy.EndpointVersionDetail); got != helpers.FetchRetryMaxAttempts {
		t.Errorf("EndpointVersionDetail count = %d, want helpers.FetchRetryMaxAttempts=%d", got, helpers.FetchRetryMaxAttempts)
	}
}

// TestArtifactDownloadExhaustsRetriesAndFails mirrors
// TestMetadataFetchExhaustsRetriesAndFails for the artifact download
// endpoint.
func TestArtifactDownloadExhaustsRetriesAndFails(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newRetryFixture(t, "artfail", true)
	s.Fail(fakegalaxy.EndpointArtifact, "acme", "artfail", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	if got := s.Count(fakegalaxy.EndpointArtifact); got != helpers.FetchRetryMaxAttempts {
		t.Errorf("EndpointArtifact count = %d, want helpers.FetchRetryMaxAttempts=%d", got, helpers.FetchRetryMaxAttempts)
	}
}
