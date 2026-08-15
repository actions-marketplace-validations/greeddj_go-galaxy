package manifest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// The vocabulary of these documents, hand-spelled rather than taken from the
// production constants the reader compares against. A fixture built from the
// same constant it is meant to pin follows that constant when it is mutated,
// which turns the assertion into a tautology; spelling the strings here means a
// change to either name or to the accepted checksum type fails these tests
// instead of moving with them.
const (
	testManifestName     = "MANIFEST.json"
	testFilesName        = "FILES.json"
	testFtypeFile        = "file"
	testFtypeDir         = "dir"
	testChksumSHA256     = "sha256"
	testEntryNameCap     = 1024
	testFilesManifestCap = 32 << 20
)

// testOtherDigest is a well-formed sha256 that is not the digest of anything a
// fixture below carries, so a row naming it is wrong about content rather than
// malformed.
const testOtherDigest = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

// chainEntry is one tar entry of a chain fixture together with what FILES.json
// is to say about it. The zero value describes a regular file, listed with its
// own true digest - so a test states only what it changes.
type chainEntry struct {
	// name is the entry's archive path.
	name string
	// linkname is a link entry's target, written verbatim: a symlink's is read
	// relative to the entry's own directory, a hardlink's to the archive root.
	linkname string
	// listAs overrides the ftype the listing row carries. Empty means "dir" for
	// a directory entry and "file" for everything else.
	listAs string
	// listDigest overrides the digest the listing row names.
	listDigest string
	// listChksumType overrides the checksum type the listing row names.
	listChksumType string
	// content is the body FILES.json's digest is computed from.
	content []byte
	// archivedContent is what the tar stream carries instead of content, for a
	// fixture whose listing and archive have to disagree.
	archivedContent []byte
	// declaredSize overrides the size the entry's header announces.
	declaredSize int64
	// typeflag defaults to tar.TypeReg, which is not its own zero value.
	typeflag byte
	// unlisted keeps the entry out of FILES.json.
	unlisted bool
	// unarchived keeps the entry out of the tar stream, leaving its listing row
	// in place.
	unarchived bool
}

// chainSpec describes one fixture. Everything left unset produces a well-formed
// artifact, which is what makes the positive-control rule structural here
// rather than a per-test obligation: TestVerifyChainAcceptsWellFormedArtifact
// proves the default is accepted, and every refusal below is that default with
// exactly one field changed.
type chainSpec struct {
	// mutateFiles rewrites FILES.json after it is rendered and BEFORE the
	// pointer is computed from it, so a fixture corrupting the listing is
	// answered by the listing's own rules rather than by the pointer
	// disagreeing with it first.
	mutateFiles func([]byte) []byte
	// mutateManifest rewrites MANIFEST.json after the pointer has been written
	// into it. The archive carries the rewritten bytes and the builder returns
	// them, so the seam between the signed document and the archive still
	// holds and only what the pointer says is at fault.
	mutateManifest func([]byte) []byte
	// filesLinkname is the FILES.json entry's link target, for the typeflag
	// below.
	filesLinkname string
	// pointerDigest overrides the digest MANIFEST.json names for FILES.json.
	// It is what lets a fixture carry a pointer that PARSES and is wrong,
	// which no other knob here produces: rewriting the rendered manifest
	// instead can only ever change the value's shape, and a pointer refused
	// for its shape never reaches the comparison against the archive.
	pointerDigest string
	entries       []chainEntry
	// filesDeclaredSize overrides the size the FILES.json header announces.
	filesDeclaredSize int64
	// filesTypeflag overrides the typeflag of the FILES.json entry itself.
	filesTypeflag byte
	// omitFilesManifest leaves FILES.json out of the tar stream entirely.
	omitFilesManifest bool
	// omitRootRow drops the "." row a listing otherwise always carries, for
	// the one fixture that needs "." to be an unoccupied key.
	omitRootRow bool
}

// listingRow is one rendered row of FILES.json. The two checksum fields are
// pointers so that a non-file row renders them as JSON null, which is the shape
// a directory row takes in these documents and which the reader's own
// string-typed field has to decode to the empty string.
type listingRow struct {
	ChksumType   *string `json:"chksum_type"`
	ChksumSha256 *string `json:"chksum_sha256"`
	Name         string  `json:"name"`
	Ftype        string  `json:"ftype"`
}

// chainEntries is the content a well-formed fixture carries: a regular file, a
// directory, and a file nested under one.
func chainEntries() []chainEntry {
	return []chainEntry{
		{name: "README.md", content: []byte("# acme.widgets\n")},
		{name: "plugins", typeflag: tar.TypeDir},
		{name: "plugins/modules/widget.py", content: []byte("# widget\n")},
	}
}

// buildChainArtifact writes one fixture to disk and returns its path together
// with the MANIFEST.json bytes a signature would have been verified over -
// which are the same bytes the archive's own MANIFEST.json entry carries,
// unless a test deliberately hands VerifyChain something else.
func buildChainArtifact(tb testing.TB, spec chainSpec) (string, []byte) {
	tb.Helper()

	filesJSON := renderFilesListing(tb, spec)
	if spec.mutateFiles != nil {
		filesJSON = spec.mutateFiles(filesJSON)
	}
	pointer := hexDigest(filesJSON)
	if spec.pointerDigest != "" {
		pointer = spec.pointerDigest
	}
	manifestJSON := renderChainManifest(tb, pointer)
	if spec.mutateManifest != nil {
		manifestJSON = spec.mutateManifest(manifestJSON)
	}

	stream := []testEntry{{name: testManifestName, content: manifestJSON}}
	if !spec.omitFilesManifest {
		stream = append(stream, filesManifestEntry(spec, filesJSON))
	}
	for _, e := range spec.entries {
		if e.unarchived {
			continue
		}
		body := e.content
		if e.archivedContent != nil {
			body = e.archivedContent
		}
		stream = append(stream, testEntry{
			name: e.name, linkname: e.linkname, content: body,
			declaredSize: e.declaredSize, typeflag: e.typeflag,
		})
	}
	return writeArtifact(tb, stream), manifestJSON
}

// filesManifestEntry renders the archive's FILES.json entry, which carries the
// listing's bytes unless the fixture asked for some other typeflag - a link
// entry cannot carry a body at all.
func filesManifestEntry(spec chainSpec, filesJSON []byte) testEntry {
	if spec.filesTypeflag != 0 {
		return testEntry{name: testFilesName, linkname: spec.filesLinkname, typeflag: spec.filesTypeflag}
	}
	return testEntry{name: testFilesName, content: filesJSON, declaredSize: spec.filesDeclaredSize}
}

// renderFilesListing builds the listing a well-formed collection would carry:
// the "." row naming the collection root, then one row per listed entry.
func renderFilesListing(tb testing.TB, spec chainSpec) []byte {
	tb.Helper()

	entries := spec.entries
	digests := chainDigests(entries)
	rows := make([]listingRow, 0, len(entries)+1)
	if !spec.omitRootRow {
		rows = append(rows, listingRow{Name: ".", Ftype: testFtypeDir})
	}
	for _, e := range entries {
		if e.unlisted {
			continue
		}
		rows = append(rows, e.listingRow(digests))
	}

	body, err := json.Marshal(struct {
		Files []listingRow `json:"files"`
	}{Files: rows})
	if err != nil {
		tb.Fatalf("failed to render the fixture's %s: %v", testFilesName, err)
	}
	return body
}

// listingRow renders one entry's row, taking its digest from what the archive
// really carries unless the fixture overrode it.
func (e chainEntry) listingRow(digests map[string]string) listingRow {
	row := listingRow{Name: e.name, Ftype: e.listingFtype()}
	if row.Ftype != testFtypeFile {
		return row
	}
	chksumType := e.listChksumType
	if chksumType == "" {
		chksumType = testChksumSHA256
	}
	digest := e.listDigest
	if digest == "" {
		digest = digests[path.Clean(e.name)]
	}
	row.ChksumType = &chksumType
	row.ChksumSha256 = &digest
	return row
}

func (e chainEntry) listingFtype() string {
	if e.listAs != "" {
		return e.listAs
	}
	if e.typeflagOrDefault() == tar.TypeDir {
		return testFtypeDir
	}
	return testFtypeFile
}

func (e chainEntry) typeflagOrDefault() byte {
	if e.typeflag == 0 {
		return tar.TypeReg
	}
	return e.typeflag
}

// chainDigests returns the digest a truthful FILES.json would name for every
// entry, resolving a link to the content of what it points at.
//
// The resolution is written independently of the reader under test rather than
// by calling into it: it is the fixture's own statement of what a link means,
// so a reader that resolved links differently would produce digests that
// disagree with these. An entry whose target the fixture does not carry gets no
// digest, which is how a fixture asks the reader about a dangling link.
func chainDigests(entries []chainEntry) map[string]string {
	contents := make(map[string][]byte)
	links := make(map[string]string)
	for _, e := range entries {
		key := path.Clean(e.name)
		switch e.typeflagOrDefault() {
		case tar.TypeReg:
			contents[key] = e.content
		case tar.TypeSymlink:
			links[key] = path.Join(path.Dir(key), e.linkname)
		case tar.TypeLink:
			links[key] = path.Clean(e.linkname)
		}
	}

	out := make(map[string]string, len(contents)+len(links))
	for key, body := range contents {
		out[key] = hexDigest(body)
	}
	for key := range links {
		if body, ok := followFixtureLink(contents, links, key); ok {
			out[key] = hexDigest(body)
		}
	}
	return out
}

// followFixtureLink walks the fixture's own link map to the content it names,
// stopping well past any chain a test builds so a cycle cannot hang the suite.
func followFixtureLink(contents map[string][]byte, links map[string]string, key string) ([]byte, bool) {
	for range len(links) + 1 {
		if body, ok := contents[key]; ok {
			return body, true
		}
		next, ok := links[key]
		if !ok {
			return nil, false
		}
		key = next
	}
	return nil, false
}

// renderChainManifest builds the signed document, pointing at a listing with
// the given digest.
func renderChainManifest(tb testing.TB, filesDigest string) []byte {
	tb.Helper()

	body, err := json.Marshal(map[string]any{
		"collection_info": map[string]string{"namespace": "acme", "name": "widgets", "version": "1.0.0"},
		"file_manifest_file": map[string]string{
			"name":          testFilesName,
			"ftype":         testFtypeFile,
			"chksum_type":   testChksumSHA256,
			"chksum_sha256": filesDigest,
		},
	})
	if err != nil {
		tb.Fatalf("failed to render the fixture's %s: %v", testManifestName, err)
	}
	return body
}

func hexDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// replaceOnce returns a document hook that rewrites the first occurrence of
// needle into replacement, failing the test when the needle is absent - so a
// fixture can never quietly stop corrupting the thing it was written to
// corrupt.
func replaceOnce(t *testing.T, needle, replacement string) func([]byte) []byte {
	t.Helper()

	return func(body []byte) []byte {
		if !bytes.Contains(body, []byte(needle)) {
			t.Fatalf("the rendered document does not contain %q", needle)
		}
		return bytes.Replace(body, []byte(needle), []byte(replacement), 1)
	}
}

// verifyChainFixture builds a fixture and runs the check over it.
func verifyChainFixture(t *testing.T, spec chainSpec) error {
	t.Helper()

	artifact, manifestJSON := buildChainArtifact(t, spec)
	return VerifyChain(t.Context(), artifact, manifestJSON)
}

func TestVerifyChainAcceptsWellFormedArtifact(t *testing.T) {
	t.Parallel()

	// The positive control every refusal below rests on: the builder's default
	// output, with its "." directory row and the null checksums such a row
	// carries, is accepted. Written first, because a refusal derived from a
	// fixture nothing has shown to be acceptable proves nothing.
	if err := verifyChainFixture(t, chainSpec{entries: chainEntries()}); err != nil {
		t.Fatalf("VerifyChain(well-formed artifact) error = %v, want no error", err)
	}
}

func TestVerifyChainRejectsMismatchedFilesManifestDigest(t *testing.T) {
	t.Parallel()

	// The second link of the chain: the pointer inside the signed document
	// against the listing the archive really carries. The pointer has to be
	// well-formed for this to be the link under test - a value refused for its
	// shape never reaches the comparison at all, and neither does one that
	// makes MANIFEST.json stop parsing - so the fixture names a real sha256
	// that is simply not this listing's. Only the manifest moves; the archive's
	// own copy of it is the same bytes, so the seam check ahead of this one
	// still passes.
	//
	// Dropping the comparison - `if gotFiles != wantFiles` in verifyChainScan
	// replaced by `if false && gotFiles != wantFiles`, applied through go test
	// -overlay so no production file is edited - fails here:
	//
	//	chain_test.go:377: VerifyChain(pointer naming another digest) error = <nil>, want the chain sentinel
	spec := chainSpec{entries: chainEntries(), pointerDigest: testOtherDigest}
	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(pointer naming another digest) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsModifiedListedFile(t *testing.T) {
	t.Parallel()

	// The third link: a listed file's own bytes. The listing names the digest
	// of content, the archive carries archivedContent, and nothing else in the
	// fixture moves.
	entries := chainEntries()
	entries[0].archivedContent = []byte("# tampered\n")

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listed file with other bytes) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsMissingListedFile(t *testing.T) {
	t.Parallel()

	entries := chainEntries()
	entries[2].unarchived = true

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listed file absent from the archive) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsUnlistedArchiveEntry(t *testing.T) {
	t.Parallel()

	// This is the injection the reverse rule exists for, and the only one of
	// the three links that a signature plus a matching listing does not already
	// cover: MANIFEST.json is authentic, FILES.json matches the digest it
	// names, every file FILES.json lists matches its own digest, and the
	// archive still carries a file nobody vouched for. An installer that
	// unpacked only the listed paths would never write such an entry; this
	// project's extractor unpacks the whole archive, so without this rule the
	// injected file lands in the installed tree.
	//
	// Dropping the rule - return checkUnlisted(scan, listed) in
	// verifyChainScan replaced by `_ = listed` and a bare return nil, applied
	// through go test -overlay so no production file is edited - fails here:
	//
	//	chain_test.go:431: VerifyChain(archive carrying an unlisted file) error = <nil>, want the chain sentinel
	entries := append(chainEntries(), chainEntry{
		name: "plugins/modules/backdoor.py", content: []byte("# injected\n"), unlisted: true,
	})

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(archive carrying an unlisted file) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsEmptyListingWithContent(t *testing.T) {
	t.Parallel()

	// An empty listing is the degenerate case of the same rule, and it is worth
	// its own test because the forward walk has nothing to say about it: no row
	// means no digest to compare, so a reader missing the reverse direction
	// accepts an archive of arbitrary content against an empty document that
	// MANIFEST.json quite correctly vouches for.
	//
	// The same mutation as above - return checkUnlisted(scan, listed) in
	// verifyChainScan replaced by `_ = listed` and a bare return nil, applied
	// through go test -overlay so no production file is edited - fails here on
	// this test's own assertion:
	//
	//	chain_test.go:458: VerifyChain(empty listing over a non-empty archive) error = <nil>, want the chain sentinel
	spec := chainSpec{
		entries: chainEntries(),
		mutateFiles: func([]byte) []byte {
			return []byte(`{"files":[]}`)
		},
	}
	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(empty listing over a non-empty archive) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsNonGzip(t *testing.T) {
	t.Parallel()

	// The manifest is well-formed and its pointer parses, so the refusal is the
	// gzip header answering rather than anything read out of the document. The
	// bytes are what a proxy or a login page delivers in an artifact's place.
	_, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})

	page := writeFile(t, "error-page.html", []byte("<html><body>404 Not Found</body></html>"))
	if err := VerifyChain(t.Context(), page, manifestJSON); !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("VerifyChain(an error page) error = %v, want the shape sentinel", err)
	}
}

func TestVerifyChainRejectsUnparseableListing(t *testing.T) {
	t.Parallel()

	// The listing is rewritten before the pointer is computed from it, so
	// MANIFEST.json vouches for exactly these bytes and every link ahead of the
	// decode passes. What is left is a document that authenticates and does not
	// parse, which has to be a refusal rather than an empty listing.
	//
	// The archive deliberately carries nothing but the two documents. With any
	// other entry in it, a reader that ignored the decode error would be caught
	// by the reverse rule instead - it would see an empty listing and an
	// archive carrying content - and this rule would look pinned when it was
	// not. Both remaining entries are exempt from that rule, so nothing else
	// can answer here.
	spec := chainSpec{
		mutateFiles: func([]byte) []byte {
			return []byte(`{"files":[{"name":`)
		},
	}
	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listing that does not parse) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainExemptsAListedFilesManifestButNotAListedManifest(t *testing.T) {
	t.Parallel()

	// The asymmetry between the two exempt names, in the forward direction. A
	// listing cannot state its own digest, so a FILES.json row is skipped and a
	// wrong one accepted; MANIFEST.json gets no such skip, so a row for it is
	// checked and can only ever be refused. Both rows carry the same wrong
	// digest, so what separates them is the rule and nothing else.
	//
	// The skip is exactly what the accepting row exercises: with it gone, that
	// row's digest would be resolved against the archive and refused, since a
	// listing cannot state its own. No mutation output is quoted for it, since
	// a refusal from VerifyChain renders the artifact's own temp path and no
	// two runs would produce the same line to quote.
	//
	// The refusing row does carry one, and it pins the asymmetry rather than
	// the skip: widening the skip to cover both names - the condition in
	// checkListedRow becoming `key == helpers.FilesManifestFileName ||
	// key == helpers.ManifestFileName`, applied through go test -overlay so no
	// production file is edited - fails here:
	//
	//	chain_test.go:543: VerifyChain(listing carrying a row for MANIFEST.json) error = <nil>, want the chain sentinel
	cases := []struct {
		name    string
		listed  string
		wantErr bool
	}{
		{name: "a row for FILES.json", listed: testFilesName},
		{name: "a row for MANIFEST.json", listed: testManifestName, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			row := listingFileRowJSON(tc.listed, testOtherDigest)
			spec := chainSpec{
				entries:     chainEntries(),
				mutateFiles: replaceOnce(t, `{"files":[`, `{"files":[`+row+`,`),
			}

			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(listing carrying %s) error = %v, want the chain sentinel", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(listing carrying %s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

// listingFileRowJSON renders one file row exactly as json.Marshal renders
// listingRow, so a test can splice a row the builder would never produce into
// an otherwise well-formed listing.
func listingFileRowJSON(name, digest string) string {
	return `{"chksum_type":"` + testChksumSHA256 + `","chksum_sha256":"` + digest +
		`","name":"` + name + `","ftype":"` + testFtypeFile + `"}`
}

func TestVerifyChainRejectsUnsafeListedName(t *testing.T) {
	t.Parallel()

	// FILES.json is attacker-chosen the moment MANIFEST.json is, so a name in
	// it is judged like a tar entry's own - as the listing's fault rather than
	// the archive's, since the archive never carried such a path.
	//
	// Every row is a DIRECTORY row, and that is what makes them pin the rule
	// rather than merely exercise it: a file row naming an unsafe path would be
	// refused a moment later anyway for naming nothing the archive carries,
	// under the same sentinel. A directory row is never resolved against the
	// archive at all, so with this refusal gone it would simply be recorded as
	// listed and the artifact accepted.
	//
	// Removing it - listedName's absolute/escape refusal replaced by a bare
	// return of the cleaned name, applied through go test -overlay so no
	// production file is edited - fails here:
	//
	//	chain_test.go:602: VerifyChain(listing naming "/etc/passwd") error = <nil>, want the chain sentinel
	cases := []struct {
		name        string
		listed      string
		omitRootRow bool
	}{
		{name: "absolute", listed: "/etc/passwd"},
		{name: "escaping", listed: "../../etc/passwd"},
		// An empty name cleans to ".", the key the listing's own root row
		// already occupies - so with this refusal gone the duplicate-row rule
		// would answer instead and hide the change. Dropping the root row
		// leaves this rule as the only one that can speak.
		{name: "empty", listed: "", omitRootRow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := append(chainEntries(), chainEntry{
				name: tc.listed, listAs: testFtypeDir, unarchived: true,
			})

			err := verifyChainFixture(t, chainSpec{entries: entries, omitRootRow: tc.omitRootRow})
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(listing naming %q) error = %v, want the chain sentinel", tc.listed, err)
			}
		})
	}
}

func TestVerifyChainRejectsUnsafeArchiveEntryPath(t *testing.T) {
	t.Parallel()

	// The same three shapes on the other document, judged by the archive's own
	// sentinels. Each entry is left out of the listing so that the scan is the
	// only thing that can answer, and each row asserts its own sentinel, since
	// an entry that survived its rule would still be refused by the reverse
	// rule under a different one.
	cases := []struct {
		want error
		name string
		path string
	}{
		{helpers.ErrArchiveEntryIsAbsolutePath, "absolute", "/etc/passwd"},
		{helpers.ErrArchiveEntryEscapesDestination, "escaping", "../evil.txt"},
		{helpers.ErrArchiveEntryHasEmptyName, "empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := append(chainEntries(), chainEntry{name: tc.path, unlisted: true})

			err := verifyChainFixture(t, chainSpec{entries: entries})
			if !errors.Is(err, tc.want) {
				t.Fatalf("VerifyChain(archive entry named %q) error = %v, want %v", tc.path, err, tc.want)
			}
		})
	}
}

