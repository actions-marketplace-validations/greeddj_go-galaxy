package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestSanitizeArchivePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		wantErr error
		name    string
		input   string
		want    string
	}{
		{name: "empty", input: "", wantErr: helpers.ErrArchiveEntryHasEmptyName},
		{name: "abs", input: "/etc/passwd", wantErr: helpers.ErrArchiveEntryIsAbsolutePath},
		{name: "escape", input: "../evil", wantErr: helpers.ErrArchiveEntryEscapesDestination},
		{name: "dot", input: ".", want: ""},
		{name: "ok", input: "dir/file", want: filepath.FromSlash("dir/file")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sanitizeArchivePath(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

// testArchiveEntry describes one tar entry for buildTestArchive.
type testArchiveEntry struct {
	name     string
	linkname string
	content  []byte
	typeflag byte
}

// buildTestArchive renders entries, in order, into an in-memory tar.gz,
// mirroring how fakegalaxy builds artifacts elsewhere in this repo.
func buildTestArchive(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()

	// testEntryModTime stamps every entry so these tests never depend on
	// wall-clock time.
	testEntryModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		header := &tar.Header{
			Typeflag: e.typeflag,
			Name:     e.name,
			Linkname: e.linkname,
			Size:     int64(len(e.content)),
			Mode:     0o644,
			ModTime:  testEntryModTime,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write tar header for %s: %v", e.name, err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("failed to write tar content for %s: %v", e.name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractMemoizedDeepTreeExtractsCorrectly extracts many files that all
// share one deep parent chain - the shape ensureNoSymlinkParents' memo
// targets - and asserts every file lands at the right path with the right
// content. The memo must be purely an optimization: it changes nothing
// about what gets extracted.
func TestExtractMemoizedDeepTreeExtractsCorrectly(t *testing.T) {
	t.Parallel()

	const (
		depth = 6
		files = 50
	)
	dirParts := make([]string, depth)
	for i := range depth {
		dirParts[i] = fmt.Sprintf("d%d", i)
	}
	prefix := strings.Join(dirParts, "/")

	entries := make([]testArchiveEntry, files)
	for i := range files {
		entries[i] = testArchiveEntry{
			typeflag: tar.TypeReg,
			name:     fmt.Sprintf("%s/f%d.txt", prefix, i),
			content:  fmt.Appendf(nil, "content-%d", i),
		}
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, e := range entries {
		//nolint:gosec // dst is a t.TempDir() and e.name is a literal from the entries slice above, not external input.
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(e.name)))
		if err != nil {
			t.Fatalf("failed to read extracted file %s: %v", e.name, err)
		}
		if string(got) != string(e.content) {
			t.Fatalf("file %s: expected content %q, got %q", e.name, e.content, got)
		}
	}
}

// TestExtractSymlinkParentRejectedDespiteMemoizedAncestors is the
// load-bearing security test for the memo. p/m/sub/keep.txt creates p/m/sub;
// p/m/other.txt is a second entry under p/m, which is what actually Lstats
// and memoizes p and m as confirmed directories. p/m/link is then a
// symlink (confined to sub, so safeSymlinkTarget accepts it and it gets
// created). p/m/link/escape.txt must still be rejected: the memo skips
// re-Lstatting p and m, but link itself was never memoized (only real
// directories are), so it is freshly Lstat'd and correctly found to be a
// symlink.
func TestExtractSymlinkParentRejectedDespiteMemoizedAncestors(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "p/m/sub/keep.txt", content: []byte("keep")},
		{typeflag: tar.TypeReg, name: "p/m/other.txt", content: []byte("other")},
		{typeflag: tar.TypeSymlink, name: "p/m/link", linkname: "sub"},
		{typeflag: tar.TypeReg, name: "p/m/link/escape.txt", content: []byte("escape")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst)
	if err == nil {
		t.Fatalf("expected extraction to fail")
	}
	if !errors.Is(err, helpers.ErrArchivePathContainsSymlinkComponent) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchivePathContainsSymlinkComponent, err)
	}

	// The symlink itself must have been created (confirming the memo
	// really did let entry 3 through before entry 4 was rejected).
	linkInfo, statErr := os.Lstat(filepath.Join(dst, "p", "m", "link"))
	if statErr != nil {
		t.Fatalf("expected symlink p/m/link to exist: %v", statErr)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected p/m/link to be a symlink")
	}

	// No file must have been written under the symlink's resolved target.
	if _, statErr := os.Lstat(filepath.Join(dst, "p", "m", "sub", "escape.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected escape.txt to not exist under the symlink target, stat error: %v", statErr)
	}
}

// TestExtractSymlinkCannotReplaceMemoizedDir extracts two files under p/m
// (which memoizes p and m as confirmed directories), then a symlink entry
// named exactly p/m. The extraction must fail because os.Symlink returns
// EEXIST for an existing directory target - a defense that lives in the
// extractor's no-overwrite behavior, not in ensureNoSymlinkParents, so this
// outcome is identical whether or not p and m happen to be memoized.
func TestExtractSymlinkCannotReplaceMemoizedDir(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "p/m/a.txt", content: []byte("a")},
		{typeflag: tar.TypeReg, name: "p/m/b.txt", content: []byte("b")},
		{typeflag: tar.TypeSymlink, name: "p/m", linkname: "a.txt"},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst)
	if err == nil {
		t.Fatalf("expected extraction to fail")
	}

	info, statErr := os.Lstat(filepath.Join(dst, "p", "m"))
	if statErr != nil {
		t.Fatalf("expected p/m to still exist: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("expected p/m to still be a directory, got mode %v", info.Mode())
	}
}

