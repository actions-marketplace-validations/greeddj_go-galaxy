package helpers

import (
	"io/fs"
	"net/http"
	"time"
)

const (
	// DirMod is the default permission for created directories.
	DirMod = 0o755
	// FileMod is the default permission for created files.
	FileMod = 0o644
	// WritePermBits is the write-permission mask (owner, group, other)
	// ReadOnlyPerm clears. It exists as a named constant rather than an
	// inline literal because the mask is applied at exactly one call site
	// (archive.extractRegularFile) and must never be reintroduced ad hoc
	// elsewhere: content extracted into the shared extracted-store CAS tree
	// is later hard-linked into every install that references it, so its
	// mode is genuinely shared inode metadata, and clearing the write bits
	// anywhere but at the moment the bytes are first created would itself be
	// an aliasing write into the CAS tree the mask exists to protect.
	WritePermBits = 0o222

	// ExtractMarkerPrefix names the marker file extractCollection writes into
	// an install path once extraction (or CAS materialization) completes,
	// suffixed with the artifact's sha256. Its presence is the fast first
	// signal a later run uses to consider skipping re-extraction; the
	// collections package's verifyExtractMarker layers a tree-tally check on
	// top of that presence check before actually trusting it.
	ExtractMarkerPrefix = ".extract-done."

	// CollectionNameParts is the expected number of parts in a collection name like "namespace.collection".
	CollectionNameParts = 2

	// CacheLatestMetadataTTL is the TTL for cached metadata before revalidation.
	CacheLatestMetadataTTL = 10 * time.Minute

	// ArchiveMaxEntrySize caps the byte size a single archive entry DECLARES in
	// its tar header. Extraction charges that declared size for every entry it
	// reads, before it dispatches on the entry's typeflag, so an entry that is
	// read past and never extracted is charged exactly like one that is
	// written to disk. What this bounds is the declaration, not the reading: a
	// header can name a size far under what archive/tar then consumes for it
	// (see archive.chargeEntrySize for which shapes do that). The bytes are
	// bounded by ArchiveMaxDecompressedSize instead.
	ArchiveMaxEntrySize = int64(512 << 20) // 512 MiB per file
	// ArchiveMaxTotalSize caps the declared header sizes of an archive's
	// entries, summed as extraction reads them. Charged at the same point and
	// with the same meaning as ArchiveMaxEntrySize: it bounds what an archive's
	// headers claim in total, which is a cheap refusal available before a body
	// byte is read, and not what the tar reader is made to consume.
	ArchiveMaxTotalSize = int64(4 << 30) // 4 GiB per archive
	// ArchiveMaxDecompressedSize caps the RAW DECOMPRESSED BYTES one archive
	// may make a reader of it pull out of its gzip reader - every byte of the
	// stream, tar framing and inter-entry padding included, not just the entry
	// bodies. This is the cap that actually bounds a decompression bomb; the
	// declared-size budgets above are an earlier, cheaper refusal that a
	// hostile archive can understate at will.
	//
	// No headroom is granted over ArchiveMaxTotalSize for that framing, even
	// though a legitimate 4 GiB-of-content archive also carries a 512-byte
	// header per entry plus padding. Any such headroom would have to be
	// computed from ArchiveMaxEntryCount, and framing is precisely what that
	// count does not bound: archive/tar consumes 'x', 'L' and 'K' meta headers
	// inside Next() and never returns them, so they are never counted as
	// entries, and a headroom allowance sized for them would be an allowance
	// for the meta-header chain itself. The narrowing this costs a legitimate
	// archive is about 102 MB of framing at ArchiveMaxEntryCount entries, and
	// only for one already at the 4 GiB ceiling - which is far outside any real
	// collection.
	ArchiveMaxDecompressedSize = ArchiveMaxTotalSize
	// ArchiveMaxEntryCount caps how many headers tar.Reader.Next may hand back
	// for a single archive, whatever their typeflag. It is the bound aimed at a
	// tarbomb of zero-byte directories or hardlinks: such an entry costs an
	// archive nothing to declare, so it never trips ArchiveMaxEntrySize or
	// ArchiveMaxTotalSize, and this count is what refuses it. What it bounds is
	// headers, not inodes - archive.ensureDir creates every missing ancestor of
	// an entry's path, so one entry named a/b/c/f costs four - and inodes are
	// bounded only transitively, by ArchiveMaxDecompressedSize, since every
	// path component's name bytes have to leave the decompressor. The value
	// sits well above any real collection (typically a few thousand to ~10-20k
	// files).
	ArchiveMaxEntryCount = int64(100_000)
	// ArchiveMaxEntryNameLen caps the bytes of one tar entry's own name, and of
	// a link entry's target name. It bounds neither disk nor decompressed
	// bytes, which is why none of the byte caps above covers it: it bounds
	// what a reader RETAINS. A pass that has to hold every entry's name until
	// the stream ends - checking an artifact's manifest chain is one, since
	// FILES.json may sit anywhere in the stream - keeps a key per entry, and
	// nothing else here says how large one may be.
	//
	// Measured, because the headroom is not obvious: archive/tar accepts a GNU
	// long name of 1,048,575 bytes and refuses one byte more, so an entry name
	// is a megabyte before this cap exists. 400 such entries make a
	// 419,841,024-byte tar stream that gzips to 436,798 bytes (961.2x) and
	// leaves 400.03 MiB of retained keys behind. Extrapolated to
	// ArchiveMaxDecompressedSize, the only ceiling such a stream ever meets,
	// 4,092 entries fit: 4.00 GiB retained for 4.26 MiB compressed, with
	// ArchiveMaxEntryCount never binding at all. The compressed figures are a
	// property of the Go release's deflate at the time of measurement and drift
	// with it; the stream length and the retained bytes do not.
	//
	// With this cap the composition inverts and ArchiveMaxEntryCount becomes
	// what binds: retention is at most that many names plus their link targets,
	// so the worst case is bounded rather than unbounded. Reading that bound off
	// the two caps alone understates it, and the gap is worth spelling out,
	// since a running counter over retained bytes is the alternative this
	// arithmetic is the argument against. A link's target is retained resolved
	// rather than as declared, and a resolution is a join of the entry's own
	// directory with the declared target, so it runs to 2,047 bytes: a key on
	// the cap has a last component of at least one byte and a separator before
	// it, leaving 1,022 for its directory, joined to a target of 1,024. At
	// ArchiveMaxEntryCount entries of 1,024-byte keys carrying such targets that
	// is roughly 307.1 MB of string bytes, half again the 204.8 MB a key and a
	// target each at the cap would suggest. 1024 is darwin's PATH_MAX, and no
	// real collection path is within an order of magnitude of it.
	//
	// The extractor applies no such cap and needs none, though not because it
	// retains nothing: archive.extractTarEntries memoizes every confirmed
	// parent component for the length of an extraction. What bounds that memo
	// is a mechanism rather than a figure, and deliberately so, since the memo
	// grows with the depth of each path as well as with the number of them and
	// no product of two caps describes it. Every string in it names a directory
	// the kernel accepted and created, because a component is memoized only
	// once os.Lstat has confirmed it a real directory or MkdirAll has made one.
	// So the memo cannot outgrow the directory tree on disk, and an extraction
	// able to fill it has exhausted the filesystem's inodes long before the
	// process's memory - the transitive inode bound archive.extractTarEntries
	// already discloses on its own entry counter, seen from the memory side.
	//
	// This cap is nonetheless the stricter rule wherever PATH_MAX exceeds it,
	// and on the release image's linux base it does: measured, a path assembled
	// from one-byte components survives to 4,094 bytes there and to 1,016 on
	// darwin, while a 256-byte component is refused on both. So it refuses
	// names an extraction would have accepted. Erring in that direction is
	// right, since what this bounds is retention rather than the filesystem,
	// but it is not the same claim as refusing only what would have failed
	// anyway.
	ArchiveMaxEntryNameLen = 1024
	// ArchiveProbeMaxBytes caps how far into an artifact's decompressed tar
	// stream archive.ProbeTarGz may read before it gives up with
	// ErrArtifactTarHeaderNotFound. It bounds the one shape that turns a single
	// tar.Reader.Next call into unbounded work inside archive/tar: archive/tar
	// consumes an 'x' (PAX extended), 'L' (GNU long name) or 'K' (GNU long
	// link) header inside Next() and continues its own loop without ever
	// returning one, so "one Next call" bounds nothing by itself and the chain
	// ahead of the first ordinary header is as long as the archive cares to
	// make it. Work beneath archive/tar is a different question this counter
	// answers nothing about, and it is answered elsewhere rather than left
	// open: a gzip member producing no bytes at all reaches this counter with
	// nothing to count, so internal/gzipstream refuses it at the member
	// (ErrEmptyGzipMember) before this cap could apply.
	//
	// It counts bytes taken OUT of the decompressor, the same side
	// ArchiveMaxDecompressedSize and ManifestScanMaxBytes are enforced on and
	// for the same reason: a limit placed on the compressed body instead is not
	// a limit at all, since one compressed kilobyte of near-identical 512-byte
	// headers yields orders of magnitude more tar stream.
	//
	// That chain's length is the only thing archive/tar leaves unbounded on the
	// way to the first header it returns. Every OTHER read it performs getting
	// there is bounded by archive/tar itself, and by the same 1 MiB it allows
	// one special file: a meta header's body (measured on an 'x' body and on an
	// 'L' body alike - 1,048,576 bytes walk through, 1,048,577 fails with
	// "archive/tar: header field too long") and a sparse map of either kind
	// ("archive/tar: sparse map too long"), with a header block's own 512 bytes
	// far beneath either. That is stated as a property of every such read rather
	// than as a list of the functions performing them deliberately: a reading
	// path this comment does not name is then covered by the sentence instead of
	// missed by the list. Every archive/tar figure and message this comment
	// states was measured on go1.27.0, the toolchain go.mod pins, so a bump to
	// that directive re-opens all of them at once.
	//
	// At most four of those reads can precede that first header - one body for
	// each of the three chainable kinds, plus at most one sparse map, since
	// handleSparseFile takes exactly one of its two branches on the returned
	// header's own typeflag and the two map kinds therefore never sum. Measured:
	// an archive declaring both at once is read to 4,195,840 bytes, the old-GNU
	// figure by itself rather than anything approaching a sum.
	//
	// That count is an input to the floor rather than an observation beside it:
	// the derivation below is four terms, and a fourth chainable meta kind, or
	// a second read taken before the returned header, makes it five. What fails
	// then is the floor rather than the ceiling - the probe refuses an archive
	// archive/tar itself accepts, with ErrArtifactTarHeaderNotFound, which
	// isLateTerminalDownloadError makes terminal for retry, so such a run fails
	// rather than degrades. Raising the cap does not close that, and the
	// arithmetic below is why: a fifth term lands on 5,245,440, the very figure
	// this comment names further down as the redundant fourth meta body this
	// value was chosen to refuse, so clearing the hypothetical means admitting
	// that redundancy. And since a term is 512 bytes more than a power of two,
	// no round cap ever clears an integral number of terms - 6 MiB carries the
	// identical residual one term up, six of them reaching 6,294,528 against
	// its 6,291,456. The residual is scale-invariant, a property of the
	// arrangement rather than of this value. No test closes it either, and none
	// can: a fixture cannot contain a header kind archive/tar has not yet
	// defined. What a future implementer owes this constant instead is a
	// re-derivation whenever the toolchain's archive/tar changes shape.
	//
	// The figure the value has to clear is therefore
	// 3 * (512 + 1 MiB) + 512 + 1 MiB = 4,196,352, and that figure is derived. A
	// meta iteration costs at most 512 + 1 MiB: its header block, its body, and
	// that body's padding, which is read at the top of the next iteration and
	// satisfies B + blockPadding(B) <= 1 MiB for every B <= 1 MiB. The header
	// the walk finally returns costs its own block plus the largest sparse map
	// archive/tar will read for it.
	//
	// Measured, that derivation is reached rather than merely approached, and
	// the witness is a composite of an 'L', a 'K' and an 'x' header each
	// carrying a 1,048,576-byte body, followed by an ordinary header whose data
	// section holds a 2,048-block PAX 1.0 sparse map (GNU.sparse.major=1,
	// GNU.sparse.minor=0): archive/tar accepts it after pulling exactly
	// 4,196,352 bytes, and refuses the same composite with a 2,049-block map.
	// The old-GNU map lands 512 bytes short of that, its extension blocks
	// counting against the same 1 MiB alongside the 'S' header block that
	// carries the map's first four fragments - 2,047 extension blocks accepted,
	// 2,048 refused, putting that composite at 4,195,840. 5 MiB clears the
	// derived ceiling by 1,046,528 bytes, which is not one more term of
	// anything: a meta iteration is 1,049,088, and no round cap could be an
	// integral number of them, a term being 512 bytes more than a power of two.
	//
	// Three kinds rather than four - TypeXGlobalHeader is the other member of
	// the 'x' family, and Next returns it rather than continuing past it, so it
	// ends a walk instead of extending one. Measured, a chain terminated by one
	// carrying a 1 MiB body is accepted at that same 4,196,352, its body
	// standing exactly where the sparse map stood.
	//
	// Three BODIES rather than three headers, and for the chain that is what
	// makes this cap a refusal of redundancy rather than of expressiveness: Next
	// ASSIGNS each kind's value rather than merging it - `paxHdrs, err =
	// parsePAX(tr)` and `gnuLongName = p.parseString(realname)` - so a second
	// 'x' discards the first's records whole and a second 'L' discards the
	// first's name (measured on the witness composite above: a fourth meta
	// header with no body puts it at 4,196,864, and one carrying a 1 MiB body
	// puts it at 5,245,440, past this cap - and neither carries a record or a
	// name the first three did not). The argument stops at the chain and must
	// not be read onto the sparse map, which is information-carrying right up to
	// archive/tar's own ceiling: measured, a maximal old-GNU map describes
	// 42,991 fragments - four in the header block and 21 in each of its 2,047
	// extension blocks - and a violation planted in the last of those slots is
	// refused, so every one of them really is parsed.
	//
	// Real prologues sit three orders of magnitude below all of it. Measured
	// against Python 3.14.6's tarfile at its default PAX format, and attributed
	// to that writer rather than to `ansible-galaxy collection build`, which was
	// not itself measured: an ordinary artifact's first entry carries a
	// 1,024-byte prologue, and one whose name runs to ArchiveMaxEntryNameLen
	// carries 2,048.
	ArchiveProbeMaxBytes = int64(5 << 20) // 5 MiB

	// ArtifactMaxDownloadSize caps the raw (compressed, on-the-wire) bytes
	// read from an artifact download before it is rejected. It bounds the
	// same disk-exhaustion risk that ArchiveMaxTotalSize bounds for the
	// extracted tree, but earlier: it caps what a server-controlled download
	// URL can make the client stream to a temp file, before extraction ever
	// runs (and even when extraction is skipped entirely). It is set equal
	// to ArchiveMaxTotalSize rather than some smaller figure: a legitimate
	// gzip+tar collection artifact is always smaller, usually far smaller,
	// than the extracted tree it decompresses to, so a real collection
	// never gets close to this ceiling, and a raw stream that does is
	// already pathological regardless of what it eventually decompresses
	// to.
	ArtifactMaxDownloadSize = ArchiveMaxTotalSize

	// GitPackMaxSize caps the compressed bytes a git fetch may write to its
	// on-disk object storage (the packfile and its index) before the fetch is
	// refused. It is deliberately far below ArtifactMaxDownloadSize: a
	// collection repository is a source tree, not an artifact archive, and
	// go-git inflates every object into memory while it indexes the pack,
	// before any of this tool's per-blob or per-tree caps can run. The cap
	// therefore bounds the inflation surface a hostile remote gets, as well as
	// the disk it can fill; what it cannot bound is the inflated size of a
	// single object inside that budget, which go-git exposes no knob for.
	GitPackMaxSize = int64(512 << 20) // 512 MiB

	// GitErrorBodyMaxSize caps how much of a non-2xx response body the git
	// HTTP client lets go-git read. go-git reads a failed request's body whole
	// to compose its error, and a pack cap bounds only bytes that reach the
	// object store, so without this a remote answering 401 or 500 with an
	// endless body could grow the process at line speed until the fetch
	// deadline. A real error page is a few kilobytes.
	GitErrorBodyMaxSize = int64(64 << 10) // 64 KiB

	// GitTreeMaxDepth caps how deep a git tree may nest before this tool
	// refuses to read it. A collection never approaches it; a tree that does
	// is a crafted one, and refusing early keeps the walk's recursion bounded
	// by something this tool chose rather than by the remote.
	GitTreeMaxDepth = 64

	// BuildMetadataMaxBytes caps a metadata file a builder reads out of a
	// source tree - a collection's galaxy.yml or MANIFEST.json, a role's
	// meta/main.yml or meta/requirements.yml - before it is decoded. A real
	// one is a few kilobytes; the cap bounds what a hostile tree can make
	// the decoder hold.
	BuildMetadataMaxBytes = 1 << 20

	// RoleVersionsMaxPages caps how many pages of a role's version list the
	// Galaxy v1 API walk follows (page_size 50, so a thousand tags) before
	// the walk is refused rather than silently cut short: a role with more
	// tags than that is not one this tool can choose a version for honestly.
	RoleVersionsMaxPages = 20

	// RoleGraphMaxRoles caps how many roles one run may discover through
	// requirements.yml and the dependencies of what it installs. A real
	// dependency graph is a handful; a chain of repositories each naming
	// the next is the shape this bounds.
	RoleGraphMaxRoles = 1000

	// ArtifactDownloadDeadline bounds the wall-clock time one artifact
	// acquisition (a fetchArtifact/downloadCollectionToCache call) may take
	// from start to finish: the response-header phase, the streamed body, the
	// single-pass extraction running alongside it when an extracted store is
	// configured, the cache commit, and every retry attempt and backoff sleep
	// in between - one shared budget covering the whole acquisition, not one
	// spent per attempt.
	//
	// It exists because the read-inactivity watchdog (see the fetch package)
	// bounds the gap between two consecutive body reads, not the total
	// transfer time: a byte-drip that keeps making genuine (if glacial)
	// progress never leaves the watchdog's idle window and so never trips it,
	// no matter how long the transfer runs. ArtifactMaxDownloadSize bounds
	// disk, not time, so it does not catch this either - a drip can stay well
	// under the byte ceiling forever. This deadline is the only thing that
	// bounds wall-clock time for a slow-but-technically-progressing transfer.
	//
	// It is deliberately NOT --timeout and never derived from it. --timeout
	// (config.Config.Timeout, wired into fetch.New) is a no-progress budget:
	// ResponseHeaderTimeout plus the watchdog's idle window, redefined that
	// way precisely so a large, healthy, but slow transfer is never bounded by
	// it. This constant is the opposite kind of budget - it fires even while
	// progress is being made - so conflating the two would either make
	// --timeout falsely fail a healthy multi-minute download or make this
	// deadline falsely tolerate an indefinite drip.
	//
	// The value, 15 minutes, is sized against ArtifactMaxDownloadSize (4 GiB):
	// a maximum-size artifact must sustain 4 GiB / 900 s = 4,772,186 B/s
	// (4.55 MiB/s, about 38.2 Mbit/s) to finish inside the budget - trivial
	// for any real network. A deliberately generous 100 MiB artifact needs
	// only 116,508 B/s (114 KiB/s, about 0.93 Mbit/s). Real Galaxy collections
	// are single-digit megabytes, so the headroom above is roughly three
	// orders of magnitude.
	//
	// This budget bounds one acquisition - not a collection, and not a run.
	//
	// A collection spends it twice on every ordinary path involving a hostile
	// origin: the prefetcher acquires the artifact ahead of time under its own
	// budget, and - since a prefetch failure is deliberately non-fatal, with
	// runInstallLevel logging it and proceeding - the install worker acquires
	// it again under a fresh budget. That doubling is why this is 15 minutes
	// rather than 30: the owner-intended per-collection ceiling is 30 minutes,
	// spent across those two acquisitions.
	//
	// One corner spends it three times, for 45 minutes: the prefetcher spends
	// the first, the install worker's cache-hit artifacts.Fetch spends the
	// second and ends in ErrSHA256Mismatch from the S3 backend's read-time
	// integrity check, and prepareWithRecovery's evict-and-refetch then spends
	// a third against the origin. Reaching it needs a hostile origin AND a
	// bucket writer AND the artifact unknown or absent at prefetch-scan time
	// (a failing Has probe fail-opens to "schedule a prefetch") but present at
	// install time - a conjunction of capabilities the S3 trust model already
	// names, not a new one. It is recorded rather than defended against: a
	// hard per-collection ceiling would take a budget threaded through the
	// prefetcher and prepareWithRecovery, which is a different mechanism from
	// this one.
	//
	// Nothing here bounds the run. One install level of N collections over
	// cfg.Workers workers can hold a runner for roughly ceil(N/Workers) times
	// the per-collection ceiling before installLevels breaks on that level's
	// failure count. That is inherent to a per-artifact budget; bounding a
	// whole run means a root-context deadline, which is a separate decision.
	//
	// It is not configurable, for the same reason ArtifactMaxDownloadSize and
	// MetadataMaxSize are not: a knob for a safety ceiling is a knob an
	// operator raises in direct response to a truncation, which is exactly
	// how the attack this ceiling defends against succeeds. Infra's
	// ArtifactDownloadDeadline field exists solely so a test can shrink this
	// value; it must never be wired to a CLI flag, an environment variable, or
	// an ansible.cfg key.
	ArtifactDownloadDeadline = 15 * time.Minute

	// StateObjectMaxCompressedSize caps the raw (on-the-wire) bytes read for a
	// persisted cache-state object (the S3 snapshot and the project registry).
	// A real state object is far smaller; anything past this is pathological, so
	// this bounds what a malicious or corrupt object can buffer into memory
	// before it is rejected, mirroring ArtifactMaxDownloadSize for downloads.
	StateObjectMaxCompressedSize = int64(256 << 20) // 256 MiB
	// StateObjectMaxDecompressedSize caps the inflated size of a gzip-encoded
	// cache-state object, so a high-ratio gzip bomb that stays under the
	// compressed ceiling cannot expand without bound.
	StateObjectMaxDecompressedSize = int64(1 << 30) // 1 GiB

	// StateObjectDeadline bounds one persisted cache-state operation on the
	// Backend seam - LoadStore, SaveStore, LoadProjectRegistry, or
	// RecordProject - end to end: the lazy Open an S3 backend performs inside
	// them, the object GET or PUT, readAllCapped's inflate, MarshalSnapshot's
	// copy+encode and the gzip on the save path, and every S3-verb retry
	// inside s3RetryPolicy, all as one shared budget.
	//
	// Its radius is larger than a single request for a reason specific to
	// this seam: LoadStore and LoadProjectRegistry both run AFTER
	// initInstall/initCleanup take the exclusive distributed lock, and the
	// lock's own heartbeat keeps renewing its TTL in the background, so a
	// stalled read here is not "one run hangs" - it is "every runner sharing
	// the bucket is locked out", each of them eventually failing only once
	// its own lockWaitCeiling wait expires. SaveStore carries the identical
	// property, since it also runs before the lock is released.
	//
	// The realistic threat this bounds is as much a degraded endpoint as a
	// hostile one: unlike the bucket-write trust boundary documented
	// elsewhere (a writer who can plant a poisoned snapshot), a byte-drip on
	// a state-object read needs only control of - or a fault on - the
	// network path or the object-store endpoint, not of the object's
	// content.
	//
	// The value, 1 minute, is sized against StateObjectMaxCompressedSize
	// (256 MiB): a maximum-size compressed object must sustain
	// 256 MiB / 60 s = 4,473,924 B/s (4.27 MiB/s, about 35.8 Mbit/s) to
	// finish inside the budget - deliberately the same class of link
	// ArtifactDownloadDeadline already demands of a maximum-size artifact
	// (4 GiB / 900 s = 4,772,186 B/s, 38.2 Mbit/s), so this ceiling is never
	// the first a slow-but-legitimate link trips. That is also why it is 60
	// seconds and not 30: at 30 seconds the same object would demand
	// 8,947,849 B/s (71.6 Mbit/s), a stricter link than the artifact
	// ceiling asks for. A full 1 GiB inflate (StateObjectMaxDecompressedSize)
	// inside 60 seconds needs 17.1 MiB/s, an order of magnitude below what
	// pgzip decompresses at, so decompression is never the binding
	// constraint. A large real snapshot (16 MiB compressed) needs only
	// 279,620 B/s (273 KiB/s, 2.2 Mbit/s) to finish inside the budget, and a
	// typical one (512 KiB) needs only 8,738 B/s (8.5 KiB/s, 0.07 Mbit/s).
	//
	// The largest legitimate operation this bounds is SaveStore, not
	// LoadStore: SaveStore's own MarshalSnapshot copy+encode and gzip run
	// inside the same budget as its PUT, while LoadStore only inflates and
	// decodes what SaveStore already produced.
	//
	// Disclosed, not hidden: the arithmetic above does not mean a
	// maximum-size snapshot fits comfortably at the minimum reference link.
	// At StateObjectMaxCompressedSize (256 MiB), the 4.27 MiB/s reference
	// link this budget is sized against consumes the entire 60 seconds on
	// the PUT alone, leaving nothing for MarshalSnapshot's deep copy, its
	// JSON encode, and the gzip - all of which run inside this same budget
	// on the save path. On any real CI link (100 Mbit/s or better) that CPU
	// work finishes in a few seconds and the upload itself takes roughly 20
	// seconds, so a maximum-size snapshot fits comfortably; on the minimum
	// reference link it does not. This asymmetry is inherent to putting CPU
	// work and a network transfer in one shared budget, and is the accepted
	// trade: a maximum-size snapshot is already a pathological outlier (see
	// StateObjectMaxCompressedSize's own doc comment), and the realistic
	// sizes above it - a large real snapshot at 279,620 B/s, a typical one at
	// 8,738 B/s - carry no such tension between CPU time and transfer time.
	// If the budget does expire on a save this size at the minimum link,
	// SaveStore returns helpers.ErrStateObjectDeadline and the run fails
	// closed with nothing torn: the install's on-disk result is already
	// complete and untouched, the S3 PUT either fully replaces the snapshot
	// object or leaves it exactly as it was (an interrupted PUT never
	// partially overwrites the previous object), and the backend lock is
	// still released normally - release runs on its own context.Background()
	// with the lock's own releaseTimeout, independent of this budget having
	// already expired. The cost of hitting this ceiling is one cold cache
	// rebuild on the next run, not corruption or a stuck lock.
	//
	// The invariant this budget must respect - pinned by a test in
	// internal/cache/s3 - is StateObjectDeadline (60s) < the S3 backend's
	// heartbeatInterval (3 min) < its lockTTL (10 min), and
	// 3*StateObjectDeadline (3 min) < its lockWaitCeiling (5 min), where 3 is
	// the number of state operations one run performs while holding the
	// lock. A stalled state read was never the only way to hold the lock for
	// a long time - a legitimate, large install already can - so this
	// converts an unbounded hold into a bounded one; it does not make a long
	// hold short.
	//
	// It is not configurable, for the same reason MetadataFetchDeadline and
	// ArtifactDownloadDeadline are not. Infra's StateObjectDeadline field
	// exists solely so a test can shrink this value; it must never be wired
	// to a CLI flag, an environment variable, or an ansible.cfg key.
	StateObjectDeadline = 1 * time.Minute

	// MetadataMaxSize caps the raw bytes read for a single Galaxy API
	// metadata response before it is rejected. Realistic Galaxy metadata JSON
	// is KB to low MB - a root document is ~1-2 KB, a versions page is
	// ~15-20 KB, a version-detail document is well under 1 MB, and even a
	// 10,000-version list is only ~1.5 MB - so 16 MiB bounds the per-fetch
	// io.ReadAll allocation and rejects a hostile giant or endless body
	// without ever false-positiving on a legitimate response.
	MetadataMaxSize = int64(16 << 20) // 16 MiB

	// MetadataFetchDeadline bounds one Galaxy metadata request end to end -
	// the response-header phase, the size-limited io.ReadAll of the body, and
	// every retry attempt and backoff sleep inside fetchJSONBody's
	// helpers.Retry loop - as one shared budget, not one spent per attempt.
	//
	// It exists for the same reason ArtifactDownloadDeadline does: the
	// read-inactivity watchdog bounds the gap between two reads, not total
	// transfer time, so a byte-drip that keeps making genuine progress never
	// trips it, and MetadataMaxSize bounds memory, not time, so it does not
	// catch a slow-but-technically-progressing drip either. Metadata fetches
	// are also the FIRST network contact this program makes in every command,
	// including every --dry-run, running on the resolver ahead of any
	// artifact acquisition - so leaving only the artifact deadline in place
	// would still leave the cheaper, earlier door open to the identical
	// attack.
	//
	// It is deliberately NOT --timeout and never derived from it, for the
	// identical reason ArtifactDownloadDeadline is not: --timeout is a
	// no-progress budget (ResponseHeaderTimeout plus the watchdog's idle
	// window), while this is a whole-transfer budget that fires even while
	// progress is being made.
	//
	// The value, 2 minutes, is sized against MetadataMaxSize (16 MiB): a
	// maximum-size response must sustain 16 MiB / 120 s = 139,810 B/s
	// (136.5 KiB/s, about 1.12 Mbit/s) to finish inside the budget. The
	// largest realistic response - a 10,000-version list, ~1.5 MB - needs
	// only 12,500 B/s (12.2 KiB/s, about 0.1 Mbit/s), and a root document
	// (1-2 KB) needs on the order of 17 B/s. Both are trivial for any real
	// network; the headroom above is deliberate.
	//
	// It is 2 minutes rather than 5 or 15: a single resolve issues many
	// metadata requests (root metadata, version detail, and every page of a
	// versions list), each spending its own copy of this budget, so a large
	// per-request value multiplies across a run instead of bounding it. 15
	// minutes would be formally protective and practically toothless against
	// a 1.5 MB response - a link that slow is already pathological long
	// before the ceiling would ever fire. This ceiling bounds one request,
	// never a resolve's total: a resolve's request count is itself
	// server-chosen, so the total time a resolve can hold the backend lock is
	// not bounded by this constant - see loadVersionsListCached's doc comment
	// (internal/galaxy/collections/resolve.go) for why that residual is
	// accepted rather than capped.
	//
	// The versions-list paging loop (loadVersionsListCached) is the one
	// exception to "one request, one budget": it establishes a single shared
	// budget for the whole page loop rather than one per page, since the
	// server itself controls how many pages a resolve issues (it declares
	// meta.count) - see that function's own doc comment for why a per-request
	// budget alone is not enough there.
	//
	// It is not configurable, for the same reason ArtifactDownloadDeadline is
	// not: a knob for a safety ceiling is a knob an operator raises in direct
	// response to a truncation, which is exactly how the attack this ceiling
	// defends against succeeds. Infra's MetadataFetchDeadline field exists
	// solely so a test can shrink this value; it must never be wired to a CLI
	// flag, an environment variable, or an ansible.cfg key.
	MetadataFetchDeadline = 2 * time.Minute

	// SignatureFetchDeadline bounds one collection's signature phase end to
	// end: every signature source gathered for that collection - a server's own
	// signatures carried in version metadata and each configured source alike -
	// together with each size-limited body read, and any retry attempt and
	// backoff sleep a gatherer makes between them, as one shared budget rather
	// than one spent per source.
	//
	// It exists for the same reason MetadataFetchDeadline and
	// ArtifactDownloadDeadline do: the read-inactivity watchdog bounds the gap
	// between two consecutive reads, not total transfer time, so a byte-drip
	// that keeps making genuine progress never leaves its idle window, and
	// SignatureMaxSize bounds memory rather than time. What is specific to this
	// surface is the multiplication - up to MaxSignaturesPerCollection blobs
	// may be gathered for one collection, so a per-source budget would multiply
	// by that count instead of bounding the phase, which is why the budget is
	// the phase's and not a request's.
	//
	// Throughput is not what sizes this one, and that is the difference from
	// its three siblings: each of those is sized for one transfer's throughput,
	// the metadata one's shared page-loop budget included, since a paging
	// resolve is still one server answering one question. This budget instead
	// covers up to MaxSignaturesPerCollection independent requests, each to a
	// location a requirements file or a server's metadata chose. The bytes
	// are the easy term - 64 blobs of SignatureMaxSize (1 MiB) is 64 MiB worst
	// case, needing 64 MiB / 60 s = 1,118,481 B/s (1.07 MiB/s, about
	// 8.9 Mbit/s), a slower link than StateObjectDeadline (4.27 MiB/s) or
	// ArtifactDownloadDeadline (4.55 MiB/s) already demands, and a realistic
	// collection carries one or two blobs of a few KiB, needing 136 B/s. The
	// binding term is per-source latency instead, and what one source costs
	// depends on which disjunct below its gatherer satisfies. Under the
	// single-attempt one - the disjunct Fetcher.FetchRequirementSource, the only
	// per-source fetch that exists today, satisfies by retrying nothing - a
	// source that black-holes costs FetchDialContextTimeout and one that accepts
	// a connection and never answers costs the operator's own --timeout
	// (ResponseHeaderTimeout). A gatherer that retries multiplies whichever of
	// the two it hit by its attempt count plus backoff. Already at one attempt,
	// a raised --timeout lets a single source consume this whole budget.
	//
	// That is a constraint on the producer, not an argument for a larger value:
	// a phase budget alone lets the first source in a list starve every source
	// after it, and list order belongs to whoever supplied the list - which,
	// for a server's own signatures, is a documented trust boundary of this
	// project. Whatever gathers these sources must therefore bound each one
	// (its own sub-budget, or a single attempt once the source count is
	// non-trivial) rather than relying on this ceiling to do it, or the
	// collection that would have verified never gets its turn.
	//
	// It is not configurable, for the same reason the three deadlines above are
	// not: a knob for a safety ceiling is a knob an operator raises in direct
	// response to a truncation, which is exactly how the attack this ceiling
	// defends against succeeds. Infra's SignatureFetchDeadline field exists
	// solely so a test can shrink this value, beside the other deadlines' own
	// test-only fields; it must never be wired to a CLI flag, an environment
	// variable, or an ansible.cfg key.
	SignatureFetchDeadline = 1 * time.Minute

	// SignatureMaxSize caps the raw bytes read for a single signature blob
	// before it is rejected. A detached OpenPGP signature is a few hundred
	// bytes to a few KiB, armored or not, so 1 MiB leaves three orders of
	// magnitude of headroom while still bounding what one hostile or broken
	// source can buffer into memory - the role MetadataMaxSize plays for a
	// metadata document.
	//
	// It bounds one blob, never the set: MaxSignaturesPerCollection blobs of
	// this size is 64 MiB for a single collection, and installs run
	// cfg.Workers collections at once, so a verifier that holds every blob of
	// a collection at once multiplies both. Verify and release one at a time.
	SignatureMaxSize = int64(1 << 20) // 1 MiB

	// MaxSignaturesPerCollection caps how many signature blobs may be gathered
	// and checked for one collection, counting a server's own and every
	// configured source together.
	//
	// The cap exists because the list is not this program's own. A persisted
	// snapshot is a documented trust boundary here - a principal who can write
	// the cache influences what a run installs - and a server-supplied
	// signature list arrives across it, so without a ceiling that boundary
	// hands the verifier an unbounded number of blobs to fetch and verify: a
	// denial of service costing the writer one edit and the run an unbounded
	// number of round trips and public-key operations. The value sits far above
	// any real collection, which carries one or two.
	MaxSignaturesPerCollection = 64

	// DefaultRequiredValidSignatureCount is the required-valid-signature-count
	// spec a run uses when no source supplied one. It is both what the flag
	// advertises as its default and what the config layer falls back to, which
	// are two different needs rather than one: a command that never registers
	// the flag reads the Go zero value of an unknown flag name, so the flag's
	// own Value is unreachable and an empty spec would otherwise reach
	// signature.ParseCountSpec, which refuses it. The value is ansible's own
	// default.
	DefaultRequiredValidSignatureCount = "1"

	// ManifestFileName is the archive-relative name of a collection's manifest,
	// the one document a collection signature is made over.
	ManifestFileName = "MANIFEST.json"
	// FilesManifestFileName is the archive-relative name of the per-file digest
	// list MANIFEST.json points at. It is the second link of the chain a
	// signature covers: the signature authenticates MANIFEST.json, which names
	// this file's digest, which names every other file's.
	FilesManifestFileName = "FILES.json"
	// FilesManifestMaxBytes caps the size the FILES.json entry's tar header may
	// declare. Every other entry a chain check reads is hashed as it streams
	// past and then dropped; this one is buffered whole and JSON-decoded, so
	// its declared size is an allocation rather than a read.
	//
	// That allocation is the one job this cap has, and it is a job nothing else
	// can do: it bounds the make([]byte, size) in
	// manifest.recordFilesManifest, where the row cap
	// manifest.walkListingRows applies only begins to act once decoding has
	// started. ArchiveMaxEntrySize (512 MiB) is otherwise the only thing
	// bounding it, three orders of magnitude above anything a listing
	// legitimately needs.
	//
	// 32 MiB is a policy choice rather than headroom over a maximum, and the
	// distinction matters because no such headroom exists: ArchiveMaxEntryCount
	// rows at ArchiveMaxEntryNameLen-byte names render to 115.7 MB, 3.4x this
	// cap, so a listing of maximum-length names is refused here rather than
	// accommodated. The cap sits well above any real listing - a collection of
	// a few thousand files spells one in a few hundred kilobytes, and even
	// ArchiveMaxEntryCount rows at the 100-byte names such a collection
	// actually carries render to roughly 23 MB - and deliberately below what
	// the accepted maximum could reach. What bounds the row count is the row
	// cap named above, never this one.
	FilesManifestMaxBytes = int64(32 << 20) // 32 MiB
	// ManifestScanMaxBytes caps how far into an artifact's decompressed tar
	// stream a scan for ManifestFileName may read before giving up with
	// ErrManifestNotFound. A real collection carries its manifest at the front
	// of the archive, so this bounds the work a hostile archive can extract
	// from a scan that will find nothing. It is deliberately far below
	// ArchiveMaxDecompressedSize, which bounds a full extraction rather than a
	// look at an archive's head.
	//
	// It counts bytes taken OUT of the decompressor, the same side
	// ArchiveMaxDecompressedSize is enforced on and for the same reason: a
	// limit placed on the compressed body instead is not a limit at all, since
	// 64 MiB of gzip input can yield orders of magnitude more tar stream. A
	// scan bounded here is transitively bounded in entries too - a tar header
	// is 512 bytes, so 64 MiB admits at most 131,072 of them - which is why no
	// separate entry-count ceiling is needed for the scan.
	ManifestScanMaxBytes = int64(64 << 20) // 64 MiB

	// S3ListMaxSize caps the raw bytes read for a single ListObjectsV2 XML
	// page response. A page is bounded by max-keys (1000 by default, which
	// renders to roughly 1 MB of XML), and pagination reads a large bucket
	// page by page, each a separately-bounded read, so a single list read
	// never scales with total bucket size. 16 MiB gives roughly 16x headroom
	// over a standard page while still bounding per-page memory and
	// rejecting a hostile or broken oversized single-page response (or a
	// gzip bomb, if the transport happens to be decompressing transparently).
	S3ListMaxSize = int64(16 << 20) // 16 MiB

	// ArtifactSHASidecarSuffix names the sidecar file written next to a
	// locally cached artifact tarball, holding its sha256 digest. A later
	// non-pinned cache hit reads this sidecar instead of re-hashing the whole
	// tarball; a frozen (pinned) cache hit never trusts it and always hashes
	// the real bytes.
	ArtifactSHASidecarSuffix = ".sha256"

	// ArtifactDownloadTempPrefix is the prefix the local artifact store uses
	// for in-flight download temp files (see local.Artifacts.TempFile). A
	// file under this prefix is removed by its own download's cleanup on
	// success or failure; one that survives past that can only be a
	// dead-run orphan, so it is the exact string matched by both the
	// dead-run sweep and the --clear-cache sweep.
	ArtifactDownloadTempPrefix = ".download-"

	// FetchDefaultTimeout is the value --timeout takes when neither the flag
	// nor its env sources supply one. It bounds time to first byte
	// (Transport.ResponseHeaderTimeout) and the body watchdog's per-read
	// inactivity window, making it a no-progress budget rather than a cap on
	// a whole request or a whole transfer: the live HTTP client carries no
	// such cap, which is what lets a large artifact keep streaming for as
	// long as it keeps making progress.
	FetchDefaultTimeout = 30 * time.Second
	// FetchDialContextTimeout is the dial timeout for outbound connections.
	FetchDialContextTimeout = 10 * time.Second
	// FetchDialContextKeepAlive is the TCP keep-alive for dials.
	FetchDialContextKeepAlive = 30 * time.Second
	// FetchForceAttemptHTTP2 enables HTTP/2 attempts when possible.
	FetchForceAttemptHTTP2 = true
	// FetchMaxIdleConns is the maximum number of idle connections.
	FetchMaxIdleConns = 256
	// FetchMaxIdleConnsPerHost limits idle connections per host.
	// Single Galaxy server is the common case, so keep it generous to match
	// worker concurrency and avoid TCP churn under high parallelism.
	FetchMaxIdleConnsPerHost = 64
	// FetchIdleConnTimeout is the idle connection timeout.
	FetchIdleConnTimeout = 30 * time.Second
	// FetchTLSHandshakeTimeout is the TLS handshake timeout.
	FetchTLSHandshakeTimeout = 3 * time.Second
	// FetchExpectContinueTimeout is the expect-continue timeout.
	FetchExpectContinueTimeout = 1 * time.Second
	// FetchMaxRedirects bounds how many hops one request may be redirected
	// through before the client refuses to follow another.
	//
	// The number is net/http's own default, and matching it is the point: a
	// client this program builds is redirected exactly as far as one carrying
	// no CheckRedirect of its own, so imposing the ceiling by hand changes
	// nothing about how far a hostile server can bounce a request. See
	// fetch.checkRedirect for why it has to be imposed by hand there at all.
	FetchMaxRedirects = 10

	// FetchRetryMaxAttempts bounds how many times a Galaxy API GET or an
	// artifact download is attempted before its last failure is returned as
	// final.
	FetchRetryMaxAttempts = 4
	// FetchRetryBackoffBase bounds the initial full-jitter backoff between
	// retried attempts of a Galaxy API GET or artifact download.
	FetchRetryBackoffBase = 200 * time.Millisecond
	// FetchRetryBackoffCap bounds the full-jitter backoff ceiling between
	// retried attempts of a Galaxy API GET or artifact download.
	FetchRetryBackoffCap = 5 * time.Second

	// MinDefaultInstallWorkers floors DefaultInstallWorkers' result, and
	// floors MaxAcceptedInstallWorkers' ceiling with it so that the two can
	// never disagree on a single permitted CPU. See DefaultInstallWorkers' doc
	// comment for why the floor buys latency hiding rather than throughput,
	// and for what it costs on a single permitted CPU; see
	// MaxAcceptedInstallWorkers' for why the ceiling takes the same floor.
	MinDefaultInstallWorkers = 2
	// MaxDefaultInstallWorkers caps DefaultInstallWorkers' result. See that
	// function's doc comment for the memory budget the number is derived
	// from and for the measured gain it must not remove.
	MaxDefaultInstallWorkers = 16

	// DownloadWorkersPerCPU is the per-CPU multiplier DefaultDownloadWorkers
	// applies before clamping. An artifact download or cache presence probe
	// mostly waits on the network rather than on the CPU - a HEAD probe or a
	// streamed GET into a temp file - and what local work a download worker
	// does do is a sha256 over every byte it writes to that temp file, with a
	// shape probe on top of it (archive.ProbeTarGz, whose decompressor is
	// sized for a probe rather than for an unpack), so a useful pool size is a
	// multiple of the permitted CPU count rather than that count itself, unlike
	// cfg.Workers, whose pool extracts a tree per worker and is capped by what
	// those concurrent extractions cost in memory (see DefaultInstallWorkers).
	DownloadWorkersPerCPU = 4
	// MinDefaultDownloadWorkers floors DefaultDownloadWorkers' result so a
	// single- or dual-core CI runner still gets meaningful download
	// concurrency instead of one artifact acquisition at a time.
	MinDefaultDownloadWorkers = 8
	// MaxDefaultDownloadWorkers caps DefaultDownloadWorkers' result. The
	// number is sized against FetchMaxIdleConnsPerHost (64): the default pool
	// never exceeds half that per-host idle-connection budget, so the
	// connection pool holds every worker's connection idle between requests
	// with a factor of two to spare rather than churning new ones.
	//
	// This pool's own decompressor claim is bounded separately and is not what
	// sizes it: a worker here opens one probe-sized reader per download
	// (archive.ProbeTarGz), so at the default ceilings this pool reserves
	// 32 * 64 KiB = 2 MiB of gzip block pool today against the 64 MiB
	// MaxDefaultInstallWorkers is derived from. At the default ceilings and
	// not in general: --download-workers is settable above this number and
	// config.BuildCollectionConfig clamps only below 1, so what this constant
	// bounds is the default rather than the pool. That is what lets this
	// ceiling be network-derived and that one memory-derived without the two
	// summing into a number nobody wrote down; see DefaultInstallWorkers for
	// the sum they do reach.
	MaxDefaultDownloadWorkers = 32

	// StoreSnapshotSchemaVersion is the current snapshot schema version.
	//
	// Bumped to 6 when the snapshot gained a warmed set (StoreBucketWarmed):
	// the collection keys `warm` materialized into the content-addressable
	// extracted store, which cleanup must keep even though warm never writes
	// an installed entry (see Store.SetWarmed / Store.WarmedArtifactSHAByKey).
	// There is no field-level migration to write: a schema-5 snapshot simply
	// has no warmed bucket, and the existing drop-and-rebuild policy already
	// produces exactly the same end state any migration could - an empty
	// warmed set that the next warm refills. The bump is deliberate rather
	// than optional even though the change is purely additive: without it, an
	// older binary sharing an S3 cache would silently drop the `warmed` key on
	// its next SaveStore and re-expose the bug this schema version fixes, and
	// a loud ErrUnsupportedSchemaVersion on that old binary is the intended
	// failure instead.
	//
	// Bumped to 7 when the snapshot gained the git pin bucket
	// (StoreBucketGitPins): what commit a (url, ref, subdir) git requirement
	// resolved to and which collections that commit carried, which is what
	// lets a rerun replay a git source without touching the remote. The same
	// reasoning as the bump to 6 applies: the change is additive, the
	// drop-and-rebuild policy yields the only end state a migration could
	// (an empty pin set the next resolve refills), and an older binary sharing
	// an S3 cache would otherwise drop the bucket on its next save.
	//
	// Bumped to 8 when the snapshot gained the role buckets
	// (StoreBucketInstalledRoles, StoreBucketRolePins): which roles are
	// installed where and from which artifact, which is what cleanup keeps
	// their extracted trees by, and what commit each role requirement resolved
	// to, which is what lets a rerun replay a role without touching the remote
	// or the Galaxy API. The change is additive and the drop-and-rebuild
	// policy again yields the only end state a migration could (empty role
	// buckets the next install and resolve refill), but the bump is not
	// optional: an older binary meeting a schema-8 snapshot on a shared cache
	// must fail loudly with ErrUnsupportedSchemaVersion rather than drop both
	// buckets on its next save and hand the next cleanup a snapshot that
	// records no installed role at all.
	//
	// Bumped to 9 when the snapshot gained the url pin bucket
	// (StoreBucketURLPins) and the role pin entry grew its url-source fields
	// (store.RolePinEntry.URL and .SHA256): what sha256 and identity a url
	// requirement's tarball resolved to, which is what lets a rerun replay a
	// url source without touching the origin. The change is additive and the
	// drop-and-rebuild policy yields the only end state a migration could
	// (an empty pin set the next resolve refills), but the bump is again not
	// optional: an older binary sharing an S3 cache would otherwise drop the
	// bucket - and every url role pin's identifying fields - on its next
	// save.
	StoreSnapshotSchemaVersion = 9

	// CacheEntryMaxAge is the retention window for persisted cache entries
	// (API responses, resolved versions lists, and dependency constraints).
	// At persist time, an entry last written longer ago than this - for API
	// entries, last written or last revalidated - is pruned from the snapshot,
	// bounding both the local Bolt file and the S3 object in size and age.
	CacheEntryMaxAge = 30 * 24 * time.Hour

	// WarmedEntryMaxAge is the retention window for a warmed entry (see
	// Store.SetWarmed). It is a separate constant from CacheEntryMaxAge, not a
	// reuse of it: CacheEntryMaxAge bounds cached metadata in the snapshot,
	// while this one decides how long real on-disk content - an extracted
	// tree, potentially hundreds of MB - stays protected from cleanup after
	// its last warm. This is the entire reachability rule for a warmed entry:
	// it has no project and no install path behind it, so the only evidence
	// that it is still wanted is that a warm run re-recorded it within this
	// window.
	WarmedEntryMaxAge = 30 * 24 * time.Hour

	// StoreDBLock is the cache lock file name.
	StoreDBLock = ".go-galaxy.lock"

	// BoltOpenTimeout bounds how long opening a Bolt file waits for its
	// flock. It is long enough to tolerate brief filesystem latency or a
	// concurrent process that is just about to release the lock, but short
	// enough to fail fast in CI instead of hanging indefinitely.
	BoltOpenTimeout = 5 * time.Second

	// StoreDBProjects is the project registry filename.
	StoreDBProjects = "projects.json"

	// StoreDBLocal is the local cache database filename.
	StoreDBLocal = "go-galaxy.db"

	// StoreSnapshotMeta is the snapshot DB filename for metadata.
	StoreSnapshotMeta = "go-galaxy-meta.db"
	// StoreSnapshotAPICache is the snapshot DB filename for API cache entries.
	StoreSnapshotAPICache = "go-galaxy-api-cache.db"
	// StoreSnapshotDepsCache is the snapshot DB filename for dependency cache.
	StoreSnapshotDepsCache = "go-galaxy-deps-cache.db"
	// StoreSnapshotInstalled is the snapshot DB filename for installed collections.
	StoreSnapshotInstalled = "go-galaxy-installed.db"
	// StoreSnapshotGraph is the snapshot DB filename for dependency graph.
	StoreSnapshotGraph = "go-galaxy-graph.db"
	// StoreSnapshotRequirements is the snapshot DB filename for requirements.
	StoreSnapshotRequirements = "go-galaxy-requirements.db"
	// StoreSnapshotRoots is the snapshot DB filename for root collections.
	StoreSnapshotRoots = "go-galaxy-roots.db"
	// StoreSnapshotResolved is the snapshot DB filename for resolved collections.
	StoreSnapshotResolved = "go-galaxy-resolved.db"
	// StoreSnapshotVersions is the snapshot DB filename for versions cache.
	StoreSnapshotVersions = "go-galaxy-versions.db"

	// StoreBucketMeta is the bucket name for snapshot metadata.
	StoreBucketMeta = "meta"
	// StoreBucketAPICache is the bucket name for API cache entries.
	StoreBucketAPICache = "api_cache"
	// StoreBucketDepsCache is the bucket name for dependency cache.
	StoreBucketDepsCache = "deps_cache"
	// StoreBucketInstalled is the bucket name for installed collections.
	StoreBucketInstalled = "installed"
	// StoreBucketGraph is the bucket name for dependency graph.
	StoreBucketGraph = "graph"
	// StoreBucketRequirements is the bucket name for requirements.
	StoreBucketRequirements = "requirements"
	// StoreBucketResolved is the bucket name for resolved collections.
	StoreBucketResolved = "resolved"
	// StoreBucketVersions is the bucket name for versions cache.
	StoreBucketVersions = "versions_cache"
	// StoreBucketWarmed is the bucket name for warmed extracted entries.
	StoreBucketWarmed = "warmed"
	// StoreBucketGitPins is the bucket name for git source pins: the commit a
	// (url, ref, subdir) requirement resolved to and the collections it held.
	StoreBucketGitPins = "git_pins"
	// StoreBucketInstalledRoles is the bucket name for installed roles, keyed
	// by install name: where each was materialized and from which artifact.
	StoreBucketInstalledRoles = "installed_roles"
	// StoreBucketRolePins is the bucket name for role pins: the repository,
	// commit and version a role requirement line resolved to.
	StoreBucketRolePins = "role_pins"
	// StoreBucketURLPins is the bucket name for url source pins: the sha256
	// and collection identity a url requirement's tarball resolved to.
	StoreBucketURLPins = "url_pins"

	// StoreMetaSchemaVersion is the metadata key for the snapshot schema version.
	StoreMetaSchemaVersion = "schema_version"
	// StoreMetaLastSnapshot is the metadata key for the last snapshot time.
	StoreMetaLastSnapshot = "last_snapshot"
	// StoreMetaContentRecorded is the metadata key for the last time a save
	// carried records of on-disk content - see store.Store.HasRecordedContent
	// for what reads it and why it is not the same question as "was this
	// snapshot ever written".
	StoreMetaContentRecorded = "content_recorded"
	// StoreMetaRequirementsHash is the metadata key for the requirements hash.
	StoreMetaRequirementsHash = "requirements_hash"
	// StoreMetaServer is the metadata key for the Galaxy server.
	StoreMetaServer = "server"
)

