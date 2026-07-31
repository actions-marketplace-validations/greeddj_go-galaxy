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
// It also covers lock's --dry-run preview (TestLockDryRun*): a diff of a
// fresh resolve against whatever lockfile is already on disk, reported
// through Okf/PersistentPrintf and never written to disk, with the snapshot
// save and the metrics-skip guard both behaving exactly as they do for
// install and warm.
//
// Reverting both runLock's --frozen warning (dropping the `if cfg.Frozen`
// Warnf block) and writeRunMetrics's explicit frozen parameter (both in
// start.go) and running just TestLockFrozenIsIgnoredAndNotReported produces
// the exact output observed:
//
//	lock_command_test.go:290: expected a --frozen-has-no-effect warning on
//	stderr, got []
//	--- FAIL: TestLockFrozenIsIgnoredAndNotReported (0.05s)
//
// which is assertion (b) - f.printer.hasWarnContaining never fires because
// runLock never called Warnf in the first place, and that t.Fatalf stops the
// test before assertion (c) is ever reached. With the warning restored but
// writeRunMetrics still reading cfg.Frozen directly instead of the
// caller-supplied false, (c) would additionally fail with the report
// carrying "frozen": true.

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// lockRun bundles the cfg/runtime/printer trio every lock test in this file
// drives Lock with, plus the fakegalaxy server and temp root behind them.
// server is exposed, not just wired into runtime's http.Client, because
// TestLockDryRunSavesMetadataCachesWhenSnapshotExists needs to register an
// additional version on it between two Lock calls.
type lockRun struct {
	cfg     *config.Config
	runtime *infra.Infra
	printer *capturingPrinter
	server  *fakegalaxy.Server
	root    string
}

// newLockRun builds a cold cache, a requirements.yml pinning nothing but
// resolving to acme.widgets@1.0.0 against a fresh fakegalaxy server, and a
// matching lockRun fixture ready to drive the exported Lock. Every lock test
// in this file starts from this same fixture so tests differ only in the
// field(s) each one sets or asserts on afterward.
func newLockRun(t *testing.T) lockRun {
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
	return lockRun{cfg: cfg, runtime: runtime, printer: printer, server: srv, root: root}
}

