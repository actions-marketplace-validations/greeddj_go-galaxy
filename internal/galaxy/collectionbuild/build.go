package collectionbuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
)

const (
	// linkMaxHops bounds how many symlinks one target may pass through
	// before the link is refused as a loop. It equals the hop bound
	// internal/galaxy/manifest follows, so no artifact this builder accepts
	// carries a chain that check would refuse.
	linkMaxHops = 8
	// documentsPerArchive is the two entries every artifact carries ahead
	// of the tree: MANIFEST.json and FILES.json. They count against the
	// entry budget like any other.
	documentsPerArchive = 2

	modeFile    = 0o644
	modeExec    = 0o755
	modeDir     = 0o755
	modeSymlink = 0o777
)

// plannedEntry is one tar entry decided in the first pass and written in the
// second. src is the repository path streamed for a regular file; it is
// empty for a directory and a symlink.
type plannedEntry struct {
	name     string
	src      string
	linkname string
	size     int64
	mode     int64
	typeflag byte
}

// builder holds the state of one build: the lazily cached tree, the blob
// digests already computed (a symlink to a file shares its target's), and
// the rows and entries collected so far with the budgets they consumed.
type builder struct {
	src      Source
	dirs     map[string][]Entry
	digests  map[string]string
	root     string
	rows     []filesRow
	entries  []plannedEntry
	warnings []string
	rules    ignoreRules
	total    int64
	count    int64
}

