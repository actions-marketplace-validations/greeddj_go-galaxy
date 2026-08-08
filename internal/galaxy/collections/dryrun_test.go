package collections

// This file covers dryrun.go's shared machinery directly: classifyDryRun's
// deterministic report order, installDryRunProbe's settled gate (reusing
// installRecordMatches rather than a second, hand-rolled check), warmDryRunProbe's
// independence from install state, the shared cache-hit classification, and
// dryRunBanner's exclusive use of Warnf. installWithState's and warmWithState's
// end-to-end dry-run substitutions (no download, no install tree, no
// recordInstall/recordWarmed, the snapshot still saved) are covered against a
// live fake Galaxy server in dry_run_e2e_test.go instead, since that is the
// level at which "nothing was downloaded" is actually observable.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

		// artifacts is nil throughout: dryRunArtifactMeta treats a nil store as
		// "not cached", which is the only classification this test needs and
		// avoids depending on any real cache state. root is nil too - cfg has no
		// DownloadPath, and a nil root already makes installRecordMatches
		// unreachable through newInstallTarget's own nil-root guard, so there is
		// nothing for a real root to add here.
		classifyDryRun(context.Background(), runtime, cfg, cols, installDryRunVerbs, installDryRunProbe(cfg, nil, nil, nil))

		got := printer.okLines()
		if len(got) != len(want) {
			t.Fatalf("iteration %d: okLines has %d entries, want %d", i, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: okLines[%d] = %q, want %q (report order must be sorted, not completion order)", i, j, got[j], want[j])
			}
		}
		// installDryRunVerbs.summaryAction/summarySettled assemble this line at
		// runtime now, rather than it being a single literal format string, so
		// this pins install's summary wording as byte-identical to what it was
		// before dryRunVerbs existed.
		wantSummary := fmt.Sprintf("Dry run: %d would install, 0 already up to date, 0 would fail", keyCount)
		if !printer.hasPersistentPrintContaining(wantSummary) {
			t.Fatalf("iteration %d: expected persistent print containing %q, got %v", i, wantSummary, printer.persists)
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
	// A fresh, empty store - not state.store, which really does hold acme.app's
	// installed record - so the probe's install-record arm can never match
	// either collection, isolating the cache-hit classification this test is
	// about from acme.app's genuine install record.
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, store.New(), state.backend.Artifacts(), installRoot)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)

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

// errSwitchOrderProbeStub is a sentinel distinct from helpers.ErrOfflineMode,
// so TestReportDryRunResultsOfflineGuardOutranksProbeFailure and its positive
// control below can tell which of the two reportDryRunResults actually
// recorded, rather than the two colliding under errors.Is.
var errSwitchOrderProbeStub = errors.New("stub probe failure, must not surface when the offline guard applies")

// TestReportDryRunResultsOfflineGuardOutranksProbeFailure pins
// reportDryRunResults' own documented switch order: its !res.cached &&
// cfg.Offline case is checked before its res.fail != nil case, so an uncached
// collection under --offline whose probe ALSO returned a non-nil fail is
// reported and recorded under the offline cause, never the probe's own fail.
// This is the only state that discriminates the two orderings - every other
// fixture in this file drives its probe online - so this drives classifyDryRun
// directly with a stub probe fixed to exactly that state instead of relying
// on any real probe to ever reach it.
//
// TestReportDryRunResultsReportsProbeFailureWhenCached is this test's
// required positive control, on the identical fixture: it proves the offline
// cause winning above is a genuine ordering effect of a state that can report
// either cause, not a stub that always reports the offline one regardless of
// what it is given.
func TestReportDryRunResultsOfflineGuardOutranksProbeFailure(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 1, Offline: true}
	col := collection{Namespace: "ns", Name: "app", Version: "1.0.0"}
	cols := map[string]collection{col.key(): col}
	probe := func(context.Context, collection) dryRunClassification {
		return dryRunClassification{cached: false, fail: errSwitchOrderProbeStub}
	}

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	summary := classifyDryRun(context.Background(), runtime, cfg, cols, installDryRunVerbs, probe)

	if summary.count != 1 {
		t.Fatalf("summary.count = %d, want 1", summary.count)
	}
	if !errors.Is(summary.cause, helpers.ErrOfflineMode) {
		t.Errorf("expected the recorded cause to be helpers.ErrOfflineMode, got %v", summary.cause)
	}
	if errors.Is(summary.cause, errSwitchOrderProbeStub) {
		t.Errorf("expected the probe's own fail to be shadowed by the offline guard, got %v", summary.cause)
	}
	if !printer.hasErrContaining("not cached and --offline forbids downloading") {
		t.Errorf("expected the offline \"Would fail\" line, got errs=%v", printer.errs)
	}
	if printer.hasErrContaining(errSwitchOrderProbeStub.Error()) {
		t.Errorf("expected the probe's own error text never to reach stderr, got errs=%v", printer.errs)
	}
}

