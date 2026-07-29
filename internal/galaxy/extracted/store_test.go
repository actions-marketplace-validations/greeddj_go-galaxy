package extracted

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// helloFileContent is the shared "foo.txt" fixture body used across most
// tests in this file, factored into one constant so its many uses (as a
// writeTarball map value, an expected read-back body, and a legacy-file
// seed) do not read as coincidentally identical literals.
const helloFileContent = "hello"

func TestStoreEnsureExtractsOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":     helloFileContent,
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
	if string(body) != helloFileContent {
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
		"foo.txt":     helloFileContent,
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
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
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
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
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
	if err := os.WriteFile(filepath.Join(dir, ReadyMarker), []byte(ReadyMarkerPayload), helpers.FileMod); err != nil {
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

// seedCASTree extracts a small tarball into a fresh store via Ensure and
// returns (store, casTree). It is the common fixture for every read-only /
// removal test below, none of which care about the tarball's own content
// beyond it being a real, hashable file.
func seedCASTree(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":     helloFileContent,
		"sub/bar.txt": "world",
	})
	store := NewStore(filepath.Join(dir, "cache"))
	got, err := store.Ensure(mustHash(t, tarPath), tarPath)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return store, got
}

// casEntryInfo resolves path's fs.FileInfo and its root-relative name,
// factored out of checkCASEntryMode purely to keep each function's
// cyclomatic complexity under the linter's budget.
func casEntryInfo(root, path string, d fs.DirEntry) (string, fs.FileInfo, error) {
	info, err := d.Info()
	if err != nil {
		return "", nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", nil, err
	}
	return rel, info, nil
}

// assertCASEntryMode applies the three-way permission contract from the
// table in this commit's own documentation: the .ready marker is
// helpers.FileMod, a directory is helpers.DirMod, and anything else (a
// regular file, the only other kind a collection tarball produces) must
// carry no write bit.
func assertCASEntryMode(t *testing.T, rel string, info fs.FileInfo, isDir bool) {
	t.Helper()
	switch {
	case rel == ReadyMarker:
		if info.Mode().Perm() != helpers.FileMod {
			t.Errorf(".ready mode = %o, want %o", info.Mode().Perm(), helpers.FileMod)
		}
	case isDir:
		if info.Mode().Perm() != helpers.DirMod {
			t.Errorf("directory %s mode = %o, want %o", rel, info.Mode().Perm(), helpers.DirMod)
		}
	default:
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("file %s carries a write bit: mode %o", rel, info.Mode().Perm())
		}
	}
}

// checkCASEntryMode is TestCASTreeIsReadOnly's per-entry WalkDir callback,
// split out as its own function so the test itself stays a simple two-line
// call: golangci-lint's cyclomatic-complexity budget counts a WalkDir
// closure's branches against its enclosing test function, and this entry's
// three-way mode check is exactly the branching that budget exists to flag.
func checkCASEntryMode(t *testing.T, root, path string, d fs.DirEntry, walkErr error) error {
	t.Helper()
	if walkErr != nil {
		return walkErr
	}
	if path == root {
		return nil
	}
	rel, info, err := casEntryInfo(root, path, d)
	if err != nil {
		return err
	}
	assertCASEntryMode(t, rel, info, d.IsDir())
	return nil
}

