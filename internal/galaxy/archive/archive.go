// Package archive unpacks collection tarballs. ExtractTarGz and
// ExtractTarGzStream walk a gzipped tar into a destination directory,
// refusing what must not land there: an empty, absolute or escaping entry
// path, a path component that is itself a symlink, a symlink target leaving
// the destination, a second entry colliding with a path already written, a
// gzip member that produces no bytes at all, and anything breaching the
// entry-count, entry-size, total-size or decompressed-stream caps declared in
// internal/galaxy/helpers. The refusals are named by sentinel, so a failing
// extraction says which rule stopped it.
//
// The context is checked on both sides of the decompressor - here on every
// read taken out of it, and in internal/gzipstream on every read taken off the
// compressed source - so a canceled unpack stops at the next read of either
// kind rather than at the end of the archive. That is still a property of
// where the checks sit rather than a promise about when they fire, but the
// compressed-side one covers the shape the decompressed-side one cannot see.
// Measured on go1.26.6, darwin/arm64 (Apple M3 Pro): one 80 MB gzip member
// whose deflate stream is nothing but zero-length stored blocks produces no
// output at all, so a read of it canceled at 20 ms ran the whole stream out
// and returned 210 ms later reporting nil when only the decompressed side was
// watched, against 21 ms and the caller's own context error once the
// compressed side is watched too.
//
// ProbeTarGz answers the cheaper question of whether a file is even shaped
// like a gzipped tar, for a caller holding bytes nothing else has looked at,
// and FileHashSHA256 hashes one. Nothing here resolves a path through an
// os.Root: the caller hands this package a destination directory it has
// already contained, and containment stays that caller's property.
package archive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/gzipstream"
)

// The probe's decompressor is sized for the one question ProbeTarGz asks -
// is there a tar header at the front of this stream - and not for a streaming
// unpack, so it reserves kilobytes where extractTarGzStream's own reader,
// gzipstream.NewReader at pgzip's defaults, reserves megabytes.
//
// probeGzipBlocks is 1, and what that gives up is all overlap between
// decompressing and consuming: pgzip's readahead goroutine takes the only
// buffer there is, fills it, and then parks on an empty pool until the
// consumer drains that block and hands it back (see gunzip.go's Read
// returning a block to blockPool). The overlap costs nothing to give up for
// an archive whose first header is an ordinary one: the probe stops inside
// that first block - archive/tar pulls one 512-byte header out of it and the
// walk ends - so a second block would be filled for a read that never comes.
// Measured on an Apple M3 Pro (darwin/arm64) over a single-entry archive of
// 5,242,880 raw bytes, deflated by pgzip's own writer at its default level to
// 5,243,941, one probe per operation: about 31,400 ns/op and 108,800 B/op at
// one block against about 52,000 ns/op and 175,400 B/op at two, the
// difference being the second block itself. Both absolute figures belong to
// that stream rather than to a probe in general, since how the fixture was
// deflated decides them - the same content through compress/gzip at its own
// default level renders to a different compressed size and measures about
// 88,000 ns/op at one block. What carries the argument is the delta between
// the two sizings, not either number.
//
// The one shape that reads further is a chain of extended headers ahead of
// the first ordinary one, and one block does not stall on it: the consumer
// drains the block, the readahead goroutine refills it, and the walk goes on
// with one block held rather than a growing number of them - measured, a PAX
// path record padded to 900 KiB is walked through and returns nil, and one
// padded to 2 MiB fails with "archive/tar: header field too long", which is
// archive/tar's own ceiling on a single special file. That ceiling bounds
// each such header and not how many of them an archive may chain (see
// chargeEntrySize), so how far this walk reads is the archive's choice up to
// helpers.ArchiveProbeMaxBytes at either sizing; what one block changes is the
// buffer it reads through, not the distance.
//
// probeGzipBlockSize may not be 512 or less: pgzip.NewReaderN coerces any such
// value back to its own 1 MiB default (gunzip.go's "Account for too small
// values"), which is the sizing these constants exist to avoid.
//
// The size is load-bearing rather than a micro-optimization. This reader is
// the only decompressor the artifact-download pool ever opens - the prefetcher
// builds its download deps with a nil extracted store, so that pool reaches
// this probe and never an ingest - so helpers.MaxDefaultDownloadWorkers times
// this budget is that pool's whole concurrent-decompressor claim. Sizing it
// like the extractor's would charge a network-bound pool against the memory
// budget helpers.MaxDefaultInstallWorkers is derived from.
const (
	probeGzipBlockSize = 64 << 10
	probeGzipBlocks    = 1
)

