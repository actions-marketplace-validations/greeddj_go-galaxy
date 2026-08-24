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
//
// Most tests here drive marker.go's functions directly against a "flat"
// installTarget (newFlatInstallTarget: rel = ".", the tree's own root) rather
// than the ansible_collections/<ns>/<name> layout a real install produces -
// these are marker-level unit tests, not install-pipeline tests, and a flat
// target keeps every filepath.Join in this file's own assertions
// byte-identical to what target.path already is.

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// validMarkerSHA is a syntactically valid 64-character lowercase hex digest
// used wherever a marker test needs a sha that passes helpers.IsSHA256Hex
// without caring what content it is a hash of.
const validMarkerSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// seedValidExtractMarker writes a syntactically and semantically valid
// extract-done marker for target by calling writeExtractMarker itself, so
// every test that seeds an "already installed" tree gets a tally that
// actually matches the tree on disk (entries=0 dirs=0 bytes=0 for an empty
// tree): canSkipInstall's tally-checking gate rejects a marker that merely
// exists but does not carry a matching tally, so a placeholder that only has
// to be found by a Stat is not enough here.
func seedValidExtractMarker(t *testing.T, target installTarget, sha string) {
	t.Helper()
	if err := writeExtractMarker(target, sha); err != nil {
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
// subdirectory - and returns a flat installTarget rooted at it plus the exact
// treeTally it must produce, so marker tests can assert against known numbers
// instead of just round-tripping scanTree's own output back on itself.
func buildFixedMarkerTree(t *testing.T) (installTarget, treeTally) {
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
	return newFlatInstallTarget(t, root), treeTally{Entries: 4, Dirs: 3, Bytes: 23}
}

// TestExtractMarkerRoundTrip pins the exact on-disk marker format, not just
// that writing and then verifying agree with each other: a format
// regression that both writes and parses consistently would still pass a
// round-trip-only test.
func TestExtractMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	target, want := buildFixedMarkerTree(t)
	const sha = "1111111111111111111111111111111111111111111111111111111111111111"

	if err := writeExtractMarker(target, sha); err != nil {
		t.Fatalf("writeExtractMarker: %v", err)
	}

	marker := filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha)
	got, err := os.ReadFile(marker) //nolint:gosec // marker path is test-controlled
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	wantContent := fmt.Sprintf("go-galaxy-extract-1 entries=%d dirs=%d bytes=%d\n", want.Entries, want.Dirs, want.Bytes)
	if string(got) != wantContent {
		t.Fatalf("marker content = %q, want %q", got, wantContent)
	}

	printer := &capturingPrinter{}
	if !verifyExtractMarker(printer, target, sha) {
		t.Fatalf("expected verifyExtractMarker to accept a freshly written marker")
	}
}

// extractMarkerMutationCase is one row of TestExtractMarkerMutationCases: a
// single mutation applied to a freshly seeded install tree, the verdict
// verifyExtractMarker must return for it, and an optional extra check for a
// row that asserts more than that verdict alone.
type extractMarkerMutationCase struct {
	mutate     func(t *testing.T, target installTarget)
	extraCheck func(t *testing.T, target installTarget, sha string, printer *capturingPrinter)
	name       string
	wantVerify bool
}

