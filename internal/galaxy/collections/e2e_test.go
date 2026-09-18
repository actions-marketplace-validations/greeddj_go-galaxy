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
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
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

// testVersion100 is the version every fixture in this file registers first,
// mirroring the internal collections package's own constant of the same
// name (unreachable from here across the package boundary, hence the
// duplicate declaration rather than a shared import).
const testVersion100 = "1.0.0"

// noopPrinter is a minimal output.Printer stub, mirroring the one the
// collections package's own internal tests use, so this external test
// package renders no progress output while still satisfying every method
// Start's runtime needs.
type noopPrinter struct{}

func (noopPrinter) Printf(string, ...any)                        {}
func (noopPrinter) PersistentPrintf(string, ...any)              {}
func (noopPrinter) Okf(string, ...any)                           {}
func (noopPrinter) OkVersionf(string, string, ...any)            {}
func (noopPrinter) Updatef(string, ...any)                       {}
func (noopPrinter) Errorf(string, ...any)                        {}
func (noopPrinter) ErrorVersionf(string, string, string, ...any) {}
func (noopPrinter) Warnf(string, ...any)                         {}
func (noopPrinter) Debugf(string, ...any)                        {}
func (noopPrinter) DebugSincef(time.Time, string, ...any)        {}

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
	appV1 := s.AddVersion("acme", "app", testVersion100, map[string]string{"acme.lib": ">=1.0.0"})
	libV1 := s.AddVersion("acme", "lib", testVersion100, nil)

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
// at version "*", the map-item shape requirements.Load parses.
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