// assertSingleWidgetsEntry fails the test unless lf contains exactly one
// entry, named acme.widgets, pinned at wantVersion. Shared by every test in
// this file that performs a real Lock: each one asserts that the lockfile
// Lock actually wrote pins nothing but a fresh resolve's own result, whatever
// staleness or override preceded the run.
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
	f := newLockRun(t)
	// A not-yet-existing parent directory ("ci/") on purpose:
	// helpers.WriteFileAtomic MkdirAll's it, so this also proves Lock never
	// requires the caller to pre-create the lockfile's directory.
	f.cfg.LockFile = filepath.Join(f.root, "ci", "pinned.lock.yml")

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	lf, err := lockfile.Load(f.cfg.LockFile)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", f.cfg.LockFile, err)
	}
	assertSingleWidgetsEntry(t, lf, testVersion100)

	defaultPath := filepath.Join(filepath.Dir(f.cfg.RequirementsFile), lockfile.DefaultName)
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Errorf("expected default lockfile path %s to not exist, stat err = %v", defaultPath, err)
	}

	assertLockMetricsDescribeFile(t, f.cfg, lf)
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
	f := newLockRun(t)

	defaultPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.widgets", Version: "0.9.0", Source: f.cfg.Server, SHA256: "0000000000000000000000000000000000000000000000000000000000bad"},
			{Name: "acme.legacy", Version: testVersion100, Source: f.cfg.Server},
		},
	}
	if err := lockfile.Save(defaultPath, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}
	staleBytes, err := os.ReadFile(defaultPath) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read stale lockfile: %v", err)
	}

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
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
// its metrics report. See this file's header comment for the exact killing-
// mutation output.
func TestLockFrozenIsIgnoredAndNotReported(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.Frozen = true

	defaultPath := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.widgets", Version: "0.9.0", Source: f.cfg.Server, SHA256: "0000000000000000000000000000000000000000000000000000000000bad"},
		},
	}
	if err := lockfile.Save(defaultPath, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
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
	if !f.printer.hasWarnContaining("--frozen has no effect on lock") {
		t.Fatalf("expected a --frozen-has-no-effect warning on stderr, got %v", f.printer.warns)
	}

	// (c) the report makes no frozen claim.
	assertReportNotFrozen(t, readMetricsReport(t, f.cfg.MetricsFile))
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

// TestLockDryRunWritesNoLockfileAndReportsAdds proves the cold-cache shape of
// lock's --dry-run preview: no lockfile exists yet, so the diff against a nil
// baseline reports every resolved collection as added, nothing is written to
// disk, and no metrics report is produced.
//
// Two mutations were run against this test, both confirmed to fail it:
//
//   - Dropping the `if cfg.DryRun` branch from lockWithState (so the real
//     lockfile.Save path runs unconditionally) fails the lockfile-absence
//     check with:
//     lock_command_test.go:338: expected no lockfile written, stat err = <nil>
//   - Removing writeRunMetrics's own internal cfg.DryRun guard (leaving
//     lockDryRun's unconditional call to it) fails the metrics-absence check
//     with:
//     lock_command_test.go:351: expected no metrics report, stat err = <nil>
func TestLockDryRunWritesNoLockfileAndReportsAdds(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no lockfile written, stat err = %v", err)
	}
	if !f.printer.hasOkContaining("Would add: acme.widgets@1.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
	wantSummary := "Dry run: lockfile would change; 1 would be added, 0 would be updated, 0 would be removed, 0 unchanged (" + path + ")"
	if !f.printer.hasPersistentPrintContaining(wantSummary) {
		t.Fatalf("persists = %v", f.printer.persists)
	}
	if f.printer.hasPersistentPrintContaining("Lockfile written") {
		t.Fatalf("dry run announced a write: %v", f.printer.persists)
	}
	if _, err := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(err) {
		t.Fatalf("expected no metrics report, stat err = %v", err)
	}
}

// TestLockDryRunReportsUpdateAndRemoval proves the preview reports both an
// updated and a removed collection - the two Diff cases
// TestLockDryRunWritesNoLockfileAndReportsAdds never exercises - against a
// stale lockfile pinning acme.widgets at an old version with no sha256 (a
// version -> version and a (none) -> sha256 change in the same line) plus a
// phantom acme.legacy entry the fresh resolve no longer has a root for. It
// also doubles as TestLockDryRunWritesNoLockfileAndReportsAdds's positive
// control on the printer: the same fixture that reports zero updates there
// reports exactly one here.
//
// Mutation (dropping the `if cfg.DryRun` branch from lockWithState) fails
// with:
//
//	lock_command_test.go:413: dry run rewrote the lockfile:
//	server: http://127.0.0.1:PORT
//	collections:
//	    - name: acme.widgets
//	      version: 1.0.0
//	      source: http://127.0.0.1:PORT
//	      sha256: c101ba4cb889e4daaf158fdf58276ac2aa15d109e2508d602207d63c3317ae15
//	schema_version: 1
func TestLockDryRunReportsUpdateAndRemoval(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	// Re-registers the same acme.widgets@1.0.0 newLockRun already added, purely
	// to capture its deterministic sha256: AddVersion computes the sha from
	// namespace/name/version/deps alone, so a repeat call overwrites the
	// existing map entry idempotently rather than registering a second one,
	// and deriving the expected value this way beats hardcoding it.
	wantVersion := f.server.AddVersion("acme", "widgets", testVersion100, nil)

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: "acme.widgets", Version: "0.9.0", Source: f.cfg.Server},
			{Name: "acme.legacy", Version: "2.0.0", Source: f.cfg.Server},
		},
	}
	if err := lockfile.Save(path, stale); err != nil {
		t.Fatalf("save stale lockfile: %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read stale lockfile: %v", err)
	}

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	after, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read lockfile after dry run: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry run rewrote the lockfile:\n%s", after)
	}
	wantUpdate := "Would update: acme.widgets (version 0.9.0 -> 1.0.0; sha256 (none) -> " + wantVersion.SHA256 + ")"
	if !f.printer.hasOkContaining(wantUpdate) {
		t.Fatalf("expected %q, got oks = %v", wantUpdate, f.printer.oks)
	}
	if !f.printer.hasOkContaining("Would remove: acme.legacy@2.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
	if f.printer.hasOkContaining("Would add:") {
		t.Fatalf("expected no Added line for an already-pinned collection, got %v", f.printer.oks)
	}
}

