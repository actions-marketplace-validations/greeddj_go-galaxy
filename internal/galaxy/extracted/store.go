// Package extracted provides a content-addressable store for unpacked
// collection tarballs. Each artifact SHA256 is extracted at most once,
// and per-project install paths are populated via hardlinks (with a
// copy fallback for cross-device cases).
//
// Every path this package creates, renames, or removes is resolved through an
// os.Root established at the configured cache directory, so no component
// beneath it - the "extracted" directory itself above all - can redirect a
// write or a recursive delete outside the tree the operator configured. The
// root is established at the cache directory and never one level lower, for
// the same reason the install side roots at the collections path rather than
// at ansible_collections: os.OpenRoot follows a symlink when establishing the
// root, so rooting at "extracted" would adopt whatever that name points at and
// leave nothing to refuse. A symlinked cache directory itself still works, and
// that is deliberate - it is the boundary, not something inside it.
package extracted

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
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
	// ingestNameAttempts bounds how many random names mkdirTemp tries before
	// giving up, matching os.MkdirTemp's own retry discipline for a name
	// collision.
	ingestNameAttempts = 10000

	// ReadyMarkerPayload is the exact content a current binary writes into
	// ReadyMarker, and the only content isReady accepts. This is a version
	// tag, not decoration: Ensure's isReady check runs before the per-sha
	// lock is taken, so a CAS tree extracted by an older binary - one that
	// wrote the legacy "ok" sentinel and left its regular files writable -
	// would otherwise be trusted verbatim and hard-linked into every install
	// that references it, silently defeating the write-bit hardening. Bumping
	// this payload is what forces every pre-existing tree to fail isReady
	// exactly once and rebuild hardened; a tree that already carries this
	// payload was, by construction, extracted by a binary new enough to have
	// applied the mask.
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
	// ErrSHAUnsafe indicates a SHA256 identifier that is not safe to use as a
	// single path element - see entryRel's own doc comment for where such a
	// value can originate.
	ErrSHAUnsafe = errors.New("artifact sha is not a single path element")
	// ErrTempOutsideStore indicates a temp path handed to Promote or Discard
	// that does not sit under the store's own cache directory. Both take a
	// path a caller received from IngestReader, and both would otherwise
	// RemoveAll it; refusing is what keeps that primitive from being aimed at
	// an arbitrary path.
	ErrTempOutsideStore = errors.New("temp path is outside the extracted store")
	// ErrStoreDirUnusable indicates the store directory under the cache
	// directory exists but cannot serve as one: a symlink leading out of the
	// cache directory, or a non-directory occupying the name. It exists
	// because the containment root reports the first of those as a bare
	// "file exists" from mkdirat and the second identically, which names
	// neither the path nor what is wrong with it.
	ErrStoreDirUnusable = errors.New("extracted store directory is not usable")
	// errIngestTempExhausted indicates mkdirTemp could not find an unused
	// name. It is unexported because no caller can act on it differently from
	// any other ingest failure.
	errIngestTempExhausted = errors.New("could not create an ingest temp directory")
)

// Store materializes tarballs once per SHA and reuses them via hardlinks.
type Store struct {
	locks    map[string]*sync.Mutex
	cacheDir string
	mu       sync.Mutex
}

// NewStore returns a Store rooted at cacheDir/extracted, or nil if cacheDir
// is empty (in which case callers should fall back to direct extraction).
func NewStore(cacheDir string) *Store {
	if cacheDir == "" {
		return nil
	}
	return &Store{
		cacheDir: cacheDir,
		locks:    make(map[string]*sync.Mutex),
	}
}

// Root returns the on-disk root of the store. It is a display and test
// affordance only: nothing in this package resolves a path by joining onto
// it, since every real operation goes through the containment root instead.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.cacheDir, RootDirName)
}

