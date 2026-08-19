package manifest

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/klauspost/pgzip"
)

// testManifest is the document every fixture below carries as its top-level
// MANIFEST.json. Its exact bytes are what the positive assertions compare
// against, so a walk that returned some other entry's body would fail on the
// content rather than merely on the length.
const testManifest = `{"collection_info":{"namespace":"acme","name":"widgets","version":"1.0.0"}}`

// testEntry describes one tar entry for tarStream. declaredSize overrides the
// size the entry's header announces, the only way to build a header declaring
// more bytes than the archive carries; zero means "the length of content".
// typeflag defaults to tar.TypeReg, and linkname is written verbatim.
type testEntry struct {
	name, linkname string
	content        []byte
	declaredSize   int64
	typeflag       byte
}

// tarStream renders entries, in order, into a raw tar byte stream.
//
// It writes each entry through its own tar.Writer, which is what lets one
// declare a size it never writes: a tar stream is a concatenation of
// self-contained 512-byte blocks, while a single writer refuses to emit a
// second header after an entry whose body it never received. Flush reports
// exactly that shortfall, and for an over-declaring entry that report is this
// builder's purpose rather than a failure - it also suppresses the padding
// Flush would otherwise write, which keeps the stream block-aligned for the
// entries that follow.
func tarStream(tb testing.TB, entries []testEntry) []byte {
	tb.Helper()

	// A fixed stamp, so a fixture's bytes never depend on when it was built.
	modTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	for _, e := range entries {
		size := e.declaredSize
		if size == 0 {
			size = int64(len(e.content))
		}
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}

		tw := tar.NewWriter(&buf)
		header := &tar.Header{Typeflag: typeflag, Name: e.name, Linkname: e.linkname, Size: size, Mode: 0o644, ModTime: modTime}
		if err := tw.WriteHeader(header); err != nil {
			tb.Fatalf("failed to write the tar header for %s: %v", e.name, err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				tb.Fatalf("failed to write the tar content for %s: %v", e.name, err)
			}
		}
		_ = tw.Flush()
	}

	// The trailer: two zero blocks.
	buf.Write(make([]byte, 2*512))
	return buf.Bytes()
}

// writeArtifact gzips entries into a file under the test's own temp directory
// and returns its absolute path, which is the shape ReadFromTarGz takes.
func writeArtifact(tb testing.TB, entries []testEntry) string {
	tb.Helper()

	var buf bytes.Buffer
	gz := pgzip.NewWriter(&buf)
	if _, err := gz.Write(tarStream(tb, entries)); err != nil {
		tb.Fatalf("failed to compress the fixture: %v", err)
	}
	if err := gz.Close(); err != nil {
		tb.Fatalf("failed to close the fixture's gzip writer: %v", err)
	}
	return writeFile(tb, "artifact.tar.gz", buf.Bytes())
}

// writePaddedArtifact writes an artifact carrying padBytes of zeros ahead of a
// top-level MANIFEST.json, so the manifest sits a chosen distance into the
// decompressed stream.
//
// The padding is compressed in chunks rather than assembled in memory, so a
// fixture reaching past the production scan bound never holds its own
// decompressed size; being zeros, it also leaves the file on disk around a
// hundred kilobytes whatever padBytes is. Both properties are what make a test
// at the real ceiling cheap enough to run unconditionally.
func writePaddedArtifact(t *testing.T, padBytes int64) string {
	t.Helper()

	var buf bytes.Buffer
	gz := pgzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	modTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	pad := &tar.Header{Typeflag: tar.TypeReg, Name: "pad.bin", Size: padBytes, Mode: 0o644, ModTime: modTime}
	if err := tw.WriteHeader(pad); err != nil {
		t.Fatalf("failed to write the padding header: %v", err)
	}
	chunk := make([]byte, 1<<20)
	for written := int64(0); written < padBytes; written += int64(len(chunk)) {
		if _, err := tw.Write(chunk[:min(int64(len(chunk)), padBytes-written)]); err != nil {
			t.Fatalf("failed to write the padding body: %v", err)
		}
	}

	manifest := &tar.Header{
		Typeflag: tar.TypeReg, Name: helpers.ManifestFileName,
		Size: int64(len(testManifest)), Mode: 0o644, ModTime: modTime,
	}
	if err := tw.WriteHeader(manifest); err != nil {
		t.Fatalf("failed to write the manifest header: %v", err)
	}
	if _, err := tw.Write([]byte(testManifest)); err != nil {
		t.Fatalf("failed to write the manifest body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close the fixture's tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close the fixture's gzip writer: %v", err)
	}
	return writeFile(t, "padded.tar.gz", buf.Bytes())
}

