package manifest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/klauspost/pgzip"
)

const (
	// chainHashBufSize is the copy buffer one scan reuses for every regular
	// file it digests, so an archive of any size costs one buffer rather than
	// one per entry.
	chainHashBufSize = 64 << 10
	// chainMaxLinkHops bounds how far a listed name may be followed through
	// link entries before the archive is refused. It is a bound rather than a
	// refusal because the reference reader resolves a link pointing at a link:
	// measured against Python's tarfile, a three-deep symlink chain hands back
	// the file's content at every step, so refusing the second hop would reject
	// an archive that library reads without complaint. Following an unbounded
	// chain is the other extreme, a loop the archive gets to size. The same
	// bound is the cycle guard, since a cycle is a chain that never ends.
	chainMaxLinkHops = 8
	// filesEntryTypeFile is the ftype FILES.json gives a row whose digest
	// describes file content. Every other ftype - "dir", most commonly - is
	// recorded as listed and not digested.
	filesEntryTypeFile = "file"
	// filesListingKey is the one key of FILES.json this reader reads: the array
	// of rows describing what the archive carries.
	filesListingKey = "files"
	// chksumTypeSHA256 is the one checksum algorithm named in these documents
	// that this reader verifies.
	chksumTypeSHA256 = "sha256"
)

// Nibble arithmetic for sha256FromHex, named rather than spelled inline: one
// digest byte is two hex digits, the first of which carries the high four bits,
// and a letter digit's value starts where the decimal digits end.
const (
	hexDigitsPerByte   = 2
	hexHighNibbleShift = 4
	hexLetterValue     = 10
)

// VerifyChain checks that a collection artifact's contents are the ones its
// MANIFEST.json vouches for. MANIFEST.json names FILES.json's digest, FILES.json
// names a digest for every other file, and this walks both links against the
// archive's actual bytes. manifestJSON is the document a signature was verified
// over; artifactPath names the artifact that signature arrived with.
//
// Without this walk a verified signature says almost nothing. A detached
// OpenPGP signature is made over MANIFEST.json alone, so verifying one makes
// exactly one document trustworthy and leaves every other byte of the tarball
// unattested. That is not a theoretical gap. A principal holding the Galaxy
// server - or the cache, which this project's trust model already treats as
// writable by an attacker - takes a legitimately signed MANIFEST.json byte for
// byte and staples it onto a tarball of their choosing; the artifact's own
// sha256 is no help, because the same server declared that too. So the tar
// stream reaching this function is fully attacker-chosen even when the
// signature verified, and it is bounded here exactly as archive.ExtractTarGz
// bounds one: the entry count, the declared per-entry and per-archive sizes,
// and a decompressed-stream cap at exactly helpers.ArchiveMaxDecompressedSize -
// not lower, since this pass has to read every body to hash it and a tighter
// cap would refuse artifacts the extractor accepts, and not higher, since it
// must not accept more. One bound the extractor carries none of applies here,
// helpers.ArchiveMaxEntryNameLen: this pass retains every entry's name until
// the stream ends, and the extractor's own retention is bounded by the
// filesystem instead - see that constant for the argument.
//
// The archive is opened a second time rather than shared with whatever read the
// manifest, and that is sound only because the seam is checked: the archive's
// own MANIFEST.json entry must be a regular file whose content hashes to
// sha256.Sum256(manifestJSON). That check is what binds signature to bytes to
// archive - a manifest verified out of one file and a chain walked over another
// is no chain at all - and it is why a path is the right argument here despite
// the second open. A shared reader would need a caller-side rewind nothing
// enforces, and the two passes read differently anyway: ReadFromTarGz stops at
// helpers.ManifestScanMaxBytes, this one reads the artifact to its end.
//
// Two names are exempt from the rule that the archive may carry nothing
// FILES.json does not list, and the exemption is deliberately asymmetric rather
// than a blanket one. Both MANIFEST.json and FILES.json are accepted when
// unlisted, since a listing naming the documents that name it is a convention
// rather than a guarantee. A file row for FILES.json is skipped in the forward
// direction as well - the skip is keyed on that ftype, so a row giving the name
// any other one is refused like any other non-file row standing over a regular
// file - for a structural reason rather than a lenient one: a listing cannot
// state its own digest, and MANIFEST.json's pointer already pins those bytes
// from inside the signed document, which is a strictly stronger authority than
// a self-reference would be. MANIFEST.json listed with a digest gets no such
// skip and is checked like any other file. That is stated plainly rather than
// sold as costing nothing: such a row can only ever be refused, because the
// manifest's own bytes carry the listing's digest, so no listing can name the
// manifest's digest without predicting bytes that depend on it. Refusing is
// the right verdict for a document asserting something it cannot have
// computed, and exempting MANIFEST.json in this direction too would instead
// accept that assertion unread.
//
// A link entry is digested as its target's content, resolved inside the
// archive. That matches how these archives are read: Python's tarfile, which
// `ansible-galaxy collection build` writes them with, hands back the in-archive
// target's bytes for a link member, resolving a symlink's target against the
// member's own directory and a hardlink's against the archive root (measured
// against tarfile, both directions, including a hardlink target written
// directory-relative, which it refuses). go-galaxy's extractor reaches the same
// outcome by a different mechanism: it materializes a real relative symlink and
// extracts the in-archive target beside it as a regular file, so reading
// through the extracted link yields the bytes this pass hashed. The one
// divergence is one-directional and safe, and it is narrower than the sentence
// naming it first suggests: a link the LISTING NAMES AS A FILE, whose
// in-archive target the archive does not carry, is refused here, where the
// extractor would materialize a dangling link. A link the listing names as
// anything else is never resolved at all, so a dangling one under such a row is
// accepted - the case a real collection carries is a symlink to a directory,
// recorded with ftype "dir". That is deliberate rather than an omission:
// recordSymlink already refuses a target escaping the archive, so such a link
// can point only at attested content or at nothing, and the rule that would
// close it would refuse that legitimate collection.
//
// ctx is checked on every read taken out of the decompressor, so a canceled
// check stops at the next read rather than at the end of the archive.
//
// Every value it touches is local, one file is opened read-only, and nothing
// anywhere is written, so it is safe to call concurrently. That is a claim
// about the code rather than an observed race-detector verdict: no test is
// written to prove it.
//
// What it does not answer: whether those digests still describe what a reader
// gets after extraction. This pass hashes the archive, and a local writer
// racing inside an already-extracted tree is the same disclosed residual
// extractCollection and cleanup's removeInstalled already carry. It also says
// nothing about who vouched for MANIFEST.json - that verdict belongs to
// internal/galaxy/signature and must already have been reached, since a chain
// walked from an unsigned manifest proves only that an archive agrees with
// itself.
func VerifyChain(ctx context.Context, artifactPath string, manifestJSON []byte) error {
	//nolint:gosec // artifactPath names an artifact this run downloaded or produced.
	file, err := os.Open(artifactPath)
	if err != nil {
		return fmt.Errorf("failed to open artifact to verify its manifest chain: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	if err := verifyChainStream(ctx, file, manifestJSON, helpers.ArchiveMaxDecompressedSize); err != nil {
		return fmt.Errorf("%s: %w", artifactPath, err)
	}
	return nil
}

// verifyChainStream is VerifyChain with the file open lifted out and the
// decompressed-stream cap injected, so a test can drive that cap with a fixture
// measured in kilobytes instead of one that has to reach the production
// ceiling - the seam archive.extractTarGzStream already draws for the identical
// reason. It does not close r.
//
// The pointer is parsed before a byte of the archive is read: a manifest that
// does not name FILES.json at all cannot be answered by anything in the tar
// stream, so reading one would be work spent on a question already settled.
func verifyChainStream(ctx context.Context, r io.Reader, manifestJSON []byte, maxDecompressed int64) error {
	wantFiles, err := parseChainPointer(manifestJSON)
	if err != nil {
		return err
	}

	uncompressed, err := pgzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %w", helpers.ErrArtifactNotTarGz, err)
	}
	defer func() {
		_ = uncompressed.Close()
	}()

	limited := &limitReader{r: uncompressed, over: helpers.ErrArchiveDecompressedTooLarge, max: maxDecompressed}
	scan, err := scanArchive(tar.NewReader(&contextReader{ctx: ctx, r: limited}))
	if err != nil {
		return err
	}
	return verifyChainScan(scan, wantFiles, manifestJSON)
}

