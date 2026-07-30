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
	lockPath, lf := newFrozenPinFixture(t, f)

	t.Run("pin overrides the highest available version", func(t *testing.T) {
		assertFrozenPinOverridesHighestVersion(t, f)
	})
	t.Run("a corrupted pin fails the run without installing", func(t *testing.T) {
		assertFrozenCorruptedPinFailsClosed(t, f, lockPath, lf)
	})
}

// newFrozenPinFixture registers acme.app@2.0.0 on f.server (after the
// fixture's own acme.app@1.0.0, so an unpinned resolution would prefer this
// one - proving a lockfile pin, not "highest available", drives a frozen
// run), writes a lockfile pinning acme.app@1.0.0 and acme.lib@1.0.0 to their
// real sha256 sums, and sets f.cfg.Frozen. Shared by the install- and
// warm-side frozen-pin e2e tests, which otherwise differ only in which
// collections.* entry point they drive.
func newFrozenPinFixture(t *testing.T, f *e2eFixture) (string, *lockfile.File) {
	t.Helper()
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
	return lockPath, lf
}

// setAppPin sets acme.app's SHA256 pin in-place within lf.Collections.
// Shared by the install- and warm-side corrupted-pin e2e tests.
func setAppPin(lf *lockfile.File, sha string) {
	for i := range lf.Collections {
		if lf.Collections[i].Name == "acme.app" {
			lf.Collections[i].SHA256 = sha
		}
	}
}