// extractMarkerMutationCases enumerates one row per post-extraction mutation
// the tally is meant to catch, plus the one it deliberately does not. Every
// row uses validMarkerSHA: each gets its own tree from buildFixedMarkerTree,
// so the sha only has to pass helpers.IsSHA256Hex, never tell one row's
// marker apart from another's.
func extractMarkerMutationCases() []extractMarkerMutationCase {
	return []extractMarkerMutationCase{
		{
			// Proves a file removed from the install tree after extraction is
			// caught: the entry count drops, the tally no longer matches, and
			// the rejection is reported at Warnf (a live integrity signal)
			// with the marker removed so no second walk is needed on the next
			// check.
			name: "deletes a file",
			mutate: func(t *testing.T, target installTarget) {
				t.Helper()
				if err := os.Remove(filepath.Join(target.path, "dirA", "file1.txt")); err != nil {
					t.Fatalf("remove file: %v", err)
				}
			},
			extraCheck: func(t *testing.T, target installTarget, sha string, printer *capturingPrinter) {
				t.Helper()
				assertPathAbsent(t, filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha))
				if len(printer.warns) == 0 {
					t.Fatalf("expected a Warnf line for a current-format tally mismatch, got none")
				}
			},
			wantVerify: false,
		},
		{
			// Proves a file added under the install tree after extraction -
			// e.g. a stray write into the shared cache - is caught the same
			// way a deletion is.
			name: "adds a file",
			mutate: func(t *testing.T, target installTarget) {
				t.Helper()
				mustWriteFile(t, filepath.Join(target.path, "dirA", "extra.txt"), []byte("new"))
			},
			wantVerify: false,
		},
		{
			// The direct answer to "an in-place edit is detected": rewriting a
			// file with different-length content changes its lstat size, which
			// changes the tally's byte sum, which fails the comparison.
			name: "resizes a file",
			mutate: func(t *testing.T, target installTarget) {
				t.Helper()
				mustWriteFile(t, filepath.Join(target.path, "dirA", "file1.txt"), []byte("this content is longer than the original"))
			},
			wantVerify: false,
		},
		{
			// Pins the documented limit of the tally check: an in-place edit
			// that preserves the edited file's exact byte length is invisible
			// to it, since the tally only tracks counts and a byte sum, never
			// content. This is an accepted tradeoff (see verifyExtractMarker's
			// doc comment) - a full re-hash would close it, but at the cost
			// this unit exists specifically to avoid paying on every warm
			// install - and this row exists so nobody later assumes
			// verifyExtractMarker is a stronger guarantee than it actually is.
			name: "misses an equal-size edit",
			mutate: func(t *testing.T, target installTarget) {
				t.Helper()
				// "hello" -> "HELLO": identical length (5 bytes), different content.
				mustWriteFile(t, filepath.Join(target.path, "dirA", "file1.txt"), []byte("HELLO"))
			},
			wantVerify: true,
		},
	}
}