// FetchRetryPolicy is the fixed retry policy shared by every Galaxy API GET
// and artifact download: a small bounded number of attempts with a
// full-jitter exponential backoff between them. It is not configurable per
// call site, unlike some cache backends' own contention loops, since these
// calls run on the ordinary resolve/install path rather than a lock's own
// retry loop.
func FetchRetryPolicy() RetryPolicy {
	return RetryPolicy{Base: FetchRetryBackoffBase, Cap: FetchRetryBackoffCap, MaxAttempts: FetchRetryMaxAttempts}
}

// DefaultInstallWorkers derives the default size of the install and warm
// worker pool - the one that extracts a collection tree per worker - from
// procs, floored at MinDefaultInstallWorkers and capped at
// MaxDefaultInstallWorkers.
//
// procs is the CPU this process is PERMITTED to use, never the machine's core
// count, so a caller passes runtime.GOMAXPROCS(0) and never runtime.NumCPU().
// The two disagree under a CFS bandwidth quota - what Kubernetes limits.cpu
// and most containerized CI runners impose - because a quota bounds how much
// CPU time the cgroup may consume without changing which CPUs are visible:
// measured in a container on an 8-vCPU guest, --cpus=2 leaves
// runtime.NumCPU() reporting 8 while runtime.GOMAXPROCS(0) reports 2. Only a
// cpuset (--cpuset-cpus=0-1) narrows both. Sizing from the core count there
// hands a pod limited to 2 CPUs on a 64-core node a pool sized from 64 for the
// 2 CPUs it may actually use: 16 workers after this function's own cap, and up
// to 64 accepted from an operator through MaxAcceptedInstallWorkers.
//
// The floor buys latency hiding, not throughput. An install worker does not
// only extract: it can block on an artifact acquisition the prefetcher did
// not cover, and at a single worker one stalled acquisition serializes the
// whole run behind it. What that costs is small and measured: on 1 permitted
// CPU, warm goes from 0.612 s at one worker to 0.686 s at two, a 12% penalty
// paid so that one stall cannot become the run's critical path.
//
// The cap is memory, not throughput, and two constraints meet at its value.
// The first is a budget of 64 MiB for gzip block pools, and what that budget
// governs is this pool: a worker here opens a decompressor per unit of work,
// and those units are strictly sequential and never nested - a verifying
// install runs manifest.ReadFromTarGz to completion before
// manifest.VerifyChain opens a reader of its own, and both are closed before
// extractCollection unpacks - so a worker holds one reader at a time however
// many it opens over one collection. Which units they are is deliberately not
// enumerated: the list is not closed, since under --no-cache this pool takes
// the artifact-download path itself, shape probe included, with no prefetcher
// ahead of it. What the budget needs is the bound, and that is the largest
// single reader, the unpack's: gzipstream.NewReader takes pgzip's own
// defaults, and pgzip eagerly fills a pool of defaultBlocks (4) buffers of
// defaultBlockSize (1 MiB) before it reads a byte, so a worker reserves 4 MiB
// for as long as a unit is running, and 64 MiB / 4 MiB is 16 workers.
//
// The artifact-download pool holds a decompressor too, so this budget is not
// the program's whole claim on one - but that one is sized for a shape probe
// rather than for an unpack (archive.ProbeTarGz, whose own doc comment holds
// the argument for the sizing), so at both defaults' ceilings the two pools
// reserve this 64 MiB plus MaxDefaultDownloadWorkers (32) times the 64 KiB
// that probe reserves today, or 66 MiB - both figures move with a re-sizing
// of the probe, which lives in another package and cannot be named here as a
// symbol. The two pools rather than every decompressor in the program: the S3
// backend opens one of its own to inflate a state object, at init, before
// either pool has started. At the default ceilings and not in general:
// --workers and --download-workers each replace their own default outright,
// and what bounds a replacement is not the same on the two. --download-workers
// is clamped only below 1, so nothing bounds that pool's claim from above;
// --workers is clamped above as well, at MaxAcceptedInstallWorkers(procs),
// which ties this pool's claim to the permitted CPU rather than removing it -
// an operator's own 64 on a node permitting 64 CPUs is inside that ceiling and
// reserves 64 * 4 MiB = 256 MiB in block pools.
//
// Beside a real run's footprint: peak RSS over a 100-collection corpus
// measured 93.0 MiB frozen+offline and 183.0 MiB s3-warm (testing/bench.sh's
// resource rows). What each row does and does not exercise is worth stating,
// since neither is evidence of headroom at this ceiling. The frozen+offline
// row opens no reader at all: the harness warms the extracted store first and
// then wipes only the install target, so every collection hardlinks through
// extracted.Store.Ensure rather than unpacking, and --offline downloads
// nothing. The s3-warm row does extract at this pool's own concurrency, since
// the harness wipes the local cache directory before it and leaves its
// extracted store cold, but opens no probe reader: its "Bytes downloaded 0" is
// a run that took no fresh origin download, the only thing that counter
// records. What neither row records is the CPU the measuring host permitted,
// so neither says how many workers ran concurrently. The comparison
// establishes the budget's scale beside what a real run of this size costs,
// not what such a run would peak at with 16 workers all holding a reader.
//
// The second constraint is that a cap must never remove a measured gain, and
// ext4 was still improving at 12 workers, so nothing below that is
// admissible. 16 satisfies both.
//
// Disclosed residual: this default encodes no filesystem's metadata-mutation
// behavior, and metadata mutation - not CPU - is what an extraction is
// actually bounded by (creating 46,100 entries costs 12.766 s where
// traversing the same 46,100 costs 0.278 s, a factor of 46; across a cold
// 100-collection corpus the total user CPU is 1.2-2.4 s against 15.9-86.8 s
// of system time). On a filesystem that serializes metadata mutation this is
// therefore not the best value - measured, APFS on a 12-core host has its
// optimum at 4, well under what this function returns there - and the remedy
// is --workers, which replaces this default outright for any count inside
// MaxAcceptedInstallWorkers' range. Lowering the count to 1 or more - the
// direction this residual points in - is always inside it. Deriving the
// default from the filesystem instead would mean probing it, which is a
// different mechanism from this one.
func DefaultInstallWorkers(procs int) int {
	return min(max(procs, MinDefaultInstallWorkers), MaxDefaultInstallWorkers)
}