// TestInstallReportNamesTheResolvedVersion proves both halves of the install
// report name the version the run settled on: the success line for the
// collection that installed, and the failure line for the one whose artifact
// the server refused. Asserted on the same run, so neither result can be the
// fixture, and asserted on the whole failure line rather than on a substring
// of it, since where the version sits is the point - behind the cause it
// would read as part of the error text.
//
// The version is the one the solver chose rather than one the requirements
// spelled: requirements.yml asks for "*" here.
func TestInstallReportNamesTheResolvedVersion(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	printer := &lineCapturingPrinter{}
	f.runtime = infra.New(printer, f.server.Client())
	f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "app", fakegalaxy.Fault{
		Status: http.StatusServiceUnavailable, Count: -1,
	})

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err == nil {
		t.Fatalf("Start: expected the armed artifact fault to fail the run")
	}

	if want := "Installed: acme.lib == " + testVersion100; !printer.hasLineContaining(want) {
		t.Fatalf("install report lacks %q:\n%v", want, printer.snapshot())
	}
	failed := "Failed: acme.app == " + testVersion100 + " error: "
	if !printer.hasLineContaining(failed) {
		t.Fatalf("install report lacks a failure line starting %q:\n%v", failed, printer.snapshot())
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
			{Name: "acme.app", Version: testVersion100, Source: f.cfg.Server, SHA256: f.appV1.SHA256, Deps: []string{"acme.lib"}},
			{Name: "acme.lib", Version: testVersion100, Source: f.cfg.Server, SHA256: f.libV1.SHA256},
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
// Nothing else in the suite pins this for install specifically: install and
// warm pass cfg.Frozen through to writeRunMetrics, and this is the
// regression net proving that still happens for the install path;
// TestLockFrozenPassesOnAnUpToDateLockfile (lock_command_test.go) pins the
// identical claim for lock, which honors --frozen too, through a different
// mechanism (a drift gate rather than consuming the lockfile).
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

	if got := readManifestVersion(t, f.downloadPath, "app"); got != testVersion100 {
		t.Errorf("installed acme.app collection_info.version = %q, want %q (lockfile pin over the 2.0.0 highest)", got, testVersion100)
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
	// helpers.ErrInstallationFailed, but the triggering cause is not
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

// TestFrozenInstallRejectsWildcardLockfilePin proves install --frozen fails
// closed on a lockfile entry whose pinned version is not an exact version -
// "*" here - with helpers.ErrLockfileInvalid and the lockfile exit class (6),
// rather than treating it as unpinned and silently installing the server's
// highest available version under a directory literally named
// "acme.app-*.info". TestLoadRejectsNonExactVersion (internal/galaxy/lockfile)
// is the unit-level proof that lockfile.Load itself refuses this shape; this
// test proves the resulting error and exit class survive up through
// install --frozen end to end, and that no manifest and specifically no
// glob-named ".info" sidecar directory are ever created.
//
// What this test does NOT prove on its own: that the wrong-version-install
// consequence is unreachable. buildCollectionsMap carries an independent
// helpers.IsExactVersion guard over the same shape (see its own doc
// comment), reached from a resolved-snapshot path that never touches a
// lockfile at all, so reverting lockfile.validate's check alone still fails
// this test closed - just under a different sentinel
// (helpers.ErrInvalidCollectionVersion, exit 2) and never reaching the
// point where a wrong version could install. What this test pins is
// specifically lockfile.validate's own sentinel and exit class on the
// --frozen lockfile path; TestPoisonedResolvedSnapshotVersionRejectsInstall
// (poisoned_version_test.go) is the analogous pin for buildCollectionsMap's
// own guard on the snapshot path.
//
// The two subtests share one fixture and run in a fixed order, not in
// parallel with each other, since the second overwrites the lockfile the
// first wrote.
func TestFrozenInstallRejectsWildcardLockfilePin(t *testing.T) {
	f := newE2EFixture(t)
	f.server.AddVersion("acme", "app", "2.0.0", nil)
	lockPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	f.cfg.Frozen = true
	wildcardInfoDir := filepath.Join(f.downloadPath, "ansible_collections", "acme.app-*.info")

	t.Run("wildcard pin fails closed and creates nothing", func(t *testing.T) {
		lf := &lockfile.File{
			SchemaVersion: lockfile.SchemaVersion,
			Server:        f.cfg.Server,
			Collections:   []lockfile.Entry{{Name: "acme.app", Version: "*", Source: f.cfg.Server}},
		}
		if err := lockfile.Save(lockPath, lf); err != nil {
			t.Fatalf("save wildcard-pinned lockfile: %v", err)
		}

		err := collections.Start(context.Background(), f.cfg, f.runtime)
		if err == nil {
			t.Fatal("expected an error from a wildcard-pinned lockfile under --frozen, got nil")
		}
		if !errors.Is(err, helpers.ErrLockfileInvalid) {
			t.Fatalf("Start error = %v, want errors.Is helpers.ErrLockfileInvalid", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitLock {
			t.Errorf("exitcode.FromError(err) = %d, want %d", got, exitcode.ExitLock)
		}
		assertPathAbsent(t, manifestPathFor(f.downloadPath, "app"))
		// The glob-shaped sidecar an unguarded binary would create from the
		// literal "*" version text - see newInstallTarget's ".info" naming.
		assertPathAbsent(t, wildcardInfoDir)
	})

	t.Run("exact pin on the identical fixture installs cleanly", func(t *testing.T) {
		lf := &lockfile.File{
			SchemaVersion: lockfile.SchemaVersion,
			Server:        f.cfg.Server,
			Collections: []lockfile.Entry{
				{Name: "acme.app", Version: testVersion100, Source: f.cfg.Server, SHA256: f.appV1.SHA256},
			},
		}
		if err := lockfile.Save(lockPath, lf); err != nil {
			t.Fatalf("save exact-pinned lockfile: %v", err)
		}

		if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("Start: %v", err)
		}
		assertManifestInstalled(t, f.downloadPath, "app")
		if got := readManifestVersion(t, f.downloadPath, "app"); got != testVersion100 {
			t.Errorf("installed acme.app collection_info.version = %q, want %q", got, testVersion100)
		}
	})
}

// warnCapturingPrinter is a minimal output.Printer stub for this package's
// own use: every method is a no-op (inherited from noopPrinter) except
// Warnf, which records each call so TestOfflineOutranksRefresh can assert on
// it. The internal collections package has its own, richer capturingPrinter
// (start_test.go), but that type lives in a _test.go file and is therefore
// invisible across the package boundary this file's own package
// (collections_test) sits on the other side of.
type warnCapturingPrinter struct {
	noopPrinter

	warns []string
}

func (p *warnCapturingPrinter) Warnf(format string, args ...any) {
	p.warns = append(p.warns, fmt.Sprintf(format, args...))
}

// hasWarnContaining reports whether any recorded Warnf line contains substr.
func (p *warnCapturingPrinter) hasWarnContaining(substr string) bool {
	for _, line := range p.warns {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// installOnceAndPublishNewerVersion runs a cold install (populating the
// cache and the download path with acme.app@1.0.0), then publishes
// acme.app@2.0.0 on the same server - with the identical acme.lib dependency
// the fixture's own 1.0.0 already declares, so the published version changes
// only the version number, not the dependency graph - and wipes the
// download path, leaving a warm cache whose persisted resolve snapshot still
// names 1.0.0. This is the fixture every --refresh e2e test in this file
// shares; the server's call counts are reset just before returning so a
// caller's own assertions start counting from the second Start alone.
func installOnceAndPublishNewerVersion(t *testing.T) *e2eFixture {
	t.Helper()
	f := newE2EFixture(t)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}
	f.server.AddVersion("acme", "app", "2.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the refresh run: %v", err)
	}
	f.server.ResetCounts()
	return f
}

// TestRefreshReSolvesInsteadOfReplayingTheSnapshot proves --refresh's whole
// point: with it unset, a second install against a warm cache replays the
// persisted resolve snapshot and installs the same 1.0.0 it always would,
// entirely from cache (Total() == 0); with it set, the run bypasses that
// snapshot, re-resolves against the live server, and picks up the newly
// published acme.app@2.0.0 instead, making at least one real HTTP request
// (Total() > 0). The two rows are mutual controls: --refresh is the only
// variable between them, and it flips both the installed version and
// whether the network was touched at all.
//
// Mutation (dropping `&& !refreshBypassesSnapshot(cfg)` from
// resolveCollectionsInternal's snapshotAllowed expression) confirmed to fail
// the "with refresh" row with:
//
//	e2e_test.go:567: installed acme.app collection_info.version = "1.0.0",
//	want "2.0.0"
//	e2e_test.go:571: server.Total() > 0 = false, want true (Total() = 0)
//	--- FAIL: TestRefreshReSolvesInsteadOfReplayingTheSnapshot/with_refresh:_re-resolves,_picks_up_the_new_version (0.08s)
func TestRefreshReSolvesInsteadOfReplayingTheSnapshot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		wantVersion string
		refresh     bool
		wantNetwork bool
	}{
		{name: "without refresh: replays the snapshot, no network", wantVersion: testVersion100, refresh: false, wantNetwork: false},
		{name: "with refresh: re-resolves, picks up the new version", wantVersion: "2.0.0", refresh: true, wantNetwork: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := installOnceAndPublishNewerVersion(t)
			f.cfg.Refresh = tc.refresh

			if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
				t.Fatalf("second Start (refresh=%v): %v", tc.refresh, err)
			}

			if got := readManifestVersion(t, f.downloadPath, "app"); got != tc.wantVersion {
				t.Errorf("installed acme.app collection_info.version = %q, want %q", got, tc.wantVersion)
			}
			gotNetwork := f.server.Total() > 0
			if gotNetwork != tc.wantNetwork {
				t.Errorf("server.Total() > 0 = %v, want %v (Total() = %d)", gotNetwork, tc.wantNetwork, f.server.Total())
			}
		})
	}
}

