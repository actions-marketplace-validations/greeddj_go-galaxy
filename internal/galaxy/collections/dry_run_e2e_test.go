package collections_test

// This file (continued from e2e_test.go) covers `install --dry-run` and
// `warm --dry-run`, and the shared dry-run foundation, end to end against a
// live fake Galaxy server: nothing is downloaded, nothing lands in the
// install tree, the artifact cache, or the extracted store, no install or
// warmed entry is recorded in the reloaded snapshot, and the backend lock is
// still taken and still held once the run is mid-resolve. The snapshot save
// itself is conditional: it proceeds when a persisted snapshot already existed
// (TestInstallDryRunSavesMetadataCachesWhenSnapshotExists - the metadata
// caches are reconstructible cache, not this command's product) and is
// skipped when one did not (TestInstallDryRunDoesNotFabricateASnapshot - a
// cold-cache dry run must not stamp Meta.LastSnapshot for the first time,
// which cleanup's sweepExtractedStore reads as positive evidence that
// nothing is installed or warmed anywhere). It also pins that `outdated`
// (which has no product) is left entirely unaffected by --dry-run.
//
// Warm's own settled predicate - artifact cached AND extracted tree
// materialized under a known sha, not cache presence alone - is pinned here
// against a live server too (TestWarmDryRunReportsWouldWarmWhenExtractedStoreIsCold),
// since a fresh runner against a warm S3-backed bucket with a cold local
// extracted store is exactly the shape a bare "cached" predicate would
// misreport as nothing to do.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
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
	assertSnapshotUnpersistedAndRegistryEmpty(t, f.cfg.CacheDir, "acme.app@1.0.0", "acme.lib@1.0.0")
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

// dryRunLockObservationCeiling is a liveness ceiling, not a timing margin:
// every assertion in assertDryRunStillTakesBackendLock is made only after an
// observed event, so a slow machine makes that helper slower, never wrong.
// The ceiling fires only when the run genuinely never reaches its resolve,
// and a run that dies before getting there is caught by the done channel
// instead, in milliseconds.
const dryRunLockObservationCeiling = 10 * time.Second

// assertDryRunStillTakesBackendLock proves initInstall's exclusive backend
// lock is acquired, and is still held once the run is mid-resolve, exactly as
// it is on a real run of whichever command run drives: a Hang fault on the
// root-metadata endpoint parks the dry run mid-resolve - well after
// initInstall's Lock call - one concurrent lock attempt against the same
// cache directory must fail while the run is parked there, and the lock must
// be free again once its context is canceled and it unwinds. Those are two
// sampled points, not a continuum: nothing here observes the interval between
// them.
//
// Waiting for that mid-resolve moment must never touch the lock itself.
// store.AcquireLock is non-blocking (LOCK_NB) and initInstall makes exactly
// one Lock attempt with no retry, so an observer holding the lock even
// momentarily can hold it in the very window initInstall tries - killing the
// run it exists to observe, which then never takes the lock at all and leaves
// the observer to conclude that no lock is ever held. The signal used instead
// is the fake server's own request counter: dispatchRootMetadata increments it
// before handleRootMetadata applies the Hang fault, and that request is issued
// from the resolve, downstream of initInstall's Lock. So a count of at least
// one means the run is past Lock and cannot proceed until its context is
// canceled: the only production timer that could unpark it is
// helpers.MetadataFetchDeadline, two minutes, orders above the gap between
// that signal and the single lock attempt that follows it.
//
// The final acquisition, after the run unwinds, is this fixture's positive
// control: the same store.AcquireLock call on the same cacheDir succeeds, so
// the mid-resolve refusal is contention with the run rather than a lock path
// that would fail for any caller. Shared by
// TestInstallDryRunStillTakesBackendLock and TestWarmDryRunStillTakesBackendLock,
// which differ only in which collections.* entry point they drive.
func assertDryRunStillTakesBackendLock(t *testing.T, run func(context.Context, *config.Config, *infra.Infra) error) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	// Pre-created so the lock attempts below never race initInstall's own
	// os.MkdirAll: each one is either "lock held elsewhere" or "lock free",
	// never "parent directory missing".
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
		done <- run(ctx, cfg, runtime)
	}()

	waitForResolveInFlight(t, s, done)

	if release, err := store.AcquireLock(cacheDir); err == nil {
		_ = release()
		t.Fatal("expected the single concurrent lock attempt to fail while the dry run holds the backend lock, but it succeeded")
	} else if !errors.Is(err, helpers.ErrAnotherInstanceIsRunning) {
		// Documentary, not pinned: this line is reachable only when
		// AcquireLock fails for a reason other than EWOULDBLOCK, which is an
		// IO failure on the lock path itself rather than anything the code
		// under test decides. It is kept to tell contention apart from one.
		t.Fatalf("expected errors.Is ErrAnotherInstanceIsRunning, got %v", err)
	}

	cancel()
	awaitCanceledRun(t, done)

	release, err := store.AcquireLock(cacheDir)
	if err != nil {
		t.Fatalf("expected the lock to be free once the dry run unwound, got %v", err)
	}
	_ = release()
}