// TestReportDryRunResultsReportsProbeFailureWhenCached is the positive
// control TestReportDryRunResultsOfflineGuardOutranksProbeFailure's own doc
// comment requires: on the identical fixture, with the collection reported
// cached instead of uncached, the offline case no longer applies and the
// probe's own fail is what wins.
func TestReportDryRunResultsReportsProbeFailureWhenCached(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 1, Offline: true}
	col := collection{Namespace: "ns", Name: "app", Version: "1.0.0"}
	cols := map[string]collection{col.key(): col}
	probe := func(context.Context, collection) dryRunClassification {
		return dryRunClassification{cached: true, fail: errSwitchOrderProbeStub}
	}

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	summary := classifyDryRun(context.Background(), runtime, cfg, cols, installDryRunVerbs, probe)

	if summary.count != 1 {
		t.Fatalf("summary.count = %d, want 1", summary.count)
	}
	if !errors.Is(summary.cause, errSwitchOrderProbeStub) {
		t.Errorf("expected the recorded cause to be the probe's own fail, got %v", summary.cause)
	}
	if errors.Is(summary.cause, helpers.ErrOfflineMode) {
		t.Errorf("expected no offline cause once the collection is reported cached, got %v", summary.cause)
	}
	if !printer.hasErrContaining(errSwitchOrderProbeStub.Error()) {
		t.Errorf("expected the probe's own \"Would fail\" line, got errs=%v", printer.errs)
	}
	if printer.hasErrContaining("not cached and --offline forbids downloading") {
		t.Errorf("expected no offline line once the collection is reported cached, got errs=%v", printer.errs)
	}
}

// TestInstallDryRunProbeMarksUpToDate proves installDryRunProbe reports a
// collection whose on-disk install already satisfies installRecordMatches as
// already up to date, not as "would install", and that this reuses
// installRecordMatches rather than a second, independent check: acme.app is
// installed for real, then reported on with the exact same
// collection/installPath installRecordMatches itself would be given at real
// install time.
func TestInstallDryRunProbeMarksUpToDate(t *testing.T) {
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
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, state.store, state.backend.Artifacts(), installRoot)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)

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

// TestClassifyDryRunMirrorsIsCacheHitUnderNoCache proves dryRunArtifactMeta
// mirrors isCacheHit's own --no-cache guard rather than a bare artifact-store
// probe: a warm cache under --no-cache must still be reported as
// "would download", since that is what a real install would actually do
// (isCacheHit itself returns false whenever cfg.NoCache is set). Without
// that mirroring, this exact configuration would make classifyDryRun claim
// the artifact was cached even though the real run would still hit the
// network.
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
	// A fresh, empty store - not state.store, which really does hold acme.app's
	// installed record - so the probe's install-record arm can never match,
	// isolating the --no-cache classification this test is about from
	// acme.app's genuine install record.
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, store.New(), state.backend.Artifacts(), installRoot)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)

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
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, state.store, state.backend.Artifacts(), installRoot)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)

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
func assertReportsSingleWouldFail(t *testing.T, printer *capturingPrinter, summary failureSummary, key string) {
	t.Helper()
	if summary.count != 1 {
		t.Fatalf("expected classifyDryRun to report 1 would-fail collection, got %d", summary.count)
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

// TestInstallDryRunDriftedOfflineEvictedReportsWouldFailAndFails pins the
// preview/run agreement for a collection that looks installed by the cheap
// check but whose tree has drifted, whose cached artifact has been evicted
// (as cleanup would evict it), and that cannot be re-downloaded because
// --offline is set. A real install would re-extract (drift detected) and
// then fail to fetch a replacement artifact; the dry run must report the
// same "would fail" verdict and return the same error class, not an
// optimistic "up to date" - which is exactly what installDryRunProbe's
// installRecordMatches-only gate alone would report, since checkExtractMarker
// is what additionally catches the drift.
func TestInstallDryRunDriftedOfflineEvictedReportsWouldFailAndFails(t *testing.T) {
	t.Parallel()
	cfg, state, cols := newDriftedOfflineEvictedFixture(t)
	cfg.Offline = true

	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, http.DefaultClient)
	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, state.store, state.backend.Artifacts(), installRoot)
	summary := classifyDryRun(context.Background(), reportRuntime, cfg, cols, installDryRunVerbs, probe)
	assertReportsSingleWouldFail(t, printer, summary, "acme.app@1.0.0")

	// installDryRun's own error-wrap must classify identically to a real
	// failed install.
	plan := &installPlan{collections: cols}
	err := installDryRun(context.Background(), cfg, reportRuntime, state, plan, time.Now(), installRoot)
	assertFailsOfflineClosed(t, err)
}

