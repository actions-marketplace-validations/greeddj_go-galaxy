package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
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

// testArchiveEntry describes one tar entry for buildTestArchive. mode is the
// tar header's permission bits; zero means "use the package's own default
// of 0o644", preserving every existing caller that never set it.
type testArchiveEntry struct {
	name     string
	linkname string
	content  []byte
	typeflag byte
	mode     int64
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
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Typeflag: e.typeflag,
			Name:     e.name,
			Linkname: e.linkname,
			Size:     int64(len(e.content)),
			Mode:     mode,
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
// directory charges nothing against either byte cap. This is the defense
// against an empty-directory (or hardlink) inode-exhaustion tarbomb.
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

// assertExtractedFileModes asserts that each dst-relative path in want has
// exactly the given permission bits, via os.Lstat (never following a
// symlink, though none of these paths are expected to be one).
func assertExtractedFileModes(t *testing.T, dst string, want map[string]os.FileMode) {
	t.Helper()
	for name, wantMode := range want {
		info, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("lstat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s: mode = %o, want %o", name, got, wantMode)
		}
	}
}

// TestExtractStripsWriteBits proves extraction masks every write bit off a
// regular file at open time regardless of the tar header's own mode, leaves
// directories at helpers.DirMod, and never touches a symlink's own mode
// (which os.Chmod would resolve through to the symlink's target instead of
// the link itself).
func TestExtractStripsWriteBits(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "rw.txt", content: []byte("a"), mode: 0o644},
		{typeflag: tar.TypeReg, name: "rwx.txt", content: []byte("b"), mode: 0o755},
		{typeflag: tar.TypeReg, name: "owner-rw.txt", content: []byte("c"), mode: 0o600},
		{typeflag: tar.TypeReg, name: "already-ro.txt", content: []byte("d"), mode: 0o400},
		{typeflag: tar.TypeDir, name: "sub/", mode: 0o755},
		{typeflag: tar.TypeReg, name: "sub/target.txt", content: []byte("e"), mode: 0o644},
		{typeflag: tar.TypeSymlink, name: "link.txt", linkname: "sub/target.txt"},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertExtractedFileModes(t, dst, map[string]os.FileMode{
		"rw.txt":         0o444,
		"rwx.txt":        0o555,
		"owner-rw.txt":   0o400,
		"already-ro.txt": 0o400,
		"sub/target.txt": 0o444,
	})

	dirInfo, err := os.Lstat(filepath.Join(dst, "sub"))
	if err != nil {
		t.Fatalf("lstat sub: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != helpers.DirMod {
		t.Fatalf("directory mode = %o, want %o (directories must never be hardened)", got, helpers.DirMod)
	}

	// The symlink entry itself must never be chmod'ed: os.Chmod follows a
	// symlink and would silently mutate whatever it points at.
	linkInfo, err := os.Lstat(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatalf("lstat link.txt: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected link.txt to be a symlink")
	}
	targetInfo, err := os.Lstat(filepath.Join(dst, "sub", "target.txt"))
	if err != nil {
		t.Fatalf("lstat symlink target: %v", err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o444 {
		t.Fatalf("symlink target mode changed to %o, want unaffected 0444", got)
	}
}

// TestExtractRejectsDuplicateEntries proves a tarball with two regular-file
// entries at the same path fails with helpers.ErrArchiveDuplicateEntry
// naming the offending path, rather than the bare, undebuggable permission
// error the second entry's OpenFile now hits against the first entry's
// already-read-only file.
func TestExtractRejectsDuplicateEntries(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "dup.txt", content: []byte("first")},
		{typeflag: tar.TypeReg, name: "dup.txt", content: []byte("second")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst)
	if !errors.Is(err, helpers.ErrArchiveDuplicateEntry) {
		t.Fatalf("expected ErrArchiveDuplicateEntry, got %v", err)
	}
	if !strings.Contains(err.Error(), "dup.txt") {
		t.Fatalf("expected the error to name the offending path, got: %v", err)
	}
}

// TestExtractAcceptsDotSlashPrefixedNames guards against the write-bit and
// duplicate-entry changes above false-positiving on a perfectly ordinary
// tarball whose entries are named with a "./" prefix (a common tar output
// convention): sanitizeArchivePath already normalizes it away, and this
// pins that normal single-entry extraction still succeeds.
func TestExtractAcceptsDotSlashPrefixedNames(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: "./MANIFEST.json", content: []byte("{}")},
		{typeflag: tar.TypeReg, name: "./sub/file.txt", content: []byte("data")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err := os.ReadFile(filepath.Join(dst, "MANIFEST.json"))
	if err != nil {
		t.Fatalf("read MANIFEST.json: %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("MANIFEST.json = %q", got)
	}
	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err = os.ReadFile(filepath.Join(dst, "sub", "file.txt"))
	if err != nil {
		t.Fatalf("read sub/file.txt: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("sub/file.txt = %q", got)
	}
}

// buildHeaderOnlyArchive renders exactly one tar header, declaring size bytes,
// into an in-memory tar.gz and writes no body for it at all. A declared size
// that no body backs is what makes "charged before the typeflag is dispatched"
// observable: for a typeflag that owes a body, the charge is then the first
// thing that can fail, and the omitted body the second.
//
// tw.Close()'s error is deliberately ignored, and both of its outcomes here
// are expected. For a typeflag archive/tar does not treat as header-only,
// Close reports `archive/tar: missed writing N bytes` - precisely because this
// fixture declares a body it intentionally omits - and stops short of the two
// zero trailer blocks; the header itself is already in the stream, which is
// all these tests read. For a header-only typeflag the writer owes no body, so
// Close returns nil and writes the trailer normally. gz.Close() is checked as
// usual, since a gzip-layer failure would mean the fixture itself is broken
// rather than deliberately truncated.
func buildHeaderOnlyArchive(t *testing.T, typeflag byte, name string, size int64) []byte {
	t.Helper()

	// headerOnlyModTime stamps the entry so these tests never depend on
	// wall-clock time.
	headerOnlyModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	header := &tar.Header{
		Typeflag: typeflag,
		Name:     name,
		Size:     size,
		Mode:     0o644,
		ModTime:  headerOnlyModTime,
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("failed to write tar header for %s: %v", name, err)
	}
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractUnknownTypeflagEntryIsSizeCapped drives the byte budget through
// an entry the dispatch switch's default arm skips: '9' is not one of the four
// typeflags extraction handles, so nothing about this entry reaches the disk -
// yet its declared size is exactly what tar.Reader.Next has to read past to
// reach the following header. It must meet the same per-entry cap a regular
// file meets.
func TestExtractUnknownTypeflagEntryIsSizeCapped(t *testing.T) {
	t.Parallel()

	archiveBytes := buildHeaderOnlyArchive(t, '9', "bomb", helpers.ArchiveMaxEntrySize+1)

	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), t.TempDir())
	// Killing mutation: delete the chargeEntrySize call from
	// extractTarEntries, together with the `declared` half of that function's
	// `var declared, entries int64` - the call is its only use, so deleting the
	// call by itself does not compile ("declared and not used: declared").
	// This assertion then fails with
	//
	//	archive_test.go:703: expected archive entry is too large, got error reading tar archive: unexpected EOF
	//
	// which is also what proves the charge runs BEFORE the body is read:
	// uncharged, the entry is skipped and the first failure the extractor can
	// report is the truncated body this fixture deliberately omits.
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveEntryIsTooLarge, err)
	}
}