// waitForResolveInFlight blocks until the fake server has seen the run's
// root-metadata request, which is the observable proof that the run is past
// initInstall's Lock and is now parked on the Hang fault. It never touches
// the backend lock; see assertDryRunStillTakesBackendLock for why an observer
// that did could kill the run it is observing. A run that returns before
// reaching its resolve never held the lock at all, so there is nothing left
// to observe and the caller is told that rather than being left to conclude
// from a silent timeout that no lock is ever taken.
func waitForResolveInFlight(t *testing.T, s *fakegalaxy.Server, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(dryRunLockObservationCeiling)
	for s.Count(fakegalaxy.EndpointRootMetadata) < 1 {
		select {
		case runErr := <-done:
			t.Fatalf("the run returned before it reached its resolve, so it never held the lock to observe: %v", runErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the root metadata endpoint received no request within %v", dryRunLockObservationCeiling)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitCanceledRun joins an already-canceled run and requires it to report an
// error. The join is bounded: an unbounded receive on a run that never
// unwinds would take the whole test binary down with a "test timed out"
// panic instead of failing this test by name.
func awaitCanceledRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case runErr := <-done:
		if runErr == nil {
			t.Fatal("expected the canceled dry run to return an error")
		}
	case <-time.After(dryRunLockObservationCeiling):
		t.Fatalf("the dry run did not return within %v after its context was canceled", dryRunLockObservationCeiling)
	}
}

// TestInstallDryRunStillTakesBackendLock is assertDryRunStillTakesBackendLock
// driven by collections.Start; see that helper's own doc comment for the
// property being pinned.
func TestInstallDryRunStillTakesBackendLock(t *testing.T) {
	assertDryRunStillTakesBackendLock(t, collections.Start)
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

// assertSnapshotUnpersistedAndRegistryEmpty reopens the backend at cacheDir
// and asserts a cold-cache dry run left no persisted snapshot behind
// (WasPersisted still false), enrolled no project in the registry, and
// recorded no install for any key in notInstalled - shared by install's and
// warm's own cold-cache dry-run fixtures.
func assertSnapshotUnpersistedAndRegistryEmpty(t *testing.T, cacheDir string, notInstalled ...string) {
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
	for _, key := range notInstalled {
		if _, ok := st.GetInstalled(key); ok {
			t.Errorf("expected no recordInstall entry for %s in the reloaded snapshot", key)
		}
	}

	registry, err := backend.LoadProjectRegistry(ctx)
	if err != nil {
		t.Fatalf("backend.LoadProjectRegistry: %v", err)
	}
	if len(registry.Projects) != 0 {
		t.Errorf("expected an empty project registry after a dry run, got %d entries: %+v", len(registry.Projects), registry.Projects)
	}
}

// TestWarmDryRunAgainstLiveServerCachesNothing is the load-bearing e2e proof
// for `warm --dry-run`: a cold-cache dry run against a real fake Galaxy
// server downloads nothing, creates no artifact-cache entry, never even
// creates the extracted store's root directory (lazily created on first
// ingest), enrolls no project, and leaves the snapshot unpersisted.
func TestWarmDryRunAgainstLiveServerCachesNothing(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.DryRun = true

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (dry run): %v", err)
	}

	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("EndpointArtifact count = %d, want 0 (a dry run must never download an artifact)", got)
	}
	appKey := helpers.ArtifactKey(f.cfg.Server, "acme-app-1.0.0.tar.gz")
	libKey := helpers.ArtifactKey(f.cfg.Server, "acme-lib-1.0.0.tar.gz")
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, appKey))
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, libKey))
	assertPathAbsent(t, filepath.Join(f.cfg.CacheDir, extracted.RootDirName))

	assertSnapshotUnpersistedAndRegistryEmpty(t, f.cfg.CacheDir)
}