// writeFile drops data at name under the test's own temp directory and returns
// the absolute path.
func writeFile(tb testing.TB, name string, data []byte) string {
	tb.Helper()

	path := filepath.Join(tb.TempDir(), name)
	if err := os.WriteFile(path, data, helpers.FileMod); err != nil {
		tb.Fatalf("failed to write the fixture %s: %v", name, err)
	}
	return path
}

// nonManifestEntries is the surrounding content a real collection carries,
// returned fresh per call so a caller can append to it without aliasing.
func nonManifestEntries() []testEntry {
	return []testEntry{
		{name: "README.md", content: []byte("# acme.widgets\n")},
		{name: "roles/", typeflag: tar.TypeDir},
		{name: "plugins/modules/widget.py", content: []byte("# widget\n")},
	}
}

func TestReadFromTarGzFindsFirstEntry(t *testing.T) {
	t.Parallel()

	artifact := writeArtifact(t, append(
		[]testEntry{{name: helpers.ManifestFileName, content: []byte(testManifest)}},
		nonManifestEntries()...))

	got, err := ReadFromTarGz(t.Context(), artifact)
	if err != nil {
		t.Fatalf("ReadFromTarGz(manifest first) error = %v, want the manifest", err)
	}
	if string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(manifest first) = %q, want %q", got, testManifest)
	}
}

func TestReadFromTarGzFindsLateEntry(t *testing.T) {
	t.Parallel()

	artifact := writeArtifact(t, append(nonManifestEntries(),
		testEntry{name: helpers.ManifestFileName, content: []byte(testManifest)},
		testEntry{name: "FILES.json", content: []byte(`{"files":[]}`)}))

	got, err := ReadFromTarGz(t.Context(), artifact)
	if err != nil {
		t.Fatalf("ReadFromTarGz(manifest fourth) error = %v, want the manifest", err)
	}
	if string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(manifest fourth) = %q, want %q", got, testManifest)
	}
}

func TestReadFromTarGzRejectsMissingManifest(t *testing.T) {
	t.Parallel()

	// The positive control comes first: the identical entries plus a top-level
	// manifest are read back, so the refusal below is this walk finding no
	// manifest rather than the fixture being unreadable to begin with.
	control := writeArtifact(t, append(nonManifestEntries(),
		testEntry{name: helpers.ManifestFileName, content: []byte(testManifest)}))
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(control with a manifest) = %q, %v, want the manifest and no error", got, err)
	}

	artifact := writeArtifact(t, nonManifestEntries())
	if _, err := ReadFromTarGz(t.Context(), artifact); !errors.Is(err, helpers.ErrManifestNotFound) {
		t.Fatalf("ReadFromTarGz(no manifest) error = %v, want the not-found sentinel", err)
	}
}

