package collections

// This file is the load-bearing proof for the prefetch temp-path handoff: the
// artifact a prefetch worker already downloaded is handed to the install
// worker via prefetcher.Wait and reused directly, instead of being fetched a
// second time from the artifact store. It exercises installLevels and
// startPrefetcher directly - both package-internal - against a stub
// cacheManager.ArtifactStore that reproduces the S3 backend's own artifact
// semantics (see internal/cache/s3/artifacts.go): Commit uploads a temp's
// bytes but does not consume the temp file, and Fetch always re-downloads
// into a fresh one. That stub's own S3 client test double lives, unexported,
// in internal/cache/s3's _test.go files and cannot be imported here, which is
// why this stub exists instead.
//
// Driving installLevels directly (rather than through collections.Start)
// also means every test here uses a real extracted.Store, so the reused
// prefetched temp still passes through the same content-addressable ingest
// path - and its verifyTarballSHA check - as any other artifact.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// wrongPinSHA256 is a well-formed but deliberately incorrect sha256 hex
// string, standing in for a lockfile pin that does not match the real
// artifact - see TestPrefetchedArtifactFailsClosedOnPinMismatch.
const wrongPinSHA256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// s3StyleArtifacts is a stub cacheManager.ArtifactStore reproducing the S3
// backend's own artifact semantics: Commit "uploads" a temp file's bytes into
// bucketDir but leaves the temp itself in place (mirroring the real S3
// backend, whose Commit uploads and returns the same tmpPath), and Fetch
// always downloads a fresh copy into a new temp under tmpBase - the very
// second-download this stub exists to let a test detect. Every Fetch and
// every Cleanup call is counted per artifact key, so a test can assert
// whether the object store was ever hit again and whether a given temp was
// released exactly once.
type s3StyleArtifacts struct {
	fetchCount   map[string]int
	cleanupCount map[string]int
	hasCount     map[string]int
	committed    map[string]string
	tmpBase      string
	bucketDir    string
	// commitOrder records the key argument of every Commit call in call order,
	// letting a test observe the sequence in which artifacts were committed to
	// the stub bucket - the signal TestPrefetchQueueOrderedByLevel uses to prove
	// the prefetch queue is level-ordered rather than raced in map order.
	commitOrder []string
	mu          sync.Mutex
}

// newS3StyleArtifacts builds an s3StyleArtifacts rooted at two fresh
// directories under t.TempDir(): tmpBase stands in for wherever an S3 backend
// stages its local temps, bucketDir stands in for the S3 bucket itself.
func newS3StyleArtifacts(t *testing.T) *s3StyleArtifacts {
	t.Helper()
	root := t.TempDir()
	tmpBase := filepath.Join(root, "tmp")
	bucketDir := filepath.Join(root, "bucket")
	if err := os.MkdirAll(tmpBase, helpers.DirMod); err != nil {
		t.Fatalf("mkdir tmpBase: %v", err)
	}
	if err := os.MkdirAll(bucketDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir bucketDir: %v", err)
	}
	return &s3StyleArtifacts{
		tmpBase:      tmpBase,
		bucketDir:    bucketDir,
		fetchCount:   make(map[string]int),
		cleanupCount: make(map[string]int),
		hasCount:     make(map[string]int),
		committed:    make(map[string]string),
	}
}

// Has reports whether key has already been committed to the stub bucket,
// counting the call (keyed by key) so a test can assert exactly how many Has
// probes a given key saw - the signal Test C uses to prove the prefetch
// scan's single probe replaced the old scan-plus-reprobe pair.
func (a *s3StyleArtifacts) Has(_ context.Context, key string) (bool, error) {
	a.mu.Lock()
	a.hasCount[key]++
	a.mu.Unlock()
	_, err := os.Stat(filepath.Join(a.bucketDir, key))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// TempFile stages a fresh temp file under tmpBase, mirroring the real S3
// backend's own TempFile.
func (a *s3StyleArtifacts) TempFile(_ context.Context, prefix string) (*os.File, func(), error) {
	f, err := os.CreateTemp(a.tmpBase, prefix)
	if err != nil {
		return nil, nil, err
	}
	path := f.Name()
	return f, func() { _ = os.Remove(path) }, nil
}

// Commit "uploads" tmpPath's bytes into the stub bucket under key without
// consuming tmpPath, exactly like the real S3 backend's Commit. Its returned
// Cleanup counts the call (keyed by key) before removing tmpPath, so a test
// can assert a temp - whether consumed by an install worker or reclaimed by
// prefetcher.Close - was released exactly once.
func (a *s3StyleArtifacts) Commit(_ context.Context, key, tmpPath string, meta map[string]string) (cacheManager.ArtifactFile, error) {
	if err := copyFile(tmpPath, filepath.Join(a.bucketDir, key)); err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	a.mu.Lock()
	a.committed[key] = tmpPath
	a.commitOrder = append(a.commitOrder, key)
	a.mu.Unlock()
	return cacheManager.ArtifactFile{Path: tmpPath, Meta: meta, Cleanup: a.countingCleanup(key, tmpPath)}, nil
}

// Fetch counts one object-store GET for key - the signal this stub exists to
// let a test observe - then copies the bucket object into a fresh temp file,
// matching the real S3 backend's own re-download-on-Fetch behavior.
func (a *s3StyleArtifacts) Fetch(_ context.Context, key string) (cacheManager.ArtifactFile, error) {
	a.mu.Lock()
	a.fetchCount[key]++
	a.mu.Unlock()
	f, err := os.CreateTemp(a.tmpBase, "fetch-")
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return cacheManager.ArtifactFile{}, err
	}
	if err := copyFile(filepath.Join(a.bucketDir, key), path); err != nil {
		_ = os.Remove(path)
		return cacheManager.ArtifactFile{}, err
	}
	return cacheManager.ArtifactFile{Path: path, Cleanup: a.countingCleanup(key, path)}, nil
}