// assertFrozenPinOverridesHighestVersion runs a frozen install and asserts it
// installs the lockfile's pinned acme.app@1.0.0 - not the higher 2.0.0 also
// registered on the server - without ever listing versions, and that a
// successful frozen install's metrics report still claims "frozen": true.
// Nothing else in the suite pins this: install and warm pass cfg.Frozen
// through to writeRunMetrics, and this is the regression net proving that
// still happens, mirrored by TestLockFrozenIsIgnoredAndNotReported's negative
// counterpart in lock_command_test.go, which proves lock never does.
func assertFrozenPinOverridesHighestVersion(t *testing.T, f *e2eFixture) {
	t.Helper()
	// f.cfg is shared with the corrupted-pin subtest that runs right after
	// this one (see TestFrozenInstallHonorsLockfilePins), so MetricsFile is
	// set here and restored afterward rather than being added to the shared
	// fixture, leaving it exactly as newFrozenPinFixture/newE2EFixture left it.
	f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")
	defer func() { f.cfg.MetricsFile = "" }()

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := readManifestVersion(t, f.downloadPath, "app"); got != "1.0.0" {
		t.Errorf("installed acme.app collection_info.version = %q, want %q (lockfile pin over the 2.0.0 highest)", got, "1.0.0")
	}
	if got := f.server.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Errorf("EndpointVersionsList count = %d, want 0 (frozen resolution never consults the versions listing)", got)
	}

	data, err := os.ReadFile(f.cfg.MetricsFile)
	if err != nil {
		t.Fatalf("read metrics file %s: %v", f.cfg.MetricsFile, err)
	}
	// Unmarshal into a map, not metrics.Report, for the same reason
	// TestArtifactMetricsWrittenToMetricsFile above does: the literal wire
	// key is what a CI dashboard actually parses.
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", f.cfg.MetricsFile, err)
	}
	if got, _ := written["frozen"].(bool); !got {
		t.Errorf("metrics frozen = %v, want true (a successful frozen install honors the lockfile)", written["frozen"])
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
	setAppPin(lf, corruptedAppSHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrupted lockfile: %v", err)
	}

	err := collections.Start(context.Background(), f.cfg, f.runtime)
	if err == nil {
		t.Fatal("expected an error from a corrupted lockfile pin, got nil")
	}
	// Start aggregates per-collection install failures behind
	// helpers.ErrInstallationFailed, but the triggering cause is no longer
	// swallowed: it is joined into the same error tree (see failureSummary),
	// so both the aggregate classification and the actual
	// helpers.ErrSHA256Mismatch cause are reachable through errors.Is at this
	// call site, one level above cmd/go-galaxy/exitcode where the latter maps
	// to the dedicated integrity exit code rather than the generic install one.
	// Verified against a real revert of failureSummary.wrap (dropping the
	// per-collection cause, returning headline unchanged): that mutation makes
	// the errors.Is(err, helpers.ErrSHA256Mismatch) assertion below fail with:
	// "expected errors.Is ErrSHA256Mismatch, got installation failed for 1 collections"
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
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
	assertArtifactFilePresent(t, cacheDir, s.URL(), "acme-solo-2.0.0.tar.gz")

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

// assertArtifactFilePresent fails the test unless a cache entry for filename,
// scoped to source (the server the collection resolved from - see
// helpers.ArtifactKey), exists directly under cacheDir.
func assertArtifactFilePresent(t *testing.T, cacheDir, source, filename string) {
	t.Helper()
	path := filepath.Join(cacheDir, helpers.ArtifactKey(source, filename))
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

// TestArtifactMetricsColdInstallCountsMisses asserts a cold-cache install
// against a fake Galaxy server counts one cache miss per artifact actually
// downloaded from the origin, no hits at all, and a positive number of bytes
// downloaded.
func TestArtifactMetricsColdInstallCountsMisses(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	totals := f.runtime.Metrics.Totals()
	if totals.CacheMisses != 2 {
		t.Errorf("CacheMisses = %d, want 2 (acme.app and acme.lib, both fetched from the origin)", totals.CacheMisses)
	}
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (a cold cache serves no hits)", totals.CacheHits)
	}
	if totals.BytesDownloaded <= 0 {
		t.Errorf("BytesDownloaded = %d, want > 0 (both artifacts were actually read off the network)", totals.BytesDownloaded)
	}
}

// TestArtifactMetricsWrittenToMetricsFile asserts that the counters a cold
// install accumulates in-process actually reach cfg.MetricsFile on disk with
// the wire-contract key names a CI dashboard reads, not just the in-memory
// runtime.Metrics.Totals() other tests in this file check. A cold install is
// the required scenario, not a warm one: it is the only one where CacheHits
// (0) and CacheMisses (2) are distinguishable from each other, so a swapped
// field mapping in writeRunMetrics's Report literal - CacheHits written where
// CacheMisses belongs, or vice versa - actually flips the file's values
// instead of leaving them coincidentally equal. A warm run, where both
// figures could plausibly collide, would not catch that class of bug and
// would make this test vacuous.
func TestArtifactMetricsWrittenToMetricsFile(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	// Set on f.cfg for this test only, never on the shared fixture: every
	// other counter test in this file reuses newE2EFixture without reading a
	// metrics file, and giving every one of them a MetricsFile would make
	// them all start writing files nothing ever reads.
	f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(f.cfg.MetricsFile)
	if err != nil {
		t.Fatalf("read metrics file %s: %v", f.cfg.MetricsFile, err)
	}
	// Unmarshal into a map, not metrics.Report: decoding into the struct
	// would resolve field names through the very same json tags the marshal
	// side used, so a renamed or swapped tag would round-trip invisibly.
	// Reading the literal wire keys pins the on-disk contract a consuming CI
	// dashboard actually parses, independent of the Go struct's field names.
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", f.cfg.MetricsFile, err)
	}

	totals := f.runtime.Metrics.Totals()
	// Each assertMetricCounter call does both halves required to catch a
	// swapped field mapping: file-vs-Totals (catches a stale read or the
	// wrong runtime) and file-vs-absolute-value (catches the mapping swap
	// itself, which the file-vs-Totals half alone cannot - both sides of
	// that comparison would swap together and still agree).
	assertMetricCounter(t, written, "cache_hits", totals.CacheHits, 0)
	assertMetricCounter(t, written, "cache_misses", totals.CacheMisses, 2)

	gotBytes := metricFloat(t, written, "bytes_downloaded")
	if int64(gotBytes) != totals.BytesDownloaded {
		t.Errorf("metrics file bytes_downloaded = %v, want %d (runtime.Metrics.Totals().BytesDownloaded)", gotBytes, totals.BytesDownloaded)
	}
	if gotBytes <= 0 {
		t.Errorf("metrics file bytes_downloaded = %v, want > 0 (both artifacts were actually read off the network)", gotBytes)
	}
}

// metricFloat extracts the JSON number stored at key in written, failing the
// test if the key is missing or not a number. encoding/json decodes every
// JSON number into a map[string]any as float64; a cache-hit/miss count of 2
// and a download size of a few KB are both far inside float64's exact-integer
// range (2^53), so converting the result to int64 for comparison is exact,
// not approximate.
func metricFloat(t *testing.T, written map[string]any, key string) float64 {
	t.Helper()
	v, ok := written[key].(float64)
	if !ok {
		t.Fatalf("metrics file %s = %v (%T), want a JSON number", key, written[key], written[key])
	}
	return v
}