// TestWarmDryRunProbeIgnoresInstallState proves warmDryRunProbe never
// consults install state at all: a collection with a valid install record and
// a matching extract marker - installDryRunProbe's own settled case - is
// still reported "Would warm" once its cached artifact is evicted, since
// warm's product is the artifact cache plus the extracted tree, not an
// install path warm tracks nothing about.
func TestWarmDryRunProbeIgnoresInstallState(t *testing.T) {
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

	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL()}
	// Evict the cached artifact even though the install itself (and its extract
	// marker) still stands: this is the one state where installDryRunProbe would
	// call the collection settled (a matching install record) but warm's own
	// product - the artifact cache - is absent.
	if err := state.backend.Artifacts().Delete(context.Background(), artifactKey(col)); err != nil {
		t.Fatalf("evict cached artifact: %v", err)
	}

	cols := map[string]collection{"acme.app@1.0.0": col}
	printer := &capturingPrinter{}
	reportRuntime := infra.New(printer, srv.Client())
	warmed := state.store.WarmedArtifactSHAByKey()
	probe := warmDryRunProbe(cfg, state.backend.Artifacts(), state.extractStore, warmed)
	classifyDryRun(context.Background(), reportRuntime, cfg, cols, warmDryRunVerbs, probe)

	if !printer.hasOkContaining("Would warm: acme.app@1.0.0 (would download)") {
		t.Errorf("expected acme.app reported as would-warm, got okLines %v", printer.okLines())
	}
	if printer.hasPersistentPrintContaining("Already warm") {
		t.Errorf("warmDryRunProbe must never report \"Already warm\" from install state alone, got %v", printer.persists)
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

// TestDryRunBannerEmittedExactlyOnceAcrossCommands proves initInstall's
// hoisted dryRunBanner call fires exactly once per run, for both install and
// warm, rather than once per call site the way it did before the hoist (only
// installWithState's own cfg.DryRun branch printed it).
func TestDryRunBannerEmittedExactlyOnceAcrossCommands(t *testing.T) {
	t.Parallel()

	assertBannerOnce := func(t *testing.T, run func(context.Context, *config.Config, *infra.Infra) error) {
		t.Helper()
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
			DryRun:           true,
		}
		printer := &capturingPrinter{}
		runtime := infra.New(printer, srv.Client())

		if err := run(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("run: %v", err)
		}
		count := 0
		for _, w := range printer.warns {
			if strings.Contains(w, "--dry-run is active") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("banner warn count = %d, want exactly 1, warns=%v", count, printer.warns)
		}
	}

	t.Run("install", func(t *testing.T) {
		t.Parallel()
		assertBannerOnce(t, Start)
	})
	t.Run("warm", func(t *testing.T) {
		t.Parallel()
		assertBannerOnce(t, Warm)
	})
}

// TestInitInstallDryRunSkipsClearCache proves --clear-cache is suppressed
// under a dry run, with a warning explaining why, since deleting cached
// artifacts is exactly the kind of destructive mutation --dry-run exists to
// prevent. Its positive control is
// TestInitInstallClearCacheWipesArtifactsAndMetadataCaches: this test
// proves only that the branch is skipped, which says nothing about what
// the branch does, so that test proves what a real (non-dry-run) run
// actually wipes and keeps.
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

	_, state, err := initInstall(context.Background(), cfg, runtime)
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

	_, state, err := initInstall(context.Background(), cfg, runtime)
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

