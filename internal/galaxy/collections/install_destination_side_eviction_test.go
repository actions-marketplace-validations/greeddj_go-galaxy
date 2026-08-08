package collections

// This file pins isDestinationSideFailure: prepareWithRecovery's action arm
// must not spend its bounded evict-and-refetch on a failure whose cause is
// the write destination, not the artifact just verified and extracted. A
// symlinked <namespace> component - "ansible_collections/<ns>" pointing
// outside cfg.DownloadPath - is the fixture: extractCollection's
// target.root.RemoveAll(target.rel) refuses it with
// helpers.ErrCollectionsPathEscape, and no different cached artifact could
// ever make that refusal succeed, so evicting the (perfectly good) cache-hit
// artifact ahead of a refetch that cannot help would just spend a real,
// destructive Delete against shared state for nothing. Both new tests here
// share one counting artifacts wrapper so the "zero evictions" assertion on
// the escape case and the "one eviction" assertion on its positive control
// are produced by the exact same counting mechanism.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// deleteCountingArtifacts wraps a real local.Artifacts store, delegating
// every method unchanged except Delete, which is counted ahead of
// delegating. Used by both tests in this file so a "zero calls" assertion
// and a "one call" assertion are observed through the identical mechanism.
type deleteCountingArtifacts struct {
	*local.Artifacts

	deleteCalls atomic.Int32
}

// Delete counts the call, then delegates to the real local store.
func (a *deleteCountingArtifacts) Delete(ctx context.Context, key string) error {
	a.deleteCalls.Add(1)
	return a.Artifacts.Delete(ctx, key)
}

// newSymlinkedNamespaceInstallDeps builds installDeps rooted at a
// downloadPath whose "ansible_collections/<col.Namespace>" is a symlink to a
// sibling "outside" directory - escaping cfg.DownloadPath the same way
// symlink_escape_test.go's symlinkedEscapeFixture does for a symlinked
// "ansible_collections" itself, one level deeper. The .info sidecar path
// ("ansible_collections/<ns>.<name>-<version>.info") is a sibling of
// "ansible_collections/<ns>", not beneath it, so this fixture is specific to
// extractCollection's target.rel write - the namespace symlink never affects
// writeGalaxyInfo at all.
//
// serverURL and httpClient are threaded through (rather than hardcoding
// http.DefaultClient) so this fixture can also serve a real refetch: without
// isDestinationSideFailure, the mutation this fixture pins reintroduces an
// eviction, and the loop's forced retry needs a real server to fetch
// metadata and artifact bytes from - a nil/absent server would fail that
// retry for an unrelated reason (no metadata endpoint), masking the
// eviction-count regression this test exists to catch behind a different
// error class instead of surfacing it as an extra Delete call.
func newSymlinkedNamespaceInstallDeps(t *testing.T, col collection, cacheDir, serverURL string, httpClient *http.Client) installDeps {
	t.Helper()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	collectionsDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, collectionsDir)
	outside := filepath.Join(root, "outside")
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(collectionsDir, col.Namespace)); err != nil {
		t.Fatalf("symlink ansible_collections/%s -> outside: %v", col.Namespace, err)
	}

	osRoot, err := os.OpenRoot(downloadPath)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = osRoot.Close()
	})

	cfg := &config.Config{Server: serverURL, CacheDir: cacheDir, DownloadPath: downloadPath, Workers: 1, NoDeps: true}
	runtime := infra.New(noopPrinter{}, httpClient)
	artifacts := &deleteCountingArtifacts{Artifacts: local.NewArtifacts(cacheDir)}
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      artifacts,
		root:           osRoot,
	}
}