// TestCASTreeIsReadOnly proves Ensure's extraction result satisfies the
// permission contract everywhere at once: every regular file has no write
// bit, every directory (including the root) keeps helpers.DirMod, and the
// .ready marker itself is at helpers.FileMod (it is go-galaxy's own sidecar,
// not artifact content, so it is deliberately not hardened).
func TestCASTreeIsReadOnly(t *testing.T) {
	t.Parallel()
	_, got := seedCASTree(t)

	err := filepath.WalkDir(got, func(path string, d fs.DirEntry, walkErr error) error {
		return checkCASEntryMode(t, got, path, d, walkErr)
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestInstalledFilesAreReadOnly proves Materialize preserves the CAS tree's
// read-only regular-file mode exactly (not merely "still read-only", but
// bit-for-bit identical to the source), that every linked file really is
// the same inode as its CAS source, and that install directories stay
// writable even though their file contents do not.
func TestInstalledFilesAreReadOnly(t *testing.T) {
	t.Parallel()
	_, src := seedCASTree(t)
	dst := filepath.Join(t.TempDir(), "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	for _, rel := range []string{"foo.txt", filepath.Join("sub", "bar.txt")} {
		srcInfo, err := os.Stat(filepath.Join(src, rel))
		if err != nil {
			t.Fatalf("stat src %s: %v", rel, err)
		}
		dstInfo, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("stat dst %s: %v", rel, err)
		}
		if dstInfo.Mode().Perm()&0o222 != 0 {
			t.Fatalf("installed %s carries a write bit: mode %o", rel, dstInfo.Mode().Perm())
		}
		if dstInfo.Mode().Perm() != srcInfo.Mode().Perm() {
			t.Fatalf("%s: installed mode %o != CAS mode %o", rel, dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
		}
		if !os.SameFile(srcInfo, dstInfo) {
			t.Fatalf("expected %s to be hard-linked to its CAS source (same inode)", rel)
		}
	}

	subInfo, err := os.Stat(filepath.Join(dst, "sub"))
	if err != nil {
		t.Fatalf("stat installed sub dir: %v", err)
	}
	if subInfo.Mode().Perm() != helpers.DirMod {
		t.Fatalf("installed directory mode = %o, want %o", subInfo.Mode().Perm(), helpers.DirMod)
	}
}

// TestInPlaceEditIsBlocked is the load-bearing regression test for this
// commit: opening an installed, hard-linked file for writing must fail with
// a permission error, and the CAS content it aliases must be unaffected by
// the attempt. Skipped under root, which bypasses every permission bit this
// test relies on to force the write to fail - detection (the tree-tally
// check from the previous commit) is what still holds in that case, not
// these mode bits.
func TestInPlaceEditIsBlocked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not block writes")
	}
	t.Parallel()

	_, src := seedCASTree(t)
	dst := filepath.Join(t.TempDir(), "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	installed := filepath.Join(dst, "foo.txt")

	//nolint:gosec // installed is derived from a t.TempDir() fixture, not external input; this is the exact write attempt under test.
	f, err := os.OpenFile(installed, os.O_WRONLY, 0)
	if err == nil {
		_ = f.Close()
		t.Fatalf("expected opening the installed file for writing to fail")
	}
	if !os.IsPermission(err) {
		t.Fatalf("expected a permission error, got %v", err)
	}

	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(src, "foo.txt"))
	if err != nil {
		t.Fatalf("read CAS file: %v", err)
	}
	if string(body) != helloFileContent {
		t.Fatalf("CAS file bytes changed by the blocked write attempt: %q", body)
	}
}

// TestReplaceByRenameDoesNotCorruptCAS pins the intentional scope of this
// commit's protection: it blocks a write *through* the shared inode, not a
// project replacing its own installed file wholesale. Renaming a new file
// over the installed path only needs write permission on the containing
// directory (which stays writable by design), so it succeeds and simply
// detaches the install from the CAS inode - the CAS content itself, still
// referenced under its own sha, is untouched.
func TestReplaceByRenameDoesNotCorruptCAS(t *testing.T) {
	t.Parallel()

	_, src := seedCASTree(t)
	dst := filepath.Join(t.TempDir(), "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	installed := filepath.Join(dst, "foo.txt")

	replacement := filepath.Join(t.TempDir(), "replacement.txt")
	if err := os.WriteFile(replacement, []byte("evil"), helpers.FileMod); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, installed); err != nil {
		t.Fatalf("rename over installed file: %v", err)
	}

	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(src, "foo.txt"))
	if err != nil {
		t.Fatalf("read CAS file: %v", err)
	}
	if string(body) != helloFileContent {
		t.Fatalf("CAS file corrupted by a rename over the installed copy: got %q", body)
	}
}

