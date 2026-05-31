package extracted

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
	const sha = "abc123"

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

	const sha = "shared"
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
	src, err := store.Ensure("sha", tarPath)
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

func TestStoreSweep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"f": "x"})
	store := NewStore(filepath.Join(dir, "cache"))

	if _, err := store.Ensure("keep", tarPath); err != nil {
		t.Fatalf("Ensure keep: %v", err)
	}
	if _, err := store.Ensure("drop", tarPath); err != nil {
		t.Fatalf("Ensure drop: %v", err)
	}

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