// TestInstallCollectionNamespaceEscapeEvictsNothing is the load-bearing proof
// for isDestinationSideFailure: a cache-hit artifact whose extraction fails
// only because the namespace component is a symlink escaping
// cfg.DownloadPath must fail the collection without ever calling
// artifacts.Delete. Without isDestinationSideFailure,
// prepareWithRecovery's action arm would evict on every action failure
// unclassified, which for this specific cause spends a real, destructive
// Delete against shared cache state on every affected run - and then
// forces a real refetch that hits the identical symlinked-namespace
// refusal again, still failing the collection, so the escape's error class
// alone cannot tell the classified behavior apart from the unclassified
// one. A real fakegalaxy server is wired in specifically so that
// distinction is only visible through the Delete counter: as classified,
// this test's own error assertion and its zero-Delete assertion agree; if
// isDestinationSideFailure's classification were dropped and the action arm
// evicted unconditionally, the error assertion would still pass - the
// second attempt fails the exact same way, since no artifact ever repairs a
// destination-side problem - while the eviction-count assertion is the only
// one that catches the regression.
func TestInstallCollectionNamespaceEscapeEvictsNothing(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	cacheDir := t.TempDir()
	deps := newSymlinkedNamespaceInstallDeps(t, col, cacheDir, srv.URL(), srv.Client())

	// The seeded content is irrelevant to this test - extractCollection's
	// RemoveAll refuses before a single byte of it is ever read - so it need
	// not match what the fake server would actually serve, only enough for
	// Has()/Fetch() to report a cache hit at all on the first attempt.
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("stand-in for a perfectly good cached artifact"))

	err := installCollection(context.Background(), col, deps, nil, nil, downloadResult{})
	if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
		t.Fatalf("installCollection error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
	}

	artifacts, ok := deps.artifacts.(*deleteCountingArtifacts)
	if !ok {
		t.Fatalf("deps.artifacts = %T, want *deleteCountingArtifacts", deps.artifacts)
	}
	if got := artifacts.deleteCalls.Load(); got != 0 {
		t.Fatalf("Delete calls = %d, want 0: a destination-side failure must never evict the cache-hit artifact", got)
	}
	// The artifact itself must still be there, corroborating the counter: no
	// tarball or sidecar was removed from the shared cache.
	assertExists(t, artifactPath)
}

// TestInstallCollectionCacheHitExtractFailureEvictsOneWithCountingArtifacts
// is the positive control TestInstallCollectionNamespaceEscapeEvictsNothing
// needs: the identical deleteCountingArtifacts wrapper, driven by an
// artifact-side extract failure (a corrupt cached tarball, the shape
// TestInstallCollectionCacheHitExtractFailureRefetchesOnce already covers
// with the real local.Artifacts type) instead of a destination-side one,
// must count exactly one Delete call. Without this, "zero calls" on the
// namespace-escape test would be unfalsifiable - a counting wrapper whose
// Delete this whole recovery path never actually reaches would report zero
// either way, for the wrong reason.
func TestInstallCollectionCacheHitExtractFailureEvictsOneWithCountingArtifacts(t *testing.T) {
	t.Parallel()
	validTarGz := buildMinimalTarGz(t)
	correctSHA := sha256Hex(validTarGz)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(validTarGz)
	}))
	defer server.Close()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, cacheDir)

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	// Not a valid gzip stream, despite what the sidecar below claims - this is
	// what makes extraction (an artifact-side failure) fail.
	mustWriteFile(t, artifactPath, []byte("not a gzip stream, despite what the sidecar claims"))
	mustWriteFile(t, artifactPath+helpers.ArtifactSHASidecarSuffix, []byte(correctSHA))

	meta := newVersionInfo(server.URL, "")
	meta.Artifact.Sha256 = correctSHA

	cfg := &config.Config{CacheDir: cacheDir, DownloadPath: downloadPath, Workers: 1, NoDeps: true}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	artifacts := &deleteCountingArtifacts{Artifacts: local.NewArtifacts(cacheDir)}
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      artifacts,
		extractStore:   extracted.NewStore(cacheDir),
		root:           newTestCollectionsRoot(t, downloadPath),
	}

	if err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{}); err != nil {
		t.Fatalf("expected the corrupt cache hit to recover via a single refetch, got %v", err)
	}
	if got := artifacts.deleteCalls.Load(); got != 1 {
		t.Fatalf("Delete calls = %d, want exactly 1 (an artifact-side failure must still evict-and-refetch)", got)
	}
}