// countingArtifactMetaCalls is a stub cacheManager.ArtifactStore that counts,
// per artifact key, how many times Has and Meta were each called - proving
// dryRunArtifactMeta replaces the dry run's former Has() round trip rather
// than adding a second one alongside it (see
// TestClassifyDryRunCallsMetaExactlyOncePerCollectionNeverHas). Fetch,
// TempFile, Commit, and Delete are never called by a dry-run probe, so they
// return errStubNotImplemented (declared in prefetch_scan_test.go) to make an
// accidental call fail loudly instead of silently.
type countingArtifactMetaCalls struct {
	metaCalls map[string]int
	hasCalls  map[string]int
	present   map[string]bool
	mu        sync.Mutex
}

func (a *countingArtifactMetaCalls) Has(_ context.Context, key string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hasCalls[key]++
	return a.present[key], nil
}

func (a *countingArtifactMetaCalls) Meta(_ context.Context, key string) (map[string]string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.metaCalls[key]++
	return nil, a.present[key], nil
}

func (a *countingArtifactMetaCalls) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *countingArtifactMetaCalls) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errStubNotImplemented
}

func (a *countingArtifactMetaCalls) Commit(context.Context, string, string, map[string]string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *countingArtifactMetaCalls) Delete(context.Context, string) error {
	return errStubNotImplemented
}

// TestClassifyDryRunCallsMetaExactlyOncePerCollectionNeverHas is the direct
// mechanical proof behind dryRunArtifactMeta's own doc comment claim: on the
// S3 backend, replacing the dry run's former Has() call with a Meta() call
// costs nothing extra, because production code calls ArtifactStore.Meta at
// most once per collection - once for a collection that reaches the artifact
// probe, never for one already reported settled - and never calls Has at
// all. This fixture's nil root makes nothing settled, so every collection
// here does reach the probe; collections alternate cached/uncached so both of
// dryRunArtifactMeta's branches run under the identical assertion.
func TestClassifyDryRunCallsMetaExactlyOncePerCollectionNeverHas(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 4}

	const keyCount = 12
	cols := make(map[string]collection, keyCount)
	artifacts := &countingArtifactMetaCalls{
		metaCalls: make(map[string]int),
		hasCalls:  make(map[string]int),
		present:   make(map[string]bool),
	}
	for i := range keyCount {
		name := fmt.Sprintf("c%02d", i)
		col := collection{Namespace: "ns", Name: name, Version: "1.0.0"}
		cols[col.key()] = col
		artifacts.present[artifactKey(col)] = i%2 == 0
	}

	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	// root is nil: cfg has no DownloadPath, so newInstallTarget's own
	// nil-root guard makes every collection report ok=false, keeping this
	// test isolated to the artifact-cache probe this test is about.
	probe := installDryRunProbe(cfg, store.New(), artifacts, nil)
	classifyDryRun(context.Background(), runtime, cfg, cols, installDryRunVerbs, probe)

	artifacts.mu.Lock()
	defer artifacts.mu.Unlock()
	for key, col := range cols {
		ak := artifactKey(col)
		if got := artifacts.metaCalls[ak]; got != 1 {
			t.Errorf("collection %s: Meta call count = %d, want exactly 1", key, got)
		}
		if got := artifacts.hasCalls[ak]; got != 0 {
			t.Errorf("collection %s: Has call count = %d, want 0 (a dry run must never call Has directly)", key, got)
		}
	}
}