// ExtractTarGz extracts a tar.gz archive into dstDir with safety checks.
//
// ctx bounds the unpack: it is checked on every read taken out of the
// decompressor and on every read taken off the compressed source, so a
// canceled or expired context stops the extraction at the next read of either
// kind rather than at the end of the archive - which bounds where the checks
// sit and not how long one read may take, the qualifier this package's own doc
// comment measures. The error a fired check produces
// keeps context.Canceled reachable through errors.Is, deliberately - unlike
// the sentinels that describe why work ended on their own, this one really
// does mean the caller stopped it, and the run must exit as interrupted.
func ExtractTarGz(ctx context.Context, tarGzFile, dstDir string) error {
	info, err := os.Stat(tarGzFile)
	if err != nil {
		return fmt.Errorf("failed to stat file %s: %w", tarGzFile, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%w: %s", helpers.ErrFileIsEmpty, tarGzFile)
	}

	//nolint:gosec // tarGzFile is a user-provided archive path expected by CLI.
	file, err := os.Open(tarGzFile)
	if err != nil {
		return fmt.Errorf("failed to open tar.gz file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	return ExtractTarGzStream(ctx, file, dstDir)
}

// ExtractTarGzStream extracts a tar.gz stream into dstDir with safety checks.
// It does not close r. ctx bounds the unpack, as described on ExtractTarGz.
func ExtractTarGzStream(ctx context.Context, r io.Reader, dstDir string) error {
	return extractTarGzStream(ctx, r, dstDir, helpers.ArchiveMaxDecompressedSize)
}

// extractTarGzStream is ExtractTarGzStream with the decompressed-stream cap
// injected, so a test can drive that cap with a fixture measured in kilobytes
// instead of one that has to reach the production ceiling. The entry cap has
// no such seam here, and needs none: a test that drives it hands its own
// tar.Reader to extractTarEntries and skips the decompressor entirely.
//
// The stream cap is the extractor's real bound on decompression bombs, and it
// is deliberately applied here rather than inside extractTarEntries: it has to
// count what leaves the gzip reader, which includes every byte archive/tar
// consumes without ever handing a header back (see chargeEntrySize).
func extractTarGzStream(ctx context.Context, r io.Reader, dstDir string, maxDecompressed int64) error {
	uncompressedStream, err := gzipstream.NewReader(ctx, r)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		_ = uncompressedStream.Close()
	}()

	// Cancellation is enforced by a reader rather than by a check inside
	// extractTarEntries, and that is the whole reason the entry loop's
	// signature is untouched. A reader also gives finer granularity than a
	// per-entry check could: archive/tar pulls every 512-byte header block and
	// every byte of an entry body through this reader, so a single enormous
	// entry is interruptible mid-body, which a check between entries would not
	// be. It wraps the decompressed side; the compressed source is watched one
	// layer down, by internal/gzipstream. The two are not redundant: this one
	// stops a walk still being fed out of a block the decompressor has already
	// filled, and that one stops a member spending wire bytes without
	// producing any for this one to see.
	limited := &decompressedLimitReader{r: uncompressedStream, over: helpers.ErrArchiveDecompressedTooLarge, max: maxDecompressed}
	tarReader := tar.NewReader(&contextReader{ctx: ctx, r: limited})
	return extractTarEntries(tarReader, dstDir, helpers.ArchiveMaxEntryCount)
}

// contextReader fails a read once ctx is done, so an unpack stops at the next
// read instead of running to the end of the archive. It reports ctx.Err()
// unwrapped, keeping context.Canceled and context.DeadlineExceeded reachable
// through errors.Is for cmd/go-galaxy/exitcode to classify.
//
// It holds the context in a field rather than taking one per call, which is
// the only shape io.Reader allows: Read's signature is fixed, so a reader
// that must observe cancellation has nowhere else to keep it.
//
//nolint:containedctx // io.Reader cannot take a context per call
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// decompressedLimitReader counts the bytes read out of an archive's
// decompressor and fails once they exceed max, so an archive is bounded by
// what it actually costs to read rather than by what its headers declare.
//
// helpers.NewSizeLimitedReader is deliberately not reused here, and the
// reason is the sentinel rather than the mechanics: it reports
// helpers.ErrResponseTooLarge, which exitcode.isTransportError classifies
// ExitNetwork, so a decompression bomb would be reported to CI as a network
// fault and retried forever. The mechanics are the same on both - zero bytes
// on the crossing call, a sticky error, a clamped p - and keeping them the
// same is deliberate: two size caps in one program that disagree about what a
// crossing read returns is a trap for whoever reads only one of them.
//
// The verdict is a field rather than a constant of this file because a pass
// over an archive bounds its own thing and has to say so: the extractor
// reports the decompressed-stream cap it read an artifact to its end against,
// while ProbeTarGz reports that no tar header was presented inside the window
// it looked in. One reader, a verdict per caller, and no caller has to know
// another's - the same shape manifest.limitReader carries for the same reason.
//
// Neither this type nor contextReader may grow a Seek method: archive/tar's
// discard type-asserts io.Seeker on the reader it was handed and, when that
// succeeds, skips a body without those bytes ever passing through Read, which
// defeats this counter and the cancellation check alike. Measured over two
// Next calls past a 1 MiB body, 1,049,600 bytes reach Read through a plain
// io.Reader against 1,025 through an io.ReadSeeker. The claim is about
// io.Seeker only; no other interface has been measured to do this here, and
// naming one would be a guess.
type decompressedLimitReader struct {
	r    io.Reader
	err  error
	over error
	max  int64
	n    int64
}

// Read passes bytes through and fails with over once the cumulative count
// exceeds max, replacing whatever the decompressor itself returned (its own
// io.EOF included), since a stream past the ceiling must never be reported as
// a clean read.
//
// The crossing call returns zero and drops the bytes it just read, which
// io.Reader permits. Returning them instead is what makes the refusal
// disappear, and the predicate is general: any caller that treats a
// fully-satisfied request as success discards a non-nil error returned
// alongside it. Three instances of that predicate sit between this reader and
// an archive's bytes, all measured: io.ReadAtLeast's `if n >= min { err = nil }`,
// reached by archive/tar's readHeader for every 512-byte header block;
// io.CopyN's `if written == n { return n, nil }`, reached by archive/tar's
// discard when it reads past a skipped entry body; and that same io.CopyN,
// reached by this package's own extractRegularFile. The zero return is what
// produces the refusal against all three.
//
// The refusal is sticky: once it fires, every later call returns it without
// touching the underlying reader, so a caller that keeps reading cannot drain
// further bytes past the ceiling.
//
// Clamping p bounds the overrun to a single byte rather than to a whole read
// buffer, which is also what makes the byte count the error reports exact. It
// is not what produces the refusal.
func (r *decompressedLimitReader) Read(p []byte) (int, error) {
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

func extractTarEntries(tarReader *tar.Reader, dstDir string, maxEntries int64) error {
	var declared, entries int64
	// verifiedDirs memoizes parent-chain components already confirmed, this
	// extraction, to be real (non-symlink) directories. It is scoped to one
	// extraction (one goroutine, one dstDir) and never shared, so a plain
	// map needs no synchronization. See ensureNoSymlinkParents for the
	// correctness argument.
	verifiedDirs := make(map[string]struct{})
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("error reading tar archive: %w", err)
		}
		// entries counts every header tar.Reader.Next hands back, whatever its
		// typeflag, so a tarbomb of many zero-byte directories or hardlinks -
		// which charges nothing against the declared-size budget - is still
		// rejected. Three size-and-count bounds split the tar walk's work, and none subsumes another: the
		// counter bounds headers, the declared-size budget charged alongside it
		// bounds what the headers CLAIM, and helpers.ArchiveMaxDecompressedSize
		// bounds the bytes actually read. Only the first two are applied here,
		// at the same level, for every header; the third has to sit one layer
		// out, in decompressedLimitReader, because the bytes it counts include
		// those archive/tar consumes without ever handing a header back, and
		// because a header's declared size is not an upper bound on what that
		// entry costs to read past at all (see chargeEntrySize). The count
		// check runs before handleTarEntry, so the (maxEntries+1)th entry is
		// rejected before it is ever extracted. What the counter bounds is
		// headers, not inodes: ensureDir creates every missing ancestor of an
		// entry's path, so one entry named a/b/c/f costs four. Inodes are
		// bounded only transitively - every path component's name bytes have to
		// leave the decompressor, so the stream cap is what caps them - and no
		// test pins that weaker bound.
		entries++
		if entries > maxEntries {
			return fmt.Errorf("%w: %d", helpers.ErrArchiveTooManyEntries, maxEntries)
		}
		if err := chargeEntrySize(header, &declared); err != nil {
			return err
		}
		if err := handleTarEntry(tarReader, header, dstDir, verifiedDirs); err != nil {
			return err
		}
	}
}

// chargeEntrySize charges header.Size against the per-entry and per-archive
// byte budgets and, on success, adds it to the running total. It runs on every
// header extractTarEntries is handed, before handleTarEntry dispatches on the
// typeflag, so every typeflag tar.Reader.Next returns is charged - including
// the two shapes that write nothing at all: an entry whose name normalizes
// away (sanitizeArchivePath returning an empty path) and one whose typeflag
// the dispatch switch skips. Three typeflags are structurally exempt, because
// they never reach this function at all: 'x' (PAX extended), 'L'
// (GNULongName) and 'K' (GNULongLink), described below.
//
// header.Size is NOT an upper bound on the bytes archive/tar reads out of the
// decompressed stream for that entry, and this budget must never be read as if
// it were. The size is exact for a regular file, and for a skipped entry that
// still carries a body - tar.Reader.Next must read and discard that body
// before it can parse the following header, so the bytes leave the
// decompressor whether or not anything is written to disk. It is conservative
// for a header-only type, where the reader reads no body. But three shapes
// make it an understatement, by a margin the archive itself chooses:
//
//   - An old-GNU sparse entry ('S'). archive/tar's handleRegularFile sizes the
//     body reader from the header's size field - the physical bytes, which the
//     following Next() discards - and readOldGNUSparseMap then overwrites
//     hdr.Size with the header's realsize field, the logical file size. They
//     are two independent fields, both attacker-chosen, and validateSparseEntries
//     checks the fragment map against the logical one only, so nothing ties
//     them together. What arrives here is the logical size; what is read is the
//     physical one.
//   - A PAX sparse entry: GNU.sparse.size or GNU.sparse.realsize records in an
//     'x' header preceding an ordinary TypeReg entry. Read from archive/tar's
//     readGNUSparsePAXHeaders, which applies those records to hdr.Size after
//     the body reader has already been sized from the entry's own size field,
//     this is the same divergence in a second encoding. That is stated as read
//     from archive/tar's source, not as something measured against this
//     extractor.
//   - The 'x', 'L' and 'K' meta headers. tar.Reader.Next reads each one's body
//     and continues its own loop without ever returning a header, so neither
//     extractTarEntries' entry counter nor this charge observes one. Each body
//     is capped at 1 MiB by archive/tar itself, but their number is capped by
//     nothing: a chain of them is arbitrarily long and costs an archive one
//     compressed kilobyte per megabyte read.
//
// So the declared charge is an early, cheap refusal - it rejects an archive
// that admits its own size before a single body byte is read - and not the
// guarantee. The guarantee is helpers.ArchiveMaxDecompressedSize, enforced by
// decompressedLimitReader on the bytes that actually leave the gzip reader,
// which is the one counter all three shapes above are visible to. Visibility
// is necessary and not sufficient: those bytes were always counted here, and
// what turns counting them into a refusal is that decompressedLimitReader.Read
// hands back none of them, so neither of the two stdlib helpers between it and
// this loop can report a satisfied request and drop the error on the way.
//
// The residual is bounded, not eliminated. The stream cap refuses an artifact
// once it has pulled one byte past helpers.ArchiveMaxDecompressedSize out of
// the decompressor; below that ceiling the amplification is untouched, and it
// is the archive that picks how far below. Measured against this extractor at
// the production cap: eight old-GNU sparse entries, each declaring 1 logical
// byte and each carrying the largest whole number of 512-byte blocks that
// still leaves room for eight headers and the trailer - that is
// (ArchiveMaxDecompressedSize - 8*512 - 1024) / 8 rounded down to a block, so
// 536,869,888 bytes apiece - make a 4,294,964,224-byte stream, 3,072 bytes
// under the cap. It is accepted and returns nil: 3.98 MiB compressed, 1028.7x,
// charged 8 bytes in total. The sparse under-charge is capped, never closed.
//
// Those figures are a recorded measurement that no committed test reproduces,
// and none is required: the fixture pins a shape this extractor deliberately
// accepts, so there is nothing whose removal it would catch, and building it
// costs a 4 GiB compression pass per run. Read them accordingly. The stream
// length and the 8-byte charge follow from the arithmetic above; the
// compressed size and the ratio are a property of the Go release's deflate at
// the time of measurement, so they drift with it and are not invariants.
//
// A header-only type normally charges zero, and every writer measured writes
// zero. Python's tarfile does so on both of its construction paths (gettarinfo
// assigns st_size only for REGTYPE, and TarInfo's own default is 0), which
// covers the corpus this extractor is aimed at, since `ansible-galaxy
// collection build` builds through it; libarchive 3.7.4 writes 0 for a
// directory; and Go's own tar.FileInfoHeader assigns Size only in its
// regular-file arm. GNU tar was not measured, so nothing here claims anything
// about what it writes.
//
// Zero is a convention rather than a rule, and it is the rule for exactly two
// typeflags: ustar states that the size field shall be specified as zero for a
// hardlink and for a symlink. For a directory a nonzero size is sanctioned,
// under two different readings - POSIX.1 reads the field as the number of
// octets the directory may hold, tar(5) as the total size of the files inside
// it - so under the first of those the declared figure is tied to nothing on
// disk at all. Either way the charge here sums size over every header, so an
// archive whose directories declare their subtree totals is charged the sum of
// size * (1 + depth): a file at depth N counts once for itself and once per
// containing directory.
//
// What that costs such an archive is arithmetic on those assumptions, not a
// measurement: at a uniform depth of 8 the 4 GiB total budget is reached at
// about 455 MiB of real content, and below depth 7 the 512 MiB per-entry cap
// binds first anyway, since the outermost directory entry declares the whole
// subtree. Exempting tar.TypeDir from the charge is the remedy if such a
// writer turns up in this corpus; it is not taken now because it buys nothing
// against any writer measured above, and it costs this function the property
// it exists for - charging without branching on the typeflag, which is what
// the two shapes named at the top of this comment, a name that normalizes away
// and a typeflag the dispatch skips, need in order to be charged at all.
//
// This is also why handleTarEntry's default arm carries no refusal of its own.
// Every entry is already charged here and every byte is already counted by the
// stream cap, so a rule down there would put the byte limits back into the
// per-typeflag branches this function exists to lift them out of - and it
// would narrow the set of archives this extractor accepts, since tar.Reader
// hands an old-GNU sparse entry back under its own 'S' typeflag, which that
// arm skips.
func chargeEntrySize(header *tar.Header, declared *int64) error {
	if header.Size < 0 {
		return fmt.Errorf("%w: %s ", helpers.ErrArchiveEntryHasNegativeSize, header.Name)
	}
	if header.Size > helpers.ArchiveMaxEntrySize {
		return fmt.Errorf("%w %s: %d bytes", helpers.ErrArchiveEntryIsTooLarge, header.Name, header.Size)
	}
	if *declared+header.Size > helpers.ArchiveMaxTotalSize {
		return fmt.Errorf("%w: %d bytes", helpers.ErrArchiveExceedsMaxSize, helpers.ArchiveMaxTotalSize)
	}
	*declared += header.Size
	return nil
}

func handleTarEntry(tarReader *tar.Reader, header *tar.Header, dstDir string, verifiedDirs map[string]struct{}) error {
	relPath, err := sanitizeArchivePath(header.Name)
	if err != nil {
		return err
	}
	if relPath == "" {
		return nil
	}
	targetPath := filepath.Join(dstDir, relPath)
	if err := ensureNoSymlinkParents(dstDir, relPath, verifiedDirs); err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return extractDir(targetPath, verifiedDirs)
	case tar.TypeReg:
		return extractRegularFile(tarReader, header, targetPath, verifiedDirs)
	case tar.TypeSymlink:
		return extractSymlink(relPath, targetPath, header, verifiedDirs)
	case tar.TypeLink:
		return extractHardlink(dstDir, targetPath, header, verifiedDirs)
	default:
		// Skipped, not refused. An unnamed typeflag is subject to all three of
		// the extractor's bounds without this arm adding a fourth: it was
		// counted against the entry cap in extractTarEntries and charged
		// against the declared-size budgets in chargeEntrySize before reaching
		// here, and every byte archive/tar reads for it - this header now, its
		// body when the following Next discards it - goes through
		// decompressedLimitReader. See chargeEntrySize for why this arm adds no
		// rule of its own on top.
		return nil
	}
}