func TestChargeEntrySizeRefusesOutOfBudgetEntries(t *testing.T) {
	t.Parallel()

	// Driven straight at chargeEntrySize, which is package-local and therefore
	// reachable from an in-package test, because two of its three arms cannot
	// be reached from any artifact a fixture can build: tar.Writer will not
	// emit a negative size, and the per-archive total is 4 GiB, which a fixture
	// would have to declare across entries whose bodies the scan then reads
	// past. TestVerifyChainChargesEveryEntryHeader below covers the call site,
	// so deleting either the function or the call to it is still caught.
	cases := []struct {
		want     error
		name     string
		size     int64
		declared int64
		wantSum  int64
	}{
		{name: "an ordinary entry is added to the total", size: 100, declared: 40, wantSum: 140},
		{name: "an entry at the per-entry cap", size: helpers.ArchiveMaxEntrySize, wantSum: helpers.ArchiveMaxEntrySize},
		{
			name: "an entry filling the per-archive total exactly", size: helpers.ArchiveMaxEntrySize,
			declared: helpers.ArchiveMaxTotalSize - helpers.ArchiveMaxEntrySize, wantSum: helpers.ArchiveMaxTotalSize,
		},
		{name: "a negative size", size: -1, want: helpers.ErrArchiveEntryHasNegativeSize},
		{
			name: "one byte past the per-entry cap", size: helpers.ArchiveMaxEntrySize + 1,
			want: helpers.ErrArchiveEntryIsTooLarge,
		},
		{
			name: "one byte past the per-archive total", size: helpers.ArchiveMaxEntrySize,
			declared: helpers.ArchiveMaxTotalSize - helpers.ArchiveMaxEntrySize + 1,
			want:     helpers.ErrArchiveExceedsMaxSize,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			declared := tc.declared
			err := chargeEntrySize(&tar.Header{Typeflag: tar.TypeReg, Name: "e.bin", Size: tc.size}, &declared)
			checkChargeOutcome(t, tc.want, err, declared, tc.declared, tc.wantSum)
		})
	}
}

// checkChargeOutcome asserts one chargeEntrySize outcome: the sentinel it
// returned, and what it left the running total at. The running total is checked
// on both arms because it is the half a sentinel says nothing about - a charge
// applied before its own refusal, or not applied at all, returns exactly the
// error the row expects.
func checkChargeOutcome(t *testing.T, want, got error, declared, before, wantSum int64) {
	t.Helper()

	if want == nil {
		if got != nil {
			t.Fatalf("chargeEntrySize onto %d error = %v, want no error", before, got)
		}
		if declared != wantSum {
			t.Fatalf("chargeEntrySize onto %d left the total at %d, want %d", before, declared, wantSum)
		}
		return
	}
	if !errors.Is(got, want) {
		t.Fatalf("chargeEntrySize onto %d error = %v, want %v", before, got, want)
	}
	if declared != before {
		t.Fatalf("chargeEntrySize onto %d refused and still moved the total to %d", before, declared)
	}
}

func TestVerifyChainChargesEveryEntryHeader(t *testing.T) {
	t.Parallel()

	// The call site the table above cannot prove exists. One ordinary entry
	// declares a byte past the per-entry cap and carries nothing, so the
	// refusal has to come off the header - there is no body to read. The
	// fixture is otherwise the default one, which is accepted.
	entries := append(chainEntries(), chainEntry{
		name: "huge.bin", declaredSize: helpers.ArchiveMaxEntrySize + 1, unlisted: true,
	})

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("VerifyChain(entry declaring past the per-entry cap) error = %v, want the entry-size sentinel", err)
	}
}

func TestVerifyChainAcceptsSymlinkHashedAsItsTarget(t *testing.T) {
	t.Parallel()

	// Both shapes a symlink takes inside a collection: one beside its target,
	// one reaching up out of its own directory. The listing names each link the
	// digest of the target's content, which is what the reference reader hands
	// back for a link member and what go-galaxy's extractor materializes.
	entries := []chainEntry{
		{name: "docs/real.md", content: []byte("real\n")},
		{name: "docs/alias.md", linkname: "real.md", typeflag: tar.TypeSymlink},
		{name: "meta/alias.md", linkname: "../docs/real.md", typeflag: tar.TypeSymlink},
	}

	if err := verifyChainFixture(t, chainSpec{entries: entries}); err != nil {
		t.Fatalf("VerifyChain(symlinks hashed as their targets) error = %v, want no error", err)
	}
}

func TestVerifyChainRejectsSymlinkWithMismatchedTargetDigest(t *testing.T) {
	t.Parallel()

	// The negative of the test above, on the identical fixture with one field
	// changed: the link is listed against a digest that is not its target's.
	entries := []chainEntry{
		{name: "docs/real.md", content: []byte("real\n")},
		{name: "docs/alias.md", linkname: "real.md", typeflag: tar.TypeSymlink, listDigest: testOtherDigest},
	}

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(symlink listed against another digest) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRefusesUnsafeSymlinkTargets(t *testing.T) {
	t.Parallel()

	// Each row asserts its own sentinel rather than merely "some refusal",
	// which is what makes them separable: an unsafe target that slipped past
	// its own rule would still be refused later for naming nothing the archive
	// carries, under the chain sentinel, so a test satisfied by any error at
	// all would pass against every one of these rules removed.
	cases := []struct {
		want     error
		name     string
		linkname string
	}{
		{nil, "in-archive target", "real.md"},
		{helpers.ErrSymlinkTargetIsEmpty, "empty target", ""},
		{helpers.ErrSymlinkTargetEscapesDestination, "escaping target", "../../../etc/passwd"},
		// One directory up from an entry in docs/ resolves to the archive root,
		// which names no file - a shape the escape test only catches because it
		// checks for "." as well as for a leading "..".
		{helpers.ErrSymlinkTargetEscapesDestination, "target resolving to the root", ".."},
		// The absolute row is what pins the order of the two checks. Tested
		// after path.Join instead, "/etc/passwd" would already have been
		// rewritten to "docs/etc/passwd", which is neither absolute nor
		// escaping - so the entry would be recorded as an ordinary link and the
		// refusal, if any, would arrive much later under a different sentinel.
		{helpers.ErrSymlinkTargetIsAbsolute, "absolute target", "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := []chainEntry{
				{name: "docs/real.md", content: []byte("real\n")},
				{name: "docs/alias.md", linkname: tc.linkname, typeflag: tar.TypeSymlink, listDigest: testOtherDigest},
			}
			// The accepting row names its target truthfully, so the digest
			// override above has to be dropped for it.
			if tc.want == nil {
				entries[1].listDigest = ""
			}

			err := verifyChainFixture(t, chainSpec{entries: entries})
			if tc.want == nil && err != nil {
				t.Fatalf("VerifyChain(symlink to %q) error = %v, want no error", tc.linkname, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("VerifyChain(symlink to %q) error = %v, want %v", tc.linkname, err, tc.want)
			}
		})
	}
}

func TestVerifyChainAcceptsHardlinkHashedAsItsTarget(t *testing.T) {
	t.Parallel()

	// A hardlink's target is read against the archive root, not against the
	// entry's own directory, which is the one place the two link typeflags part
	// company. The first two rows pin that base from opposite sides: the
	// accepting target only resolves when it is read from the root, and the
	// refused one only resolves when it is read from the entry's directory. The
	// last two are the shapes that name no file at all.
	cases := []struct {
		want     error
		name     string
		linkname string
	}{
		{nil, "root-relative target", "docs/real.md"},
		{helpers.ErrArchiveEntryEscapesDestination, "directory-relative target", "../docs/real.md"},
		{helpers.ErrHardlinkTargetIsEmpty, "empty target", ""},
		{helpers.ErrHardlinkTargetIsEmpty, "target naming the archive root", "."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := []chainEntry{
				{name: "docs/real.md", content: []byte("real\n")},
				{name: "meta/hard.md", linkname: tc.linkname, typeflag: tar.TypeLink},
			}
			if tc.want != nil {
				entries[1].listDigest = testOtherDigest
			}

			err := verifyChainFixture(t, chainSpec{entries: entries})
			if tc.want == nil && err != nil {
				t.Fatalf("VerifyChain(hardlink to %q) error = %v, want no error", tc.linkname, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("VerifyChain(hardlink to %q) error = %v, want %v", tc.linkname, err, tc.want)
			}
		})
	}
}

func TestVerifyChainAcceptsUnlistedDirectories(t *testing.T) {
	t.Parallel()

	// A directory carries no content to digest, so it is neither hashed nor
	// held against the reverse rule - and both ways a builder can record one
	// have to be accepted, since a listing may name a directory with null
	// checksums and nothing obliges it to name every directory it ships.
	entries := []chainEntry{
		{name: "roles", typeflag: tar.TypeDir},
		{name: "plugins", typeflag: tar.TypeDir, unlisted: true},
		{name: "plugins/widget.py", content: []byte("# widget\n")},
	}

	if err := verifyChainFixture(t, chainSpec{entries: entries}); err != nil {
		t.Fatalf("VerifyChain(listed and unlisted directories) error = %v, want no error", err)
	}
}

func TestVerifyChainAcceptsSymlinkToDirectoryListedAsDir(t *testing.T) {
	t.Parallel()

	// A symlink pointing at a directory is recorded with ftype dir while
	// arriving as a symlink entry in the tar. So "listed" has to mean "appears
	// in files at all": a reverse rule built from the file rows alone would
	// refuse this collection, which is a legitimate one.
	entries := []chainEntry{
		{name: "plugins/modules", typeflag: tar.TypeDir},
		{name: "plugins/alias", linkname: "modules", typeflag: tar.TypeSymlink, listAs: testFtypeDir},
	}

	if err := verifyChainFixture(t, chainSpec{entries: entries}); err != nil {
		t.Fatalf("VerifyChain(symlink to a directory listed as dir) error = %v, want no error", err)
	}
}