// contextReader fails a read once ctx is done, so a chain check stops at the
// next read instead of running to the end of the archive. It reports ctx.Err()
// unwrapped, keeping context.Canceled and context.DeadlineExceeded reachable
// through errors.Is for cmd/go-galaxy/exitcode to classify - the same shape and
// the same reason as the extractor's own reader of this name.
//
// It holds the context in a field rather than taking one per call, which is the
// only shape io.Reader allows: Read's signature is fixed, so a reader that must
// observe cancellation has nowhere else to keep it.
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

// chainPointer is the one field of MANIFEST.json this package reads: the name
// and digest of the file listing the rest of the archive.
//
// It is a local type rather than the vendored hub one, and deliberately so.
// The hub package declares the sibling listing's per-file digest as `any`,
// which costs an interface allocation and a type switch per entry where a
// declared string gets the same fail-closed behavior straight from the decoder
// (measured: a JSON null decodes to "" with a nil error, a number or a bool
// yields a *json.UnmarshalTypeError). A rename in a dependency arriving through
// a grouped bump would also silently turn this check into a no-op, since an
// unmatched json tag is not an error. encoding/json skips the keys not named
// here without allocating for them, so reading one field costs no more than
// reading the document would.
type chainPointer struct {
	FileManifestFile struct {
		Name         string `json:"name"`
		ChksumType   string `json:"chksum_type"`
		ChksumSha256 string `json:"chksum_sha256"`
	} `json:"file_manifest_file"`
}

// filesEntry is one row of FILES.json.
type filesEntry struct {
	Name         string `json:"name"`
	Ftype        string `json:"ftype"`
	ChksumType   string `json:"chksum_type"`
	ChksumSha256 string `json:"chksum_sha256"`
}

// archiveScan is everything one pass over the tar stream retains: a digest per
// regular file, a target per link entry, FILES.json's own bytes, and the order
// the entries arrived in.
//
// The order is kept because the reverse rule reports one offending path out of
// however many are unlisted, and Go's map iteration would make which one it
// names vary between two runs over the identical archive. An operator reading
// that line has to be able to go and look at the entry it names.
type archiveScan struct {
	hashes map[string][32]byte
	links  map[string]string
	files  []byte
	order  []string
}

// chainScanner is the per-call state a scan reuses across entries: the hash,
// the copy buffer it digests through, and the scratch the digest is summed
// into. None of the three may be allocated per entry.
type chainScanner struct {
	scan    *archiveScan
	digest  hash.Hash
	buf     []byte
	scratch [sha256.Size]byte
}