// TestExtractHardlinkWithMemoizedParentChain smoke-tests that the memo
// threads correctly into extractHardlink: p/q/a.txt and p/q/target.txt
// memoize p and q, then p/q/link.txt hardlinks to p/q/target.txt, whose own
// parent-chain check (inside extractHardlink) must reuse that memo and
// still extract correctly.
func TestExtractHardlinkWithMemoizedParentChain(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "p/q/a.txt", content: []byte("a")},
		{typeflag: tar.TypeReg, name: "p/q/target.txt", content: []byte("hello")},
		{typeflag: tar.TypeLink, name: "p/q/link.txt", linkname: "p/q/target.txt"},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err := os.ReadFile(filepath.Join(dst, "p", "q", "link.txt"))
	if err != nil {
		t.Fatalf("failed to read linked file: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("expected linked content %q, got %q", "hello", got)
	}
}

// buildEntriesArchive builds n flat tar entries of the given typeflag, named
// entry0, entry1, ..., entry<n-1>. tar.TypeDir entries get a trailing slash
// and zero size, per tar convention. This drives extractTarEntries' entry-
// count cap directly, with a tiny injected cap, instead of needing a real
// 100,001-entry archive to exercise the boundary.
func buildEntriesArchive(tb testing.TB, n int, typeflag byte) []byte {
	tb.Helper()

	// entriesModTime stamps every generated entry so these tests never
	// depend on wall-clock time.
	entriesModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte("x")
	for i := range n {
		name := fmt.Sprintf("entry%d", i)
		var size int64
		if typeflag == tar.TypeDir {
			name += "/"
		} else {
			size = int64(len(content))
		}
		header := &tar.Header{
			Typeflag: typeflag,
			Name:     name,
			Size:     size,
			Mode:     0o644,
			ModTime:  entriesModTime,
		}
		if err := tw.WriteHeader(header); err != nil {
			tb.Fatalf("failed to write tar header: %v", err)
		}
		if size > 0 {
			if _, err := tw.Write(content); err != nil {
				tb.Fatalf("failed to write tar content: %v", err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		tb.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		tb.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// tarReaderFrom decompresses gzip-encoded archive bytes into a *tar.Reader,
// for tests that call the unexported extractTarEntries directly with an
// injected cap rather than going through the public gzip-wrapping API.
func tarReaderFrom(t *testing.T, archiveBytes []byte) *tar.Reader {
	t.Helper()

	gzr, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	t.Cleanup(func() {
		_ = gzr.Close()
	})
	return tar.NewReader(gzr)
}

// TestExtractEntryCountUnderCap asserts that an archive with fewer entries
// than the injected cap extracts every entry normally.
func TestExtractEntryCountUnderCap(t *testing.T) {
	t.Parallel()

	const maxEntries = 5
	archiveBytes := buildEntriesArchive(t, 4, tar.TypeReg)

	dst := t.TempDir()
	if err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range 4 {
		name := fmt.Sprintf("entry%d", i)
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
}

// TestExtractEntryCountAtCap pins the boundary: exactly maxEntries entries
// must succeed, since the cap check is entries > maxEntries, not >=.
func TestExtractEntryCountAtCap(t *testing.T) {
	t.Parallel()

	const maxEntries = 5
	archiveBytes := buildEntriesArchive(t, maxEntries, tar.TypeReg)

	dst := t.TempDir()
	if err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range maxEntries {
		name := fmt.Sprintf("entry%d", i)
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
}

// TestExtractEntryCountOverCapFailsClosed asserts that one entry past the
// cap is rejected with ErrArchiveTooManyEntries, at the (maxEntries+1)th
// header.
func TestExtractEntryCountOverCapFailsClosed(t *testing.T) {
	t.Parallel()

	const maxEntries = 5
	archiveBytes := buildEntriesArchive(t, maxEntries+1, tar.TypeReg)

	dst := t.TempDir()
	err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries)
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveTooManyEntries, err)
	}
}

// TestExtractEntryCountCountsNonRegularEntries proves the entry-count cap
// counts every typeflag, not just regular files: four zero-byte directory
// entries against a cap of three must be rejected, even though a zero-byte
// directory never trips either byte cap in extractRegularFile. This is the
// defense against an empty-directory (or hardlink) inode-exhaustion
// tarbomb.
func TestExtractEntryCountCountsNonRegularEntries(t *testing.T) {
	t.Parallel()

	const maxEntries = 3
	archiveBytes := buildEntriesArchive(t, maxEntries+1, tar.TypeDir)

	dst := t.TempDir()
	err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries)
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveTooManyEntries, err)
	}
}

// TestExtractEntryCountCountsHardlinkEntries is the hardlink variant of the
// non-regular-entry coverage above: a regular target file plus hardlinks to
// it are each counted, so the fourth entry (also a hardlink) trips the cap
// before it is linked. The first three entries must extract successfully
// (proving the cap does not fire early), and only the fourth is rejected.
func TestExtractEntryCountCountsHardlinkEntries(t *testing.T) {
	t.Parallel()

	const maxEntries = 3
	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "target.txt", content: []byte("hello")},
		{typeflag: tar.TypeLink, name: "link1.txt", linkname: "target.txt"},
		{typeflag: tar.TypeLink, name: "link2.txt", linkname: "target.txt"},
		{typeflag: tar.TypeLink, name: "link3.txt", linkname: "target.txt"},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := extractTarEntries(tarReaderFrom(t, archiveBytes), dst, maxEntries)
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveTooManyEntries, err)
	}

	// The first three entries (the target file and two hardlinks) must have
	// been extracted before the fourth tripped the cap.
	if _, statErr := os.Stat(filepath.Join(dst, "target.txt")); statErr != nil {
		t.Fatalf("expected target.txt to exist: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "link1.txt")); statErr != nil {
		t.Fatalf("expected link1.txt to exist: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "link2.txt")); statErr != nil {
		t.Fatalf("expected link2.txt to exist: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "link3.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected link3.txt to not exist, stat error: %v", statErr)
	}
}

// TestExtractLegitMultiFileArchiveStillExtracts proves the cap causes no
// false positive on an ordinary collection-sized archive: a flat 50-file
// archive extracted through the public ExtractTarGzStream API, using the
// real ArchiveMaxEntryCount rather than an injected cap, must still succeed.
func TestExtractLegitMultiFileArchiveStillExtracts(t *testing.T) {
	t.Parallel()

	const files = 50
	archiveBytes := buildEntriesArchive(t, files, tar.TypeReg)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range files {
		name := fmt.Sprintf("entry%d", i)
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
}

// TestExtractOverlongPathComponentFailsClosed drives
// checkPathComponentNotSymlink's non-ErrNotExist Lstat error arm: a tar
// entry whose path has a component exceeding the filesystem's name limit
// makes os.Lstat fail with ENAMETOOLONG rather than os.ErrNotExist, so
// extraction must fail closed with a wrapped stat error instead of silently
// treating the component as absent and proceeding.
func TestExtractOverlongPathComponentFailsClosed(t *testing.T) {
	t.Parallel()

	// overlongComponent exceeds every common filesystem's per-component
	// name limit (typically 255 bytes on Linux and macOS), guaranteeing
	// os.Lstat rejects it with ENAMETOOLONG regardless of whether it exists.
	overlongComponent := strings.Repeat("a", 1000)
	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: overlongComponent + "/file.txt", content: []byte("x")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst)
	if err == nil {
		t.Fatalf("expected extraction to fail")
	}
	if errors.Is(err, helpers.ErrArchivePathContainsSymlinkComponent) {
		t.Fatalf("expected a stat failure, not the symlink-component rejection: %v", err)
	}
	if !strings.Contains(err.Error(), "failed to stat path") {
		t.Fatalf("expected a wrapped stat error, got: %v", err)
	}
}