// Ensure extracts tarPath into the store under sha if not already present
// and returns the path of the extracted tree. Concurrent callers for the
// same sha share the work.
//
// The returned path is an ordinary string, deliberately: its consumer
// (Materialize) walks the tree with fs.WalkDir, which does not follow
// symlinks, and the tree it names was resolved through the containment root
// moments earlier. A local writer racing between this return and that walk is
// the same disclosed residual the install side carries for its own extraction
// target, not a gap this root closes.
func (s *Store) Ensure(ctx context.Context, sha, tarPath string) (string, error) {
	if s == nil {
		return "", ErrStoreNotConfigured
	}
	if sha == "" {
		return "", ErrSHAEmpty
	}
	rel, ok := entryRel(sha)
	if !ok {
		return "", ErrSHAUnsafe
	}
	final := s.abs(rel)
	if s.readyRel(rel) {
		return final, nil
	}

	lock := s.lockFor(sha)
	lock.Lock()
	defer lock.Unlock()

	if s.readyRel(rel) {
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
	return s.extractInto(ctx, rel, tarPath)
}

// Ready reports whether sha's content-addressable tree is present and
// finalized, without extracting, promoting, or mutating anything. It is the
// pure read half of Ensure's own isReady short-circuit, exported for a
// preview that must describe the store's state without changing it.
//
// It checks the ready marker's exact payload, not merely its presence: a tree
// extracted by an older binary carries the legacy sentinel and unhardened
// write bits, and Ensure would rebuild it, so Ready must report false for it
// too. A caller that stat'ed the marker itself would silently miss that and
// drift from Ensure.
//
// sha reaches this method from a lockfile pin or the persisted snapshot's
// warmed record, neither of which is validated before it gets here (see
// entryRel), so an unsafe sha - one that is not a single path element -
// reports false rather than joining it into a path at all.
func (s *Store) Ready(sha string) bool {
	if s == nil {
		return false
	}
	rel, ok := entryRel(sha)
	return ok && s.readyRel(rel)
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
// path and a SHA to finalize, or Discard() it on error. The reader
// is always drained to EOF so that an upstream io.Pipe writer cannot
// deadlock when the gzip stream ends before the body does.
func (s *Store) IngestReader(ctx context.Context, r io.Reader) (string, error) {
	if s == nil {
		_, _ = io.Copy(io.Discard, r)
		return "", ErrStoreNotConfigured
	}
	root, err := s.openRootForWrite()
	if err != nil {
		_, _ = io.Copy(io.Discard, r)
		return "", err
	}
	defer func() { _ = root.Close() }()

	tmpRel, err := mkdirTemp(root)
	if err != nil {
		_, _ = io.Copy(io.Discard, r)
		return "", err
	}
	extractErr := archive.ExtractTarGzStream(ctx, r, s.abs(tmpRel))
	_, _ = io.Copy(io.Discard, r)
	if extractErr != nil {
		_ = root.RemoveAll(tmpRel)
		return "", extractErr
	}
	return s.abs(tmpRel), nil
}

// Promote atomically renames a tmpRoot from IngestReader into <root>/<sha>.
// If <sha> is already finalized, tmpRoot is removed and the existing path
// returned.
//
// sha is hex by construction on every caller today (a hash of the bytes just
// streamed), so the entryRel check below is defense in depth against a
// future caller, not a case reachable now - but it still guards a
// RemoveAll-then-Rename pair, and a Rename target built from an unvalidated
// sha is exactly the kind of path this package must never construct.
//
// The sha is validated before tmpRoot is even looked at, so a caller passing
// both an unusable sha and an out-of-store temp still gets the sha's own
// error: the sha is what this method is being asked to do something with,
// while the temp is only what it would clean up on the way out.
func (s *Store) Promote(tmpRoot, sha string) (string, error) {
	if s == nil {
		return "", ErrStoreNotConfigured
	}
	if sha == "" {
		_ = s.Discard(tmpRoot)
		return "", ErrSHAEmpty
	}
	rel, ok := entryRel(sha)
	if !ok {
		_ = s.Discard(tmpRoot)
		return "", ErrSHAUnsafe
	}

	root, err := s.openRootForWrite()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	tmpRel, ok := s.rel(tmpRoot)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrTempOutsideStore, tmpRoot)
	}

	lock := s.lockFor(sha)
	lock.Lock()
	defer lock.Unlock()

	if isReady(root, rel) {
		_ = root.RemoveAll(tmpRel)
		return s.abs(rel), nil
	}
	if err := writeReadyMarker(root, tmpRel); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	return s.finalize(root, tmpRel, rel)
}