// scanArchive walks the whole tar stream once, digesting every regular file,
// recording every link's target, and holding FILES.json's bytes aside.
//
// One pass is enough and the order of the entries does not matter: nothing is
// compared until EOF, so FILES.json may sit anywhere in the stream. Requiring
// it at a fixed position would be a narrowing of what this reader accepts
// rather than a defense, since a hostile archive can put it wherever the rule
// demands.
func scanArchive(tarReader *tar.Reader) (*archiveScan, error) {
	scanner := &chainScanner{
		scan:   &archiveScan{hashes: make(map[string][32]byte)},
		digest: sha256.New(),
		buf:    make([]byte, chainHashBufSize),
	}

	var declared, entries int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return scanner.scan, nil
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read the artifact's tar stream: %w", err)
		}
		entries++
		if entries > helpers.ArchiveMaxEntryCount {
			return nil, fmt.Errorf("%w: %d", helpers.ErrArchiveTooManyEntries, helpers.ArchiveMaxEntryCount)
		}
		// Ahead of chargeEntrySize, which renders header.Name with %q on two of
		// its three arms and would otherwise do so while the name is still
		// unbounded. Measured: a 200,000-byte name with an over-cap declared
		// size produced a 200,046-byte error message, and 88 bytes once the cap
		// runs first.
		if err := checkEntryNameLengths(header); err != nil {
			return nil, err
		}
		if err := chargeEntrySize(header, &declared); err != nil {
			return nil, err
		}
		if err := scanner.entry(tarReader, header); err != nil {
			return nil, err
		}
	}
}

// chargeEntrySize charges header.Size against the per-entry and per-archive
// byte budgets and, on success, adds it to the running total. It is the twin of
// archive.chargeEntrySize and refuses the same three shapes at the same moment:
// on every header, before the typeflag is dispatched on, so an entry this pass
// ignores is charged exactly like one it digests.
//
// What the two share is the constants in helpers, not the code. The archive
// one stays unexported because exporting it would make one package's extraction
// bounds another package's API, and each reader has its own reason to charge:
// there, an entry read past still costs decompressed bytes; here, an entry
// skipped still costs a header the archive got to declare.
//
// The reasoning about what a declared size is and is not an upper bound on -
// sparse entries, PAX and GNU meta headers, the amplification that survives
// below the stream cap - is recorded once, on archive.chargeEntrySize, and
// applies here unchanged.
func chargeEntrySize(header *tar.Header, declared *int64) error {
	if header.Size < 0 {
		return fmt.Errorf("%w: %q", helpers.ErrArchiveEntryHasNegativeSize, header.Name)
	}
	if header.Size > helpers.ArchiveMaxEntrySize {
		return fmt.Errorf("%w %q: %d bytes", helpers.ErrArchiveEntryIsTooLarge, header.Name, header.Size)
	}
	if *declared+header.Size > helpers.ArchiveMaxTotalSize {
		return fmt.Errorf("%w: %d bytes", helpers.ErrArchiveExceedsMaxSize, helpers.ArchiveMaxTotalSize)
	}
	*declared += header.Size
	return nil
}

// entry records one tar entry into the scan.
//
// The typeflag is dispatched on first, so an entry this pass has nothing to say
// about costs no path work and retains nothing. That is an argument about
// path.Clean, which allocates, and about a key held until the stream ends - not
// about measuring a name, which scanArchive has already done by the time this
// runs and which is a field read either way. Ignoring a directory is right for
// a reason of its own rather than by analogy with the extractor, which creates
// directories: a directory carries no content to digest, and a listing row for
// one names no digest to compare it against - it carries a null where a file's
// row carries a sha256.
func (s *chainScanner) entry(tarReader *tar.Reader, header *tar.Header) error {
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
	default:
		return nil
	}
	key, err := cleanEntryPath(header.Name)
	if err != nil {
		return err
	}
	if key == "" {
		// The entry's name normalized away to the archive root, which names no
		// file. Skipped rather than refused, matching the extractor's own
		// handling of the same shape.
		return nil
	}
	if err := s.scan.claim(key); err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeSymlink:
		return s.scan.recordSymlink(key, header.Linkname)
	case tar.TypeLink:
		return s.scan.recordHardlink(key, header.Linkname)
	default:
		return s.recordRegular(tarReader, header, key)
	}
}

// checkEntryNameLength refuses one tar entry's own name past
// helpers.ArchiveMaxEntryNameLen, reporting its length rather than the name.
//
// Both passes this package makes over an archive apply it, ahead of anything
// that renders a name, and each has its own reason to: the chain scan retains
// every name until the stream ends, while readFromTarGzStream retains none and
// still renders one, in the refusal it raises for an over-declared entry. So a
// tar entry's name reaching a refusal either pass composes is one this rule has
// already measured.
//
// The refusal reports a length rather than the name, the same trade
// checkFieldLength makes on the two documents' own string fields, and being an
// exception to rendering an archive-chosen value with %q is the point of it:
// the name is what is being refused for its size, so rendering it would put a
// megabyte of archive-chosen bytes on an operator's terminal to explain that a
// megabyte was too much.
func checkEntryNameLength(name string) error {
	if len(name) <= helpers.ArchiveMaxEntryNameLen {
		return nil
	}
	return fmt.Errorf("%w: an entry names itself in %d bytes, the limit is %d",
		helpers.ErrArchiveEntryNameTooLong, len(name), helpers.ArchiveMaxEntryNameLen)
}

