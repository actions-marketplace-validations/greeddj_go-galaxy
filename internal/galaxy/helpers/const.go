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
	// may make the extractor pull out of its gzip reader - every byte of the
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

	// DownloadWorkersPerCPU is the per-core multiplier DefaultDownloadWorkers
	// applies before clamping. An artifact download or cache presence probe
	// waits on the network, not the CPU - a HEAD probe or a streamed GET into
	// a temp file, never an extraction - so a useful pool size is a multiple
	// of the core count rather than the core count itself, unlike cfg.Workers,
	// whose pool extracts a tree per worker and is genuinely CPU-bound.
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
	StoreSnapshotSchemaVersion = 6

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

// DefaultDownloadWorkers derives the default size of the artifact-download
// and cache-presence-probe worker pool from the machine's core count:
// cpus * DownloadWorkersPerCPU, floored at MinDefaultDownloadWorkers and
// capped at MaxDefaultDownloadWorkers.
//
// This is a deliberately different derivation from cfg.Workers' own worker
// count (one per CPU), and the split stops at exactly these two pools. An
// install or warm worker extracts a tree in the same goroutine that
// downloaded it, so its parallelism is genuinely CPU-bound and stays sized
// to the core count - raising it to this function's own ceiling would mean
// up to 32 simultaneous extractions on one machine, trading speed for
// thrashing on a small runner rather than gaining anything. A download or
// cache-presence-probe worker, by contrast, only waits on the network - a
// HEAD probe or a streamed GET into a temp file, never an extraction - so
// its useful pool size tracks outstanding requests rather than CPU cores,
// and sizing it to the core count alone would leave a low-core CI runner far
// below what the network link can sustain. outdated is deliberately left on
// cfg.Workers rather than gaining this knob too, so the split has exactly
// one seam.
func DefaultDownloadWorkers(cpus int) int {
	return min(max(cpus*DownloadWorkersPerCPU, MinDefaultDownloadWorkers), MaxDefaultDownloadWorkers)
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