func extractDir(targetPath string, verifiedDirs map[string]struct{}) error {
	if err := ensureDir(targetPath, verifiedDirs); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", targetPath, err)
	}
	return nil
}

// ensureDir creates dir unless this extraction already proved it a real,
// non-symlink directory, and records it as one afterwards. It is the only
// place this package creates a directory.
//
// Skipping the syscall is sound for exactly the reason the memo itself is (see
// ensureNoSymlinkParents): a memoized component is a real directory and stays
// one for the rest of the extraction, so os.MkdirAll on it would find it
// already there and return nil. The measured win is not the skipped mkdirat
// alone but the whole walk os.MkdirAll performs - it Stats the full path and,
// on a miss, every ancestor - repeated once per archive entry over a parent
// chain the entries almost always share.
//
// Recording the directory afterwards is what makes the skip reachable at all,
// since the first entry under a new parent has to create it, and it is safe
// for a reason that comes from the caller rather than from here: every
// component of the entry's own path, this one included, was Lstat'd by
// ensureNoSymlinkParents before the entry was dispatched, so a symlink can
// never be what os.MkdirAll found already present. A future caller that
// reaches this function without that check first would break the memo's
// invariant, not merely this optimization.
func ensureDir(dir string, verifiedDirs map[string]struct{}) error {
	if _, ok := verifiedDirs[dir]; ok {
		return nil
	}
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		return err
	}
	verifiedDirs[dir] = struct{}{}
	return nil
}

