package collections

// This file owns the save-failure contract shared by all three commands'
// *WithState work functions: installWithState, warmWithState, and
// lockWithState. Each command's success path saves the snapshot and writes a
// run-metrics report as its very last steps; this file proves that a failing
// SaveStore does not skip the metrics report and, for install/warm, does not
// silently swap out the run's collection-failure classification for the save
// error's own (unclassified) one. TestWarmPartialFailureKeepsSuccessfulCollectionCached
// (warm_e2e_test.go) already proves SaveStore itself runs despite a nonzero
// collection-failure count, and TestWarmMetricsWrittenForSuccessAndFailure's
// "failing warm" subtest (same file) already proves metrics are written on a
// failing warm; the tests below close the remaining gap - what happens when
// SaveStore itself fails.
//
// Each test below was verified against a real revert of the production
// change it pins, and this comment quotes the actual output observed:
//
//   - TestWarmWithStateSaveFailureWritesMetricsNoCollections: reverting
//     warmWithState to call SaveStore, return early on its error, and only
//     then call writeRunMetrics makes it fail with:
//     "read metrics file /.../metrics.json: open /.../metrics.json: no such
//     file or directory"
//     since readMetricsCommand's t.Fatalf on the missing file fires before
//     the test's own assertion is ever reached.
//   - TestWarmWithStateSaveFailureKeepsWarmFailureClass: reverting
//     warmWithState to classify by the save failure first instead of by the
//     collection-failure count - SaveStore, then writeRunMetrics, then
//     `if saveErr != nil { return saveErr }` ahead of the failures check,
//     with no annotateSaveFailure - makes it fail with:
//     "expected errors.Is helpers.ErrInstallationFailed, got simulated
//     SaveStore failure"
//   - TestInstallWithStateSaveFailureWritesMetrics: a regression pin on an
//     already-correct order, not a fix. Making installWithState skip
//     writeRunMetrics when finalizeInstall returned a non-nil error (guarding
//     the call with `if finalErr == nil`) makes it fail with:
//     "read metrics file /.../metrics.json: open /.../metrics.json: no such
//     file or directory"
//   - TestInstallWithStateSaveFailureKeepsInstallFailureClass: reverting
//     finalizeInstall to classify by the save failure first instead of by
//     the collection-failure count (`if err := backend.SaveStore(...); err
//     != nil { return err }`, checked before the failures tally) makes it
//     fail with:
//     "expected errors.Is helpers.ErrInstallationFailed, got simulated
//     SaveStore failure"
//     confirming that a failing save silently discarded the
//     ErrInstallationFailed classification instead of folding the save error
//     in alongside it.
//   - TestLockWithStateSaveFailureWritesMetricsAndKeepsLockfile: reverting
//     lockWithState's tail to return early on a SaveStore error - before
//     writeRunMetrics runs and before the "Lockfile written" line is printed
//     - makes assertion (c) fail with:
//     "expected a persistent \"Lockfile written\" line, got []"
//     and assertion (d), which is fatal and therefore the one that actually
//     stops the test, fail with:
//     "read metrics file /.../metrics.json: open /.../metrics.json: no such
//     file or directory"
//   - TestLockWithStateWritesLockfileAndMetrics exercises lockWithState's
//     happy path with a real (not save-failing) backend, confirming the split
//     between runLock and lockWithState does not leave any of lockWithState's
//     own logic untested.
//   - TestInstallWithStateSaveFailureKeepsInstallFailureClass and
//     TestWarmWithStateSaveFailureKeepsWarmFailureClass's added
//     helpers.ErrDownloadFailed assertion: reverting failureSummary.wrap to
//     always return headline unchanged (dropping the per-collection cause)
//     makes both fail with:
//     "expected errors.Is helpers.ErrDownloadFailed, got installation failed:
//     warm failed for 1 collections; snapshot save failed: simulated
//     SaveStore failure"
//     and
//     "expected errors.Is helpers.ErrDownloadFailed, got installation failed
//     for 1 collections; snapshot save failed: simulated SaveStore failure"
//     respectively.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// errSaveFailSentinel stands in for a real backend's SaveStore failure (disk
// full, an S3 partition) that has nothing to do with whether any collection
// itself succeeded. It stays private to this file - production code never
// compares against it.
var errSaveFailSentinel = errors.New("simulated SaveStore failure")

