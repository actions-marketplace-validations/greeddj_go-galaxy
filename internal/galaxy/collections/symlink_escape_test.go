package collections

// This file proves the install-side symlink hardening (installroot.go):
// every write this package makes into cfg.DownloadPath funnels through one
// os.Root, so a symlinked ansible_collections - or a symlinked namespace/name
// component beneath it - cannot redirect a write outside DownloadPath.
// Each test targets one write site directly (extractCollection, writeGalaxyInfo,
// verifyExtractMarker, canSkipInstall) with a pre-existing "outside" tree
// standing in for real content a symlink swap would otherwise have destroyed
// or leaked into - the same shape TestExtractCollectionRefusesNonCanonicalSHABeforeDestroyingTree
// already uses for the malformed-sha guard. The full-run version (a symlinked
// ansible_collections reached through Start/installWithState) lives in
// symlink_run_test.go instead, since only that level can observe "the whole
// run failed once, not once per collection".

import (
	"context"
	"errors"
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

// symlinkForm selects how symlinkedEscapeFixture points its symlinked
// "ansible_collections" at the sibling "outside" directory. Both forms
// resolve to the identical on-disk target, so a test that only ever exercises
// symlinkAbsolute cannot tell "this code refuses any absolute symlink target
// outright" (a coarser, weaker property - see
// TestAnsibleCollectionsSymlinkToSiblingInsideDownloadPathSucceeds, where an
// absolute target is refused even when it geometrically resolves inside the
// root) apart from "this code refuses an escaping target" (the property this
// file actually exists to prove). A committed requirements.yml can only ever
// drive col's identity, never the symlink itself, but the shape a hostile
// checkout ships - a relative symlink such as "ansible_collections ->
// ../outside" that both survives a repository clone and travels with it - is
// exactly symlinkRelative.
type symlinkForm int

const (
	// symlinkAbsolute points ansible_collections at outside's absolute path.
	symlinkAbsolute symlinkForm = iota
	// symlinkRelative points ansible_collections at outside via a relative
	// ".." target, resolved against downloadPath (the symlink's own
	// directory), landing on the exact same on-disk location as
	// symlinkAbsolute.
	symlinkRelative
)

// symlinkedEscapeFixture builds a downloadPath containing a symlinked
// "ansible_collections" pointing at a sibling "outside" directory - as an
// absolute or a relative target, per form - and returns the installTarget for
// col rooted at downloadPath alongside the real, on-disk location the
// symlink redirects col's writes to (outside/<namespace>/<name>). Every test
// in this file drives one production write site against this same shape.
func symlinkedEscapeFixture(t *testing.T, col collection, form symlinkForm) (installTarget, string) {
	t.Helper()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, downloadPath)
	outside := filepath.Join(root, "outside")
	mustMkdirAll(t, outside)

	linkTarget := outside
	if form == symlinkRelative {
		linkTarget = filepath.Join("..", "outside")
	}
	if err := os.Symlink(linkTarget, filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> %s: %v", linkTarget, err)
	}

	osRoot, err := os.OpenRoot(downloadPath)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = osRoot.Close()
	})

	cfg := &config.Config{DownloadPath: downloadPath, Server: "https://galaxy.example.com"}
	target, ok := newInstallTarget(osRoot, cfg, col)
	if !ok {
		t.Fatalf("newInstallTarget(%s.%s@%s): unsafe identity", col.Namespace, col.Name, col.Version)
	}
	return target, filepath.Join(outside, col.Namespace, col.Name)
}

// realInstallFixture builds col's installTarget the same shape
// symlinkedEscapeFixture does - a downloadPath, an "ansible_collections"
// entry, col's install directory beneath it - except that entry is a real
// directory, never a symlink. It exists so each escape test can be paired
// with a positive control: the same seed (InstalledEntry, marker, sidecar)
// placed where the production code is actually meant to find it, proving the
// fixture is capable of producing the accepting answer at all before its
// escape sibling is trusted to prove the refusing one. Without this, a
// fixture that can only ever say "false" (a stat that fails for any reason,
// escape or not) would make every assertion of "false" vacuous.
func realInstallFixture(t *testing.T, col collection) installTarget {
	t.Helper()
	downloadPath := t.TempDir()
	cfg := &config.Config{DownloadPath: downloadPath, Server: "https://galaxy.example.com"}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)
	return target
}

