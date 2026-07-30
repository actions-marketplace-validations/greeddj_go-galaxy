package collections

// This file provides the shared test scaffolding every test that needs a
// real installTarget or a real os.Root builds on, so each test file does not
// hand-roll its own os.OpenRoot/newInstallTarget boilerplate and risk one of
// them skipping the cleanup or the validation another gets right.

import (
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// newTestCollectionsRoot opens (creating if needed) the collections root for
// downloadPath via openCollectionsRoot, the same entry point installWithState
// itself uses, and registers t.Cleanup to close it. This is the helper every
// test that drives production code expecting a live *os.Root (installLevels,
// installCollection, shouldSchedulePrefetch, installDryRunProbe, ...) should
// use, so the root is opened and torn down exactly the way a real run does.
func newTestCollectionsRoot(t *testing.T, downloadPath string) *os.Root {
	t.Helper()
	root, err := openCollectionsRoot(downloadPath, true)
	if err != nil {
		t.Fatalf("openCollectionsRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = root.Close()
	})
	return root
}

// newTestInstallTarget opens a fresh collections root for cfg.DownloadPath
// (via newTestCollectionsRoot) and builds col's installTarget through the
// real newInstallTarget chokepoint, failing the test outright if col's
// identity is reported unsafe - every caller of this helper is exercising
// something else and expects a valid target to work with.
func newTestInstallTarget(t *testing.T, cfg *config.Config, col collection) installTarget {
	t.Helper()
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	target, ok := newInstallTarget(root, cfg, col)
	if !ok {
		t.Fatalf("newInstallTarget(%s.%s@%s): unsafe identity", col.Namespace, col.Name, col.Version)
	}
	return target
}

// newFlatInstallTarget builds an installTarget whose "install directory" is
// dir itself (rel = "."), for marker-level unit tests that operate directly
// on a bare tree rather than the ansible_collections/<namespace>/<name>
// layout a real install produces. It deliberately bypasses newInstallTarget's
// own identifier validation - these tests exercise scanTree/markerRel/
// writeExtractMarker/verifyExtractMarker directly and supply their own
// (often deliberately unsafe) sha, not a namespace/name/version triple.
func newFlatInstallTarget(t *testing.T, dir string) installTarget {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		_ = root.Close()
	})
	return installTarget{root: root, rel: ".", path: dir}
}