func TestVerifyChainRejectsMalformedListedDigest(t *testing.T) {
	t.Parallel()

	// Every shape a digest can arrive in and not be one. The last four are JSON
	// types rather than bad strings, and they are here because the reader
	// declares the field as a string rather than as an interface: the decoder
	// answers a null with the empty string and a number or a bool with a type
	// error, and both land on a refusal rather than on a value.
	trueRow := `"chksum_sha256":"` + hexDigest([]byte("# acme.widgets\n")) + `"`
	cases := []struct {
		name        string
		digest      string
		needle      string
		replacement string
	}{
		{name: "64 characters that are not hex", digest: strings.Repeat("z", 64)},
		{name: "one character short", digest: testOtherDigest[:63]},
		{name: "one character long", digest: testOtherDigest + "a"},
		{name: "uppercase", digest: strings.ToUpper(testOtherDigest)},
		// An empty listDigest means "use the true digest" to the builder, so
		// the row asks for a real one and then rewrites it away.
		{name: "empty", digest: testOtherDigest, needle: testOtherDigest, replacement: ""},
		{name: "json number", needle: trueRow, replacement: `"chksum_sha256":42`},
		{name: "json bool", needle: trueRow, replacement: `"chksum_sha256":true`},
		{name: "json null", needle: trueRow, replacement: `"chksum_sha256":null`},
		{name: "absent", needle: trueRow + ",", replacement: ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := chainEntries()
			entries[0].listDigest = tc.digest
			spec := chainSpec{entries: entries}
			if tc.needle != "" {
				spec.mutateFiles = replaceOnce(t, tc.needle, tc.replacement)
			}

			err := verifyChainFixture(t, spec)
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(listed digest %s) error = %v, want the chain sentinel", tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsUnsupportedListedChecksumType(t *testing.T) {
	t.Parallel()

	// The listed digest is left truthful, so what is refused is the algorithm
	// the row names rather than the value under it.
	entries := chainEntries()
	entries[0].listChksumType = "sha512"

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listing naming sha512) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsMalformedChainPointer(t *testing.T) {
	t.Parallel()

	// The first link, read out of the signed document before the archive is
	// touched at all. A pointer naming another algorithm is deliberately not a
	// row of its own: its value is not a sha256 either, so it lands on the
	// digest shape refusal rather than on a rule about the label.
	//
	// The digest rows state the value directly rather than splicing text into
	// the rendered manifest, so each row's name is what the fixture really
	// carries. Spliced, "one character short" is easy to write as something
	// that leaves the document unparseable instead, which is a different branch
	// under a name that hides the fact.
	//
	// The two decode rows are named apart because only one of them is
	// backstopped, and the difference is inside encoding/json. A syntax error
	// leaves the pointer zero, so the FILES.json name rule would refuse that
	// document even with the decode arm gone; a type error is written as far as
	// the decoder got, so the pointer arrives intact and nothing below would
	// look at the error at all.
	//
	// Ignoring it - `err != nil` in parseChainPointer's decode arm replaced by
	// `err != nil && false`, applied through go test -overlay so no production
	// file is edited - fails the type row alone:
	//
	//	chain_test.go:1011: VerifyChain(wrong type inside the pointer) error = <nil>, want the chain sentinel
	cases := []struct {
		name          string
		document      string
		needle        string
		replacement   string
		pointerDigest string
	}{
		{name: "syntax error in the manifest", document: "{not json"},
		{
			name:   "wrong type inside the pointer",
			needle: `"chksum_type":"` + testChksumSHA256 + `"`, replacement: `"chksum_type":42`,
		},
		{name: "pointer absent", document: `{"collection_info":{"name":"widgets"}}`},
		{name: "pointer names another file", needle: `"name":"` + testFilesName + `"`, replacement: `"name":"OTHER.json"`},
		{name: "digest of 64 characters that are not hex", pointerDigest: strings.Repeat("z", 64)},
		{name: "digest one character short", pointerDigest: testOtherDigest[:63]},
		{name: "digest one character long", pointerDigest: testOtherDigest + "a"},
		{name: "digest uppercase", pointerDigest: strings.ToUpper(testOtherDigest)},
		// An empty pointerDigest means "use the true digest" to the builder,
		// so the row asks for a real one and then rewrites it away.
		{name: "digest empty", pointerDigest: testOtherDigest, needle: testOtherDigest, replacement: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{entries: chainEntries(), pointerDigest: tc.pointerDigest}
			switch {
			case tc.document != "":
				spec.mutateManifest = func([]byte) []byte { return []byte(tc.document) }
			case tc.needle != "":
				spec.mutateManifest = replaceOnce(t, tc.needle, tc.replacement)
			}

			err := verifyChainFixture(t, spec)
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want the chain sentinel", tc.name, err)
			}
		})
	}
}

