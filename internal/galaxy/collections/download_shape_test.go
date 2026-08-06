package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// downloadShapeFixture wires the download arm that has no extracted store to
// ingest through - the one the prefetcher builds for itself, and the one any
// run without a cache-side extracted store takes - and serves body from a
// local server as the artifact.
//
// meta declares no sha256 at all, deliberately: that is the combination in
// which verifyDownloadSHA compares nothing, so whatever arrives would reach
// the shared cache slot on the strength of having been transferred.
func downloadShapeFixture(t *testing.T, body []byte) (installDeps, *types.GalaxyCollectionVersionInfo, string) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	cfg := &config.Config{CacheDir: cacheDir, Workers: 1, NoDeps: true}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      local.NewArtifacts(cacheDir),
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	return deps, meta, artifactKey(col)
}

// TestDownloadWithoutExtractStoreRejectsNonArchiveBytes proves the shape probe
// keeps bytes that are not an archive out of the shared artifact cache. The
// server answers with an error page rather than a tarball - the everyday shape
// of a misrouted download - and the declared sha is empty, so nothing else on
// this arm would have looked at the bytes at all.
//
// The second assertion is the point of the test: refusing the download while
// still committing it would leave every later consumer of that cache slot to
// discover the same error page for itself.
func TestDownloadWithoutExtractStoreRejectsNonArchiveBytes(t *testing.T) {
	t.Parallel()
	deps, meta, key := downloadShapeFixture(t, []byte("<html>404</html>"))

	ctx := context.Background()
	_, err := downloadCollectionToCache(ctx, deps, key, "", meta, true)

	// Errorf, not Fatalf: the cache assertion below is the one this test exists
	// for, and stopping here would leave it unreached - and therefore unpinned
	// by the mutation quoted on it - whenever the sentinel check is the one
	// that breaks.
	if !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Errorf("downloadCollectionToCache = %v, want errors.Is helpers.ErrArtifactNotTarGz", err)
	}

	// Killing mutation: deleting the archive.ProbeTarGz call from
	// attemptDownloadToCache fails this test on both assertions, the cache one
	// reading `key "e3b0c44298fc.acme-widgets-1.0.0.tar.gz" entered the
	// artifact cache despite not being an archive`. The error page really is
	// committed under that mutation - the refusal and the cache hygiene are
	// one behavior, not a sentinel with a side effect.
	cached, hasErr := deps.artifacts.Has(ctx, key)
	if hasErr != nil {
		t.Fatalf("artifacts.Has: %v", hasErr)
	}
	if cached {
		t.Fatalf("key %q entered the artifact cache despite not being an archive", key)
	}
}

// TestDownloadWithoutExtractStoreCommitsAValidArchive is the positive control
// on the identical fixture: the same arm, the same empty declared sha, and a
// real tarball, which must be committed. Without it, the refusal above would
// be indistinguishable from a fixture that never reaches the commit at all.
func TestDownloadWithoutExtractStoreCommitsAValidArchive(t *testing.T) {
	t.Parallel()
	deps, meta, key := downloadShapeFixture(t, buildMinimalTarGz(t))

	ctx := context.Background()
	result, err := downloadCollectionToCache(ctx, deps, key, "", meta, true)
	if err != nil {
		t.Fatalf("downloadCollectionToCache = %v, want nil", err)
	}
	if result.Cleanup != nil {
		defer result.Cleanup()
	}

	cached, hasErr := deps.artifacts.Has(ctx, key)
	if hasErr != nil {
		t.Fatalf("artifacts.Has: %v", hasErr)
	}
	if !cached {
		t.Fatalf("key %q did not enter the artifact cache", key)
	}
}
