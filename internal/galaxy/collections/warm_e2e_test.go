package collections_test

// This file (continued from e2e_test.go) exercises the `warm` command's real
// pipeline end-to-end against the fake Galaxy server, mirroring the shape of
// the install-side e2e suite: a cold warm, a re-warm served entirely from
// cache, a frozen lockfile-pinned warm (both honoring and rejecting a pin),
// an offline warm, a partial failure that still caches the collection that
// succeeded, lock discipline across a failing run, the --no-cache usage
// rejection, and the metrics file warm now shares with install.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// assertExtractedStorePresent fails the test unless the content-addressable
// extracted store under cacheDir has a ready (fully extracted) entry for sha.
func assertExtractedStorePresent(t *testing.T, cacheDir, sha string) {
	t.Helper()
	path := filepath.Join(cacheDir, extracted.RootDirName, sha, extracted.ReadyMarker)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected extracted store entry for sha %s to exist, stat error: %v", sha, err)
	}
}

// loadStoreSnapshot opens cfg's cache backend, loads its persisted snapshot,
// and returns it, closing the backend before returning. Safe to use only
// after any collections.Warm/Start run against the same cache directory has
// already completed and released its own lock.
func loadStoreSnapshot(t *testing.T, cfg *config.Config, runtime *infra.Infra) *store.Store {
	t.Helper()
	ctx := context.Background()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("cacheBackend.New: %v", err)
	}
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() {
		_ = backend.Close(ctx)
	}()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return st
}

// assertMetricsCommand reads the metrics file at path and fails the test
// unless its "command" field equals want.
func assertMetricsCommand(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixed cfg.MetricsFile, not user input.
	if err != nil {
		t.Fatalf("read metrics file %s: %v", path, err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", path, err)
	}
	got, _ := written["command"].(string)
	if got != want {
		t.Errorf("metrics file %s command = %q, want %q", path, got, want)
	}
}

// TestWarmColdCachePopulatesCacheWithoutInstalling asserts a cold-cache warm
// downloads and extracts every collection in the dependency graph into the
// artifact cache and the content-addressable extracted store, without ever
// creating an ansible_collections tree under cfg.DownloadPath (warm never
// installs) and without recording anything in the persisted snapshot's
// installed set (warm never calls recordInstall). It also asserts warm's
// side of this commit's fix: each collection gets a Warmed entry keyed by its
// own ns.name@version, recording exactly the artifact sha the collection
// actually resolved to.
func TestWarmColdCachePopulatesCacheWithoutInstalling(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
	assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-lib-1.0.0.tar.gz")
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.libV1.SHA256)

	assertPathAbsent(t, filepath.Join(f.downloadPath, "ansible_collections"))

	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	if got := len(st.InstalledArtifactSHAByKey()); got != 0 {
		t.Errorf("installed set size = %d, want 0 (warm never calls recordInstall)", got)
	}

	warmed := st.WarmedArtifactSHAByKey()
	if got := warmed[collectionKey(f.appV1)]; got != f.appV1.SHA256 {
		t.Errorf("warmed[%q] = %q, want %q", collectionKey(f.appV1), got, f.appV1.SHA256)
	}
	if got := warmed[collectionKey(f.libV1)]; got != f.libV1.SHA256 {
		t.Errorf("warmed[%q] = %q, want %q", collectionKey(f.libV1), got, f.libV1.SHA256)
	}
}

// TestInstallRecordsNoWarmedEntries proves installCollection never calls
// recordWarmed: recordInstall alone already keeps a normal install's
// extracted tree reachable through InstalledArtifactSHAByKey, and a warmed
// entry there would be redundant at best (see recordWarmed's own doc
// comment) - this guards that invariant against a future refactor
// accidentally wiring the call in on the install path too.
func TestInstallRecordsNoWarmedEntries(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	if got := len(st.WarmedArtifactSHAByKey()); got != 0 {
		t.Errorf("warmed set size = %d, want 0 (install never calls recordWarmed)", got)
	}
}