// TestExtractMarkerMutationCases drives each post-extraction mutation against
// its own freshly seeded tree and asserts the verdict verifyExtractMarker
// reaches for it. A row's extraCheck is where anything beyond that verdict
// goes: only the deletion row has one, pinning a rejection's two observable
// effects - the marker removed, a Warnf line emitted - once rather than on
// every rejecting row.
func TestExtractMarkerMutationCases(t *testing.T) {
	t.Parallel()

	for _, tc := range extractMarkerMutationCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target, _ := buildFixedMarkerTree(t)
			seedValidExtractMarker(t, target, validMarkerSHA)

			tc.mutate(t, target)

			printer := &capturingPrinter{}
			if got := verifyExtractMarker(printer, target, validMarkerSHA); got != tc.wantVerify {
				t.Fatalf("verifyExtractMarker = %v, want %v", got, tc.wantVerify)
			}
			if tc.extraCheck != nil {
				tc.extraCheck(t, target, validMarkerSHA, printer)
			}
		})
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
	target, _ := buildFixedMarkerTree(t)
	const sha = "6666666666666666666666666666666666666666666666666666666666666666"
	marker := filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha)
	mustWriteFile(t, marker, []byte("ok"))

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, sha) {
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
			target, _ := buildFixedMarkerTree(t)
			const sha = "7777777777777777777777777777777777777777777777777777777777777777"
			marker := filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha)
			mustWriteFile(t, marker, tt.content)

			printer := &capturingPrinter{}
			if verifyExtractMarker(printer, target, sha) {
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
	target, want := buildFixedMarkerTree(t)

	before, err := scanTree(target)
	if err != nil {
		t.Fatalf("scanTree before: %v", err)
	}
	if before != want {
		t.Fatalf("scanTree before sibling = %+v, want %+v", before, want)
	}

	mustWriteFile(t, filepath.Join(target.path, helpers.ExtractMarkerPrefix+"deadbeef"), bytes.Repeat([]byte("x"), 128))

	after, err := scanTree(target)
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

	target := newFlatInstallTarget(t, root)
	got, err := scanTree(target)
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
// unreadable after seeding a valid marker, so scanTree's own fs.WalkDir
// fails, and proves checkExtractMarker reports that failure faithfully
// (status, scanErr, no removal) while verifyExtractMarker logs it at Debugf
// (never Warnf - a scan failure is not a confirmed tally mismatch) and still
// performs its best-effort removal.
func testCheckExtractMarkerScanFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	installPath := t.TempDir()
	target := newFlatInstallTarget(t, installPath)
	const sha = "deadbeefcafedeadbeefcafedeadbeefcafedeadbeefcafedeadbeefcafedead"
	seedValidExtractMarker(t, target, sha)
	markerPath := filepath.Join(installPath, helpers.ExtractMarkerPrefix+sha)

	// 0o311 (write+execute, no read): a directory missing the read bit still
	// fails a directory listing - so scanTree's fs.WalkDir fails, as required -
	// but keeping the write bit lets the directory entry for the marker
	// actually be unlinked afterward (verified empirically: a bare
	// execute-only 0o111 blocks Remove outright on this filesystem, which
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

	assertScanFailedOutcome(t, checkExtractMarker(target, sha), markerPath)
	assertVerifyExtractMarkerScanFailed(t, target, sha, markerPath)
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
func assertVerifyExtractMarkerScanFailed(t *testing.T, target installTarget, sha, markerPath string) {
	t.Helper()
	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, sha) {
		t.Fatal("expected verifyExtractMarker to return false")
	}
	wantMsg := "failed to scan " + target.path + " to verify its extract marker"
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
	target := newFlatInstallTarget(t, installPath)
	const sha = "deadbeefcafedeadbeefcafedeadbeefcafedeadbeefcafedeadbeefcafedead"
	markerPath := filepath.Join(installPath, helpers.ExtractMarkerPrefix+sha)

	outcome := checkExtractMarker(target, sha)
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
	if verifyExtractMarker(printer, target, sha) {
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

// TestPrefetchScanUsesCheapCheck proves shouldSchedulePrefetch calls
// installRecordMatches - the cheap check, which roots the marker path through
// markerRel and then only asks target.root.Stat whether it and the sidecar
// are there, reading neither - rather than
// canSkipInstall's strict tally verification: a seeded install whose marker
// is in the legacy "ok" format (which canSkipInstall rejects) must
// still make the prefetch scan report "already installed", so the prefetch
// scan never pays the cost or the strictness that belongs to
// installCollection alone.
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
	mustWriteFile(t, filepath.Join(infoDir, "GALAXY.yml"), sidecarFor(col))

	cfg := &config.Config{DownloadPath: root, Workers: 1}
	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    installPath,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})
	testRoot := newTestCollectionsRoot(t, root)
	deps := newPrefetchDeps(cfg, infra.New(noopPrinter{}, http.DefaultClient), st, &presenceArtifacts{}, testRoot)

	if schedule, _ := shouldSchedulePrefetch(t.Context(), deps, col); schedule {
		t.Fatalf("expected shouldSchedulePrefetch to report already-installed via the cheap check, even with a legacy marker")
	}
}

// buildTraversalFixture builds an installPath four real path elements deep
// under a "containment" directory (collections/ansible_collections/ns/name -
// the exact shape a real install produces), itself nested one level inside a
// t.TempDir() sandbox, keeping every path this fixture's tests can possibly
// reach - including an unbounded escape - inside the sandbox this test owns
// and t.TempDir() cleans up, never a real host path. The traversal counts
// below are load-bearing: six ".." segments (no leading dot) exactly cancel
// the four real installPath elements plus the one pop that only undoes the
// ".extract-done.." literal filename the join produces (never a real ".."
// token on its own), landing the escape exactly at containment - the same
// arithmetic a real root/home/ci/.ssh/authorized_keys escape would require.
// A leading "./" on that same six-segment sha adds one more real pop with no
// corresponding cancellation, landing one level further out, at sandbox
// itself - the unbounded property: capping ".." tokens at installPath's own
// component count would not have stopped this, since the leading "./"
// buys an extra pop for free.
//
// These tests drive markerRel/verifyExtractMarker/writeExtractMarker/
// checkExtractMarker directly against a flat installTarget (rel = "."), not
// through newInstallTarget/cfg.DownloadPath - the guard under test here is
// markerRel's own sha validation, unconditional and independent of the root
// boundary, which is why a traversal sha is refused before any path is ever
// joined regardless of what target.rel is.
//
// Returns sandbox, containment, installPath, in that order (unnamed, per
// nonamedreturns).
func buildTraversalFixture(t *testing.T) (string, string, string) {
	t.Helper()
	sandbox := t.TempDir()
	containment := filepath.Join(sandbox, "root")
	installPath := filepath.Join(containment, "collections", "ansible_collections", "ns", "name")
	mustMkdirAll(t, installPath)
	return sandbox, containment, installPath
}