// Build turns one candidate into a tar.gz artifact written into the file
// tempFile supplies. The walk is made twice: once to decide the rows, hash
// every blob and charge the archive budgets, once to stream the bytes, so
// FILES.json and MANIFEST.json can lead the archive as every reader of
// these artifacts expects. The result's Cleanup removes the file and is
// idempotent; on any error the file is already gone.
func Build(ctx context.Context, src Source, cand Candidate, tempFile TempFileFunc) (Built, error) {
	if tempFile == nil {
		return Built{}, fmt.Errorf("%w: no temp file supplier", helpers.ErrConfigIsNil)
	}
	b := &builder{
		src:     src,
		dirs:    make(map[string][]Entry),
		digests: make(map[string]string),
		root:    cand.Subdir,
		rows:    []filesRow{dirRow(rootRowName)},
		rules:   newIgnoreRules(cand.Meta.Namespace, cand.Meta.Name, cand.Meta.BuildIgnore),
		count:   documentsPerArchive,
	}
	if err := b.walk(ctx, rootRowName); err != nil {
		return Built{}, err
	}
	manifestJSON, filesJSON, err := encodeDocuments(&cand.Meta, b.rows)
	if err != nil {
		return Built{}, fmt.Errorf("encoding %s: %w", helpers.ManifestFileName, err)
	}
	// The chain check refuses a FILES.json over helpers.FilesManifestMaxBytes;
	// measured here, before a byte is written, so a tree inside every
	// per-entry cap but listing past this one fails as the budget it broke
	// rather than as a self-check defect of the builder.
	if int64(len(filesJSON)) > helpers.FilesManifestMaxBytes {
		return Built{}, fmt.Errorf("%w: %s is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryIsTooLarge, helpers.FilesManifestFileName, len(filesJSON), helpers.FilesManifestMaxBytes)
	}

	file, cleanup, err := tempFile(ctx)
	if err != nil {
		return Built{}, err
	}
	artifactPath := file.Name()
	digest, err := b.writeArchive(ctx, file, manifestJSON, filesJSON)
	if err != nil {
		_ = file.Close()
		cleanup()
		return Built{}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return Built{}, fmt.Errorf("closing %s: %w", artifactPath, err)
	}
	if err := selfCheck(ctx, artifactPath, manifestJSON); err != nil {
		cleanup()
		return Built{}, err
	}

	var once sync.Once
	deps := make(map[string]string, len(cand.Meta.Dependencies))
	maps.Copy(deps, cand.Meta.Dependencies)
	return Built{
		Cleanup:      func() { once.Do(cleanup) },
		Dependencies: deps,
		Namespace:    cand.Meta.Namespace,
		Name:         cand.Meta.Name,
		Version:      cand.Meta.Version,
		Subdir:       cand.Subdir,
		ArtifactPath: artifactPath,
		SHA256:       digest,
		Warnings:     b.warnings,
	}, nil
}

// selfCheck reads the artifact back the way the pipeline will: the manifest
// must be found and be the bytes just written, and the chain must verify.
// Any failure is this package's defect. The cause is rendered with %v, not
// %w, so a chain refusal never classifies as an integrity failure of the
// remote's bytes - see helpers.ErrGitArtifactSelfCheck.
func selfCheck(ctx context.Context, artifactPath string, manifestJSON []byte) error {
	readBack, err := manifest.ReadFromTarGz(ctx, artifactPath)
	if err != nil {
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: reading the manifest back: %v", helpers.ErrGitArtifactSelfCheck, err)
	}
	if !bytes.Equal(readBack, manifestJSON) {
		return fmt.Errorf("%w: the manifest read back differs from the one written", helpers.ErrGitArtifactSelfCheck)
	}
	if err := manifest.VerifyChain(ctx, artifactPath, manifestJSON); err != nil {
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: %v", helpers.ErrGitArtifactSelfCheck, err)
	}
	return nil
}

// repoPath maps a collection-relative path onto the repository.
func (b *builder) repoPath(rel string) string {
	if rel == rootRowName {
		return b.root
	}
	return joinPath(b.root, rel)
}

// display renders a collection-relative path as its repository path for a
// message or a warning.
func (b *builder) display(rel string) string {
	return displayDir(b.repoPath(rel))
}

func relJoin(dir, name string) string {
	if dir == rootRowName {
		return name
	}
	return dir + "/" + name
}

func (b *builder) readDir(repo string) ([]Entry, error) {
	if entries, ok := b.dirs[repo]; ok {
		return entries, nil
	}
	entries, err := b.src.ReadDir(repo)
	if err != nil {
		return nil, err
	}
	b.dirs[repo] = entries
	return entries, nil
}

// lookup finds the entry at the collection-relative path rel.
func (b *builder) lookup(rel string) (Entry, bool, error) {
	dir, name := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = rootRowName
	}
	entries, err := b.readDir(b.repoPath(dir))
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// walk collects the rows and entries under the collection-relative
// directory relDir in the tree's own order, parents before children.
func (b *builder) walk(ctx context.Context, relDir string) error {
	entries, err := b.readDir(b.repoPath(relDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := relJoin(relDir, e.Name)
		if strings.Count(b.repoPath(rel), "/")+1 > helpers.GitTreeMaxDepth {
			return fmt.Errorf("%w: %s", helpers.ErrGitTreeTooDeep, b.display(rel))
		}
		if err := b.visit(ctx, rel, e); err != nil {
			return err
		}
	}
	return nil
}

// visit handles one entry of the walk: the ignore rules first, with a
// directory's rules applied to a submodule since that is what a checkout
// would show, then the kind's own recording.
func (b *builder) visit(ctx context.Context, rel string, e Entry) error {
	if b.rules.skip(rel, e.Kind == EntryDir || e.Kind == EntrySubmodule) {
		return nil
	}
	switch e.Kind {
	case EntryDir:
		if err := b.addEntry(rel, dirRow(rel), plannedEntry{name: rel, mode: modeDir, typeflag: tar.TypeDir}); err != nil {
			return err
		}
		return b.walk(ctx, rel)
	case EntryFile, EntryExecutable:
		return b.addFile(rel, e)
	case EntrySubmodule:
		b.warnings = append(b.warnings, "skipping submodule "+b.display(rel)+": submodules are never fetched")
		return nil
	case EntrySymlink:
		return b.addSymlink(rel, e)
	default:
		return fmt.Errorf("%w: %s has an unknown entry kind %d", helpers.ErrGitTreeEntryInvalid, b.display(rel), e.Kind)
	}
}

// addEntry charges the name and count budgets and records one row with its
// tar entry.
func (b *builder) addEntry(rel string, row filesRow, entry plannedEntry) error {
	if len(rel) > helpers.ArchiveMaxEntryNameLen {
		return fmt.Errorf("%w: an entry names itself in %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, len(rel), helpers.ArchiveMaxEntryNameLen)
	}
	b.count++
	if b.count > helpers.ArchiveMaxEntryCount {
		return fmt.Errorf("%w: more than %d entries", helpers.ErrArchiveTooManyEntries, helpers.ArchiveMaxEntryCount)
	}
	b.rows = append(b.rows, row)
	b.entries = append(b.entries, entry)
	return nil
}

// addFile charges the size budgets against the declared size, hashes the
// blob and records a file row.
func (b *builder) addFile(rel string, e Entry) error {
	if err := b.chargeSize(rel, e.Size); err != nil {
		return err
	}
	digest, err := b.hashBlob(rel, e.Size)
	if err != nil {
		return err
	}
	mode := int64(modeFile)
	if e.Kind == EntryExecutable {
		mode = modeExec
	}
	return b.addEntry(rel, fileRow(rel, digest),
		plannedEntry{name: rel, src: b.repoPath(rel), size: e.Size, mode: mode, typeflag: tar.TypeReg})
}

// chargeSize charges one file's declared size against the per-entry and
// the per-archive caps, before a byte of it is read.
func (b *builder) chargeSize(rel string, size int64) error {
	if size > helpers.ArchiveMaxEntrySize {
		return fmt.Errorf("%w: %s is %d bytes", helpers.ErrArchiveEntryIsTooLarge, b.display(rel), size)
	}
	if b.total+size > helpers.ArchiveMaxTotalSize {
		return fmt.Errorf("%w: %d bytes", helpers.ErrArchiveExceedsMaxSize, helpers.ArchiveMaxTotalSize)
	}
	b.total += size
	return nil
}

// hashBlob digests the blob at the collection-relative path rel, which must
// stream exactly the size its tree entry declares.
func (b *builder) hashBlob(rel string, declared int64) (string, error) {
	repo := b.repoPath(rel)
	if digest, ok := b.digests[repo]; ok {
		return digest, nil
	}
	r, err := b.src.Open(repo)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", b.display(rel), err)
	}
	if n != declared {
		return "", fmt.Errorf("%w: %s streamed %d bytes where the tree declares %d",
			helpers.ErrGitCommitMismatch, b.display(rel), n, declared)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	b.digests[repo] = digest
	return digest, nil
}

// addSymlink resolves a link through the tree and records it as the row
// its final target warrants - a file row carrying the target's digest, or a
// dir row - with a tar symlink entry pointing straight at that final entry.
// A link whose target the artifact will not carry is skipped with a
// warning rather than refused, as ansible skips a link leaving the
// collection; a dangling or looping one is refused.
func (b *builder) addSymlink(rel string, e Entry) error {
	res, err := b.resolveLink(rel, e)
	if err != nil {
		return err
	}
	if res.reason == "" {
		if res.kind == EntryDir && b.rules.skip(rel, true) {
			return nil
		}
		res.reason = b.skipReason(rel, res)
	}
	if res.reason != "" {
		b.warnings = append(b.warnings, "skipping symlink "+b.display(rel)+": "+res.reason)
		return nil
	}
	linkname := relativeLink(path.Dir(rel), res.final)
	if len(linkname) > helpers.ArchiveMaxEntryNameLen {
		return fmt.Errorf("%w: the link target of %s is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, b.display(rel), len(linkname), helpers.ArchiveMaxEntryNameLen)
	}
	entry := plannedEntry{name: rel, linkname: linkname, mode: modeSymlink, typeflag: tar.TypeSymlink}
	if res.kind == EntryDir {
		return b.addEntry(rel, dirRow(rel), entry)
	}
	digest, err := b.hashBlob(res.final, res.size)
	if err != nil {
		return err
	}
	return b.addEntry(rel, fileRow(rel, digest), entry)
}

// skipReason names why a resolved link is still left out: a target the
// extractor would refuse (its own directory, spelled "."), or one the ignore
// rules keep out of the artifact, which the chain check could not follow.
func (b *builder) skipReason(rel string, res linkResult) string {
	switch {
	case res.final == path.Dir(rel):
		return "target resolves to its own directory, which the extractor refuses"
	case b.rules.excludes(res.final, res.kind == EntryDir):
		return "target is excluded from the build"
	default:
		return ""
	}
}

// linkResult is what resolving one link yields: the final
// collection-relative path with its kind and declared size, or a reason the
// link is skipped.
type linkResult struct {
	final  string
	reason string
	size   int64
	kind   EntryKind
}

// linkState is the resolution in progress: the link whose target is being
// followed, that target as read, and the hops spent so far.
type linkState struct {
	cur    string
	target string
	hops   int
}

// resolveLink follows the link at rel to the real entry it names, through
// directory symlinks on the way and symlinks at the end, up to linkMaxHops.
func (b *builder) resolveLink(rel string, e Entry) (linkResult, error) {
	target, err := b.readLinkTarget(rel, e)
	if err != nil {
		return linkResult{}, err
	}
	st := linkState{cur: rel, target: target, hops: 1}
	for {
		if path.IsAbs(st.target) {
			return linkResult{reason: "target outside the collection"}, nil
		}
		resolved := path.Join(path.Dir(st.cur), st.target)
		if resolved == rootRowName || resolved == ".." || strings.HasPrefix(resolved, "../") {
			return linkResult{reason: "target outside the collection"}, nil
		}
		res, again, err := b.followComponents(rel, resolved, &st)
		if err != nil || !again {
			return res, err
		}
	}
}

// followComponents walks resolved component by component. Meeting a symlink
// on the way rewrites st to continue from it and reports again; otherwise
// the walk ends at the final entry or at a refusal.
func (b *builder) followComponents(rel, resolved string, st *linkState) (linkResult, bool, error) {
	components := strings.Split(resolved, "/")
	prefix := ""
	for i, c := range components {
		prefix = joinPath(prefix, c)
		entry, ok, err := b.lookup(prefix)
		if err != nil {
			return linkResult{}, false, err
		}
		if !ok {
			return linkResult{}, false, fmt.Errorf("%w: %s points at %s, which does not exist",
				helpers.ErrGitSymlinkUnresolvable, b.display(rel), b.display(prefix))
		}
		if entry.Kind == EntrySymlink {
			err := b.hop(rel, prefix, entry, strings.Join(components[i+1:], "/"), st)
			return linkResult{}, err == nil, err
		}
		res, done, err := b.stepEntry(rel, prefix, entry, i == len(components)-1)
		if done {
			return res, false, err
		}
	}
	return linkResult{}, false, fmt.Errorf("%w: %s resolves to nothing", helpers.ErrGitSymlinkUnresolvable, b.display(rel))
}

// hop moves the resolution onto the symlink met at prefix, carrying the
// components not yet walked along behind its target.
func (b *builder) hop(rel, prefix string, entry Entry, rest string, st *linkState) error {
	st.hops++
	if st.hops > linkMaxHops {
		return fmt.Errorf("%w: %s resolves through more than %d links",
			helpers.ErrGitSymlinkUnresolvable, b.display(rel), linkMaxHops)
	}
	next, err := b.readLinkTarget(prefix, entry)
	if err != nil {
		return err
	}
	st.cur = prefix
	st.target = joinPath(next, rest)
	return nil
}

// stepEntry judges one non-link component: a directory is walked through
// or, when last, is the answer; a file is the answer only when last; a
// submodule ends the resolution with a skip.
func (b *builder) stepEntry(rel, prefix string, entry Entry, last bool) (linkResult, bool, error) {
	switch entry.Kind {
	case EntryDir:
		if last {
			return linkResult{final: prefix, kind: EntryDir}, true, nil
		}
		return linkResult{}, false, nil
	case EntryFile, EntryExecutable:
		if last {
			return linkResult{final: prefix, kind: entry.Kind, size: entry.Size}, true, nil
		}
		return linkResult{}, true, fmt.Errorf("%w: %s passes through %s, which is not a directory",
			helpers.ErrGitSymlinkUnresolvable, b.display(rel), b.display(prefix))
	case EntrySubmodule:
		return linkResult{reason: "target is a submodule, which is never fetched"}, true, nil
	case EntrySymlink:
		// followComponents takes every symlink onto hop before it gets here.
		fallthrough
	default:
		return linkResult{}, true, fmt.Errorf("%w: %s has an unknown entry kind %d",
			helpers.ErrGitTreeEntryInvalid, b.display(prefix), entry.Kind)
	}
}

// readLinkTarget reads a link's blob, which is its target, refusing an
// empty one, one with a NUL and one past the name cap.
func (b *builder) readLinkTarget(rel string, e Entry) (string, error) {
	if e.Size > helpers.ArchiveMaxEntryNameLen {
		return "", fmt.Errorf("%w: the link target of %s is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, b.display(rel), e.Size, helpers.ArchiveMaxEntryNameLen)
	}
	r, err := b.src.Open(b.repoPath(rel))
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, helpers.ArchiveMaxEntryNameLen+1))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", b.display(rel), err)
	}
	return checkLinkTarget(b.display(rel), data)
}

// checkLinkTarget applies the shape rules to a link's raw target.
func checkLinkTarget(display string, data []byte) (string, error) {
	switch {
	case len(data) > helpers.ArchiveMaxEntryNameLen:
		return "", fmt.Errorf("%w: the link target of %s is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, display, len(data), helpers.ArchiveMaxEntryNameLen)
	case len(data) == 0:
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetIsEmpty, display)
	case bytes.IndexByte(data, 0) >= 0:
		return "", fmt.Errorf("%w: the target of %s contains a NUL", helpers.ErrSymlinkTarget, display)
	}
	return string(data), nil
}

// relativeLink renders target, a clean collection-relative path, relative to
// the directory fromDir ("." for the root), the way the tar entry's Linkname
// must read for both the extractor and the chain check to land on target.
func relativeLink(fromDir, target string) string {
	var from []string
	if fromDir != rootRowName {
		from = strings.Split(fromDir, "/")
	}
	to := strings.Split(target, "/")
	i := 0
	for i < len(from) && i < len(to) && from[i] == to[i] {
		i++
	}
	parts := make([]string, 0, len(from)-i+len(to)-i)
	for range from[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, to[i:]...)
	return strings.Join(parts, "/")
}

// writeArchive streams the artifact into file, digesting the compressed
// bytes as they are written, and returns the lowercase hex sha256.
func (b *builder) writeArchive(ctx context.Context, file *os.File, manifestJSON, filesJSON []byte) (string, error) {
	h := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(file, h))
	tw := tar.NewWriter(gz)
	when := b.src.CommitTime().UTC().Truncate(time.Second)

	for _, doc := range []struct {
		name string
		data []byte
	}{{helpers.ManifestFileName, manifestJSON}, {helpers.FilesManifestFileName, filesJSON}} {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: doc.name, Size: int64(len(doc.data)), Mode: modeFile, ModTime: when}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", fmt.Errorf("writing %s: %w", doc.name, err)
		}
		if _, err := tw.Write(doc.data); err != nil {
			return "", fmt.Errorf("writing %s: %w", doc.name, err)
		}
	}
	for i := range b.entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := b.writeEntry(tw, &b.entries[i], when); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("closing the tar stream: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("closing the gzip stream: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeEntry writes one planned entry. A regular file is re-opened and must
// stream exactly the size the first pass charged; a longer stream is
// refused before the tar writer sees a byte past it.
func (b *builder) writeEntry(tw *tar.Writer, entry *plannedEntry, when time.Time) error {
	hdr := &tar.Header{
		Typeflag: entry.typeflag,
		Name:     entry.name,
		Linkname: entry.linkname,
		Size:     entry.size,
		Mode:     entry.mode,
		ModTime:  when,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing %s: %w", displayDir(entry.src), err)
	}
	if entry.typeflag != tar.TypeReg {
		return nil
	}
	r, err := b.src.Open(entry.src)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	n, err := io.CopyN(tw, r, entry.size)
	if err != nil || n != entry.size {
		return fmt.Errorf("%w: %s streamed %d bytes where the tree declares %d",
			helpers.ErrGitCommitMismatch, displayDir(entry.src), n, entry.size)
	}
	var probe [1]byte
	if extra, _ := r.Read(probe[:]); extra > 0 {
		return fmt.Errorf("%w: %s streams more than the %d bytes the tree declares",
			helpers.ErrGitCommitMismatch, displayDir(entry.src), entry.size)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