// TestLockDryRunNoChangeReportsAllUnchanged proves an up-to-date lockfile
// reports zero adds/updates/removals and exactly one unchanged collection,
// with no per-entry line at all - the fixture is first driven through a real
// Lock (this test's own positive control, proving the fixture can produce a
// real lockfile, not just refuse to touch one) so the dry run that follows
// diffs against genuinely current content rather than an empty or synthetic
// baseline.
//
// This test is also TestLockDryRunReportsAServerOnlyChange's positive
// control: both drive the identical fixture to the identical add/update/
// remove/unchanged counts and differ only in the file-level Server field -
// this one leaves it alone and reports "lockfile is up to date", the other
// changes only it and reports "lockfile would change". That pairing is what
// proves the verdict term discriminates on the server field alone rather
// than on the counts, which are identical in both tests.
//
// Mutation (dropping the `if cfg.DryRun` branch from lockWithState) fails
// with:
//
//	lock_command_test.go:475: persists = [✅ Lockfile written to /.../requirements.lock.yml (1 collections)]
func TestLockDryRunNoChangeReportsAllUnchanged(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	before, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read seeded lockfile: %v", err)
	}

	dryPrinter := &capturingPrinter{}
	dryRuntime := infra.New(dryPrinter, f.server.Client())
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, dryRuntime); err != nil {
		t.Fatalf("dry Lock: %v", err)
	}

	after, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read lockfile after dry run: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry run rewrote the lockfile:\n%s", after)
	}
	wantSummary := "Dry run: lockfile is up to date; 0 would be added, 0 would be updated, 0 would be removed, 1 unchanged (" + path + ")"
	if !dryPrinter.hasPersistentPrintContaining(wantSummary) {
		t.Fatalf("persists = %v", dryPrinter.persists)
	}
	if len(dryPrinter.oks) != 0 {
		t.Fatalf("expected no per-entry lines, got %v", dryPrinter.oks)
	}
}

// TestLockDryRunReportsAServerOnlyChange proves the file-level Server field
// is itself reported, and itself flips the summary's verdict, even though
// every collection is untouched and every add/update/remove/unchanged count
// stays exactly what TestLockDryRunNoChangeReportsAllUnchanged's identical
// fixture reports - see that test's own doc comment for the full pairing.
// Without this, a diff that changes only Diff.Server renders zero per-entry
// lines and a count-only summary indistinguishable from "nothing to do",
// which is exactly the bug this test exists to catch: the fixture stages a
// lockfile whose Server disagrees with cfg.Server (rewritten through
// lockfile.Save, not a hand-built struct, so the staged file is exactly what
// a real, older lock run would have left behind) while every entry stays
// untouched.
//
// The two assertions below are pinned by two different mutations - see each
// one's own comment - because they exercise two independent halves of
// reportLockfileDiff's own logic (the server line itself, and the
// Empty()-derived verdict term), and because the first assertion's own
// t.Fatalf would mask the second one's failure under any mutation that also
// breaks the first: the second assertion is reachable only under a mutation
// that leaves the server line intact.
func TestLockDryRunReportsAServerOnlyChange(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	staleServer := "https://old-server.example"
	lf.Server = staleServer
	if err := lockfile.Save(path, lf); err != nil {
		t.Fatalf("save server-only-stale lockfile: %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read staged lockfile: %v", err)
	}

	dryPrinter := &capturingPrinter{}
	dryRuntime := infra.New(dryPrinter, f.server.Client())
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, dryRuntime); err != nil {
		t.Fatalf("dry Lock: %v", err)
	}

	// Mutation (dropping reportLockfileDiff's `if diff.Server != nil` block
	// entirely, leaving no server line at all) fails this check with:
	//
	//	lock_command_test.go:536: oks = []
	if !dryPrinter.hasOkContaining("Would change: server " + staleServer + " -> " + f.cfg.Server) {
		t.Fatalf("oks = %v", dryPrinter.oks)
	}
	// Mutation (keeping the server line, but dropping the
	// `verdict := ...; if diff.Empty() { ... }` logic and its use in the
	// PersistentPrintf format string, leaving a count-only summary) leaves
	// the check above passing, since the server line survives this mutation
	// untouched, and fails this one with:
	//
	//	lock_command_test.go:548: persists = [Dry run: 0 would be added, 0
	//	would be updated, 0 would be removed, 1 unchanged (/.../requirements.lock.yml)]
	wantSummary := "Dry run: lockfile would change; 0 would be added, 0 would be updated, 0 would be removed, 1 unchanged (" + path + ")"
	if !dryPrinter.hasPersistentPrintContaining(wantSummary) {
		t.Fatalf("persists = %v", dryPrinter.persists)
	}

	after, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixture, not user input.
	if err != nil {
		t.Fatalf("read lockfile after dry run: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry run rewrote the lockfile:\n%s", after)
	}
}