// TestVerifyExtractMarkerRefusesTraversalSHA proves verifyExtractMarker
// rejects a traversal sha before ever computing a marker path from it: a
// victim seeded at exactly the location a naive join (installPath plus
// ".extract-done." plus the raw sha) would reach
// (containment/home/ci/.ssh/authorized_keys, six ".." segments popping
// installPath's four real elements plus the one that only cancels the
// ".extract-done.." literal name) survives byte-identical, and exactly one
// Warnf line is emitted - never a Debugf, since an unsafe sha is a live
// integrity signal, not an expected upgrade artifact.
func TestVerifyExtractMarkerRefusesTraversalSHA(t *testing.T) {
	t.Parallel()
	_, containment, installPath := buildTraversalFixture(t)
	target := newFlatInstallTarget(t, installPath)

	victim := filepath.Join(containment, "home", "ci", ".ssh", "authorized_keys")
	const victimContent = "ssh-ed25519 AAAA... ci@legit\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	const traversalSHA = "../../../../../../home/ci/.ssh/authorized_keys"
	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, traversalSHA) {
		t.Fatal("expected verifyExtractMarker to reject a traversal sha")
	}
	assertFileContent(t, victim, victimContent)
	if len(printer.warns) != 1 {
		t.Fatalf("expected exactly one Warnf line, got %v", printer.warns)
	}
	if len(printer.debugs) != 0 {
		t.Fatalf("expected no Debugf line for an unsafe sha, got %v", printer.debugs)
	}
}

// TestVerifyExtractMarkerRefusesUnboundedTraversalSHA covers the leading
// "./" variant: the same six ".." segments as
// TestVerifyExtractMarkerRefusesTraversalSHA, but with one more real pop
// than that capped case buys for free, landing one level past containment
// (at sandbox itself, standing in for a real /etc/passwd escape past the
// test root entirely). Nothing under sandbox - not just
// under installPath - may be touched.
func TestVerifyExtractMarkerRefusesUnboundedTraversalSHA(t *testing.T) {
	t.Parallel()
	sandbox, _, installPath := buildTraversalFixture(t)
	target := newFlatInstallTarget(t, installPath)

	victim := filepath.Join(sandbox, "etc", "passwd")
	const victimContent = "root:x:0:0:root:/root:/bin/sh\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	const traversalSHA = "./../../../../../../etc/passwd"
	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, traversalSHA) {
		t.Fatal("expected verifyExtractMarker to reject an unbounded traversal sha")
	}
	assertFileContent(t, victim, victimContent)
	if len(printer.warns) != 1 {
		t.Fatalf("expected exactly one Warnf line, got %v", printer.warns)
	}
}