// assertCopyFileCreatesIndependentCopy asserts copyFile(src, dst, perm)
// succeeds, dst's content matches want, dst's mode is exactly perm (not
// merely read-only), and dst is a distinct inode from src rather than a
// hard link.
func assertCopyFileCreatesIndependentCopy(t *testing.T, dst, src, want string, perm os.FileMode) {
	t.Helper()
	if err := copyFile(src, dst, perm); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(body) != want {
		t.Fatalf("dst content = %q, want %q", body, want)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if dstInfo.Mode().Perm() != perm {
		t.Fatalf("dst mode = %o, want %o", dstInfo.Mode().Perm(), perm)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	if os.SameFile(srcInfo, dstInfo) {
		t.Fatalf("expected copyFile to create a distinct inode, not a hard link")
	}
}

// assertMaterializeFileSurfacesRemoveError drives materializeFile's
// checked-remove error path by pre-creating dst as a non-empty directory:
// os.Remove fails on a non-empty directory (unlike an empty one, which it
// would happily remove), so this deterministically forces the checked
// remove to surface a real error instead of silently falling through to a
// confusing EEXIST/EISDIR failure further down.
func assertMaterializeFileSurfacesRemoveError(t *testing.T, dir, src string, perm os.FileMode) {
	t.Helper()
	occupiedDst := filepath.Join(dir, "occupied-dst")
	if err := os.MkdirAll(filepath.Join(occupiedDst, "occupant"), helpers.DirMod); err != nil {
		t.Fatalf("mkdir non-empty occupied dst: %v", err)
	}
	err := materializeFile(src, occupiedDst, perm)
	if err == nil {
		t.Fatalf("expected materializeFile to fail against a non-empty directory at dst")
	}
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("expected the checked remove's own ENOTEMPTY error, got: %v", err)
	}
}

// TestMaterializeCopyFallback exercises copyFile's cross-device fallback
// branch directly, without needing a second filesystem, then drives
// materializeFile's checked-remove error path. See
// assertCopyFileCreatesIndependentCopy and
// assertMaterializeFileSurfacesRemoveError for what each half proves.
func TestMaterializeCopyFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const casPerm = os.FileMode(0o444)
	const payload = "payload"
	src := filepath.Join(dir, "src.txt")
	//nolint:gosec // test fixture; the permissive create mode is immediately narrowed by the Chmod below.
	if err := os.WriteFile(src, []byte(payload), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.Chmod(src, casPerm); err != nil {
		t.Fatalf("chmod src to CAS mode: %v", err)
	}

	assertCopyFileCreatesIndependentCopy(t, filepath.Join(dir, "dst.txt"), src, payload, casPerm)
	assertMaterializeFileSurfacesRemoveError(t, dir, src, casPerm)
}

// TestLegacyReadyMarkerForcesRebuild proves a CAS tree left over from a
// pre-hardening binary - carrying the legacy "ok" marker payload and a
// writable regular file - is never trusted: Ensure must reject its stale
// marker, rebuild the tree via extractInto's RemoveAll-then-rebuild path,
// and the rebuilt tree must be hardened (and carry the current marker
// payload) exactly like a first-time extraction would.
func TestLegacyReadyMarkerForcesRebuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	sha := mustHash(t, tarPath)

	store := NewStore(filepath.Join(dir, "cache"))
	final := filepath.Join(store.Root(), sha)
	if err := os.MkdirAll(final, helpers.DirMod); err != nil {
		t.Fatalf("mkdir legacy final: %v", err)
	}
	//nolint:gosec // test fixture: the legacy tree's writable file is the exact condition under test, not a leak risk.
	if err := os.WriteFile(filepath.Join(final, "foo.txt"), []byte(helloFileContent), 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(final, ReadyMarker), []byte("ok"), helpers.FileMod); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}

	got, err := store.Ensure(sha, tarPath)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	info, err := os.Stat(filepath.Join(got, "foo.txt"))
	if err != nil {
		t.Fatalf("stat rebuilt file: %v", err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("rebuilt file mode = %o, want 0444 (the legacy tree must be rebuilt hardened)", info.Mode().Perm())
	}
	//nolint:gosec // path under t.TempDir().
	marker, err := os.ReadFile(filepath.Join(got, ReadyMarker))
	if err != nil {
		t.Fatalf("read ready marker: %v", err)
	}
	if string(marker) != ReadyMarkerPayload {
		t.Fatalf("ready marker = %q, want %q", marker, ReadyMarkerPayload)
	}
}

