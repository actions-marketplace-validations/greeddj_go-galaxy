package archive

// This file measures the figure helpers.ArchiveProbeMaxBytes is derived from -
// how far archive/tar can be made to read before Next returns its first header
// - by building the maximal composite that reaches it and handing it to
// archive/tar. It lives apart from archive_test.go for the reason
// probe_decompressor_test.go states about itself: archive_test.go's comments
// cite their own line numbers, so an import or a helper added there shifts
// citations this has no business re-running. Nothing is added there; five
// things it already declares - tarBlockSize, putTarOctal, sealTarBlock,
// countingReader and probeArchiveBytes - are used where they stand.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strconv"
	"strings"
	"testing"
)

const (
	// metaBodyMaxBytes is archive/tar's own maxSpecialFileSize, spelled here
	// because it is unexported there: the 1 MiB it allows one meta header's
	// body and one sparse map alike. See helpers.ArchiveProbeMaxBytes for the
	// measurement of both bounds.
	metaBodyMaxBytes = 1 << 20
	// sparseEntryName is the name the composite's ordinary header carries. Its
	// GNU.sparse.name record and its long-name meta headers spell the same
	// value, so no part of the composite disagrees about which file it
	// describes.
	sparseEntryName = "collection/README.md"
	// compositeModTime stamps every block the composite builds, so the fixture
	// never depends on wall-clock time.
	compositeModTime = int64(1704067200) // 2024-01-01T00:00:00Z
)

// TestMetaHeaderCeilingIsWhatArchiveTarReads measures the floor
// helpers.ArchiveProbeMaxBytes has to clear, against archive/tar rather than
// against that constant's own prose: the maximal composite the comment
// describes is built here and archive/tar is made to read it.
//
// It is the runtime half of a cross-check whose structural half is
// TestArchiveProbeMaxBytesClearsTheMetaHeaderCeiling, in
// probe_decompressor_test.go. That one says the constant clears the documented
// figure, and needs no builder to say it; this one says the documented figure
// is what archive/tar actually does. Neither covers the other: lowering the
// constant fails only the structural half, while a toolchain whose archive/tar
// reads a different number of bytes before returning its first header - a go
// directive bump being the routine way that happens - fails only this one.
//
// 4,196,352 is hand-spelled rather than computed from
// helpers.ArchiveProbeMaxBytes or from a term formula built out of it: an
// expectation derived from the value under test follows every mutation of it
// and can never fail one.
//
// The straddle is what makes the first assertion a measurement of a MAXIMUM
// rather than of a builder that happens to emit 4,196,352 bytes: the same
// composite with a sparse map one block longer is refused. Measured on
// go1.27.1, the toolchain go.mod pins, that refusal reads "archive/tar: sparse
// map too long" and arrives after the identical 4,196,352 bytes, archive/tar
// refusing the longer map rather than reading it. The text is recorded rather
// than asserted, so archive/tar's own wording stays free to change.
//
// The gzip arm ties the measurement to the probe. It pins the direction a
// stale figure fails in - the probe refusing an archive archive/tar accepts -
// against behavior rather than against the constant's arithmetic.
func TestMetaHeaderCeilingIsWhatArchiveTarReads(t *testing.T) {
	t.Parallel()

	const (
		ceilingBytes     = 4_196_352
		acceptedMapBlock = 2048
		refusedMapBlocks = 2049
	)

	raw := buildMetaCeilingComposite(t, acceptedMapBlock)

	counter := &countingReader{r: bytes.NewReader(raw)}
	if _, err := tar.NewReader(counter).Next(); err != nil {
		t.Fatalf("maximal composite: Next = %v, want the ordinary header it ends on", err)
	}
	if counter.read != ceilingBytes {
		t.Fatalf("maximal composite: archive/tar read %d bytes before its first header, want %d",
			counter.read, ceilingBytes)
	}

	over := buildMetaCeilingComposite(t, refusedMapBlocks)
	if _, err := tar.NewReader(bytes.NewReader(over)).Next(); err == nil {
		t.Fatalf("composite with a %d-block sparse map: Next = %v, want a refusal",
			refusedMapBlocks, err)
	}

	var gzipped bytes.Buffer
	gz := gzip.NewWriter(&gzipped)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("failed to compress the maximal composite: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	if err := probeArchiveBytes(t, gzipped.Bytes()); err != nil {
		t.Fatalf("maximal composite: ProbeTarGz = %v, want it accepted inside the scan bound", err)
	}
}

