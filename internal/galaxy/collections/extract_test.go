package collections

// This file proves extractCollection's own guard against a non-canonical
// artifactSHA: the check runs before any of the destructive work
// (verifyExtractMarker, os.RemoveAll, MkdirAll, unpack) rather than being
// left to writeExtractMarker's own guard at the end of the function, which
// runs only after all of that has already happened.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// TestExtractCollectionRefusesNonCanonicalSHABeforeDestroyingTree is the
// load-bearing proof: installPath already holds a real, pre-existing tree
// (as a warm reinstall would find), the tarball is a valid one
// extractCollection could otherwise extract cleanly, and artifactSHA is a
// traversal string. Both the sentinel and the untouched pre-existing tree
// are asserted - the tree assertion is what actually discriminates, since
// the error alone would still pass even if the check were left at the end
// of the function (writeExtractMarker's own guard). t.Errorf, not
// t.Fatalf, on the sentinel check, so the tree check still runs if the
// sentinel check itself fails under mutation.
func TestExtractCollectionRefusesNonCanonicalSHABeforeDestroyingTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	installPath := filepath.Join(root, "ansible_collections", "acme", "widgets")
	preexisting := filepath.Join(installPath, "README.md")
	const preexistingContent = "# a real, previously installed tree\n"
	mustMkdirAll(t, installPath)
	mustWriteFile(t, preexisting, []byte(preexistingContent))

	tarPath := filepath.Join(root, "artifact.tar.gz")
	mustWriteFile(t, tarPath, buildMinimalTarGz(t))

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	target := newFlatInstallTarget(t, installPath)

	const traversalSHA = "../../../../../../home/ci/.ssh/authorized_keys"
	err := extractCollection(context.Background(), col, tarPath, target, runtime, nil, traversalSHA, false)
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Errorf("extractCollection error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	assertFileContent(t, preexisting, preexistingContent)
}

// TestResetCollectionInfoSweepsOnlyThisCollectionsVersions pins what the
// sweep a collection extraction makes may remove: every .info directory of
// this collection, whatever version it names, and nothing else. A directory
// that merely starts with the same characters - one whose remainder is not an
// exact version - and another collection's directory both survive, and the
// extracted version's own directory comes back empty.
func TestResetCollectionInfoSweepsOnlyThisCollectionsVersions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.Config{DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)
	collectionsDir := filepath.Join(root, "ansible_collections")
	for _, dir := range []string{
		"acme.widgets-1.0.0.info", "acme.widgets-2.1.0.info", "acme.widgets-3.0.0-rc.1.info",
		"acme.widgets-notes.info", "acme.widgets_extra-1.0.0.info", "other.widgets-1.0.0.info",
	} {
		mustMkdirAll(t, filepath.Join(collectionsDir, dir))
		mustWriteFile(t, filepath.Join(collectionsDir, dir, galaxyYAMLFileName), []byte("seeded\n"))
	}

	if err := resetCollectionInfo(target); err != nil {
		t.Fatalf("resetCollectionInfo: %v", err)
	}

	for _, gone := range []string{"acme.widgets-2.1.0.info", "acme.widgets-3.0.0-rc.1.info"} {
		assertPathAbsent(t, filepath.Join(collectionsDir, gone))
	}
	for _, kept := range []string{"acme.widgets-notes.info", "acme.widgets_extra-1.0.0.info", "other.widgets-1.0.0.info"} {
		assertExists(t, filepath.Join(collectionsDir, kept, galaxyYAMLFileName))
	}
	entries, err := os.ReadDir(filepath.Join(collectionsDir, "acme.widgets-1.0.0.info"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("own .info directory = %v (%v), want it recreated empty", entries, err)
	}
}