func TestReadFromTarGzRejectsNestedManifest(t *testing.T) {
	t.Parallel()

	nested := testEntry{name: "vendored/other/" + helpers.ManifestFileName, content: []byte(`{"decoy":true}`)}

	// The positive control puts the nested entry AHEAD of the real one, so a
	// name check matching on the base name alone would return the decoy rather
	// than merely accepting an archive that has both.
	//
	// Relaxing that check - path.Clean(header.Name) in topLevelManifest
	// replaced by path.Base(header.Name), applied through go test -overlay so
	// no production file is edited - fails here first:
	//
	//	read_test.go:235: ReadFromTarGz(nested entry ahead of the real one) = "{\"decoy\":true}", want the top-level manifest (err: <nil>)
	control := writeArtifact(t, []testEntry{
		nested,
		{name: helpers.ManifestFileName, content: []byte(testManifest)},
	})
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(nested entry ahead of the real one) = %q, want the top-level manifest (err: %v)", got, err)
	}

	artifact := writeArtifact(t, append(nonManifestEntries(), nested))
	if _, err := ReadFromTarGz(t.Context(), artifact); !errors.Is(err, helpers.ErrManifestNotFound) {
		t.Fatalf("ReadFromTarGz(nested manifest only) error = %v, want the not-found sentinel", err)
	}
}

func TestReadFromTarGzRejectsOversizeEntry(t *testing.T) {
	t.Parallel()

	// An entry declaring one byte past the per-entry cap and carrying nothing.
	// The walk has to refuse it on its header alone, since reading past a body
	// this size is exactly what the cap exists to prevent - and the fixture
	// could not carry those bytes anyway.
	oversize := testEntry{name: "huge.bin", declaredSize: helpers.ArchiveMaxEntrySize + 1}
	manifest := testEntry{name: helpers.ManifestFileName, content: []byte(testManifest)}

	// The positive control is the same stream with the over-declared entry
	// left out, so the refusal below is the size check answering rather than
	// the manifest sitting somewhere this walk never looks.
	control := writeArtifact(t, []testEntry{{name: "small.bin", content: []byte("x")}, manifest})
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(control without the over-declared entry) = %q, %v, want the manifest", got, err)
	}

	artifact := writeArtifact(t, []testEntry{oversize, manifest})
	if _, err := ReadFromTarGz(t.Context(), artifact); !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("ReadFromTarGz(over-declared entry ahead of the manifest) error = %v, want the entry-size sentinel", err)
	}
}

func TestReadFromTarGzRejectsScanOverrun(t *testing.T) {
	t.Parallel()

	// Both fixtures carry a real top-level MANIFEST.json BEHIND their padding,
	// which is what makes the refusal mean something: an archive that simply
	// ran out of entries would be refused by the end of the stream alone, so
	// the manifest behind the bound is the only thing separating "stopped at
	// the ceiling" from "read everything and found nothing".
	//
	// Loosening the ceiling - max: helpers.ManifestScanMaxBytes in
	// readFromTarGzStream replaced by max: helpers.ArchiveMaxDecompressedSize,
	// applied through go test -overlay so no production file is edited - lets
	// the walk reach that manifest and hand it back:
	//
	//	read_test.go:286: ReadFromTarGz(manifest past the scan bound) error = <nil>, want the not-found sentinel
	beyond := writePaddedArtifact(t, helpers.ManifestScanMaxBytes+(1<<20))
	if _, err := ReadFromTarGz(t.Context(), beyond); !errors.Is(err, helpers.ErrManifestNotFound) {
		t.Fatalf("ReadFromTarGz(manifest past the scan bound) error = %v, want the not-found sentinel", err)
	}

	// The control sits just under the same ceiling, so the refusal above is
	// the bound answering rather than padding this walk cannot read through.
	within := writePaddedArtifact(t, helpers.ManifestScanMaxBytes-(1<<20))
	got, err := ReadFromTarGz(t.Context(), within)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(manifest just inside the scan bound) = %q, %v, want the manifest", got, err)
	}
}