// pinVerdictTestPin and pinVerdictTestRecorded are two well-formed but
// distinct 64-char lowercase hex digests, used by both
// TestDryRunPinVerdictSuppressedWhenOnline and
// TestDryRunPinVerdictSuppressedForSettledCollection as a lockfile
// pin/recorded-digest pair that disagrees.
const (
	pinVerdictTestPin      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinVerdictTestRecorded = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestDryRunPinVerdictSuppressedWhenOnline proves dryRunPinVerdict's
// !cfg.Offline arm: a cached artifact whose recorded digest disagrees with
// the lockfile pin produces no verdict while online, since a real run's own
// canRetryCacheHit can still evict and refetch a mismatched cache hit in
// that case - the disagreement is a cost (one wasted refetch), never a
// certain failure, so reporting one here would be the wrong answer. The
// positive control is TestDryRunPinVerdictSuppressedForSettledCollection's
// own fixture-sanity check, which proves the identical two digests DO
// produce a verdict once cfg.Offline is true - so this test is not passing
// merely because dryRunPinVerdict never fires for any input.
func TestDryRunPinVerdictSuppressedWhenOnline(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Offline: false}
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", SHA256: pinVerdictTestPin}
	meta := map[string]string{"sha256": pinVerdictTestRecorded}

	if err := dryRunPinVerdict(cfg, true, meta, col); err != nil {
		t.Fatalf("expected no verdict while online, got %v", err)
	}
}

// TestDryRunPinVerdictSuppressedForSettledCollection proves
// installDryRunProbe's settled check runs, and returns, before
// dryRunPinVerdict is ever consulted: acme.app is installed for real first
// (a matching install record, a matching extract marker, and a cache
// sidecar digest equal to the lockfile pin), and only then is the artifact
// cache's SIDECAR alone - not the tarball, not the store's own installed
// record, not the extract marker - overwritten with a different, well-formed
// digest. The probe must still report the collection settled, with no fail,
// because installRecordMatches (installEntryMatches, specifically) already
// required entry.ArtifactSHA256 == col.SHA256 before this probe ever reaches
// the artifact cache at all - see installDryRunProbe's own doc comment for
// why settled is checked first.
//
// This is non-vacuous: the fixture-sanity check below calls dryRunPinVerdict
// directly against the identical pin/recorded-digest pair, under the
// identical --offline config, and requires it to fire.
//
// Verified against a real mutation that drops the settled branch's early
// return (letting the function fall through to the pin check regardless of
// a settled match, while discarding the now-unused entry): this test failed
// with "expected the collection to be reported settled despite the drifted
// sidecar, got {fail:0x... settled:false cached:true}" followed by
// "expected no fail verdict for a settled collection, got sha256 mismatch:
// the cached artifact's recorded digest does not match the lockfile pin and
// --offline forbids refetching" - both assertions below this comment fire,
// confirming they pin the settled-first order rather than restating
// dryRunPinVerdict's own contract a second time.
func TestDryRunPinVerdictSuppressedForSettledCollection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	version := srv.AddVersion("acme", "app", "1.0.0", nil)

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

	// The probe below runs under --offline: that is the one config where
	// dryRunPinVerdict can fire at all (its own !cfg.Offline arm suppresses
	// every online disagreement), so this is the config that actually
	// exercises the settled short-circuit rather than vacuously passing
	// through the offline guard.
	cfg.Offline = true
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", Source: srv.URL(), SHA256: version.SHA256}

	sidecarPath := filepath.Join(cacheDir, artifactKey(col)) + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(pinVerdictTestRecorded), helpers.FileMod); err != nil {
		t.Fatalf("drift the cache sidecar: %v", err)
	}

	if err := dryRunPinVerdict(cfg, true, map[string]string{"sha256": pinVerdictTestRecorded}, col); err == nil {
		t.Fatal("fixture sanity: expected dryRunPinVerdict to fire directly for the drifted sidecar under --offline")
	}

	installRoot := newTestCollectionsRoot(t, cfg.DownloadPath)
	probe := installDryRunProbe(cfg, state.store, state.backend.Artifacts(), installRoot)
	got := probe(context.Background(), col)
	if !got.settled {
		t.Errorf("expected the collection to be reported settled despite the drifted sidecar, got %+v", got)
	}
	if got.fail != nil {
		t.Errorf("expected no fail verdict for a settled collection, got %v", got.fail)
	}
}

