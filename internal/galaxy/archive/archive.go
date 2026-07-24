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
	uncompressedStream, err := pgzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		_ = uncompressedStream.Close()
	}()

	tarReader := tar.NewReader(uncompressedStream)
	return extractTarEntries(tarReader, dstDir, helpers.ArchiveMaxEntryCount)
}

func extractTarEntries(tarReader *tar.Reader, dstDir string, maxEntries int64) error {
	var extracted, entries int64
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
		// entries counts every header regardless of typeflag, so a tarbomb of
		// many zero-byte directories or hardlinks - which never trips the
		// byte caps in extractRegularFile - is still rejected. The check
		// runs before handleTarEntry, so the (maxEntries+1)th entry is
		// rejected before it is ever extracted: at most maxEntries inodes get
		// created before the caller discards the whole destination.
		entries++
		if entries > maxEntries {
			return fmt.Errorf("%w: %d", helpers.ErrArchiveTooManyEntries, maxEntries)
		}
		if err := handleTarEntry(tarReader, header, dstDir, &extracted, verifiedDirs); err != nil {
			return err
		}
	}
}

func handleTarEntry(tarReader *tar.Reader, header *tar.Header, dstDir string, extracted *int64, verifiedDirs map[string]struct{}) error {
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
		return extractDir(targetPath)
	case tar.TypeReg:
		return extractRegularFile(tarReader, header, targetPath, extracted)
	case tar.TypeSymlink:
		return extractSymlink(relPath, targetPath, header)
	case tar.TypeLink:
		return extractHardlink(dstDir, targetPath, header, verifiedDirs)
	default:
		return nil
	}
}

func extractDir(targetPath string) error {
	if err := os.MkdirAll(targetPath, helpers.DirMod); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", targetPath, err)
	}
	return nil
}

func extractRegularFile(tarReader *tar.Reader, header *tar.Header, targetPath string, extracted *int64) error {
	if header.Size < 0 {
		return fmt.Errorf("%w: %s ", helpers.ErrArchiveEntryHasNegativeSize, header.Name)
	}
	if header.Size > helpers.ArchiveMaxEntrySize {
		return fmt.Errorf("%w %s: %d bytes", helpers.ErrArchiveEntryIsTooLarge, header.Name, header.Size)
	}
	if *extracted+header.Size > helpers.ArchiveMaxTotalSize {
		return fmt.Errorf("%w: %d bytes", helpers.ErrArchiveExceedsMaxSize, helpers.ArchiveMaxTotalSize)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), helpers.DirMod); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	mode := header.FileInfo().Mode().Perm()
	//nolint:gosec // targetPath is sanitized archive entry under dstDir.
	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", targetPath, err)
	}
	written, err := io.CopyN(file, tarReader, header.Size)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to write file %s: %w", targetPath, err)
	}
	*extracted += written
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close file %s: %w", targetPath, err)
	}
	return nil
}

func extractSymlink(relPath, targetPath string, header *tar.Header) error {
	linkTarget, err := safeSymlinkTarget(relPath, header.Linkname)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), helpers.DirMod); err != nil {
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
	if err := os.MkdirAll(filepath.Dir(targetPath), helpers.DirMod); err != nil {
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
// memo skips os.Lstat(C) ONLY when C was already Lstat'd earlier in this
// extraction and found to be a real, non-symlink directory. By the
// no-overwrite invariant below, C is therefore still a real directory at
// the time it is skipped, so a fresh Lstat(C) would still pass. Skipping
// yields the identical decision. Components that are symlinks, nonexistent,
// or non-directories are never memoized, so they are always Lstat'd exactly
// as before. Therefore the memo produces the identical accept/reject
// outcome for every entry, for every component.
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
