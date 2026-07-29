package collections

// This file proves the extract-marker tally (marker.go): that a valid marker
// round-trips in the exact pinned wire format, that it detects a file being
// added, removed, or resized under an install tree, that it deliberately
// misses a same-length in-place edit (the documented limit, not a bug), that
// a legacy or garbage marker invalidates quietly while a genuine tally
// mismatch warns loudly, that a stray same-prefixed sibling file never skews
// the tally, and that scanTree itself never follows a symlink. It also proves
// the prefetch scan still uses the cheap installRecordMatches check rather
// than the strict tally, and ships the benchmark that puts a real number on
// the cost this unit adds.

import (
	"bytes"
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
)

// seedValidExtractMarker writes a syntactically and semantically valid
// extract-done marker for installPath by calling writeExtractMarker itself,
// so every test that seeds an "already installed" tree gets a tally that
// actually matches the tree on disk (entries=0 dirs=0 bytes=0 for an empty
// tree) - the shared helper the architect asked for, so canSkipInstall's now
// tally-checking gate does not reject a seed that used to pass under the bare
// os.Stat check the legacy "ok" sentinel satisfied.
func seedValidExtractMarker(t *testing.T, installPath, sha string) {
	t.Helper()
	if err := writeExtractMarker(installPath, sha); err != nil {
		t.Fatalf("seed extract marker: %v", err)
	}
}

// mustMkdirAll creates path (and parents), failing the test on error.
func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// buildFixedMarkerTree creates a small, deterministic file tree - one root
// file, two files under one subdirectory, one file under a nested
// subdirectory - and returns its root plus the exact treeTally it must
// produce, so marker tests can assert against known numbers instead of just
// round-tripping scanTree's own output back on itself.
func buildFixedMarkerTree(t *testing.T) (string, treeTally) {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "file0.txt"), []byte("root-file")) // 9 bytes
	mustMkdirAll(t, filepath.Join(root, "dirA"))
	mustWriteFile(t, filepath.Join(root, "dirA", "file1.txt"), []byte("hello"))  // 5 bytes
	mustWriteFile(t, filepath.Join(root, "dirA", "file2.txt"), []byte("world!")) // 6 bytes
	mustMkdirAll(t, filepath.Join(root, "dirB", "nested"))
	mustWriteFile(t, filepath.Join(root, "dirB", "nested", "file3.txt"), []byte("abc")) // 3 bytes
	// entries: file0, file1, file2, file3 = 4; dirs: dirA, dirB, dirB/nested = 3;
	// bytes: 9 + 5 + 6 + 3 = 23.
	return root, treeTally{Entries: 4, Dirs: 3, Bytes: 23}
}

// TestExtractMarkerRoundTrip pins the exact on-disk marker format, not just
// that writing and then verifying agree with each other: a format
// regression that both writes and parses consistently would still pass a
// round-trip-only test.
func TestExtractMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	root, want := buildFixedMarkerTree(t)
	const sha = "1111111111111111111111111111111111111111111111111111111111111111"

	if err := writeExtractMarker(root, sha); err != nil {
		t.Fatalf("writeExtractMarker: %v", err)
	}

	marker := filepath.Join(root, helpers.ExtractMarkerPrefix+sha)
	got, err := os.ReadFile(marker) //nolint:gosec // marker path is test-controlled
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	wantContent := fmt.Sprintf("go-galaxy-extract-1 entries=%d dirs=%d bytes=%d\n", want.Entries, want.Dirs, want.Bytes)
	if string(got) != wantContent {
		t.Fatalf("marker content = %q, want %q", got, wantContent)
	}

	printer := &capturingPrinter{}
	if !verifyExtractMarker(printer, root, sha) {
		t.Fatalf("expected verifyExtractMarker to accept a freshly written marker")
	}
}