func TestVerifyChainReadsThePointerBeforeTheArchive(t *testing.T) {
	t.Parallel()

	// A manifest whose pointer is not a digest is refused without the artifact
	// being read at all, which is what lets this fixture hand VerifyChain a
	// file that is not an archive: getting the chain sentinel rather than the
	// shape sentinel is the whole assertion.
	//
	// It is also the only way to separate the pointer's shape rule from the
	// comparison further down. Reached with a real archive, a pointer refused
	// for its shape and one that merely disagrees produce the same sentinel -
	// an unparsed digest falls through as the zero array, and no listing hashes
	// to that - so a fixture carrying a real archive would leave that rule
	// looking pinned when nothing had touched it.
	_, manifestJSON := buildChainArtifact(t, chainSpec{
		entries:       chainEntries(),
		pointerDigest: strings.Repeat("z", 64),
	})

	notAnArchive := writeFile(t, "not-an-archive.bin", []byte("<html>404</html>"))
	err := VerifyChain(t.Context(), notAnArchive, manifestJSON)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(malformed pointer over a non-archive) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsMissingFilesManifest(t *testing.T) {
	t.Parallel()

	// Both ways the listing can be missing as a regular file. The symlink row
	// matters on its own: a reader asking only "is there an entry called
	// FILES.json" would find one, and would then have nothing to decode.
	cases := []struct {
		name string
		spec chainSpec
	}{
		{
			name: "absent entirely",
			spec: chainSpec{entries: chainEntries(), omitFilesManifest: true},
		},
		{
			name: "present as a symlink",
			spec: chainSpec{
				entries:       chainEntries(),
				filesTypeflag: tar.TypeSymlink,
				filesLinkname: "elsewhere/list.json",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, tc.spec)
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s %s) error = %v, want the chain sentinel", testFilesName, tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsForeignManifestBytes(t *testing.T) {
	t.Parallel()

	// The seam that makes opening the archive a second time sound: the bytes a
	// signature was verified over have to be the bytes the archive's own
	// MANIFEST.json carries. This is the whole attack the chain exists under -
	// a legitimately signed manifest stapled onto somebody else's tarball - and
	// the fixture is built so that the seam is the only thing that can refuse
	// it: the foreign document names the very digest this archive's FILES.json
	// hashes to, so the pointer parses, the listing matches it, and every
	// listed file matches the listing. Only its own bytes differ.
	//
	// Dropping it - the `got != sha256.Sum256(manifestJSON)` half of
	// verifyChainScan's first condition removed, leaving only the presence
	// check, applied through go test -overlay so no production file is edited -
	// fails here:
	//
	//	chain_test.go:1105: VerifyChain(another artifact's manifest) error = <nil>, want the chain sentinel
	artifact, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})

	foreign := bytes.Replace(manifestJSON, []byte(`"version":"1.0.0"`), []byte(`"version":"9.9.9"`), 1)
	if bytes.Equal(foreign, manifestJSON) {
		t.Fatalf("the fixture's %s no longer carries the version this test rewrites", testManifestName)
	}

	err := VerifyChain(t.Context(), artifact, foreign)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(another artifact's manifest) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsDuplicateArchiveEntry(t *testing.T) {
	t.Parallel()

	// Two entries competing for one name. Refusing it is what stops an archive
	// presenting one document to the pass that read the manifest and another to
	// the pass that walks the chain, since the two take different entries when
	// the name is ambiguous.
	//
	// The two document names are rows of their own because they are the two
	// this rule is likeliest to lose: the reverse rule exempts exactly those
	// names, and an exemption copied from there into this refusal reads like
	// symmetry. It is not one. Exempt here, a second entry under either name
	// overwrites what the first recorded, so the pass that walks the chain
	// answers from the last such entry while ReadFromTarGz answers from the
	// first - the ambiguity this refusal exists to remove.
	//
	// The control is the default fixture, which carries exactly one entry under
	// each of these names and is accepted; every row below is that fixture plus
	// a second entry under one of them.
	//
	// Exempting them - claim's condition gaining `&& key !=
	// helpers.ManifestFileName && key != helpers.FilesManifestFileName`,
	// applied through go test -overlay so no production file is edited - fails
	// both document rows. No mutation output is quoted for it, since a refusal
	// from VerifyChain renders the artifact's own temp path and no two runs
	// would produce the same line to quote.
	cases := []struct {
		name      string
		duplicate chainEntry
	}{
		{name: "an ordinary file", duplicate: chainEntry{name: "README.md", content: []byte("# second\n")}},
		{name: testManifestName, duplicate: chainEntry{name: testManifestName, content: []byte(`{"decoy":true}`)}},
		{name: testFilesName, duplicate: chainEntry{name: testFilesName, content: []byte(`{"files":[]}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The duplicate is an entry no listing vouches for, which is the
			// shape this refusal exists to answer, and keeping it out leaves
			// the fixture's listing the well-formed one it would be without it.
			duplicate := tc.duplicate
			duplicate.unlisted = true

			err := verifyChainFixture(t, chainSpec{entries: append(chainEntries(), duplicate)})
			if !errors.Is(err, helpers.ErrArchiveDuplicateEntry) {
				t.Fatalf("VerifyChain(a second %s) error = %v, want the duplicate sentinel", tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsDuplicateListingEntry(t *testing.T) {
	t.Parallel()

	// A listing naming one path twice contradicts itself: one of the two rows
	// decides, and which one it is would be an artifact of iteration order.
	trueDigest := hexDigest([]byte("# acme.widgets\n"))
	row := `{"chksum_type":"` + testChksumSHA256 + `","chksum_sha256":"` + trueDigest +
		`","name":"README.md","ftype":"` + testFtypeFile + `"}`
	spec := chainSpec{
		entries:     chainEntries(),
		mutateFiles: replaceOnce(t, row, row+","+row),
	}

	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listing naming one path twice) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsOverlongEntryName(t *testing.T) {
	t.Parallel()

	// The cap this pass carries and the extractor does not, because this pass
	// holds every entry name until the stream ends. The four lengths are spelled
	// out rather than derived from helpers.ArchiveMaxEntryNameLen, so a change
	// to that constant fails one of these rows instead of moving all of them.
	//
	// Removing the cap - checkEntryNameLengths' body replaced by a bare return nil through go test -overlay - fails both
	// refusing rows on their one shared assertion, neither by accepting the archive: another rule answers first, the
	// listing's own field cap here and the reverse rule for the link. A fragment is quoted rather than the whole line,
	// which renders this run's own temp path and runs past the 140 columns the linter permits:
	//	chain_test.go:1210: ... FILES.json carries a name field of 1025 bytes ...
	cases := []struct {
		name    string
		nameLen int
		linkLen int
		wantErr bool
	}{
		{name: "name at the cap", nameLen: testEntryNameCap},
		{name: "name one byte over", nameLen: testEntryNameCap + 1, wantErr: true},
		{name: "link target at the cap", nameLen: 16, linkLen: testEntryNameCap},
		{name: "link target one byte over", nameLen: 16, linkLen: testEntryNameCap + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, chainSpec{entries: longNameEntries(tc.nameLen, tc.linkLen)})
			if tc.wantErr && !errors.Is(err, helpers.ErrArchiveEntryNameTooLong) {
				t.Fatalf("VerifyChain(%s) error = %v, want the name-length sentinel", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

// longNameEntries builds a fixture whose one interesting entry names itself, or
// its link target, in the requested number of bytes.
//
// Within the cap the link case carries a real file under the target's name, so
// the accepting row resolves rather than merely being refused for some other
// reason. Past the cap that file cannot exist - its own name would breach the
// same rule, and the refusal quoted would be the file's rather than the link
// target's - so the fixture leaves the link dangling and out of the listing.
// The name-length rule is then the only thing between this archive and the
// reverse rule, which refuses under a different sentinel than the row asserts.
func longNameEntries(nameLen, linkLen int) []chainEntry {
	name := strings.Repeat("a", nameLen)
	if linkLen == 0 {
		return []chainEntry{{name: name, content: []byte("body\n")}}
	}
	target := strings.Repeat("b", linkLen)
	link := chainEntry{name: name, linkname: target, typeflag: tar.TypeSymlink}
	if linkLen > testEntryNameCap {
		link.unlisted = true
		return []chainEntry{link}
	}
	return []chainEntry{link, {name: target, content: []byte("body\n")}}
}

func TestVerifyChainRejectsOversizeFilesManifest(t *testing.T) {
	t.Parallel()

	// The listing is the one entry read into memory whole, so its header's
	// declared size is an allocation rather than a read - and it is refused on
	// that declaration, before a byte of it is pulled through. The control
	// carries the same document with an honest header.
	if err := verifyChainFixture(t, chainSpec{entries: chainEntries()}); err != nil {
		t.Fatalf("VerifyChain(%s with an honest header) error = %v, want no error", testFilesName, err)
	}

	spec := chainSpec{entries: chainEntries(), filesDeclaredSize: testFilesManifestCap + 1}
	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("VerifyChain(%s declaring past its cap) error = %v, want the entry-size sentinel", testFilesName, err)
	}
}

func TestVerifyChainRejectsDecompressedStreamOverrun(t *testing.T) {
	t.Parallel()

	// The stream cap, driven through the injected seam so the fixture stays a
	// kilobyte instead of reaching the production ceiling. The control runs the
	// identical bytes under a cap they fit inside, so the refusal is the
	// ceiling answering rather than anything about the archive.
	artifact, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})
	//nolint:gosec // artifact is a path this test just wrote under its own temp directory.
	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("failed to read back the fixture: %v", err)
	}

	if err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize); err != nil {
		t.Fatalf("verifyChainStream(under the production cap) error = %v, want no error", err)
	}

	const tinyCap = 1024
	err = verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, tinyCap)
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("verifyChainStream(cap of %d bytes) error = %v, want the stream-cap sentinel", tinyCap, err)
	}
}

func TestVerifyChainRejectsTooManyEntries(t *testing.T) {
	t.Parallel()

	// Driven against scanArchive directly, over a raw tar stream with no gzip
	// and no bodies: a header is 512 bytes whatever it names, so reaching the
	// production cap costs one buffer and no compression pass. The control at
	// exactly the cap is what makes the refusal mean the counter fired rather
	// than the stream ending.
	//
	// This is the slowest test in the package by a wide margin - measured
	// between 4.07 s and 4.26 s across runs, of a package run just under six
	// seconds under -race - and that is the price of pinning
	// helpers.ArchiveMaxEntryCount itself rather than a cap injected for the
	// test's convenience, which would move what is proven to a test-only value.
	if _, err := scanArchive(tar.NewReader(bytes.NewReader(headerOnlyStream(t, helpers.ArchiveMaxEntryCount)))); err != nil {
		t.Fatalf("scanArchive(exactly the entry cap) error = %v, want no error", err)
	}

	_, err := scanArchive(tar.NewReader(bytes.NewReader(headerOnlyStream(t, helpers.ArchiveMaxEntryCount+1))))
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("scanArchive(one entry past the cap) error = %v, want the entry-count sentinel", err)
	}
}

// headerOnlyStream renders count zero-byte regular entries as a raw tar stream.
func headerOnlyStream(t *testing.T, count int64) []byte {
	t.Helper()

	var buf bytes.Buffer
	buf.Grow(int(count+2) * 512)
	tw := tar.NewWriter(&buf)
	for i := range count {
		header := &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("e/%d", i), Mode: 0o644}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write header %d: %v", i, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close the header-only stream: %v", err)
	}
	return buf.Bytes()
}

func TestVerifyChainStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	// The context is read where the archive is, not before it: the pointer is
	// parsed out of the signed document first, so a fixture whose manifest is
	// well-formed is what proves the cancellation reached the tar walk.
	artifact, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := VerifyChain(ctx, artifact, manifestJSON)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("VerifyChain(canceled context) error = %v, want context.Canceled reachable", err)
	}
}

func TestVerifyChainResolvesLinkChainWithinTheHopBound(t *testing.T) {
	t.Parallel()

	// A link pointing at a link is a shape the reference reader resolves, so
	// the bound is on the length of the chain rather than on there being one.
	// The two accepting rows sit at either end of what is permitted, and the
	// refusing pair covers the two ways a chain fails to end: one link too
	// long, and one that never ends at all.
	cases := []struct {
		name    string
		entries []chainEntry
		wantErr bool
	}{
		{name: "two hops", entries: linkChainEntries(2)},
		{name: "at the hop bound", entries: linkChainEntries(chainMaxLinkHops)},
		{name: "one hop past the bound", entries: linkChainEntries(chainMaxLinkHops + 1), wantErr: true},
		{name: "cycle", entries: linkCycleEntries(), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, chainSpec{entries: tc.entries})
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want the chain sentinel", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

// linkChainEntries builds a file with links long ahead of it: link0 points at
// link1, and the last one points at the file, so resolving link0 costs exactly
// links hops.
func linkChainEntries(links int) []chainEntry {
	entries := make([]chainEntry, 0, links+1)
	entries = append(entries, chainEntry{name: "real.md", content: []byte("real\n")})
	for i := range links {
		target := "real.md"
		if i+1 < links {
			target = fmt.Sprintf("link%d.md", i+1)
		}
		entries = append(entries, chainEntry{
			name: fmt.Sprintf("link%d.md", i), linkname: target, typeflag: tar.TypeSymlink,
		})
	}
	return entries
}

// linkCycleEntries builds two links pointing at each other. The rows carry a
// digest of their own because the fixture's resolver reaches no content for
// them either, which is the point: the hop bound is what ends the walk.
func linkCycleEntries() []chainEntry {
	return []chainEntry{
		{name: "a.md", linkname: "b.md", typeflag: tar.TypeSymlink, listDigest: testOtherDigest},
		{name: "b.md", linkname: "a.md", typeflag: tar.TypeSymlink, listDigest: testOtherDigest},
	}
}

// chainStreamFixture builds a fixture and hands back its compressed bytes
// alongside the manifest a signature would have been verified over.
//
// It exists for the two things the path form cannot do: running one archive
// through the check many times without reopening it, and producing a failure
// line worth quoting - VerifyChain prefixes its own error with the artifact's
// path, which is a fresh temp directory on every run.
func chainStreamFixture(t *testing.T, spec chainSpec) ([]byte, []byte) {
	t.Helper()

	artifact, manifestJSON := buildChainArtifact(t, spec)
	//nolint:gosec // artifact is a path this test just wrote under its own temp directory.
	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("failed to read back the fixture: %v", err)
	}
	return raw, manifestJSON
}

// reportedUnlistedEntry pulls the quoted path out of the reverse rule's
// refusal, so a test can assert WHICH entry was named as one comparison and
// fail with a line naming both paths instead of repeating the whole message.
func reportedUnlistedEntry(t *testing.T, err error) string {
	t.Helper()

	_, rest, found := strings.Cut(err.Error(), "the archive carries ")
	name, _, complete := strings.Cut(rest, ", which")
	if !found || !complete {
		t.Fatalf("error = %v, want the reverse rule's own refusal", err)
	}
	return name
}

func TestVerifyChainNamesTheFirstUnlistedEntryInStreamOrder(t *testing.T) {
	t.Parallel()

	// The reverse rule reports one offending path out of however many are
	// unlisted, so which one it names is part of what it says: an operator has
	// to be able to go and look at the entry the line names, and a line naming
	// a different entry on every run is not one to go and look at.
	//
	// Two unlisted entries are what it takes to see that. With one, a walk over
	// the scan's maps names it exactly as reliably as a walk over the order the
	// stream carried. The two names are chosen so that stream order and lexical
	// order disagree, which pins the property to the archive's own order rather
	// than to any total order over the keys.
	//
	// Ranging over the scan's maps instead - the loop in checkUnlisted becoming
	// `for key := range scan.hashes` followed by the same body over scan.links,
	// applied through go test -overlay so no production file is edited - fails
	// here, on the first run that reaches the other entry first:
	//
	//	chain_test.go:1490: verifyChainStream(two unlisted entries) named "plugins/alpha.py" on run 8, want "plugins/zebra.py"
	entries := append(chainEntries(),
		chainEntry{name: "plugins/zebra.py", content: []byte("# first in the stream\n")},
		chainEntry{name: "plugins/alpha.py", content: []byte("# second in the stream\n")},
	)

	// The control: the identical archive with both entries listed is accepted,
	// so what the runs below report is the reverse rule rather than the fixture.
	if err := verifyChainFixture(t, chainSpec{entries: entries}); err != nil {
		t.Fatalf("VerifyChain(both entries listed) error = %v, want no error", err)
	}

	first, second := &entries[len(entries)-2], &entries[len(entries)-1]
	first.unlisted, second.unlisted = true, true
	raw, manifestJSON := chainStreamFixture(t, chainSpec{entries: entries})

	// One archive read many times, because a walk over the scan's maps happens
	// to agree with the stream far more often than it disagrees: measured
	// against that mutation, 256 runs out of 2000 named the later entry, so a
	// single run would miss it seven times in eight. 128 runs leave that at
	// about one in forty million. The ratio is a property of the Go release's
	// map iteration and drifts with it; what does not drift is that one run
	// proves nothing here.
	const runs = 128
	wanted := fmt.Sprintf("%q", first.name)
	for run := range runs {
		err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("verifyChainStream(two unlisted entries) run %d error = %v, want the chain sentinel", run, err)
		}
		if got := reportedUnlistedEntry(t, err); got != wanted {
			t.Fatalf("verifyChainStream(two unlisted entries) named %s on run %d, want %s", got, run, wanted)
		}
	}
}

