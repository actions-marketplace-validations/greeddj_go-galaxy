// Package extracted provides a content-addressable store for unpacked
// collection tarballs. Each artifact SHA256 is extracted at most once,
// and per-project install paths are populated via hardlinks (with a
// copy fallback for cross-device cases).
package extracted

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// RootDirName is the directory name under cacheDir holding extracted entries.
	RootDirName = "extracted"
	// ReadyMarker is the file written last to mark a complete extraction.
	ReadyMarker = ".ready"
	tmpSuffix   = ".tmp"
	// ingestPrefix names the temp directories IngestReader creates under the
	// store root before a stream is finalized via Promote.
	ingestPrefix = "ingest-"

	// ReadyMarkerPayload is the exact content a current binary writes into
	// ReadyMarker, and the only content isReady accepts. This is a version
	// tag, not decoration: Ensure's isReady check runs before the per-sha
	// lock is taken, so a CAS tree extracted by an older binary - one that
	// wrote the legacy "ok" sentinel and left its regular files writable -
	// would otherwise be trusted verbatim and hard-linked into every install
	// that references it, silently defeating the write-bit hardening this
	// commit adds. Bumping this payload is what forces every pre-existing
	// tree to fail isReady exactly once and rebuild hardened; a tree that
	// already carries this payload was, by construction, extracted by a
	// binary new enough to have applied the mask.
	ReadyMarkerPayload = "ro1"
	// readyMarkerMaxReadSize bounds the read of a ready marker file. A
	// well-formed marker is a few bytes, so anything longer is either
	// corrupt or hostile and is rejected outright without the caller ever
	// needing to learn the file's real length - mirroring
	// collections.extractMarkerMaxReadSize's same reasoning for the
	// extract-done marker.
	readyMarkerMaxReadSize = 64
)

var (
	// ErrStoreNotConfigured indicates the store has no cache directory.
	ErrStoreNotConfigured = errors.New("extracted store is not configured")
	// ErrSHAEmpty indicates a missing SHA256 identifier.
	ErrSHAEmpty = errors.New("artifact sha is empty")
)

// Store materializes tarballs once per SHA and reuses them via hardlinks.
type Store struct {
	locks map[string]*sync.Mutex
	root  string
	mu    sync.Mutex
}

// NewStore returns a Store rooted at cacheDir/extracted, or nil if cacheDir
// is empty (in which case callers should fall back to direct extraction).
func NewStore(cacheDir string) *Store {
	if cacheDir == "" {
		return nil
	}
	return &Store{
		root:  filepath.Join(cacheDir, RootDirName),
		locks: make(map[string]*sync.Mutex),
	}
}

// Root returns the on-disk root of the store.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Ensure extracts tarPath into the store under sha if not already present
// and returns the path of the extracted tree. Concurrent callers for the
// same sha share the work.
func (s *Store) Ensure(sha, tarPath string) (string, error) {
	if s == nil {
		return "", ErrStoreNotConfigured
	}
	if sha == "" {
		return "", ErrSHAEmpty
	}
	final := filepath.Join(s.root, sha)
	if isReady(final) {
		return final, nil
	}

	lock := s.lockFor(sha)
	lock.Lock()
	defer lock.Unlock()

	if isReady(final) {
		return final, nil
	}
	// The CAS tree for sha is absent, so we are about to ingest tarPath's bytes
	// under sha as their content-addressable key. Verify the bytes actually
	// hash to sha first: a rotted or tampered tarball whose sidecar-derived sha
	// no longer matches its bytes must never be extracted into the shared store
	// keyed by a sha its content does not produce, which would hand every other
	// project that later references that sha content which does not hash to it.
	//
	// This costs one full read of tarPath ahead of extraction, but only on the
	// CAS-absent ingest path taken at most once per sha (the lock above and the
	// isReady checks bracketing it ensure that): the hot path where the CAS
	// tree already exists short-circuits above before ever taking the lock, and
	// a fresh download never reaches Ensure at all - it populates the CAS via
	// Promote instead, whose sha is derived from a hash of the bytes just
	// streamed, not from an unverified sidecar.
	if err := verifyTarballSHA(tarPath, sha); err != nil {
		return "", err
	}
	return s.extractInto(final, tarPath)
}

// verifyTarballSHA reports nil when the file at tarPath hashes to sha,
// returning a helpers.ErrSHA256Mismatch-wrapped error otherwise so callers can
// classify a corrupt cached tarball uniformly with the rest of the install
// path's integrity checks.
func verifyTarballSHA(tarPath, sha string) error {
	actual, err := archive.FileHashSHA256(tarPath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, sha) {
		return fmt.Errorf("%w: %s: %s != %s", helpers.ErrSHA256Mismatch, tarPath, actual, sha)
	}
	return nil
}

