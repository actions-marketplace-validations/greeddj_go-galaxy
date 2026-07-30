package collections

// This file pins prefetchOne's metadata-load failure branch (prefetch.go):
// loadCollectionMetadata erroring returns a nil meta and a zero downloadResult
// with no download attempted, and that failure is recorded by finish and
// surfaced through Wait rather than lost. It also proves the install worker
// absorbs a prefetch metadata failure - logging it and reloading metadata
// itself - rather than propagating it as an install failure.
//
// A plain 500 fault would not reach this branch: loadCollectionMetadata's own
// GET retries on a retryable status (helpers.IsRetryableHTTPStatus), so a
// one-shot 500 would be consumed inside that retry loop and never surface as
// an error from loadCollectionMetadata itself. A 404 is not in the retryable
// set, so it fails the version-detail fetch on the first attempt.

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestPrefetchMetadataErrorSurfacedByWait drives startPrefetcher directly
// (bypassing installLevels) against a collection whose version-detail fetch
// always 404s, and asserts prefetchOne's metadata-load failure branch: the
// task is still scheduled and completed (ok == true), its recorded error is
// non-nil, and both its metadata and its downloaded artifact are the zero
// value, since prefetchOne returns before ever reaching the download step.
func TestPrefetchMetadataErrorSurfacedByWait(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	// Persistent (Count: -1): this test only exercises the prefetcher, so
	// nothing else consumes the fault afterward.
	srv.Fail(fakegalaxy.EndpointVersionDetail, "acme", "app", fakegalaxy.Fault{Status: http.StatusNotFound, Count: -1})

	fx := newPrefetchHandoffFixture(t, srv, 1)
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	collections := map[string]collection{col.key(): col}
	graph := map[string][]string{col.key(): {}}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}

	prefetch := startPrefetcher(context.Background(), newPrefetchDeps(fx.cfg, fx.runtime, fx.st, fx.artifacts, fx.root), collections, levels)

	meta, dl, ok, waitErr := prefetch.Wait(col.key())
	if !ok {
		t.Fatalf("Wait ok = false, want true (a prefetch task was scheduled for %s)", col.key())
	}
	if waitErr == nil {
		t.Fatalf("Wait err = nil, want the metadata-load failure recorded by finish")
	}
	if meta != nil {
		t.Fatalf("Wait meta = %+v, want nil (prefetchOne must not return metadata on a metadata-load failure)", meta)
	}
	if dl.Path != "" {
		t.Fatalf("Wait dl.Path = %q, want empty (prefetchOne returns before ever downloading an artifact)", dl.Path)
	}

	prefetch.Close()
}

// TestPrefetchMetadataErrorAbsorbedInstallRecovers proves the recovery
// behavior end to end: a one-shot (Count: 1) 404 on the version-detail
// endpoint is consumed by the prefetcher's own metadata fetch, which runs
// strictly before the install worker's since runInstallLevel always calls
// prefetch.Wait(key) first. The install worker then observes a prefetch
// failure (ok == true, prefetchErr != nil), logs it, and proceeds with a nil
// meta and an empty prefetched handoff, so its own resolveMetadata call
// reloads metadata from scratch - a fresh GET, since the failed prefetch
// fetch was never cached - which this time succeeds because the fault was
// already consumed. The install completes successfully, and exactly two
// version-detail requests were made: the failed prefetch attempt and the
// successful install attempt.
func TestPrefetchMetadataErrorAbsorbedInstallRecovers(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointVersionDetail, "acme", "app", fakegalaxy.Fault{Status: http.StatusNotFound, Count: 1})

	fx := newPrefetchHandoffFixture(t, srv, 1)
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	collections := map[string]collection{col.key(): col}
	graph := map[string][]string{col.key(): {}}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}

	prefetch, failures, err := fx.runLevels(collections, graph, levels)
	if err != nil {
		t.Fatalf("installLevels: %v", err)
	}
	if failures != 0 {
		t.Fatalf("failures = %d, want 0 (the install worker's own metadata reload must recover)", failures)
	}
	assertFileContent(t, filepath.Join(fx.installPath(col), "README.md"), "# acme.app\n")

	if got := srv.Count(fakegalaxy.EndpointVersionDetail); got != 2 {
		t.Fatalf("EndpointVersionDetail count = %d, want 2 (one failed prefetch attempt, one successful install attempt)", got)
	}

	prefetch.Close()
}
