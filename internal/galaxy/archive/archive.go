package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/klauspost/pgzip"
)

// ExtractTarGz extracts a tar.gz archive into dstDir with safety checks.
func ExtractTarGz(tarGzFile, dstDir string) error {
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

	return ExtractTarGzStream(file, dstDir)
}

// ExtractTarGzStream extracts a tar.gz stream into dstDir with safety checks.
// It does not close r.
func ExtractTarGzStream(r io.Reader, dstDir string) error {
	return extractTarGzStream(r, dstDir, helpers.ArchiveMaxDecompressedSize)
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
func extractTarGzStream(r io.Reader, dstDir string, maxDecompressed int64) error {
	uncompressedStream, err := pgzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		_ = uncompressedStream.Close()
	}()

	limited := &decompressedLimitReader{r: uncompressedStream, max: maxDecompressed}
	tarReader := tar.NewReader(limited)
	return extractTarEntries(tarReader, dstDir, helpers.ArchiveMaxEntryCount)
}

// decompressedLimitReader counts the bytes read out of an archive's
// decompressor and fails once they exceed max, so an archive is bounded by
// what it actually costs to read rather than by what its headers declare.
//
// helpers.NewSizeLimitedReader is deliberately not reused here, and the
// mechanics are not identical either: one of the differences is the point
// this reader's refusal turns on, since it returns zero bytes alongside that
// refusal (see Read below) while sizeLimitedReader hands back the bytes it
// read. The reason not to reuse it stands on its own regardless - it reports
// helpers.ErrResponseTooLarge, which exitcode.isTransportError classifies
// ExitNetwork, so a decompression bomb would be reported to CI as a network
// fault and retried forever.
type decompressedLimitReader struct {
	r   io.Reader
	err error
	max int64
	n   int64
}

// Read passes bytes through and fails with
// helpers.ErrArchiveDecompressedTooLarge once the cumulative count exceeds
// max, replacing whatever the decompressor itself returned (its own io.EOF
// included), since a stream past the ceiling must never be reported as a
// clean read.
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
		r.err = fmt.Errorf("%w: read %d bytes, limit is %d bytes", helpers.ErrArchiveDecompressedTooLarge, r.n, r.max)
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
		// rejected. Three bounds split the work, and none subsumes another: the
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
// this. Regular files now extract read-only (see extractRegularFile above),
// so a tarball with two entries competing for one path fails this OpenFile
// with a bare permission error against whatever the earlier entry already
// put there, which is undebuggable on its own: previously, with writable
// extracted files, the second entry's O_TRUNC open silently succeeded and
// the last entry won. An os.Stat of the target distinguishes "this path is
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