// Discard removes a temp tree IngestReader created, refusing any path outside
// the store's own cache directory rather than turning RemoveAll loose on it.
// It exists so a caller that has to abandon an ingest does not have to reach
// for os.RemoveAll on a path this package handed it.
func (s *Store) Discard(tmpRoot string) error {
	if s == nil || tmpRoot == "" {
		return nil
	}
	tmpRel, ok := s.rel(tmpRoot)
	if !ok {
		return fmt.Errorf("%w: %s", ErrTempOutsideStore, tmpRoot)
	}
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(tmpRel)
}

// Remove deletes the extracted entry for sha. Best-effort: it has zero
// production callers today, so this exists for a future caller that will
// pass whatever the persisted snapshot or a lockfile pin recorded, neither
// validated (see entryRel) - an unsafe sha makes Remove refuse rather than
// remove, returning nil exactly as it does for an empty sha, so the nil must
// not be read as "the entry is gone"; it means "nothing was touched".
func (s *Store) Remove(sha string) error {
	if s == nil || sha == "" {
		return nil
	}
	rel, ok := entryRel(sha)
	if !ok {
		return nil
	}
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(rel)
}

// SweepPlan lists the extracted entries under the store root whose name is
// not present in keep, sorted for deterministic output. It performs no
// filesystem mutation, so callers can use it to report what Sweep
// would remove without actually removing anything (e.g. a dry-run). A
// missing root directory is not an error: it yields (nil, nil), matching
// Sweep's own behavior when there is nothing to sweep yet.
func (s *Store) SweepPlan(keep map[string]bool) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()

	entries, exists, err := readStoreDir(root)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	planned := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] {
			continue
		}
		planned = append(planned, name)
	}
	slices.Sort(planned)
	return planned, nil
}

// Sweep removes extracted entries whose SHA is not in keep. It is
// best-effort per entry - one undeletable tree must not stop the rest from
// being reclaimed - but it does not discard the outcome entirely: the first
// removal failure is remembered and returned once every other entry has been
// attempted, so a caller has something to report. A refusal by the
// containment root, which is what an escaping "extracted" symlink produces,
// surfaces through exactly that path.
//
// ctx is read before each entry, so a caller that stopped owning the store -
// cleanup's own holder context being canceled after another holder took the
// cache lock - stops removing trees that holder may already be rebuilding.
// The granularity is one entry: a root.RemoveAll already walking a tree is
// not interruptible and runs to completion. Its error preempts a removal
// failure recorded earlier in the same pass, deliberately: a caller that no
// longer owns the store needs to know that, not which of the trees it was
// permitted to reclaim resisted.
func (s *Store) Sweep(ctx context.Context, keep map[string]bool) error {
	if s == nil {
		return nil
	}
	planned, err := s.SweepPlan(keep)
	if err != nil {
		return err
	}
	if len(planned) == 0 {
		return nil
	}

	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()

	var firstErr error
	for _, name := range planned {
		if err := ctx.Err(); err != nil {
			return err
		}
		if removeErr := root.RemoveAll(path.Join(RootDirName, name)); removeErr != nil && firstErr == nil {
			firstErr = removeErr
		}
	}
	return firstErr
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
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()

	entries, _, err := readStoreDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !isTempEntryName(name) {
			continue
		}
		if err := root.RemoveAll(path.Join(RootDirName, name)); err != nil {
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

// entryRel returns sha's content-addressable tree as a slash path relative to
// the containment root, or ok=false when sha is not a single, safe path
// element. Every method that turns a sha into a path goes through this: a sha
// reaches this package from a lockfile pin (lockfile.File.validate checks only
// duplicate names, never hex shape) and from the persisted snapshot's warmed
// and installed records, neither of which is validated, and path.Join would
// happily clean "../../.." into an escape.
//
// The containment root refuses such an escape too, so this predicate is no
// longer the only thing standing between an unvalidated sha and a RemoveAll.
// It is kept ahead of the root anyway, because it is what lets the store
// answer "this sha is unusable" without creating the cache directory, opening
// a descriptor, or reporting an operating-system error for what is really a
// bad identifier.
func entryRel(sha string) (string, bool) {
	if !helpers.IsPathElement(sha) {
		return "", false
	}
	return path.Join(RootDirName, sha), true
}

// abs turns a root-relative slash path into the absolute path callers outside
// this package receive.
func (s *Store) abs(rel string) string {
	return filepath.Join(s.cacheDir, filepath.FromSlash(rel))
}

// rel is abs's inverse, refusing a path that does not sit under the store's
// cache directory. The check is lexical and therefore only a fast, precise
// refusal for a caller that passed the wrong path outright; containment
// itself is enforced by the root, which re-resolves every component.
func (s *Store) rel(abs string) (string, bool) {
	relPath, err := filepath.Rel(s.cacheDir, abs)
	if err != nil {
		return "", false
	}
	if relPath == "." || relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return filepath.ToSlash(relPath), true
}

// openRoot opens a containment root at the store's cache directory without
// creating it, so a read-only caller against a cache that does not exist yet
// gets fs.ErrNotExist to degrade on rather than a directory it did not ask
// for.
func (s *Store) openRoot() (*os.Root, error) {
	return os.OpenRoot(s.cacheDir)
}

// openRootForWrite opens the containment root, creating the cache directory
// and the store directory under it first. The MkdirAll of the cache directory
// itself is deliberately not rooted: that directory is the boundary, and
// creating the boundary cannot be contained by it.
func (s *Store) openRootForWrite() (*os.Root, error) {
	if err := os.MkdirAll(s.cacheDir, helpers.DirMod); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.cacheDir)
	if err != nil {
		return nil, err
	}
	if err := root.MkdirAll(RootDirName, helpers.DirMod); err != nil {
		classified := classifyStoreDirError(root, err)
		_ = root.Close()
		return nil, classified
	}
	return root, nil
}

// classifyStoreDirError names what is actually wrong when an operation on the
// store directory fails, rather than passing on the operating system's own
// answer. The root refuses an escaping symlink at that name with the same
// bare "file exists" a regular file in its place produces, so an operator
// reading either would learn nothing about the cache directory they need to
// fix. Anything the Lstat below does not recognize is returned unchanged: a
// permission problem or a full disk is not this condition and must not be
// renamed into it.
func classifyStoreDirError(root *os.Root, err error) error {
	info, statErr := root.Lstat(RootDirName)
	if statErr != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symlink that does not resolve to a directory inside the cache directory",
			ErrStoreDirUnusable, RootDirName)
	case !info.IsDir():
		return fmt.Errorf("%w: %s is not a directory", ErrStoreDirUnusable, RootDirName)
	default:
		return err
	}
}

