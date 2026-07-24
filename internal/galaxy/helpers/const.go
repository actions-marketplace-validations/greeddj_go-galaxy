package helpers

import (
	"net/http"
	"time"
)

const (
	// DirMod is the default permission for created directories.
	DirMod = 0o755
	// FileMod is the default permission for created files.
	FileMod = 0o644

	// CollectionNameParts is the expected number of parts in a collection name like "namespace.collection".
	CollectionNameParts = 2

	// CacheLatestMetadataTTL is the TTL for cached metadata before revalidation.
	CacheLatestMetadataTTL = 10 * time.Minute

	// ArchiveMaxEntrySize caps a single archive entry size during extraction.
	ArchiveMaxEntrySize = int64(512 << 20) // 512 MiB per file
	// ArchiveMaxTotalSize caps total extracted bytes per archive.
	ArchiveMaxTotalSize = int64(4 << 30) // 4 GiB per archive
	// ArchiveMaxEntryCount caps the number of entries extracted from a single
	// archive. It complements the byte caps above by bounding inode
	// exhaustion: a tarbomb of many zero-byte directories or hardlinks never
	// trips ArchiveMaxEntrySize or ArchiveMaxTotalSize but can still exhaust
	// filesystem inodes one cheap entry at a time. It is set well above any
	// real collection (typically a few thousand to ~10-20k files) and well
	// below a count that would meaningfully exhaust inodes.
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

	// MetadataMaxSize caps the raw bytes read for a single Galaxy API
	// metadata response before it is rejected. Realistic Galaxy metadata JSON
	// is KB to low MB - a root document is ~1-2 KB, a versions page is
	// ~15-20 KB, a version-detail document is well under 1 MB, and even a
	// 10,000-version list is only ~1.5 MB - so 16 MiB bounds the per-fetch
	// io.ReadAll allocation and rejects a hostile giant or endless body
	// without ever false-positiving on a legitimate response.
	MetadataMaxSize = int64(16 << 20) // 16 MiB

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

	// FetchDefaultTimeout is the overall HTTP client timeout.
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

	// StoreSnapshotSchemaVersion is the current snapshot schema version.
	StoreSnapshotSchemaVersion = 4

	// CacheEntryMaxAge is the retention window for persisted cache entries
	// (API responses, resolved versions lists, and dependency constraints).
	// At persist time, an entry last written longer ago than this - for API
	// entries, last written or last revalidated - is pruned from the snapshot,
	// bounding both the local Bolt file and the S3 object in size and age.
	CacheEntryMaxAge = 30 * 24 * time.Hour

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
	// StoreBucketRoots is the bucket name for root collections.
	StoreBucketRoots = "roots"
	// StoreBucketResolved is the bucket name for resolved collections.
	StoreBucketResolved = "resolved"
	// StoreBucketVersions is the bucket name for versions cache.
	StoreBucketVersions = "versions_cache"

	// StoreMetaSchemaVersion is the metadata key for the snapshot schema version.
	StoreMetaSchemaVersion = "schema_version"
	// StoreMetaLastSnapshot is the metadata key for the last snapshot time.
	StoreMetaLastSnapshot = "last_snapshot"
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

// IsRetryableHTTPStatus reports whether status is one of the small set of
// transient HTTP statuses (rate limiting and server-side failures) that are
// safe to retry on an idempotent Galaxy API GET or artifact download.
func IsRetryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