// buildMetaCeilingComposite renders the composite helpers.ArchiveProbeMaxBytes
// is derived against, as raw uncompressed tar bytes: an 'L', a 'K' and an 'x'
// meta header each carrying a maximal body, then an ordinary header whose data
// section holds a PAX 1.0 sparse map of mapBlocks blocks. The bytes are raw
// because what is measured is archive/tar rather than the probe; a caller that
// wants the probe compresses them itself.
//
// Exactly one 'x' is present, and that is what the sparse records depend on
// rather than a matter of ordering: archive/tar assigns each meta kind's value
// rather than merging it, so a second 'x' would discard this one's records
// whole.
//
// Every block is assembled by hand, at the offsets archive/tar's own reader
// parses them from, because tar.Writer refuses the three meta typeflags
// outright - archive_test.go's buildMetaHeaderChainArchive quotes that
// refusal - and has no field for a sparse map. The ordinary header follows the
// same path rather than mixing a writer into a stream it could not produce
// three quarters of.
func buildMetaCeilingComposite(t *testing.T, mapBlocks int) []byte {
	t.Helper()

	sparseMap := buildPAXSparseMap(t, mapBlocks)

	// The records the ordinary header needs to read as PAX 1.0 sparse, padded
	// out to a maximal body by one filler record. Every fragment in that map is
	// an empty one, which is why a realsize of zero validates: a zero-length
	// fragment at offset zero lies inside a zero-size file, so archive/tar's own
	// sparse validation accepts as many of them in a row as the map cares to
	// spell.
	records := paxRecord("GNU.sparse.major", "1") +
		paxRecord("GNU.sparse.minor", "0") +
		paxRecord("GNU.sparse.name", sparseEntryName) +
		paxRecord("GNU.sparse.realsize", "0")
	records += paxFillerRecord(t, "comment", metaBodyMaxBytes-len(records))

	// The 'L' and 'K' bodies are a name followed by NUL padding rather than a
	// megabyte of text: what the ceiling counts is bytes read, which is the
	// declared body size either way.
	longName := make([]byte, metaBodyMaxBytes)
	copy(longName, sparseEntryName)

	var buf bytes.Buffer
	buf.Grow(3*(tarBlockSize+metaBodyMaxBytes) + tarBlockSize + len(sparseMap) + 2*tarBlockSize)
	for _, chunk := range [][]byte{
		gnuMetaHeaderBlock(tar.TypeGNULongName, metaBodyMaxBytes), longName,
		gnuMetaHeaderBlock(tar.TypeGNULongLink, metaBodyMaxBytes), longName,
		paxMetaHeaderBlock(metaBodyMaxBytes), []byte(records),
		ordinaryHeaderBlock(int64(len(sparseMap))), sparseMap,
		// The two zero blocks that terminate a tar stream. The walk stops on
		// the ordinary header above and never reads this far, but an archive
		// without them is not one archive/tar could have been handed.
		make([]byte, 2*tarBlockSize),
	} {
		buf.Write(chunk)
	}
	return buf.Bytes()
}

// buildPAXSparseMap renders a PAX 1.0 sparse map occupying exactly blocks tar
// blocks: the fragment count, then that many offset/length pairs, NUL-padded
// to the block boundary.
//
// Every pair is spelled "0\n0\n", the shortest a pair can be, so the map
// describes as many fragments as its bytes allow and the read it forces is a
// function of the block count alone. The padding is never parsed: archive/tar
// reads this map a whole block at a time and stops on the last newline it
// needs, which the count below places inside the final block. The second guard
// refuses a count that would leave that block newline-free, since such a map is
// one of blocks-1 blocks wearing this one's name.
func buildPAXSparseMap(t *testing.T, blocks int) []byte {
	t.Helper()

	target := blocks * tarBlockSize
	mapLen := func(entries int) int { return len(strconv.Itoa(entries)) + 1 + 4*entries }

	// The largest fragment count that still fits: four bytes per fragment,
	// plus the count itself spelled once ahead of them and its own newline.
	entries := (target - 8) / 4
	for mapLen(entries+1) <= target {
		entries++
	}
	if entries < 1 || mapLen(entries) > target {
		t.Fatalf("no fragment count renders a sparse map of %d bytes", target)
	}
	if mapLen(entries) <= target-tarBlockSize {
		t.Fatalf("a %d-fragment map ends at %d bytes, short of the last of %d blocks",
			entries, mapLen(entries), blocks)
	}

	var buf bytes.Buffer
	buf.Grow(target)
	buf.WriteString(strconv.Itoa(entries))
	buf.WriteByte('\n')
	for range entries {
		buf.WriteString("0\n0\n")
	}
	buf.Write(make([]byte, target-buf.Len()))
	return buf.Bytes()
}