// Delete removes key's object from the stub bucket, ignoring an
// already-absent object exactly like a real backend's idempotent delete.
func (a *s3StyleArtifacts) Delete(_ context.Context, key string) error {
	err := os.Remove(filepath.Join(a.bucketDir, key))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// countingCleanup returns a Cleanup func that increments key's cleanup
// counter before removing path.
func (a *s3StyleArtifacts) countingCleanup(key, path string) func() {
	return func() {
		a.mu.Lock()
		a.cleanupCount[key]++
		a.mu.Unlock()
		_ = os.Remove(path)
	}
}

// fetchCountFor reports how many times Fetch has been called for key.
func (a *s3StyleArtifacts) fetchCountFor(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fetchCount[key]
}

// cleanupCountFor reports how many times a Cleanup returned for key has run.
func (a *s3StyleArtifacts) cleanupCountFor(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cleanupCount[key]
}

// hasCountFor reports how many times Has has been called for key.
func (a *s3StyleArtifacts) hasCountFor(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hasCount[key]
}

// committedPath returns the tmpPath Commit last recorded for key, or "" if
// key was never committed.
func (a *s3StyleArtifacts) committedPath(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.committed[key]
}

// commitOrderSnapshot returns a copy of the keys passed to Commit, in call
// order, so a test can assert on commit sequencing without racing a
// concurrent Commit call.
func (a *s3StyleArtifacts) commitOrderSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	order := make([]string, len(a.commitOrder))
	copy(order, a.commitOrder)
	return order
}

// copyFile copies src's bytes to dst, creating (or truncating) dst with
// helpers.FileMod permissions.
func copyFile(src, dst string) error {
	//nolint:gosec // both paths are this test's own temp dirs.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	//nolint:gosec // dst is this test's own temp dir.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, helpers.FileMod)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// assertPathAbsent fails the test unless path does not exist.
func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s to be absent", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error on %s: %v", path, err)
	}
}

// prefetchHandoffFixture bundles the pieces every test in this file needs to
// drive installLevels directly against a stub S3-style artifact store and a
// real content-addressable extracted store.
type prefetchHandoffFixture struct {
	cfg          *config.Config
	runtime      *infra.Infra
	st           *store.Store
	artifacts    *s3StyleArtifacts
	extractStore *extracted.Store
	// root is the real collections root opened for cfg.DownloadPath, threaded
	// into every newPrefetchDeps/installLevels call this fixture drives - both
	// now build an installTarget from it on every collection.
	root *os.Root
}

// newPrefetchHandoffFixture builds a fixture wired to srv with workers
// installer/prefetch workers, NoDeps set purely for symmetry with the other
// fixtures in this package (this file only asserts the temp-handoff behavior,
// not dependency resolution).
func newPrefetchHandoffFixture(t *testing.T, srv *fakegalaxy.Server, workers int) *prefetchHandoffFixture {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	cfg := &config.Config{
		Server:       srv.URL(),
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      workers,
		NoDeps:       true,
	}
	return &prefetchHandoffFixture{
		cfg:          cfg,
		runtime:      infra.New(noopPrinter{}, srv.Client()),
		st:           store.New(),
		artifacts:    newS3StyleArtifacts(t),
		extractStore: extracted.NewStore(cacheDir),
		root:         newTestCollectionsRoot(t, downloadPath),
	}
}

// installPath returns the ansible_collections install path installLevels
// would use for col.
func (f *prefetchHandoffFixture) installPath(col collection) string {
	return filepath.Join(f.cfg.DownloadPath, "ansible_collections", col.Namespace, col.Name)
}