func TestReadFromTarGzRejectsNonGzip(t *testing.T) {
	t.Parallel()

	// The positive control proves a path handed to this function is read at
	// all, so the two refusals below are the gzip header answering rather than
	// anything about how the fixture reached disk.
	control := writeArtifact(t, []testEntry{{name: helpers.ManifestFileName, content: []byte(testManifest)}})
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(control gzipped artifact) = %q, %v, want the manifest", got, err)
	}

	text := writeFile(t, "error-page.html", []byte("<html><body>404 Not Found</body></html>"))
	if _, err := ReadFromTarGz(t.Context(), text); !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("ReadFromTarGz(an error page) error = %v, want the shape sentinel", err)
	}

	// A bare tar, never compressed: a well-formed archive of the right inner
	// shape that is still not what this function reads.
	bare := writeFile(t, "bare.tar", tarStream(t, []testEntry{{name: helpers.ManifestFileName, content: []byte(testManifest)}}))
	if _, err := ReadFromTarGz(t.Context(), bare); !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("ReadFromTarGz(an uncompressed tar) error = %v, want the shape sentinel", err)
	}
}

func TestReadFromTarGzRejectsEmptyManifest(t *testing.T) {
	t.Parallel()

	// A one-byte manifest is the positive control, and it is the tightest one
	// available: the entry sits at the same position under the same name, so
	// only its length separates it from the refusal below.
	control := writeArtifact(t, append(nonManifestEntries(),
		testEntry{name: helpers.ManifestFileName, content: []byte("{")}))
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != "{" {
		t.Fatalf("ReadFromTarGz(one-byte manifest) = %q, %v, want that one byte", got, err)
	}

	// A signature over a zero-byte document verifies whenever a keyring key
	// made it, so an empty manifest reaching a verifier is indistinguishable
	// from a real one. It is refused here, where the difference between an
	// empty document and a real one is still visible.
	//
	// Admitting it instead - `if size <= 0` in readManifestBody replaced by
	// `if size < 0`, applied through go test -overlay so no production file
	// is edited - fails on that refusal:
	//
	//	read_test.go:349: ReadFromTarGz(zero-length manifest) error = <nil>, want the not-found sentinel
	artifact := writeArtifact(t, append(nonManifestEntries(),
		testEntry{name: helpers.ManifestFileName}))
	if _, err := ReadFromTarGz(t.Context(), artifact); !errors.Is(err, helpers.ErrManifestNotFound) {
		t.Fatalf("ReadFromTarGz(zero-length manifest) error = %v, want the not-found sentinel", err)
	}
}

func TestReadFromTarGzNeverReportsAnEmptyManifest(t *testing.T) {
	t.Parallel()

	// A header declaring more than the archive carries, in the two shapes that
	// differ. Both are pinned because only one of them errors, and the contract
	// - a nil error never comes with no bytes - has to hold across both.
	const declared = 100
	body := []byte("0123456789")

	// The archive's own trailer sits behind the short body, so the read is
	// satisfied from it: exactly the declared count, padded with NUL.
	padded := writeArtifact(t, []testEntry{
		{name: helpers.ManifestFileName, content: body, declaredSize: declared},
	})
	got, err := ReadFromTarGz(t.Context(), padded)
	if err != nil || len(got) != declared {
		t.Fatalf("ReadFromTarGz(header over a short body) = %d bytes, %v, want %d bytes and no error", len(got), err, declared)
	}

	// The same header with the stream ending inside the body instead: nothing
	// follows it, not even a trailer, so archive/tar has nothing to satisfy the
	// declaration from.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	header := &tar.Header{Typeflag: tar.TypeReg, Name: helpers.ManifestFileName, Size: declared, Mode: 0o644}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("failed to write the truncated fixture's header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("failed to write the truncated fixture's body: %v", err)
	}

	var compressed bytes.Buffer
	gz := pgzip.NewWriter(&compressed)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatalf("failed to compress the truncated fixture: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close the truncated fixture's gzip writer: %v", err)
	}

	truncated := writeFile(t, "truncated.tar.gz", compressed.Bytes())
	got, err = ReadFromTarGz(t.Context(), truncated)
	if err == nil || len(got) != 0 {
		t.Fatalf("ReadFromTarGz(stream ending inside the manifest) = %d bytes, %v, want no bytes and an error", len(got), err)
	}
}