// TestInstallPreservesWarmedEntries is TestInstallRecordsNoWarmedEntries's
// guard pair from the opposite side: that test proves install never adds a
// warmed entry, this one proves install never removes or rewrites one that
// warm already wrote. This is pinned as its own regression test rather than
// left to whatever installCollection and recordWarmed happen to do, because
// initInstall loads the full snapshot and finalizeInstall writes it back
// through the shared snapshotData copy path - the warmed set only survives an
// install because nothing on the install path mutates m.Warmed, an invariant
// a future change to either function could break with every other test still
// green. Concretely, this guards against a future "prune warmed entries for
// keys we just installed" optimization, which would silently reintroduce the
// original bug (see TestWarmColdCachePopulatesCacheWithoutInstalling) with an
// otherwise green suite.
func TestInstallPreservesWarmedEntries(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm (populate the warmed set): %v", err)
	}

	warmedBefore := loadStoreSnapshot(t, f.cfg, f.runtime).WarmedArtifactSHAByKey()
	appKey, libKey := collectionKey(f.appV1), collectionKey(f.libV1)
	if got := warmedBefore[appKey]; got != f.appV1.SHA256 {
		t.Fatalf("warmedBefore[%q] = %q, want %q", appKey, got, f.appV1.SHA256)
	}
	if got := warmedBefore[libKey]; got != f.libV1.SHA256 {
		t.Fatalf("warmedBefore[%q] = %q, want %q", libKey, got, f.libV1.SHA256)
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	warmedAfter := loadStoreSnapshot(t, f.cfg, f.runtime).WarmedArtifactSHAByKey()
	// A plain len() comparison would also pass if install rewrote both entries
	// with different (but still two) values, so each key is checked against
	// the exact sha recorded before install ran.
	if got := warmedAfter[appKey]; got != warmedBefore[appKey] {
		t.Errorf("warmedAfter[%q] = %q, want unchanged %q", appKey, got, warmedBefore[appKey])
	}
	if got := warmedAfter[libKey]; got != warmedBefore[libKey] {
		t.Errorf("warmedAfter[%q] = %q, want unchanged %q", libKey, got, warmedBefore[libKey])
	}
}

// TestWarmRewarmIsFullyCacheServed asserts that warming an already-warm cache
// a second time never touches the network: both the artifact cache and the
// extracted store are served straight from disk.
func TestWarmRewarmIsFullyCacheServed(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Warm (populate the cache): %v", err)
	}

	f.server.ResetCounts()
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Warm (re-warm): %v", err)
	}

	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after re-warm = %d, want 0 (cache-served, no HTTP at all)", got)
	}
}

// TestWarmFrozenHonorsLockfilePinAndFailsClosedOnCorruption asserts --frozen
// warm honors a lockfile pin (installing/caching the pinned version rather
// than a higher one also registered on the server), that a corrupted pin
// fails the whole warm run rather than warming drifted bytes, and that the
// cache is left un-poisoned by the failed pin check: a later unfrozen warm
// still serves acme.app entirely from the cache the first, honest run
// populated, proving the cached tarball was never corrupted.
func TestWarmFrozenHonorsLockfilePinAndFailsClosedOnCorruption(t *testing.T) {
	f := newE2EFixture(t)
	lockPath, lf := newFrozenPinFixture(t, f)

	// Sequential, not parallel subtests: the second reuses and mutates the
	// same lockfile the first already wrote, mirroring
	// TestFrozenInstallHonorsLockfilePins's own explicit ordering dependency.
	t.Run("pin overrides the highest available version", func(t *testing.T) {
		assertWarmFrozenHonorsPin(t, f)
	})
	t.Run("a corrupted pin fails closed without poisoning the cache", func(t *testing.T) {
		assertWarmFrozenCorruptedPinFailsClosed(t, f, lockPath, lf)
	})
}

// assertWarmFrozenHonorsPin runs a frozen warm and asserts it warms the
// lockfile's pinned acme.app@1.0.0 - not the higher 2.0.0 also registered on
// the server - without ever listing versions.
func assertWarmFrozenHonorsPin(t *testing.T, f *e2eFixture) {
	t.Helper()
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("frozen Warm honoring the pin: %v", err)
	}
	assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
	if got := f.server.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Errorf("EndpointVersionsList count = %d, want 0 (frozen resolution never consults the versions listing)", got)
	}
}