// paxRecord renders one PAX extended record in the "<len> <key>=<value>\n"
// form archive/tar's parser reads back, where <len> counts its own digits -
// which is why the length is computed twice: spelling it can push the record
// across a power of ten.
func paxRecord(key, value string) string {
	const padding = 3 // the space, the '=' and the newline

	size := len(key) + len(value) + padding
	size += len(strconv.Itoa(size))
	record := strconv.Itoa(size) + " " + key + "=" + value + "\n"
	if len(record) != size {
		record = strconv.Itoa(len(record)) + " " + key + "=" + value + "\n"
	}
	return record
}

// paxFillerRecord renders a record of exactly target bytes. archive/tar parses
// an 'x' body whole and refuses the header when a record in it is malformed, so
// a body padded out to a maximal special file has to be padded with a record
// rather than with filler.
func paxFillerRecord(t *testing.T, key string, target int) string {
	t.Helper()

	const padding = 3 // the space, the '=' and the newline

	valueLen := target - len(key) - padding - len(strconv.Itoa(target))
	if valueLen < 1 {
		t.Fatalf("a %q record cannot be filled out to %d bytes", key, target)
	}
	record := paxRecord(key, strings.Repeat("a", valueLen))
	if len(record) != target {
		t.Fatalf("filler record is %d bytes, want %d", len(record), target)
	}
	return record
}

// gnuMetaHeaderBlock assembles one GNU meta header block ('L' or 'K') for a
// body of size bytes, carrying the GNU magic and version archive/tar needs to
// recognize the block as GNU format at all.
func gnuMetaHeaderBlock(typeflag byte, size int64) []byte {
	blk := newHeaderBlock("././@LongLink", typeflag, size)
	copy(blk[257:265], "ustar  \x00")
	sealTarBlock(blk)
	return blk
}

// paxMetaHeaderBlock assembles the 'x' block, spelling its name the way a real
// writer does.
func paxMetaHeaderBlock(size int64) []byte {
	blk := newHeaderBlock("PaxHeaders.0/README.md", tar.TypeXHeader, size)
	copy(blk[257:263], "ustar\x00")
	copy(blk[263:265], "00")
	sealTarBlock(blk)
	return blk
}

// ordinaryHeaderBlock assembles the header the walk finally returns. Its size
// field is the PHYSICAL size - the sparse map that follows it - since the
// logical size arrives separately, in the 'x' header's GNU.sparse.realsize
// record.
func ordinaryHeaderBlock(size int64) []byte {
	blk := newHeaderBlock(sparseEntryName, tar.TypeReg, size)
	copy(blk[257:263], "ustar\x00")
	copy(blk[263:265], "00")
	sealTarBlock(blk)
	return blk
}

// newHeaderBlock fills the fields every block of this composite shares. Its
// caller supplies the magic and seals the checksum, since the magic is what
// separates the GNU blocks from the POSIX ones and the checksum covers it.
func newHeaderBlock(name string, typeflag byte, size int64) []byte {
	blk := make([]byte, tarBlockSize)
	copy(blk[0:100], name)           // name
	putTarOctal(blk[100:108], 0o644) // mode
	putTarOctal(blk[108:116], 0)     // uid
	putTarOctal(blk[116:124], 0)     // gid
	putTarOctal(blk[124:136], size)  // size
	putTarOctal(blk[136:148], compositeModTime)
	blk[156] = typeflag // typeflag
	return blk
}
