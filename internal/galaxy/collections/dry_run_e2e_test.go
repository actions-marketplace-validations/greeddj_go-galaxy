package collections_test

// This file (continued from e2e_test.go) covers `install --dry-run`, and the
// shared dry-run foundation, end to end against a live fake Galaxy server:
// nothing is downloaded, nothing lands in the install tree or the artifact
// cache, no install is recorded in the reloaded snapshot, and the backend
// lock is still taken for the run's whole duration. The snapshot save itself
// is conditional: it proceeds when a persisted snapshot already existed
// (TestInstallDryRunSavesMetadataCachesWhenSnapshotExists - the metadata
// caches are reconstructible cache, not this command's product) and is
// skipped when one did not (TestInstallDryRunDoesNotFabricateASnapshot - a
// cold-cache dry run must not stamp Meta.LastSnapshot for the first time,
// which cleanup's sweepExtractedStore reads as positive evidence that
// nothing is installed or warmed anywhere). It also pins that `outdated`
// (which has no product) is left entirely unaffected by --dry-run.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestInstallDryRunAgainstLiveServerDoesNotMutate is the load-bearing e2e
// proof for `install --dry-run`: a cold-cache dry run against a real fake
// Galaxy server downloads nothing, creates no install tree, records no
// install in the reloaded snapshot, and leaves the artifact cache and the
// extracted content-addressable store untouched. This fixture starts from a
// genuinely cold cache - no persisted snapshot exists yet - so the save
// itself is also skipped here (TestInstallDryRunDoesNotFabricateASnapshot
// covers that guard directly); the positive case, where a persisted snapshot
// already exists and the dry run's metadata caches ARE saved on top of it,
// is TestInstallDryRunSavesMetadataCachesWhenSnapshotExists.
func TestInstallDryRunAgainstLiveServerDoesNotMutate(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.DryRun = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (dry run): %v", err)
	}

	assertNothingDownloadedOrInstalled(t, f)
	assertReloadedSnapshotClean(t, f.cfg.CacheDir)
}

// assertNothingDownloadedOrInstalled checks the three on-disk halves of "a
// dry run downloaded and installed nothing": no artifact request reached the
// fake server, the install tree was never created, and neither the artifact
// cache nor the extracted content-addressable store gained an entry.
func assertNothingDownloadedOrInstalled(t *testing.T, f *e2eFixture) {
	t.Helper()
	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("EndpointArtifact count = %d, want 0 (a dry run must never download an artifact)", got)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))

	appKey := helpers.ArtifactKey(f.cfg.Server, "acme-app-1.0.0.tar.gz")
	libKey := helpers.ArtifactKey(f.cfg.Server, "acme-lib-1.0.0.tar.gz")
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, appKey))
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, libKey))

	// The content-addressable extracted store root is created lazily on the
	// first ingest; a dry run must never trigger that first ingest at all.
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, extracted.RootDirName))
}