// TestWarmDryRunWritesNoWarmedEntry proves warm --dry-run never calls
// recordWarmed: a real install first seeds a persisted snapshot and
// populates the artifact cache and the extracted store (without ever writing
// a warmed entry itself - see TestWarmColdCachePopulatesCacheWithoutInstalling's
// sibling assertion on the install side), so the subsequent dry run's own
// warmed set is checked against a persisted snapshot rather than a cold one a
// skipped save would produce vacuously.
func TestWarmDryRunWritesNoWarmedEntry(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (seed a persisted snapshot via a real install): %v", err)
	}

	f.server.ResetCounts()
	f.cfg.DryRun = true
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (dry run against an already-installed cache): %v", err)
	}

	if got := f.server.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("EndpointArtifact count = %d, want 0 (a dry run must never download an artifact)", got)
	}

	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	if !st.WasPersisted() {
		t.Error("expected WasPersisted() to still be true: a persisted snapshot already existed before the dry run")
	}
	if got := len(st.WarmedArtifactSHAByKey()); got != 0 {
		t.Errorf("warmed set size = %d, want 0 (warm --dry-run must never call recordWarmed)", got)
	}
}

// TestWarmDryRunReportsWouldWarmWhenExtractedStoreIsCold pins the corrected
// completion predicate this command was built around: cache presence alone
// is not enough, since the artifact store and the extracted store are
// independent. A real warm first populates both halves for both
// collections; the extracted root is then wiped entirely (simulating a fresh
// runner against a warm S3-backed artifact bucket with a cold local
// extracted store), and a subsequent dry run must report both collections as
// "would warm" rather than "already warm" - this test must be shown to fail
// against the recorded (and rejected) "cached implies settled" predicate.
func TestWarmDryRunReportsWouldWarmWhenExtractedStoreIsCold(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (populate both the artifact cache and the extracted store): %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.cfg.CacheDir, extracted.RootDirName)); err != nil {
		t.Fatalf("remove the extracted store root: %v", err)
	}

	f.cfg.DryRun = true
	var warmErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm (dry run against a cold extracted store): %v", warmErr)
	}

	if !bytes.Contains(stdout, []byte("Would warm: acme.app@1.0.0 (artifact cached)")) {
		t.Errorf("expected a \"Would warm: acme.app@1.0.0 (artifact cached)\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Would warm: acme.lib@1.0.0 (artifact cached)")) {
		t.Errorf("expected a \"Would warm: acme.lib@1.0.0 (artifact cached)\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Dry run: 2 would warm, 0 already warm, 0 would fail")) {
		t.Errorf("expected the would-warm summary line, got stdout=%q", stdout)
	}
}

// TestWarmDryRunReportsAlreadyWarmWhenFullyWarm asserts the positive
// counterpart of the cold-extracted-store case above: once both halves of
// warm's product genuinely exist, a subsequent dry run reports both
// collections as already warm and never touches the network.
func TestWarmDryRunReportsAlreadyWarmWhenFullyWarm(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (populate the cache): %v", err)
	}

	f.server.ResetCounts()
	f.cfg.DryRun = true
	var warmErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm (dry run against a fully warm cache): %v", warmErr)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after the dry run = %d, want 0 (a fully warm cache needs no network access to preview)", got)
	}

	if !bytes.Contains(stdout, []byte("Already warm: acme.app@1.0.0")) {
		t.Errorf("expected an \"Already warm: acme.app@1.0.0\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Already warm: acme.lib@1.0.0")) {
		t.Errorf("expected an \"Already warm: acme.lib@1.0.0\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Dry run: 0 would warm, 2 already warm, 0 would fail")) {
		t.Errorf("expected the already-warm summary line, got stdout=%q", stdout)
	}
}

