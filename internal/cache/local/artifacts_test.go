package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// testSHA is a canonical 64-char lowercase hex digest used across these
// tests: "0123456789abcdef" repeated four times, so its length is
// self-evidently correct at a glance.
const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// testArtifactKey is the cache key every test in this file commits and
// fetches under, factored out since none of them ever vary it.
const testArtifactKey = "ns.name-1.0.0.tar.gz"

// commitTempArtifact seeds a temp file with content and commits it under
// testArtifactKey, returning the resulting ArtifactFile. It fails the test
// on any error.
func commitTempArtifact(t *testing.T, a *Artifacts, content []byte, meta map[string]string) cacheManager.ArtifactFile {
	t.Helper()
	file, cleanup, err := a.TempFile(context.Background(), ".download-")
	if err != nil {
		t.Fatalf("TempFile error: %v", err)
	}
	defer cleanup()
	if _, err := file.Write(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	result, err := a.Commit(context.Background(), testArtifactKey, file.Name(), meta)
	if err != nil {
		t.Fatalf("Commit error: %v", err)
	}
	return result
}

func TestArtifactsCommitWritesSHASidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)

	result := commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	if got := result.Meta["sha256"]; got != testSHA {
		t.Fatalf("expected Commit to return sha256 %q in Meta, got %q", testSHA, got)
	}
	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	data, err := os.ReadFile(sidecarPath) //nolint:gosec // sidecarPath is a test-controlled temp path.
	if err != nil {
		t.Fatalf("expected sidecar file to exist, read error: %v", err)
	}
	if strings.TrimSpace(string(data)) != testSHA {
		t.Fatalf("expected sidecar content %q, got %q", testSHA, string(data))
	}
}

// TestArtifactsCommitSwallowsSidecarWriteFailure proves that Commit still
// reports success (with the tarball in place) when the sidecar cannot be
// written - here because a directory occupies the sidecar's path - since the
// artifact itself was already committed by that point, and a missing
// sidecar merely falls back to hashing on the next Fetch.
func TestArtifactsCommitSwallowsSidecarWriteFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)

	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	if err := os.MkdirAll(sidecarPath, helpers.DirMod); err != nil {
		t.Fatalf("seed blocking directory at sidecar path: %v", err)
	}

	file, cleanup, err := a.TempFile(context.Background(), ".download-")
	if err != nil {
		t.Fatalf("TempFile error: %v", err)
	}
	defer cleanup()
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	result, err := a.Commit(context.Background(), testArtifactKey, file.Name(), map[string]string{"sha256": testSHA})
	if err != nil {
		t.Fatalf("expected Commit to succeed despite the sidecar write failure, got %v", err)
	}
	wantPath := filepath.Join(dir, testArtifactKey)
	if result.Path != wantPath {
		t.Fatalf("expected committed path %q, got %q", wantPath, result.Path)
	}
	if _, statErr := os.Stat(result.Path); statErr != nil {
		t.Fatalf("expected the tarball to be committed, stat error: %v", statErr)
	}
}

func TestArtifactsFetchReturnsSidecarSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	fetched, err := a.Fetch(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if got := fetched.Meta["sha256"]; got != testSHA {
		t.Fatalf("expected Fetch to return sha256 %q via Meta, got %q", testSHA, got)
	}
}

func TestArtifactsFetchMissingSidecarReturnsNilMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	// Commit with no sha256 in meta, so no sidecar is ever written.
	commitTempArtifact(t, a, []byte("tarball bytes"), nil)

	fetched, err := a.Fetch(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if fetched.Meta != nil {
		t.Fatalf("expected nil Meta when no sidecar exists, got %#v", fetched.Meta)
	}
}

func TestArtifactsFetchTornSidecarReturnsNilMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	// Simulate a torn/short write: truncate the sidecar to fewer than 64 hex chars.
	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(testSHA[:10]), helpers.FileMod); err != nil {
		t.Fatalf("truncate sidecar: %v", err)
	}

	fetched, err := a.Fetch(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if fetched.Meta != nil {
		t.Fatalf("expected nil Meta for a torn sidecar, got %#v", fetched.Meta)
	}
}

func TestArtifactsFetchGarbageSidecarReturnsNilMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	// Overwrite the sidecar with non-hex garbage of the right length.
	garbage := strings.Repeat("z", 64)
	sidecarPath := filepath.Join(dir, testArtifactKey) + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(garbage), helpers.FileMod); err != nil {
		t.Fatalf("corrupt sidecar: %v", err)
	}

	fetched, err := a.Fetch(context.Background(), testArtifactKey)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if fetched.Meta != nil {
		t.Fatalf("expected nil Meta for a garbage sidecar, got %#v", fetched.Meta)
	}
}

func TestArtifactsDeleteRemovesTarballAndSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), map[string]string{"sha256": testSHA})

	tarballPath := filepath.Join(dir, testArtifactKey)
	sidecarPath := tarballPath + helpers.ArtifactSHASidecarSuffix
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("expected sidecar to exist before Delete, stat error: %v", err)
	}

	if err := a.Delete(context.Background(), testArtifactKey); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	if _, err := os.Stat(tarballPath); !os.IsNotExist(err) {
		t.Fatalf("expected tarball to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("expected sidecar to be removed, stat error: %v", err)
	}
}

// TestArtifactsDeleteToleratesMissingSidecar confirms Delete succeeds when
// only the tarball exists (e.g. it was committed with no sha256 in meta),
// so the absence of a sidecar is never treated as an error.
func TestArtifactsDeleteToleratesMissingSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := NewArtifacts(dir)
	commitTempArtifact(t, a, []byte("tarball bytes"), nil)

	if err := a.Delete(context.Background(), testArtifactKey); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
}

// The digest-shape predicate itself (valid hex, wrong length, uppercase,
// non-hex characters) moved to helpers.IsSHA256Hex and is exercised by
// helpers.TestIsSHA256Hex; this package now only calls it, so it no longer
// needs its own copy of that table.