func extractRegularFile(tarReader *tar.Reader, header *tar.Header, targetPath string, verifiedDirs map[string]struct{}) error {
	if err := ensureDir(filepath.Dir(targetPath), verifiedDirs); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	// Strip every write bit at the moment the file is created. This is the
	// single point in the whole pipeline where content extracted from an
	// artifact becomes read-only, and it is forced rather than chosen: the
	// extracted bytes are later hard-linked into every install that
	// references them, so their mode is shared inode metadata, and masking
	// it anywhere downstream (e.g. in extracted.Materialize) would itself be
	// a write aliased into every other hard link of the same content.
	mode := helpers.ReadOnlyPerm(header.FileInfo().Mode().Perm())
	//nolint:gosec // targetPath is sanitized archive entry under dstDir.
	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return classifyOpenRegularFileError(targetPath, header.Name, err)
	}
	if _, err := io.CopyN(file, tarReader, header.Size); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to write file %s: %w", targetPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close file %s: %w", targetPath, err)
	}
	return nil
}

// classifyOpenRegularFileError runs only after os.OpenFile in
// extractRegularFile has already failed - the happy path pays nothing for
// this. Regular files extract read-only (see extractRegularFile above), so a
// tarball with two entries competing for one path fails this OpenFile with a
// bare permission error against whatever the earlier entry already put
// there, which is undebuggable on its own: with writable extracted files the
// second entry's O_TRUNC open would silently succeed and the last entry
// would win. An os.Stat of the target distinguishes "this path is
// already occupied by something our own extraction just created" from a
// genuine open failure against an absent path (an unwritable destination, a
// vanished parent directory, and so on), and reports the former as
// helpers.ErrArchiveDuplicateEntry naming the offending archive path;
// anything else is returned as the original wrapped error, unchanged.
//
// The check is existence-based rather than errno-based on purpose: a
// regular-file entry landing on an earlier directory entry's path is the
// same actionable fact - two entries want one path - and reporting it under
// one name beats surfacing a raw EISDIR the operator would have to decode.
func classifyOpenRegularFileError(targetPath, entryName string, openErr error) error {
	if _, statErr := os.Stat(targetPath); statErr == nil {
		return fmt.Errorf("%w: %s", helpers.ErrArchiveDuplicateEntry, entryName)
	}
	return fmt.Errorf("failed to create file %s: %w", targetPath, openErr)
}