// TestWarmDryRunUsesLockfilePinAsSHASource proves warmDryRunSHA's precedence:
// a lockfile pin, not the (here, absent) warmed record, is what the frozen
// dry run checks the extracted store under. A real install first populates
// the artifact cache and the extracted store while writing no warmed entry
// at all (install never calls recordWarmed); the frozen-pin fixture then
// registers a higher version and pins the lockfile back to the version the
// install actually produced. If warmDryRunSHA's pin arm were dropped, the
// probe would fall back to the (empty) warmed map, name no sha at all, and
// report "would warm" instead - so this must be shown to fail under that
// mutation.
func TestWarmDryRunUsesLockfilePinAsSHASource(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (populate the artifact cache and the extracted store, writing no warmed entry): %v", err)
	}
	newFrozenPinFixture(t, f)

	f.cfg.DryRun = true
	var warmErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm --frozen --dry-run: %v", warmErr)
	}

	if !bytes.Contains(stdout, []byte("Already warm: acme.app@1.0.0")) {
		t.Errorf("expected an \"Already warm: acme.app@1.0.0\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Already warm: acme.lib@1.0.0")) {
		t.Errorf("expected an \"Already warm: acme.lib@1.0.0\" line, got stdout=%q", stdout)
	}
}

// TestWarmDryRunReportsWouldWarmWhenNoSHACanBeNamed pins the pessimistic
// side of warmDryRunSHA's design rule: the extracted tree for acme.app and
// acme.lib is genuinely present (a real install populated both the artifact
// cache and the extracted store), but this run is unfrozen (col.SHA256 is
// empty) and warm has never run (the warmed set is empty), so no sha can be
// named without downloading the artifact's bytes to learn one - the cost
// this preview exists to avoid. warmDryRunSHA must return "" here, and the
// probe must report "would warm" rather than guess at "already warm".
//
// This is the mutation this test exists to catch: a future warmDryRunSHA
// that added a third fallback - reading the sha off this collection's
// InstalledEntry.ArtifactSHA256, the way installDryRunProbe does for install
// - would make this exact case report "Already warm" instead, on a machine
// where warm has genuinely never run and the warmed set genuinely still
// needs writing. Every other test in this suite would still pass.
func TestWarmDryRunReportsWouldWarmWhenNoSHACanBeNamed(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (populate the artifact cache and the extracted store, writing no warmed entry): %v", err)
	}

	f.cfg.DryRun = true
	var warmErr error
	stdout, _ := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm --dry-run: %v", warmErr)
	}

	if !bytes.Contains(stdout, []byte("Would warm: acme.app@1.0.0 (artifact cached)")) {
		t.Errorf("expected a \"Would warm: acme.app@1.0.0 (artifact cached)\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Would warm: acme.lib@1.0.0 (artifact cached)")) {
		t.Errorf("expected a \"Would warm: acme.lib@1.0.0 (artifact cached)\" line, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Dry run: 2 would warm, 0 already warm, 0 would fail")) {
		t.Errorf("expected the would-warm summary line, got stdout=%q", stdout)
	}
}

// driftCachedTarballBytes overwrites the cached artifact file at
// cacheDir/artifactKey in place, same path and same length, with every byte
// flipped - simulating on-disk bit rot or corruption that leaves the file's
// presence and size looking entirely ordinary to a Has() probe, while its
// sidecar and any content-addressable tree keyed by the pre-drift hash are
// left completely untouched (neither lives at this path).
func driftCachedTarballBytes(t *testing.T, cacheDir, artifactKey string) {
	t.Helper()
	tarPath := filepath.Join(cacheDir, artifactKey)
	original, err := os.ReadFile(tarPath) //nolint:gosec // path is this test's own cache fixture under t.TempDir().
	if err != nil {
		t.Fatalf("read the cached tarball before drifting it: %v", err)
	}
	drifted := make([]byte, len(original))
	for i, b := range original {
		drifted[i] = b ^ 0xFF
	}
	if err := os.WriteFile(tarPath, drifted, helpers.FileMod); err != nil {
		t.Fatalf("drift the cached tarball in place: %v", err)
	}
}