// IngestReader extracts a tar.gz stream into a fresh tmp directory under
// the store root. The caller must call Promote() with the resulting tmp
// path and a SHA to finalize, or remove the tmp tree on error. The reader
// is always drained to EOF so that an upstream io.Pipe writer cannot
// deadlock when the gzip stream ends before the body does.
func (s *Store) IngestReader(r io.Reader) (string, error) {
	if s == nil {
		_, _ = io.Copy(io.Discard, r)
		return "", ErrStoreNotConfigured
	}
	if err := os.MkdirAll(s.root, helpers.DirMod); err != nil {
		_, _ = io.Copy(io.Discard, r)
		return "", err
	}
	tmpDir, err := os.MkdirTemp(s.root, ingestPrefix)
	if err != nil {
		_, _ = io.Copy(io.Discard, r)
		return "", err
	}
	extractErr := archive.ExtractTarGzStream(r, tmpDir)
	_, _ = io.Copy(io.Discard, r)
	if extractErr != nil {
		_ = os.RemoveAll(tmpDir)
		return "", extractErr
	}
	return tmpDir, nil
}

// Promote atomically renames a tmpRoot from IngestReader into <root>/<sha>.
// If <sha> is already finalized, tmpRoot is removed and the existing path
// returned.
func (s *Store) Promote(tmpRoot, sha string) (string, error) {
	if s == nil {
		_ = os.RemoveAll(tmpRoot)
		return "", ErrStoreNotConfigured
	}
	if sha == "" {
		_ = os.RemoveAll(tmpRoot)
		return "", ErrSHAEmpty
	}
	final := filepath.Join(s.root, sha)

	lock := s.lockFor(sha)
	lock.Lock()
	defer lock.Unlock()

	if isReady(final) {
		_ = os.RemoveAll(tmpRoot)
		return final, nil
	}
	if err := writeReadyMarker(tmpRoot); err != nil {
		_ = os.RemoveAll(tmpRoot)
		return "", err
	}
	_ = os.RemoveAll(final)
	if err := os.Rename(tmpRoot, final); err != nil {
		_ = os.RemoveAll(tmpRoot)
		return "", err
	}
	return final, nil
}

// Remove deletes the extracted entry for sha. Best-effort.
func (s *Store) Remove(sha string) error {
	if s == nil || sha == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(s.root, sha))
}

// SweepPlan lists the extracted entries under the store root whose name is
// not present in keep, sorted for deterministic output. It performs no
// filesystem mutation, so callers can use it to report what Sweep(keep)
// would remove without actually removing anything (e.g. a dry-run). A
// missing root directory is not an error: it yields (nil, nil), matching
// Sweep's own behavior when there is nothing to sweep yet.
func (s *Store) SweepPlan(keep map[string]bool) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	planned := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] {
			continue
		}
		planned = append(planned, name)
	}
	sort.Strings(planned)
	return planned, nil
}

// Sweep removes extracted entries whose SHA is not in keep.
func (s *Store) Sweep(keep map[string]bool) error {
	if s == nil {
		return nil
	}
	planned, err := s.SweepPlan(keep)
	if err != nil {
		return err
	}
	for _, name := range planned {
		_ = os.RemoveAll(filepath.Join(s.root, name))
	}
	return nil
}

// SweepTemp removes leftover temporary entries under the store root left by a
// previously killed run: the "ingest-" directories created by IngestReader
// and the "<sha>.tmp" directories created by Ensure/extractInto, both renamed
// to their final CAS location on success and cleaned on failure. A finalized
// CAS tree - a bare sha directory with a .ready marker, carrying neither the
// "ingest-" prefix nor the ".tmp" suffix - is never matched. A missing root
// is not an error. The caller must hold the install lock so every match is a
// dead-run orphan.
func (s *Store) SweepTemp() error {
	if s == nil {
		return nil
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !isTempEntryName(name) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, name)); err != nil {
			return err
		}
	}
	return nil
}

// isTempEntryName reports whether name is one of the two temp forms the store
// creates under its root, as opposed to a finalized CAS tree named by a bare
// sha.
func isTempEntryName(name string) bool {
	return strings.HasPrefix(name, ingestPrefix) || strings.HasSuffix(name, tmpSuffix)
}

func (s *Store) extractInto(final, tarPath string) (string, error) {
	if err := os.MkdirAll(s.root, helpers.DirMod); err != nil {
		return "", err
	}
	tmp := final + tmpSuffix
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, helpers.DirMod); err != nil {
		return "", err
	}
	if err := archive.ExtractTarGz(tarPath, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := writeReadyMarker(tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	_ = os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	return final, nil
}

func (s *Store) lockFor(sha string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.locks[sha]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[sha] = lock
	}
	return lock
}