// TestDryRunPinVerdictNoVerdictArms is a table-driven proof of every "no
// verdict" arm dryRunPinVerdict's own doc comment names, other than the
// !cfg.Offline arm (pinned separately by
// TestDryRunPinVerdictSuppressedWhenOnline) and the recorded-equals-pin arm
// (pinned by TestWarmDryRunAndRunDisagreeOnAFrozenOfflineDriftedCacheHit's
// own drifted-bytes fixture): an empty pin, an uncached collection, an empty
// recorded digest, and a recorded digest that is not helpers.IsSHA256Hex all
// produce nil under --offline, the one config where a verdict could
// otherwise fire. The positive control in the same table (matching row) is
// what proves the config itself is capable of producing a verdict, so a nil
// result on every other row is a real refusal rather than evidence the
// helper never fires at all.
func TestDryRunPinVerdictNoVerdictArms(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Offline: true}

	for _, tt := range dryRunPinVerdictNoVerdictCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := dryRunPinVerdict(cfg, tt.cached, tt.meta, tt.col)
			if tt.wantErr && err == nil {
				t.Fatal("expected a verdict, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no verdict, got %v", err)
			}
		})
	}
}

// dryRunPinVerdictNoVerdictCases is the table
// TestDryRunPinVerdictNoVerdictArms runs, factored out purely to keep that
// function itself short.
func dryRunPinVerdictNoVerdictCases() []struct {
	meta    map[string]string
	name    string
	col     collection
	cached  bool
	wantErr bool
} {
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0", SHA256: pinVerdictTestPin}
	unpinnedCol := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

	return []struct {
		meta    map[string]string
		name    string
		col     collection
		cached  bool
		wantErr bool
	}{
		{
			name:   "empty pin",
			col:    unpinnedCol,
			cached: true,
			meta:   map[string]string{"sha256": pinVerdictTestRecorded},
		},
		{
			name:   "not cached",
			col:    col,
			cached: false,
			meta:   map[string]string{"sha256": pinVerdictTestRecorded},
		},
		{
			name:   "empty recorded digest",
			col:    col,
			cached: true,
			meta:   map[string]string{"sha256": ""},
		},
		{
			name:   "nil meta map",
			col:    col,
			cached: true,
			meta:   nil,
		},
		{
			name:   "malformed recorded digest",
			col:    col,
			cached: true,
			meta:   map[string]string{"sha256": "not-a-valid-hex-digest"},
		},
		{
			name:   "matching recorded digest",
			col:    col,
			cached: true,
			meta:   map[string]string{"sha256": pinVerdictTestPin},
		},
		{
			// Positive control: the identical cached/offline config, with a
			// well-formed recorded digest that genuinely disagrees with the
			// pin, must produce a verdict - proving the "no verdict" rows
			// above are real refusals of that same config, not evidence
			// dryRunPinVerdict never fires under it at all.
			name:    "disagreeing well-formed digest fires",
			col:     col,
			cached:  true,
			meta:    map[string]string{"sha256": pinVerdictTestRecorded},
			wantErr: true,
		},
	}
}

// TestInstallDryRunProbeReportsUnsafeIdentifierWhenRootExists proves
// installDryRunProbe's own newInstallTarget ok=false, root != nil arm: a
// collection whose namespace fails helpers.IsPathElement is reported a
// would-fail carrying helpers.ErrUnsafeCollectionIdentifier, rather than
// silently falling through unclassified. This arm is unreachable through the
// full production pipeline - buildCollectionsMap already rejects the
// identical identifier before any collection reaches a probe (see
// installDryRunProbe's own doc comment) - so it is exercised here by calling
// the probe directly, bypassing buildCollectionsMap, the only way to reach
// it at all.
func TestInstallDryRunProbeReportsUnsafeIdentifierWhenRootExists(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{DownloadPath: t.TempDir()}
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	col := collection{Namespace: "../escape", Name: "app", Version: "1.0.0"}

	probe := installDryRunProbe(cfg, store.New(), nil, root)
	got := probe(context.Background(), col)

	if got.settled {
		t.Errorf("expected the collection not to be reported settled, got %+v", got)
	}
	if got.fail == nil {
		t.Fatal("expected a fail verdict for an unsafe collection identifier, got nil")
	}
	if !errors.Is(got.fail, helpers.ErrUnsafeCollectionIdentifier) {
		t.Errorf("expected errors.Is helpers.ErrUnsafeCollectionIdentifier, got %v", got.fail)
	}
}