// TestExtractMarkerDetectsDeletedFile proves a file removed from the install
// tree after extraction is caught: the entry count drops, the tally no
// longer matches, and the rejection is reported at Warnf (a live integrity
// signal) with the marker removed so no second walk is needed on the next
// check.
func TestExtractMarkerDetectsDeletedFile(t *testing.T) {
	t.Parallel()
	root, _ := buildFixedMarkerTree(t)
	const sha = "2222222222222222222222222222222222222222222222222222222222222222"
	seedValidExtractMarker(t, root, sha)

	if err := os.Remove(filepath.Join(root, "dirA", "file1.txt")); err != nil {
		t.Fatalf("remove file: %v", err)
	}

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, root, sha) {
		t.Fatalf("expected verifyExtractMarker to reject a tree missing a file")
	}
	assertPathAbsent(t, filepath.Join(root, helpers.ExtractMarkerPrefix+sha))
	if len(printer.warns) == 0 {
		t.Fatalf("expected a Warnf line for a current-format tally mismatch, got none")
	}
}

// TestExtractMarkerDetectsAddedFile proves a file added under the install
// tree after extraction - e.g. a stray write into the shared cache - is
// caught the same way a deletion is.
func TestExtractMarkerDetectsAddedFile(t *testing.T) {
	t.Parallel()
	root, _ := buildFixedMarkerTree(t)
	const sha = "3333333333333333333333333333333333333333333333333333333333333333"
	seedValidExtractMarker(t, root, sha)

	mustWriteFile(t, filepath.Join(root, "dirA", "extra.txt"), []byte("new"))

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, root, sha) {
		t.Fatalf("expected verifyExtractMarker to reject a tree with an added file")
	}
}

// TestExtractMarkerDetectsSizeChange is the direct answer to "an in-place
// edit is detected": rewriting a file with different-length content changes
// its lstat size, which changes the tally's byte sum, which fails the
// comparison.
func TestExtractMarkerDetectsSizeChange(t *testing.T) {
	t.Parallel()
	root, _ := buildFixedMarkerTree(t)
	const sha = "4444444444444444444444444444444444444444444444444444444444444444"
	seedValidExtractMarker(t, root, sha)

	mustWriteFile(t, filepath.Join(root, "dirA", "file1.txt"), []byte("this content is longer than the original"))

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, root, sha) {
		t.Fatalf("expected verifyExtractMarker to reject a tree with a resized file")
	}
}

// TestExtractMarkerMissesEqualSizeEdit pins the documented limit of the
// tally check: an in-place edit that preserves the edited file's exact byte
// length is invisible to it, since the tally only tracks counts and a byte
// sum, never content. This is an accepted tradeoff (see verifyExtractMarker's
// doc comment) - a full re-hash would close it, but at the cost this unit
// exists specifically to avoid paying on every warm install - and this test
// exists so nobody later assumes verifyExtractMarker is a stronger guarantee
// than it actually is.
func TestExtractMarkerMissesEqualSizeEdit(t *testing.T) {
	t.Parallel()
	root, _ := buildFixedMarkerTree(t)
	const sha = "5555555555555555555555555555555555555555555555555555555555555555"
	seedValidExtractMarker(t, root, sha)

	// "hello" -> "HELLO": identical length (5 bytes), different content.
	mustWriteFile(t, filepath.Join(root, "dirA", "file1.txt"), []byte("HELLO"))

	printer := &capturingPrinter{}
	if !verifyExtractMarker(printer, root, sha) {
		t.Fatalf("expected verifyExtractMarker to MISS a same-length in-place edit (documented limit, not a bug)")
	}
}

// TestExtractMarkerLegacyFormatInvalidates proves the pre-tally sentinel
// content ("ok", written by every extractCollection before this unit) is
// treated as unparseable rather than crashing or being misread, and that
// this specific case - expected on every upgrade, for every previously
// installed collection - logs at Debugf only, not Warnf: a warning here
// would be a one-time storm for every user upgrading past the marker format
// change.
func TestExtractMarkerLegacyFormatInvalidates(t *testing.T) {
	t.Parallel()
	root, _ := buildFixedMarkerTree(t)
	const sha = "6666666666666666666666666666666666666666666666666666666666666666"
	marker := filepath.Join(root, helpers.ExtractMarkerPrefix+sha)
	mustWriteFile(t, marker, []byte("ok"))

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, root, sha) {
		t.Fatalf("expected verifyExtractMarker to reject the legacy \"ok\" marker")
	}
	if len(printer.warns) != 0 {
		t.Fatalf("expected no Warnf for a legacy marker, got %v", printer.warns)
	}
	if !printer.hasDebugContaining("legacy or corrupt") {
		t.Fatalf("expected a Debugf line reporting the unparseable legacy marker, got %v", printer.debugs)
	}
	assertPathAbsent(t, marker)
}

