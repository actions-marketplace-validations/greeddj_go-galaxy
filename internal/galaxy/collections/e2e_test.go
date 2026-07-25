// Package collections_test exercises the real install pipeline end-to-end
// against an in-memory fake Galaxy server (github.com/greeddj/go-galaxy/
// internal/testing/fakegalaxy), driving collections.Start exactly the way a
// CI job would: a cold network install, a warm cache-served reinstall, a
// frozen lockfile-pinned install (both honoring and rejecting a pin), and an
// offline install with the network transport hard-disabled.
package collections_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// e2eTimeout is the HTTP client timeout every fixture in this file uses: long
// enough that it is never the reason a test fails, short enough that a real
// hang would still fail fast.
const e2eTimeout = 30 * time.Second

// corruptedAppSHA256 is a well-formed but deliberately wrong sha256 hex
// string, used to simulate a lockfile whose pin no longer matches the
// artifact it names.
const corruptedAppSHA256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// noopPrinter is a minimal output.Printer stub, mirroring the one the
// collections package's own internal tests use, so this external test
// package renders no progress output while still satisfying every method
// Start's runtime needs.
type noopPrinter struct{}

func (noopPrinter) Printf(string, ...any)                 {}
func (noopPrinter) PersistentPrintf(string, ...any)       {}
func (noopPrinter) Okf(string, ...any)                    {}
func (noopPrinter) Errorf(string, ...any)                 {}
func (noopPrinter) Warnf(string, ...any)                  {}
func (noopPrinter) Debugf(string, ...any)                 {}
func (noopPrinter) DebugSincef(time.Time, string, ...any) {}

// e2eFixture bundles one scenario's fake server, configuration, and runtime.
// Every fixture registers the same two collections - acme.app@1.0.0
// depending on acme.lib>=1.0.0, and acme.lib@1.0.0 with no dependencies - and
// a requirements.yml requiring acme.app at any version, so scenarios differ
// only in the config/runtime knobs they flip and how many times they call
// collections.Start.
type e2eFixture struct {
	server       *fakegalaxy.Server
	cfg          *config.Config
	runtime      *infra.Infra
	appV1        fakegalaxy.Version
	libV1        fakegalaxy.Version
	downloadPath string
}

// newE2EFixture builds a fresh fake server and a matching cold cache/install
// pair rooted under t.TempDir, wired for an online install against the fake
// server's own client.
func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.app")

	s := fakegalaxy.New(t)
	appV1 := s.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	libV1 := s.AddVersion("acme", "lib", "1.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}

	return &e2eFixture{
		server:       s,
		cfg:          cfg,
		runtime:      infra.New(noopPrinter{}, s.Client()),
		appV1:        appV1,
		libV1:        libV1,
		downloadPath: downloadPath,
	}
}

// writeRequirements writes a minimal requirements.yml at path requiring name
// at version "*", the map-item shape requirements.LoadCollections parses.
func writeRequirements(t *testing.T, path, name string) {
	t.Helper()
	content := "collections:\n  - name: " + name + "\n    version: \"*\"\n"
	if err := os.WriteFile(path, []byte(content), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
}

// installPathFor returns where acme.name lands under downloadPath, matching
// the fixed ansible_collections layout Start installs into. Every collection
// this file registers lives under the "acme" namespace, so it is fixed here
// rather than threaded through as a parameter every caller would pass the
// same value for.
func installPathFor(downloadPath, name string) string {
	return filepath.Join(downloadPath, "ansible_collections", "acme", name)
}

// manifestPathFor returns acme.name's installed MANIFEST.json path under
// downloadPath.
func manifestPathFor(downloadPath, name string) string {
	return filepath.Join(installPathFor(downloadPath, name), "MANIFEST.json")
}

// assertManifestInstalled fails the test unless acme.name's MANIFEST.json
// exists under downloadPath.
func assertManifestInstalled(t *testing.T, downloadPath, name string) {
	t.Helper()
	path := manifestPathFor(downloadPath, name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected acme.%s installed, MANIFEST.json missing at %s: %v", name, path, err)
	}
}

// assertPathAbsent fails the test if path exists, or if stat-ing it fails
// for any reason other than its absence.
func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s to not exist, but it does", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error on %s: %v", path, err)
	}
}