func extractSymlink(relPath, targetPath string, header *tar.Header, verifiedDirs map[string]struct{}) error {
	linkTarget, err := safeSymlinkTarget(relPath, header.Linkname)
	if err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(targetPath), verifiedDirs); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	if err := os.Symlink(linkTarget, targetPath); err != nil {
		return fmt.Errorf("failed to create symlink %s -> %s: %w", targetPath, linkTarget, err)
	}
	return nil
}

func extractHardlink(dstDir, targetPath string, header *tar.Header, verifiedDirs map[string]struct{}) error {
	linkRel, err := sanitizeArchivePath(header.Linkname)
	if err != nil {
		return err
	}
	if linkRel == "" {
		return fmt.Errorf("%w for %s", helpers.ErrHardlinkTargetIsEmpty, header.Name)
	}
	if err := ensureNoSymlinkParents(dstDir, linkRel, verifiedDirs); err != nil {
		return err
	}
	target := filepath.Join(dstDir, linkRel)
	if err := ensureDir(filepath.Dir(targetPath), verifiedDirs); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	if err := os.Link(target, targetPath); err != nil {
		return fmt.Errorf("failed to create hardlink %s -> %s: %w", targetPath, target, err)
	}
	return nil
}

// ProbeTarGz reports whether the file at path carries the outer shape of a
// collection artifact: gzip on the outside, with a tar stream beginning
// inside it. It stops at the first tar header tar.Reader.Next hands back, and
// how far it reads to reach that point is the archive's own choice, up to
// helpers.ArchiveProbeMaxBytes. One Next call is not one header block: Next
// consumes an 'x', 'L' or 'K' meta header and continues its own loop without
// returning anything, so the chain ahead of the first ordinary header is as
// long as the archive cares to make it, and past that bound the walk stops
// with helpers.ErrArtifactTarHeaderNotFound. Nothing is unpacked and nothing
// is written to disk.
//
// An archive with no entries is accepted: Next reporting io.EOF describes a
// well-formed empty tar, not a malformed one, and refusing it would make this
// probe stricter than the extractor it stands in front of.
//
// What it deliberately does not check: that the archive is complete, what the
// archive contains, or that its bytes match any sha256. Those questions belong
// to the extractor and to the sha verification that already run on the paths
// that install an artifact. This is a shape check on the way into a shared
// cache slot, so a later consumer of that slot does not open it only to find
// an error page inside. It reads through internal/gzipstream exactly as the
// extractor does, differently sized, so "gzip" here is the extractor's own
// notion rather than a second, subtly different one - which is not the same as
// accepting exactly what the extractor accepts, and never was: the extractor
// reads the whole stream and refuses on entry-count, size and path rules this
// walk never reaches.
//
// ctx bounds the probe, on the compressed side, which is the only side there
// is to bound here: this walk stops at the first tar header, so a canceled or
// expired probe is one stopped at the next read taken off the file. A refusal
// carrying the caller's own cancellation is reported as that cancellation
// rather than as a verdict on the artifact's shape - see notTarGzError.
//
// The bound above is on the bytes this walk pulls OUT of the decompressor, so
// what it bounds is the meta-header amplification and not this probe in
// general. A gzip stream whose members produce no output reaches that counter
// with nothing to count, so what refuses it is the seam rather than the bound:
// internal/gzipstream ends a member that produced no bytes with
// helpers.ErrEmptyGzipMember, reported here as ErrArtifactNotTarGz naming the
// path. That is what keeps an artifact of any size whose gzip members carry
// nothing out of a shared cache slot - measured on go1.26.6, darwin/arm64
// (Apple M3 Pro), an 80 MB member of zero-length stored blocks presents a
// perfectly well-formed empty tar to any reader counting only what comes out
// of it.
//
// The same seam answers the member-loop recursion this reader would otherwise
// inherit: pgzip starts the next member by recursing inside its own Read, so
// on the same machine 3,200,000 empty members - 64,000,000 bytes on the wire,
// twenty apiece - kill a process reading them that way with an unrecoverable
// "fatal error: stack overflow" against a 512 MiB goroutine stack, while
// 3,000,000 survive and take 21.8 s. Sizing this reader differently does not
// narrow that, because what recurses is the member loop rather than the
// buffer, which is why the answer is a shared seam rather than a constant
// here.
//
// Truncation is not caught, at any sizing. pgzip turns a truncated read that
// still produced bytes into a short block carrying no error at all (gunzip.go
// in doReadAhead, io.ErrUnexpectedEOF with n > 0), so a copy cut short is
// refused only when what survives cannot produce the first 512-byte tar
// header: measured over a 262,446-byte archive, a copy of its first 20 bytes
// is refused with "unexpected EOF" - reported by tar.Reader.Next, the gzip
// header parse itself having succeeded - while copies of its first 200, 1,024,
// 65,536, 131,072 and 262,445 bytes all return nil.
//
// A defect further into the stream is noticed only within the first block this
// reader buffers, which is a narrower window than the extractor's: pgzip
// discards the data of a block whose fill errored and delivers the error in
// its place, and this walk ends inside the block it first pulls (see
// probeGzipBlocks for the one shape that reads past it). Measured against a
// stream whose stored-block header carries an NLEN that is not ^LEN, a hard
// "flate: corrupt input": a defect at 32 KiB is refused at both sizings, one
// at 64 KiB, 200 KiB or 512 KiB is refused at the extractor's 1 MiB blocks and
// accepted here, and one at 1 MiB is accepted at both. Where that boundary
// falls is pgzip's block arithmetic rather than a property this probe offers,
// which is why no test pins it.
func ProbeTarGz(ctx context.Context, path string) error {
	//nolint:gosec // path is an artifact temp file this process just wrote.
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open artifact for a shape probe: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	gz, err := gzipstream.NewReaderN(ctx, file, probeGzipBlockSize, probeGzipBlocks)
	if err != nil {
		return notTarGzError(path, err)
	}
	defer func() {
		_ = gz.Close()
	}()

	limited := &decompressedLimitReader{r: gz, over: helpers.ErrArtifactTarHeaderNotFound, max: helpers.ArchiveProbeMaxBytes}
	if _, err := tar.NewReader(limited).Next(); err != nil && !errors.Is(err, io.EOF) {
		// A crossing of the scan bound is its own headline rather than a
		// gzip-or-tar verdict: the stream is both, and saying otherwise would
		// send an operator looking for an error page inside an artifact that
		// does hold a tar stream (see helpers.ErrArtifactTarHeaderNotFound).
		if errors.Is(err, helpers.ErrArtifactTarHeaderNotFound) {
			return fmt.Errorf("%w: %s", helpers.ErrArtifactTarHeaderNotFound, path)
		}
		return notTarGzError(path, err)
	}
	return nil
}