// checkEntryNameLengths refuses an entry naming itself, or its link target,
// past helpers.ArchiveMaxEntryNameLen.
//
// It runs on every header the stream hands back, whatever the typeflag, and
// ahead of every other rule this pass renders a name in - chargeEntrySize among
// them. So a directory's name is capped exactly like a regular file's, which is
// more consistent rather than less, and no message this pass produces can carry
// a name nothing has measured.
//
// The link target is this pass's own half of the rule rather than the shared
// one's, since the scan that reads a manifest never looks at one. It reports a
// length for the reason argued on checkEntryNameLength, and the entry's own
// name IS quoted alongside it, which is reached only once that name has passed
// the same cap.
func checkEntryNameLengths(header *tar.Header) error {
	if err := checkEntryNameLength(header.Name); err != nil {
		return err
	}
	if header.Typeflag != tar.TypeSymlink && header.Typeflag != tar.TypeLink {
		return nil
	}
	if len(header.Linkname) > helpers.ArchiveMaxEntryNameLen {
		return fmt.Errorf("%w: the link target of %q is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, header.Name, len(header.Linkname), helpers.ArchiveMaxEntryNameLen)
	}
	return nil
}

// cleanEntryPath normalizes one tar entry name into the key this scan holds it
// under, or refuses it. An empty return with a nil error means the name
// normalized to the archive root, which the caller skips.
//
// The name is cleaned with path, not filepath, and archive.sanitizeArchivePath
// is deliberately not reused rather than merely not imported: that one is
// filepath-based, while a tar entry name is slash-separated by specification on
// every platform. This package already made that argument once, on
// topLevelManifest, and the consequence is the same - a backslash is an
// ordinary character inside one path element here, not a separator whose
// meaning changes with the operating system the check runs on.
func cleanEntryPath(name string) (string, error) {
	if name == "" {
		return "", helpers.ErrArchiveEntryHasEmptyName
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", nil
	}
	if path.IsAbs(cleaned) {
		return "", fmt.Errorf("%w: %q", helpers.ErrArchiveEntryIsAbsolutePath, name)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %q", helpers.ErrArchiveEntryEscapesDestination, name)
	}
	return cleaned, nil
}

// claim reserves key for one entry and records the archive's ordering of it.
//
// The duplicate refusal is required rather than incidental. It removes the
// first-versus-last ambiguity between this pass and ReadFromTarGz, which takes
// the first match it walks past, and it mirrors the extractor, which refuses
// two entries competing for one path outright. Without it, an archive could
// present one MANIFEST.json to the pass that verified a signature and another
// to the pass that walks the chain.
func (a *archiveScan) claim(key string) error {
	_, hashed := a.hashes[key]
	_, linked := a.links[key]
	if hashed || linked {
		return fmt.Errorf("%w: %q", helpers.ErrArchiveDuplicateEntry, key)
	}
	a.order = append(a.order, key)
	return nil
}

// recordSymlink resolves a symlink entry's target against the entry's own
// directory and records it, deferring the lookup until the whole stream has
// been read - the target may not have arrived yet.
func (a *archiveScan) recordSymlink(key, linkname string) error {
	if linkname == "" {
		return fmt.Errorf("%w for %q", helpers.ErrSymlinkTargetIsEmpty, key)
	}
	// Tested on the raw value, before any join. path.Join("a", "/etc/passwd")
	// yields "a/etc/passwd", so an absolute target checked after the join has
	// already been silently rewritten into an in-archive one and would pass.
	if path.IsAbs(linkname) {
		return fmt.Errorf("%w: %q", helpers.ErrSymlinkTargetIsAbsolute, linkname)
	}
	resolved := path.Join(path.Dir(key), linkname)
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%w: %q", helpers.ErrSymlinkTargetEscapesDestination, linkname)
	}
	a.link(key, resolved)
	return nil
}

// recordHardlink records a hardlink entry's target, which is relative to the
// archive root rather than to the entry's own directory - the one way the two
// link typeflags differ here. Both the extractor, which resolves a hardlink's
// linkname straight against the destination directory, and the reference reader
// read it that way; a directory-relative hardlink target resolves to nothing in
// either.
func (a *archiveScan) recordHardlink(key, linkname string) error {
	if linkname == "" {
		return fmt.Errorf("%w for %q", helpers.ErrHardlinkTargetIsEmpty, key)
	}
	target, err := cleanEntryPath(linkname)
	if err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("%w for %q", helpers.ErrHardlinkTargetIsEmpty, key)
	}
	a.link(key, target)
	return nil
}

// link records one resolved link, allocating the map on first use: a collection
// carrying no links pays nothing for the ones it does not have.
func (a *archiveScan) link(key, target string) {
	if a.links == nil {
		a.links = make(map[string]string)
	}
	a.links[key] = target
}

// recordRegular digests one regular file into the scan, routing FILES.json to
// the arm that keeps its bytes.
func (s *chainScanner) recordRegular(tarReader *tar.Reader, header *tar.Header, key string) error {
	if key == helpers.FilesManifestFileName {
		return s.recordFilesManifest(tarReader, header.Size)
	}

	// The buffer is really used, and that rests on both halves of
	// io.CopyBuffer's dispatch, which ignores the caller's buffer whenever the
	// source implements io.WriterTo OR the destination implements
	// io.ReaderFrom. Verified on go1.26.6 that neither holds: archive/tar keeps
	// its Reader's writeTo unexported, and crypto/sha256's digest has no
	// ReadFrom. Were either to gain one, every entry would instead be copied
	// through a 32 KiB buffer allocated per call inside that dispatch, and this
	// one would go unused.
	s.digest.Reset()
	if _, err := io.CopyBuffer(s.digest, tarReader, s.buf); err != nil {
		return fmt.Errorf("failed to read the artifact's tar stream: %w", err)
	}
	// Summed into a reusable array rather than onto a fresh nil slice, which
	// costs 32 B and one allocation per entry.
	s.scan.hashes[key] = [sha256.Size]byte(s.digest.Sum(s.scratch[:0]))
	return nil
}