// readManifestVersion reads and parses the collection_info.version field of
// an installed acme.name collection's MANIFEST.json.
func readManifestVersion(t *testing.T, downloadPath, name string) string {
	t.Helper()
	path := manifestPathFor(downloadPath, name)
	data, err := os.ReadFile(path) //nolint:gosec // path is built from this test's own temp dirs.
	if err != nil {
		t.Fatalf("read MANIFEST.json at %s: %v", path, err)
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse MANIFEST.json at %s: %v", path, err)
	}
	return manifest.CollectionInfo.Version
}

// TestFreshInstallDownloadsFromNetwork asserts a cold-cache install against a
// fake Galaxy server succeeds, installs every collection in the dependency
// graph, and actually hits the network to do so.
func TestFreshInstallDownloadsFromNetwork(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := f.server.Count(fakegalaxy.EndpointArtifact); got < 2 {
		t.Errorf("EndpointArtifact count = %d, want at least 2 (a real network install happened)", got)
	}
}

// TestWarmInstallServesFromCache asserts that reinstalling into a wiped
// download path, with the same cache directory already populated by a prior
// run, succeeds entirely from the persisted snapshot and local artifact
// cache without a single HTTP request.
func TestWarmInstallServesFromCache(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}

	f.server.ResetCounts()
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath to force a reinstall from cache: %v", err)
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (warm cache): %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after the warm reinstall = %d, want 0 (snapshot- and cache-served, no HTTP at all)", got)
	}
}

// TestFrozenInstallHonorsLockfilePins asserts --frozen installs pin exactly
// the version and artifact a lockfile names, even when a higher version is
// available, and that a corrupted pin fails the run closed rather than
// installing drifted bytes. Its two subtests intentionally run sequentially,
// not in parallel with each other: the corrupted-pin subtest reuses and
// mutates the same lockfile the pin-overrides subtest already wrote, so this
// test itself does not call t.Parallel either, keeping that dependency
// explicit rather than racing the two subtests against each other.
func TestFrozenInstallHonorsLockfilePins(t *testing.T) {
	f := newE2EFixture(t)
	// Registered after the fixture's own acme.app@1.0.0, so an unpinned
	// resolution would prefer this one - proving the lockfile pin, not
	// "highest available", drives a frozen install.
	f.server.AddVersion("acme", "app", "2.0.0", map[string]string{"acme.lib": ">=1.0.0"})

	lockPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.app", Version: "1.0.0", Source: f.cfg.Server, SHA256: f.appV1.SHA256, Deps: []string{"acme.lib"}},
			{Name: "acme.lib", Version: "1.0.0", Source: f.cfg.Server, SHA256: f.libV1.SHA256},
		},
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	f.cfg.Frozen = true

	t.Run("pin overrides the highest available version", func(t *testing.T) {
		assertFrozenPinOverridesHighestVersion(t, f)
	})
	t.Run("a corrupted pin fails the run without installing", func(t *testing.T) {
		assertFrozenCorruptedPinFailsClosed(t, f, lockPath, lf)
	})
}

// assertFrozenPinOverridesHighestVersion runs a frozen install and asserts it
// installs the lockfile's pinned acme.app@1.0.0 - not the higher 2.0.0 also
// registered on the server - without ever listing versions.
func assertFrozenPinOverridesHighestVersion(t *testing.T, f *e2eFixture) {
	t.Helper()
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := readManifestVersion(t, f.downloadPath, "app"); got != "1.0.0" {
		t.Errorf("installed acme.app collection_info.version = %q, want %q (lockfile pin over the 2.0.0 highest)", got, "1.0.0")
	}
	if got := f.server.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Errorf("EndpointVersionsList count = %d, want 0 (frozen resolution never consults the versions listing)", got)
	}
}

