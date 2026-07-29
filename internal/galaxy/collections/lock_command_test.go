package collections

// This file pins three properties of the lock command's end-to-end behavior,
// exercised through the exported Lock entry point (not lockWithState) so
// runLock's own lifecycle - including its --frozen warning - is covered:
//
//   - TestLockHonorsExplicitLockFilePath: an explicit --lock-file path (with a
//     not-yet-existing parent directory) is where the lockfile actually lands,
//     the conventional default path next to requirements.yml is left untouched,
//     and the metrics report's lockfile/lockfile_hash pair describes the file
//     actually written at the overridden path, not the default one.
//   - TestLockOverwritesAnExistingLockfile: a stale lockfile at the default path
//     is replaced wholesale by a fresh resolve, not merged with it, and the
//     resolve itself never consults the stale file's entries.
//   - TestLockFrozenIsIgnoredAndNotReported: --frozen has no effect on lock. The
//     run still regenerates the lockfile from a fresh resolve (ignoring a stale
//     pin the flag would otherwise have honored on install/warm), warns on
//     stderr rather than failing, and never claims "frozen": true in its
//     metrics report.
//
// TestLockFrozenIsIgnoredAndNotReported FAILS ON HEAD before the fix this file
// accompanies (runLock's --frozen warning and writeRunMetrics's explicit
// frozen parameter, both in start.go). Reverting those two changes and running
// just this test produces the exact output observed:
//
//	lock_command_test.go:269: expected a --frozen-has-no-effect warning on
//	stderr, got []
//	--- FAIL: TestLockFrozenIsIgnoredAndNotReported (0.04s)
//
// which is assertion (b) - printer.hasWarnContaining never fires because
// runLock never called Warnf in the first place, and that t.Fatalf stops the
// test before assertion (c) is ever reached. With the warning restored but
// writeRunMetrics still reading cfg.Frozen directly instead of the
// caller-supplied false, (c) would additionally fail with the report
// carrying "frozen": true.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// newLockRun builds a cold cache, a requirements.yml pinning nothing but
// resolving to acme.widgets@1.0.0 against a fresh fakegalaxy server, and a
// matching cfg/runtime/printer trio ready to drive the exported Lock. Every
// lock test in this file starts from this same fixture so the three tests
// differ only in the cfg field(s) each one is pinning behavior for.
func newLockRun(t *testing.T) (*config.Config, *infra.Infra, *capturingPrinter, string) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		MetricsFile:      filepath.Join(root, "metrics.json"),
		Workers:          1,
	}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())
	return cfg, runtime, printer, root
}

// assertSingleWidgetsEntry fails the test unless lf contains exactly one
// entry, named acme.widgets, pinned at wantVersion. Shared by all three
// tests in this file: each one asserts that the lockfile Lock actually wrote
// pins nothing but a fresh resolve's own result, whatever staleness or
// override preceded the run.
func assertSingleWidgetsEntry(t *testing.T, lf *lockfile.File, wantVersion string) {
	t.Helper()
	if len(lf.Collections) != 1 {
		t.Fatalf("unexpected lockfile collections: %+v, want exactly 1 entry", lf.Collections)
	}
	entry := lf.Collections[0]
	if entry.Name != "acme.widgets" || entry.Version != wantVersion {
		t.Fatalf("unexpected lockfile entry: %+v, want acme.widgets@%s", entry, wantVersion)
	}
}

// readMetricsReport reads the metrics file at path and decodes it into a
// map, not metrics.Report: decoding into the struct would resolve field
// names through the very same json tags the marshal side used, so a renamed
// or swapped tag would round-trip invisibly. Reading the literal wire keys
// pins the on-disk contract a consuming CI dashboard actually parses,
// independent of the Go struct's field names (see
// TestArtifactMetricsWrittenToMetricsFile in e2e_test.go for the same
// reasoning).
func readMetricsReport(t *testing.T, path string) map[string]any {
	t.Helper()
	//nolint:gosec // path is this test's own fixed cfg.MetricsFile, not user input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metrics file %s: %v", path, err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", path, err)
	}
	return written
}

// TestLockHonorsExplicitLockFilePath asserts that an explicit --lock-file
// path - including one whose parent directory does not exist yet - is where
// the lockfile actually lands, that the conventional default path next to
// requirements.yml is left untouched, and that the metrics report describes
// the file actually written rather than the default location.
func TestLockHonorsExplicitLockFilePath(t *testing.T) {
	t.Parallel()
	cfg, runtime, _, root := newLockRun(t)
	// A not-yet-existing parent directory ("ci/") on purpose:
	// helpers.WriteFileAtomic MkdirAll's it, so this also proves Lock never
	// requires the caller to pre-create the lockfile's directory.
	cfg.LockFile = filepath.Join(root, "ci", "pinned.lock.yml")

	if err := Lock(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	lf, err := lockfile.Load(cfg.LockFile)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", cfg.LockFile, err)
	}
	assertSingleWidgetsEntry(t, lf, testVersion100)

	defaultPath := filepath.Join(filepath.Dir(cfg.RequirementsFile), lockfile.DefaultName)
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Errorf("expected default lockfile path %s to not exist, stat err = %v", defaultPath, err)
	}

	assertLockMetricsDescribeFile(t, cfg, lf)
}

