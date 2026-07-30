package collections

// This file proves extractCollection's own guard against a non-canonical
// artifactSHA: the check runs before any of the destructive work
// (verifyExtractMarker, os.RemoveAll, MkdirAll, unpack) rather than being
// left to writeExtractMarker's own guard at the end of the function, which
// runs only after all of that has already happened.

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"

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
	err := extractCollection(col, tarPath, target, runtime, nil, traversalSHA)
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Errorf("extractCollection error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	assertFileContent(t, preexisting, preexistingContent)
}