// assertFrozenCorruptedPinFailsClosed corrupts acme.app's pin in lf, saves it
// at lockPath, and asserts the resulting frozen install fails the whole run
// rather than installing bytes that no longer match what the lockfile names.
func assertFrozenCorruptedPinFailsClosed(t *testing.T, f *e2eFixture, lockPath string, lf *lockfile.File) {
	t.Helper()
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the corrupted-pin run: %v", err)
	}
	for i := range lf.Collections {
		if lf.Collections[i].Name == "acme.app" {
			lf.Collections[i].SHA256 = corruptedAppSHA256
		}
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrupted lockfile: %v", err)
	}

	err := collections.Start(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from a corrupted lockfile pin, got nil")
	}
	// Start aggregates per-collection install failures behind
	// helpers.ErrInstallationFailed rather than propagating the triggering
	// cause (installLevels only logs the underlying helpers.ErrSHA256Mismatch
	// through the printer); this is the sentinel that actually reaches this
	// call site.
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
}

// TestOfflineInstall asserts --offline refuses to reach the network on a
// cold cache, and installs entirely from a warm cache with the network
// transport hard-disabled.
func TestOfflineInstall(t *testing.T) {
	t.Parallel()

	t.Run("cold cache rejects the network", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.Offline = true
		f.runtime = infra.New(noopPrinter{}, fetch.NewOffline(f.cfg.Timeout))

		err := collections.Start(context.Background(), f.cfg, f.runtime)
		if err == nil {
			t.Fatal("expected an offline-mode error on a cold cache, got nil")
		}
		if !errors.Is(err, helpers.ErrOfflineMode) {
			t.Fatalf("expected errors.Is ErrOfflineMode, got %v", err)
		}
		if got := f.server.Total(); got != 0 {
			t.Errorf("Total() = %d, want 0 (the offline transport never dials)", got)
		}
	})

	t.Run("warm cache installs with the network hard disabled", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)

		if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("first Start (populate the cache online): %v", err)
		}

		f.server.ResetCounts()
		f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)
		f.cfg.Offline = true
		if err := os.RemoveAll(f.downloadPath); err != nil {
			t.Fatalf("remove downloadPath to force a reinstall: %v", err)
		}

		if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("second Start (offline warm reinstall): %v", err)
		}

		assertManifestInstalled(t, f.downloadPath, "app")
		assertManifestInstalled(t, f.downloadPath, "lib")
		if got := f.server.Total(); got != 0 {
			t.Errorf("Total() after the offline warm reinstall = %d, want 0", got)
		}
	})
}

