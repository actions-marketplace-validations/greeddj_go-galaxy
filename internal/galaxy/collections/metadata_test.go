package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// v2FallThroughVersionsURL is a marker only the v2 candidate's response body
// carries, so a test can prove the returned root came from that candidate
// and not from any other one that might otherwise satisfy the request.
const v2FallThroughVersionsURL = "http://v2-candidate.example/versions/"

// v2FallThroughBody is a minimal but valid types.GalaxyCollection root
// metadata document, just enough for loadRootMetadataCached to unmarshal and
// return without error.
const v2FallThroughBody = `{"versions_url":"` + v2FallThroughVersionsURL +
	`","highest_version":{"href":"http://v2-candidate.example/versions/9.9.9/","version":"9.9.9"}}`

// newFallThroughServer starts an httptest.Server that answers 404 for any
// candidate URL under "/api/v3/" and 200 with body for any candidate under
// successMatch - checked only once the "/api/v3/" case has not already
// matched, so successMatch may itself be a substring of an "/api/v3/" path
// (e.g. "/v3/") without misrouting one of those 404s into a false success -
// any other path also 404s. It records every request path it receives, in
// order, guarded by a mutex since the handler runs on the server's own
// goroutine. Shared by newRootMetadataFallThroughServer (the standard
// galaxy.ansible.com v3-then-v2 fallback) and newHubShapedServer (the Galaxy
// NG / Automation Hub v3-then-bare-v3 fallback), which differ only in which
// candidate succeeds and with what body.
func newFallThroughServer(t *testing.T, successMatch, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		switch {
		case strings.Contains(r.URL.Path, "/api/v3/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, successMatch):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	return srv, seen
}

// newRootMetadataFallThroughServer starts an httptest.Server that answers 404
// for any candidate URL under "/api/v3/" and 200 with v2FallThroughBody for
// any candidate under "/api/v2/" (any other path also 404s).
func newRootMetadataFallThroughServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	return newFallThroughServer(t, "/api/v2/", v2FallThroughBody)
}

// hubShapeVersionsURL is a marker only the hub-shaped bare-/v3 candidate's
// response body carries, so a test can prove the returned root came from
// that candidate and not from any other one that might otherwise satisfy
// the request.
const hubShapeVersionsURL = "http://hub-shape-candidate.example/versions/"

// hubShapeBody is a minimal but valid types.GalaxyCollection root metadata
// document, just enough for loadRootMetadataCached to unmarshal and return
// without error.
const hubShapeBody = `{"versions_url":"` + hubShapeVersionsURL +
	`","highest_version":{"href":"http://hub-shape-candidate.example/versions/9.9.9/","version":"9.9.9"}}`

// newHubShapedServer starts an httptest.Server simulating a Galaxy NG /
// Automation Hub deployment whose v3 API is mounted directly under its own
// base path - "<base>/v3/collections/...", not the galaxy.ansible.com shape
// "<base>/api/v3/collections/..." - by 404ing any candidate under "/api/v3/"
// and answering any candidate under "/v3/" (that is not also under
// "/api/v3/") with hubShapeBody. This shape cannot be reproduced with
// fakegalaxy.NewAtBasePath: that server's routing unconditionally requires
// the literal segments "api", "v3", "collections" adjacent to each other
// regardless of its configured base path, so it can only ever serve
// "<prefix>/api/v3/collections/...", never the bare "<prefix>/v3/collections/..."
// this apiRootCandidates fallback exists for.
func newHubShapedServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	return newFallThroughServer(t, "/v3/", hubShapeBody)
}