// readStoreDir lists the store directory through root, reporting exists=false
// when it is simply not there yet - the cache has nothing in it, which is not
// a failure - and distinguishing that from a directory that is present and
// empty, which callers report differently. A directory that exists and cannot
// be listed is classified rather than passed on raw.
func readStoreDir(root *os.Root) ([]fs.DirEntry, bool, error) {
	entries, err := fs.ReadDir(root.FS(), RootDirName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, classifyStoreDirError(root, err)
	}
	return entries, true, nil
}

// readyRel reports whether the tree at rel carries a current ready marker,
// opening and closing its own containment root. A cache directory that does
// not exist yet reports false rather than an error, which is what every
// caller would do with one anyway.
func (s *Store) readyRel(rel string) bool {
	root, err := s.openRoot()
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	return isReady(root, rel)
}

// extractInto unpacks tarPath into a temp tree beside rel and renames it into
// place. Unlike the store's own path operations, the unpack itself is handed
// an ordinary path: the temp directory was created through the root
// immediately above, so nothing can have been pre-planted inside it, and
// archive's own per-entry symlink-parent check governs what the tarball
// itself may create - the same split extractCollection documents for the
// collections tree.
func (s *Store) extractInto(ctx context.Context, rel, tarPath string) (string, error) {
	root, err := s.openRootForWrite()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	tmpRel := rel + tmpSuffix
	// Checked, not discarded: this and the RemoveAll in finalize are the two
	// destructive primitives here, and a refusal by the root is exactly the
	// signal that something under the cache directory is redirecting them.
	if err := root.RemoveAll(tmpRel); err != nil {
		return "", err
	}
	if err := root.MkdirAll(tmpRel, helpers.DirMod); err != nil {
		return "", err
	}
	if err := archive.ExtractTarGz(ctx, tarPath, s.abs(tmpRel)); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	if err := writeReadyMarker(root, tmpRel); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	return s.finalize(root, tmpRel, rel)
}

