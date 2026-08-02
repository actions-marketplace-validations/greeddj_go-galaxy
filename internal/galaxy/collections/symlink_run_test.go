package collections

// This file drives the symlink hardening at the whole-run level (Start),
// where two properties become observable that no single write-site test can
// show on its own: that a symlinked ansible_collections aborts the entire run
// with exactly one operator-facing failure, before any collection is even
// dispatched to a worker, rather than one failure per collection in the
// level; and that install --dry-run against an absent DownloadPath never
// creates it while still classifying every collection, exactly as it would
// against a real one.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestSymlinkedAnsibleCollectionsFailsWholeRunWithoutDestroyingOutsideTree
// runs a real install against a DownloadPath whose ansible_collections is a
// symlink to a directory holding real, pre-existing content, and must fail with
// helpers.ErrCollectionsPathEscape, and that pre-existing content must
// survive byte-identical - the containment guarantee that an escaping
// ansible_collections symlink must never let a run RemoveAll what it points
// at, observed at the same entry point (Start) a real CI job would call.
// t.Errorf, not t.Fatalf, on the sentinel check: the filesystem assertions
// below are what actually discriminate a real fix, so they must still run
// even if the error class itself regresses.
//
// Both symlinkForm values are exercised, matching
// TestExtractCollectionSymlinkedPrefixLeavesOutsideTreeIntact's own reasoning:
// a relative "ansible_collections -> ../outside" target is what a hostile
// checkout actually ships (it survives a repository clone; an absolute host
// path could not), so a suite that only ever plants an absolute target could
// be passing for the coarser, wrong reason ("any absolute symlink is
// refused") rather than the one this test exists to prove ("an escaping
// target is refused").
func TestSymlinkedAnsibleCollectionsFailsWholeRunWithoutDestroyingOutsideTree(t *testing.T) {
	t.Parallel()
	for _, form := range []symlinkForm{symlinkAbsolute, symlinkRelative} {
		t.Run(symlinkFormName(form), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			downloadPath := filepath.Join(root, "install")
			mustMkdirAll(t, downloadPath)
			outside := filepath.Join(root, "outside")
			mustMkdirAll(t, outside)
			victim := filepath.Join(outside, "victim.txt")
			const victimContent = "# a real, pre-existing outside tree\n"
			mustWriteFile(t, victim, []byte(victimContent))
			linkTarget := outside
			if form == symlinkRelative {
				linkTarget = filepath.Join("..", "outside")
			}
			if err := os.Symlink(linkTarget, filepath.Join(downloadPath, "ansible_collections")); err != nil {
				t.Fatalf("symlink ansible_collections -> %s: %v", linkTarget, err)
			}

			cacheDir := filepath.Join(root, "cache")
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
			runtime := infra.New(noopPrinter{}, srv.Client())

			err := Start(context.Background(), cfg, runtime)
			if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
				t.Errorf("Start error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
			}
			assertFileContent(t, victim, victimContent)
			entries, readErr := os.ReadDir(outside)
			if readErr != nil {
				t.Fatalf("read outside dir: %v", readErr)
			}
			if len(entries) != 1 || entries[0].Name() != "victim.txt" {
				t.Errorf("expected outside dir to contain only the pre-existing victim.txt, got %v", entries)
			}
			if got := srv.Total(); got != 0 {
				t.Errorf("fake server request count = %d, want 0 (the escape must be caught before resolution ever starts)", got)
			}
		})
	}
}

// TestSymlinkedAnsibleCollectionsProducesOneFailureNotOnePerCollection checks
// that a requirements.yml resolving to two collections (acme.app depending on
// acme.lib) must still surface exactly one operator-facing failure, not one
// per collection, when ansible_collections is symlinked. This is what
// installWithState opening the collections root once, before requirements
// are even loaded (see its own doc comment), buys over opening it once per
// collection: the escape aborts the run during initialization, so
// installLevels - and its per-collection "Failed: ..." Errorf line - is never
// reached for either collection. The fake server's zero request count is the
// strongest proof: resolution itself never started, so neither collection was
// ever considered individually.
func TestSymlinkedAnsibleCollectionsProducesOneFailureNotOnePerCollection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, downloadPath)
	outside := filepath.Join(root, "outside")
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> outside: %v", err)
	}

	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})
	srv.AddVersion("acme", "lib", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          4,
	}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	err := Start(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrCollectionsPathEscape) {
		t.Fatalf("Start error = %v, want errors.Is helpers.ErrCollectionsPathEscape", err)
	}
	// Start's own Errorf wrapper ("Error: %s") is the run's single top-level
	// failure line; a per-collection failure would additionally add one
	// "Failed: acme.<name> error: ..." line per collection dispatched.
	if len(printer.errs) != 1 {
		t.Errorf("printer recorded %d Errorf lines, want exactly 1 (the top-level run failure), got %v", len(printer.errs), printer.errs)
	}
	if printer.hasErrContaining("Failed: acme.") {
		t.Errorf("expected no per-collection \"Failed: acme.*\" line, got %v", printer.errs)
	}
	if got := srv.Total(); got != 0 {
		t.Errorf("fake server request count = %d, want 0 (neither collection was ever individually considered)", got)
	}
}

// TestDryRunInstallLeavesAbsentDownloadPathAbsentAndStillClassifies checks that
// install --dry-run against a DownloadPath that does not exist yet must never
// create it (openCollectionsRoot's own create=false contract: a dry run must
// never create the directory it is only describing), while still reporting
// every collection's would-install verdict exactly as it would against a real
// one - the nil root openCollectionsRoot returns for an absent DownloadPath
// on a dry run only ever makes installDryRunProbe treat every collection as
// "not settled", never as unclassifiable.
func TestDryRunInstallLeavesAbsentDownloadPathAbsentAndStillClassifies(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install") // deliberately never created
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
		DryRun:           true,
	}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertPathAbsent(t, downloadPath)
	if !printer.hasOkContaining("Would install: acme.app@1.0.0") {
		t.Errorf("expected acme.app classified as would-install, got okLines %v", printer.okLines())
	}
	if !printer.hasPersistentPrintContaining("1 would install, 0 already up to date, 0 would fail") {
		t.Errorf("expected the dry-run summary line to count the collection, got %v", printer.persists)
	}
}