// metaFoundWithErrorArtifacts is a stub cacheManager.ArtifactStore whose Meta
// always answers found=true alongside a non-nil error - the one state
// ArtifactStore's own doc comment marks as meaningless ("a non-nil err means
// the store could not be consulted, and found carries no meaning in that
// case") but that a degraded or unreachable backend can still produce, and
// that dryRunArtifactMeta's own `err != nil || !found` guard exists to fail
// closed on. found=true paired with a non-nil error is the discriminating
// shape here, and nothing else in this package produces it: two of this
// package's other stub ArtifactStores (concurrentProbeArtifacts,
// presenceArtifacts) do return a non-nil Meta error, but always paired with
// found=false, so `!found` already short-circuits dryRunArtifactMeta's guard
// before `err != nil` is ever load-bearing - which is exactly why
// go tool cover -func reports the guard as 100% covered purely on the
// strength of its `!found` half, while a mutation dropping the `err != nil`
// check still goes undetected: a stub answering found=false here would leave
// that same gap open. Has and every method besides Meta return
// errStubNotImplemented, since neither installDryRunProbe nor
// warmDryRunProbe ever needs them.
type metaFoundWithErrorArtifacts struct{}

// errStubMetaUnreachable is the error metaFoundWithErrorArtifacts.Meta always
// returns, standing in for a cache backend that could not be consulted at
// all (a transport failure, most concretely).
var errStubMetaUnreachable = errors.New("stub: meta probe unreachable")

func (metaFoundWithErrorArtifacts) Has(context.Context, string) (bool, error) {
	return false, errStubNotImplemented
}

func (metaFoundWithErrorArtifacts) Meta(context.Context, string) (map[string]string, bool, error) {
	return map[string]string{"sha256": pinVerdictTestRecorded}, true, errStubMetaUnreachable
}

func (metaFoundWithErrorArtifacts) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (metaFoundWithErrorArtifacts) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errStubNotImplemented
}

func (metaFoundWithErrorArtifacts) Commit(context.Context, string, string, map[string]string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (metaFoundWithErrorArtifacts) Delete(context.Context, string) error {
	return errStubNotImplemented
}

// TestInstallDryRunProbeTreatsMetaErrorAsNotCached proves dryRunArtifactMeta's
// `err != nil` half of its `if err != nil || !found` guard, reached through
// installDryRunProbe: a Meta call answering found=true alongside a non-nil
// error must be classified not-cached, never a cache hit and never a
// probe-level fail of its own - a store that could not be consulted is not
// evidence the artifact is absent, and reporting it as either "cached" or
// "would fail" would both be lies a preview cannot afford.
//
// root is nil so newInstallTarget's own nil-root guard makes the collection
// report ok=false before the pin check, isolating this test to the artifact
// probe branch alone - the same isolation
// TestClassifyDryRunCallsMetaExactlyOncePerCollectionNeverHas already uses.
func TestInstallDryRunProbeTreatsMetaErrorAsNotCached(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

	probe := installDryRunProbe(cfg, store.New(), metaFoundWithErrorArtifacts{}, nil)
	got := probe(context.Background(), col)

	if got.cached {
		t.Errorf("expected cached=false when Meta answers found=true alongside a non-nil error, got %+v", got)
	}
	if got.fail != nil {
		t.Errorf("expected no fail verdict from a Meta error alone, got %v", got.fail)
	}
	if got.settled {
		t.Errorf("expected settled=false, got %+v", got)
	}
}

// TestWarmDryRunProbeTreatsMetaErrorAsNotCached is
// TestInstallDryRunProbeTreatsMetaErrorAsNotCached's counterpart for
// warmDryRunProbe: the identical Meta failure must also be reported as
// not-cached there, before ever reaching extractStore.Ready under a sha this
// probe could not have named from a genuine cache miss anyway.
func TestWarmDryRunProbeTreatsMetaErrorAsNotCached(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	col := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	extractStore := extracted.NewStore(t.TempDir())

	probe := warmDryRunProbe(cfg, metaFoundWithErrorArtifacts{}, extractStore, map[string]string{})
	got := probe(context.Background(), col)

	if got.cached {
		t.Errorf("expected cached=false when Meta answers found=true alongside a non-nil error, got %+v", got)
	}
	if got.fail != nil {
		t.Errorf("expected no fail verdict from a Meta error alone, got %v", got.fail)
	}
	if got.settled {
		t.Errorf("expected settled=false, got %+v", got)
	}
}