// TestWarmDryRunAndRunDisagreeOnAFrozenOfflineDriftedCacheHit pins a known
// and deliberate limit of this preview, so it is never mistaken for a
// stronger guarantee than it actually is: a cached artifact whose bytes
// drift in place, while its sidecar and its extracted tree (keyed by the
// original sha, unaffected by corrupting a different file in the cache) are
// left untouched, still names the same sha warmDryRunSHA would name under a
// pin. The preview therefore reports the drifted collection "Already warm"
// with a would-fail count of zero, while the real --frozen --offline run
// re-hashes the actual bytes on disk (resolveArtifactSHA, forced by the
// non-empty pin), finds them no longer match, and fails closed with
// helpers.ErrSHA256Mismatch - canRetryCacheHit refuses to evict and refetch
// while offline, since there is nothing to replace the bad bytes with.
//
// Closing this gap is rejected, not overlooked: detecting it needs the
// artifact's actual bytes, which on the S3 backend means ArtifactStore.Fetch
// downloading the whole object - exactly the cost this preview exists to
// avoid - and a recorded sidecar sha cannot substitute, since the sidecar is
// precisely what the drifted bytes no longer match. See warmDryRunSHA's own
// doc comment for the same reasoning in the production code.
func TestWarmDryRunAndRunDisagreeOnAFrozenOfflineDriftedCacheHit(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (populate the artifact cache and the extracted tree, unpinned): %v", err)
	}

	// Drift acme.app's cached tarball bytes in place, leaving its sidecar
	// sha256 and its extracted tree exactly as the honest warm above produced
	// them.
	driftCachedTarballBytes(t, f.cfg.CacheDir, helpers.ArtifactKey(f.cfg.Server, "acme-app-1.0.0.tar.gz"))

	newFrozenPinFixture(t, f)
	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)

	f.cfg.DryRun = true
	var dryErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		dryErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if dryErr != nil {
		t.Fatalf("warm --frozen --offline --dry-run: %v (expected the preview to report success despite the drift)", dryErr)
	}
	if !bytes.Contains(stdout, []byte("Already warm: acme.app@1.0.0")) {
		t.Errorf("expected \"Already warm: acme.app@1.0.0\" despite the drifted bytes, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("0 would warm, 2 already warm, 0 would fail")) {
		t.Errorf("expected a would-fail count of zero, got stdout=%q", stdout)
	}
	if !bytes.Contains(stderr, []byte("--frozen --offline: this preview checks that an artifact is cached")) {
		t.Errorf("expected the --frozen --offline warning on stderr, got stderr=%q", stderr)
	}

	f.cfg.DryRun = false
	var realErr error
	_, realStderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		realErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	// warmWithState wraps a per-collection failure as helpers.ErrInstallationFailed
	// without chaining the underlying cause through errors.Is, so the specific
	// helpers.ErrSHA256Mismatch this drift produces is only observable on the
	// error-tier "Failed:" line warmCollections prints, not on the returned
	// error itself - the same limitation TestWarmFrozenHonorsLockfilePinAndFailsClosedOnCorruption's
	// corrupted-pin subtest already works within.
	if !errors.Is(realErr, helpers.ErrInstallationFailed) {
		t.Fatalf("expected the real --frozen --offline warm to fail with errors.Is ErrInstallationFailed, got %v", realErr)
	}
	if !bytes.Contains(realStderr, []byte(helpers.ErrSHA256Mismatch.Error())) {
		t.Errorf("expected the real run's failure line to name %q, got stderr=%q", helpers.ErrSHA256Mismatch.Error(), realStderr)
	}
}