// errPrimarySentinel stands in for a run's primary error - whatever
// annotateSaveFailure is asked to preserve. It carries no meaning beyond
// being a stable, comparable value.
var errPrimarySentinel = errors.New("simulated primary failure")

// metricsCommandLock is the metrics report's "command" field value a lock
// run writes, shared across every test in this package that asserts on it
// (readMetricsCommand's own callers here and in lock_command_test.go) so the
// literal exists in exactly one place.
const metricsCommandLock = "lock"

// saveFailBackend wraps a real cacheManager.Backend - a real *local.Backend
// in every test below - and overrides only SaveStore to always fail, so
// Open/Lock/LoadStore/Artifacts/SweepTemp/RecordProject all behave exactly
// like the real backend they wrap. Same wrap-a-real-implementation shape as
// s3_cache_recovery_test.go's fetchOnceMismatchArtifacts.
type saveFailBackend struct {
	cacheManager.Backend
}

// SaveStore always fails, simulating a persistence failure at the very last
// step of a run.
func (saveFailBackend) SaveStore(context.Context, *store.Store) error {
	return errSaveFailSentinel
}

// readMetricsCommand reads the metrics file at path and returns its "command"
// field, failing the test on any I/O or decode error. A thin wrapper over
// readMetricsReport (lock_command_test.go) so the package has one decoder for
// the metrics file's wire keys.
func readMetricsCommand(t *testing.T, path string) string {
	t.Helper()
	command, _ := readMetricsReport(t, path)["command"].(string)
	return command
}

// TestAnnotateSaveFailurePassesPrimaryUnchangedWhenSaveSucceeds pins the half
// of annotateSaveFailure's contract that every other test here can only reach
// by inference. The nine indirect callers on the save-succeeded path all
// assert errors.Is(err, helpers.ErrInstallationFailed), which would stay true
// even if the function wrapped unconditionally, so only an identity check
// proves the primary error is returned untouched rather than rewrapped.
func TestAnnotateSaveFailurePassesPrimaryUnchangedWhenSaveSucceeds(t *testing.T) {
	t.Parallel()

	//nolint:err113,errorlint // identity is the assertion: errors.Is would also
	// hold for an unconditional wrap, which is exactly the regression this test
	// exists to catch, so comparing the values is the only check that bites.
	if got := annotateSaveFailure(errPrimarySentinel, nil); got != errPrimarySentinel {
		t.Fatalf("expected the primary error returned unchanged, got %v", got)
	}
}

// TestWarmWithStateSaveFailureWritesMetricsNoCollections asserts that, with
// nothing to warm, a failing SaveStore still returns the save error and still
// leaves cfg.MetricsFile on disk with command == "warm": writeRunMetrics runs
// unconditionally, even on a save failure, and is not gated behind whether the
// save succeeded.
func TestWarmWithStateSaveFailureWritesMetricsNoCollections(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
		Offline:          true,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	err := warmWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	if got := readMetricsCommand(t, metricsPath); got != "warm" {
		t.Errorf("metrics file not written despite the save failure: command = %q, want %q", got, "warm")
	}
}

// TestWarmWithStateSaveFailureKeepsWarmFailureClass asserts that when both a
// collection's artifact download fails and SaveStore itself fails,
// warmWithState's returned error still matches helpers.ErrInstallationFailed
// (the sentinel cmd/go-galaxy/exitcode maps to the install exit class) as
// well as the save error itself - a nonzero collection-failure count stays
// the primary, classifiable error even when the save also failed, with the
// save error folded in as context rather than replacing it. Classifying by
// the save error alone would instead let it win outright and lose the
// ErrInstallationFailed class.
func TestWarmWithStateSaveFailureKeepsWarmFailureClass(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "widgets", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := warmWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}
	// The 503 artifact endpoint is the per-collection cause warmCollections
	// records: proving errors.Is also reaches helpers.ErrDownloadFailed here
	// confirms the headline, the real cause, and the save error all coexist
	// in the same error tree through the real pipeline, not just in the
	// unit-level failureSummary tests.
	if !errors.Is(err, helpers.ErrDownloadFailed) {
		t.Fatalf("expected errors.Is helpers.ErrDownloadFailed, got %v", err)
	}
}