// TestExtractMarkerOversizedAndGarbageInvalidate proves two more unparseable
// shapes are both rejected without panicking: a marker well past the bounded
// read cap (rejected without the reader ever learning its true length), and
// a truncated marker that starts with the right version tag but never
// reaches a complete field set.
func TestExtractMarkerOversizedAndGarbageInvalidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []byte
	}{
		{name: "oversized marker", content: bytes.Repeat([]byte("a"), 300)},
		{name: "truncated marker", content: []byte("go-galaxy-extract-1 entries=x")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root, _ := buildFixedMarkerTree(t)
			const sha = "7777777777777777777777777777777777777777777777777777777777777777"
			marker := filepath.Join(root, helpers.ExtractMarkerPrefix+sha)
			mustWriteFile(t, marker, tt.content)

			printer := &capturingPrinter{}
			if verifyExtractMarker(printer, root, sha) {
				t.Fatalf("expected verifyExtractMarker to reject %s", tt.name)
			}
			if len(printer.warns) != 0 {
				t.Fatalf("expected no Warnf for an unparseable marker (%s), got %v", tt.name, printer.warns)
			}
			assertPathAbsent(t, marker)
		})
	}
}

// TestExtractMarkerIgnoresSiblingMarkers proves a stray top-level file that
// merely shares the marker prefix - a different sha's marker, most plausibly
// left behind by a previous version of the same collection - never
// contributes to the tally, in either direction: scanTree must produce the
// identical result whether or not that sibling is present.
func TestExtractMarkerIgnoresSiblingMarkers(t *testing.T) {
	t.Parallel()
	root, want := buildFixedMarkerTree(t)

	before, err := scanTree(root)
	if err != nil {
		t.Fatalf("scanTree before: %v", err)
	}
	if before != want {
		t.Fatalf("scanTree before sibling = %+v, want %+v", before, want)
	}

	mustWriteFile(t, filepath.Join(root, helpers.ExtractMarkerPrefix+"deadbeef"), bytes.Repeat([]byte("x"), 128))

	after, err := scanTree(root)
	if err != nil {
		t.Fatalf("scanTree after: %v", err)
	}
	if after != want {
		t.Fatalf("scanTree after sibling marker = %+v, want unchanged %+v", after, want)
	}
}

// TestScanTreeDoesNotFollowSymlinks proves a symlink at the top level of the
// tree - even one pointing at a directory outside the tree entirely -
// contributes exactly one non-directory entry to the tally and is never
// descended into. Without this, a symlink loop could hang the walk, and a
// symlink to a large external directory could silently inflate (or, if the
// target later changes, silently destabilize) the tally.
func TestScanTreeDoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "real.txt"), []byte("data"))

	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "a.txt"), []byte("aaaa"))
	mustWriteFile(t, filepath.Join(outside, "b.txt"), []byte("bbbbb"))
	mustMkdirAll(t, filepath.Join(outside, "sub"))

	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := scanTree(root)
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}
	// real.txt plus the symlink itself: 2 non-directory entries. The
	// symlinked directory's own contents (2 files, 1 subdirectory) must
	// never be descended into or counted.
	if got.Entries != 2 {
		t.Fatalf("Entries = %d, want 2 (real.txt + the symlink itself, not its target's contents)", got.Entries)
	}
	if got.Dirs != 0 {
		t.Fatalf("Dirs = %d, want 0 (a symlink to a directory must not be descended into)", got.Dirs)
	}
}

// TestExtractMarkerOutcomeZeroValueFailsClosed proves a zero-value
// extractMarkerOutcome{} - the shape a future error path might return by
// mistake, e.g. a bare `return extractMarkerOutcome{}` - never satisfies
// matches(): extractMarkerUnknown is deliberately the zero value of
// extractMarkerStatus, so a construction site that forgets to set status
// explicitly fails closed (treated as invalid) rather than silently
// reporting a valid marker.
func TestExtractMarkerOutcomeZeroValueFailsClosed(t *testing.T) {
	t.Parallel()
	if (extractMarkerOutcome{}).matches() {
		t.Fatal("expected a zero-value extractMarkerOutcome{} to never satisfy matches()")
	}
}