// runLevels starts a prefetcher for collections and drives installLevels over
// graph/levels against it, returning the prefetcher still open - the caller
// must Close it once done asserting on the artifact store's state - alongside
// installLevels' own results.
func (f *prefetchHandoffFixture) runLevels(
	collections map[string]collection,
	graph map[string][]string,
	levels [][]string,
) (*prefetcher, int32, error) {
	prefetch := startPrefetcher(context.Background(), newPrefetchDeps(f.cfg, f.runtime, f.st, f.artifacts, f.root), collections, levels)
	failures, err := installLevels(
		context.Background(),
		f.cfg,
		f.runtime,
		f.st,
		f.artifacts,
		f.extractStore,
		collections,
		graph,
		levels,
		prefetch,
		f.root,
	)
	return prefetch, failures, err
}

// TestPrefetchedArtifactReusedNotRefetched is the headline proof for the
// prefetch temp-path handoff: the artifact the prefetcher already downloaded
// for acme.app is reused by the install worker rather than fetched a second
// time from the artifact store.
//
// Discrimination: on the pre-handoff code, installCollection had no way to
// learn about the prefetcher's already-downloaded temp. prepareInstall would
// see meta already set (from prefetch.Wait's old two-value signature) and
// isCacheHit true (Has() reports the prefetcher's own Commit), taking the
// cache-hit branch and calling artifacts.Fetch - so fetchCountFor would read
// 1, not 0. This test's fetchCountFor(key) == 0 assertion is exactly the line
// that would fail without this change.
//
// The hasCountFor(key) == 1 assertion below additionally proves the scan/
// re-probe consolidation: buildPrefetchTasks' parallel scan issues the sole
// Has probe for this key, prefetchOne no longer re-probes it, and the install
// worker never probes a prefetched key either (that path was already removed
// earlier). With the prefetchOne re-probe still present this would read 2.
func TestPrefetchedArtifactReusedNotRefetched(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

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
		t.Fatalf("failures = %d, want 0", failures)
	}
	assertFileContent(t, filepath.Join(fx.installPath(col), "README.md"), "# acme.app\n")

	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Fatalf("EndpointArtifact count = %d, want 1 (downloaded once, by the prefetcher)", got)
	}
	key := artifactKey(col)
	if got := fx.artifacts.fetchCountFor(key); got != 0 {
		t.Fatalf("fetchCount = %d, want 0 (the prefetched temp must be reused, not refetched)", got)
	}
	if got := fx.artifacts.hasCountFor(key); got != 1 {
		t.Fatalf("hasCount = %d, want exactly 1 (one scan probe, no prefetchOne re-probe, no install-worker probe)", got)
	}

	prefetch.Close()
	if got := fx.artifacts.cleanupCountFor(key); got != 1 {
		t.Fatalf("cleanupCount = %d, want exactly 1 (consumed once by installCollection, not double-removed by Close)", got)
	}
}

// TestUnconsumedPrefetchTempReclaimedOnLevelFailure proves the other half of
// the ownership contract: a prefetched artifact whose install level never
// ran - because an earlier level failed and installLevels broke before
// scheduling it - is still reclaimed exactly once, by Close, rather than
// leaking on disk.
func TestUnconsumedPrefetchTempReclaimedOnLevelFailure(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	srv.AddVersion("acme", "lib", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusInternalServerError, Count: -1})

	fx := newPrefetchHandoffFixture(t, srv, 4)
	app := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	lib := collection{Namespace: "acme", Name: "lib", Version: "1.0.0"}
	collections := map[string]collection{app.key(): app, lib.key(): lib}
	graph := map[string][]string{
		app.key(): {lib.key()},
		lib.key(): {},
	}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}
	if len(levels) != 2 || len(levels[0]) != 1 || levels[0][0] != lib.key() {
		t.Fatalf("unexpected levels, want [[lib],[app]]: %#v", levels)
	}

	prefetch, failures, err := fx.runLevels(collections, graph, levels)
	if err != nil {
		t.Fatalf("installLevels: %v", err)
	}
	if failures == 0 {
		t.Fatalf("expected failures > 0 from acme.lib's persistently failing artifact download")
	}

	appKey := artifactKey(app)
	appTemp := fx.artifacts.committedPath(appKey)
	if appTemp == "" {
		t.Fatalf("expected the prefetcher to have committed acme.app's artifact before level 1 was reached")
	}
	assertExists(t, appTemp)

	prefetch.Close()
	assertReclaimedExactlyOnce(t, fx.artifacts, appKey, appTemp)

	// acme.lib's own artifact GET failed with a 500 before ever reaching
	// writeDownloadToTemp, so its prefetch never produced a temp to reclaim.
	libKey := artifactKey(lib)
	if got := fx.artifacts.cleanupCountFor(libKey); got != 0 {
		t.Fatalf("lib cleanupCount = %d, want 0 (its prefetch download never produced a temp)", got)
	}

	assertPathAbsent(t, fx.installPath(app))
	assertPathAbsent(t, fx.installPath(lib))
}