// notTarGzError names a failure to read an artifact's outer shape as the
// verdict it is - except when what failed it is the caller's own
// cancellation, which is returned unchanged.
//
// The exception exists because the context now reaches the decompressor: the
// reader gzipstream builds observes ctx on the compressed side, so a canceled
// or expired probe fails with ctx.Err() rather than with anything about gzip,
// at its constructor and part-way through its walk alike - which is why both
// of ProbeTarGz's arms go through this function rather than only the one that
// opens the reader. The exit class is right either way, since
// cmd/go-galaxy/exitcode checks cancellation ahead of every other class, but
// the message would tell an operator who pressed Ctrl-C that the artifact is
// not a tar.gz. Only the two context values are excepted, so every genuine
// refusal - a truncated header, an error page, a member that produces no
// bytes - still carries the sentinel and the path it was read from.
func notTarGzError(path string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", helpers.ErrArtifactNotTarGz, path, err)
}

// FileHashSHA256 calculates the SHA256 hash of a file on disk.
func FileHashSHA256(path string) (string, error) {
	//nolint:gosec // path is caller-provided and expected for hashing.
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
	}()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sanitizeArchivePath validates and normalizes a tar entry path.
func sanitizeArchivePath(name string) (string, error) {
	if name == "" {
		return "", helpers.ErrArchiveEntryHasEmptyName
	}
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == "." {
		return "", nil
	}
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("%w: %s", helpers.ErrArchiveEntryIsAbsolutePath, name)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s", helpers.ErrArchiveEntryEscapesDestination, name)
	}
	return cleaned, nil
}