// TestExtractCollectionSymlinkedPrefixLeavesOutsideTreeIntact is the
// load-bearing proof that extractCollection's os.RemoveAll(installPath) does
// not follow a symlinked ansible_collections and destroy whatever real
// content lives at its target. A real, pre-existing tree is seeded at the
// symlink's actual on-disk target - the same location an unrooted
// os.RemoveAll would reach - and must survive byte-identical.
// t.Errorf, not t.Fatalf, on the sentinel error check: the tree assertion
// below is what actually discriminates a real fix from one that merely
// returns the right error class while still destroying data through a stale
// os.RemoveAll call, so it must run regardless of whether the error check
// itself passes.
//
// Both symlinkForm values are exercised, on this write site specifically -
// extractCollection's os.RemoveAll is the severest primitive in the whole
// pipeline (see its own doc comment: "the previous tree may be gone" on
// failure, not "nothing was written yet"), so it is where an absolute-only
// suite would be most consequential to leave vacuous. A relative target (the
// "ansible_collections -> ../outside" shape a hostile repository checkout
// would actually ship, since it survives a clone byte-for-byte where an
// absolute host path could not) proves the refusal tracks the escape itself,
// not merely "any absolute symlink target", which
// TestAnsibleCollectionsSymlinkToSiblingInsideDownloadPathSucceeds already
// shows this codebase treats differently from an escaping one.
func TestExtractCollectionSymlinkedPrefixLeavesOutsideTreeIntact(t *testing.T) {
	t.Parallel()
	for _, form := range []symlinkForm{symlinkAbsolute, symlinkRelative} {
		t.Run(symlinkFormName(form), func(t *testing.T) {
			t.Parallel()
			col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
			target, outsideInstallDir := symlinkedEscapeFixture(t, col, form)

			preexisting := filepath.Join(outsideInstallDir, "README.md")
			const preexistingContent = "# a real, previously installed tree\n"
			mustMkdirAll(t, outsideInstallDir)
			mustWriteFile(t, preexisting, []byte(preexistingContent))

			tarRoot := t.TempDir()
			tarPath := filepath.Join(tarRoot, "artifact.tar.gz")
			mustWriteFile(t, tarPath, buildMinimalTarGz(t))

			runtime := infra.New(noopPrinter{}, http.DefaultClient)
			err := extractCollection(context.Background(), col, tarPath, target, runtime, nil, "", false)
			if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
				t.Errorf("extractCollection error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
			}
			assertFileContent(t, preexisting, preexistingContent)
		})
	}
}

// symlinkFormName renders form as a subtest name.
func symlinkFormName(form symlinkForm) string {
	if form == symlinkRelative {
		return "relative"
	}
	return "absolute"
}

// TestWriteGalaxyInfoSymlinkedPrefixWritesNothingOutside proves
// writeGalaxyInfo's own rooted MkdirAll/WriteFile pair refuses the same
// symlinked prefix before creating the .info sidecar directory anywhere,
// inside or outside the collections root: target.info's own MkdirAll is the
// very first statement writeGalaxyInfo executes, so a refusal there leaves no
// WriteFile ever attempted.
func TestWriteGalaxyInfoSymlinkedPrefixWritesNothingOutside(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target, outsideInstallDir := symlinkedEscapeFixture(t, col, symlinkAbsolute)
	// The sidecar directory ("<ns>.<name>-<version>.info") is a sibling of
	// col's install directory under the symlinked prefix, not underneath it.
	outsideInfoDir := filepath.Join(filepath.Dir(filepath.Dir(outsideInstallDir)), col.Namespace+"."+col.Name+"-"+col.Version+".info")

	// writeGalaxyInfo only reads cfg.Server (for the sidecar body); it never
	// touches cfg.DownloadPath, so a bare cfg is enough here.
	cfg := &config.Config{Server: "https://galaxy.example.com"}
	err := writeGalaxyInfo(target, cfg, col, nil)
	if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
		t.Errorf("writeGalaxyInfo error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
	}
	assertPathAbsent(t, outsideInfoDir)
	assertPathAbsent(t, filepath.Join(outsideInfoDir, galaxyYAMLFileName))
}