// TestStoreReady proves the pure read-only predicate Ready mirrors Ensure's
// own isReady short-circuit exactly: false before a tree is ever extracted,
// false for an empty sha, true once the tree is genuinely promoted, and false
// again for a tree whose ready marker carries the legacy "ok" payload - the
// same pre-hardening shape TestLegacyReadyMarkerForcesRebuild proves Ensure
// rejects and rebuilds rather than trusts. A bare os.Stat of the marker would
// report true for that legacy case, so this pins Ready against exactly the
// regression its own doc comment warns about.
func TestStoreReady(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	sha := mustHash(t, tarPath)

	store := NewStore(filepath.Join(dir, "cache"))

	if store.Ready(sha) {
		t.Error("expected Ready to report false before the tree is ever extracted")
	}
	if store.Ready("") {
		t.Error("expected Ready to report false for an empty sha")
	}

	if _, err := store.Ensure(sha, tarPath); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !store.Ready(sha) {
		t.Error("expected Ready to report true for a genuinely promoted tree")
	}

	// Overwrite the marker with the legacy pre-hardening payload - the exact
	// shape TestLegacyReadyMarkerForcesRebuild builds by hand - and prove Ready
	// rejects it just as Ensure's own isReady check would.
	markerPath := filepath.Join(store.Root(), sha, ReadyMarker)
	if err := os.WriteFile(markerPath, []byte("ok"), helpers.FileMod); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}
	if store.Ready(sha) {
		t.Error("expected Ready to report false for a tree carrying the legacy \"ok\" marker payload")
	}

	// Traversal guard: a sha crafted to escape the store root, naming a real
	// sibling directory that genuinely holds a valid-looking ready marker,
	// must still report false. storePath rejects any sha containing a path
	// separator before Ready ever calls isReady, so this never even joins
	// into a path that could reach the sibling.
	siblingDir := filepath.Join(store.Root(), "..", "sibling")
	if err := os.MkdirAll(siblingDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(siblingDir, ReadyMarker), []byte(ReadyMarkerPayload), helpers.FileMod); err != nil {
		t.Fatalf("write sibling ready marker: %v", err)
	}
	if store.Ready("../sibling") {
		t.Error("expected Ready to report false for a traversal sha, even one naming a directory with a valid ready marker")
	}
}

// TestEnsureAndPromoteRejectTraversalSHA proves both of the store's
// tree-creating entry points refuse an unsafe sha - one that is not a single
// path element - rather than joining it into a filepath.Join/os.Rename target
// that could escape the store root. Neither call may create or remove
// anything outside store.Root().
func TestEnsureAndPromoteRejectTraversalSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})

	store := NewStore(filepath.Join(dir, "cache"))
	const traversalSHA = "../../../../../../etc/ssh"

	if _, err := store.Ensure(traversalSHA, tarPath); !errors.Is(err, ErrSHAUnsafe) {
		t.Errorf("Ensure(%q, ...) error = %v, want ErrSHAUnsafe", traversalSHA, err)
	}
	// The rejection happens before Ensure ever reaches extractInto's own
	// os.MkdirAll(s.root, ...), so the store root itself must not exist yet -
	// the strongest available proof that nothing was created anywhere, inside
	// the root or out.
	if _, err := os.Stat(store.Root()); !os.IsNotExist(err) {
		t.Errorf("expected Ensure to create nothing at all for a rejected sha, store.Root() stat error = %v", err)
	}

	tmpRoot := t.TempDir()
	if _, err := store.Promote(tmpRoot, traversalSHA); !errors.Is(err, ErrSHAUnsafe) {
		t.Errorf("Promote(_, %q) error = %v, want ErrSHAUnsafe", traversalSHA, err)
	}
	if _, err := os.Stat(store.Root()); !os.IsNotExist(err) {
		t.Errorf("expected Promote to create nothing at all for a rejected sha, store.Root() stat error = %v", err)
	}
	if _, err := os.Stat(tmpRoot); !os.IsNotExist(err) {
		t.Errorf("expected Promote to still remove tmpRoot on rejection, stat error = %v", err)
	}
}

// TestRemoveRefusesTraversalSHA proves Remove leaves a traversal target
// intact and returns nil (its normal best-effort "nothing to do" result,
// matching an empty sha) rather than removing whatever path an unsafe sha
// would otherwise resolve to outside the store root.
func TestRemoveRefusesTraversalSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	target := filepath.Join(dir, "sibling-victim")
	if err := os.MkdirAll(target, helpers.DirMod); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	marker := filepath.Join(target, "keepme")
	if err := os.WriteFile(marker, []byte("keep"), helpers.FileMod); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	if err := store.Remove("../sibling-victim"); err != nil {
		t.Errorf("Remove: %v, want nil", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("expected the traversal target to survive Remove, stat error: %v", err)
	}
}