// assertWarmFrozenCorruptedPinFailsClosed corrupts acme.app's pin, asserts
// the resulting frozen warm fails the whole run, then restores the true pin
// and re-warms (still frozen): if the failed run had left the cache poisoned
// or missing an entry, this second warm - served entirely from
// lockfile-driven resolution plus an artifact-cache hit, with no pin
// mismatch this time - would have no choice but to hit the network to
// repair it. It must not.
func assertWarmFrozenCorruptedPinFailsClosed(t *testing.T, f *e2eFixture, lockPath string, lf *lockfile.File) {
	t.Helper()
	setAppPin(lf, corruptedAppSHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrupted lockfile: %v", err)
	}

	err := collections.Warm(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed for the corrupted pin, got %v", err)
	}

	setAppPin(lf, f.appV1.SHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("restore the true pin in the lockfile: %v", err)
	}

	f.server.ResetCounts()
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("recovery Warm after the corrupted-pin failure: %v", err)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after the recovery Warm = %d, want 0 (cache un-poisoned by the failed pin check)", got)
	}
}

// TestWarmOffline asserts --offline warm refuses to reach the network on a
// cold cache, and warms entirely from a warm cache with the network
// transport hard-disabled, mirroring TestOfflineInstall.
func TestWarmOffline(t *testing.T) {
	t.Parallel()

	t.Run("cold cache rejects the network", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.Offline = true
		f.runtime = infra.New(noopPrinter{}, fetch.NewOffline(f.cfg.Timeout))

		err := collections.Warm(context.Background(), f.cfg, f.runtime)
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

	t.Run("warm cache re-warms with the network hard disabled", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)

		if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("first Warm (populate the cache online): %v", err)
		}

		f.server.ResetCounts()
		f.runtime.HTTP = fetch.NewOffline(f.cfg.Timeout)
		f.cfg.Offline = true

		if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("second Warm (offline re-warm): %v", err)
		}
		if got := f.server.Total(); got != 0 {
			t.Errorf("Total() after the offline re-warm = %d, want 0", got)
		}
	})
}

// TestWarmPartialFailureKeepsSuccessfulCollectionCached asserts that a warm
// run in which one collection's artifact download persistently fails still
// caches (both tarball and extracted tree) the other, successful collection,
// still saves the snapshot, and still records nothing in the installed set -
// warm has no ordering dependency between collections and never calls
// recordInstall, so a partial failure must not corrupt or roll back the work
// that did succeed. It also asserts the corresponding warmed-set split: the
// successful acme.app has a warmed entry recording its extracted artifact
// sha, while the failed acme.lib has none - recordWarmed only ever runs after
// prepareWithRecovery/warmVerifyAndEnsure have already succeeded for that
// one collection.
func TestWarmPartialFailureKeepsSuccessfulCollectionCached(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	err := collections.Warm(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
	}

	assertArtifactFilePresent(t, f.cfg.CacheDir, f.cfg.Server, "acme-app-1.0.0.tar.gz")
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)

	st := loadStoreSnapshot(t, f.cfg, f.runtime)
	// Resolution always calls Store.SetRequirements, independent of any later
	// artifact-download outcome, so a non-empty snapshot here proves SaveStore
	// actually ran and persisted despite the partial failure - it is not just
	// a fresh, still-empty store created by LoadStore on a first read.
	if got := len(st.RequirementsSnapshot()); got == 0 {
		t.Errorf("RequirementsSnapshot is empty, want the snapshot to have been saved despite the partial failure")
	}
	if got := len(st.InstalledArtifactSHAByKey()); got != 0 {
		t.Errorf("installed set size = %d, want 0 (warm never calls recordInstall)", got)
	}

	warmed := st.WarmedArtifactSHAByKey()
	if got := warmed[collectionKey(f.appV1)]; got != f.appV1.SHA256 {
		t.Errorf("warmed[%q] = %q, want %q (the successful collection)", collectionKey(f.appV1), got, f.appV1.SHA256)
	}
	if _, ok := warmed[collectionKey(f.libV1)]; ok {
		t.Errorf("warmed[%q] present, want absent (the collection whose download persistently failed)", collectionKey(f.libV1))
	}
}