// TestVerifyExtractMarkerSymlinkedPrefixLeavesOutsideMarkerIntact proves
// verifyExtractMarker's best-effort cleanup never unlinks a marker reachable
// only through a symlinked prefix. The cleanup resolves the marker path
// through target.root rather than a plain path join
// (filepath.Join(installPath, ...) then os.Remove); a plain join would
// follow the symlink, but target.root refuses to traverse the escaping
// "ansible_collections" component at all, so the outside marker survives
// even though verifyExtractMarker still correctly reports it as unverified.
func TestVerifyExtractMarkerSymlinkedPrefixLeavesOutsideMarkerIntact(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target, outsideInstallDir := symlinkedEscapeFixture(t, col, symlinkAbsolute)
	mustMkdirAll(t, outsideInstallDir)

	const sha = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcd12"
	const markerContent = "go-galaxy-extract-1 entries=0 dirs=0 bytes=0\n"
	markerPath := filepath.Join(outsideInstallDir, helpers.ExtractMarkerPrefix+sha)
	mustWriteFile(t, markerPath, []byte(markerContent))

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, sha) {
		t.Fatal("expected verifyExtractMarker to reject a marker reachable only through a symlinked prefix")
	}
	assertFileContent(t, markerPath, markerContent)
}

// TestVerifyExtractMarkerRealInstallReturnsTrue is the positive control for
// TestVerifyExtractMarkerSymlinkedPrefixLeavesOutsideMarkerIntact: the
// identical marker content, seeded through the real production write path
// (writeExtractMarker, via seedValidExtractMarker) at a real, non-symlinked
// install directory, must be accepted. Without this, "false" on the symlinked
// sibling would be unfalsifiable - a fixture whose stat can only ever fail,
// escape or not, would make that refusal free rather than earned.
func TestVerifyExtractMarkerRealInstallReturnsTrue(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target := realInstallFixture(t, col)

	const sha = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	seedValidExtractMarker(t, target, sha)

	printer := &capturingPrinter{}
	if !verifyExtractMarker(printer, target, sha) {
		t.Fatal("expected verifyExtractMarker to accept a valid marker at a real, non-symlinked install directory")
	}
}

// TestInstallRecordMatchesSymlinkedPrefixReturnsFalse is
// installRecordMatches's own single-gate proof, driving it directly rather
// than through canSkipInstall: the same seeded InstalledEntry, extract
// marker, and GALAXY.yml sidecar as TestCanSkipInstallSymlinkedPrefixReturnsFalse,
// reachable only through the symlinked "ansible_collections" prefix. Unlike
// that test - which stays true even with either one of two rooting gates
// reverted, since the other alone still suffices - this one is killed by
// reverting matchingInstalledRecord's own two target.root.Stat calls to a
// plain, absolute os.Stat on their own, with no second gate standing behind it.
func TestInstallRecordMatchesSymlinkedPrefixReturnsFalse(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target, outsideInstallDir := symlinkedEscapeFixture(t, col, symlinkAbsolute)
	mustMkdirAll(t, outsideInstallDir)

	const sha = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	markerPath := filepath.Join(outsideInstallDir, helpers.ExtractMarkerPrefix+sha)
	mustWriteFile(t, markerPath, []byte("go-galaxy-extract-1 entries=0 dirs=0 bytes=0\n"))
	outsideInfoDir := filepath.Join(filepath.Dir(filepath.Dir(outsideInstallDir)), col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, outsideInfoDir)
	mustWriteFile(t, filepath.Join(outsideInfoDir, galaxyYAMLFileName), []byte("format_version: 1.0.0\n"))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})

	if installRecordMatches(target, col, st) {
		t.Fatal("expected installRecordMatches to return false when the record, marker, and " +
			"sidecar are all reachable only through a symlinked prefix")
	}
}

