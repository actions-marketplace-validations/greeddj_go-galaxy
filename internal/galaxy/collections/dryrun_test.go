package collections

// This file covers dryrun.go's two shared helpers directly: classifyDryRun's
// deterministic report order, its checkInstalled gate (reusing
// installRecordMatches rather than a second, hand-rolled check), its cache-hit
// classification, and dryRunBanner's exclusive use of Warnf. installWithState's
// end-to-end dry-run substitution (no download, no install tree, no
// recordInstall, the snapshot still saved) is covered against a live fake
// Galaxy server in dry_run_e2e_test.go instead, since that is the level at
// which "nothing was downloaded" is actually observable.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestClassifyDryRunSortedOrder proves classifyDryRun's report is in sorted
// key order - not map order, and not completion order - by exercising the
// actual concurrent shape it runs under in production.
//
// Both the worker count and the key count below are load-bearing, not
// arbitrary: cfg.Workers is set to 8, wide enough that many probe goroutines
// are genuinely in flight together rather than one finishing before the next
// starts (a serial probe - the shape a Workers-less-than-2 config like
// &config.Config{} degrades to, since max(cfg.Workers, 1) then makes the
// semaphore capacity 1 - would coincidentally preserve dispatch order even
// with a completion-order bug, since only one goroutine ever runs at a
// time, so it would never catch the class of bug this test exists for). The
// key count (24) is wide enough that, across true 8-way concurrency,
// completion order almost certainly differs from dispatch order at least
// once per run. This was verified directly: a mutation that appends each
// goroutine's result under a mutex instead of writing it to its own
// pre-sized index (so the report order becomes completion order, not
// dispatch/sorted order) failed this exact test 5 times out of 5 with these
// parameters; a serial (Workers: 1, few keys) version of this test would not
// have caught that mutation at all.
func TestClassifyDryRunSortedOrder(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 8}

	const keyCount = 24
	cols := make(map[string]collection, keyCount)
	want := make([]string, keyCount)
	for i := range keyCount {
		// Zero-padded so lexicographic (sort.Strings) order equals the
		// generated numeric order, letting want be built in one straight
		// pass rather than pre-sorted by hand.
		name := fmt.Sprintf("c%02d", i)
		key := fmt.Sprintf("ns.%s@1.0.0", name)
		cols[key] = collection{Namespace: "ns", Name: name, Version: "1.0.0"}
		want[i] = fmt.Sprintf("Would install: %s (would download)", key)
	}

	for i := range 15 {
		printer := &capturingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)

		// artifacts is nil throughout: dryRunArtifactCached treats a nil store as
		// "not cached", which is the only classification this test needs and
		// avoids depending on any real cache state.
		classifyDryRun(context.Background(), runtime, cfg, nil, nil, cols, false)

		got := printer.okLines()
		if len(got) != len(want) {
			t.Fatalf("iteration %d: okLines has %d entries, want %d", i, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: okLines[%d] = %q, want %q (report order must be sorted, not completion order)", i, j, got[j], want[j])
			}
		}
	}
}