// TestCheckExtractMarkerScanFailedAndMissing covers two checkExtractMarker
// outcomes no other test in this file reaches directly: a scanTree failure
// (the installed tree becomes unreadable after a valid marker was already
// written) and a marker that is simply absent (the shape of every
// first-ever extraction, before writeExtractMarker has ever run). Both
// subtests also prove checkExtractMarker itself never touches the marker -
// unlike verifyExtractMarker, exercised here for the exact messages, tiers,
// and best-effort cleanup its own doc comment promises. Each subtest's body
// lives in its own named function, kept out of this dispatcher, to stay
// under the cognitive-complexity budget.
func TestCheckExtractMarkerScanFailedAndMissing(t *testing.T) {
	t.Run("scan failed", testCheckExtractMarkerScanFailed)
	t.Run("missing", testCheckExtractMarkerMissing)
}

// testCheckExtractMarkerScanFailed makes the installed tree itself
// unreadable after seeding a valid marker, so scanTree's own
// filepath.WalkDir fails, and proves checkExtractMarker reports that
// failure faithfully (status, scanErr, no removal) while verifyExtractMarker
// logs it at Debugf (never Warnf - a scan failure is not a confirmed tally
// mismatch) and still performs its best-effort removal.
func testCheckExtractMarkerScanFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	installPath := t.TempDir()
	const sha = "deadbeefcafe"
	seedValidExtractMarker(t, installPath, sha)
	markerPath := filepath.Join(installPath, helpers.ExtractMarkerPrefix+sha)

	// 0o311 (write+execute, no read): a directory missing the read bit still
	// fails os.ReadDir - so scanTree's filepath.WalkDir fails, as required -
	// but keeping the write bit lets the directory entry for the marker
	// actually be unlinked afterward (verified empirically: a bare
	// execute-only 0o111 blocks os.Remove outright on this filesystem, which
	// would make the removal assertion below untestable rather than
	// genuinely exercised). Deliberately more permissive than 0600 for that
	// reason, not an oversight.
	//nolint:gosec // G302: 0o311 is this test's own fixture permission; see comment above.
	if err := os.Chmod(installPath, 0o311); err != nil {
		t.Fatalf("chmod installPath: %v", err)
	}
	// Restored before t.TempDir's own cleanup runs, which needs to list (and
	// remove) installPath itself.
	t.Cleanup(func() {
		if err := os.Chmod(installPath, helpers.DirMod); err != nil {
			t.Errorf("restore installPath perms: %v", err)
		}
	})

	assertScanFailedOutcome(t, checkExtractMarker(installPath, sha), markerPath)
	assertVerifyExtractMarkerScanFailed(t, installPath, sha, markerPath)
}

// assertScanFailedOutcome checks checkExtractMarker's own return value for
// the scan-failed scenario, and that it never touched the marker file - it
// is a pure read.
func assertScanFailedOutcome(t *testing.T, outcome extractMarkerOutcome, markerPath string) {
	t.Helper()
	if outcome.status != extractMarkerScanFailed {
		t.Fatalf("outcome.status = %v, want extractMarkerScanFailed", outcome.status)
	}
	if outcome.scanErr == nil {
		t.Fatal("expected a non-nil scanErr")
	}
	if outcome.matches() {
		t.Fatal("expected matches() to be false")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected checkExtractMarker to leave the marker untouched, stat error: %v", err)
	}
}

// assertVerifyExtractMarkerScanFailed calls verifyExtractMarker for the
// scan-failed scenario and checks its return value, its Debugf-tier message
// (and the absence of a Warnf line - a scan failure is not a confirmed tally
// mismatch), and that its best-effort removal actually removed the marker.
func assertVerifyExtractMarkerScanFailed(t *testing.T, installPath, sha, markerPath string) {
	t.Helper()
	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, installPath, sha) {
		t.Fatal("expected verifyExtractMarker to return false")
	}
	wantMsg := "failed to scan " + installPath + " to verify its extract marker"
	if !printer.hasDebugContaining(wantMsg) {
		t.Errorf("expected a Debugf line containing %q, got %v", wantMsg, printer.debugs)
	}
	if len(printer.warns) != 0 {
		t.Errorf("expected no Warnf line for a scan failure, got %v", printer.warns)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("expected verifyExtractMarker's best-effort removal to have actually removed the marker, stat error = %v", err)
	}
}

