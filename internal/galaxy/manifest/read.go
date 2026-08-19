package manifest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/gzipstream"
)

// errScanLimitReached is the verdict readFromTarGzStream's own limitReader
// carries. It is a separate value from the sentinel a caller sees purely so the
// reader, which counts bytes and knows nothing about manifests, does not have
// to name one; walkError is the single place it becomes
// helpers.ErrManifestNotFound.
var errScanLimitReached = errors.New("manifest scan limit reached")

// ReadFromTarGz returns the bytes of the MANIFEST.json a collection artifact
// carries at the top level of its tar stream. artifactPath names a file this
// run downloaded or produced; nothing is written and no byte of the archive is
// unpacked anywhere.
//
// The entry has to be a regular file whose name cleans to exactly
// helpers.ManifestFileName, so a MANIFEST.json some directory inside the
// artifact carries is walked past rather than returned: a collection's manifest
// is the one at its root, and a nested one belongs to something else - a
// fixture, a vendored tree, or a decoy placed where a looser match would find
// it first. The first entry that qualifies wins.
//
// In practice exactly one entry is ever read. `ansible-galaxy collection build`
// writes MANIFEST.json as the first entry of the archive, and this project's
// own internal/testing/fakegalaxy does the same, so the walk below exists for a
// hostile or exotic builder rather than for the normal case.
//
// Four refusals bound it, and each is a verdict rather than an abandoned
// search:
//
//   - An entry naming itself in more than helpers.ArchiveMaxEntryNameLen bytes
//     is refused before anything has rendered that name. This walk retains no
//     name at all, so the cap is not here for the reason the chain check
//     carries it; it is here because the refusal below quotes one, and
//     archive/tar accepts a GNU long name of 1,048,575 bytes - a megabyte of
//     archive-chosen text put on an operator's terminal to report that the
//     entry carrying it declared too large a size. internal/safeout bounds
//     which characters reach that terminal, never how many.
//   - An entry whose header declares more than helpers.ArchiveMaxEntrySize is
//     refused where its header is read, before a byte of its body is pulled
//     through the decompressor, exactly as archive.chargeEntrySize refuses one.
//     It applies to every entry rather than to the manifest alone, because the
//     walk has to read past all of them to reach the manifest.
//   - helpers.ManifestScanMaxBytes bounds the walk, counted on the bytes taken
//     OUT of the decompressor rather than on the compressed body, since gzip
//     input places no bound at all on the tar stream it yields. Crossing it is
//     helpers.ErrManifestNotFound, and so is a tar stream that ends with no
//     match: an artifact that has not presented its manifest inside that window
//     has not presented one, whether because it carries none or because the
//     entry it names does not fit inside the window.
//   - A MANIFEST.json entry carrying zero bytes is helpers.ErrManifestNotFound
//     too, and that refusal has to live here rather than downstream: a detached
//     signature over a zero-byte document is a perfectly valid signature
//     whenever a keyring key made it, so a verifier handed those bytes cannot
//     tell "this artifact's manifest is empty" from "this artifact's manifest
//     is this". Refusing the empty document is what keeps the question from
//     being asked.
//
// It never reports success with no bytes: every path returning a nil error
// returns a manifest of at least one byte, so a caller cannot mistake "found
// nothing" for "found an empty manifest".
//
// ctx bounds the walk on both sides of the decompressor, so a canceled or
// expired read stops at the next read of either kind rather than at the end of
// the archive. A refusal carrying that cancellation is returned as the
// cancellation rather than as a verdict on the artifact's shape - see
// decompressorOpenError.
func ReadFromTarGz(ctx context.Context, artifactPath string) ([]byte, error) {
	//nolint:gosec // artifactPath names an artifact this run downloaded or produced.
	file, err := os.Open(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open artifact to read its manifest: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	data, err := readFromTarGzStream(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", artifactPath, err)
	}
	return data, nil
}

// readFromTarGzStream walks r as a gzipped tar and returns the top-level
// manifest, under the bounds ReadFromTarGz documents. It is split from the
// open so that the file handle's lifetime and the walk stay separate concerns,
// and it does not close r.
//
// It reads through internal/gzipstream, the one seam this module opens a gzip
// reader over foreign bytes through, rather than a second notion of "gzip" - so
// an artifact this walk accepts is one the extractor would also have opened,
// and the member rule that seam enforces (helpers.ErrEmptyGzipMember) bounds
// this pass exactly as it bounds an unpack. archive.ProbeTarGz makes the same
// argument for the same seam, sized smaller.
func readFromTarGzStream(ctx context.Context, r io.Reader) ([]byte, error) {
	uncompressed, err := gzipstream.NewReader(ctx, r)
	if err != nil {
		return nil, decompressorOpenError(err)
	}
	defer func() {
		_ = uncompressed.Close()
	}()

	limited := &limitReader{r: uncompressed, over: errScanLimitReached, max: helpers.ManifestScanMaxBytes}
	tarReader := tar.NewReader(limited)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: the tar stream ended after %d bytes", helpers.ErrManifestNotFound, limited.n)
		}
		if err != nil {
			return nil, walkError(err)
		}
		// Measured ahead of the refusal below, the one message this walk
		// renders an archive-chosen name in.
		if err := checkEntryNameLength(header.Name); err != nil {
			return nil, err
		}
		// Charged whether or not this is the entry being searched for, since
		// the walk has to read past every other one to reach it.
		if header.Size > helpers.ArchiveMaxEntrySize {
			return nil, fmt.Errorf("%w %q: %d bytes", helpers.ErrArchiveEntryIsTooLarge, header.Name, header.Size)
		}
		if !topLevelManifest(header) {
			continue
		}
		return readManifestBody(tarReader, header.Size)
	}
}