// assertReloadedSnapshotClean reopens the backend at cacheDir and checks that
// a cold-cache dry run - one starting with no persisted snapshot - leaves it
// that way: WasPersisted() still reports false (the save was skipped; see
// installDryRun's own doc comment), and no install was recorded and no
// project was enrolled either, which would be true regardless but are worth
// pinning here too since a cold-cache run is the shape a schema bump or an
// expired S3 state object also produces.
func assertReloadedSnapshotClean(t *testing.T, cacheDir string) {
	t.Helper()
	ctx := context.Background()
	backend := local.New(cacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	if st.WasPersisted() {
		t.Error("expected WasPersisted() to still be false: a cold-cache dry run must not fabricate a persisted snapshot")
	}
	if _, ok := st.GetInstalled("acme.app@1.0.0"); ok {
		t.Error("expected no recordInstall entry for acme.app in the reloaded snapshot")
	}
	if _, ok := st.GetInstalled("acme.lib@1.0.0"); ok {
		t.Error("expected no recordInstall entry for acme.lib in the reloaded snapshot")
	}

	registry, err := backend.LoadProjectRegistry(ctx)
	if err != nil {
		t.Fatalf("backend.LoadProjectRegistry: %v", err)
	}
	if len(registry.Projects) != 0 {
		t.Errorf("expected an empty project registry after a dry run, got %d entries: %+v", len(registry.Projects), registry.Projects)
	}
}

// TestInstallDryRunDoesNotFabricateASnapshot is the direct proof for the
// guard itself: against a fresh cache directory - no persisted snapshot
// exists at all, the same shape a schema bump that drops one, or a missing
// or expired S3 state object, would also produce - a dry run must not stamp
// Meta.LastSnapshot for the first time. Every real Save/MarshalSnapshot
// stamps it unconditionally, so the only way to keep it unset is to skip the
// save entirely when none was there to begin with. Asserted via
// WasPersisted(), not file presence: the local backend creates its Bolt file
// on Open regardless of whether anything was ever saved into it.
func TestInstallDryRunDoesNotFabricateASnapshot(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.DryRun = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (dry run against a cold cache): %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	if st.WasPersisted() {
		t.Fatal("expected WasPersisted() to be false: a dry run against a cache with no persisted snapshot must not create one")
	}
}

// TestInstallDryRunSavesMetadataCachesWhenSnapshotExists proves the guard is
// not over-applied into "a dry run never saves": once a persisted snapshot
// already exists (seeded here by a prior real install), a subsequent dry run
// still saves its own resolve-side metadata caches on top of it. A second
// collection is added to requirements.yml between the two runs so the dry
// run's resolve is a genuinely fresh solve - a different RequirementsHash
// than what is already on disk - rather than being served verbatim from the
// snapshot the first run left behind, which would make this test pass
// vacuously without the dry run's own recordResolution call ever running.
func TestInstallDryRunSavesMetadataCachesWhenSnapshotExists(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (seed a persisted snapshot with a real install): %v", err)
	}

	f.server.AddVersion("acme", "extra", "1.0.0", nil)
	writeRequirementsMulti(t, f.cfg.RequirementsFile, "acme.app", "acme.extra")
	f.cfg.DryRun = true

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (dry run against an already-persisted snapshot): %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	if !st.WasPersisted() {
		t.Error("expected WasPersisted() to still be true: a persisted snapshot already existed before the dry run")
	}
	req := st.RequirementsSnapshot()
	if _, ok := req["acme.extra"]; !ok {
		t.Errorf("expected the dry run's own fresh resolve to have saved acme.extra's requirement spec, got %v", req)
	}
}

// TestInstallDryRunStillTakesBackendLock proves initInstall's exclusive
// backend lock is acquired, and held for the run's whole duration, exactly
// as it is on a real install: a Hang fault on the root-metadata endpoint
// parks the dry run mid-resolve - well after initInstall's Lock call - and a
// concurrent lock attempt against the same cache directory must fail for as
// long as that run is in flight, then succeed once its context is canceled
// and it unwinds.
func TestInstallDryRunStillTakesBackendLock(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	// Pre-created so the polling loop below never races initInstall's own
	// os.MkdirAll: every lock attempt it makes is either "lock held elsewhere"
	// or "lock free", never "parent directory missing".
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.app")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "app", "1.0.0", nil)
	s.Fail(fakegalaxy.EndpointRootMetadata, "acme", "app", fakegalaxy.Fault{Hang: true, Count: -1})

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
		DryRun:           true,
	}
	// No client-side Timeout: only ctx's own cancellation (below) may ever
	// unblock the hung request, mirroring prefetch_cancel_e2e_test.go's own
	// fixture.
	runtime := infra.New(noopPrinter{}, s.Client())

	ctx, cancel := context.WithCancel(context.Background())
	// Deliberately in addition to the explicit cancel() call below, not
	// instead of it: without this defer, a t.Fatal above (or any future
	// assertion added ahead of the explicit cancel()) would exit this test
	// via runtime.Goexit with the Hang-faulted request still parked and
	// nothing left to ever cancel it, so fakegalaxy's own t.Cleanup
	// (httptest.Server.Close) would then block forever waiting for that
	// request to finish - taking down the whole test binary with a
	// "test timed out" panic instead of a clean, named test failure.
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- collections.Start(ctx, cfg, runtime)
	}()

	deadline := time.Now().Add(5 * time.Second)
	var lockErr error
	for time.Now().Before(deadline) {
		release, err := store.AcquireLock(cacheDir)
		if err == nil {
			// Raced ahead of initInstall's own Lock call; release immediately
			// and retry rather than falsely concluding no lock is held.
			_ = release()
			time.Sleep(5 * time.Millisecond)
			continue
		}
		lockErr = err
		break
	}
	if lockErr == nil {
		t.Fatal("expected a concurrent lock attempt to fail while the dry run holds the backend lock, but it kept succeeding")
	}
	if !errors.Is(lockErr, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("expected errors.Is ErrAnotherInstanceIsRunning, got %v", lockErr)
	}

	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected the canceled dry run to return an error")
	}

	release, err := store.AcquireLock(cacheDir)
	if err != nil {
		t.Fatalf("expected the lock to be free once the dry run unwound, got %v", err)
	}
	_ = release()
}