// assertReclaimedExactlyOnce fails the test unless key's temp at path was
// cleaned up exactly once (by Close's drain of an unconsumed prefetch, in
// this file's tests) and no longer exists on disk.
func assertReclaimedExactlyOnce(t *testing.T, artifacts *s3StyleArtifacts, key, path string) {
	t.Helper()
	if got := artifacts.cleanupCountFor(key); got != 1 {
		t.Fatalf("%s cleanupCount = %d, want exactly 1 (reclaimed once by Close)", key, got)
	}
	assertPathAbsent(t, path)
}

// TestPrefetchedArtifactFailsClosedOnPinMismatch proves the reused prefetched
// temp still goes through the same verifyPinnedSHA lockfile pin verification
// as any other artifact: a lockfile pin that does not match the real
// (prefetched) bytes fails the install closed, with no futile refetch from
// the object store.
func TestPrefetchedArtifactFailsClosedOnPinMismatch(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	fx := newPrefetchHandoffFixture(t, srv, 1)
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", SHA256: wrongPinSHA256}
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
	if failures == 0 {
		t.Fatalf("expected failures > 0 from the pin mismatch")
	}
	assertPathAbsent(t, fx.installPath(col))

	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Fatalf("EndpointArtifact count = %d, want 1 (a single origin download, no futile refetch)", got)
	}
	key := artifactKey(col)
	if got := fx.artifacts.fetchCountFor(key); got != 0 {
		t.Fatalf("fetchCount = %d, want 0 (a pin mismatch must fail closed without hitting the object store)", got)
	}

	prefetch.Close()
	if got := fx.artifacts.cleanupCountFor(key); got != 1 {
		t.Fatalf("cleanupCount = %d, want exactly 1", got)
	}
}

// TestInstallCollectionSkipReleasesPrefetchedTempExactlyOnce proves the other
// half of the prefetch handoff's ownership contract at the installCollection
// level: the canSkipInstall branch (install.go, immediately after the
// "already installed" log line) releases a prefetched artifact's temp
// exactly once rather than leaking it until Close.
//
// This drives installCollection directly rather than through installLevels
// because the triggering condition - a prefetched collection that turns out
// to already be installed by the time its own worker call happens - reduces,
// at the installCollection level, to exactly two preconditions: canSkipInstall
// observes an already-installed entry, and the caller still hands in a
// non-empty prefetched downloadResult. Reproducing the condition end to end
// would require a collection that is simultaneously a prefetched top-level
// entry and a dependency some other collection's own MANIFEST-driven install
// already installed first; asserting the two preconditions directly is the
// precise, minimal way to pin down this branch's behavior.
func TestInstallCollectionSkipReleasesPrefetchedTempExactlyOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	cfg := &config.Config{DownloadPath: downloadPath}
	target := newTestInstallTarget(t, cfg, col)
	const installedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if err := os.MkdirAll(target.path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir installPath: %v", err)
	}
	seedValidExtractMarker(t, target, installedSHA)
	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir infoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), []byte("format_version: 1.0.0\n"), helpers.FileMod); err != nil {
		t.Fatalf("write GALAXY.yml: %v", err)
	}

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})
	deps := installDeps{collectionDeps: collectionDeps{
		cfg:     cfg,
		runtime: infra.New(noopPrinter{}, http.DefaultClient),
		st:      st,
	}, root: target.root}

	tempPath := filepath.Join(t.TempDir(), "prefetched.tar.gz")
	if err := os.WriteFile(tempPath, []byte("prefetched tarball bytes"), helpers.FileMod); err != nil {
		t.Fatalf("seed prefetched temp: %v", err)
	}
	var cleanupCount atomic.Int32
	prefetched := downloadResult{
		Path: tempPath,
		SHA:  installedSHA,
		Cleanup: func() {
			cleanupCount.Add(1)
			_ = os.Remove(tempPath)
		},
	}

	if err := installCollection(context.Background(), col, deps, nil, nil, prefetched); err != nil {
		t.Fatalf("installCollection: %v", err)
	}

	if got := cleanupCount.Load(); got != 1 {
		t.Fatalf("cleanupCount = %d, want exactly 1 (released once by the skip branch, not leaked, not double-removed)", got)
	}
	assertPathAbsent(t, tempPath)
}