// TestLockDryRunWarnsOnAnUnreadableBaseline proves lockDryRunBaseline's own
// disclosure: a lockfile already on disk that fails lockfile.Load (here, an
// unsupported schema version) is warned about by name rather than silently
// treated the same as "no lockfile at all", and the run still succeeds and
// still reports every collection as added - the positive control proving the
// preview did not merely abort on the unreadable file.
//
// Mutation (dropping the Warnf call in lockDryRunBaseline while keeping its
// nil return) fails with:
//
//	lock_command_test.go:587: warns = [--dry-run is active: no artifact will
//	be downloaded, installed, or cached; the resolved metadata caches are
//	still saved no persisted snapshot was found; a dry run will not create
//	one, so the metadata caches this run built are discarded --dry-run:
//	skipping metrics report to /.../metrics.json]
func TestLockDryRunWarnsOnAnUnreadableBaseline(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	mustWriteFile(t, path, []byte("schema_version: 9999\ncollections: []\n"))

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	if !f.printer.hasWarnContaining("cannot be read") {
		t.Fatalf("warns = %v", f.printer.warns)
	}
	if !f.printer.hasOkContaining("Would add: acme.widgets@1.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
}

// TestLockDryRunDoesNotFabricateASnapshot proves a dry run against a cold
// cache - no persisted snapshot yet - never creates one, mirroring
// TestInstallDryRunDoesNotFabricateASnapshot: saveDryRunSnapshotIfPersisted's
// WasPersisted() guard must refuse to save here, or a later cleanup run would
// read the resulting persisted-and-empty snapshot as "nothing is installed or
// warmed anywhere" and sweep the whole extracted store.
//
// Mutation (replacing lockDryRun's saveDryRunSnapshotIfPersisted call with a
// bare state.backend.SaveStore(ctx, state.store)) fails with:
//
//	lock_command_test.go:624: lock --dry-run against a cold cache must not create a persisted snapshot
func TestLockDryRunDoesNotFabricateASnapshot(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if st.WasPersisted() {
		t.Fatal("lock --dry-run against a cold cache must not create a persisted snapshot")
	}
}

// TestLockDryRunSavesMetadataCachesWhenSnapshotExists is
// TestLockDryRunDoesNotFabricateASnapshot's positive control on the same
// fixture: once a real Lock has seeded a persisted snapshot, a later dry run
// still saves the metadata caches a genuinely fresh resolve builds. A second
// collection is registered on the server and added to requirements.yml
// between the seeding Lock and the dry run specifically so the dry run's
// resolve is not just replaying an already-cached result.
//
// The WasPersisted() check below is a PRECONDITION GUARD, not a pin: it is
// satisfied by the seeding Lock call alone, before the dry run ever runs, so
// no mutation of the dry-run path can make it fail while the requirement-spec
// check after it still passes - that property is already pinned by
// TestLockDryRunDoesNotFabricateASnapshot, on the cold-cache fixture where
// WasPersisted() genuinely depends on what the dry run itself did. What
// actually pins THIS test's own claim - that the dry run's own fresh resolve
// was saved, not just that a persisted snapshot exists from some earlier
// write - is the RequirementsSnapshot() check: acme.extra only enters that
// map through this run's own resolve, never through the seeding Lock, which
// never heard of it.
//
// Killing mutation: replacing lockDryRun's saveDryRunSnapshotIfPersisted call
// with a no-op (var saveErr error) leaves WasPersisted() true - the seed
// Lock's own save already made it true - but fails the requirement-spec
// check with:
//
//	lock_command_test.go:684: expected the dry run's own fresh resolve to
//	have saved acme.extra's requirement spec, got map[acme.widgets:{*  galaxy []}]
func TestLockDryRunSavesMetadataCachesWhenSnapshotExists(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("seed Lock: %v", err)
	}

	f.server.AddVersion("acme", "extra", testVersion100, nil)
	mustWriteFile(t, f.cfg.RequirementsFile,
		[]byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n  - name: acme.extra\n    version: \"*\"\n"))
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("dry Lock: %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if !st.WasPersisted() {
		t.Fatal("expected WasPersisted() true: a dry run must still save the metadata caches when a persisted snapshot already existed")
	}
	if _, ok := st.RequirementsSnapshot()["acme.extra"]; !ok {
		t.Fatalf("expected the dry run's own fresh resolve to have saved acme.extra's requirement spec, got %v", st.RequirementsSnapshot())
	}
}

