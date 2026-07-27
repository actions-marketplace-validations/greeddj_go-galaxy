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
//     then call writeRunMetrics (undoing change 3's reordering) makes it fail
//     with:
//     "read metrics file /.../metrics.json: open /.../metrics.json: no such
//     file or directory"
//     since readMetricsCommand's t.Fatalf on the missing file fires before
//     the test's own assertion is ever reached.
//   - TestWarmWithStateSaveFailureKeepsWarmFailureClass: reverting
//     warmWithState's tail to the old precedence - SaveStore, then
//     writeRunMetrics, then `if saveErr != nil { return saveErr }` ahead of
//     the failures check, with no annotateSaveFailure - makes it fail with:
//     "expected errors.Is helpers.ErrInstallationFailed, got simulated
//     SaveStore failure"
//   - TestInstallWithStateSaveFailureWritesMetrics: a regression pin on an
//     already-correct order, not a fix. Making installWithState skip
//     writeRunMetrics when finalizeInstall returned a non-nil error (guarding
//     the call with `if finalErr == nil`) makes it fail with:
//     "read metrics file /.../metrics.json: open /.../metrics.json: no such
//     file or directory"
//   - TestInstallWithStateSaveFailureKeepsInstallFailureClass: FAILS ON HEAD
//     before this change - reverting finalizeInstall to its old precedence
//     (`if err := backend.SaveStore(...); err != nil { return err }`, checked
//     before the failures count) makes it fail with:
//     "expected errors.Is helpers.ErrInstallationFailed, got simulated
//     SaveStore failure"
//     confirming the architect's probe: a failing save silently discarded the
//     ErrInstallationFailed classification instead of folding the save error
//     in alongside it.
//   - TestLockWithStateSaveFailureWritesMetricsAndKeepsLockfile: FAILS ON HEAD
//     before this change - reverting lockWithState's tail to the old order
//     (SaveStore, return early on its error, then writeRunMetrics, then the
//     "Lockfile written" announcement) makes assertion (c) fail with:
//     "expected a persistent \"Lockfile written\" line, got []"
//     and assertion (d), which is fatal and therefore the one that actually
//     stops the test, fail with:
//     "read metrics file /.../metrics.json: open /.../metrics.json: no such
//     file or directory"
//   - TestLockWithStateWritesLockfileAndMetrics: this is the refactor's own
//     regression net for lockWithState - there was zero prior test coverage of
//     any lock run, so this passes on both HEAD and the fix and exists purely
//     so the split does not silently move untested code.

import (
	"context"
	"encoding/json"
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
// field, failing the test on any I/O or decode error.
func readMetricsCommand(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixed cfg.MetricsFile, not user input.
	if err != nil {
		t.Fatalf("read metrics file %s: %v", path, err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", path, err)
	}
	command, _ := written["command"].(string)
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
// save error folded in as context rather than replacing it. This inverts the
// pre-fix test, which asserted the save error won outright and the
// ErrInstallationFailed class was lost.
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

// TestInstallWithStateSaveFailureKeepsInstallFailureClass pins the fix to
// finalizeInstall's precedence bug (change 2): before the fix, a failing
// SaveStore returned immediately with the bare save error, before failures
// was ever checked, so a run that also had a failed collection lost its
// ErrInstallationFailed classification entirely and degraded to the
// generic/unclassified exit code. FAILS ON HEAD - see this file's header
// comment for the exact observed message.
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
	if got := readMetricsCommand(t, metricsPath); got != "lock" {
		t.Errorf("metrics file not written despite the save failure: command = %q, want %q", got, "lock")
	}
}

// TestLockWithStateWritesLockfileAndMetrics is the refactor's own regression
// net: before this change, there was zero test coverage of any lock run at
// all, so splitting runLock into runLock/lockWithState would otherwise move
// entirely untested code. It exercises the happy path with a real (not
// save-failing) backend.
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

	if got := readMetricsCommand(t, metricsPath); got != "lock" {
		t.Errorf("metrics command = %q, want %q", got, "lock")
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