// TestInstallRecordMatchesRealInstallReturnsTrue is the positive control for
// TestInstallRecordMatchesSymlinkedPrefixReturnsFalse: the identical
// InstalledEntry, marker, and sidecar seed, placed at a real (non-symlinked)
// install directory, must be accepted - proving the fixture shape can say
// "true" before its symlinked sibling is trusted to prove "false".
func TestInstallRecordMatchesRealInstallReturnsTrue(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target := realInstallFixture(t, col)

	const sha = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	seedValidExtractMarker(t, target, sha)
	infoDir := filepath.Join(filepath.Dir(filepath.Dir(target.path)), col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, galaxyYAMLFileName), []byte("format_version: 1.0.0\n"))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})

	if !installRecordMatches(target, col, st) {
		t.Fatal("expected installRecordMatches to return true against a real, non-symlinked install " +
			"directory holding a matching record, marker, and sidecar")
	}
}

// TestCanSkipInstallSymlinkedPrefixReturnsFalse proves canSkipInstall never
// mistakes a symlink-only fake install for a real one: an InstalledEntry, an
// extract marker, and a GALAXY.yml sidecar all exist, but only reachable
// through the symlinked "ansible_collections" prefix, exactly as they would
// if an earlier, unpatched binary had installed through the symlink. Every
// check matchingInstalledRecord performs goes through target.root.Stat, which
// refuses to traverse the escaping component, so this must report false - a
// silent wrong "true" here would skip a real install and keep serving
// whatever sits at the symlink's target forever.
//
// This test is conjunction-killed by design, not a weak or vacuous check:
// canSkipInstall layers two independent rooting gates -
// matchingInstalledRecord's own two target.root.Stat calls, and
// verifyExtractMarker's rooted scanTree/readExtractMarker pass - and
// reverting only one of them still leaves the other refusing the symlinked
// seed on its own, so this test stays green either way. That is expected, not
// a gap: each gate has its own single-gate killer elsewhere -
// TestInstallRecordMatchesSymlinkedPrefixReturnsFalse pins the
// matchingInstalledRecord half, TestVerifyExtractMarkerSymlinkedPrefixLeavesOutsideMarkerIntact
// pins the verifyExtractMarker half - and this test is what proves the two
// gates actually compose into one fail-closed decision at the canSkipInstall
// level, the property canSkipInstall's own defense-in-depth is supposed to
// buy. A reader who reverts one gate, sees this test stay green, and reads
// that as "this test caught nothing" should look at those two tests instead,
// not conclude this one is worthless.
func TestCanSkipInstallSymlinkedPrefixReturnsFalse(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target, outsideInstallDir := symlinkedEscapeFixture(t, col, symlinkAbsolute)
	mustMkdirAll(t, outsideInstallDir)

	const sha = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	markerPath := filepath.Join(outsideInstallDir, helpers.ExtractMarkerPrefix+sha)
	mustWriteFile(t, markerPath, []byte("go-galaxy-extract-1 entries=0 dirs=0 bytes=0\n"))
	outsideInfoDir := filepath.Join(filepath.Dir(filepath.Dir(outsideInstallDir)), col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, outsideInfoDir)
	mustWriteFile(t, filepath.Join(outsideInfoDir, galaxyYAMLFileName), []byte("format_version: 1.0.0\n"))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})

	if canSkipInstall(target, col, st, noopPrinter{}) {
		t.Fatal("expected canSkipInstall to return false when the record, marker, and sidecar are all reachable only through a symlinked prefix")
	}
}

// TestCanSkipInstallRealInstallReturnsTrue is the positive control for
// TestCanSkipInstallSymlinkedPrefixReturnsFalse: the identical InstalledEntry,
// marker, and sidecar seed, placed at a real (non-symlinked) install
// directory, must be accepted. This is what makes the symlinked test's
// "false" meaningful rather than a fixture that could only ever say "false" -
// see TestCanSkipInstallPinGate and TestCanSkipInstallSourceGate in
// lock_pin_test.go for the same shape of positive proof applied to
// canSkipInstall's other gates.
func TestCanSkipInstallRealInstallReturnsTrue(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	target := realInstallFixture(t, col)

	const sha = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	seedValidExtractMarker(t, target, sha)
	infoDir := filepath.Join(filepath.Dir(filepath.Dir(target.path)), col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, galaxyYAMLFileName), []byte("format_version: 1.0.0\n"))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})

	if !canSkipInstall(target, col, st, noopPrinter{}) {
		t.Fatal("expected canSkipInstall to return true against a real, non-symlinked install " +
			"directory holding a matching record, marker, and sidecar")
	}
}