// TestExtractUnknownTypeflagUnderCapIsSkipped is the positive control for
// TestExtractUnknownTypeflagEntryIsSizeCapped: a '9'-typeflag entry with a
// declared size the budget accepts must still be skipped silently while the
// rest of the archive extracts. Without it, "the extractor refused" would be
// indistinguishable from "the extractor never accepted this shape at all".
//
// It is deliberately not the same fixture, and the difference is worth stating
// rather than glossing. The refusal above uses buildHeaderOnlyArchive, whose
// single entry declares a body it never writes; measured, that builder yields
// a '9' archive this extractor accepts only at declared size 0, failing with
// `unexpected EOF` at any nonzero size under the cap (which is the output the
// refusal's own killing mutation quotes). So the only same-fixture control
// available would declare 0 - and that cannot distinguish "the budget accepted
// this size" from "there was no size to charge at all". A body-backed
// buildTestArchive entry can declare a real, nonzero size and be accepted,
// which is the property a control has to demonstrate.
func TestExtractUnknownTypeflagUnderCapIsSkipped(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: '9', name: "bomb", content: []byte("body")},
		{typeflag: tar.TypeReg, name: "README.md", content: []byte("# ok\n")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err := os.ReadFile(filepath.Join(dst, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	if string(got) != "# ok\n" {
		t.Fatalf("README.md = %q", got)
	}
	if _, statErr := os.Lstat(filepath.Join(dst, "bomb")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected the skipped entry to leave nothing on disk, stat error: %v", statErr)
	}
}

// TestExtractDotNamedEntryIsSizeCapped closes the second path that reached a
// tar header without charging it: an entry named "." normalizes to an empty
// relative path, so handleTarEntry returns before it dispatches on anything at
// all. The declared size still has to be read past.
func TestExtractDotNamedEntryIsSizeCapped(t *testing.T) {
	t.Parallel()

	archiveBytes := buildHeaderOnlyArchive(t, tar.TypeReg, ".", helpers.ArchiveMaxEntrySize+1)

	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), t.TempDir())
	// Killing mutation: the same one the default-arm test above describes -
	// delete the chargeEntrySize call from extractTarEntries and the `declared`
	// half of its `var declared, entries int64`, without which the mutant does
	// not compile. This assertion then fails with
	//
	//	archive_test.go:771: expected archive entry is too large, got error reading tar archive: unexpected EOF
	//
	// the same output the default-arm test above reports, for the same reason:
	// the sanitize-to-empty return hands the entry back to tar.Reader.Next
	// uncharged.
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveEntryIsTooLarge, err)
	}
}

// TestExtractDotEntryUnderCapIsSkipped is the positive control for
// TestExtractDotNamedEntryIsSizeCapped: a regular-file entry named "." with a
// declared size the budget accepts must still be normalized away and skipped,
// leaving the rest of the archive to extract. It is body-backed rather than
// header-only, so it is not the refusal's own fixture - see
// TestExtractUnknownTypeflagUnderCapIsSkipped for why the header-only builder
// cannot supply a control that proves anything here.
func TestExtractDotEntryUnderCapIsSkipped(t *testing.T) {
	t.Parallel()

	entries := []testArchiveEntry{
		{typeflag: tar.TypeReg, name: ".", content: []byte("x")},
		{typeflag: tar.TypeReg, name: "file.txt", content: []byte("data")},
	}
	archiveBytes := buildTestArchive(t, entries)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	//nolint:gosec // dst is a t.TempDir() and the joined path is a literal, not external input.
	got, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("file.txt = %q", got)
	}
}

// TestExtractHeaderOnlyEntryWithDeclaredSizeIsCharged pins the one shape this
// budget deliberately narrows. A directory entry declaring a nonzero size is
// otherwise a valid, extractable archive: the writer emits no body for a
// header-only typeflag and closes cleanly, and the reader hands back the
// declared size and parses the following entry normally - see
// TestExtractHeaderOnlyEntryUnderCapIsAccepted, the same fixture under the
// cap. Charging every header regardless of typeflag is what turns a
// header-only entry declaring more than the per-entry cap into a refusal.
func TestExtractHeaderOnlyEntryWithDeclaredSizeIsCharged(t *testing.T) {
	t.Parallel()

	archiveBytes := buildHeaderOnlyArchive(t, tar.TypeDir, "d/", helpers.ArchiveMaxEntrySize+1)

	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), t.TempDir())
	// Killing mutation: add `if header.Typeflag != tar.TypeReg { return nil }`
	// at the top of chargeEntrySize - the narrowest way to put the budget back
	// where it was, charging only the typeflag that writes bytes. This
	// assertion then fails with
	//
	//	archive_test.go:832: expected archive entry is too large, got <nil>
	//
	// a bare nil rather than a read failure, because a header-only entry
	// carries no body to trip over afterwards. That is what this fixture adds
	// over the two above: it separates "never charged" from "charged, and then
	// the body was read".
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveEntryIsTooLarge, err)
	}
}

