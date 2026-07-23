package extracted

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestStoreEnsureExtractsOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":     "hello",
		"sub/bar.txt": "world",
	})

	store := NewStore(filepath.Join(dir, "cache"))
	sha := mustHash(t, tarPath)

	got, err := store.Ensure(sha, tarPath)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !filepath.IsAbs(got) && filepath.Base(filepath.Dir(got)) != RootDirName {
		t.Fatalf("unexpected root: %s", got)
	}

	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(got, "foo.txt"))
	if err != nil {
		t.Fatalf("read foo.txt: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("foo.txt = %q", body)
	}
	if _, err := os.Stat(filepath.Join(got, ReadyMarker)); err != nil {
		t.Fatalf("ready marker missing: %v", err)
	}

	// Calling again must be idempotent and return the same path without
	// re-extracting (we delete the tarball to prove it).
	if err := os.Remove(tarPath); err != nil {
		t.Fatalf("remove tar: %v", err)
	}
	got2, err := store.Ensure(sha, tarPath)
	if err != nil {
		t.Fatalf("Ensure idempotent: %v", err)
	}
	if got2 != got {
		t.Fatalf("path mismatch: %q vs %q", got, got2)
	}
}

func TestStoreEnsureConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"file": "data"})
	store := NewStore(filepath.Join(dir, "cache"))

	sha := mustHash(t, tarPath)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := store.Ensure(sha, tarPath)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Ensure: %v", err)
		}
	}
}

func TestMaterializeUsesHardlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":     "hello",
		"sub/bar.txt": "world",
	})
	store := NewStore(filepath.Join(dir, "cache"))
	src, err := store.Ensure(mustHash(t, tarPath), tarPath)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dst := filepath.Join(dir, "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(dst, "sub", "bar.txt"))
	if err != nil {
		t.Fatalf("read materialized: %v", err)
	}
	if string(body) != "world" {
		t.Fatalf("sub/bar.txt = %q", body)
	}
	if _, err := os.Stat(filepath.Join(dst, ReadyMarker)); !os.IsNotExist(err) {
		t.Fatalf("ReadyMarker should not be materialized: err=%v", err)
	}

	srcStat, err := os.Stat(filepath.Join(src, "foo.txt"))
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstStat, err := os.Stat(filepath.Join(dst, "foo.txt"))
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !os.SameFile(srcStat, dstStat) {
		t.Fatalf("expected hard-linked file (same inode)")
	}
}

// TestEnsureRejectsTarballNotMatchingSHA proves Ensure refuses to ingest a
// tarball into the CAS tree under a sha its bytes do not actually hash to,
// leaving neither a finalized entry nor a leftover tmp directory behind.
func TestEnsureRejectsTarballNotMatchingSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": "hello"})
	store := NewStore(filepath.Join(dir, "cache"))

	// A well-formed but wrong sha256: valid-length (64) hex that is not
	// tarPath's actual hash.
	const wrongSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	if _, err := store.Ensure(wrongSHA, tarPath); !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	if _, err := os.Stat(filepath.Join(store.Root(), wrongSHA)); !os.IsNotExist(err) {
		t.Fatalf("expected no CAS tree for the rejected sha, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), wrongSHA+tmpSuffix)); !os.IsNotExist(err) {
		t.Fatalf("expected no leftover tmp dir for the rejected sha, stat err=%v", err)
	}
}

// TestEnsureAcceptsMatchingSHA proves Ensure succeeds and finalizes the CAS
// tree when the supplied sha does match the tarball's actual bytes.
func TestEnsureAcceptsMatchingSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": "hello"})
	store := NewStore(filepath.Join(dir, "cache"))

	sha := mustHash(t, tarPath)
	got, err := store.Ensure(sha, tarPath)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got, ReadyMarker)); err != nil {
		t.Fatalf("ready marker missing: %v", err)
	}
}

