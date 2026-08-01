package collections

// This file proves the symlink hardening in installroot.go is not a blanket
// refusal of every symlink: os.Root allows a component symlink that resolves
// inside the root it was opened at, and openCollectionsRoot's own os.MkdirAll
// on cfg.DownloadPath is a no-op when DownloadPath is itself a symlink to an
// existing directory. Both are load-bearing for drop-in ansible.cfg
// compatibility - collections_path is routinely a symlink in real CI setups
// (a cache mount, a workspace alias) - so a regression here would silently
// break every one of those deployments while fixing the escape this whole
// change targets.

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

// TestDownloadPathSymlinkInstallSucceeds proves --download-path (or
// [defaults] collections_path) pointing at a symlink to a real directory - the
// drop-in-compatibility case openCollectionsRoot's own doc comment names -
// still installs cleanly, landing the real files under the symlink's target.
func TestDownloadPathSymlinkInstallSucceeds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realDownloadPath := filepath.Join(root, "real-install")
	downloadPathLink := filepath.Join(root, "install-link")
	mustMkdirAll(t, realDownloadPath)
	if err := os.Symlink(realDownloadPath, downloadPathLink); err != nil {
		t.Fatalf("symlink downloadPath -> real: %v", err)
	}

	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPathLink,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	runtime := infra.New(noopPrinter{}, srv.Client())

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	manifest := filepath.Join(realDownloadPath, "ansible_collections", "acme", "app", "MANIFEST.json")
	assertExists(t, manifest)
}

// TestAnsibleCollectionsSymlinkToSiblingInsideDownloadPathSucceeds proves the
// escape guard is specifically about escaping cfg.DownloadPath, not about
// symlinks in general: "ansible_collections" itself being a symlink to
// another directory that still resolves inside DownloadPath must install
// successfully, since os.Root allows a component symlink whose target stays
// within the root it was opened at.
//
// The symlink target must be relative ("real-ansible-collections", not an
// absolute path to the same directory): verified empirically against this
// Go version's os.Root, an absolute symlink target is refused unconditionally
// - even when it geometrically resolves inside the root - because os.Root
// never consults the absolute filesystem namespace at all; only a relative
// target is walked component-by-component against the root and allowed to
// stay within it. This is the one case in this file where the distinction
// matters: every escape test elsewhere in this package uses an absolute
// target on purpose, since escaping is exactly what it must prove.
func TestAnsibleCollectionsSymlinkToSiblingInsideDownloadPathSucceeds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, downloadPath)
	realCollectionsDir := filepath.Join(downloadPath, "real-ansible-collections")
	mustMkdirAll(t, realCollectionsDir)
	if err := os.Symlink("real-ansible-collections", filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> sibling inside DownloadPath: %v", err)
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

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	manifest := filepath.Join(realCollectionsDir, "acme", "app", "MANIFEST.json")
	assertExists(t, manifest)
}

// TestNewInstallTargetRejectsUnsafeIdentityAndNilRoot proves newInstallTarget
// - the single chokepoint every construction of an installTarget goes through
// - refuses each of col.Namespace, col.Name, and col.Version independently
// when it fails helpers.IsPathElement, and refuses a nil root outright rather
// than panicking on first use, matching the doc comment's own "fails closed
// here rather than by convention" claim for warm's deliberate nil root.
func TestNewInstallTargetRejectsUnsafeIdentityAndNilRoot(t *testing.T) {
	t.Parallel()
	downloadPath := t.TempDir()
	validRoot := newTestCollectionsRoot(t, downloadPath)
	cfg := &config.Config{DownloadPath: downloadPath}

	base := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}

	tests := []struct {
		root *os.Root
		name string
		col  collection
	}{
		{name: "unsafe namespace", root: validRoot, col: collection{Namespace: "../escape", Name: base.Name, Version: base.Version}},
		{name: "unsafe name", root: validRoot, col: collection{Namespace: base.Namespace, Name: "..", Version: base.Version}},
		{name: "unsafe version", root: validRoot, col: collection{Namespace: base.Namespace, Name: base.Name, Version: "../../.."}},
		{name: "nil root", root: nil, col: base},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := newInstallTarget(tc.root, cfg, tc.col); ok {
				t.Fatalf("newInstallTarget(ns=%q name=%q version=%q, root=%v) ok = true, want false",
					tc.col.Namespace, tc.col.Name, tc.col.Version, tc.root)
			}
		})
	}
}

// TestBuildCollectionsMapRejectsUnsafeNamespace proves buildCollectionsMap's
// own IsPathElement guard - kept even though newInstallTarget validates the
// same three components again later per collection, see its own doc comment
// for why a caller's check does not make a callee's guard redundant - fires
// before any install work starts. helpers.SplitFQDN itself performs no
// path-safety validation at all (TestSplitFQDNDoesNotValidatePathSafety), so
// this is the guard that actually stops "foo/../.." from reaching a real
// filesystem write through this call path.
func TestBuildCollectionsMapRejectsUnsafeNamespace(t *testing.T) {
	t.Parallel()
	resolved := map[string]collection{
		"x": {Namespace: "foo/../..", Name: "bar", Version: "1.0.0"},
	}
	_, err := buildCollectionsMap(resolved)
	if !errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) {
		t.Fatalf("buildCollectionsMap error = %v, want errors.Is helpers.ErrUnsafeCollectionIdentifier", err)
	}
}

// TestBuildCollectionsMapRejectsInvalidVersion proves buildCollectionsMap's
// version guard fires under its own sentinel, helpers.ErrInvalidCollectionVersion,
// distinct from ErrUnsafeCollectionIdentifier above: "*" is a syntactically
// safe path element (helpers.IsPathElement("*") is true) but not a version
// anything could install, which is exactly the shape a poisoned snapshot or
// an unvalidated lockfile entry can carry. TestSolverResultSlotsIntoInstallLevels
// (solve_test.go) is this test's positive control on the same function: an
// exact version reaches buildInstallLevels successfully through the
// identical call.
func TestBuildCollectionsMapRejectsInvalidVersion(t *testing.T) {
	t.Parallel()
	resolved := map[string]collection{
		"x": {Namespace: "acme", Name: "widgets", Version: "*"},
	}
	_, err := buildCollectionsMap(resolved)
	if !errors.Is(err, helpers.ErrInvalidCollectionVersion) {
		t.Fatalf("buildCollectionsMap error = %v, want errors.Is helpers.ErrInvalidCollectionVersion", err)
	}
}