func TestReadFromTarGzRefusesAnArtifactItCannotOpen(t *testing.T) {
	t.Parallel()

	// The path is the run's own rather than anything read out of an archive, so
	// the failure this reports is an artifact that is not where it was said to
	// be - evicted from the cache, or never written. The control is the same
	// fixture at the path it really sits on.
	artifact := writeArtifact(t, []testEntry{{name: helpers.ManifestFileName, content: []byte(testManifest)}})
	got, err := ReadFromTarGz(t.Context(), artifact)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(the fixture where it sits) = %q, %v, want the manifest", got, err)
	}

	if _, err := ReadFromTarGz(t.Context(), artifact+".absent"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFromTarGz(a path carrying no artifact) error = %v, want a missing-file error", err)
	}
}

// errTestCeiling stands in for whichever verdict a caller injects, so the test
// below is about the reader rather than about either sentinel this package
// hands it.
var errTestCeiling = errors.New("test ceiling")

// countingReader answers every read with one byte and counts the calls it was
// handed, which is what tells a reader that stopped touching its source apart
// from one that merely keeps reporting the same refusal.
type countingReader struct {
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x'
	return 1, nil
}

func TestLimitReaderStaysRefusedPastItsCeiling(t *testing.T) {
	t.Parallel()

	// The source is endless, so nothing but the ceiling can end this. That is
	// also the positive control: the reads under the ceiling hand a byte back
	// each, so the refusal is the count crossing rather than the source
	// running out.
	//
	// What the stickiness is worth is visible in the source's call count and
	// nowhere else. A reader that recomputed the refusal on every call would
	// report the same verdict and still pull a byte out of the decompressor
	// for every call it was handed, and bytes out of the decompressor are what
	// this reader exists to count.
	//
	// Dropping it - `if r.err != nil` in limitReader.Read replaced by
	// `if false && r.err != nil`, applied through go test -overlay so no
	// production file is edited - fails here:
	//
	//	read_test.go:477: limitReader.Read past the ceiling = 0, test ceiling: read 4 bytes, limit is 2 bytes after 4 source reads, want 3
	source := &countingReader{}
	limited := &limitReader{r: source, over: errTestCeiling, max: 2}
	buf := make([]byte, 8)

	for read := range 2 {
		if n, err := limited.Read(buf); n != 1 || err != nil {
			t.Fatalf("limitReader.Read under the ceiling = %d, %v on read %d, want one byte and no error", n, err, read)
		}
	}

	n, crossing := limited.Read(buf)
	if n != 0 || !errors.Is(crossing, errTestCeiling) {
		t.Fatalf("limitReader.Read crossing the ceiling = %d, %v, want no bytes and the injected verdict", n, crossing)
	}
	crossed := source.reads

	n, again := limited.Read(buf)
	if n != 0 || !errors.Is(again, crossing) || source.reads != crossed {
		t.Fatalf("limitReader.Read past the ceiling = %d, %v after %d source reads, want %d", n, again, source.reads, crossed)
	}
}