// TestWriteExtractMarkerRefusesTraversalSHA proves writeExtractMarker - the
// hard-error side of this guard, since it runs only after a real extraction
// - refuses the same traversal sha before scanTree, Remove, or WriteFile
// ever run: the victim survives untouched, no marker is created anywhere
// under installPath, and the returned error wraps
// helpers.ErrMalformedArtifactSHA256.
func TestWriteExtractMarkerRefusesTraversalSHA(t *testing.T) {
	t.Parallel()
	_, containment, installPath := buildTraversalFixture(t)
	target := newFlatInstallTarget(t, installPath)

	victim := filepath.Join(containment, "home", "ci", ".ssh", "authorized_keys")
	const victimContent = "ssh-ed25519 AAAA... ci@legit\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	const traversalSHA = "../../../../../../home/ci/.ssh/authorized_keys"
	err := writeExtractMarker(target, traversalSHA)
	// t.Errorf, not t.Fatalf: the victim-survival and empty-installPath
	// assertions below must still run even if this one fails, so a mutation
	// that returns the wrong error class but still touches the filesystem is
	// caught by those, not masked by an early abort here.
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Errorf("writeExtractMarker error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	assertFileContent(t, victim, victimContent)

	entries, readErr := os.ReadDir(installPath)
	if readErr != nil {
		t.Fatalf("read installPath: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected installPath to remain empty (no marker written), got %v", entries)
	}
}

// TestCheckExtractMarkerReportsUnsafeSHA proves the pure predicate side of
// this guard: checkExtractMarker reports extractMarkerUnsafeSHA (never
// matches()) for a traversal sha, and never touches installPath at all -
// consistent with it being a pure read on every other status.
func TestCheckExtractMarkerReportsUnsafeSHA(t *testing.T) {
	t.Parallel()
	_, _, installPath := buildTraversalFixture(t)
	target := newFlatInstallTarget(t, installPath)

	before, err := os.ReadDir(installPath)
	if err != nil {
		t.Fatalf("read installPath before: %v", err)
	}

	const traversalSHA = "../../../../../../home/ci/.ssh/authorized_keys"
	outcome := checkExtractMarker(target, traversalSHA)
	if outcome.status != extractMarkerUnsafeSHA {
		t.Fatalf("outcome.status = %v, want extractMarkerUnsafeSHA", outcome.status)
	}
	if outcome.matches() {
		t.Fatal("expected matches() to be false")
	}

	after, err := os.ReadDir(installPath)
	if err != nil {
		t.Fatalf("read installPath after: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("installPath directory listing changed: before=%v after=%v", before, after)
	}
}

// TestMarkerRelRejectsNonDigest is markerRel's own table: it must return
// ok=false for anything that is not exactly 64 lowercase hex characters -
// including short-but-hex, uppercase, a multi-element path, and both dot
// forms - and ok=true with the exact expected join for one real digest.
func TestMarkerRelRejectsNonDigest(t *testing.T) {
	t.Parallel()
	installPath := filepath.Join(t.TempDir(), "ansible_collections", "ns", "name")
	mustMkdirAll(t, installPath)
	target := newFlatInstallTarget(t, installPath)

	badCases := []struct {
		name string
		sha  string
	}{
		{"empty", ""},
		{"short but hex", "deadbeef"},
		{"uppercase", strings.ToUpper(validMarkerSHA)},
		{"multi-element path", "a/b"},
		{"double dot", ".."},
		{"traversal", "../../../../../../home/ci/.ssh/authorized_keys"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := markerRel(target, tc.sha); ok {
				t.Fatalf("markerRel(%q, %q) ok = true, want false", installPath, tc.sha)
			}
		})
	}

	got, ok := markerRel(target, validMarkerSHA)
	if !ok {
		t.Fatalf("markerRel with a valid 64-hex sha: ok = false, want true")
	}
	want := path.Join(target.rel, helpers.ExtractMarkerPrefix+validMarkerSHA)
	if got != want {
		t.Fatalf("markerRel = %q, want %q", got, want)
	}
}

// BenchmarkScanTree measures one scanTree pass over a single
// collection-sized tree: 500 files spread across 50 subdirectories at ~2000
// bytes each, matching the per-collection shape behind a fleet-scale
// measurement (100 such trees - 50,000 files, 100 MB total, APFS, warm -
// costing 0.56 ms serially per tree, 222 ms total serially, 73 ms at 8
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
			p := filepath.Join(dir, fmt.Sprintf("file%d.dat", j))
			if err := os.WriteFile(p, content, helpers.FileMod); err != nil {
				b.Fatalf("write %s: %v", p, err)
			}
		}
	}

	osRoot, err := os.OpenRoot(root)
	if err != nil {
		b.Fatalf("os.OpenRoot(%s): %v", root, err)
	}
	b.Cleanup(func() {
		_ = osRoot.Close()
	})
	target := installTarget{root: osRoot, rel: ".", path: root}

	for b.Loop() {
		if _, err := scanTree(target); err != nil {
			b.Fatalf("scanTree: %v", err)
		}
	}
}