// collectionKey builds the ns.name@version snapshot key for a fakegalaxy
// Version, matching the unexported collection.key() format the production
// code stamps into the store.
func collectionKey(v fakegalaxy.Version) string {
	return v.Namespace + "." + v.Name + "@" + v.Version
}

// TestWarmLockReleasedAfterFailingRun asserts that a failing warm run still
// releases the backend's exclusive lock, proving a second Warm against the
// same cache directory is not left blocked behind the first run's lock.
func TestWarmLockReleasedAfterFailingRun(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	// Bounded to exactly the retry budget: the first Warm's own retries
	// exhaust this fault, so by the time it returns the rule is disarmed and
	// the second Warm below hits a clean server rather than needing its own
	// fault-clearing step.
	f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{
		Status: http.StatusServiceUnavailable,
		Count:  helpers.FetchRetryMaxAttempts,
	})

	err := collections.Warm(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("first Warm: expected errors.Is ErrInstallationFailed, got %v", err)
	}

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Warm after the failing run: %v (the first run's lock was not released)", err)
	}
}

// TestWarmNoCacheRejectsBeforeResolving asserts warm --no-cache is rejected
// as a usage error, before any resolution or network call, rather than
// silently downloading every artifact and discarding it uncommitted while
// reporting success.
func TestWarmNoCacheRejectsBeforeResolving(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.NoCache = true

	err := collections.Warm(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrWarmCacheDisabled) {
		t.Fatalf("expected errors.Is ErrWarmCacheDisabled, got %v", err)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() = %d, want 0 (--no-cache must reject before resolving anything)", got)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", got, exitcode.ExitUsage)
	}
}

// TestWarmDryRunRejectsBeforeAnythingOpens asserts warm --dry-run is refused
// as a usage error before the backend is even opened, rather than silently
// downloading and committing every artifact while announcing a normal warm -
// warm has no dry-run implementation, and an ambient GO_GALAXY_DRY_RUN must
// not make it do the opposite of what was asked.
func TestWarmDryRunRejectsBeforeAnythingOpens(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.cfg.DryRun = true

	err := collections.Warm(context.Background(), f.cfg, f.runtime)
	if !errors.Is(err, helpers.ErrDryRunUnsupported) {
		t.Fatalf("expected errors.Is ErrDryRunUnsupported, got %v", err)
	}
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() = %d, want 0 (--dry-run must reject before any network call)", got)
	}
	// The cache directory is never created: proof the backend was never
	// opened at all (cacheBackend.New/backend.Open both create it), not just
	// that no lock survived to be released.
	if _, statErr := os.Stat(f.cfg.CacheDir); !os.IsNotExist(statErr) {
		t.Errorf("expected cacheDir to never be created, stat error = %v", statErr)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", got, exitcode.ExitUsage)
	}
}

// TestWarmMetricsWrittenForSuccessAndFailure asserts a metrics file is
// produced with command == "warm" both when warm succeeds and when it fails,
// mirroring writeRunMetrics's unconditional call on both outcomes.
func TestWarmMetricsWrittenForSuccessAndFailure(t *testing.T) {
	t.Parallel()

	t.Run("successful warm", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")

		if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
			t.Fatalf("Warm: %v", err)
		}
		assertMetricsCommand(t, f.cfg.MetricsFile, "warm")
	})

	t.Run("failing warm", func(t *testing.T) {
		t.Parallel()
		f := newE2EFixture(t)
		f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")
		f.server.Fail(fakegalaxy.EndpointArtifact, "acme", "lib", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

		err := collections.Warm(context.Background(), f.cfg, f.runtime)
		if !errors.Is(err, helpers.ErrInstallationFailed) {
			t.Fatalf("expected errors.Is ErrInstallationFailed, got %v", err)
		}
		assertMetricsCommand(t, f.cfg.MetricsFile, "warm")
	})
}