// TestLockDryRunSkipsMetricsAndSaysSo proves writeRunMetrics's own
// cfg.DryRun guard applies to lock exactly as it does to install and warm:
// no metrics file is written, and the operator is told why on stderr. The
// two assertions are pinned by two different mutations - see each one's own
// comment below - because the call-site mutation is unobservable through the
// filesystem alone.
func TestLockDryRunSkipsMetricsAndSaysSo(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Mutation (removing writeRunMetrics's own internal cfg.DryRun guard)
	// fails this check with:
	//
	//	lock_command_test.go:707: expected no metrics report, stat err = <nil>
	if _, err := os.Stat(f.cfg.MetricsFile); !os.IsNotExist(err) {
		t.Fatalf("expected no metrics report, stat err = %v", err)
	}
	// Mutation (removing the writeRunMetrics call from lockDryRun entirely,
	// leaving the file-absence check above still passing) fails this check
	// with:
	//
	//	lock_command_test.go:724: warns = [--dry-run is active: no artifact
	//	will be downloaded, installed, or cached; the resolved metadata
	//	caches are still saved no persisted snapshot was found; a dry run
	//	will not create one, so the metadata caches this run built are
	//	discarded]
	//
	// A file-absence-only version of this test does not catch that second
	// mutation at all: dropping the call site is unobservable through the
	// filesystem, since writeRunMetrics's own guard already means the file
	// would be absent either way.
	if !f.printer.hasWarnContaining("--dry-run: skipping metrics report to " + f.cfg.MetricsFile) {
		t.Fatalf("warns = %v", f.printer.warns)
	}
}

// TestLockDryRunFrozenStillWarns pins the resolved --frozen/--dry-run
// interaction: both the "--frozen has no effect on lock" warning and the
// "--dry-run is active" banner fire together, and the run still succeeds.
// TestLockFrozenIsIgnoredAndNotReported is this test's positive control on
// the identical --frozen warning without --dry-run in play, proving the
// warning's own wording is unaffected by --dry-run rather than this test
// merely asserting a warning that always uses different wording.
func TestLockDryRunFrozenStillWarns(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	f.cfg.Frozen = true
	f.cfg.DryRun = true

	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	if !f.printer.hasWarnContaining("--frozen has no effect on lock") {
		t.Fatalf("expected a --frozen-has-no-effect warning on stderr, got %v", f.printer.warns)
	}
	if !f.printer.hasWarnContaining("--dry-run is active") {
		t.Fatalf("expected the --dry-run banner on stderr, got %v", f.printer.warns)
	}
}

// TestLockDryRunRendersHostileBaselineWithoutActingOnIt proves lock's
// --dry-run preview survives an adversarial baseline lockfile through the
// real production path - lockfile.Save writes it, lockDryRunBaseline's own
// lockfile.Load reads it back, Compare diffs it, reportLockfileDiff prints
// it - not through a hand-built *File handed directly to Compare, which
// would only pin lockfile package behavior TestCompareRendersHostileEntryVerbatim
// (internal/galaxy/lockfile) already covers. Assertion (1) below, that Load
// itself returns the hostile name unmangled, is what makes this a
// production-path test rather than that same Compare unit test in disguise:
// without it, nothing here would prove the hostile bytes ever survived a
// real YAML round trip before reaching Compare at all.
//
// The baseline carries a path-traversal Name ("../../../../etc/passwd"), a
// Source embedding a NUL byte, an ANSI escape, and a CRLF, and an oversized
// Deps element - the identical hostile shape
// TestCompareRendersHostileEntryVerbatim exercises at the lockfile-package
// level, here driven through Lock end to end instead.
//
// This test asserts CONTAINMENT of the traversal name in the printed output,
// deliberately not byte-exact rendering of it or of the control-byte Source:
// containment survives a possible future switch of the printer's format verb
// from %s to %q unchanged (%q leaves "." and "/" unescaped), while a
// control-byte string's exact rendering does not (%q would escape the
// NUL/ANSI/CRLF that %s passes through verbatim) - asserting exact rendering
// here would make this test require a second edit the day that switch
// happens, for a property (byte-exact control-character rendering) this test
// does not exist to pin in the first place. lock.go renders every operator-
// facing identifier with %s today, matching every other printer call site in
// this package - printer-wide sanitization, if it happens, is separate work
// this test does not depend on either way.
func TestLockDryRunRendersHostileBaselineWithoutActingOnIt(t *testing.T) {
	t.Parallel()
	f := newLockRun(t)
	const hostileName = "../../../../etc/passwd"
	hostileSource := "https://x.example\x00\x1b[31m\r\nInstalled: totally.fine"
	oversizedDep := strings.Repeat("d", 10000)

	path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, f.cfg.LockFile)
	stale := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        f.cfg.Server,
		Collections: []lockfile.Entry{
			{Name: hostileName, Version: "9.9.9", Source: hostileSource, Deps: []string{oversizedDep}},
		},
	}
	if err := lockfile.Save(path, stale); err != nil {
		t.Fatalf("save hostile baseline lockfile: %v", err)
	}
	assertHostileNameSurvivesLoad(t, path, hostileName)

	f.cfg.DryRun = true
	if err := Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// (2) the traversal name appears in the printed report - contained, not
	// asserted byte-exact; see this test's own doc comment for why.
	if !f.printer.hasOkContaining(hostileName) {
		t.Fatalf("oks = %v", f.printer.oks)
	}
	assertNoPathMatchesHostileName(t, f.root)

	// (4) the run still succeeded and still reported the legitimate
	// collection - the hostile baseline degraded nothing else in the report.
	if !f.printer.hasOkContaining("Would add: acme.widgets@1.0.0") {
		t.Fatalf("oks = %v", f.printer.oks)
	}
}