func TestVerifyChainSkipsEntriesNamingTheArchiveRoot(t *testing.T) {
	t.Parallel()

	// Both shapes a tar entry takes when its name normalizes to the archive
	// root, which names no file: tar.Writer emits either one with a regular
	// file's typeflag and a body, so an archive can carry both. They are
	// skipped rather than refused, matching the extractor, which is what takes
	// them out of the reverse rule's scope - neither is listed, and neither
	// could be: a row naming either one cleans to ".", the key this fixture's
	// own root row already occupies.
	//
	// One fixture covers both, because a shape that stopped normalizing to the
	// root would arrive as an ordinary unlisted entry and be refused here.
	//
	// Removing the skip - `if key == ""` in entry replaced by `if false`,
	// applied through go test -overlay so no production file is edited - fails
	// here:
	//
	//	chain_test.go:1521: verifyChainStream(root-named entries) error = archive contains a duplicate entry: "", want no error
	raw, manifestJSON := chainStreamFixture(t, chainSpec{entries: append(chainEntries(),
		chainEntry{name: ".", content: []byte("# root\n"), unlisted: true},
		chainEntry{name: "foo/..", content: []byte("# also root\n"), unlisted: true},
	)})

	err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
	if err != nil {
		t.Fatalf("verifyChainStream(root-named entries) error = %v, want no error", err)
	}
}

func TestVerifyChainRefusesAnArtifactItCannotOpen(t *testing.T) {
	t.Parallel()

	// The open, which is the one step VerifyChain takes before handing the
	// archive to the walk, on a path that comes from the run rather than from
	// any document: an artifact evicted from the cache between the download and
	// this check is simply gone by the time the chain is walked.
	//
	// The control is the same fixture at the path it really sits on, so the
	// refusal below is the open failing rather than a fixture that would have
	// been refused wherever it sat.
	artifact, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})
	if err := VerifyChain(t.Context(), artifact, manifestJSON); err != nil {
		t.Fatalf("VerifyChain(the fixture where it sits) error = %v, want no error", err)
	}

	if err := VerifyChain(t.Context(), artifact+".absent", manifestJSON); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VerifyChain(a path carrying no artifact) error = %v, want a missing-file error", err)
	}
}

func TestVerifyChainRejectsAStreamEndingInsideAnEntryBody(t *testing.T) {
	t.Parallel()

	// The two bodies this pass reads, each through code of its own: an ordinary
	// file is streamed into the digest through the scan's copy buffer, while
	// FILES.json is read whole, since it has to be decoded rather than merely
	// hashed. Both rows declare an entry far larger than the bytes the archive
	// carries behind that header, so the read runs off the end of the stream
	// rather than off the end of the entry.
	//
	// Each propagates what the reader handed back instead of turning it into a
	// verdict about the chain, and that is deliberate rather than an omission:
	// a stream ending early says nothing about whether the documents agree, and
	// a truncated artifact is the ordinary way it happens.
	const overDeclared = 1 << 20

	// The control is the same entries declaring the sizes they really carry,
	// which is the default fixture and is accepted.
	if err := verifyChainFixture(t, chainSpec{entries: chainEntries()}); err != nil {
		t.Fatalf("VerifyChain(the same entries declaring their real sizes) error = %v, want no error", err)
	}

	oversizedFile := chainEntries()
	oversizedFile[0].declaredSize = overDeclared

	cases := []struct {
		name string
		spec chainSpec
	}{
		{name: "an ordinary file", spec: chainSpec{entries: oversizedFile}},
		{name: testFilesName, spec: chainSpec{entries: chainEntries(), filesDeclaredSize: overDeclared}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, tc.spec)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("VerifyChain(%s declaring %d bytes) error = %v, want the stream ending early", tc.name, overDeclared, err)
			}
		})
	}
}

// listingDocument renders a FILES.json carrying exactly the rows given, so a
// test can hand the reader a listing the fixture builder would never produce.
func listingDocument(rows ...string) []byte {
	return []byte(`{"files":[` + strings.Join(rows, ",") + `]}`)
}

// dirRowListing renders a listing of count directory rows, each naming a path
// no fixture archive carries, so how many rows it has is the only thing about
// it a reader can object to.
func dirRowListing(count int64) []byte {
	var buf bytes.Buffer
	// Around 32 bytes a row at the counts this is called with, so the document
	// is grown once rather than doubled a dozen times on the way up.
	buf.Grow(int(count)*32 + 16)
	buf.WriteString(`{"files":[`)
	for i := range count {
		if i > 0 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf, `{"name":"d/%d","ftype":%q}`, i, testFtypeDir)
	}
	buf.WriteString(`]}`)
	return buf.Bytes()
}