func TestStoreSweep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	seedFinalizedEntry(t, store, "keep")
	seedFinalizedEntry(t, store, "drop")

	if err := store.Sweep(map[string]bool{"keep": true}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "keep")); err != nil {
		t.Fatalf("keep was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "drop")); !os.IsNotExist(err) {
		t.Fatalf("drop survived sweep: err=%v", err)
	}
}

// TestStoreSweepPlan proves SweepPlan reports the same set Sweep would
// remove, sorted for deterministic output, without touching the filesystem.
func TestStoreSweepPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	seedFinalizedEntry(t, store, "keep")
	seedFinalizedEntry(t, store, "drop-b")
	seedFinalizedEntry(t, store, "drop-a")

	planned, err := store.SweepPlan(map[string]bool{"keep": true})
	if err != nil {
		t.Fatalf("SweepPlan: %v", err)
	}
	want := []string{"drop-a", "drop-b"}
	if len(planned) != len(want) || planned[0] != want[0] || planned[1] != want[1] {
		t.Fatalf("SweepPlan = %v, want %v", planned, want)
	}

	// SweepPlan must not mutate anything: every entry, including the ones
	// it planned to drop, is still present on disk afterward.
	for _, name := range []string{"keep", "drop-a", "drop-b"} {
		if _, statErr := os.Stat(filepath.Join(store.Root(), name)); statErr != nil {
			t.Fatalf("SweepPlan removed %s from disk: %v", name, statErr)
		}
	}
}

// TestStoreSweepPlanMissingRoot proves SweepPlan on a store whose root
// directory does not exist yet returns (nil, nil) rather than an error,
// matching Sweep's own behavior on a missing root.
func TestStoreSweepPlanMissingRoot(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	planned, err := store.SweepPlan(map[string]bool{"keep": true})
	if err != nil {
		t.Fatalf("SweepPlan: %v", err)
	}
	if planned != nil {
		t.Fatalf("expected nil plan for a missing root, got %v", planned)
	}
}

// TestStoreSweepPlanPropagatesRealReadDirError proves SweepPlan surfaces a
// genuine (non-not-exist) ReadDir error - e.g. a permission error - rather
// than treating it the same as a missing root. Skipped when running as
// root, since root bypasses the permission bits this test relies on to
// force the read failure.
func TestStoreSweepPlanPropagatesRealReadDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))
	if err := os.MkdirAll(store.Root(), helpers.DirMod); err != nil {
		t.Fatalf("failed to create store root: %v", err)
	}
	if err := os.Chmod(store.Root(), 0o000); err != nil {
		t.Fatalf("failed to chmod store root unreadable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(store.Root(), helpers.DirMod); err != nil {
			t.Errorf("failed to restore store root perms: %v", err)
		}
	})

	if _, err := store.SweepPlan(map[string]bool{}); err == nil {
		t.Fatalf("expected a non-nil error reading an unreadable store root")
	}
}

// TestStoreSweepTempRemovesOrphanTempsKeepsFinalized proves SweepTemp removes
// both temp forms the store creates under its root - an "ingest-" directory
// from IngestReader and a "<sha>.tmp" directory from Ensure/extractInto -
// while leaving a finalized CAS tree (a bare sha directory with its .ready
// marker) untouched.
func TestStoreSweepTempRemovesOrphanTempsKeepsFinalized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	finalized := seedFinalizedEntry(t, store, "deadbeef")

	ingestDir := filepath.Join(store.Root(), "ingest-orphan")
	seedTempEntry(t, ingestDir)

	extractTmpDir := filepath.Join(store.Root(), "cafebabe"+tmpSuffix)
	seedTempEntry(t, extractTmpDir)

	if err := store.SweepTemp(); err != nil {
		t.Fatalf("SweepTemp: %v", err)
	}

	if _, err := os.Stat(ingestDir); !os.IsNotExist(err) {
		t.Fatalf("ingest-* dir survived SweepTemp: err=%v", err)
	}
	if _, err := os.Stat(extractTmpDir); !os.IsNotExist(err) {
		t.Fatalf("<sha>.tmp dir survived SweepTemp: err=%v", err)
	}
	if _, err := os.Stat(finalized); err != nil {
		t.Fatalf("finalized entry removed by SweepTemp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(finalized, ReadyMarker)); err != nil {
		t.Fatalf("finalized ready marker removed by SweepTemp: %v", err)
	}
}

