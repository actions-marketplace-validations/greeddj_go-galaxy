package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
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

// newRootMetadataFallThroughServer starts an httptest.Server that answers 404
// for any candidate URL under "/api/v3/" and 200 with v2FallThroughBody for
// any candidate under "/api/v2/" (any other path also 404s). It records
// every request path it receives, in order, guarded by a mutex since the
// handler runs on the server's own goroutine.
func newRootMetadataFallThroughServer(t *testing.T) (*httptest.Server, func() []string) {
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
		case strings.Contains(r.URL.Path, "/api/v2/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(v2FallThroughBody))
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

// TestLoadRootMetadataCachedFallsThroughOn404 asserts that when a collection
// has no explicit source, a 404 on the v3 root-metadata candidate is treated
// as "try the next candidate" rather than a hard failure: the loader must
// advance to the v2 candidate and return its root metadata.
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