func TestReadFromTarGzCapsAnEntryNameBeforeAnythingRendersIt(t *testing.T) {
	t.Parallel()

	// This walk retains no entry name and still has to measure one: the
	// over-declared-entry refusal quotes the name it refuses, and archive/tar
	// hands back a GNU long name of up to 1,048,575 bytes, so without the cap a
	// megabyte of archive-chosen text reaches an operator to report that the
	// entry carrying it declared too large a size.
	//
	// The five rows separate the two rules from each other and from the walk.
	// The first two accept: the fixture as it stands, then the same fixture
	// carrying an entry named at exactly the cap, which is the tightest control
	// available - one byte from the row below it. The third is that name one
	// byte over. The fourth declares an over-cap size under an ordinary name,
	// so the size arm is shown reachable and shown to quote what it refuses.
	// The fifth is that entry with a name past the cap: the name rule answers
	// first, under its own sentinel and in a message bounded whatever the name.
	//
	// Moving the check back - the checkEntryNameLength call in
	// readFromTarGzStream moved below the size refusal, applied through go test
	// -overlay so no production file is edited - fails the fifth row on the
	// message ceiling rather than on the sentinel, which is the whole point:
	//
	//	read_test.go:549: readFromTarGzStream(a name past the cap under an over-cap size) message is 200046 bytes, want under 300
	//
	// Rendering that name with %s rather than %q, applied the same way, fails
	// the fourth row instead. A bare name can carry a newline of its own, which
	// safeout.Clean passes through, so the quoting is what keeps an entry name
	// from forging a line of an operator's output:
	//
	//	read_test.go:549: readFromTarGzStream(an over-cap size under an ordinary name) message does not quote "huge.bin"
	const (
		longNameLen         = 200_000
		oversizeDeclaration = helpers.ArchiveMaxEntrySize + 1
	)
	cases := []nameCapCase{
		{name: "the fixture as it stands"},
		{name: "a name at the cap", entry: strings.Repeat("a", testEntryNameCap)},
		{
			name: "a name one byte over the cap", entry: strings.Repeat("a", testEntryNameCap+1),
			want: helpers.ErrArchiveEntryNameTooLong,
		},
		{
			name: "an over-cap size under an ordinary name", entry: "huge.bin", size: oversizeDeclaration,
			want: helpers.ErrArchiveEntryIsTooLarge, quotes: `"huge.bin"`,
		},
		{
			name: "a name past the cap under an over-cap size", size: oversizeDeclaration,
			entry: strings.Repeat("a", longNameLen), want: helpers.ErrArchiveEntryNameTooLong,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := nonManifestEntries()
			if tc.entry != "" {
				entries = append(entries, testEntry{name: tc.entry, declaredSize: tc.size})
			}
			entries = append(entries, testEntry{name: helpers.ManifestFileName, content: []byte(testManifest)})

			got, err := artifactStream(t, entries)
			if tc.want == nil {
				if err != nil || string(got) != testManifest {
					t.Fatalf("readFromTarGzStream(%s) = %q, %v, want the manifest", tc.name, got, err)
				}
				return
			}
			checkNameCapRefusal(t, tc, err)
		})
	}
}

// nameCapCase is one row of the table above: an entry to put ahead of the
// manifest, and what the walk owes for it. A zero want means the row is one of
// the accepting ones, and quotes names a fragment the refusal has to render.
type nameCapCase struct {
	want   error
	name   string
	entry  string
	quotes string
	size   int64
}

// checkNameCapRefusal asserts what one refusing row owes: a message an
// operator can read, the sentinel it names, and the fragment it has to quote.
//
// The message ceiling is checked ahead of the sentinel deliberately, and not
// because the sentinel needs the help: run with this ceiling disabled - the
// `n > messageMax` condition below becoming `n > messageMax && false` - the
// ordering mutation the table above describes still fails the fifth row on the
// sentinel alone, the size arm raising helpers.ErrArchiveEntryIsTooLarge where
// that row wants the name one. The order is about what failing on the sentinel
// COSTS, since that assertion renders err with %v:
//
//	read_test.go:549: readFromTarGzStream(a name past the cap under an over-cap size) error = archive entry is too large "aaaa
//
// elided after four of the 200,000 a's, which is the whole of what checking the
// ceiling first keeps out of a failure log.
func checkNameCapRefusal(t *testing.T, tc nameCapCase, err error) {
	t.Helper()

	const messageMax = 300
	if err == nil {
		t.Fatalf("readFromTarGzStream(%s) error = <nil>, want %v", tc.name, tc.want)
	}
	if n := len(err.Error()); n > messageMax {
		t.Fatalf("readFromTarGzStream(%s) message is %d bytes, want under %d", tc.name, n, messageMax)
	}
	if !errors.Is(err, tc.want) {
		t.Fatalf("readFromTarGzStream(%s) error = %v, want %v", tc.name, err, tc.want)
	}
	if tc.quotes != "" && !strings.Contains(err.Error(), tc.quotes) {
		t.Fatalf("readFromTarGzStream(%s) message does not quote %s", tc.name, tc.quotes)
	}
}