// mustHash returns the sha256 hex digest of the file at path, failing the
// test on error. Used to derive the real hash of a fixture tarball rather
// than hardcoding a value that Ensure's poisoning guard would now reject.
func mustHash(t *testing.T, path string) string {
	t.Helper()
	sha, err := archive.FileHashSHA256(path)
	if err != nil {
		t.Fatalf("FileHashSHA256(%s): %v", path, err)
	}
	return sha
}

// seedFinalizedEntry creates a finalized CAS tree directly under store's
// root - a bare directory named name carrying only the ReadyMarker - without
// going through Ensure. This is used by tests (Sweep/SweepTemp) that only
// need a finalized entry addressable by a readable, arbitrary name, decoupling
// them from Ensure's sha-verification requirement.
func seedFinalizedEntry(t *testing.T, store *Store, name string) string {
	t.Helper()
	dir := filepath.Join(store.Root(), name)
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ReadyMarker), []byte("ok"), helpers.FileMod); err != nil {
		t.Fatalf("write ready marker in %s: %v", dir, err)
	}
	return dir
}

// seedTempEntry creates a directory at path containing a single file,
// simulating a leftover temp entry left under a store root by a killed run.
func seedTempEntry(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "partial"), []byte("x"), helpers.FileMod); err != nil {
		t.Fatalf("write file in %s: %v", path, err)
	}
}

// TestStoreSweepTempMissingRootAndNilReceiver proves SweepTemp treats a
// not-yet-created store root as nothing to sweep, and is safe to call on a
// nil *Store (the "caching disabled" case), both returning a nil error.
func TestStoreSweepTempMissingRootAndNilReceiver(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	if err := store.SweepTemp(); err != nil {
		t.Fatalf("expected nil error for a missing store root, got %v", err)
	}

	var nilStore *Store
	if err := nilStore.SweepTemp(); err != nil {
		t.Fatalf("expected nil error for a nil store, got %v", err)
	}
}

func TestNewStoreEmptyCacheDir(t *testing.T) {
	t.Parallel()
	if got := NewStore(""); got != nil {
		t.Fatalf("expected nil store for empty cacheDir, got %v", got)
	}
}

func TestIngestAndPromote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"a.txt":   "alpha",
		"b/c.txt": "charlie",
	})
	store := NewStore(filepath.Join(dir, "cache"))

	final := ingestAndPromote(t, store, tarPath, "deadbeef")
	verifyPromoted(t, final)

	// Promote again from a second ingest of the same SHA must be a no-op
	// (existing finalized entry wins, tmp removed).
	final2 := ingestAndPromote(t, store, tarPath, "deadbeef")
	if final2 != final {
		t.Fatalf("expected same final path, got %q vs %q", final, final2)
	}
}

func ingestAndPromote(t *testing.T, store *Store, tarPath, sha string) string {
	t.Helper()
	//nolint:gosec // path under t.TempDir().
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	tmp, err := store.IngestReader(f)
	if err != nil {
		t.Fatalf("IngestReader: %v", err)
	}
	final, err := store.Promote(tmp, sha)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	return final
}

func verifyPromoted(t *testing.T, final string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(final, ReadyMarker)); err != nil {
		t.Fatalf("ReadyMarker missing: %v", err)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(final, "b", "c.txt"))
	if err != nil {
		t.Fatalf("read b/c.txt: %v", err)
	}
	if string(body) != "charlie" {
		t.Fatalf("b/c.txt = %q", body)
	}
}

func writeTarball(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write tar: %v", err)
	}
}