// recordFilesManifest reads the FILES.json entry whole, since the document has
// to be decoded rather than merely hashed, and refuses one whose header
// declares more than helpers.FilesManifestMaxBytes.
//
// The refusal is on the declared size, before the body is read, because the
// declaration is what sizes the allocation. size is already known non-negative
// and within helpers.ArchiveMaxEntrySize: chargeEntrySize refused both shapes
// before this entry was dispatched.
func (s *chainScanner) recordFilesManifest(tarReader *tar.Reader, size int64) error {
	if size > helpers.FilesManifestMaxBytes {
		return fmt.Errorf("%w %s: %d bytes", helpers.ErrArchiveEntryIsTooLarge, helpers.FilesManifestFileName, size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(tarReader, body); err != nil {
		return fmt.Errorf("failed to read the artifact's tar stream: %w", err)
	}
	s.scan.files = body
	s.scan.hashes[helpers.FilesManifestFileName] = sha256.Sum256(body)
	return nil
}

// parseChainPointer reads the digest MANIFEST.json names for FILES.json.
//
// chksum_type is deliberately not checked here. A pointer naming some other
// algorithm carries a value that is not a sha256, so it lands on the digest
// shape refusal below rather than needing a rule of its own - and one rule that
// judges the value beats two that judge the label and the value separately.
func parseChainPointer(manifestJSON []byte) ([32]byte, error) {
	// Failing closed on a document that does not decode cleanly into the
	// pointer shape is a verdict rather than a nicety, and the reason is a
	// split inside encoding/json worth stating. It checks syntax over the whole
	// document before writing any of it, so a SYNTAX error leaves doc zero and
	// the name check below would have refused it anyway. A TYPE error is
	// different: the decoder writes every field it could and reports at the
	// end, so a manifest whose chksum_type is a number still arrives here
	// naming FILES.json with a well-formed digest, and both arms below let it
	// through. Measured on go1.26.6, both directions.
	var doc chainPointer
	if err := json.Unmarshal(manifestJSON, &doc); err != nil {
		return [32]byte{}, fmt.Errorf("%w: %s does not parse: %w",
			helpers.ErrManifestChainMismatch, helpers.ManifestFileName, err)
	}
	if err := checkPointerFieldLengths(&doc); err != nil {
		return [32]byte{}, err
	}
	if path.Clean(doc.FileManifestFile.Name) != helpers.FilesManifestFileName {
		return [32]byte{}, fmt.Errorf("%w: %s points at %q rather than at %s",
			helpers.ErrManifestChainMismatch, helpers.ManifestFileName, doc.FileManifestFile.Name, helpers.FilesManifestFileName)
	}
	// Refusing here rather than downstream is what keeps this whole check off
	// the archive: a manifest that cannot name a digest is answered before a
	// byte is decompressed, which is the property
	// TestVerifyChainReadsThePointerBeforeTheArchive pins by handing VerifyChain
	// a file that is not an archive at all. Reached with a real archive instead,
	// this arm would decide only the message, since an unparsed digest is the
	// zero array and no listing hashes to that.
	want, ok := sha256FromHex(doc.FileManifestFile.ChksumSha256)
	if !ok {
		return [32]byte{}, fmt.Errorf("%w: %s names %q as the digest of %s, which is not a sha256",
			helpers.ErrManifestChainMismatch, helpers.ManifestFileName,
			doc.FileManifestFile.ChksumSha256, helpers.FilesManifestFileName)
	}
	return want, nil
}

// verifyChainScan checks the scanned archive against the signed manifest, in
// the one order that lets each step assume the last: the archive is the one the
// manifest describes, it carries the listing that manifest names, the listing
// is what it claims to be, every listed file matches, and nothing is carried
// that is not listed.
func verifyChainScan(scan *archiveScan, wantFiles [32]byte, manifestJSON []byte) error {
	if got, ok := scan.hashes[helpers.ManifestFileName]; !ok || got != sha256.Sum256(manifestJSON) {
		return fmt.Errorf("%w: the archive's %s is not the document that was verified",
			helpers.ErrManifestChainMismatch, helpers.ManifestFileName)
	}
	// A message-not-verdict arm, and it stands in front of three others rather
	// than one. Absent the entry, gotFiles is the zero array, so the comparison
	// one line below refuses it - no listing hashes to that. The decode further
	// down is the second, reached only when the pointer is 64 zeros, a value
	// IsSHA256Hex accepts and which decodes to exactly that array. checkUnlisted
	// is the third, since an empty listing leaves every entry the archive
	// carries unlisted. Measured: this arm removed alongside any two of those
	// still refuses, and only removing all four accepts the artifact. It is
	// kept because "the archive carries no regular-file FILES.json" is
	// actionable where none of the other three messages is, and it is untested
	// as a rule of its own because no errors.Is assertion separates them.
	gotFiles, ok := scan.hashes[helpers.FilesManifestFileName]
	if !ok {
		return fmt.Errorf("%w: the archive carries no regular-file %s",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName)
	}
	if gotFiles != wantFiles {
		return fmt.Errorf("%w: %s does not match the digest %s names for it",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, helpers.ManifestFileName)
	}

	listed, err := verifyListing(scan)
	if err != nil {
		return err
	}
	return checkUnlisted(scan, listed)
}

// verifyListing walks FILES.json forward - every row against the archive - and
// returns the set of names it listed, whatever their ftype.
//
// "Listed" has to mean "appears in files at all" rather than "appears with
// ftype file", and that is what the returned set records. A row's ftype is a
// claim about what the listing takes the path to be, not about the typeflag the
// tar carries, and the two legitimately disagree: a symlink pointing at a
// directory is recorded as a directory row while arriving as a symlink entry. A
// set built from the file rows alone would refuse that collection through the
// reverse rule below, which is what the returned set is handed to.
//
// The document is streamed rather than decoded whole into a slice of rows, and
// that is a bound rather than a preference. helpers.FilesManifestMaxBytes caps
// the bytes; nothing caps what they decode into. Measured on go1.26.6 against
// the production row type: a 32 MiB body of empty rows - the cap exactly, and
// 31.9 KiB on the wire after gzip - decodes to 11,184,807 rows behind a 746.2
// MiB backing array, leaving 1375.4 MiB on the heap before a single row has
// been looked at. Streaming holds one row at a time, so the peak is a row
// rather than the listing, and it costs a real collection nothing to get that:
// measured over a 20,000-row listing, about 6% quicker with half the bytes
// allocated, 10.1 MB down to 5.0 MB, since the backing array and its growth
// doubling stop existing. The allocation COUNT rises the other way, 80,097 to
// 100,085, which is the trade and is worth stating rather than rounding off.
//
// The presize hint comes from the archive rather than from the document.
// scan.order is bounded by helpers.ArchiveMaxEntryCount at scan time, so the
// hint is bounded by construction; a count the listing declares - or one a
// first pass counts out of it - is arithmetic the attacker chooses.
func verifyListing(scan *archiveScan) (map[string]struct{}, error) {
	listed := make(map[string]struct{}, len(scan.order))
	if err := walkListingDocument(json.NewDecoder(bytes.NewReader(scan.files)), scan, listed); err != nil {
		return nil, err
	}
	return listed, nil
}

// walkListingDocument reads the listing's top-level object: its opening brace,
// each of its keys, the brace that closes it, and the end of the document.
//
// A second "files" key is refused rather than resolved. json.Unmarshal takes
// the LAST of two while a streaming loop naturally takes the FIRST - measured,
// both directions - so a document carrying both is read differently by two
// readers of one signed archive. That is the first-versus-last ambiguity claim
// already refuses on the tar side, arriving inside a document nothing else in
// the chain would notice. Zero "files" keys is a different shape and stays
// accepted: it yields an empty set, and every entry the archive carries is then
// refused by checkUnlisted, which is the right answer for a listing describing
// nothing.
//
// Any other key is skipped whole. These documents carry more than this reader
// reads - "format" among them - so refusing what is not understood would refuse
// real collections.
//
// Nothing may follow the object. json.Unmarshal refuses trailing content and so
// does the reference reader, Python's json.loads, so accepting it would make
// this the only reader of the three that disagrees about where a signed
// document ends. Measured: a loop stopping at the closing bracket accepted both
// a document with garbage after its final brace and one with garbage in place
// of that brace.
func walkListingDocument(dec *json.Decoder, scan *archiveScan, listed map[string]struct{}) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	var walked bool
	for {
		tok, err := dec.Token()
		if err != nil {
			return listingParseError(err)
		}
		key, ok := tok.(string)
		if !ok {
			// The brace closing the object: a key is a string by construction,
			// so nothing else can stand in this position.
			break
		}
		if key != filesListingKey {
			if err := skipListingValue(dec); err != nil {
				return err
			}
			continue
		}
		if walked {
			return fmt.Errorf("%w: %s carries more than one %q key",
				helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, filesListingKey)
		}
		walked = true
		if err := walkListingRows(dec, scan, listed); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %s carries content after its top-level object",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName)
	}
	return nil
}

// walkListingRows reads the array under the "files" key, one row at a time.
//
// The row cap is helpers.ArchiveMaxEntryCount rather than a constant of its
// own, and it is that constant as a property of the thing described rather than
// by convenience: a listing describes an archive, an archive may carry at most
// that many entries, so a listing with more rows describes an archive this
// reader would refuse before ever reaching its listing. The sentinel is the
// chain one rather than ErrArchiveTooManyEntries for the reason listedName
// already gives - a bad name in the tar is a bad archive, the same name in
// FILES.json is a bad listing - which also keeps the refusal in the same exit
// class as every other verdict about this document.
//
// It is one row short of what a full archive can legitimately produce, and
// deliberately so: a well-formed listing carries a "." row for the collection
// root, matching no archive entry, so a listing describing an archive at
// exactly the entry ceiling has one more row than that ceiling. The
// off-by-one is written down rather than corrected with a +1 because real
// collections run well below the cap, while a bound spelled "one more than the
// entry ceiling" is a number nobody could justify to the next reader.
func walkListingRows(dec *json.Decoder, scan *archiveScan, listed map[string]struct{}) error {
	if err := expectDelim(dec, '['); err != nil {
		return err
	}
	var rows int64
	for dec.More() {
		rows++
		if rows > helpers.ArchiveMaxEntryCount {
			return fmt.Errorf("%w: %s lists more than %d entries",
				helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, helpers.ArchiveMaxEntryCount)
		}
		// Declared inside the loop because json.Decoder.Decode does not zero
		// its destination, and the failure that costs is a fail-open one:
		// hoisted, a row omitting a field keeps the previous row's value for
		// it, so a row with no ftype inherits "dir" and skips its content
		// check. Measured on go1.26.6, with the variable hoisted, the row
		// {"name":"b"} came back as
		// {Name:b Ftype:file ChksumType:sha256 ChksumSha256:ff}.
		var row filesEntry
		if err := dec.Decode(&row); err != nil {
			// Every decoder failure is returned, a TYPE error included, and
			// that is the same split parseChainPointer's own decode turns on:
			// encoding/json writes every field it could before reporting a type
			// error, so a row whose ftype is a number arrives here partially
			// populated and would otherwise be judged on the fields that did
			// decode.
			return listingParseError(err)
		}
		if err := checkListedRow(scan, listed, &row); err != nil {
			return err
		}
	}
	return expectDelim(dec, ']')
}

// checkListedRow checks one row of FILES.json: the lengths of its fields, the
// path it names, and - for a file row - the digest it claims against what the
// archive carries. It records the name as listed whatever the row's ftype,
// which is the set verifyListing hands back.
//
// A row claiming not to be a file may not name a regular-file entry. Without
// that rule the row marks its path listed and nothing ever checks the content
// under it, so the archive may carry a regular file of arbitrary bytes at a
// path the signed listing calls a directory. Measured: an artifact carrying
// "rm -rf /\n" at a path listed with ftype "dir" was accepted, and so was the
// same file under an ftype no reader knows.
//
// The rule is about regular files rather than about the typeflag or about
// links, and narrowing it that far is what keeps a legitimate collection
// acceptable: a symlink pointing at a directory is recorded with ftype "dir" by
// the builder these archives come from, and it lands in scan.links rather than
// in scan.hashes, so this rule never sees it. The two residuals that leaves are
// accepted rather than overlooked. A hardlink under a non-file row resolves to
// an in-archive path, which is itself either listed and verified or refused by
// checkUnlisted, and when the archive carries no such path the extractor's own
// os.Link fails with ENOENT. A symlink under a non-file row is contained by
// recordSymlink's escape refusal, so it can point only at attested content or
// at nothing.
func checkListedRow(scan *archiveScan, listed map[string]struct{}, row *filesEntry) error {
	if err := checkListedFieldLengths(row); err != nil {
		return err
	}
	key, err := listedName(row.Name)
	if err != nil {
		return err
	}
	if _, dup := listed[key]; dup {
		return fmt.Errorf("%w: %s lists %q twice",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, key)
	}
	listed[key] = struct{}{}

	if row.Ftype != filesEntryTypeFile {
		if _, isFile := scan.hashes[key]; isFile {
			return fmt.Errorf("%w: %s gives %q the type %q while the archive carries a regular file there",
				helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, key, row.Ftype)
		}
		return nil
	}
	// FILES.json's own file row, if it carries one, is not checked against the
	// archive: a listing cannot state its own digest, and MANIFEST.json
	// already pinned these bytes from inside the signed document. The row for
	// it under any other ftype was answered one branch above, where the archive
	// carrying a regular file under a non-file row is refused - so what this
	// skips is the digest comparison and not the rest of the rule.
	if key == helpers.FilesManifestFileName {
		return nil
	}
	return verifyListedFile(scan, key, row)
}

// expectDelim reads one token and requires it to be want, so a document of the
// wrong shape is refused where it is read rather than by whatever the next step
// makes of it.
//
// The offending token is deliberately not rendered: nothing has been
// length-checked at this point, and a token standing where a delimiter belongs
// can be the whole of a helpers.FilesManifestMaxBytes document.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return listingParseError(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != want {
		return fmt.Errorf("%w: %s does not parse: %q was expected",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, want)
	}
	return nil
}