func TestVerifyChainRejectsTooManyListedRows(t *testing.T) {
	t.Parallel()

	// The listing's own row count, which helpers.FilesManifestMaxBytes does not
	// bound: 32 MiB of empty rows is 31.9 KiB on the wire and eleven million
	// rows in memory. The cap is helpers.ArchiveMaxEntryCount, pinned against
	// the production constant with no injected seam - the rule
	// TestVerifyChainRejectsTooManyEntries states for the tar side - and cheap
	// here, since 100,001 directory rows render to about 3 MB of JSON.
	//
	// The control at exactly the cap is what makes the refusal mean the counter
	// fired rather than the listing being unacceptable for some other reason.
	// Every row names a directory the archive does not carry, and the archive
	// carries nothing but the two documents, so no other rule in this reader
	// has anything to say about either fixture.
	//
	// Removing the cap - `rows > helpers.ArchiveMaxEntryCount` in
	// walkListingRows replaced by `false`, applied through go test -overlay so
	// no production file is edited - fails here:
	//
	//	chain_test.go:1651: VerifyChain(one row past the cap) error = <nil>, want the chain sentinel
	cases := []struct {
		name    string
		rows    int64
		wantErr bool
	}{
		{name: "exactly the row cap", rows: helpers.ArchiveMaxEntryCount},
		{name: "one row past the cap", rows: helpers.ArchiveMaxEntryCount + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{mutateFiles: func([]byte) []byte { return dirRowListing(tc.rows) }}
			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want the chain sentinel", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainJudgesEveryRowOnItsOwnFields(t *testing.T) {
	t.Parallel()

	// json.Decoder.Decode does not zero its destination, so a row variable
	// hoisted out of the streaming loop hands each row the previous row's
	// values for whatever fields it omits. The fixture is built so that
	// inheriting is ACCEPTANCE rather than some other refusal: both files carry
	// identical bytes, so a second row inheriting the first row's ftype,
	// checksum type and digest resolves against the archive and matches.
	//
	// The control is the same listing with that second row spelled out in full,
	// which is what a real builder writes and which is accepted.
	//
	// Hoisting the variable - `var row filesEntry` moved above the
	// `for dec.More()` loop in walkListingRows, applied through go test
	// -overlay so no production file is edited - fails here:
	//
	//	chain_test.go:1701: VerifyChain(the second row omitting ftype and digest) error = <nil>, want refusal
	shared := []byte("# shared\n")
	digest := hexDigest(shared)
	entries := []chainEntry{
		{name: "README.md", content: shared},
		{name: "second.md", content: shared},
	}
	cases := []struct {
		name    string
		second  string
		wantErr bool
	}{
		{name: "the second row spelled out in full", second: listingFileRowJSON("second.md", digest)},
		{name: "the second row omitting ftype and digest", second: `{"name":"second.md"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{entries: entries, mutateFiles: func([]byte) []byte {
				return listingDocument(listingFileRowJSON("README.md", digest), tc.second)
			}}
			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsContentAroundTheListing(t *testing.T) {
	t.Parallel()

	// The walk has to consume the closing bracket, then the closing brace, then
	// require the document to end. Measured against a loop that stopped at the
	// bracket: both a document with garbage after its final brace and one with
	// garbage in place of that brace were ACCEPTED, where json.Unmarshal
	// refuses either. Python's json.loads refuses trailing content too, so
	// accepting it would be a parser differential against the reference reader
	// rather than mere leniency.
	//
	// The control is the document as the builder renders it, so every refusal
	// below is one edit away from a listing this reader accepts.
	//
	// Dropping the end-of-document check - the `if _, err := dec.Token();
	// !errors.Is(err, io.EOF)` arm in walkListingDocument replaced by a bare
	// return nil, applied through go test -overlay so no production file is
	// edited - fails the first refusing row:
	//
	//	chain_test.go:1763: VerifyChain(garbage after the closing brace) error = <nil>, want refusal
	cases := []struct {
		mutate  func([]byte) []byte
		name    string
		wantErr bool
	}{
		{name: "the document as rendered"},
		{
			name:    "garbage after the closing brace",
			mutate:  func(b []byte) []byte { return []byte(string(b) + " GARBAGE") },
			wantErr: true,
		},
		{
			name:    "garbage in place of the closing brace",
			mutate:  func(b []byte) []byte { return []byte(strings.TrimSuffix(string(b), "}") + " GARBAGE") },
			wantErr: true,
		},
		{
			name:    "the array truncated inside a row",
			mutate:  func([]byte) []byte { return []byte(`{"files":[{"name":`) },
			wantErr: true,
		},
		{
			name:    "the array never closed",
			mutate:  func([]byte) []byte { return []byte(`{"files":[`) },
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, chainSpec{entries: chainEntries(), mutateFiles: tc.mutate})
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsASecondFilesKey(t *testing.T) {
	t.Parallel()

	// Two readers disagree about which of two "files" keys decides: measured,
	// json.Unmarshal takes the LAST while a streaming loop naturally takes the
	// FIRST. That is the first-versus-last ambiguity claim already refuses on
	// the tar side, arriving inside one signed document, and the answer is the
	// same - refuse the document rather than pick a winner.
	//
	// The second array is EMPTY, which is what makes this pin the rule: a
	// reader taking the first key would ignore it and accept the artifact,
	// where one taking the last would refuse through the reverse rule instead.
	// Only an explicit refusal answers both. The control is the same listing
	// with one key, which is accepted.
	//
	// Dropping the refusal - the `if walked` arm in walkListingDocument
	// replaced by `if false`, applied through go test -overlay so no production
	// file is edited - fails here:
	//
	//	chain_test.go:1810: VerifyChain(a second files key) error = <nil>, want refusal
	cases := []struct {
		mutate  func([]byte) []byte
		name    string
		wantErr bool
	}{
		{name: "one files key"},
		{
			name:    "a second files key",
			mutate:  func(b []byte) []byte { return []byte(strings.TrimSuffix(string(b), "}") + `,"files":[]}`) },
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, chainSpec{entries: chainEntries(), mutateFiles: tc.mutate})
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainAcceptsAListingWithNoFilesKey(t *testing.T) {
	t.Parallel()

	// Zero "files" keys is not the ambiguous shape a second one is, and it has
	// to stay accepted: it yields an empty set, which is exactly what a decode
	// into a struct produced for a document whose files field was absent. What
	// answers such a listing is the reverse rule rather than the decode, which
	// is why the two rows are the same document over two archives - one
	// carrying nothing but the two exempt documents, one carrying files as
	// well.
	cases := []struct {
		name    string
		entries []chainEntry
		wantErr bool
	}{
		{name: "an archive carrying only the two documents"},
		{name: "an archive carrying files as well", entries: chainEntries(), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{
				entries:     tc.entries,
				mutateFiles: func([]byte) []byte { return []byte(`{"format":1}`) },
			}
			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainSkipsUnknownListingKeys(t *testing.T) {
	t.Parallel()

	// These documents carry more than this reader reads - "format" among them -
	// so a key it does not understand is skipped whole rather than refused,
	// wherever in the object it sits and whatever shape its value takes.
	//
	// The last row is what keeps the skip honest: an unknown key ahead of a
	// listing that names a file the archive does not carry still reaches that
	// refusal, so skipping a value consumes exactly the value and not the rest
	// of the document.
	absent := listingFileRowJSON("absent.md", testOtherDigest)
	cases := []struct {
		name        string
		needle      string
		replacement string
		wantErr     bool
	}{
		{name: "a scalar before the array", needle: `{"files":[`, replacement: `{"format":1,"files":[`},
		{name: "a scalar after the array", needle: `]}`, replacement: `],"format":1}`},
		{
			name:   "a nested value before the array",
			needle: `{"files":[`, replacement: `{"extra":{"a":[1,2,{"b":null}],"c":"d"},"files":[`,
		},
		{
			name:   "a scalar ahead of a listing that still refuses",
			needle: `{"files":[`, replacement: `{"format":1,"files":[` + absent + `,`, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{entries: chainEntries(), mutateFiles: replaceOnce(t, tc.needle, tc.replacement)}
			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

// fieldMessageCap bounds a refusal that reports a field's length: every such
// message is a sentence plus two numbers, so anything near this bound is the
// value having been rendered rather than described.
const fieldMessageCap = 200

// overlongFieldRow renders one directory row with field set to length bytes,
// so a test can push any one of a row's four strings past the cap while the
// other three stay ordinary. It returns the row and the value it carries, which
// is what an assertion looks for in the message.
func overlongFieldRow(t *testing.T, field string, length int) (string, string) {
	t.Helper()

	value := strings.Repeat("a", length)
	fields := map[string]string{"name": "extra", "ftype": testFtypeDir}
	fields[field] = value
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("failed to render the fixture's row: %v", err)
	}
	return string(body), value
}

func TestVerifyChainRejectsOverlongListedFields(t *testing.T) {
	t.Parallel()

	// FILES.json's strings reach this reader from a document that is
	// attacker-chosen the moment the archive is, and every one of them can be
	// interpolated into a refusal. They are capped at the tar side's own
	// helpers.ArchiveMaxEntryNameLen, and the refusal reports the LENGTH rather
	// than the value, for the reason checkEntryNameLengths already gives.
	//
	// Each field is paired at the cap and one byte over, so every refusing row
	// is its own positive control: the accepted row is the identical listing
	// with one byte fewer.
	//
	// Two mutations, because two properties are at stake. Removing the cap -
	// checkListedFieldLengths' body replaced by a bare return nil, applied
	// through go test -overlay so no production file is edited - fails all
	// four over-cap rows, a 1,025-byte directory row being otherwise
	// unobjectionable:
	//
	//	chain_test.go:1981: verifyChainStream(name one byte over) error = <nil>, want refusal
	//
	// Rendering the value instead of its length - checkFieldLength's message
	// gaining %q of value, same overlay method - keeps the refusal and fails
	// the first assertion checkBoundedRefusal makes:
	//
	//	chain_test.go:1983: verifyChainStream(name one byte over) message reproduces the 1025-byte value
	cases := []struct {
		name    string
		field   string
		length  int
		wantErr bool
	}{
		{name: "name at the cap", field: "name", length: testEntryNameCap},
		{name: "name one byte over", field: "name", length: testEntryNameCap + 1, wantErr: true},
		{name: "ftype at the cap", field: "ftype", length: testEntryNameCap},
		{name: "ftype one byte over", field: "ftype", length: testEntryNameCap + 1, wantErr: true},
		{name: "chksum_type at the cap", field: "chksum_type", length: testEntryNameCap},
		{name: "chksum_type one byte over", field: "chksum_type", length: testEntryNameCap + 1, wantErr: true},
		{name: "chksum_sha256 at the cap", field: "chksum_sha256", length: testEntryNameCap},
		{name: "chksum_sha256 one byte over", field: "chksum_sha256", length: testEntryNameCap + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			row, value := overlongFieldRow(t, tc.field, tc.length)
			raw, manifestJSON := chainStreamFixture(t, chainSpec{
				entries:     chainEntries(),
				mutateFiles: replaceOnce(t, `{"files":[`, `{"files":[`+row+`,`),
			})
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s) error = %v, want refusal", tc.name, err)
			}
			checkBoundedRefusal(t, tc.name, err, value)
		})
	}
}

// checkBoundedRefusal asserts that a refusal describes an over-long value
// rather than reproducing it: the value itself must not appear, and the whole
// message must stay inside fieldMessageCap. Both are needed - a message
// rendering a 500-byte prefix satisfies the first and not the second,
// measured at 618 bytes against exactly that mutation.
func checkBoundedRefusal(t *testing.T, name string, err error, value string) {
	t.Helper()

	if strings.Contains(err.Error(), value) {
		t.Fatalf("verifyChainStream(%s) message reproduces the %d-byte value", name, len(value))
	}
	if got := len(err.Error()); got > fieldMessageCap {
		t.Fatalf("verifyChainStream(%s) message is %d bytes, want under %d", name, got, fieldMessageCap)
	}
}

func TestVerifyChainRejectsOverlongPointerFields(t *testing.T) {
	t.Parallel()

	// MANIFEST.json is bounded only by helpers.ManifestScanMaxBytes (64 MiB),
	// and each of the pointer's three strings is rendered with %q by a refusal
	// below it: measured, a 32 MiB field of escaping bytes renders to exactly
	// 128.0 MiB that way. The same cap and the same length-not-value rule
	// therefore apply here.
	//
	// The control is the manifest as rendered, which is accepted, so each
	// refusal below is that document with one field lengthened.
	//
	// Removing the cap - checkPointerFieldLengths' body replaced by a bare
	// return nil, applied through go test -overlay so no production file is
	// edited - fails all three rows: chksum_type is accepted outright, nothing
	// else reading it, while name is refused and renders what it read:
	//
	//	chain_test.go:2054: verifyChainStream(name) message reproduces the 1025-byte value
	long := strings.Repeat("a", testEntryNameCap+1)
	cases := []struct {
		name    string
		needle  string
		digest  string
		wantErr bool
	}{
		{name: "the manifest as rendered"},
		{name: "name", needle: `"name":"` + testFilesName + `"`, wantErr: true},
		{name: "chksum_type", needle: `"chksum_type":"` + testChksumSHA256 + `"`, wantErr: true},
		{name: "chksum_sha256", digest: long, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{entries: chainEntries(), pointerDigest: tc.digest}
			if tc.needle != "" {
				field, _, _ := strings.Cut(tc.needle, ":")
				spec.mutateManifest = replaceOnce(t, tc.needle, field+`:"`+long+`"`)
			}
			raw, manifestJSON := chainStreamFixture(t, spec)
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s) error = %v, want refusal", tc.name, err)
			}
			checkBoundedRefusal(t, tc.name, err, long)
		})
	}
}

func TestVerifyChainRejectsARegularFileListedAsSomethingElse(t *testing.T) {
	t.Parallel()

	// A row claiming not to be a file marks its path listed and nothing checks
	// the content under it, so without this rule the archive may carry a
	// regular file of arbitrary bytes at a path the signed listing calls a
	// directory. The reverse rule cannot answer it either, since the path IS
	// listed - which is why the content below is what an attacker would put
	// there rather than a placeholder.
	//
	// The control is the identical archive with that file listed as the file it
	// is, which is accepted.
	//
	// Dropping the rule - the `if _, isFile := scan.hashes[key]; isFile` arm in
	// checkListedRow replaced by `if false`, applied through go test -overlay
	// so no production file is edited - fails both refusing rows, the first
	// being:
	//
	//	chain_test.go:2097: VerifyChain(listed as a directory) error = <nil>, want refusal
	cases := []struct {
		name    string
		listAs  string
		wantErr bool
	}{
		{name: "listed as the file it is", listAs: testFtypeFile},
		{name: "listed as a directory", listAs: testFtypeDir, wantErr: true},
		{name: "listed under an ftype no reader knows", listAs: "wharrgarbl", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := chainEntries()
			entries[0].content = []byte("rm -rf /\n")
			entries[0].listAs = tc.listAs

			err := verifyChainFixture(t, chainSpec{entries: entries})
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainCapsAnEntryNameBeforeAnythingRendersIt(t *testing.T) {
	t.Parallel()

	// chargeEntrySize renders header.Name with %q on two of its three arms and
	// runs on every header, so the name cap has to run ahead of it or those
	// messages carry a name nothing has measured. Measured with the cap inside
	// chainScanner.entry instead: a 200,000-byte name with an over-cap declared
	// size produced a 200,046-byte error message.
	//
	// The three rows are what separate the two rules. The first is the fixture
	// accepted as it stands. The second declares the same over-cap size under
	// an ordinary name, so chargeEntrySize is shown to be reachable and to
	// quote the name it refuses. The third is that entry with a name past the
	// cap: the name rule answers first, under its own sentinel and in a message
	// bounded whatever the name's length.
	//
	// Moving the check back - checkEntryNameLengths' call moved out of
	// scanArchive to the head of chainScanner.entry, applied through go test
	// -overlay so no production file is edited - fails the third row on the
	// message bound rather than on the sentinel, which is the whole point:
	//
	//	chain_test.go:2167: verifyChainStream(a name past the cap) message is 200046 bytes, want under 300
	const (
		longNameLen         = 200_000
		entryMessageCap     = 300
		oversizeDeclaration = helpers.ArchiveMaxEntrySize + 1
	)
	cases := []struct {
		want   error
		name   string
		entry  string
		quotes string
		size   int64
	}{
		{name: "the fixture as it stands"},
		{
			name: "an over-cap size under an ordinary name", entry: "huge.bin", size: oversizeDeclaration,
			want: helpers.ErrArchiveEntryIsTooLarge, quotes: "huge.bin",
		},
		{
			name: "a name past the cap", entry: strings.Repeat("a", longNameLen), size: oversizeDeclaration,
			want: helpers.ErrArchiveEntryNameTooLong,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := chainEntries()
			if tc.entry != "" {
				entries = append(entries, chainEntry{name: tc.entry, declaredSize: tc.size, unlisted: true})
			}
			raw, manifestJSON := chainStreamFixture(t, chainSpec{entries: entries})
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
				}
				return
			}
			if got := len(err.Error()); got > entryMessageCap {
				t.Fatalf("verifyChainStream(%s) message is %d bytes, want under %d", tc.name, got, entryMessageCap)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("verifyChainStream(%s) error = %v, want %v", tc.name, err, tc.want)
			}
			if tc.quotes != "" && !strings.Contains(err.Error(), tc.quotes) {
				t.Fatalf("verifyChainStream(%s) message does not name %q", tc.name, tc.quotes)
			}
		})
	}
}

func TestVerifyChainRejectsAListingOfTheWrongShape(t *testing.T) {
	t.Parallel()

	// expectDelim's two arms, one per delimiter it is asked for: the brace that
	// opens the document, and the bracket that opens the array under "files".
	// Every other fixture in this file carries a listing of the right shape and
	// corrupts something inside it, so nothing else here reaches either arm.
	//
	// The archive carries nothing but the two documents, deliberately. With any
	// other entry in it, a reader falling through these arms onto an empty
	// listing would be caught by the reverse rule instead - it would see an
	// empty listing over an archive carrying content - and the arms would look
	// pinned when nothing had touched them. The control is that same fixture
	// with the listing the builder renders, which is accepted.
	//
	// Each refusing row asserts the token the message names as well as the
	// sentinel, and the pair is what separates a refusal made where the document
	// is read from one the next step trips over: with both arms neutered, four
	// of these six documents are still refused, but for running out of document
	// rather than for their shape.
	//
	// Two of the six are a behavior delta rather than a stricter message. A
	// whole-document json.Unmarshal into a struct of rows accepts the document
	// `null` and the document `{"files":null}` as an empty listing - measured on
	// the production row type, nil error and zero rows for both - while the
	// other four are type errors there too. Refusing all six is what the
	// reference reader does: Python's json.loads hands back None for the first
	// and {'files': None} for the second, and raises on data['files'] either
	// way.
	//
	// Neutering both arms - `if delim, ok := tok.(json.Delim); !ok || delim !=
	// want` in expectDelim becoming `if delim, ok := tok.(json.Delim); false &&
	// (!ok || delim != want)`, applied through go test -overlay so no production
	// file is edited - fails every refusing row, two of them by accepting the
	// artifact outright:
	//
	//	chain_test.go:2250: verifyChainStream(a null document) message does not carry "{" was expected
	//	chain_test.go:2247: verifyChainStream(files as an object) error = <nil>, want refusal
	cases := []struct {
		name     string
		document string
		expects  string
	}{
		{name: "the listing as rendered"},
		{name: "a null document", document: `null`, expects: `"{" was expected`},
		{name: "an array document", document: `[]`, expects: `"{" was expected`},
		{name: "a number document", document: `42`, expects: `"{" was expected`},
		{name: "files as an object", document: `{"files":{}}`, expects: `"[" was expected`},
		{name: "files as null", document: `{"files":null}`, expects: `"[" was expected`},
		{name: "files as a number", document: `{"files":42}`, expects: `"[" was expected`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var spec chainSpec
			if tc.document != "" {
				spec.mutateFiles = func([]byte) []byte { return []byte(tc.document) }
			}
			raw, manifestJSON := chainStreamFixture(t, spec)
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if tc.expects == "" {
				if err != nil {
					t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s) error = %v, want refusal", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.expects) {
				t.Fatalf("verifyChainStream(%s) message does not carry %s", tc.name, tc.expects)
			}
		})
	}
}

func TestVerifyChainExemptsOnlyAFileRowForTheFilesManifest(t *testing.T) {
	t.Parallel()

	// The exemption that skips FILES.json's own digest comparison covers a FILE
	// row and nothing else. A row giving it any other ftype is judged by the
	// rule above the skip, which refuses a non-file row over a regular file -
	// the rule TestVerifyChainRejectsARegularFileListedAsSomethingElse pins for
	// an ordinary path. What is pinned here is that the exemption does not lift
	// it for this one name.
	//
	// The control is the exemption itself, on the same fixture: a file row
	// naming a digest the listing cannot have computed, which is accepted
	// exactly because it is skipped.
	//
	// Widening the exemption - the `if key == helpers.FilesManifestFileName`
	// skip in checkListedRow hoisted above the ftype branch, applied through go
	// test -overlay so no production file is edited - fails the refusing row:
	//
	//	chain_test.go:2293: verifyChainStream(a dir row for FILES.json) error = <nil>, want refusal
	cases := []struct {
		name    string
		row     string
		wantErr bool
	}{
		{name: "a file row naming a digest it cannot have computed", row: listingFileRowJSON(testFilesName, testOtherDigest)},
		{name: "a dir row", row: `{"name":"` + testFilesName + `","ftype":"` + testFtypeDir + `"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, manifestJSON := chainStreamFixture(t, chainSpec{
				entries:     chainEntries(),
				mutateFiles: replaceOnce(t, `{"files":[`, `{"files":[`+tc.row+`,`),
			})
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s for %s) error = %v, want refusal", tc.name, testFilesName, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verifyChainStream(%s for %s) error = %v, want no error", tc.name, testFilesName, err)
			}
		})
	}
}

func TestVerifyChainRejectsATruncatedSkippedValue(t *testing.T) {
	t.Parallel()

	// skipListingValue consumes the value under a key this reader does not
	// read, and what this pins is that the consumption ends when the document
	// does. The arm it reaches guards a loop rather than a verdict: a value
	// that never closes leaves the decoder handing back io.EOF at a depth that
	// never returns to zero, inside the one function whose whole job is to
	// consume bytes an attacker chose.
	//
	// The archive carries nothing but the two documents, for the reason
	// TestVerifyChainRejectsAListingOfTheWrongShape gives: with any other entry
	// in it, a reader that fell through onto an empty listing would be refused
	// by the reverse rule instead, and the arm would look pinned when nothing
	// had touched it.
	//
	// The control is the same document with the skipped value closed, and two
	// mutations measured what its acceptance is worth - both applied through
	// go test -overlay so no production file is edited. A skip stopping one
	// token short, and one running one token past the value, each leave the
	// decoder somewhere other than the "files" key, and each makes the control
	// row fail with `collection manifest chain does not match: FILES.json
	// carries content after its top-level object`.
	//
	// So the control bounds the skip's endpoint to within a token rather than
	// to that key exactly, and the gap is worth naming: a skip swallowing the
	// whole "files" pair would leave the decoder at the closing brace, where
	// the document ends cleanly and - with no entry in the archive to be
	// unlisted - an empty listing is accepted. Measured by handing the same
	// fixture the document `{"extra":[1,2]}`, which is that state written out:
	// accepted, no error. No single-token error reaches that position.
	//
	// What no row here separates is returning the error from returning nil:
	// with `return listingParseError(err)` replaced by `return nil`, both rows
	// still pass, since walkListingDocument's own dec.Token() hits the same
	// io.EOF one iteration later and renders it through the same helper.
	// Measured, the message is identical either way: `collection manifest chain
	// does not match: FILES.json does not parse: EOF`.
	//
	// Dropping the arm entirely - `tok, err := dec.Token()` and its error check
	// becoming `tok, _ := dec.Token()`, applied through go test -overlay so no
	// production file is edited - fails no assertion at all, it spins: the
	// token is nil, no delimiter matches it, and the loop asks for the next
	// one. The artifact is therefore a timeout, from a run under an explicit
	// `-timeout 30s`, and it is quoted trimmed to its first three lines. The
	// goroutine dump under them names manifest.skipListingValue, called from
	// manifest.walkListingDocument and running inside
	// encoding/json.(*Decoder).Token; those frames are dropped here because
	// internal/proseaudit bans a comment citing a production file's line.
	//
	//	panic: test timed out after 30s
	//		running tests:
	//			TestVerifyChainRejectsATruncatedSkippedValue/a_value_that_runs_out (30s)
	cases := []struct {
		name     string
		document string
		wantErr  bool
	}{
		{name: "a value that closes", document: `{"extra":[1,2],"files":[]}`},
		{name: "a value that runs out", document: `{"extra":[1,2`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, manifestJSON := chainStreamFixture(t, chainSpec{
				mutateFiles: func([]byte) []byte { return []byte(tc.document) },
			})
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}