// artifactStream runs the walk over a fixture without going through
// ReadFromTarGz, so an assertion about the length of a refusal measures what
// the walk renders rather than the artifact path ReadFromTarGz prefixes on.
func artifactStream(tb testing.TB, entries []testEntry) ([]byte, error) {
	tb.Helper()

	raw, err := os.ReadFile(writeArtifact(tb, entries))
	if err != nil {
		tb.Fatalf("failed to read the fixture back: %v", err)
	}
	return readFromTarGzStream(tb.Context(), bytes.NewReader(raw))
}

// TestReadFromTarGzRefusesAGzipMemberProducingNoBytes pins the one refusal
// decompressorOpenError names as arriving from somewhere else: a member whose
// gzip HEADER is well-formed, so the decompressor opens over it and the
// verdict comes from the walk behind it instead of from the open.
//
// The positive control is TestReadFromTarGzRejectsNonGzip's, which reads a
// well-formed artifact back through this same ReadFromTarGz path out of a file
// the same writeFile builder wrote. This fixture differs from it in what the
// member carries and in nothing else.
func TestReadFromTarGzRefusesAGzipMemberProducingNoBytes(t *testing.T) {
	t.Parallel()

	// Twenty bytes that are a whole gzip member: the ten-byte header, one
	// fixed-Huffman final block carrying nothing, and an eight-byte trailer
	// over zero bytes. Hand-spelled rather than produced by a writer, which
	// pays 23 bytes for the same nothing - twenty is what one member of a
	// flood costs its author on the wire.
	member := make([]byte, 0, 20)
	member = append(member, "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff"...)
	member = append(member, 0x03, 0x00)
	member = append(member, 0, 0, 0, 0, 0, 0, 0, 0)

	_, err := ReadFromTarGz(t.Context(), writeFile(t, "empty.tar.gz", member))

	// Dropping the member rule - `if r.n == 0` in gzipstream.Reader.Read
	// becoming `if false && r.n == 0`, applied through go test -overlay so no
	// production file is edited - lets the Reset behind it run the stream out,
	// leaving the walk to report an artifact that carried nothing:
	//
	//	read_test.go:646: ReadFromTarGz(an empty gzip member) error = .../empty.tar.gz: collection
	//	artifact contains no MANIFEST.json: the tar stream ended after 0 bytes, want the empty-member sentinel
	//
	// Both quotes here carry the test's own temp directory elided to .../ and
	// are wrapped, since neither fits this file's line width whole.
	if !errors.Is(err, helpers.ErrEmptyGzipMember) {
		t.Fatalf("ReadFromTarGz(an empty gzip member) error = %v, want the empty-member sentinel", err)
	}

	// And that verdict is the walk's rather than the open's, which is the whole
	// of what this fixture separates. Rendering it as the open's - walkError
	// replaced by decompressorOpenError at readFromTarGzStream's own
	// tarReader.Next error arm, applied through an overlay too - leaves the
	// sentinel above readable and puts the shape one on it as well:
	//
	//	read_test.go:658: ReadFromTarGz(an empty gzip member) error = .../empty.tar.gz: downloaded
	//	artifact is not a gzip-compressed tar archive: gzip stream carries a member that produces no bytes, want no shape sentinel
	if errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("ReadFromTarGz(an empty gzip member) error = %v, want no shape sentinel", err)
	}
}