// assertLockMetricsDescribeFile asserts that cfg.MetricsFile's report names
// cfg.LockFile as the lockfile path and carries the SHA256 hash of the
// lockfile actually written there - not the default path's hash - proving
// the report describes the file the run produced, not a hardcoded default.
func assertLockMetricsDescribeFile(t *testing.T, cfg *config.Config, lf *lockfile.File) {
	t.Helper()
	written := readMetricsReport(t, cfg.MetricsFile)
	if got, _ := written["lockfile"].(string); got != cfg.LockFile {
		t.Errorf("metrics lockfile = %q, want %q", got, cfg.LockFile)
	}
	wantHash, err := lf.Hash()
	if err != nil {
		t.Fatalf("lf.Hash: %v", err)
	}
	gotHash, _ := written["lockfile_hash"].(string)
	if gotHash == "" || gotHash != wantHash {
		t.Errorf("metrics lockfile_hash = %q, want %q (the hash of the file actually written at the overridden path)", gotHash, wantHash)
	}
}

// TestLockOverwritesAnExistingLockfile asserts that a stale lockfile at the
// default path is replaced wholesale by a fresh resolve, not merged with it,
// and that the resolve itself never consulted the stale entries: the run
// pins only what requirements.yml + the live server resolve to, and the
// stale file's own entries never leak into the result.
func TestLockOverwritesAnExistingLockfile(t *testing.T) {
	t.Parallel()
	cfg, runtime, _, _ := newLockRun(t)

	defaultPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.widgets", Version: "0.9.0", Source: cfg.Server, SHA256: "0000000000000000000000000000000000000000000000000000000000bad"},
			{Name: "acme.legacy", Version: testVersion100, Source: cfg.Server},
		},
	}
	if err := lockfile.Save(defaultPath, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}
	staleBytes, err := os.ReadFile(defaultPath) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read stale lockfile: %v", err)
	}

	if err := Lock(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	lf, err := lockfile.Load(defaultPath)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", defaultPath, err)
	}
	assertSingleWidgetsEntry(t, lf, testVersion100)

	freshBytes, err := os.ReadFile(defaultPath) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read fresh lockfile: %v", err)
	}
	assertReplacedNotMerged(t, string(freshBytes), string(staleBytes))
}

// assertReplacedNotMerged fails the test unless fresh differs from stale and
// carries neither the stale acme.legacy entry nor its 0.9.0 pin, proving a
// stale lockfile is replaced wholesale by a fresh resolve rather than merged
// with it.
func assertReplacedNotMerged(t *testing.T, fresh, stale string) {
	t.Helper()
	if fresh == stale {
		t.Fatalf("expected the fresh lockfile bytes to differ from the stale ones, both were:\n%s", fresh)
	}
	if strings.Contains(fresh, "acme.legacy") {
		t.Errorf("fresh lockfile still contains the stale acme.legacy entry: %s", fresh)
	}
	if strings.Contains(fresh, "0.9.0") {
		t.Errorf("fresh lockfile still contains the stale 0.9.0 pin: %s", fresh)
	}
}

// TestLockFrozenIsIgnoredAndNotReported asserts that --frozen has no effect
// on lock: the run still regenerates the lockfile from a fresh resolve
// (ignoring a stale pin the flag would otherwise have honored on
// install/warm), warns rather than fails, and never claims "frozen": true in
// its metrics report. See this file's header comment for the exact pre-fix
// failure.
func TestLockFrozenIsIgnoredAndNotReported(t *testing.T) {
	t.Parallel()
	cfg, runtime, printer, _ := newLockRun(t)
	cfg.Frozen = true

	defaultPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.widgets", Version: "0.9.0", Source: cfg.Server, SHA256: "0000000000000000000000000000000000000000000000000000000000bad"},
		},
	}
	if err := lockfile.Save(defaultPath, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}

	if err := Lock(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// (a) the run regenerated the lockfile from a fresh resolve rather than
	// consuming the stale pin, proving --frozen was not honored.
	lf, err := lockfile.Load(defaultPath)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", defaultPath, err)
	}
	assertSingleWidgetsEntry(t, lf, testVersion100)

	// (b) the operator was warned instead of the run silently ignoring the flag.
	if !printer.hasWarnContaining("--frozen has no effect on lock") {
		t.Fatalf("expected a --frozen-has-no-effect warning on stderr, got %v", printer.warns)
	}

	// (c) the report makes no frozen claim.
	assertReportNotFrozen(t, readMetricsReport(t, cfg.MetricsFile))
}

// assertReportNotFrozen fails the test if written's "frozen" key is present
// and true. With `omitempty`, the key is absent on a false value, so either
// absence or an explicit false is acceptable - only an explicit true is the
// regression this guards against.
func assertReportNotFrozen(t *testing.T, written map[string]any) {
	t.Helper()
	v, ok := written["frozen"]
	if !ok {
		return
	}
	if b, isBool := v.(bool); !isBool || b {
		t.Errorf("metrics frozen = %v, want absent or false (lock never consumes the lockfile it writes)", v)
	}
}

// TestLockDryRunRejectsBeforeAnythingOpens asserts lock --dry-run is refused
// as a usage error before the backend is even opened, rather than silently
// overwriting the lockfile while announcing a normal lock run - lock has no
// dry-run implementation, and an ambient GO_GALAXY_DRY_RUN must not make it
// do the opposite of what was asked. Deliberately builds its own fixture
// rather than reusing newLockRun, which pre-creates cacheDir: this test's own
// point is that cacheDir must never come into existence at all.
func TestLockDryRunRejectsBeforeAnythingOpens(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
		DryRun:           true,
	}
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := Lock(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrDryRunUnsupported) {
		t.Fatalf("expected errors.Is ErrDryRunUnsupported, got %v", err)
	}

	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Errorf("expected no lockfile to be written, stat error = %v", statErr)
	}
	if got := srv.Total(); got != 0 {
		t.Errorf("Total() = %d, want 0 (--dry-run must reject before any network call)", got)
	}
	// The cache directory is never created: proof the backend was never
	// opened at all, not just that no lock survived to be released.
	if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
		t.Errorf("expected cacheDir to never be created, stat error = %v", statErr)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", got, exitcode.ExitUsage)
	}
}
