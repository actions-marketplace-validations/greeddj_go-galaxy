// Package extracted provides a content-addressable store for unpacked
// collection tarballs. Each artifact SHA256 is extracted at most once,
// and per-project install paths are populated via hardlinks (with a
// copy fallback for cross-device cases).
package extracted

import (
	"errors"
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
	return s.extractInto(final, tarPath)
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
	if err := os.WriteFile(filepath.Join(tmpRoot, ReadyMarker), []byte("ok"), helpers.FileMod); err != nil {
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
	if err := os.WriteFile(filepath.Join(tmp, ReadyMarker), []byte("ok"), helpers.FileMod); err != nil {
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

func isReady(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ReadyMarker))
	return err == nil
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

func materializeFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), helpers.DirMod); err != nil {
		return err
	}
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst, perm)
}

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
	return out.Close()
}