// TestRefreshDoesNotRedownloadCachedArtifacts proves the content-addressed
// carve-out --refresh leaves alone: with nothing new published upstream,
// --refresh still re-fetches root metadata (the version-free cached answer
// it exists to bypass) but the artifact itself - already cached from the
// first install, and named by a version this run resolves to the identical
// 1.0.0 - is never re-downloaded. isCacheHit and the extracted store are
// correct here only by omission (neither one consults cfg.Refresh at all),
// and that omission is exactly what this test pins.
//
// Mutation (adding `|| deps.cfg.Refresh` to isCacheHit's own early-return
// condition, so refresh forces a cache miss the way forceDownload already
// does) confirmed to fail with:
//
//	e2e_test.go:613: EndpointArtifact count = 2, want 0 (a cached artifact
//	must not be re-downloaded)
//	--- FAIL: TestRefreshDoesNotRedownloadCachedArtifacts (0.07s)
func TestRefreshDoesNotRedownloadCachedArtifacts(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the refresh run: %v", err)
	}
	f.server.ResetCounts()
	f.cfg.Refresh = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (refresh): %v", err)
	}

	if got := f.server.Count(fakegalaxy.EndpointRootMetadata); got == 0 {
		t.Errorf("EndpointRootMetadata count = %d, want > 0 (refresh must bypass the version-free cached answer)", got)
	}
	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("EndpointArtifact count = %d, want 0 (a cached artifact must not be re-downloaded)", got)
	}
	if got := readManifestVersion(t, f.downloadPath, "app"); got != testVersion100 {
		t.Errorf("installed acme.app collection_info.version = %q, want %q", got, testVersion100)
	}
}