// TestNoDepsUnpinnedInstallsResolvedVersion asserts that --no-deps resolves
// an unpinned root to a concrete version - the highest registered - rather
// than keeping the literal "*" constraint as the collection's Version. That
// concrete version must then flow, as the single source of truth, into the
// installed MANIFEST.json, the artifact cache key on disk, and a
// subsequently generated lockfile entry.
func TestNoDepsUnpinnedInstallsResolvedVersion(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.solo")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "solo", "1.0.0", nil)
	s.AddVersion("acme", "solo", "2.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
		NoDeps:           true,
	}
	runtime := infra.New(noopPrinter{}, s.Client())

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := readManifestVersion(t, downloadPath, "solo"); got != "2.0.0" {
		t.Errorf("installed acme.solo collection_info.version = %q, want %q (the highest registered)", got, "2.0.0")
	}

	assertNoWildcardArtifactFilenames(t, cacheDir)
	assertArtifactFilePresent(t, cacheDir, "acme-solo-2.0.0.tar.gz")

	if err := collections.Lock(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	entry := findLockEntry(t, lf, "acme.solo")
	if entry.Version != "2.0.0" {
		t.Errorf("lockfile entry version = %q, want %q, not the literal %q constraint", entry.Version, "2.0.0", "*")
	}
}

// assertNoWildcardArtifactFilenames walks cacheDir recursively and fails the
// test if any file name contains a percent-escaped or literal "*" - the
// telltale sign of an unresolved "*" version constraint having leaked into an
// artifact cache key (see artifactKey, which url.QueryEscapes the
// namespace-name-version filename).
func assertNoWildcardArtifactFilenames(t *testing.T, cacheDir string) {
	t.Helper()
	err := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.Contains(name, "%2A") || strings.Contains(name, "*") {
			t.Errorf("found wildcard artifact filename %q under %s", name, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cacheDir %s: %v", cacheDir, err)
	}
}

// assertArtifactFilePresent fails the test unless a file named filename
// exists directly under cacheDir, where the local artifact backend keys a
// cached tarball by its (query-escaped) filename.
func assertArtifactFilePresent(t *testing.T, cacheDir, filename string) {
	t.Helper()
	path := filepath.Join(cacheDir, filename)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected cached artifact %s to exist, stat error: %v", path, err)
	}
}

// findLockEntry returns the lockfile.Entry named name from lf, failing the
// test if no such entry exists.
func findLockEntry(t *testing.T, lf *lockfile.File, name string) lockfile.Entry {
	t.Helper()
	for _, e := range lf.Collections {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("lockfile has no entry named %q", name)
	return lockfile.Entry{}
}

// writeRequirementsMulti writes a requirements.yml at path listing every name
// in names at version "*" - the multi-collection variant of
// writeRequirements, used to force requirements to change between two runs
// that share a cache.
func writeRequirementsMulti(t *testing.T, path string, names ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("collections:\n")
	for _, name := range names {
		b.WriteString("  - name: ")
		b.WriteString(name)
		b.WriteString("\n    version: \"*\"\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
}

// TestNoDepsSnapshotNotReusedByDepsRun proves the full-match snapshot-reuse
// path (loadResolvedFromSnapshot, gated by RequirementsHash) cannot serve a
// --no-deps snapshot - roots only, nil graph edges - to a later run that
// resolves the full dependency graph. Before the fix, requirementsSignatureFromSpec
// did not encode the --no-deps mode, so the second run's identical (in every
// field the old signature hashed) requirements matched the stored hash and
// reused the --no-deps graph verbatim, silently skipping acme.lib.
func TestNoDepsSnapshotNotReusedByDepsRun(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.NoDeps = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (--no-deps, populate the snapshot): %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))

	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the deps-following run: %v", err)
	}
	f.cfg.NoDeps = false

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (deps-following, must not reuse the --no-deps snapshot): %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
}

// TestNoDepsSnapshotNotReusedIncrementally proves the incremental snapshot-
// reuse path (tryIncrementalResolve) cannot preserve a --no-deps root's
// nil-deps graph entry across a mode change either. Adding acme.tool as a
// second, previously-unseen root alongside the unchanged acme.app root is
// what routes resolution through tryIncrementalResolve rather than
// loadResolvedFromSnapshot: before the fix, tryIncrementalResolve checked
// only per-root spec equality against RequirementsSnapshot and never
// consulted RequirementsHash, so it happily preserved acme.app's --no-deps
// (nil-deps) snapshot entry, silently skipping acme.lib.
func TestNoDepsSnapshotNotReusedIncrementally(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.server.AddVersion("acme", "tool", "1.0.0", nil)
	f.cfg.NoDeps = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (--no-deps, populate the snapshot): %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))

	writeRequirementsMulti(t, f.cfg.RequirementsFile, "acme.app", "acme.tool")
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the deps-following run: %v", err)
	}
	f.cfg.NoDeps = false

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (deps-following, must not incrementally reuse the --no-deps snapshot): %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	assertManifestInstalled(t, f.downloadPath, "tool")
}