// TestLoadRootMetadataCachedResolvesHubShapedBase asserts that a base whose
// v3 API is mounted directly under its own path (no "/api" immediately
// before "/v3", the Galaxy NG / Automation Hub shape) resolves via the bare
// "/v3" fallback candidate rather than failing outright.
func TestLoadRootMetadataCachedResolvesHubShapedBase(t *testing.T) {
	t.Parallel()
	srv, seenPaths := newHubShapedServer(t)

	cfg := &config.Config{Server: srv.URL}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("expected the hub-shaped base to resolve via the bare /v3 candidate, got error: %v", err)
	}
	if root == nil {
		t.Fatal("expected a non-nil root, got nil")
	}
	if root.VersionsURL != hubShapeVersionsURL {
		t.Fatalf("expected the root served by the bare /v3 candidate (versions_url=%q), got versions_url=%q",
			hubShapeVersionsURL, root.VersionsURL)
	}

	var winningPath string
	for _, p := range seenPaths() {
		if strings.Contains(p, "/v3/") && !strings.Contains(p, "/api/v3/") {
			winningPath = p
			break
		}
	}
	if winningPath == "" {
		t.Fatalf("expected a request to hit the bare /v3 candidate, got requests: %v", seenPaths())
	}
}

// TestLoadRootMetadataCachedFallsThroughOn404ToHubShape asserts the fallback
// mechanics behind TestLoadRootMetadataCachedResolvesHubShapedBase: the
// candidate walk tries the galaxy.ansible.com-shaped "/api/v3" candidate
// first (and 404s), then falls through to the bare "/v3" candidate that
// succeeds, rather than skipping straight to it or failing on the first 404.
func TestLoadRootMetadataCachedFallsThroughOn404ToHubShape(t *testing.T) {
	t.Parallel()
	srv, seenPaths := newHubShapedServer(t)

	cfg := &config.Config{Server: srv.URL}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	paths := seenPaths()
	if len(paths) == 0 {
		t.Fatal("expected the server to have received at least one request")
	}
	if !strings.Contains(paths[0], "/api/v3/") {
		t.Fatalf("expected the first request to hit the galaxy.ansible.com-shaped /api/v3 candidate, got %q (all: %v)",
			paths[0], paths)
	}
}

// TestLoadRootMetadataCachedGalaxyShapeResolvesOnFirstCandidateNoRegression
// is the no-regression guard for this unit's change: a galaxy.ansible.com-shaped
// base (its v3 API mounted at "<base>/api/v3", the shape apiRootCandidates
// has always tried first) must still resolve on the very first candidate,
// with exactly the same request count as before the Galaxy NG / Automation
// Hub fallback candidates existed. The common case must never pay for the
// new candidates.
func TestLoadRootMetadataCachedGalaxyShapeResolvesOnFirstCandidateNoRegression(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	cfg := &config.Config{Server: srv.URL()}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root == nil {
		t.Fatal("expected a non-nil root, got nil")
	}
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 1 {
		t.Fatalf("expected exactly 1 root metadata request for the galaxy.ansible.com shape, got %d", got)
	}
}

// TestLoadRootMetadataCachedFallsThroughOn404 asserts that a 404 on the v3
// root-metadata candidate is treated as "try the next candidate" rather than
// a hard failure: the loader must advance to the v2 candidate and return its
// root metadata. This holds regardless of whether the collection carries an
// explicit source.
func TestLoadRootMetadataCachedFallsThroughOn404(t *testing.T) {
	t.Parallel()
	srv, seenPaths := newRootMetadataFallThroughServer(t)

	cfg := &config.Config{Server: srv.URL}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("expected the 404 on v3 to fall through to v2, got error: %v", err)
	}
	if root == nil {
		t.Fatal("expected a non-nil root, got nil")
	}
	if root.VersionsURL != v2FallThroughVersionsURL {
		t.Fatalf("expected the root served by the v2 candidate (versions_url=%q), got versions_url=%q",
			v2FallThroughVersionsURL, root.VersionsURL)
	}

	paths := seenPaths()
	if len(paths) == 0 {
		t.Fatal("expected the server to have received at least one request")
	}
	if !strings.Contains(paths[0], "/api/v3/") {
		t.Fatalf("expected the first request to hit the v3 candidate, got %q (all requests: %v)", paths[0], paths)
	}

	var hitV2 bool
	for _, p := range paths {
		if strings.Contains(p, "/api/v2/") {
			hitV2 = true
			break
		}
	}
	if !hitV2 {
		t.Fatalf("expected a request to hit the v2 candidate after the v3 404, got requests: %v", paths)
	}
}