// TestInstallWithStateSaveFailureWritesMetrics is a regression pin on an
// already-correct order, not a fix: installWithState already writes the run
// metrics report after finalizeInstall's save attempt, regardless of whether
// that save succeeded. Moving writeRunMetrics behind a save-error return
// would make readMetricsCommand's t.Fatalf on the missing file fire before
// this test's own assertions ever run - see this file's header comment for
// the exact observed message.
func TestInstallWithStateSaveFailureWritesMetrics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
		Offline:          true,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	err := installWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	if got := readMetricsCommand(t, metricsPath); got != "install" {
		t.Errorf("metrics file not written despite the save failure: command = %q, want %q", got, "install")
	}
}

// TestInstallWithStateSaveFailureKeepsInstallFailureClass pins
// finalizeInstall's failure-classification precedence: a failing SaveStore
// must not return immediately with the bare save error ahead of the
// failures check, since a run that also had a failed collection would then
// lose its ErrInstallationFailed classification entirely and degrade to the
// generic/unclassified exit code. See this file's header comment for the
// exact observed message from the killing mutation.
func TestInstallWithStateSaveFailureKeepsInstallFailureClass(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "widgets", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := installWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}
	// The 503 artifact endpoint is the per-collection cause installLevels
	// records: proving errors.Is also reaches helpers.ErrDownloadFailed here
	// confirms the headline, the real cause, and the save error all coexist
	// in the same error tree through the real pipeline, not just in the
	// unit-level failureSummary tests.
	if !errors.Is(err, helpers.ErrDownloadFailed) {
		t.Fatalf("expected errors.Is helpers.ErrDownloadFailed, got %v", err)
	}
}

// TestLockWithStateSaveFailureWritesMetricsAndKeepsLockfile asserts
// lockWithState's tail: the lockfile itself lands on disk and the "Lockfile
// written" line is announced before the save is even attempted, and the run
// metrics report is written after the save regardless of whether it
// succeeded. Assertions run in this specific order because readMetricsCommand
// (used by assertion (d)) t.Fatalf's on a missing file, which would otherwise
// mask a real (b) or (c) failure - see this file's header comment for the
// exact message observed on HEAD, where (d) is the one that fails.
func TestLockWithStateSaveFailureWritesMetricsAndKeepsLockfile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
	}
	state := newSaveFailState(t, cfg)
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	err := lockWithState(context.Background(), cfg, runtime, state, time.Now())

	// (a) the returned error is the save failure.
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	// (b) the lockfile itself was written and is valid, independent of the
	// snapshot save's outcome.
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, loadErr := lockfile.Load(path)
	if loadErr != nil {
		t.Errorf("expected lockfile.Load to succeed despite the save failure, got %v", loadErr)
	} else if len(lf.Collections) != 1 {
		t.Errorf("expected 1 lockfile entry, got %d", len(lf.Collections))
	}

	// (c) the operator was told the file landed, even though the run still
	// exits nonzero.
	if !printer.hasPersistentPrintContaining("Lockfile written") {
		t.Errorf("expected a persistent \"Lockfile written\" line, got %v", printer.persists)
	}

	// (d) last: readMetricsCommand t.Fatalf's on a missing file, which would
	// otherwise mask a real failure in (b) or (c) above.
	if got := readMetricsCommand(t, metricsPath); got != metricsCommandLock {
		t.Errorf("metrics file not written despite the save failure: command = %q, want %q", got, metricsCommandLock)
	}
}