// TestClassifyDryRunReportsCacheHitVsMiss proves classifyDryRun distinguishes
// a collection whose artifact tarball is already cached from one that has
// never been fetched, reusing artifactExists/artifactKey rather than a
// duplicated lookup. acme.app is installed for real first (populating its
// cache entry); acme.other is registered on the server but never touched, so
// its artifact cache entry never exists.
func TestClassifyDryRunReportsCacheHitVsMiss(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	srv.AddVersion("acme", "other", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (populate acme.app's cache entry): %v", err)
	}

	cols := map[string]collection{
		"acme.app@1.0.0":   {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
		"acme.other@1.0.0": {Namespace: "acme", Name: "other", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	classifyDryRun(context.Background(), reportRuntime, cfg, state.store, state.backend.Artifacts(), cols, false)

	if !printer.hasOkContaining("acme.app@1.0.0 (artifact cached)") {
		t.Errorf("expected acme.app reported as cached, got okLines %v", printer.okLines())
	}
	if !printer.hasOkContaining("acme.other@1.0.0 (would download)") {
		t.Errorf("expected acme.other reported as would-download, got okLines %v", printer.okLines())
	}
	if !printer.hasPersistentPrintContaining("2 would install, 0 already up to date") {
		t.Errorf("expected a summary line counting both collections as would-install, got %v", printer.persists)
	}
}

// TestClassifyDryRunCheckInstalledMarksUpToDate proves checkInstalled=true
// reports a collection whose on-disk install already satisfies
// installRecordMatches as already up to date, not as "would install", and
// that this reuses installRecordMatches rather than a second, independent
// check: acme.app is installed for real, then reported on with the exact
// same collection/installPath installRecordMatches itself would be given at
// real install time.
func TestClassifyDryRunCheckInstalledMarksUpToDate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	cols := map[string]collection{
		"acme.app@1.0.0": {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	classifyDryRun(context.Background(), reportRuntime, cfg, state.store, state.backend.Artifacts(), cols, true)

	if !printer.hasPersistentPrintContaining("Up to date: acme.app@1.0.0") {
		t.Errorf("expected acme.app reported as up to date, got %v", printer.persists)
	}
	if len(printer.okLines()) != 0 {
		t.Errorf("expected no \"would install\" line for an already-satisfied install, got %v", printer.okLines())
	}
	if !printer.hasPersistentPrintContaining("0 would install, 1 already up to date") {
		t.Errorf("expected a summary line counting the collection as already up to date, got %v", printer.persists)
	}
}

// TestClassifyDryRunMirrorsIsCacheHitUnderNoCache proves dryRunArtifactCached
// mirrors isCacheHit's own --no-cache guard rather than a bare artifact-store
// probe: a warm cache under --no-cache must still be reported as
// "would download", since that is what a real install would actually do
// (isCacheHit itself returns false whenever cfg.NoCache is set). Before this
// guard existed, this exact configuration made classifyDryRun claim the
// artifact was cached while the real run would still hit the network.
func TestClassifyDryRunMirrorsIsCacheHitUnderNoCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (populate acme.app's cache entry): %v", err)
	}

	// Report against the same warm cache, but with --no-cache now set: a real
	// install against this cfg would not read the cache at all.
	cfg.NoCache = true
	cols := map[string]collection{
		"acme.app@1.0.0": {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	classifyDryRun(context.Background(), reportRuntime, cfg, state.store, state.backend.Artifacts(), cols, false)

	if !printer.hasOkContaining("acme.app@1.0.0 (would download)") {
		t.Errorf("expected acme.app reported as would-download under --no-cache despite a warm cache, got %v", printer.okLines())
	}
	if printer.hasOkContaining("artifact cached") {
		t.Errorf("expected no \"artifact cached\" report under --no-cache, got %v", printer.okLines())
	}
}

// TestClassifyDryRunNeverDeletesDriftedExtractMarker proves classifyDryRun
// detects a drifted installed tree exactly like a real install would - via
// checkExtractMarker, the pure predicate marker.go factored out of
// verifyExtractMarker - while never calling verifyExtractMarker itself (which
// would delete the drifted marker as a side effect of deciding to
// re-extract). After installing acme.app for real and then drifting its
// installed tree (adding a file), this proves both halves of the same
// property: the verdict is correct (reported as "would install", exactly
// what a real install would also decide, not an optimistic "up to date")
// AND the marker survives untouched (no deletion, unlike verifyExtractMarker).
func TestClassifyDryRunNeverDeletesDriftedExtractMarker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	entry, ok := state.store.GetInstalled("acme.app@1.0.0")
	if !ok {
		t.Fatalf("expected acme.app to be recorded installed")
	}
	markerPath := filepath.Join(entry.InstallPath, helpers.ExtractMarkerPrefix+entry.ArtifactSHA256)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected the extract marker to exist right after install, stat error: %v", err)
	}

	// Drift the installed tree: a real install's canSkipInstall would detect
	// this via a changed tally (through verifyExtractMarker) and delete the
	// marker as part of deciding to re-extract. classifyDryRun must detect
	// the same drift (through checkExtractMarker) but never delete the
	// marker itself.
	mustWriteFile(t, filepath.Join(entry.InstallPath, "drifted-file.txt"), []byte("unexpected"))

	cols := map[string]collection{
		"acme.app@1.0.0": {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	classifyDryRun(context.Background(), reportRuntime, cfg, state.store, state.backend.Artifacts(), cols, true)

	if printer.hasPersistentPrintContaining("Up to date") {
		t.Errorf("expected the drifted install NOT reported up to date, got %v", printer.persists)
	}
	if !printer.hasOkContaining("acme.app@1.0.0") {
		t.Errorf("expected the drifted install reported as would-install, got okLines %v", printer.okLines())
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("expected the extract marker to survive classifyDryRun despite the drift, stat error: %v", err)
	}
}

// newDriftedOfflineEvictedFixture installs acme.app for real, then evicts its
// cached artifact (tarball plus sha256 sidecar, exactly as cleanup would for
// an unreachable key) and drifts its installed tree by adding one file -
// the exact combination TestInstallDryRunDriftedOfflineEvictedReportsWouldFailAndFails
// needs. cfg.Offline is left false; the caller sets it once the fixture is
// ready, and the returned cols map is keyed exactly like classifyDryRun and
// installDryRun expect.
func newDriftedOfflineEvictedFixture(t *testing.T) (*config.Config, *installState, map[string]collection) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	entry, ok := state.store.GetInstalled("acme.app@1.0.0")
	if !ok {
		t.Fatalf("expected acme.app to be recorded installed")
	}

	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()}
	if err := state.backend.Artifacts().Delete(context.Background(), artifactKey(col)); err != nil {
		t.Fatalf("evict cached artifact: %v", err)
	}
	mustWriteFile(t, filepath.Join(entry.InstallPath, "drifted-file.txt"), []byte("unexpected"))

	return cfg, state, map[string]collection{"acme.app@1.0.0": col}
}

// assertReportsSingleWouldFail asserts classifyDryRun reported exactly one
// would-fail collection, named key, on the Errorf tier, and reported it on
// neither the "up to date" nor the "would install" tier.
func assertReportsSingleWouldFail(t *testing.T, printer *capturingPrinter, wouldFail int, key string) {
	t.Helper()
	if wouldFail != 1 {
		t.Fatalf("expected classifyDryRun to report 1 would-fail collection, got %d", wouldFail)
	}
	if !printer.hasErrContaining("Would fail: " + key) {
		t.Errorf("expected a \"Would fail: %s\" line, got %v", key, printer.errs)
	}
	if printer.hasPersistentPrintContaining("Up to date") || printer.hasOkContaining(key) {
		t.Errorf("expected no \"up to date\" or \"would install\" report for %s, got persists=%v oks=%v",
			key, printer.persists, printer.okLines())
	}
}

// assertFailsOfflineClosed asserts err is non-nil and matches both
// helpers.ErrInstallationFailed and helpers.ErrOfflineMode: the same
// classification a real failed --offline install returns, since
// cmd/go-galaxy/exitcode checks isInstallError ahead of isNetworkError.
func assertFailsOfflineClosed(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error for a would-fail collection")
	}
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Errorf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Errorf("expected errors.Is helpers.ErrOfflineMode, got %v", err)
	}
}

// TestInstallDryRunDriftedOfflineEvictedReportsWouldFailAndFails closes the
// specific preview/run disagreement this test was written to catch: a
// collection that looks installed by the cheap check but whose tree has
// drifted, whose cached artifact has been evicted (as cleanup would evict
// it), and that cannot be re-downloaded because --offline is set. A real
// install would re-extract (drift detected) and then fail to fetch a
// replacement artifact; the dry run must report the same "would fail"
// verdict and return the same error class, not the optimistic "up to date"
// this exact scenario used to produce before checkExtractMarker replaced the
// installRecordMatches-only gate in classifyOneDryRun.
func TestInstallDryRunDriftedOfflineEvictedReportsWouldFailAndFails(t *testing.T) {
	t.Parallel()
	cfg, state, cols := newDriftedOfflineEvictedFixture(t)
	cfg.Offline = true

	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, http.DefaultClient)
	wouldFail := classifyDryRun(context.Background(), reportRuntime, cfg, state.store, state.backend.Artifacts(), cols, true)
	assertReportsSingleWouldFail(t, printer, wouldFail, "acme.app@1.0.0")

	// installDryRun's own error-wrap must classify identically to a real
	// failed install.
	plan := &installPlan{collections: cols}
	err := installDryRun(context.Background(), cfg, reportRuntime, state, plan, time.Now())
	assertFailsOfflineClosed(t, err)
}

// TestClassifyDryRunCheckInstalledFalseIgnoresInstallState proves that
// checkInstalled=false - the shape a future warm caller will use - never
// consults installRecordMatches at all: even an actually-installed
// collection is reported as "would install", since warm tracks no install
// path to compare against.
func TestClassifyDryRunCheckInstalledFalseIgnoresInstallState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())
	if err := installWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("installWithState (perform the real install): %v", err)
	}

	cols := map[string]collection{
		"acme.app@1.0.0": {Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()},
	}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	classifyDryRun(context.Background(), reportRuntime, cfg, state.store, state.backend.Artifacts(), cols, false)

	if len(printer.okLines()) != 1 {
		t.Fatalf("expected exactly one would-install line, got %v", printer.okLines())
	}
	if printer.hasPersistentPrintContaining("Up to date") {
		t.Errorf("checkInstalled=false must never report \"up to date\", got %v", printer.persists)
	}
}

