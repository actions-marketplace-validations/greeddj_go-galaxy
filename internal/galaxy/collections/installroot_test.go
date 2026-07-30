package collections

// This file exercises classifyCollectionsRootError directly rather than
// through a production write site, since its own doc comment now makes a
// claim - "load-bearing for classification, not for containment" - that no
// end-to-end test can falsify: an end-to-end test only ever observes the
// already-refused write, never whether the function correctly told an escape
// apart from an ordinary filesystem error along the way.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestClassifyCollectionsRootErrorReturnsRawErrorForNonEscapeFailure is the
// test classifyCollectionsRootError's own doc comment cannot substitute for:
// a root operation can fail for a reason that has nothing to do with an
// escape - here, a regular file sitting where a directory component is
// expected, measured (see the probe run ahead of writing this test) to
// surface from the kernel as "openat blocker: not a directory", never
// anything mentioning a symlink or an escape. classifyCollectionsRootError's
// Lstat walk must find no symlink component here (there is none) and fall
// through to returning err unchanged, not helpers.ErrCollectionsPathEscape.
// Without this test, the walk could be widened into a catch-all - reporting
// every root-operation failure as an escape - and nothing in this package
// would notice, since every other test in this file drives an actual escape.
func TestClassifyCollectionsRootErrorReturnsRawErrorForNonEscapeFailure(t *testing.T) {
	t.Parallel()
	downloadPath := t.TempDir()
	mustWriteFile(t, filepath.Join(downloadPath, "blocker"), []byte("not a directory"))

	root, err := os.OpenRoot(downloadPath)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = root.Close()
	})

	mkdirErr := root.MkdirAll("blocker/sub", helpers.DirMod)
	if mkdirErr == nil {
		t.Fatal("expected root.MkdirAll(\"blocker/sub\", ...) to fail against a regular file component")
	}

	got := classifyCollectionsRootError(root, "blocker/sub", mkdirErr)
	if errors.Is(got, helpers.ErrCollectionsPathEscape) {
		t.Fatalf("classifyCollectionsRootError(%v) = %v, want it NOT to report ErrCollectionsPathEscape for a non-escape failure",
			mkdirErr, got)
	}
	if !errors.Is(got, mkdirErr) {
		t.Fatalf("classifyCollectionsRootError(%v) = %v, want the raw error returned unchanged", mkdirErr, got)
	}
}

// TestClassifyCollectionsRootErrorReturnsEscapeForSymlinkComponent is the
// positive control for TestClassifyCollectionsRootErrorReturnsRawErrorForNonEscapeFailure:
// the identical call shape, but with an actual symlink component standing in
// for "blocker", must be reported as helpers.ErrCollectionsPathEscape. Without
// this, "not reported as an escape" on the sibling test would be
// unfalsifiable - a classifyCollectionsRootError that never recognized an
// escape at all would also pass that test for the wrong reason.
func TestClassifyCollectionsRootErrorReturnsEscapeForSymlinkComponent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, downloadPath)
	outside := filepath.Join(root, "outside")
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(downloadPath, "escape")); err != nil {
		t.Fatalf("symlink escape -> outside: %v", err)
	}

	osRoot, err := os.OpenRoot(downloadPath)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", downloadPath, err)
	}
	t.Cleanup(func() {
		_ = osRoot.Close()
	})

	mkdirErr := osRoot.MkdirAll("escape/sub", helpers.DirMod)
	if mkdirErr == nil {
		t.Fatal("expected root.MkdirAll(\"escape/sub\", ...) to fail against a symlinked component")
	}

	got := classifyCollectionsRootError(osRoot, "escape/sub", mkdirErr)
	if !errors.Is(got, helpers.ErrCollectionsPathEscape) {
		t.Fatalf("classifyCollectionsRootError(%v) = %v, want errors.Is helpers.ErrCollectionsPathEscape", mkdirErr, got)
	}
}