// finalize replaces the tree at rel with the finished temp tree at tmpRel,
// cleaning the temp up on either failure so a refused promotion never leaves
// a half-named directory behind for SweepTemp to find later.
func (s *Store) finalize(root *os.Root, tmpRel, rel string) (string, error) {
	if err := root.RemoveAll(rel); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	if err := root.Rename(tmpRel, rel); err != nil {
		_ = root.RemoveAll(tmpRel)
		return "", err
	}
	return s.abs(rel), nil
}

// mkdirTemp creates a uniquely named ingest directory under the store
// directory through root, returning its root-relative path. os.Root has no
// MkdirTemp, so this reproduces the part of os.MkdirTemp that matters:
// os.Root.Mkdir fails with fs.ErrExist rather than reusing an existing name,
// which is what makes retrying on collision safe, and never follows a symlink
// planted at the name it is about to create.
//
// The name comes from crypto/rand rather than a counter or the clock. Not
// because a guessed name would defeat the Mkdir - it would only cost a
// retry - but because it makes the retry loop's bound unreachable in practice
// instead of merely unlikely.
func mkdirTemp(root *os.Root) (string, error) {
	for range ingestNameAttempts {
		rel := path.Join(RootDirName, ingestPrefix+rand.Text())
		err := root.Mkdir(rel, helpers.DirMod)
		if err == nil {
			return rel, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errIngestTempExhausted
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

// writeReadyMarker writes ReadyMarkerPayload into dirRel's ready marker,
// removing any existing file at that path first (ignoring fs.ErrNotExist).
// The remove-then-write is not decorative: a tarball can ship its own
// root-level ".ready" entry, which archive.extractRegularFile extracts
// read-only along with every other regular file, so a plain WriteFile
// here would fail EACCES trying to truncate it in place. Removing first
// always starts from a clean, writable slate regardless of what extraction
// left behind at that path.
func writeReadyMarker(root *os.Root, dirRel string) error {
	p := path.Join(dirRel, ReadyMarker)
	if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.WriteFile(p, []byte(ReadyMarkerPayload), helpers.FileMod)
}

// readReadyMarker reads rel with a hard cap of readyMarkerMaxReadSize+1
// bytes, reporting ok=false for a missing/unreadable file and for a file at
// least one byte over the cap - mirroring
// collections.readExtractMarker's same bounded-read shape.
func readReadyMarker(root *os.Root, rel string) (string, bool) {
	f, err := root.Open(rel)
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

// isReady reports whether the tree at dirRel holds a finalized CAS tree
// written by a binary new enough to apply the write-bit hardening. It checks
// the marker's exact content, not merely its presence: a tree extracted by an
// older binary carries the legacy "ok" sentinel (or, on a much older binary,
// nothing at all in this format) and its regular files are still writable,
// so isReady must reject it here rather than let Ensure/Materialize trust it
// verbatim and hand out unhardened hard links. Rejecting forces
// extractInto/Promote's own RemoveAll-then-rebuild path, which re-extracts
// the tree through the current, hardened archive package.
func isReady(root *os.Root, dirRel string) bool {
	content, ok := readReadyMarker(root, path.Join(dirRel, ReadyMarker))
	return ok && content == ReadyMarkerPayload
}

// Materialize mirrors srcRoot into dstRoot using hardlinks, falling back
// to copy for files that cannot be linked (e.g. cross-device). The
// ReadyMarker at the root is skipped.
//
// No entry below the root creates a parent directory of its own, and none
// needs to: dstRoot is created here before the walk starts,
// filepath.WalkDir visits a directory before anything inside it, and
// materializeEntry's directory arm creates each one as it is visited, so
// every directory on an entry's path exists by the time that entry is
// reached. Every entry the walk creates nothing for is one that cannot hold
// another entry beneath it: srcRoot itself, whose dstRoot counterpart
// already exists; the root's ReadyMarker, which writeReadyMarker leaves a
// regular file on every extraction; and, in materializeEntry's default arm,
// anything that is neither directory, symlink, nor regular file. That makes
// the directory arm and the dstRoot MkdirAll below load-bearing rather than
// conveniences: without either, materializeFile and materializeSymlink have
// no directory to write into.
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