// assertHostileNameSurvivesLoad is assertion (1) from
// TestLockDryRunRendersHostileBaselineWithoutActingOnIt's own doc comment:
// the production path (lockfile.Save -> lockfile.Load) itself carries the
// hostile name through unmangled, before Compare ever sees it - the property
// that makes that test a production-path test rather than a Compare unit
// test wearing this file's name. Factored out to keep the caller's own
// cyclomatic complexity within budget.
func assertHostileNameSurvivesLoad(t *testing.T, path, hostileName string) {
	t.Helper()
	loaded, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("lockfile.Load(%s): %v", path, err)
	}
	if len(loaded.Collections) != 1 || loaded.Collections[0].Name != hostileName {
		t.Fatalf("lockfile.Load returned %+v, want the hostile name unmangled", loaded.Collections)
	}
}

// assertNoPathMatchesHostileName is assertion (3) from
// TestLockDryRunRendersHostileBaselineWithoutActingOnIt's own doc comment:
// the preview never touched the filesystem on the hostile entry's behalf -
// no path anywhere under root resolves to, or is even named after, the
// traversal target "passwd". Factored out to keep the caller's own
// cyclomatic complexity within budget; the WalkDir callback records the
// first match into a closure variable rather than returning a dynamically
// constructed error, since err113 forbids exactly that.
func assertNoPathMatchesHostileName(t *testing.T, root string) {
	t.Helper()
	var match string
	walkErr := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if match == "" && strings.Contains(filepath.Base(p), "passwd") {
			match = p
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("filesystem walk: %v", walkErr)
	}
	if match != "" {
		t.Fatalf("found a path matching the hostile name: %s", match)
	}
}