// topLevelManifest reports whether header names the artifact's own manifest
// rather than one carried by some directory inside it.
//
// The name is cleaned with path, not filepath, because a tar entry's name is
// slash-separated by specification on every platform: a backslash in it is an
// ordinary character inside a single path element, not a separator that could
// turn a nested entry into a top-level one on one operating system and not
// another.
func topLevelManifest(header *tar.Header) bool {
	return header.Typeflag == tar.TypeReg && path.Clean(header.Name) == helpers.ManifestFileName
}

// readManifestBody reads size bytes of manifest out of the entry r is currently
// positioned on, and refuses an entry that carries none.
//
// The buffer is sized from the declared size, clamped to what the scan bound
// permits, so a header declaring far more than it delivers cannot turn a few
// bytes of archive into a matching allocation - the amplification shape
// signature.checkPacketFraming exists to close on a different parser.
//
// Clamping bounds the allocation and nothing else; what makes the non-empty
// return structural is archive/tar, which hands back exactly the declared
// number of bytes or fails. Measured against a header declaring 100 bytes over
// a body of 10: with the archive's own trailer behind it the read returns 100
// bytes, the last 90 drawn from that trailer, and a nil error; with the stream
// ending inside the body instead it returns io.ErrUnexpectedEOF and no bytes at
// all. So past the zero check below, a nil error means a manifest of exactly
// size bytes, and size is at least one.
func readManifestBody(r io.Reader, size int64) ([]byte, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: the entry named %s is empty", helpers.ErrManifestNotFound, helpers.ManifestFileName)
	}

	// The extra bytes.MinRead is the headroom ReadFrom wants available before
	// it stops, so the buffer is sized once and never grown.
	buf := bytes.NewBuffer(make([]byte, 0, min(size, helpers.ManifestScanMaxBytes)+bytes.MinRead))
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, walkError(err)
	}
	return buf.Bytes(), nil
}