// MaxAcceptedInstallWorkers is the largest --workers value a run accepts on a
// machine permitting procs CPUs. It bounds a value an OPERATOR supplied, which
// is what separates it from MaxDefaultInstallWorkers: that constant caps what
// DefaultInstallWorkers may DERIVE, so a machine permitting 64 CPUs derives 16
// and still accepts an operator's own 32.
//
// The ceiling is the permitted CPU because that is what an install worker
// competes for - it extracts a tree in its own goroutine - so past that number
// the workers contend rather than add. That is where they certainly stop
// adding rather than where they always do: metadata mutation rather than CPU
// is what an extraction is actually bounded by, so on a filesystem that
// serializes it the useful count sits lower still - the residual
// DefaultInstallWorkers discloses on its own default, and one this ceiling
// does not close either. procs carries exactly the meaning it has in
// DefaultInstallWorkers, whose doc comment holds the measurement of why a CFS
// quota makes runtime.GOMAXPROCS(0) and runtime.NumCPU() different answers; a
// caller supplies it the same way here.
//
// What contends past that number is the extraction work, and that is a
// disclosed narrowing rather than the whole story of what a worker does: an
// install worker can also block on an artifact acquisition the prefetcher did
// not cover, which is the stall DefaultInstallWorkers' own floor buys tolerance
// for - deliberately, at a measured throughput cost, and by one extra worker
// rather than by a count that keeps scaling. That argument does not stop at a
// single permitted CPU, and nothing measured here says a larger
// oversubscription buys more tolerance, so an operator asking for 12 on a
// machine permitting 8 in order to hide uncovered acquisitions is replaced and
// warned like any other value outside the range: a deliberate oversubscription
// for stall hiding is the configuration this ceiling refuses.
//
// It is floored at MinDefaultInstallWorkers so that the ceiling can never sit
// below the value substituted for one outside it: on a single permitted CPU
// DefaultInstallWorkers returns 2, and an unfloored ceiling of 1 would put
// that substitute outside the very range it is being substituted into.
//
// It governs cfg.Workers alone and must not be extended to cfg.DownloadWorkers,
// whose default is deliberately a multiple of the permitted CPU (see
// DownloadWorkersPerCPU): that pool waits on the network rather than on the
// CPU, so a count well above the permitted CPU is its useful configuration
// rather than an oversubscription of it.
func MaxAcceptedInstallWorkers(procs int) int {
	return max(procs, MinDefaultInstallWorkers)
}