// skipListingValue consumes the value under a key this reader does not read,
// whatever shape it takes, retaining none of it. Decoding it into a
// json.RawMessage would be shorter and would copy up to a whole document for a
// key nothing ever looks at.
func skipListingValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return listingParseError(err)
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			// A scalar, which is the whole value when nothing has opened.
			if depth == 0 {
				return nil
			}
			continue
		}
		if delim == '{' || delim == '[' {
			depth++
			continue
		}
		depth--
		if depth == 0 {
			return nil
		}
	}
}

// listingParseError renders one decoder failure over FILES.json.
//
// The decoder's own message is safe to wrap: a syntax error renders at most the
// single character that ended the parse - measured on go1.26.6, "invalid
// character 'q' looking for beginning of value", with the offset it also
// carries left out of that text and readable only as a struct field - and a
// type error names the Go field and the JSON KIND it found rather than the
// value, so neither carries a document-sized string past the caps below.
func listingParseError(err error) error {
	return fmt.Errorf("%w: %s does not parse: %w",
		helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, err)
}

// checkListedFieldLengths caps every string one FILES.json row can carry.
//
// One uniform cap rather than a table of per-field ones: all four are names or
// short labels a real listing spells in tens of bytes, and a per-field table
// would be four numbers to justify where one already stands.
func checkListedFieldLengths(row *filesEntry) error {
	if err := checkFieldLength(helpers.FilesManifestFileName, "name", row.Name); err != nil {
		return err
	}
	if err := checkFieldLength(helpers.FilesManifestFileName, "ftype", row.Ftype); err != nil {
		return err
	}
	if err := checkFieldLength(helpers.FilesManifestFileName, "chksum_type", row.ChksumType); err != nil {
		return err
	}
	return checkFieldLength(helpers.FilesManifestFileName, "chksum_sha256", row.ChksumSha256)
}