// TestLockFrozenSaveFailureJoinsBehindDrift asserts lockFrozen's own tail
// draws the identical save-failure contract lockWithState's plain write path
// already has, over a different pair of outcomes: a drifted lockfile (the
// fresh resolve disagreeing with a stale file already on disk) and a failing
// SaveStore both hold at once, through the real lockFrozen -> saveLockSnapshot
// -> annotateSaveFailure(driftErr, saveErr) path - not the hand-constructed
// error tree exitcode_test.go's TestLockDriftOutranksSaveFailure builds to
// pin the classification alone. The lockfile itself is never touched: unlike
// lockWithState's plain path (which writes the file before the save even
// runs), lockFrozen's whole point is to compare against what is already
// there, so the stale file on disk must survive byte-for-byte regardless of
// whether the save also failed.
func TestLockFrozenSaveFailureJoinsBehindDrift(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
		Frozen:           true,
	}

	// A stale lockfile pinning a version the live server's fresh resolve
	// (1.0.0, the only version registered) will disagree with - the drift
	// lockFrozen must report before saveLockSnapshot even runs.
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        cfg.Server,
		Collections:   []lockfile.Entry{{Name: "acme.widgets", Version: "0.9.0", Source: cfg.Server}},
	}
	if err := lockfile.Save(path, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}
	before := mustReadFile(t, path)

	state := newSaveFailState(t, cfg)
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	err := lockWithState(context.Background(), cfg, runtime, state, time.Now())

	// (a) both the drift verdict and the save failure are reachable through
	// the same returned error.
	if !errors.Is(err, helpers.ErrLockfileDrift) {
		t.Fatalf("expected errors.Is helpers.ErrLockfileDrift, got %v", err)
	}
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	// (b) the lockfile on disk is untouched: lockFrozen never writes it,
	// drift or not, save failure or not.
	after := mustReadFile(t, path)
	if string(before) != string(after) {
		t.Fatalf("lockFrozen rewrote the lockfile despite drift:\n%s", after)
	}

	// (c) writeRunMetrics still ran despite both failures.
	if got := readMetricsCommand(t, metricsPath); got != metricsCommandLock {
		t.Errorf("metrics file not written despite drift and the save failure: command = %q, want %q", got, metricsCommandLock)
	}
}

// TestLockWithStateWritesLockfileAndMetrics exercises lockWithState's happy
// path with a real (not save-failing) backend, confirming a successful lock
// run writes both the lockfile and the metrics report.
func TestLockWithStateWritesLockfileAndMetrics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	err := lockWithState(context.Background(), cfg, runtime, state, time.Now())
	if err != nil {
		t.Fatalf("expected lockWithState to succeed, got %v", err)
	}

	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, loadErr := lockfile.Load(path)
	if loadErr != nil {
		t.Fatalf("lockfile.Load: %v", loadErr)
	}
	if len(lf.Collections) != 1 || lf.Collections[0].Name != "acme.widgets" {
		t.Fatalf("unexpected lockfile collections: %+v", lf.Collections)
	}

	if got := readMetricsCommand(t, metricsPath); got != metricsCommandLock {
		t.Errorf("metrics command = %q, want %q", got, metricsCommandLock)
	}
	if !printer.hasPersistentPrintContaining("Lockfile written") {
		t.Errorf("expected a persistent \"Lockfile written\" line, got %v", printer.persists)
	}
}

// newLocalState builds an installState wired to a real *local.Backend rooted
// at cfg.CacheDir, opened and loaded exactly like initInstall would, but
// without ever taking the backend's lock: none of the *WithState functions
// this file exercises touch state.release or call backend.Close themselves,
// so this helper does not need to provide either.
func newLocalState(t *testing.T, cfg *config.Config) *installState {
	t.Helper()
	backend := local.New(cfg.CacheDir)
	ctx := context.Background()
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close(ctx) })
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return &installState{
		backend:      backend,
		store:        st,
		extractStore: newExtractStore(cfg),
	}
}

// newSaveFailState is newLocalState with its backend wrapped in
// saveFailBackend, so SaveStore always fails while everything else - Open,
// LoadStore, Artifacts, SweepTemp, RecordProject - behaves like the real
// backend.
func newSaveFailState(t *testing.T, cfg *config.Config) *installState {
	t.Helper()
	state := newLocalState(t, cfg)
	state.backend = saveFailBackend{Backend: state.backend}
	return state
}
