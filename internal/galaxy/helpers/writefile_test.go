package helpers

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// mustReadFile reads path and fails the test on error. Centralizing the read
// keeps each WriteFileAtomic test case focused on its own assertion and gives
// gosec's G304 (potential file inclusion via variable) a single call site to
// annotate instead of one per test.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	//nolint:gosec // path is built from this test's own t.TempDir fixture, never external input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return data
}

// TestWriteFileAtomicWritesContentAndMode asserts a successful write round
// trips the content, stamps FileMod exactly (no umask filtering), and leaves
// no temp file behind in the target directory.
func TestWriteFileAtomicWritesContentAndMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	want := []byte(`{"ok":true}`)

	if err := WriteFileAtomic(path, want); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got := mustReadFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != FileMod {
		t.Fatalf("mode = %o, want %o", perm, FileMod)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no leftover temp files, found %v", matches)
	}
}

// TestWriteFileAtomicCreatesMissingParents asserts the parent tree is created
// when absent, since directory existence is WriteFileAtomic's own precondition
// rather than the caller's. The directory mode is deliberately not asserted:
// os.MkdirAll is umask-filtered, unlike the file's explicit Chmod to FileMod,
// so an exact DirMod check would be flaky under a non-default umask.
func TestWriteFileAtomicCreatesMissingParents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "report.json")
	want := []byte("nested")

	if err := WriteFileAtomic(path, want); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got := mustReadFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestWriteFileAtomicReplacesSymlinkWithoutFollowing is the regression test
// for the whole point of this helper: a symlink pre-planted at the target
// path must be replaced by rename, never followed and truncated in place.
// This is the test that fails against a naive os.WriteFile(path, ...)
// implementation.
func TestWriteFileAtomicReplacesSymlinkWithoutFollowing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	victimDir := filepath.Join(dir, "victim-dir")
	if err := os.Mkdir(victimDir, DirMod); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	victim := filepath.Join(victimDir, "victim.txt")
	sentinel := []byte("do-not-touch")
	if err := os.WriteFile(victim, sentinel, FileMod); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}

	target := filepath.Join(dir, "report.json")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	want := []byte("new-content")
	if err := WriteFileAtomic(target, want); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	gotVictim := mustReadFile(t, victim)
	if !bytes.Equal(gotVictim, sentinel) {
		t.Fatalf("victim content = %q, want unchanged %q", gotVictim, sentinel)
	}

	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat target: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("target is still a symlink after WriteFileAtomic")
	}

	gotTarget := mustReadFile(t, target)
	if !bytes.Equal(gotTarget, want) {
		t.Fatalf("target content = %q, want %q", gotTarget, want)
	}
}

// TestWriteFileAtomicOverwritesExistingFile asserts a second write to the
// same path replaces the first: this is why the target itself is not opened
// with O_EXCL/O_NOFOLLOW - that would break the legitimate overwrite-on-rerun
// case a repeated install/metrics run relies on.
func TestWriteFileAtomicOverwritesExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")

	if err := WriteFileAtomic(path, []byte("first")); err != nil {
		t.Fatalf("WriteFileAtomic (first): %v", err)
	}
	if err := WriteFileAtomic(path, []byte("second")); err != nil {
		t.Fatalf("WriteFileAtomic (second): %v", err)
	}

	got := mustReadFile(t, path)
	if string(got) != "second" {
		t.Fatalf("content = %q, want %q", got, "second")
	}
}

// TestWriteFileAtomicLeavesNoTempOnFailure asserts a failed write (here, the
// target path is itself an existing directory, so the final os.Rename fails)
// leaves no temp file behind. The error is asserted only as non-nil: the
// underlying errno differs by platform (EEXIST on darwin, EISDIR on Linux),
// and this failure mode is root-safe (unlike a permission-based failure), so
// no root skip is needed.
func TestWriteFileAtomicLeavesNoTempOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "report.json")
	if err := os.Mkdir(target, DirMod); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("data")); err == nil {
		t.Fatalf("expected WriteFileAtomic to fail when target is an existing directory")
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no leftover temp files, found %v", matches)
	}
}