// writeReadyMarker writes ReadyMarkerPayload into dir's ready marker,
// removing any existing file at that path first (ignoring fs.ErrNotExist).
// The remove-then-write is not decorative: a tarball can ship its own
// root-level ".ready" entry, which archive.extractRegularFile now extracts
// read-only along with every other regular file, so a plain os.WriteFile
// here would fail EACCES trying to truncate it in place. Removing first
// always starts from a clean, writable slate regardless of what extraction
// left behind at that path.
func writeReadyMarker(dir string) error {
	path := filepath.Join(dir, ReadyMarker)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, []byte(ReadyMarkerPayload), helpers.FileMod)
}

// readReadyMarker reads path with a hard cap of readyMarkerMaxReadSize+1
// bytes, reporting ok=false for a missing/unreadable file and for a file at
// least one byte over the cap - mirroring
// collections.readExtractMarker's same bounded-read shape.
func readReadyMarker(path string) (string, bool) {
	f, err := os.Open(path) //nolint:gosec // path is this store's own marker path, not attacker-controlled input.
	if err != nil {
		return "", false
	}
	defer func() {
		_ = f.Close()
	}()

	buf := make([]byte, readyMarkerMaxReadSize+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return string(buf[:n]), true
	default:
		return "", false
	}
}

// isReady reports whether dir holds a finalized CAS tree written by a
// binary new enough to apply the write-bit hardening. It checks the marker's
// exact content, not merely its presence: a tree extracted by an older
// binary carries the legacy "ok" sentinel (or, on a much older binary,
// nothing at all in this format) and its regular files are still writable,
// so isReady must reject it here rather than let Ensure/Materialize trust it
// verbatim and hand out unhardened hard links. Rejecting forces
// extractInto/Promote's own RemoveAll-then-rebuild path, which re-extracts
// the tree through the current, hardened archive package.
func isReady(dir string) bool {
	content, ok := readReadyMarker(filepath.Join(dir, ReadyMarker))
	return ok && content == ReadyMarkerPayload
}

// Materialize mirrors srcRoot into dstRoot using hardlinks, falling back
// to copy for files that cannot be linked (e.g. cross-device). The
// ReadyMarker at the root is skipped.
func Materialize(srcRoot, dstRoot string) error {
	if err := os.MkdirAll(dstRoot, helpers.DirMod); err != nil {
		return err
	}
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ReadyMarker {
			return nil
		}
		dst := filepath.Join(dstRoot, rel)
		return materializeEntry(path, dst, d)
	})
}

func materializeEntry(src, dst string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	mode := info.Mode()
	switch {
	case d.IsDir():
		return os.MkdirAll(dst, mode.Perm())
	case mode&fs.ModeSymlink != 0:
		return materializeSymlink(src, dst)
	case mode.IsRegular():
		return materializeFile(src, dst, mode.Perm())
	default:
		return nil
	}
}

func materializeSymlink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), helpers.DirMod); err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Symlink(target, dst)
}

// materializeFile links src into dst, falling back to a byte copy when the
// link fails (e.g. src and dst are on different devices). The remove before
// linking is checked rather than best-effort: extractCollection always wipes
// installPath with os.RemoveAll before a real install, so in the ordinary
// path dst is simply absent here and os.Remove returns fs.ErrNotExist, which
// is ignored; any other remove failure would otherwise fall through to an
// EEXIST link error and then an EACCES/EISDIR open inside copyFile, hiding
// the real cause behind a confusing downstream failure.
func materializeFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), helpers.DirMod); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst, perm)
}

// copyFile is materializeFile's cross-device fallback: a plain byte copy
// into a freshly created dst at the CAS entry's own mode. The create mode
// passed to OpenFile is masked by the process umask (unlike a hard link,
// which carries the source inode's mode verbatim and is never subject to
// umask), so under a restrictive umask the created file could otherwise end
// up with fewer permission bits than a linked install of the same CAS entry
// - breaking the "installed mode == CAS mode" invariant Materialize
// otherwise guarantees. The explicit Chmod after the copy - an fchmod on an
// already-open, still-writable descriptor - re-asserts perm exactly,
// independent of umask, and succeeds even though perm has no write bit: an
// open O_WRONLY descriptor keeps writing after its own mode is dropped to
// read-only, since fchmod affects future opens, not the fd already held.
func copyFile(src, dst string, perm os.FileMode) error {
	//nolint:gosec // src comes from the trusted extracted store.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	//nolint:gosec // dst is derived from a sanitized archive path under dstRoot.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(perm); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