// ensureNoSymlinkParents rejects paths that traverse symlink parents.
//
// verifiedDirs memoizes parent-chain components already confirmed, earlier
// in this extraction, to be real (non-symlink) directories, so a deeply
// nested collection does not re-Lstat its whole ancestor chain for every
// entry.
//
// Correctness theorem: without the memo, this function calls os.Lstat(C)
// for every parent component C and rejects only if C is an existing
// symlink (nonexistent, a real directory, or a regular file all pass). The
// memo skips os.Lstat(C) ONLY when C is known to be a real, non-symlink
// directory - either because it was Lstat'd earlier in this extraction and
// found to be one, or because ensureDir created it as one. By the
// no-overwrite invariant below, C is therefore still a real directory at
// the time it is skipped, so a fresh Lstat(C) would still pass. Skipping
// yields the identical decision. Components that are symlinks, nonexistent,
// or non-directories are never memoized, so they are always Lstat'd exactly
// as before. Therefore the memo produces the identical accept/reject
// outcome for every entry, for every component.
//
// The two writers do not weaken each other. This function memoizes only
// after an Lstat proved the component a directory and not a symlink;
// ensureDir memoizes only after os.MkdirAll returned, which for an already
// present path means it found a directory there - and that path is always
// one this function already Lstat'd for the entry being extracted, so
// os.MkdirAll's own symlink-following Stat cannot have accepted a symlink
// that this function would have rejected.
//
// Load-bearing invariant: a directory confirmed non-symlink during an
// extraction stays a non-symlink directory for the rest of that extraction,
// because the extractor never overwrites an existing path - os.Symlink and
// os.Link return EEXIST, os.OpenFile on a directory target and os.MkdirAll
// over a non-directory both fail, and all of these abort the extraction.
// So a memoized component can never be turned into a symlink after it is
// memoized. THIS MEMO'S SAFETY DEPENDS ON THAT no-overwrite/abort-on-EEXIST
// behavior; a future change that made extraction remove-then-recreate or
// overwrite existing entries would invalidate the memo and must re-audit
// this function.
//
// The leaf component of relPath - the entry's own path - is never memoized
// here, since it is typically nonexistent at this point (see the
// non-directory guard below), so it is always freshly Lstat'd, preserving
// the symlink-overwrite defense on the entry's own path.
func ensureNoSymlinkParents(baseDir, relPath string, verifiedDirs map[string]struct{}) error {
	// Defensive: sanitizeArchivePath normalizes empty and "." path elements
	// away before any caller reaches this function, so this early return and
	// the per-component empty/"." skip below are unreachable via the current
	// callers; both are kept as defense-in-depth for any future caller that
	// passes an unsanitized relPath.
	if relPath == "" || relPath == "." {
		return nil
	}
	current := baseDir
	for part := range strings.SplitSeq(relPath, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := checkPathComponentNotSymlink(current, verifiedDirs); err != nil {
			return err
		}
	}
	return nil
}