// TestWarmDryRunOfflineFailsClosed proves a warm dry run never reports
// success for a collection a real --offline warm would certainly fail to
// fetch, mirroring TestInstallDryRunOfflineReportsWouldFailAndFailsClosed on
// the warm side: a real Lock run first populates the snapshot's metadata
// caches (never touching the artifact store), so the dry run's own resolve
// is served entirely from the snapshot with the network transport hard
// disabled. With no cached artifact and --offline set, a real warm would
// fail fetchArtifact's own offline guard; this dry run must report and fail
// the same way instead of claiming it would warm.
func TestWarmDryRunOfflineFailsClosed(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock (populate the snapshot's metadata caches): %v", err)
	}

	f.cfg.DryRun = true
	f.cfg.Offline = true
	f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)

	var warmErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})

	if warmErr == nil {
		t.Fatal("expected an error from an offline dry run against an uncached artifact")
	}
	if !errors.Is(warmErr, helpers.ErrInstallationFailed) {
		t.Errorf("expected errors.Is ErrInstallationFailed, got %v", warmErr)
	}
	if !errors.Is(warmErr, helpers.ErrOfflineMode) {
		t.Errorf("expected errors.Is ErrOfflineMode, got %v", warmErr)
	}
	if got := exitcode.FromError(warmErr); got != exitcode.ExitInstall {
		t.Errorf("exitcode.FromError(err) = %d, want ExitInstall (%d)", got, exitcode.ExitInstall)
	}
	if !bytes.Contains(stderr, []byte("Would fail:")) {
		t.Errorf("expected a \"Would fail:\" line on stderr, got %q", stderr)
	}
}

// TestWarmDryRunStillTakesBackendLock is assertDryRunStillTakesBackendLock
// driven by collections.Warm; see that helper's own doc comment for the
// property being pinned. TestWarmNoCacheRejectsBeforeResolving's own "no
// lock" property must not be over-generalized into "a dry run never locks" -
// --no-cache is rejected before initInstall ever runs, while a --dry-run warm
// that gets past that guard locks exactly like a real one.
func TestWarmDryRunStillTakesBackendLock(t *testing.T) {
	assertDryRunStillTakesBackendLock(t, collections.Warm)
}

// TestWarmDryRunSkipsMetrics proves writeRunMetrics's shared cfg.DryRun guard
// covers warm too: with a configured MetricsFile, a warm dry run never writes
// it and instead warns, matching install's own behavior since the guard
// lives inside writeRunMetrics itself.
func TestWarmDryRunSkipsMetrics(t *testing.T) {
	f := newE2EFixture(t)
	f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")
	f.cfg.DryRun = true

	var warmErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm (dry run): %v", warmErr)
	}
	if _, statErr := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(statErr) {
		t.Errorf("expected no metrics file written by a dry run, stat error = %v", statErr)
	}
	if !bytes.Contains(stderr, []byte(f.cfg.MetricsFile)) {
		t.Errorf("expected a warning naming the skipped metrics path, got stderr=%q", stderr)
	}
}

// TestWarmDryRunBannerSurvivesQuiet proves dryRunBanner's output actually
// reaches a human even in --quiet mode for warm, against the real
// progress.Printer (not a test stub) - mirroring
// TestInstallDryRunBannerSurvivesQuiet on the warm side, now that initInstall
// emits the banner for every dry-run command rather than installWithState
// alone.
func TestWarmDryRunBannerSurvivesQuiet(t *testing.T) {
	f := newE2EFixture(t)
	f.cfg.DryRun = true
	f.cfg.Quiet = true

	var warmErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(f.cfg.Verbose, f.cfg.Quiet)
		defer printer.Close()
		f.runtime.Output = printer
		warmErr = collections.Warm(context.Background(), f.cfg, f.runtime)
	})
	if warmErr != nil {
		t.Fatalf("Warm: %v", warmErr)
	}

	if !bytes.Contains(stderr, []byte("--dry-run")) {
		t.Errorf("expected the dry-run banner on stderr despite --quiet, got stderr=%q", stderr)
	}
}