// decompressorOpenError names a decompressor that refused to open over an
// artifact as the shape verdict it is - except when what refused it is the
// caller's own cancellation, which is returned unchanged.
//
// The exception exists because the context now reaches the decompressor:
// gzipstream observes ctx on the compressed side, so a canceled or expired
// pass fails its constructor with ctx.Err() rather than with anything about
// gzip. The exit class is right either way, since cmd/go-galaxy/exitcode
// checks cancellation ahead of every other class, but the message would tell
// an operator who pressed Ctrl-C that the artifact is not a tar.gz. Only the
// two context values are excepted, so every genuine refusal this function is
// reached with - an error page, an uncompressed tar - still carries the
// sentinel. Both readers in this package share it, since both open the same
// kind of stream over the same kind of bytes.
//
// A gzip member producing no bytes is not among them, and cannot be: opening a
// stream parses its first member's gzip HEADER, which such a member has, so
// the constructor succeeds and helpers.ErrEmptyGzipMember arrives from the
// walk instead. Measured through ReadFromTarGz over a single twenty-byte empty
// member, the verdict is walkError's - "failed to read the artifact's tar
// stream: gzip stream carries a member that produces no bytes", with
// errors.Is against helpers.ErrArtifactNotTarGz reporting false.
// archive.notTarGzError states that same shape as covered rather than absent,
// and is right to: ProbeTarGz routes its walk through that function as well as
// its open, where this package routes only the open.
func decompressorOpenError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", helpers.ErrArtifactNotTarGz, err)
}

// walkError renders one failure from the tar walk: the scan bound reported as
// the not-found verdict it means, anything else as itself.
//
// The bound is reported the same way whether it was crossed hunting for the
// entry or reading the one that was found, because this package's contract is
// the manifest an artifact presents within the window - an entry whose bytes
// run past the window was not presented within it either.
func walkError(err error) error {
	if errors.Is(err, errScanLimitReached) {
		return fmt.Errorf("%w within the first %d bytes of the tar stream",
			helpers.ErrManifestNotFound, helpers.ManifestScanMaxBytes)
	}
	return fmt.Errorf("failed to read the artifact's tar stream: %w", err)
}

// limitReader counts the bytes read out of an artifact's decompressor and fails
// with over once they exceed max, so a pass over an archive is bounded by what
// it actually costs to read rather than by what the archive's headers declare
// or by how large its compressed body is.
//
// The verdict is a field rather than a constant of this file because a pass
// over an archive bounds its own thing and has to say so: a scan for one
// document reports that the document was not presented inside the window, while
// a pass that reads an artifact to its end reports the same
// decompressed-stream cap the extractor reports. One reader, a verdict per
// caller, and no caller has to know another's.
//
// It counts on the decompressed side rather than on the compressed source for
// the same reason archive.decompressedLimitReader does: gzip input places no
// bound at all on the tar stream it yields. Cancellation is a different
// question and is watched on both sides - see internal/gzipstream for the one
// this reader cannot see, a member spending wire bytes without producing any.
type limitReader struct {
	r    io.Reader
	err  error
	over error
	max  int64
	n    int64
}

// Read passes bytes through and fails with over once the cumulative count
// exceeds max, replacing whatever the decompressor itself returned (its own
// io.EOF included), since a stream past the ceiling must never be reported as a
// clean read.
//
// The crossing call returns zero and drops the bytes it just read, which
// io.Reader permits, and that is what produces the refusal rather than merely
// recording it: the helpers between this reader and the tar walk treat a
// fully-satisfied request as success and discard the error returned alongside
// it - io.ReadAtLeast's `if n >= min { err = nil }`, reached by archive/tar for
// every 512-byte header block, and io.CopyN's `if written == n { return n, nil }`,
// reached by archive/tar's discard when it reads past an entry body this walk
// skipped. Handing back none of the bytes leaves them nothing to satisfy.
// archive.decompressedLimitReader keeps the identical mechanics for its own
// cap, deliberately: two size caps in one program that disagree about what a
// crossing read returns is a trap for whoever reads only one of them.
//
// The refusal is sticky: once it fires, every later call returns it without
// touching the underlying reader, so a caller that keeps reading cannot drain
// further bytes past the ceiling.
//
// Clamping p bounds the overrun to a single byte rather than to a whole read
// buffer, which is also what makes the byte count the error reports exact. It
// is not what produces the refusal.
func (r *limitReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	// Keeps p[:remaining+1] in range: a max below zero drives remaining
	// negative, and at -2 or lower that reslice panics.
	remaining := max(r.max-r.n, 0)
	// Taking this branch means remaining < len(p), hence remaining+1 <= len(p),
	// so the reslice is always within p.
	if remaining < int64(len(p)) {
		p = p[:remaining+1]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.max {
		r.err = fmt.Errorf("%w: read %d bytes, limit is %d bytes", r.over, r.n, r.max)
		return 0, r.err
	}
	return n, err
}