// TestDryRunBannerOnlyWarns proves dryRunBanner emits exactly one line,
// through Warnf and nothing else. Warnf is the tier that always writes to
// stderr and always survives --quiet (see internal/progress's own Warnf
// behavior); routing the banner through any other tier would let a quiet
// dry run announce itself nowhere at all.
func TestDryRunBannerOnlyWarns(t *testing.T) {
	t.Parallel()
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	dryRunBanner(runtime)

	if len(printer.warns) != 1 {
		t.Fatalf("expected exactly one Warnf line, got %v", printer.warns)
	}
	if !printer.hasWarnContaining("--dry-run") {
		t.Errorf("expected the banner to mention --dry-run, got %q", printer.warns[0])
	}
	if len(printer.prints) != 0 || len(printer.persists) != 0 || len(printer.oks) != 0 {
		t.Errorf("expected no output on any other tier, got prints=%v persists=%v oks=%v", printer.prints, printer.persists, printer.oks)
	}
}

// TestInitInstallDryRunSkipsClearCache proves --clear-cache is suppressed
// under a dry run, with a warning explaining why, since deleting cached
// artifacts is exactly the kind of destructive mutation --dry-run exists to
// prevent.
func TestInitInstallDryRunSkipsClearCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	// A file directly under cacheDir, named like a real cached artifact
	// (store.ClearCacheFiles only deletes files matching its own
	// shouldDeleteCacheFile patterns, ".tar.gz" among them), stands in for a
	// cached artifact: its survival is what proves ClearFiles was never
	// called, not a filename ClearFiles would have ignored anyway.
	sentinelPath := filepath.Join(cacheDir, "sentinel.acme-app-1.0.0.tar.gz")
	mustWriteFile(t, sentinelPath, []byte("cached bytes"))

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		ClearCache:       true,
		DryRun:           true,
		Workers:          1,
	}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	state, err := initInstall(context.Background(), cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})

	if _, statErr := os.Stat(sentinelPath); statErr != nil {
		t.Errorf("expected the cache sentinel file to survive a dry run's --clear-cache, stat error: %v", statErr)
	}
	if !printer.hasWarnContaining("--clear-cache") {
		t.Errorf("expected a warning naming --clear-cache, got %v", printer.warns)
	}
}