// TestTarballShippingReadyMarkerPromotes is the regression test for the bug
// hardening introduces: a tarball that ships its own root-level ".ready"
// entry now extracts it read-only along with everything else, so a naive
// os.WriteFile of the real marker would fail EACCES trying to truncate it
// in place. Promote's remove-then-write must tolerate this and finish with
// the current payload, not the tarball's own content.
func TestTarballShippingReadyMarkerPromotes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":   helloFileContent,
		ReadyMarker: "not-ready-yet",
	})
	store := NewStore(filepath.Join(dir, "cache"))

	//nolint:gosec // path under t.TempDir().
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tarball: %v", err)
	}
	defer func() { _ = f.Close() }()
	tmp, err := store.IngestReader(f)
	if err != nil {
		t.Fatalf("IngestReader: %v", err)
	}
	sha := mustHash(t, tarPath)
	final, err := store.Promote(tmp, sha)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	//nolint:gosec // path under t.TempDir().
	content, err := os.ReadFile(filepath.Join(final, ReadyMarker))
	if err != nil {
		t.Fatalf("read ready marker: %v", err)
	}
	if string(content) != ReadyMarkerPayload {
		t.Fatalf("ready marker = %q, want %q (the tarball's own .ready entry must be overwritten)", content, ReadyMarkerPayload)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(final, "foo.txt"))
	if err != nil {
		t.Fatalf("read foo.txt: %v", err)
	}
	if string(body) != helloFileContent {
		t.Fatalf("foo.txt = %q", body)
	}
}

// TestRemovalPathsWithReadOnlyFiles proves every removal path this
// package's callers rely on still succeeds against a tree of read-only
// regular files: Store.Remove, Sweep, and SweepTemp for CAS trees, plus a
// bare os.RemoveAll over a Materialize'd install tree - the exact pattern
// collections.extractCollection uses to wipe an install path before a fresh
// extraction. None of these need the target file's own write bit, only the
// containing directory's, so hardening the files must not affect any of
// them.
func TestRemovalPathsWithReadOnlyFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// remove performs the removal against target (a finalized CAS tree
		// for the Store-level cases, or a Materialize'd install tree for the
		// last one) and returns the path that must be gone afterward.
		remove func(t *testing.T, store *Store, target string) (removed string, err error)
		name   string
	}{
		{
			name: "Store.Remove",
			remove: func(t *testing.T, store *Store, target string) (string, error) {
				t.Helper()
				return target, store.Remove(filepath.Base(target))
			},
		},
		{
			name: "Sweep",
			remove: func(t *testing.T, store *Store, target string) (string, error) {
				t.Helper()
				return target, store.Sweep(map[string]bool{})
			},
		},
		{
			name: "SweepTemp",
			remove: func(t *testing.T, store *Store, target string) (string, error) {
				t.Helper()
				// SweepTemp deliberately never touches a finalized entry, so
				// reshape it into the "<sha>.tmp" form its matcher targets.
				tempTarget := target + tmpSuffix
				if err := os.Rename(target, tempTarget); err != nil {
					t.Fatalf("rename to temp shape: %v", err)
				}
				return tempTarget, store.SweepTemp()
			},
		},
		{
			name: "os.RemoveAll over a Materialize'd install tree",
			remove: func(t *testing.T, _ *Store, target string) (string, error) {
				t.Helper()
				installDir := filepath.Join(filepath.Dir(target), "install")
				if err := Materialize(target, installDir); err != nil {
					t.Fatalf("Materialize: %v", err)
				}
				return installDir, os.RemoveAll(installDir)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, target := seedCASTree(t)

			removed, err := tt.remove(t, store, target)
			if err != nil {
				t.Fatalf("removal failed against a hardened tree: %v", err)
			}
			if _, statErr := os.Stat(removed); !os.IsNotExist(statErr) {
				t.Fatalf("expected %s to be removed, stat error: %v", removed, statErr)
			}
		})
	}
}

// TestNewFilesCanStillBeCreatedInInstallTree encodes the __pycache__
// guarantee: hardening regular files must never extend to blocking new file
// or directory creation inside an install tree, since directories are never
// hardened and their write bit is exactly what os.Mkdir and os.WriteFile
// need. A future change that hardened directories too would break this
// test.
func TestNewFilesCanStillBeCreatedInInstallTree(t *testing.T) {
	t.Parallel()
	_, src := seedCASTree(t)
	dst := filepath.Join(t.TempDir(), "install")
	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	newDir := filepath.Join(dst, "__pycache__")
	if err := os.Mkdir(newDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir __pycache__ in hardened install tree: %v", err)
	}
	newFile := filepath.Join(newDir, "module.cpython-312.pyc")
	//nolint:gosec // test fixture; an ordinary writable mode is exactly what a real __pycache__ write would use.
	if err := os.WriteFile(newFile, []byte("compiled"), 0o644); err != nil {
		t.Fatalf("write new file in hardened install tree: %v", err)
	}
	//nolint:gosec // path under t.TempDir().
	body, err := os.ReadFile(newFile)
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if string(body) != "compiled" {
		t.Fatalf("new file content = %q", body)
	}
}