// TestExtractHeaderOnlyEntryUnderCapIsAccepted is the positive control for
// TestExtractHeaderOnlyEntryWithDeclaredSizeIsCharged: the same fixture and
// the same header-only typeflag, differing only in the declared size, must
// extract the directory normally - so that test's refusal is a verdict on the
// size, not on the shape.
func TestExtractHeaderOnlyEntryUnderCapIsAccepted(t *testing.T) {
	t.Parallel()

	archiveBytes := buildHeaderOnlyArchive(t, tar.TypeDir, "d/", 4096)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Lstat(filepath.Join(dst, "d"))
	if err != nil {
		t.Fatalf("lstat d: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected d to be a directory, got mode %v", info.Mode())
	}
}

// tarBlockSize is the size of one tar header block, and of one data block.
const tarBlockSize = 512

// sparseFixturePhysical and sparseFixtureLogical are the two independent sizes
// the sparse fixture below declares: the bytes really present in the stream,
// which archive/tar reads past, and the logical file size, which is all
// chargeEntrySize ever sees. A megabyte against a single byte is a ratio wide
// enough that no nonzero declared-size budget could refuse the entry.
const (
	sparseFixturePhysical = int64(1 << 20)
	sparseFixtureLogical  = int64(1)
)

// putTarOctal writes v into a tar header field as zero-padded octal with the
// customary NUL terminator, which is the encoding archive/tar's own parser
// reads back out of these fields.
func putTarOctal(field []byte, v int64) {
	copy(field, fmt.Sprintf("%0*o", len(field)-1, v))
	field[len(field)-1] = 0x00
}