// testCheckExtractMarkerMissing leaves installPath with no marker at all -
// the shape of every first-ever extraction - and proves checkExtractMarker
// and verifyExtractMarker both treat that identically to any other
// unverifiable marker: reported, not removed by the former (nothing to
// remove), logged at Debugf by the latter.
func testCheckExtractMarkerMissing(t *testing.T) {
	installPath := t.TempDir()
	const sha = "deadbeefcafe"
	markerPath := filepath.Join(installPath, helpers.ExtractMarkerPrefix+sha)

	outcome := checkExtractMarker(installPath, sha)
	if outcome.status != extractMarkerMissing {
		t.Fatalf("outcome.status = %v, want extractMarkerMissing", outcome.status)
	}
	if outcome.matches() {
		t.Fatal("expected matches() to be false")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("checkExtractMarker must never create a marker; stat error = %v", err)
	}

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, installPath, sha) {
		t.Fatal("expected verifyExtractMarker to return false")
	}
	wantMsg := "extract marker missing or unreadable at " + markerPath
	if !printer.hasDebugContaining(wantMsg) {
		t.Errorf("expected a Debugf line containing %q, got %v", wantMsg, printer.debugs)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("expected no marker to exist (there was nothing to remove), stat error = %v", err)
	}
}

// TestPrefetchScanUsesCheapCheck proves shouldSchedulePrefetch still calls
// installRecordMatches - the bare-os.Stat cheap check - rather than
// canSkipInstall's strict tally verification: a seeded install whose marker
// is in the legacy "ok" format (which canSkipInstall would now reject) must
// still make the prefetch scan report "already installed", so this unit does
// not accidentally make the prefetch scan pay the cost or the strictness the
// architect scoped to installCollection alone.
func TestPrefetchScanUsesCheapCheck(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0", Type: "galaxy"}
	installPath := filepath.Join(root, "ansible_collections", col.Namespace, col.Name)
	mustMkdirAll(t, installPath)

	const installedSHA = "8888888888888888888888888888888888888888888888888888888888888888"
	mustWriteFile(t, filepath.Join(installPath, helpers.ExtractMarkerPrefix+installedSHA), []byte("ok"))

	infoDir := filepath.Join(root, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, "GALAXY.yml"), []byte("format_version: 1.0.0\n"))

	cfg := &config.Config{DownloadPath: root, Workers: 1}
	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    installPath,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})
	deps := newPrefetchDeps(cfg, infra.New(noopPrinter{}, http.DefaultClient), st, &presenceArtifacts{})

	if shouldSchedulePrefetch(t.Context(), deps, col) {
		t.Fatalf("expected shouldSchedulePrefetch to report already-installed via the cheap check, even with a legacy marker")
	}
}

// BenchmarkScanTree measures one scanTree pass over a single
// collection-sized tree: 500 files spread across 50 subdirectories at ~2000
// bytes each, matching the per-collection shape behind the architect's
// fleet-scale measurement (100 such trees - 50,000 files, 100 MB total, APFS,
// warm - costing 0.56 ms serially per tree, 222 ms total serially, 73 ms at 8
// workers). This keeps that number reproducible in the repository rather
// than living only in a report.
func BenchmarkScanTree(b *testing.B) {
	root := b.TempDir()
	const subdirs = 50
	const filesPerSubdir = 10
	const fileSize = 2000
	content := bytes.Repeat([]byte("x"), fileSize)
	for i := range subdirs {
		dir := filepath.Join(root, fmt.Sprintf("sub%d", i))
		if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
			b.Fatalf("mkdir %s: %v", dir, err)
		}
		for j := range filesPerSubdir {
			path := filepath.Join(dir, fmt.Sprintf("file%d.dat", j))
			if err := os.WriteFile(path, content, helpers.FileMod); err != nil {
				b.Fatalf("write %s: %v", path, err)
			}
		}
	}

	b.ResetTimer()
	for range b.N {
		if _, err := scanTree(root); err != nil {
			b.Fatalf("scanTree: %v", err)
		}
	}
}