// TestInitInstallDryRunSkipsRecordProject proves a dry run never enrolls the
// project in the persistent project registry: RecordProject feeds cleanup,
// a destructive command, so a dry run against a broken requirements.yml must
// not be able to poison every future cleanup run on a shared cache with a
// project that never actually installed anything.
func TestInitInstallDryRunSkipsRecordProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		DryRun:           true,
		Workers:          1,
	}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	state, err := initInstall(context.Background(), cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})

	registry, err := store.LoadProjectRegistry(cacheDir)
	if err != nil {
		t.Fatalf("store.LoadProjectRegistry: %v", err)
	}
	if len(registry.Projects) != 0 {
		t.Errorf("expected an empty project registry after a dry run, got %d entries: %+v", len(registry.Projects), registry.Projects)
	}
}

// TestWriteRunMetricsDryRunSkipsAndWarns proves writeRunMetrics's own
// cfg.DryRun guard: with a configured MetricsFile, a dry run never writes it
// and instead warns, since a dry run's counters would be indistinguishable
// from a real run's in the report's wire shape. This guard lives inside
// writeRunMetrics itself, so install, warm, and lock all inherit it.
func TestWriteRunMetricsDryRunSkipsAndWarns(t *testing.T) {
	t.Parallel()
	metricsPath := filepath.Join(t.TempDir(), "metrics.json")
	cfg := &config.Config{MetricsFile: metricsPath, DryRun: true}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	writeRunMetrics(cfg, runtime, "install", time.Now(), 1, 0, false)

	if _, statErr := os.Stat(metricsPath); !os.IsNotExist(statErr) {
		t.Errorf("expected no metrics file written by a dry run, stat error = %v", statErr)
	}
	if !printer.hasWarnContaining(metricsPath) {
		t.Errorf("expected a warning naming the skipped metrics path, got %v", printer.warns)
	}
}