// buildOldGNUSparseArchive renders exactly one old-GNU sparse entry ('S') into
// an in-memory tar.gz: physical bytes in the header's size field, logical
// bytes in both its realsize field and its single sparse-map fragment, and a
// body of physical zero bytes. physical must be a whole number of tar blocks,
// so the entry needs no trailing padding.
//
// The 512-byte block is assembled by hand, at the offsets archive/tar's reader
// parses them from, because tar.Header has no realsize or sparse-map field for
// tar.Writer to encode: measured, it writes 'S' with both left as NUL bytes.
//
// The two sizes are the whole point of the fixture. archive/tar sizes the
// entry's body reader from the physical one and then overwrites Header.Size
// with the logical one, so an entry that costs the extractor `physical` bytes
// to read past is charged `logical` against the declared-size budgets. A body
// of zeros is also what keeps the fixture tiny: a megabyte of them compresses
// to roughly a kilobyte.
func buildOldGNUSparseArchive(t *testing.T, name string, physical, logical int64) []byte {
	t.Helper()

	if physical%tarBlockSize != 0 {
		t.Fatalf("physical size %d is not a whole number of %d-byte blocks", physical, tarBlockSize)
	}

	// sparseModTime stamps the entry so this fixture never depends on
	// wall-clock time.
	sparseModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()

	blk := make([]byte, tarBlockSize)
	copy(blk[0:100], name)              // name
	putTarOctal(blk[100:108], 0o644)    // mode
	putTarOctal(blk[108:116], 0)        // uid
	putTarOctal(blk[116:124], 0)        // gid
	putTarOctal(blk[124:136], physical) // size: the bytes really in the stream
	putTarOctal(blk[136:148], sparseModTime)
	blk[156] = tar.TypeGNUSparse       // typeflag
	copy(blk[257:263], "ustar ")       // GNU magic
	copy(blk[263:265], " \x00")        // GNU version
	putTarOctal(blk[386:398], 0)       // sparse fragment 0: offset
	putTarOctal(blk[398:410], logical) // sparse fragment 0: length
	putTarOctal(blk[483:495], logical) // realsize: the logical file size

	// The checksum is computed over the block with its own field blanked to
	// spaces, as archive/tar verifies it; sealTarBlock below is its other copy.
	copy(blk[148:156], "        ")
	var sum int64
	for _, b := range blk {
		sum += int64(b)
	}
	copy(blk[148:156], fmt.Sprintf("%06o\x00 ", sum))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	// The trailer is the two zero blocks that end every tar archive.
	for _, chunk := range [][]byte{blk, make([]byte, physical), make([]byte, 2*tarBlockSize)} {
		if _, err := gz.Write(chunk); err != nil {
			t.Fatalf("failed to write sparse fixture: %v", err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractSparseEntryTripsDecompressedCap is the load-bearing test for the
// decompressed-stream cap, and for why that cap exists at all: this entry
// declares one byte and costs a megabyte to read past. The declared-size
// budgets charge 1 and could not refuse it at any nonzero setting, so the
// sentinel this asserts on is also the assertion that the refusal came from
// the stream cap rather than from either of them.
//
// The cap is injected rather than left at helpers.ArchiveMaxDecompressedSize
// deliberately. Driving the production ceiling needs a 4 GiB decompressed
// stream, which is affordable plain and much less so under -race, which is how
// CI runs this suite; a 64 KiB cap against a fixture of about a kilobyte
// exercises the identical code path in milliseconds. This is the same
// injection the entry-count tests above already use for the same reason.
func TestExtractSparseEntryTripsDecompressedCap(t *testing.T) {
	t.Parallel()

	// refuseCap sits far above the entry's declared size and far below its
	// physical body, so only the stream cap can produce a refusal here.
	const refuseCap = int64(64 << 10)
	archiveBytes := buildOldGNUSparseArchive(t, "sparse.bin", sparseFixturePhysical, sparseFixtureLogical)

	err := extractTarGzStream(bytes.NewReader(archiveBytes), t.TempDir(), refuseCap)
	// Killing mutation: unwrap the decompressor in extractTarGzStream - delete
	// the `limited := &decompressedLimitReader{...}` line and hand
	// uncompressedStream straight to tar.NewReader (deleting only the wrap
	// leaves `limited` unused, which does not compile). This assertion then
	// fails with
	//
	//	archive_test.go:978: expected archive decompressed stream exceeds maximum size, got <nil>
	//
	// a bare nil, not a smaller refusal: with the stream uncounted there is no
	// rule left that this entry breaks.
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestExtractSparseEntryUnderDecompressedCapIsAccepted is the positive control
// for TestExtractSparseEntryTripsDecompressedCap, and a true same-fixture one:
// the identical archive through the identical call, differing only in the
// injected cap. Without it, "the extractor refused" would be indistinguishable
// from "this hand-built sparse header is unreadable here". It also pins what
// the entry does on the accepting path - 'S' reaches handleTarEntry's default
// arm, so a megabyte of stream leaves nothing at all on disk.
func TestExtractSparseEntryUnderDecompressedCapIsAccepted(t *testing.T) {
	t.Parallel()

	// acceptCap is comfortably above the fixture's whole decompressed stream.
	const acceptCap = int64(8 << 20)
	archiveBytes := buildOldGNUSparseArchive(t, "sparse.bin", sparseFixturePhysical, sparseFixtureLogical)

	dst := t.TempDir()
	if err := extractTarGzStream(bytes.NewReader(archiveBytes), dst, acceptCap); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, statErr := os.Lstat(filepath.Join(dst, "sparse.bin")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected the skipped sparse entry to leave nothing on disk, stat error: %v", statErr)
	}
}

// buildGNUHeaderOnlyArchive renders exactly one header-only tar entry, pinned
// to the GNU format, and writes no body for it.
//
// Naming the format is what keeps the negative-size fixture below down to a
// single header. buildHeaderOnlyArchive sets no Format, and for a size octal
// cannot encode archive/tar falls back to PAX rather than failing: measured,
// that builder does produce a readable archive carrying the same negative
// Header.Size, but it carries it in a PAX record and emits an 'x' meta header
// ahead of the entry - 160 bytes against this builder's 94. That 'x' header is
// one of the three typeflags chargeEntrySize structurally never sees, so a
// test about what gets charged is better off with no such header in its
// fixture at all. GNU's base-256 encoding puts the negative number in the
// entry's own size field instead, which is also the shape a hostile archive
// would reach for.
//
// tw.Close()'s error is checked here, unlike in buildHeaderOnlyArchive, and
// the difference is not an oversight either way: this builder is for
// header-only typeflags, for which the writer owes no body, so Close must
// succeed and emit the trailer - a failure would mean the fixture itself is
// broken rather than deliberately truncated.
func buildGNUHeaderOnlyArchive(t *testing.T, typeflag byte, name string, size int64) []byte {
	t.Helper()

	// gnuHeaderModTime stamps the entry so this fixture never depends on
	// wall-clock time.
	gnuHeaderModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	header := &tar.Header{
		Typeflag: typeflag,
		Name:     name,
		Size:     size,
		Mode:     0o755,
		ModTime:  gnuHeaderModTime,
		Format:   tar.FormatGNU,
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("failed to write GNU tar header for %s: %v", name, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// negativeDeclaredSize is a size no legitimate archive declares, chosen large
// in magnitude so that the mutation below is unmistakable: added to the
// running total it puts that total nowhere near the per-archive ceiling again,
// for any number of entries that follow.
const negativeDeclaredSize = -(int64(1) << 62)

// TestExtractNegativeDeclaredSizeFailsClosed drives chargeEntrySize's negative
// branch, which nothing else in this repository's suite reaches. The branch is
// not a guard against a value archive/tar could never produce: archive/tar
// parses a GNU base-256 size field as a signed number and forces the body
// reader to zero for a header-only typeflag, so the negative size arrives
// intact on an archive that is otherwise completely well-formed - one header,
// one trailer, no truncation.
//
// It is load-bearing because the per-archive budget is a running sum: a
// negative charge DECREASES that sum, so a single such entry buys everything
// after it an effectively unlimited declared size.
func TestExtractNegativeDeclaredSizeFailsClosed(t *testing.T) {
	t.Parallel()

	archiveBytes := buildGNUHeaderOnlyArchive(t, tar.TypeDir, "d/", negativeDeclaredSize)

	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), t.TempDir())
	// Killing mutation: delete chargeEntrySize's `if header.Size < 0` branch.
	// This assertion then fails with
	//
	//	archive_test.go:1090: expected archive entry has negative size, got <nil>
	//
	// a bare nil rather than one of the two size sentinels below it in that
	// function: a negative size is under the per-entry cap and drags the
	// running total further under the per-archive one, so with this branch
	// gone nothing downstream has anything to object to.
	if !errors.Is(err, helpers.ErrArchiveEntryHasNegativeSize) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveEntryHasNegativeSize, err)
	}
}

// TestExtractGNUFormatHeaderOnlyEntryIsAccepted is the positive control for
// TestExtractNegativeDeclaredSizeFailsClosed, and a true same-fixture one: the
// same builder, typeflag and name, differing only in the sign of the declared
// size. Without it, "the extractor refused" would be indistinguishable from "a
// GNU-format header-only archive is unreadable here".
func TestExtractGNUFormatHeaderOnlyEntryIsAccepted(t *testing.T) {
	t.Parallel()

	archiveBytes := buildGNUHeaderOnlyArchive(t, tar.TypeDir, "d/", 4096)

	dst := t.TempDir()
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Lstat(filepath.Join(dst, "d"))
	if err != nil {
		t.Fatalf("lstat d: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected d to be a directory, got mode %v", info.Mode())
	}
}

// atCapDirCount is how many entries each declaring helpers.ArchiveMaxEntrySize
// fit exactly inside helpers.ArchiveMaxTotalSize. It is derived rather than
// written down, so the boundary the two tests below pin follows a change to
// either constant instead of quietly stopping at the boundary.
const atCapDirCount = int(helpers.ArchiveMaxTotalSize / helpers.ArchiveMaxEntrySize)

// buildDeclaredSizeDirArchive renders count header-only directory entries,
// named d0/ through d<count-1>/, each declaring size bytes it does not carry.
// tar.TypeDir is what makes that expressible at all: archive/tar forces a
// header-only typeflag's body reader to zero while still handing Header.Size
// back verbatim, so every entry costs 512 bytes to read, charges size against
// the budgets, and leaves the following header exactly where it is expected -
// the archive round-trips as an ordinary, complete tar.gz.
func buildDeclaredSizeDirArchive(t *testing.T, count int, size int64) []byte {
	t.Helper()

	// declaredSizeModTime stamps every entry so this fixture never depends on
	// wall-clock time.
	declaredSizeModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for i := range count {
		header := &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     fmt.Sprintf("d%d/", i),
			Size:     size,
			Mode:     0o755,
			ModTime:  declaredSizeModTime,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write tar header for d%d/: %v", i, err)
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

// TestExtractCumulativeDeclaredSizeOverCapFailsClosed drives chargeEntrySize's
// per-archive branch, the other one nothing else in this suite reached. Every
// entry here is individually legal - each declares exactly
// helpers.ArchiveMaxEntrySize, which the per-entry check accepts - so the
// running total is the only rule that can refuse this archive, and it has to
// refuse it at the entry that crosses the ceiling rather than at any earlier
// one.
func TestExtractCumulativeDeclaredSizeOverCapFailsClosed(t *testing.T) {
	t.Parallel()

	archiveBytes := buildDeclaredSizeDirArchive(t, atCapDirCount+1, helpers.ArchiveMaxEntrySize)

	dst := t.TempDir()
	err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst)
	// Killing mutation: delete chargeEntrySize's
	// `if *declared+header.Size > helpers.ArchiveMaxTotalSize` branch, keeping
	// the `*declared += header.Size` below it. This assertion then fails with
	//
	//	archive_test.go:1188: expected archive exceeds maximum total size, got <nil>
	//
	// a bare nil: with the running total no longer consulted, every one of
	// these entries passes the per-entry cap on its own and the archive
	// extracts clean.
	if !errors.Is(err, helpers.ErrArchiveExceedsMaxSize) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveExceedsMaxSize, err)
	}

	// The refusal lands on the crossing entry and not before it: every earlier
	// directory is already on disk, and the crossing one never was.
	for i := range atCapDirCount {
		name := fmt.Sprintf("d%d", i)
		if _, statErr := os.Stat(filepath.Join(dst, name)); statErr != nil {
			t.Fatalf("expected %s to exist: %v", name, statErr)
		}
	}
	crossing := fmt.Sprintf("d%d", atCapDirCount)
	if _, statErr := os.Lstat(filepath.Join(dst, crossing)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected %s to not exist, stat error: %v", crossing, statErr)
	}
}

// TestExtractCumulativeDeclaredSizeAtCapIsAccepted is the positive control for
// the test above, and it pins the boundary the same way TestExtractEntryCountAtCap
// pins the entry-count one: a running total landing exactly on
// helpers.ArchiveMaxTotalSize must be accepted, because the check is
// `*declared+header.Size > cap` rather than `>=`.
func TestExtractCumulativeDeclaredSizeAtCapIsAccepted(t *testing.T) {
	t.Parallel()

	archiveBytes := buildDeclaredSizeDirArchive(t, atCapDirCount, helpers.ArchiveMaxEntrySize)

	dst := t.TempDir()
	// Killing mutation: change chargeEntrySize's per-archive comparison from
	// `>` to `>=`. This assertion then fails with
	//
	//	archive_test.go:1225: unexpected error: archive exceeds maximum total size: 4294967296 bytes
	//
	// on the entry that brings the total to exactly the cap. This control pins
	// that boundary directly: the refusal test above reports the same sentinel
	// either way, and catches the shift only incidentally, in its placement loop.
	if err := ExtractTarGzStream(bytes.NewReader(archiveBytes), dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range atCapDirCount {
		name := fmt.Sprintf("d%d", i)
		if _, statErr := os.Stat(filepath.Join(dst, name)); statErr != nil {
			t.Fatalf("expected %s to exist: %v", name, statErr)
		}
	}
}

// sealTarBlock writes blk's own checksum into its checksum field, computed
// over the block with that field blanked to spaces, as archive/tar verifies
// it. buildOldGNUSparseArchive above holds this file's other copy of it.
func sealTarBlock(blk []byte) {
	copy(blk[148:156], "        ")
	var sum int64
	for _, b := range blk {
		sum += int64(b)
	}
	copy(blk[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

// buildMetaHeaderChainArchive renders count zero-size GNU long-link ('K')
// headers into an in-memory tar.gz, followed by the two zero blocks that
// terminate a tar stream.
//
// The block is assembled by hand because tar.Writer refuses this typeflag
// outright ("cannot manually encode TypeXHeader, TypeGNULongName, or
// TypeGNULongLink headers"): 'K' is a meta header archive/tar produces and
// consumes inside Next() on its own, and never hands a caller a header for.
// Declaring size 0 is what makes the chain cost nothing but its framing - the
// reader's own 1 MiB cap on a meta-header body never comes into it.
//
// The trailer is not decoration; without it the fixture cannot exercise the
// rule it exists for. Measured: with the trailer omitted the stream simply
// runs out, the last io.ReadFull of a header block comes up short of its
// minimum, and a short read keeps its error instead of discarding it - so the
// extractor reports the cap whatever the reader below it returned, and the
// test passes without the rule under test doing any work.
func buildMetaHeaderChainArchive(t *testing.T, count int) []byte {
	t.Helper()

	// metaChainModTime stamps the header so this fixture never depends on
	// wall-clock time.
	metaChainModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()

	blk := make([]byte, tarBlockSize)
	copy(blk[0:100], "././@LongLink") // name, the one GNU tar itself writes
	putTarOctal(blk[100:108], 0o644)  // mode
	putTarOctal(blk[108:116], 0)      // uid
	putTarOctal(blk[116:124], 0)      // gid
	putTarOctal(blk[124:136], 0)      // size: no body at all
	putTarOctal(blk[136:148], metaChainModTime)
	blk[156] = tar.TypeGNULongLink // typeflag
	copy(blk[257:263], "ustar ")   // GNU magic
	copy(blk[263:265], " \x00")    // GNU version
	sealTarBlock(blk)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for range count {
		if _, err := gz.Write(blk); err != nil {
			t.Fatalf("failed to write meta header: %v", err)
		}
	}
	if _, err := gz.Write(make([]byte, 2*tarBlockSize)); err != nil {
		t.Fatalf("failed to write tar trailer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestExtractMetaHeaderChainTripsDecompressedCap drives the stream cap with
// the shape that has no other rule to meet. tar.Reader.Next consumes a 'K'
// header and continues its own loop without returning anything, so
// extractTarEntries' entry counter never counts one and chargeEntrySize never
// charges one: the sentinel asserted here is therefore also an assertion
// about which rule fired, because no other rule can see this archive at all.
//
// The cap is injected rather than left at helpers.ArchiveMaxDecompressedSize
// for the reason the sparse test above gives: a few hundred 512-byte headers
// against a 4 KiB cap exercise the identical code path in microseconds.
func TestExtractMetaHeaderChainTripsDecompressedCap(t *testing.T) {
	t.Parallel()

	const (
		headers   = 200
		refuseCap = int64(4 << 10)
	)
	archiveBytes := buildMetaHeaderChainArchive(t, headers)

	err := extractTarGzStream(bytes.NewReader(archiveBytes), t.TempDir(), refuseCap)
	// Killing mutation: strip decompressedLimitReader.Read back to a plain
	// pass-through - delete its sticky-error block and its clamp block, and
	// return the crossing read's bytes alongside the refusal (`return n,
	// fmt.Errorf(...)` instead of storing the error and returning zero). This
	// assertion then fails with
	//
	//	archive_test.go:1332: expected archive decompressed stream exceeds maximum size, got <nil>
	//
	// a bare nil: every one of those refusals is raised into an io.ReadFull
	// that had its 512 bytes and threw the error away, and the extractor reads
	// the chain to its end and reports success.
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestExtractMetaHeaderChainUnderDecompressedCapIsAccepted is the positive
// control for the test above, and a true same-fixture one: the identical
// archive through the identical call, differing only in the injected cap.
// Without it, "the extractor refused" would be indistinguishable from "this
// hand-built meta header is unreadable here". It also pins what the chain
// does on the accepting path - a header Next never returns extracts nothing,
// so the whole destination stays empty.
func TestExtractMetaHeaderChainUnderDecompressedCapIsAccepted(t *testing.T) {
	t.Parallel()

	const (
		headers   = 200
		acceptCap = int64(8 << 20)
	)
	archiveBytes := buildMetaHeaderChainArchive(t, headers)

	dst := t.TempDir()
	if err := extractTarGzStream(bytes.NewReader(archiveBytes), dst, acceptCap); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	left, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("expected the meta-header chain to leave nothing on disk, got %d entries", len(left))
	}
}

// TestExtractHeaderOnlyEntriesTripDecompressedCap drives the same refusal
// through the shape the entry counter CAN see, which is what separates it
// from the meta-header chain above: ordinary tar.Writer-built directory
// entries, each one returned by Next and counted. They declare size 0, so
// both byte budgets charge nothing, and the entry cap is left at its
// production value, so the only rule this archive can break is the stream
// cap - which it breaks on framing alone, at 512 bytes per header.
func TestExtractHeaderOnlyEntriesTripDecompressedCap(t *testing.T) {
	t.Parallel()

	const (
		dirs      = 20
		refuseCap = int64(4 << 10)
	)
	archiveBytes := buildDeclaredSizeDirArchive(t, dirs, 0)

	err := extractTarGzStream(bytes.NewReader(archiveBytes), t.TempDir(), refuseCap)
	// Killing mutation: the same one the meta-header test above describes -
	// strip decompressedLimitReader.Read back to a plain pass-through that
	// returns the crossing read's bytes alongside the refusal. This assertion
	// then fails with
	//
	//	archive_test.go:1394: expected archive decompressed stream exceeds maximum size, got <nil>
	//
	// for the identical reason, on a shape every other rule in the extractor
	// does see: the entry counter counts all twenty of these headers and the
	// byte budgets charge all twenty, and neither has anything to object to.
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestExtractHeaderOnlyEntriesUnderDecompressedCapAreAccepted is the positive
// control for the test above: the same fixture through the same call under a
// cap its whole stream fits in, extracting every directory. Without it, "the
// extractor refused" would be indistinguishable from "a size-0 directory
// entry is unreadable here".
func TestExtractHeaderOnlyEntriesUnderDecompressedCapAreAccepted(t *testing.T) {
	t.Parallel()

	const (
		dirs      = 20
		acceptCap = int64(8 << 20)
	)
	archiveBytes := buildDeclaredSizeDirArchive(t, dirs, 0)

	dst := t.TempDir()
	if err := extractTarGzStream(bytes.NewReader(archiveBytes), dst, acceptCap); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := range dirs {
		name := fmt.Sprintf("d%d", i)
		if _, statErr := os.Stat(filepath.Join(dst, name)); statErr != nil {
			t.Fatalf("expected %s to exist: %v", name, statErr)
		}
	}
}

// countingReader counts the bytes it hands out, so a test can tell a reader
// above it that stopped pulling from one that merely stopped reporting.
type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// TestDecompressedLimitReaderRefusesThroughReadFull pins the zero return
// against io.ReadAtLeast, which is what archive/tar's readHeader reads every
// 512-byte header block with: its `if n >= min { err = nil }` throws away a
// non-nil error whenever the request was satisfied in full, so a limiter
// handing back the crossing read's bytes has its refusal discarded here.
//
// max is exactly one below the request on purpose, and rounding it off would
// silently un-pin the test: the clamp shortens the crossing read to
// remaining+1 bytes, so at any lower max io.ReadFull comes up short of its
// minimum, keeps the error for that reason alone, and stops distinguishing a
// limiter that returns zero from one that does not.
func TestDecompressedLimitReaderRefusesThroughReadFull(t *testing.T) {
	t.Parallel()

	const (
		request = 512
		limit   = int64(request - 1)
	)
	lim := &decompressedLimitReader{r: bytes.NewReader(make([]byte, 4<<10)), max: limit}

	_, err := io.ReadFull(lim, make([]byte, request))
	// Killing mutation: return the crossing read's bytes alongside the
	// refusal - `return n, r.err` in place of `return 0, r.err` - keeping the
	// sticky error and the clamp. This assertion then fails with
	//
	//	archive_test.go:1472: expected archive decompressed stream exceeds maximum size, got <nil>
	//
	// a bare nil: io.ReadFull got its 512 bytes, so io.ReadAtLeast nils the
	// error out. Measured, both archive-level tests above pass under that same
	// mutation, which is why the zero return has to be pinned here rather than
	// up there: a swallow needs the crossing read to deliver exactly the
	// number of bytes io.ReadFull still wants, and only a max one byte below
	// the request makes that certain.
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestDecompressedLimitReaderRefusesThroughCopyN pins the zero return against
// io.CopyN, the second discarding shape: its `if written == n { return n, nil }`
// reports success whenever the requested count was delivered. archive/tar's
// discard reaches it reading past a skipped entry body, and this package's own
// extractRegularFile reaches it writing one to disk.
//
// The requested count is exactly remaining+1 for the same reason max is
// exactly one below the request above: any larger request leaves io.CopyN
// short of what it asked for, which keeps the error regardless of what the
// limiter returned.
func TestDecompressedLimitReaderRefusesThroughCopyN(t *testing.T) {
	t.Parallel()

	const limit = int64(10)
	lim := &decompressedLimitReader{r: bytes.NewReader(make([]byte, 4<<10)), max: limit}

	_, err := io.CopyN(io.Discard, lim, limit+1)
	// Killing mutation: the same one the io.ReadFull test above describes -
	// `return n, r.err` in place of `return 0, r.err`. This assertion then
	// fails with
	//
	//	archive_test.go:1502: expected archive decompressed stream exceeds maximum size, got <nil>
	//
	// a bare nil: io.CopyN was handed all 11 bytes it asked for, so it reports
	// success and drops the error.
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
}

// TestDecompressedLimitReaderRefusalIsSticky pins retention rather than
// reporting. A caller that keeps reading after the refusal - archive/tar does
// not, but nothing in io.Reader's contract stops one - must not be able to
// drain the decompressor one clamped byte per call.
//
// Of the two assertions below only the second is pinned, and the difference is
// the point of the test. Without the sticky error every post-refusal read
// still returns (0, sentinel), because the cumulative count is already past
// max and the clamp shortens each of those reads to a single byte; what
// changes is that each one pulls that byte through. The first assertion is
// documentary - it states the shape the caller sees, which holds either way.
func TestDecompressedLimitReaderRefusalIsSticky(t *testing.T) {
	t.Parallel()

	const (
		limit      = int64(8)
		afterReads = 5
	)
	counter := &countingReader{r: bytes.NewReader(make([]byte, 4<<10))}
	lim := &decompressedLimitReader{r: counter, max: limit}

	buf := make([]byte, 64)
	if _, err := lim.Read(buf); !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected the first read to trip the cap, got %v", err)
	}
	atRefusal := counter.read

	for i := range afterReads {
		n, err := lim.Read(buf)
		if n != 0 || !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
			t.Fatalf("read %d after the refusal returned (%d, %v)", i, n, err)
		}
	}

	// Killing mutation: delete Read's `if r.err != nil` block. This assertion
	// then fails with
	//
	//	archive_test.go:1549: underlying reader advanced by 5 bytes after the refusal
	//
	// one byte per post-refusal read, which is the clamp doing its job on a
	// cumulative count that is already over: the refusal is re-raised every
	// time and every time a byte has already been pulled to raise it.
	if counter.read != atRefusal {
		t.Fatalf("underlying reader advanced by %d bytes after the refusal", counter.read-atRefusal)
	}
}

// TestDecompressedLimitReaderClampsOverrunToOneByte pins the clamp, the one
// of this reader's three properties that is about accounting rather than
// about refusing. A 32 KiB request one kilobyte from the ceiling must pull a
// single byte past it rather than a whole buffer past it, which is what makes
// the byte count in the reported error the exact number of bytes this reader
// let through.
func TestDecompressedLimitReaderClampsOverrunToOneByte(t *testing.T) {
	t.Parallel()

	const (
		limit   = int64(1024)
		request = 32 << 10
	)
	lim := &decompressedLimitReader{r: bytes.NewReader(make([]byte, 64<<10)), max: limit}

	if _, err := lim.Read(make([]byte, request)); !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("expected %v, got %v", helpers.ErrArchiveDecompressedTooLarge, err)
	}
	// Killing mutation: delete Read's clamp block, leaving p at whatever
	// length the caller passed, together with the `remaining :=` line above it
	// - the clamp is its only use, so deleting the block by itself does not
	// compile ("declared and not used: remaining"). This assertion then fails
	// with
	//
	//	archive_test.go:1584: read 31744 bytes past a 1024-byte ceiling, want exactly one
	//
	// the whole 32 KiB request landing past a 1 KiB ceiling. The refusal above
	// still fires - which is why it is not the assertion that catches this -
	// but the count it reports is the buffer size the caller happened to
	// choose rather than the ceiling this reader enforces.
	if lim.n != limit+1 {
		t.Fatalf("read %d bytes past a %d-byte ceiling, want exactly one", lim.n-limit, limit)
	}
}

// TestDecompressedLimitReaderClampSurvivesExtremeMax covers the two values of
// max whose arithmetic the clamp has to survive rather than act on: one where
// remaining+1 overflows, and one where remaining is negative enough for the
// reslice to index below zero. Neither is reachable from this package's own
// call site, which passes helpers.ArchiveMaxDecompressedSize; both are
// reachable from the injected-cap seam these tests themselves use.
func TestDecompressedLimitReaderClampSurvivesExtremeMax(t *testing.T) {
	t.Parallel()

	t.Run("max at MaxInt64 passes the read through", func(t *testing.T) {
		t.Parallel()

		const payload = "hello"
		lim := &decompressedLimitReader{r: bytes.NewReader([]byte(payload)), max: math.MaxInt64}
		// Killing mutation: write the clamp as `if int64(len(p)) > remaining+1`
		// instead of `if remaining < int64(len(p))`. The two pick out the same
		// reslices - the single case they disagree on is the one where
		// p[:remaining+1] is p itself - but they are not the same in int64
		// arithmetic: at this max, remaining+1 overflows to the smallest
		// negative value, so the comparison holds for any buffer at all and the
		// read below panics with
		//
		//	panic: runtime error: slice bounds out of range [:-9223372036854775808]
		//
		// rather than failing an assertion.
		n, err := lim.Read(make([]byte, 16))
		if n != len(payload) || err != nil {
			t.Fatalf("read = (%d, %v), want (%d, <nil>)", n, err, len(payload))
		}
	})

	t.Run("negative max refuses without panicking", func(t *testing.T) {
		t.Parallel()

		const limit = int64(-1024)
		lim := &decompressedLimitReader{r: bytes.NewReader(make([]byte, 64)), max: limit}
		// Killing mutation: drop Read's clamp to zero, computing remaining as
		// `r.max - r.n` instead of `max(r.max-r.n, 0)`. remaining is then this
		// max itself, the clamp below reslices p to remaining+1, and the read
		// below panics with
		//
		//	panic: runtime error: slice bounds out of range [:-1023]
		//
		// rather than failing an assertion. A max of -1 would not do: it
		// reslices to p[:0], which is legal, so it takes -2 or lower to reach
		// the panic this case exists for.
		n, err := lim.Read(make([]byte, 64))
		if n != 0 || !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
			t.Fatalf("read = (%d, %v), want (0, %v)", n, err, helpers.ErrArchiveDecompressedTooLarge)
		}
	})
}

// TestDecompressedLimitReaderClampAcceptsEmptyInputs covers the other two
// boundary shapes: a zero-length destination buffer, and a zero max over a
// stream with nothing in it. Both must pass through untouched - a reader that
// has delivered nothing has not exceeded anything, whatever max says - and
// neither may be turned into a refusal or a panic by the clamp's reslice.
func TestDecompressedLimitReaderClampAcceptsEmptyInputs(t *testing.T) {
	t.Parallel()

	t.Run("zero-length buffer reads nothing", func(t *testing.T) {
		t.Parallel()

		lim := &decompressedLimitReader{r: bytes.NewReader([]byte("hello")), max: 1024}
		n, err := lim.Read(nil)
		if n != 0 || err != nil {
			t.Fatalf("read = (%d, %v), want (0, <nil>)", n, err)
		}
	})

	t.Run("zero max over an empty stream reports EOF", func(t *testing.T) {
		t.Parallel()

		lim := &decompressedLimitReader{r: bytes.NewReader(nil), max: 0}
		n, err := lim.Read(make([]byte, 16))
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read = (%d, %v), want (0, %v)", n, err, io.EOF)
		}
	})
}

// probeTarGzCase is one table entry for TestProbeTarGz.
type probeTarGzCase struct {
	// build returns the bytes to write at the probed path, or nil to skip
	// writing the file at all (the missing-path row).
	build   func(t *testing.T) []byte
	wantErr error
	name    string
	// wantAnyErr marks a row that must fail without carrying
	// helpers.ErrArtifactNotTarGz: an unreadable path is an environment
	// problem, not a statement about the artifact's shape.
	wantAnyErr bool
}

// probeTarGzCases enumerates what ProbeTarGz accepts - a real archive, and an
// archive that is well-formed but holds no entries - alongside the two ways
// the outer shape can be wrong (not gzip at all, and gzip wrapping something
// that is not a tar) and the one failure that is about the file rather than
// its content.
func probeTarGzCases() []probeTarGzCase {
	notAnArchive := []byte("<html>404</html>")
	return []probeTarGzCase{
		{
			name: "real archive accepted",
			build: func(t *testing.T) []byte {
				t.Helper()
				return buildTestArchive(t, []testArchiveEntry{{name: "README.md", content: []byte("x")}})
			},
		},
		{
			// A tar with no entries is well-formed; Next reports io.EOF on the
			// first call and the probe must read that as "empty", not "broken".
			name:  "empty archive accepted",
			build: func(t *testing.T) []byte { t.Helper(); return buildTestArchive(t, nil) },
		},
		{
			name:    "non-gzip bytes refused",
			build:   func(t *testing.T) []byte { t.Helper(); return notAnArchive },
			wantErr: helpers.ErrArtifactNotTarGz,
		},
		{
			name:    "gzip wrapping something that is not a tar refused",
			build:   func(t *testing.T) []byte { t.Helper(); return gzipBytes(t, notAnArchive) },
			wantErr: helpers.ErrArtifactNotTarGz,
		},
		{
			name:       "missing path fails without claiming a shape",
			build:      nil,
			wantAnyErr: true,
		},
	}
}

// gzipBytes compresses data with gzip and returns the compressed bytes, so a
// test can build a stream that is valid gzip and nothing else.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// assertProbeOutcome checks err against what tt expects. Split out of the
// test body so the loop stays within the cyclomatic-complexity budget the
// three-way outcome would otherwise push it past.
func assertProbeOutcome(t *testing.T, tt probeTarGzCase, path string, err error) {
	t.Helper()
	switch {
	case tt.wantAnyErr:
		if err == nil {
			t.Fatalf("ProbeTarGz(%q) = nil, want a non-nil error", path)
		}
		if errors.Is(err, helpers.ErrArtifactNotTarGz) {
			t.Fatalf("ProbeTarGz(%q) = %v, want an error that does not claim the artifact's shape", path, err)
		}
	case tt.wantErr != nil:
		if !errors.Is(err, tt.wantErr) {
			t.Fatalf("ProbeTarGz(%q) = %v, want errors.Is %v", path, err, tt.wantErr)
		}
	default:
		if err != nil {
			t.Fatalf("ProbeTarGz(%q) = %v, want nil", path, err)
		}
	}
}

// TestProbeTarGz drives ProbeTarGz over every row in probeTarGzCases against
// real files on disk, since the probe takes a path rather than a reader.
func TestProbeTarGz(t *testing.T) {
	t.Parallel()

	for _, tt := range probeTarGzCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "artifact.tar.gz")
			if tt.build != nil {
				if err := os.WriteFile(path, tt.build(t), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}

			assertProbeOutcome(t, tt, path, ProbeTarGz(path))
		})
	}
}