// assertMetricCounter cross-checks the JSON metrics file's value at key
// against both the in-process totals value (a stale-read/wrong-runtime
// check) and its expected absolute value (a swapped-field-mapping check).
func assertMetricCounter(t *testing.T, written map[string]any, key string, wantTotals, wantAbsolute int64) {
	t.Helper()
	got := int64(metricFloat(t, written, key))
	if got != wantTotals {
		t.Errorf("metrics file %s = %d, want %d (runtime.Metrics.Totals())", key, got, wantTotals)
	}
	if got != wantAbsolute {
		t.Errorf("metrics file %s = %d, want %d", key, got, wantAbsolute)
	}
}

// TestArtifactMetricsWarmInstallCountsHits asserts a warm-cache reinstall
// into a wiped download path counts one cache hit per artifact served from
// the artifact cache, no misses, and zero bytes downloaded, since a cache hit
// reads no bytes from a Galaxy origin's artifact response body.
func TestArtifactMetricsWarmInstallCountsHits(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}

	f.server.ResetCounts()
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath to force a reinstall from cache: %v", err)
	}
	// Rebuild the runtime so its Metrics starts fresh for the warm reinstall:
	// there is no Reset method, and reusing f.runtime across two Start calls
	// would let the cold run's misses bleed into this run's totals.
	f.runtime = infra.New(noopPrinter{}, f.server.Client())

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (warm cache): %v", err)
	}

	totals := f.runtime.Metrics.Totals()
	if totals.CacheHits != 2 {
		t.Errorf("CacheHits = %d, want 2 (acme.app and acme.lib, both served from the artifact cache)", totals.CacheHits)
	}
	if totals.CacheMisses != 0 {
		t.Errorf("CacheMisses = %d, want 0 (a warm reinstall never reaches the origin)", totals.CacheMisses)
	}
	if totals.BytesDownloaded != 0 {
		t.Errorf("BytesDownloaded = %d, want 0 (a cache hit reads no bytes from a Galaxy origin's artifact response body)",
			totals.BytesDownloaded)
	}
}

// TestArtifactMetricsPrefetchHandoffCountedOnce asserts that a cold install's
// prefetch handoff (payloadFromPrefetched) contributes no separate miss of
// its own: CacheMisses must equal exactly the number of EndpointArtifact
// requests the fake server actually served, proving the miss is counted once,
// at the prefetcher's own download, not again when installCollection consumes
// the handed-off artifact. The CacheHits == 0 assertion is what makes this
// test self-supporting rather than vacuous: payloadFromPrefetched reports
// servedFromCache as false and never touches ArtifactStore.Fetch, so an
// install worker that actually consumed the handoff records no hit at all.
// If prefetching were silently disabled (or an install worker ignored the
// handoff and re-fetched from the cache the prefetcher had just populated),
// that worker would find the artifact the prefetcher already committed and
// serve a HIT instead - so a zero hit count is the proof the handed-off bytes
// were the ones actually installed, not that prefetching ran at all in name
// only.
func TestArtifactMetricsPrefetchHandoffCountedOnce(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantMisses := int64(f.server.Count(fakegalaxy.EndpointArtifact))
	totals := f.runtime.Metrics.Totals()
	if totals.CacheMisses != wantMisses {
		t.Errorf("CacheMisses = %d, want %d (one per EndpointArtifact request; the prefetch handoff must not add a second one)",
			totals.CacheMisses, wantMisses)
	}
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (a consumed prefetch handoff never calls ArtifactStore.Fetch, so it can never register a hit)",
			totals.CacheHits)
	}
}

// TestArtifactMetricsLockCountsNothing asserts collections.Lock reports all
// three artifact counters as zero: runLock resolves and writes a lockfile
// without ever touching an ArtifactStore, so this is a truthful zero, not a
// gap in coverage.
func TestArtifactMetricsLockCountsNothing(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	totals := f.runtime.Metrics.Totals()
	if totals.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 (Lock never touches an ArtifactStore)", totals.CacheHits)
	}
	if totals.CacheMisses != 0 {
		t.Errorf("CacheMisses = %d, want 0 (Lock never touches an ArtifactStore)", totals.CacheMisses)
	}
	if totals.BytesDownloaded != 0 {
		t.Errorf("BytesDownloaded = %d, want 0 (Lock never touches an ArtifactStore)", totals.BytesDownloaded)
	}
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