// checkPathComponentNotSymlink is the per-component check inside
// ensureNoSymlinkParents' loop, split into its own function to keep both
// functions under the cyclomatic-complexity budget; it carries no
// independent logic beyond what is documented on ensureNoSymlinkParents.
// It memoizes current in verifiedDirs (and skips the Lstat if already
// memoized) using exactly the memo rules from that comment: only a
// confirmed, non-symlink directory is memoized.
func checkPathComponentNotSymlink(current string, verifiedDirs map[string]struct{}) error {
	if _, ok := verifiedDirs[current]; ok {
		return nil
	}
	info, err := os.Lstat(current)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to stat path %s: %w", current, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", helpers.ErrArchivePathContainsSymlinkComponent, current)
	}
	if info.IsDir() {
		verifiedDirs[current] = struct{}{}
	}
	return nil
}

// safeSymlinkTarget validates a symlink target within the archive.
func safeSymlinkTarget(relPath, linkName string) (string, error) {
	if linkName == "" {
		return "", fmt.Errorf("%w for %s", helpers.ErrSymlinkTargetIsEmpty, relPath)
	}
	if filepath.IsAbs(linkName) || filepath.VolumeName(linkName) != "" {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetIsAbsolute, linkName)
	}
	cleaned := filepath.Clean(filepath.FromSlash(linkName))
	if cleaned == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTarget, linkName)
	}
	baseDir := filepath.Dir(relPath)
	resolved := filepath.Clean(filepath.Join(baseDir, cleaned))
	if resolved == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetResolvesToRoot, linkName)
	}
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetEscapesDestination, linkName)
	}
	relTarget, err := filepath.Rel(baseDir, resolved)
	if err != nil {
		return "", err
	}
	if relTarget == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetResolvesToSelf, linkName)
	}
	return relTarget, nil
}