// TestLoadRootMetadataCachedAllCandidates404ReturnsLastError asserts that
// when every root-metadata candidate 404s, loadRootMetadataCached returns
// the last 404 (rather than nil or helpers.ErrLoadMetadataFailed), so the
// caller's error message still reflects the real upstream response.
func TestLoadRootMetadataCachedAllCandidates404ReturnsLastError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Server: srv.URL}
	col := collection{Namespace: "acme", Name: "gadgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err == nil {
		t.Fatal("expected an error when every candidate 404s, got nil")
	}
	if root != nil {
		t.Fatalf("expected a nil root on failure, got %+v", root)
	}

	var statusErr *cacheManager.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected errors.As to *cacheManager.HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.Code != http.StatusNotFound {
		t.Fatalf("expected status code %d, got %d", http.StatusNotFound, statusErr.Code)
	}
}

// TestLoadRootMetadataCachedMemoizesWinningAPIRootAcrossCollections asserts
// the memoization behavior: once a phase's apiRootMemo learns a server's
// winning API root from one collection, a second collection under the same
// server base must not re-probe the losing v3 candidate at all. The first
// collection legitimately probes both of the v3 apiRoot's trailing-slash
// variants (both 404) before falling through to v2 and succeeding, so the
// v3 request count is captured after the first fetch rather than hardcoded;
// what matters is that the count does not grow at all after the second
// fetch - i.e. zero further v3 requests once the memo is populated.
func TestLoadRootMetadataCachedMemoizesWinningAPIRootAcrossCollections(t *testing.T) {
	t.Parallel()
	var v3Requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/v3/"):
			v3Requests.Add(1)
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/api/v2/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(v2FallThroughBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	// One collectionDeps (and therefore one apiRootMemo) is shared across
	// both fetches below, mirroring how a single phase's memo is shared by
	// value-copy across every worker resolving collections on the same server.
	deps := newCollectionDeps(cfg, runtime, store.New())

	first := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	if _, err := loadRootMetadataCached(context.Background(), deps, first, cacheManager.Policy{}); err != nil {
		t.Fatalf("first collection: unexpected error: %v", err)
	}
	afterFirst := v3Requests.Load()
	if afterFirst == 0 {
		t.Fatal("expected the first collection to probe v3 at least once before falling through to v2")
	}

	second := collection{Namespace: "acme", Name: "gadgets", Version: "1.0.0"}
	if _, err := loadRootMetadataCached(context.Background(), deps, second, cacheManager.Policy{}); err != nil {
		t.Fatalf("second collection: unexpected error: %v", err)
	}
	if afterSecond := v3Requests.Load(); afterSecond != afterFirst {
		t.Fatalf(
			"expected no additional v3 probes for a second collection under the same server "+
				"(the memoized apiRoot should skip the v3 probe entirely): had %d after the first fetch, %d after the second",
			afterFirst, afterSecond,
		)
	}
}

// TestLoadRootMetadataCachedNonNotFoundErrorAbortsImmediately asserts the
// other half of the candidate-walk rule: a non-404 error is not something a
// different apiRoot could route around, so it aborts the whole walk on the
// very first candidate instead of falling through to the remaining
// v3/v2/bare-API candidates. This holds regardless of whether the collection
// carries an explicit source - unlike the now-removed hasExplicitSource
// short-circuit, both cases share this one rule. The response uses 400
// rather than a transient status such as 500, since helpers.
// IsRetryableHTTPStatus would otherwise make fetchJSONWithCachePolicy retry
// the same candidate URL up to FetchRetryMaxAttempts times, which would
// inflate the request count this test asserts on without probing any
// further candidate.
func TestLoadRootMetadataCachedNonNotFoundErrorAbortsImmediately(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Server: srv.URL}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root, err := loadRootMetadataCached(context.Background(), deps, col, cacheManager.Policy{})
	if err == nil {
		t.Fatal("expected an error from the 400 response, got nil")
	}
	if root != nil {
		t.Fatalf("expected a nil root on failure, got %+v", root)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request (no probing further candidates after a non-404 error), got %d", got)
	}
}