// TestInstallDryRunOfflineReportsWouldFailAndFailsClosed proves a dry run
// never reports success for a collection a real --offline install would
// certainly fail to fetch. acme.app's metadata is cached ahead of time via a
// real Lock run - which resolves and populates the snapshot's metadata
// caches without ever touching the artifact store or the install tree - so
// the subsequent dry run's own resolve is served entirely from the snapshot
// with the network transport hard-disabled, exactly like a real --offline
// install's resolve step. With no cached artifact and --offline set, a real
// install would fail fetchArtifact's offline guard (helpers.ErrOfflineMode);
// this dry run must report and fail the same way instead of claiming it
// would install.
func TestInstallDryRunOfflineReportsWouldFailAndFailsClosed(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock (populate the snapshot's metadata caches): %v", err)
	}

	f.cfg.DryRun = true
	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)

	var startErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		startErr = collections.Start(context.Background(), f.cfg, f.runtime)
	})

	if startErr == nil {
		t.Fatal("expected an error from an offline dry run against an uncached artifact")
	}
	if !errors.Is(startErr, helpers.ErrInstallationFailed) {
		t.Errorf("expected errors.Is ErrInstallationFailed, got %v", startErr)
	}
	if !errors.Is(startErr, helpers.ErrOfflineMode) {
		t.Errorf("expected errors.Is ErrOfflineMode, got %v", startErr)
	}
	if !bytes.Contains(stderr, []byte("Would fail:")) {
		t.Errorf("expected a \"Would fail:\" line on stderr, got %q", stderr)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))
	assertPathAbsent(t, installPathFor(f.downloadPath, "lib"))
}

// TestInstallDryRunBannerSurvivesQuiet proves dryRunBanner's output actually
// reaches a human even in --quiet mode, against the real progress.Printer
// (not a test stub): a bare Printf-tier banner would be silently swallowed by
// quiet mode, which is exactly the failure mode this banner exists to avoid
// for an env-sourced --dry-run.
func TestInstallDryRunBannerSurvivesQuiet(t *testing.T) {
	f := newE2EFixture(t)
	f.cfg.DryRun = true
	f.cfg.Quiet = true

	var startErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		startErr = collections.Start(context.Background(), f.cfg, f.runtime)
	})
	if startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}

	if !bytes.Contains(stderr, []byte("--dry-run")) {
		t.Errorf("expected the dry-run banner on stderr despite --quiet, got stderr=%q", stderr)
	}
}

// TestOutdatedDryRunMutatesNothing pins that `outdated` - which has no
// product at all - is left completely unaffected by --dry-run: the lockfile
// it reads is untouched, no cache directory is ever created even though one
// is configured, no metrics file is ever written even though one is
// configured, and toggling cfg.DryRun changes nothing about its result. This
// is deliberate: `outdated` must never grow a cfg.DryRun branch, since it has
// nothing for --dry-run to suppress.
func TestOutdatedDryRunMutatesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	metricsPath := filepath.Join(root, "metrics.json")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "widgets", "1.0.0", nil)
	s.AddVersion("acme", "widgets", "2.0.0", nil)

	lockPath := lockfile.ResolveDefaultPath(reqPath, "")
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        s.URL(),
		Collections:   []lockfile.Entry{{Name: "acme.widgets", Version: "1.0.0", Source: s.URL()}},
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	before, err := os.ReadFile(lockPath) //nolint:gosec // path is this test's own temp dir.
	if err != nil {
		t.Fatalf("read lockfile before Outdated: %v", err)
	}

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		DryRun:           true,
		Workers:          2,
	}
	runtime := infra.New(noopPrinter{}, s.Client())

	dryErr := collections.Outdated(context.Background(), cfg, runtime)
	if dryErr != nil {
		t.Fatalf("Outdated (dry run): %v", dryErr)
	}

	after, err := os.ReadFile(lockPath) //nolint:gosec // path is this test's own temp dir.
	if err != nil {
		t.Fatalf("read lockfile after Outdated: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("expected the lockfile to be byte-identical after Outdated, before=%q after=%q", before, after)
	}
	if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
		t.Errorf("expected Outdated to never create the configured cache directory, stat error = %v", statErr)
	}
	if _, statErr := os.Stat(metricsPath); !os.IsNotExist(statErr) {
		t.Errorf("expected Outdated to never write the configured metrics file, stat error = %v", statErr)
	}

	// The result itself must not depend on --dry-run: a second, otherwise
	// identical run with cfg.DryRun cleared must succeed exactly the same.
	cfg.DryRun = false
	normalErr := collections.Outdated(context.Background(), cfg, infra.New(noopPrinter{}, s.Client()))
	if normalErr != nil {
		t.Fatalf("Outdated (normal run): %v", normalErr)
	}
}