// DefaultDownloadWorkers derives the default size of the artifact-download
// and cache-presence-probe worker pool from the CPU this process is permitted
// to use: procs * DownloadWorkersPerCPU, floored at MinDefaultDownloadWorkers
// and capped at MaxDefaultDownloadWorkers. procs carries exactly the meaning
// it has in DefaultInstallWorkers, and a caller supplies it the same way.
//
// This is a deliberately different derivation from cfg.Workers' own worker
// count (see DefaultInstallWorkers), and the split stops at exactly these two
// pools. An install or warm worker extracts a tree in the same goroutine that
// downloaded it, so its pool tracks permitted CPU and is capped by what those
// concurrent extractions cost in memory - raising it to this function's own
// ceiling would mean up to 32 simultaneous extractions on one machine,
// trading speed for thrashing on a small runner rather than gaining anything.
// A download or cache-presence-probe worker, by contrast, mostly waits on the
// network - a HEAD probe or a streamed GET into a temp file - and what local
// work it does do is a sha256 over every byte it writes to that temp file,
// with a shape probe on top of it (archive.ProbeTarGz, whose decompressor is
// sized for a probe rather than for an unpack), so its useful pool size is
// still decided by outstanding requests rather than by what that local work
// costs, and sizing it to the permitted CPU alone would leave a low-core CI
// runner far below what the network link can sustain. outdated is
// deliberately left on cfg.Workers rather than gaining this knob too, so the
// split has exactly one seam.
func DefaultDownloadWorkers(procs int) int {
	return min(max(procs*DownloadWorkersPerCPU, MinDefaultDownloadWorkers), MaxDefaultDownloadWorkers)
}

// ReadOnlyPerm strips every write bit (owner, group, other) from perm,
// leaving its read and execute bits untouched. It is meant for regular files
// only: a directory must never be passed through this, since a read-only
// directory blocks entry creation and deletion (os.RemoveAll itself would
// start failing), and a symlink's mode is meaningless on every platform this
// project targets and must never be chmod'ed - os.Chmod follows a symlink
// and would silently mutate whatever it points at instead of the link
// itself.
func ReadOnlyPerm(perm fs.FileMode) fs.FileMode {
	return perm &^ fs.FileMode(WritePermBits)
}

// IsRetryableHTTPStatus reports whether status is one of the small set of
// transient HTTP statuses (rate limiting and server-side failures) that are
// safe to retry on an idempotent request: a Galaxy API GET, an artifact
// download, or an idempotent S3 verb. It is the single definition of that
// set for the whole program - a subsystem must not keep a private copy,
// since two copies could drift into retrying different statuses depending
// only on which one issued the request.
func IsRetryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