// checkPointerFieldLengths caps the three strings MANIFEST.json's pointer can
// carry.
//
// The manifest is bounded only by helpers.ManifestScanMaxBytes, orders of
// magnitude above anything this pointer needs, and every one of these fields is
// rendered with %q by a refusal below it. What that costs is measured against
// what a document inside that bound can actually reach rather than against %q
// over a string in isolation, since the field has to arrive as JSON source and
// its escapes are what decide the ratio. Measured on go1.26.6 with one field
// filling a manifest at the ceiling: ordinary bytes render one for one, 64.0
// MiB in and out, while DEL (0x7f) renders as \x7f - four bytes out per source
// byte, and it costs only one, since JSON refuses raw only U+0000 through
// U+001F - for 256.0 MiB out of a 64.0 MiB document. An escaped control
// character is cheaper rather than dearer: the escape \u0001 spends six
// source bytes to render four.
//
// What this bounds is that rendering and never the decode, which has already
// happened by the time it runs. A field of invalid UTF-8 is not refused by the
// decoder but replaced byte by byte with U+FFFD, three bytes for one, so the
// same ceiling admits a 192.0 MiB Go string (measured: 201,326,490 bytes) that
// exists before this check can have an opinion about it. Bounding the decode
// too would mean reading the document some other way than into this struct,
// which is a larger change than the amplification justifies while the manifest
// itself stays capped.
func checkPointerFieldLengths(doc *chainPointer) error {
	if err := checkFieldLength(helpers.ManifestFileName, "name", doc.FileManifestFile.Name); err != nil {
		return err
	}
	if err := checkFieldLength(helpers.ManifestFileName, "chksum_type", doc.FileManifestFile.ChksumType); err != nil {
		return err
	}
	return checkFieldLength(helpers.ManifestFileName, "chksum_sha256", doc.FileManifestFile.ChksumSha256)
}