// TestOfflineOutranksRefresh proves --offline wins when both are set:
// resolution replays the persisted snapshot exactly as an unrefreshed
// offline run would (Total() == 0, installs the pinned 1.0.0 despite
// acme.app@2.0.0 being available upstream), and the operator is warned that
// --refresh was skipped rather than the run silently dropping it.
// TestRefreshReSolvesInsteadOfReplayingTheSnapshot's own "with refresh" row
// is this test's positive control on the identical fixture shape: the same
// --refresh, without --offline, does reach the network and does pick up
// 2.0.0 - proving --offline is what changes the outcome here, not some
// other difference between the two fixtures.
//
// The count assertions are checked before the warning one deliberately.
// Mutation (dropping the `runtime.Output.Warnf` call from initInstall's
// `if cfg.Refresh && cfg.Offline` block, keeping the veto itself) confirmed
// to fail only the warning assertion, with the counts and installed version
// above it still passing:
//
//	e2e_test.go:676: expected a --refresh-skipped warning on stderr, got []
//	--- FAIL: TestOfflineOutranksRefresh (0.08s)
//
// A second mutation was also tried and did NOT kill this test: dropping the
// `!cfg.Offline` term from refreshBypassesSnapshot (so it vetoes the
// snapshot on --refresh alone, offline or not) still resolves 1.0.0 with
// Total() == 0. cache.PolicyForConstraint's own IsOffline()-first check is
// why: with the resolve snapshot no longer consulted, the fallback solve's
// root-metadata read still goes through that policy, which forces
// Read: true, Write: false under --offline regardless of --refresh - so it
// serves the already-cached (pre-2.0.0) root metadata document from the API
// cache instead of reaching the network, landing on 1.0.0 again by a
// different route. This fixture's API cache is warm purely as a side effect
// of installOnceAndPublishNewerVersion's own seeding install, not because
// the offline check fired - so this test cannot pin the `!cfg.Offline` term
// itself. internal/galaxy/collections' own
// TestRefreshOfflinePreservesResolveWithStaleMetadataCaches (package
// collections, not collections_test) seeds the state where the metadata
// caches are empty and the resolve snapshot is the only thing that can
// answer, and that test does kill on the identical mutation.
func TestOfflineOutranksRefresh(t *testing.T) {
	t.Parallel()
	f := installOnceAndPublishNewerVersion(t)
	f.cfg.Refresh = true
	f.cfg.Offline = true
	printer := &warnCapturingPrinter{}
	runtime := infra.New(printer, f.server.Client())

	if err := collections.Start(context.Background(), f.cfg, runtime); err != nil {
		t.Fatalf("Start (refresh + offline): %v", err)
	}

	if got := f.server.Total(); got != 0 {
		t.Errorf("server.Total() = %d, want 0 (offline must never reach the network)", got)
	}
	if got := readManifestVersion(t, f.downloadPath, "app"); got != testVersion100 {
		t.Errorf("installed acme.app collection_info.version = %q, want %q", got, testVersion100)
	}
	if !printer.hasWarnContaining("--offline: skipping --refresh") {
		t.Errorf("expected a --refresh-skipped warning on stderr, got %v", printer.warns)
	}
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
	s.AddVersion("acme", "solo", testVersion100, nil)
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
// resolves the full dependency graph. requirementsSignatureFromSpec encodes
// the --no-deps mode as part of the hashed requirements signature, so a mode
// change alone changes RequirementsHash and forces a fresh resolve instead
// of matching the stored hash and reusing the --no-deps graph verbatim,
// which would silently skip acme.lib.
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
// loadResolvedFromSnapshot: tryIncrementalResolve checks RequirementsHash in
// addition to per-root spec equality against RequirementsSnapshot, so
// acme.app's --no-deps (nil-deps) snapshot entry is not reused once the mode
// has changed, and acme.lib is resolved rather than silently skipped.
func TestNoDepsSnapshotNotReusedIncrementally(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.server.AddVersion("acme", "tool", testVersion100, nil)
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

// assertInstalledProvenance fails the test unless the .info directory of
// the installed acme.<name>-<version> names server in its GALAXY.yml, keeps
// provenanceLine out of that document - ansible discards a GALAXY.yml
// carrying any key outside its schema - and holds that line, and only it, in
// go-galaxy.yml beside it.
func assertInstalledProvenance(t *testing.T, downloadPath, name, version, server, provenanceLine string) {
	t.Helper()
	infoDir := filepath.Join(downloadPath, "ansible_collections", "acme."+name+"-"+version+".info")
	data, err := os.ReadFile(filepath.Join(infoDir, "GALAXY.yml")) //nolint:gosec // path is built from this test's own temp dirs.
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	key, _, _ := strings.Cut(provenanceLine, ":")
	if !strings.Contains(string(data), "server: "+server) || strings.Contains(string(data), key) {
		t.Fatalf("sidecar does not name %s, or carries %s outside ansible's schema:\n%s", server, key, data)
	}
	provenance, err := os.ReadFile(filepath.Join(infoDir, "go-galaxy.yml")) //nolint:gosec // path is built from this test's own temp dirs.
	if err != nil || string(provenance) != provenanceLine+"\n" {
		t.Fatalf("go-galaxy.yml = %q (%v), want %q", provenance, err, provenanceLine+"\n")
	}
}