// checkFieldLength refuses one JSON string field past
// helpers.ArchiveMaxEntryNameLen, reporting its length rather than its value.
//
// The cap is the tar side's rather than one invented here, because these fields
// describe an archive's entries and reach this reader from a document that is
// attacker-chosen the moment the archive is. Reporting the length rather than
// the value is the same exception checkEntryNameLength makes, for the reason
// argued there.
func checkFieldLength(document, field, value string) error {
	if len(value) <= helpers.ArchiveMaxEntryNameLen {
		return nil
	}
	return fmt.Errorf("%w: %s carries a %s field of %d bytes, the limit is %d",
		helpers.ErrManifestChainMismatch, document, field, len(value), helpers.ArchiveMaxEntryNameLen)
}

// listedName normalizes one listed name into the key the scan holds entries
// under, refusing a name that could not describe a file inside the archive.
//
// The refusals raise helpers.ErrManifestChainMismatch rather than the archive
// sentinels the identical shapes raise in cleanEntryPath, and the split is by
// which document is at fault: an escaping name in the tar stream is a bad
// archive, the same name in FILES.json is a bad listing. A "." is left alone
// rather than refused, since it names the collection root - a row a listing may
// legitimately carry, describing no file and escaping nothing.
func listedName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: %s lists an entry with no name",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName)
	}
	cleaned := path.Clean(name)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %s lists %q, which is not a path inside the archive",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, name)
	}
	return cleaned, nil
}

// verifyListedFile checks one listed file row against what the archive carries
// under that name. The row is taken by pointer rather than by value only to
// keep a 64-byte copy off a path that runs once per listed row.
func verifyListedFile(scan *archiveScan, key string, row *filesEntry) error {
	if row.ChksumType != chksumTypeSHA256 {
		return fmt.Errorf("%w: %s gives %q the checksum type %q, and this reader verifies %s alone",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, key, row.ChksumType, chksumTypeSHA256)
	}
	// Message, not verdict, for the same reason the pointer's own shape check
	// is when it is reached with a real archive: a value that is not a digest
	// yields the zero array, and no file hashes to that, so the comparison
	// below refuses either way. Untested as a separate rule for exactly that.
	want, ok := sha256FromHex(row.ChksumSha256)
	if !ok {
		return fmt.Errorf("%w: %s names %q as the digest of %q, which is not a sha256",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, row.ChksumSha256, key)
	}
	got, err := resolveEntryDigest(scan, key)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: %q does not match the digest %s names for it",
			helpers.ErrManifestChainMismatch, key, helpers.FilesManifestFileName)
	}
	return nil
}

// resolveEntryDigest returns the digest of what key names, following link
// entries until it reaches a regular file or runs out of hops.
//
// Following at all, and following more than once, is what the reference reader
// does with the same archive - see chainMaxLinkHops for the measurement and for
// why the bound is where it is.
func resolveEntryDigest(scan *archiveScan, key string) ([32]byte, error) {
	for hop := 0; hop <= chainMaxLinkHops; hop++ {
		if sum, ok := scan.hashes[key]; ok {
			return sum, nil
		}
		// Removing this arm would not change the verdict either: an
		// unresolvable name spins the loop to its bound and is refused there
		// under the same sentinel. It is kept because it names the actual
		// defect, where the hop-bound message would misdescribe it, and no
		// test separates the two.
		target, ok := scan.links[key]
		if !ok {
			return [32]byte{}, fmt.Errorf("%w: %s lists %q, which the archive does not carry",
				helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, key)
		}
		key = target
	}
	return [32]byte{}, fmt.Errorf("%w: %q resolves through more than %d links",
		helpers.ErrManifestChainMismatch, key, chainMaxLinkHops)
}

// checkUnlisted walks the archive backward against the listing: every entry the
// scan retained must appear in FILES.json.
//
// An installer that unpacked only the paths FILES.json names would get this
// direction for free, since an entry nothing lists would never be written
// anywhere. This project's does not work that way: archive.ExtractTarGz writes
// what the tar carries rather than what the listing names. So without this rule
// a signed manifest and a listing that matches it would still admit any number
// of files nobody vouched for, sitting in the installed tree alongside the ones
// that were.
//
// MANIFEST.json and FILES.json are exempt because a listing naming the
// documents that name it is a convention, not a guarantee, and neither is
// unattested: the signature covers the first and the first covers the second.
func checkUnlisted(scan *archiveScan, listed map[string]struct{}) error {
	for _, key := range scan.order {
		if key == helpers.ManifestFileName || key == helpers.FilesManifestFileName {
			continue
		}
		if _, ok := listed[key]; !ok {
			return fmt.Errorf("%w: the archive carries %q, which %s does not list",
				helpers.ErrManifestChainMismatch, key, helpers.FilesManifestFileName)
		}
	}
	return nil
}

// sha256FromHex decodes a declared digest into the array the scan compares
// against, reporting false for anything that is not one.
//
// The alphabet rule is helpers.IsSHA256Hex's alone - length, lowercase, hex -
// so this function adds no second opinion about what a digest may look like and
// only turns an accepted one into bytes. The nibble loop is what keeps that
// free: encoding/hex would take a []byte conversion of the string for every
// listed row in an archive that can carry helpers.ArchiveMaxEntryCount of them.
func sha256FromHex(s string) ([32]byte, bool) {
	var out [sha256.Size]byte
	if !helpers.IsSHA256Hex(s) {
		return out, false
	}
	for i := range out {
		high := hexNibble(s[i*hexDigitsPerByte])
		low := hexNibble(s[i*hexDigitsPerByte+1])
		out[i] = high<<hexHighNibbleShift | low
	}
	return out, true
}

// hexNibble is the value of one hex digit. It is total only for the alphabet
// helpers.IsSHA256Hex accepts, which every caller has already checked.
func hexNibble(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + hexLetterValue
}
