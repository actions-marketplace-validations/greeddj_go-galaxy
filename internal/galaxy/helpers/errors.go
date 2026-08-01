package helpers

import "errors"

var (
	// ErrSymlinkTargetResolvesToSelf indicates a symlink resolves to itself.
	ErrSymlinkTargetResolvesToSelf = errors.New("symlink target resolves to self")
	// ErrSymlinkTargetEscapesDestination indicates a symlink escapes the target directory.
	ErrSymlinkTargetEscapesDestination = errors.New("symlink target escapes destination")
	// ErrSymlinkTarget indicates a symlink target is invalid.
	ErrSymlinkTarget = errors.New("symlink target is invalid")
	// ErrSymlinkTargetResolvesToRoot indicates a symlink resolves to the root directory.
	ErrSymlinkTargetResolvesToRoot = errors.New("symlink target resolves to root")
	// ErrSymlinkTargetIsAbsolute indicates a symlink target is an absolute path.
	ErrSymlinkTargetIsAbsolute = errors.New("symlink target is absolute")
	// ErrSymlinkTargetIsEmpty indicates a symlink target is empty.
	ErrSymlinkTargetIsEmpty = errors.New("symlink target is empty")

	// ErrArchivePathContainsSymlinkComponent indicates an archive path traverses a symlink.
	ErrArchivePathContainsSymlinkComponent = errors.New("archive path contains symlink component")
	// ErrArchiveExceedsMaxSize indicates an archive exceeds the maximum total size.
	ErrArchiveExceedsMaxSize = errors.New("archive exceeds maximum total size")
	// ErrArchiveEntryHasNegativeSize indicates an archive entry has a negative size.
	ErrArchiveEntryHasNegativeSize = errors.New("archive entry has negative size")
	// ErrArchiveEntryIsTooLarge indicates an archive entry is too large.
	ErrArchiveEntryIsTooLarge = errors.New("archive entry is too large")
	// ErrArchiveEntryEscapesDestination indicates an archive entry escapes the destination.
	ErrArchiveEntryEscapesDestination = errors.New("archive entry escapes destination")
	// ErrArchiveEntryIsAbsolutePath indicates an archive entry uses an absolute path.
	ErrArchiveEntryIsAbsolutePath = errors.New("archive entry is absolute path")
	// ErrArchiveEntryHasEmptyName indicates an archive entry has an empty name.
	ErrArchiveEntryHasEmptyName = errors.New("archive entry has empty name")
	// ErrArchiveTooManyEntries indicates an archive contains more entries
	// than ArchiveMaxEntryCount allows.
	ErrArchiveTooManyEntries = errors.New("archive contains too many entries")
	// ErrArchiveDuplicateEntry indicates a tar archive contains a regular-file
	// entry whose on-disk path is already occupied by something the same
	// archive extracted earlier - most often a second regular-file entry at
	// that path, but equally a directory entry the file now collides with.
	// Extracted regular files are opened read-only (see
	// archive.extractRegularFile), so such an entry fails os.OpenFile with a
	// bare permission error against what is already there, which is
	// undebuggable on its own; this sentinel reclassifies that failure into
	// something that names the offending archive path. The classification is
	// existence-based rather than errno-based, so it deliberately covers both
	// collision shapes under one name - "two entries want the same path" is
	// the actionable fact either way. Previously a duplicate regular-file
	// entry silently won; that is a deliberate behavior change, not a
	// regression.
	ErrArchiveDuplicateEntry = errors.New("archive contains a duplicate entry")

	// ErrHardlinkTargetIsEmpty indicates a hardlink target is empty.
	ErrHardlinkTargetIsEmpty = errors.New("hardlink target is empty")
	// ErrFileIsEmpty indicates a file is empty.
	ErrFileIsEmpty = errors.New("file is empty")

	// ErrS3EmptyCreds indicates S3 cache credentials are required but missing.
	ErrS3EmptyCreds = errors.New("s3 cache requires access/secret keys when GO_GALAXY_S3_BUCKET is set")

	// ErrArtifactCacheNotConfigured indicates the artifact cache is unavailable.
	ErrArtifactCacheNotConfigured = errors.New("artifact cache is not configured")
	// ErrMetadataIsNil indicates metadata is nil when required.
	ErrMetadataIsNil = errors.New("metadata is nil")
	// ErrMissingDownloadURL indicates a collection download URL is missing.
	ErrMissingDownloadURL = errors.New("missing download url")
	// ErrConfigIsNil indicates a nil config was provided.
	ErrConfigIsNil = errors.New("config is nil")
	// ErrSHA256Mismatch indicates a checksum mismatch.
	ErrSHA256Mismatch = errors.New("sha256 mismatch")
	// ErrMetadataUnavailable indicates metadata could not be loaded.
	ErrMetadataUnavailable = errors.New("metadata unavailable")
	// ErrUnsupportedRequirementsFormat indicates the requirements file format is unsupported.
	ErrUnsupportedRequirementsFormat = errors.New("unsupported requirements file format")

	// ErrCacheDirEmpty indicates the cache directory is empty.
	ErrCacheDirEmpty = errors.New("cache directory is empty")
	// ErrAnotherInstanceIsRunning indicates another instance is already running.
	ErrAnotherInstanceIsRunning = errors.New("another instance is running")
	// ErrCacheBusy indicates the cache is held by another holder and could
	// not be acquired within this backend's own ceiling - true of a local
	// Bolt open timeout and of the S3 distributed lock's wait-ceiling
	// timeout against an observed holder alike; neither names the other's
	// mechanism.
	ErrCacheBusy = errors.New("another process holds the cache")
	// ErrCacheBackendUnavailable indicates a remote cache backend could not
	// be reached, or answered a request with a failure that is not this
	// program's own doing.
	ErrCacheBackendUnavailable = errors.New("cache backend unavailable")
	// ErrCacheBackendUnusable indicates the configured backend cannot
	// provide a guarantee this tool requires, or cannot be addressed at
	// all; no retry can change that and the only remedy is a configuration
	// change.
	ErrCacheBackendUnusable = errors.New("cache backend cannot be used as configured")
	// ErrNoSemverCandidates indicates no semver candidates are available.
	ErrNoSemverCandidates = errors.New("no semver candidates available")
	// ErrMissingResolvedParent indicates a resolved parent is missing.
	ErrMissingResolvedParent = errors.New("missing resolved parent")
	// ErrMissingResolvedDependency indicates a resolved dependency is missing.
	ErrMissingResolvedDependency = errors.New("missing resolved dependency")
	// ErrNoVersionSatisfiesConstraints indicates no version satisfies constraints.
	ErrNoVersionSatisfiesConstraints = errors.New("no version satisfies constraints")
	// ErrConflictingRootConstraints indicates root constraints conflict.
	ErrConflictingRootConstraints = errors.New("conflicting root constraints")
	// ErrConflictingExactVersions indicates exact version constraints conflict.
	ErrConflictingExactVersions = errors.New("conflicting exact versions")
	// ErrDependencyGraphHasACycle indicates the dependency graph has a cycle.
	ErrDependencyGraphHasACycle = errors.New("dependency graph has a cycle")
	// ErrVersionsPayloadEmpty indicates a versions payload is empty.
	ErrVersionsPayloadEmpty = errors.New("versions payload is empty")
	// ErrVersionsPayloadUnsupported indicates a versions payload is unsupported.
	ErrVersionsPayloadUnsupported = errors.New("unsupported versions payload")
	// ErrVersionsPagingExceeded indicates the versions list kept reporting
	// more pages than the page ceiling allows. This is treated as a hard
	// failure rather than a silent truncation, since a caller resolving
	// against a truncated list could pick a version that does not actually
	// satisfy the requested constraints.
	ErrVersionsPagingExceeded = errors.New("versions pagination exceeded the page ceiling")
	// ErrDownloadFailed indicates a download failed.
	ErrDownloadFailed = errors.New("download failed")
	// ErrMissingResolvedRoot indicates a resolved root is missing.
	ErrMissingResolvedRoot = errors.New("missing resolved root")
	// ErrInstallationFailed indicates installation failed.
	ErrInstallationFailed = errors.New("installation failed")
	// ErrInvalidCollectionsList indicates the collections list is invalid.
	ErrInvalidCollectionsList = errors.New("invalid collections list")
	// ErrMissingCollection indicates a collection is missing.
	ErrMissingCollection = errors.New("missing collection")
	// ErrInvalidCollectionEntry indicates a collection entry is invalid.
	ErrInvalidCollectionEntry = errors.New("invalid collection entry")
	// ErrEmptyCollectionName indicates a collection name is empty.
	ErrEmptyCollectionName = errors.New("empty collection name")
	// ErrUnsupportedCollectionSource indicates a collection source is unsupported.
	ErrUnsupportedCollectionSource = errors.New("unsupported collection source")
	// ErrUnsupportedCollectionType indicates a collection type is unsupported.
	ErrUnsupportedCollectionType = errors.New("unsupported collection type")
	// ErrUnsupportedCollectionFormat indicates a collection format is unsupported.
	ErrUnsupportedCollectionFormat = errors.New("unsupported collection format")
	// ErrInvalidCollectionName indicates a collection name is invalid.
	ErrInvalidCollectionName = errors.New("invalid collection name")
	// ErrInvalidDependencyKey indicates a dependency map key is not a valid
	// "namespace.name" FQDN.
	ErrInvalidDependencyKey = errors.New("invalid dependency key")
	// ErrConflictingNamespaceName indicates an explicit namespace was given
	// alongside a dotted collection name, which would otherwise silently
	// install a different collection than either field implies alone.
	ErrConflictingNamespaceName = errors.New("explicit namespace conflicts with dotted collection name")
	// ErrInvalidCollectionKey indicates a collection key is invalid.
	ErrInvalidCollectionKey = errors.New("invalid collection key")
	// ErrDuplicateCollectionRequirement indicates a duplicate collection requirement.
	ErrDuplicateCollectionRequirement = errors.New("duplicate collection requirement")
	// ErrLoadMetadataFailed indicates loading collection metadata failed.
	ErrLoadMetadataFailed = errors.New("failed to load collection metadata")
	// ErrDuplicateCollectionKey indicates a duplicate collection entry.
	ErrDuplicateCollectionKey = errors.New("duplicate collection entry")
	// ErrWarmCacheDisabled indicates the warm command was run with --no-cache.
	// Unlike install, whose --no-cache still yields a correct install, warm's
	// entire output IS cache state: with caching disabled, every artifact
	// would be downloaded and then discarded without ever being committed to
	// the cache or the extracted store, so the run would report success while
	// warming nothing. This is rejected before any backend lock is taken or
	// any network request is made.
	ErrWarmCacheDisabled = errors.New("warm requires a cache: --no-cache leaves nothing to warm")

	// ErrDbNil indicates a nil Bolt DB was provided.
	ErrDbNil = errors.New("bolt DB is nil")
	// ErrStoreNil indicates a nil store was provided.
	ErrStoreNil = errors.New("store is nil")
	// ErrUnsupportedSchemaVersion indicates the snapshot schema version is unsupported.
	ErrUnsupportedSchemaVersion = errors.New("unsupported snapshot schema version")
	// ErrOutdatedSchemaVersion indicates the snapshot schema version is older
	// than the current one and the snapshot should be dropped and rebuilt.
	ErrOutdatedSchemaVersion = errors.New("outdated snapshot schema version")
	// ErrCorruptProjectRegistry indicates the project registry file or
	// object could not be decoded. It must never be treated as an empty
	// registry: cleanup computes reachability from every recorded project,
	// so silently substituting an empty registry would make it believe
	// nothing is reachable and delete every installed collection.
	ErrCorruptProjectRegistry = errors.New("corrupt project registry")
	// ErrStateObjectTooLarge indicates a persisted cache-state object (the S3
	// snapshot or project registry) could not be read within its declared
	// size ceiling - StateObjectMaxCompressedSize on the wire,
	// StateObjectMaxDecompressedSize once inflated - so a planted oversized
	// or high-ratio gzip object is never buffered whole into memory. Like
	// ErrCorruptProjectRegistry, the object this names cannot be trusted by
	// anyone and must be discarded (or replaced by clearing the cache)
	// before a run can proceed; it is never retried, since the same bytes
	// would overrun the same ceiling again.
	ErrStateObjectTooLarge = errors.New("cache state object exceeds the maximum allowed size")

	// ErrOfflineMode indicates a network operation was attempted in offline mode.
	ErrOfflineMode = errors.New("offline mode is enabled, network access is forbidden")
	// ErrReadStalled indicates a response body read made no progress within
	// the configured timeout window while the request's own context was
	// still live. A parent-context cancellation is reported as
	// context.Canceled instead, never as ErrReadStalled.
	//
	// It deliberately does NOT wrap its cause with %w, and this is a rule, not
	// a note about one call site: a sentinel this program raises to describe
	// why work ended must never leave a context sentinel reachable through
	// errors.Is. The watchdog aborts a stall by canceling its own derived
	// context to unblock the stuck read, so the cause is context.Canceled; left
	// wrapped with %w it would steal this failure's exit-code classification,
	// because exitcode.FromError checks context.Canceled ahead of every other
	// class and would report a hostile or degraded server as a caught Ctrl-C.
	// The cause is rendered with %v instead, so it stays diagnosable without
	// being matchable.
	ErrReadStalled = errors.New("network read stalled")
	// ErrArtifactDownloadDeadline indicates one artifact's acquisition exceeded
	// ArtifactDownloadDeadline: the whole-transfer ceiling the read-inactivity
	// watchdog cannot enforce, since a byte-drip makes real progress in every
	// idle window and so never trips it. It is deliberately distinct from
	// ErrReadStalled (no progress at all within one idle window) and from a
	// caller cancellation, which still surfaces as context.Canceled.
	//
	// It deliberately does NOT wrap its cause with %w. The cause is
	// context.DeadlineExceeded, or - when the watchdog's own derived-context
	// cancel raced the deadline - context.Canceled, and either one left in the
	// error tree would steal this failure's exit-code classification:
	// exitcode.FromError checks context.Canceled first and would report a
	// hostile server as ExitInterrupt, i.e. as a Ctrl-C. The cause is rendered
	// into the message with %v instead, so it stays diagnosable without being
	// matchable.
	//
	// It is never retried: the budget is spent, so every remaining attempt
	// would fail instantly against the same dead context.
	ErrArtifactDownloadDeadline = errors.New("artifact download deadline exceeded")
	// ErrMetadataFetchDeadline indicates one Galaxy metadata request exceeded
	// MetadataFetchDeadline: the whole-request ceiling that catches a
	// byte-drip response, which the read-inactivity watchdog cannot enforce
	// since it always makes real progress within every idle window.
	//
	// It deliberately does NOT wrap its cause with %w - the rule
	// ErrReadStalled's own doc comment states and this sentinel follows in
	// its own words: a sentinel raised to describe why work ended must never
	// leave a context sentinel reachable through errors.Is, because
	// exitcode.FromError checks context.Canceled ahead of every other class
	// and would report a hostile or degraded Galaxy server as a caught
	// Ctrl-C. The cause is rendered into the message with %v instead, so it
	// stays diagnosable without being matchable.
	//
	// It is never retried: the budget is spent, so every remaining attempt
	// would fail instantly against the same dead context.
	//
	// It is also never raised for an error that does not itself carry a
	// context signal (a %v-rendered context.DeadlineExceeded or
	// context.Canceled): a *cacheManager.HTTPStatusError - a 404, a 401/403,
	// or a retryable status - keeps its own identity even when this
	// request's budget expires in the same instant, so the root-metadata
	// server walk keeps routing on status instead of misreading a race
	// between "the response arrived" and "the budget expired" as a deadline.
	ErrMetadataFetchDeadline = errors.New("galaxy metadata fetch deadline exceeded")
	// ErrStateObjectDeadline indicates one persisted cache-state operation
	// (LoadStore, SaveStore, LoadProjectRegistry, or RecordProject) exceeded
	// StateObjectDeadline. Unlike ErrMetadataFetchDeadline, the operator
	// remediation this names is not "your Galaxy server is dripping" but
	// "your object store is dripping, and while it was, it held the
	// distributed lock and blocked every runner sharing this bucket" - hence
	// a separate sentinel rather than reusing ErrMetadataFetchDeadline for
	// both surfaces.
	//
	// It deliberately does NOT wrap its cause with %w, for the identical rule
	// ErrReadStalled's own doc comment states: a sentinel raised to describe
	// why work ended must never leave a context sentinel reachable through
	// errors.Is, since exitcode.FromError checks context.Canceled ahead of
	// every other class and would report a hostile or degraded object store
	// as a caught Ctrl-C. The cause is rendered into the message with %v
	// instead, so it stays diagnosable without being matchable.
	//
	// It is never retried: the budget is spent, so every remaining attempt
	// would fail instantly against the same dead context.
	ErrStateObjectDeadline = errors.New("cache state object deadline exceeded")
	// ErrLockfileMismatch indicates the lockfile content does not match the resolution.
	ErrLockfileMismatch = errors.New("lockfile does not match resolved requirements")
	// ErrLockfileMissing indicates a lockfile was required but not found.
	ErrLockfileMissing = errors.New("lockfile is required by --frozen but not found")
	// ErrLockfileInvalid indicates the lockfile is malformed or unsupported.
	ErrLockfileInvalid = errors.New("lockfile is invalid")
	// ErrLockfileDrift indicates lock --frozen compared a fresh resolve
	// against the lockfile already on disk and found a difference: the file
	// that exists is not the file a real `lock` run would write right now.
	// This is distinct from ErrLockfileMismatch (a lockfile that does not
	// cover the requirements roots at all) the same way a malformed digest
	// is kept distinct from a mismatched one - both pairs describe a
	// different failure shape under the same exit class, not the same
	// failure under two names.
	ErrLockfileDrift = errors.New("lockfile is out of date")

	// ErrInvalidTimeout indicates the --timeout value is neither a positive
	// integer number of seconds nor a valid positive Go duration string.
	ErrInvalidTimeout = errors.New("invalid timeout")

	// ErrAnsibleConfigNotFound indicates an explicitly requested ansible.cfg
	// path does not exist.
	ErrAnsibleConfigNotFound = errors.New("ansible config file not found")

	// ErrUnsafeCollectionIdentifier indicates a manifest field (namespace,
	// name, or version) cannot be safely used as a single filesystem path
	// element, e.g. it contains a path separator or is "..".
	ErrUnsafeCollectionIdentifier = errors.New("unsafe collection identifier")
	// ErrInvalidCollectionVersion indicates a resolved collection's version
	// is not IsExactVersion - a constraint string like "*" or ">=1.0.0", or
	// any other value that does not name one release - reaching a point in
	// the pipeline that requires an already-resolved, installable version.
	// This is deliberately distinct from ErrUnsafeCollectionIdentifier: an
	// unsafe identifier describes a path-traversal shape, while this
	// sentinel describes a version that is well-formed as a path element but
	// still not a version anything could install - the same distinction
	// ErrMalformedArtifactSHA256 draws against a syntactically fine but
	// wrong-shaped digest.
	ErrInvalidCollectionVersion = errors.New("collection version is not an exact version")
	// ErrUnsafeRemovalPath indicates a computed removal path failed a
	// containment check against its expected root directory.
	ErrUnsafeRemovalPath = errors.New("unsafe removal path")
	// ErrProjectRequirementsUnreadable indicates a recorded project's
	// requirements file could not be read or parsed even though the
	// project's workspace is present on disk. Cleanup must abort rather
	// than silently treat it as contributing zero reachability roots,
	// since that would make every uniquely-installed collection under
	// that project look unreachable and get deleted.
	ErrProjectRequirementsUnreadable = errors.New("project requirements file is unreadable")
	// ErrCorruptManifest indicates a MANIFEST.json file exists but could
	// not be parsed as JSON. The install it describes cannot be
	// identified, so it is reported rather than silently discarded, but is
	// still treated as neither a reachability source nor a deletion
	// candidate.
	ErrCorruptManifest = errors.New("corrupt manifest")

	// ErrResponseTooLarge indicates a response body read through
	// NewSizeLimitedReader exceeded its configured ceiling before it finished
	// streaming - an artifact download over ArtifactMaxDownloadSize, a Galaxy
	// metadata document over MetadataMaxSize, or an S3 list or batch-delete
	// response over S3ListMaxSize. It is never retried: a server that streams
	// past the ceiling once will do so again, so retrying would only spend
	// the retry budget re-fetching a hostile or broken response.
	//
	// A persisted cache-state object is capped by the same reader but never
	// surfaces as this sentinel: s3.readObject reclassifies it into
	// ErrStateObjectTooLarge before it reaches a caller, since an unreadable
	// state object is a corrupt-cache condition rather than a fetch that
	// failed.
	ErrResponseTooLarge = errors.New("response body exceeds the maximum allowed size")

	// ErrUnsupportedGalaxyServerKey indicates a [galaxy_server.<id>] section
	// used a key this tool deliberately refuses to interpret: username,
	// password, auth_url, or client_id imply Basic auth or a Keycloak/SSO
	// token exchange, neither of which this tool implements. This fails
	// config loading outright, before any request is made, rather than
	// silently sending an unauthenticated request and getting a confusing
	// 401 later.
	ErrUnsupportedGalaxyServerKey = errors.New("unsupported galaxy_server key: configure a Galaxy API token instead")
	// ErrUnsupportedGalaxyServerAPIVersion indicates a [galaxy_server.<id>]
	// section set api_version to something other than the one value this
	// tool understands ("v3").
	ErrUnsupportedGalaxyServerAPIVersion = errors.New("unsupported galaxy_server api_version")
	// ErrMissingGalaxyServerURL indicates a server named in server_list has
	// no url configured, from either its [galaxy_server.<id>] section or
	// the matching ANSIBLE_GALAXY_SERVER_<ID>_URL env var.
	ErrMissingGalaxyServerURL = errors.New("galaxy server is missing its url")
	// ErrInvalidGalaxyServerURL indicates a configured Galaxy server URL
	// could not be parsed as an absolute URL with a scheme and host. The
	// raw value is deliberately never included in this error's context: it
	// may carry embedded userinfo, and echoing it back would defeat the
	// point of ErrGalaxyServerURLUserinfo below.
	ErrInvalidGalaxyServerURL = errors.New("invalid galaxy server url")
	// ErrInvalidGalaxyServerID indicates a server_list id contains a
	// character outside [A-Za-z0-9_-]. Ansible does not enforce this, but
	// this tool does: a "." would make the "[galaxy_server.<id>]" section
	// grammar ambiguous, and every other character is unsafe to fold into
	// an ANSIBLE_GALAXY_SERVER_<ID>_* environment variable name.
	ErrInvalidGalaxyServerID = errors.New("invalid galaxy server id")
	// ErrDuplicateGalaxyServerID indicates server_list repeats an id, or
	// lists two ids that differ only in case. The latter is rejected
	// because both would resolve to the same ANSIBLE_GALAXY_SERVER_<ID>_*
	// environment variable prefix, making per-server env overrides
	// ambiguous.
	ErrDuplicateGalaxyServerID = errors.New("duplicate galaxy server id")
	// ErrInvalidValidateCerts indicates a validate_certs value is not one
	// of ansible's recognized boolean spellings (true/false, yes/no,
	// on/off, 1/0, case-insensitive). This is always a hard error: an
	// unparseable value must never silently resolve to "false" (certs
	// unverified).
	ErrInvalidValidateCerts = errors.New("invalid validate_certs value")
	// ErrGalaxyServerURLUserinfo indicates a configured Galaxy server URL, or
	// a requirements.yml collection's "source:", embeds userinfo (e.g.
	// "https://user:pass@hub/"). url.URL.String() renders the password back
	// out in plain text, so such a URL would otherwise leak into debug
	// output, the lockfile, GALAXY.yml, and the persisted snapshot; rejecting
	// it at config/requirements load closes that off structurally instead of
	// relying on every downstream consumer to remember to redact it.
	ErrGalaxyServerURLUserinfo = errors.New("galaxy server url must not contain userinfo")
	// ErrInsecureTokenTransport indicates a token is configured for a
	// plaintext (http) origin that is not loopback (localhost,
	// 127.0.0.0/8, or ::1). Sending a token over such a connection lets any
	// on-path observer capture it, so this is rejected at config load
	// rather than warned about at request time.
	ErrInsecureTokenTransport = errors.New("galaxy server token configured for an insecure plaintext transport")
	// ErrConflictingServerTLSPolicy indicates two configured servers share
	// a normalized origin (scheme, host, and effective port) but disagree
	// on validate_certs. One origin must map to exactly one transport, so
	// this conflict is a config error rather than an arbitrary
	// last-one-wins resolution.
	ErrConflictingServerTLSPolicy = errors.New("conflicting validate_certs for the same galaxy server origin")
	// ErrConflictingServerToken indicates two configured servers share a
	// normalized origin but carry different tokens (including one set and
	// one unset). Like ErrConflictingServerTLSPolicy, this keeps "one
	// origin, one transport" a structural invariant instead of a silent
	// pick between two credentials for the same endpoint.
	ErrConflictingServerToken = errors.New("conflicting token for the same galaxy server origin")
	// ErrAmbiguousGalaxyToken indicates --token (or GO_GALAXY_TOKEN) was set
	// while a multi-entry server_list is in effect. The flag names no server,
	// so there is no answer to which one the credential belongs to, and
	// guessing could send a private hub's token to the public Galaxy.
	// Configure the token in that server's own [galaxy_server.<id>] section
	// or its ANSIBLE_GALAXY_SERVER_<ID>_TOKEN variable instead.
	ErrAmbiguousGalaxyToken = errors.New("--token is ambiguous with a multi-entry server_list")

	// ErrGalaxyAuthFailed indicates a configured Galaxy server answered a
	// root-metadata request with 401 or 403. This is fail-closed: unlike a
	// 404 (which only means this server does not have the collection and
	// advances the server-list walk to the next candidate), a credential
	// failure aborts the whole run rather than silently falling through to
	// another server that might answer anonymously.
	ErrGalaxyAuthFailed = errors.New("galaxy server authentication failed")
	// ErrGalaxyServerUnavailable indicates a configured Galaxy server kept
	// answering a root-metadata request with a retryable status (429, 500,
	// 502, 503, or 504) until the retry budget was spent. Like
	// ErrGalaxyAuthFailed, this aborts the run instead of advancing to the
	// next server in the list: a transient outage on one server is not
	// evidence the collection is absent there, so silently falling through
	// would risk installing from the wrong server once the outage clears.
	ErrGalaxyServerUnavailable = errors.New("galaxy server unavailable")

	// ErrCollectionsPathEscape indicates an install-side write refused to
	// follow a path component under cfg.DownloadPath (most dangerously
	// ansible_collections itself, or a namespace/name component beneath it)
	// because it resolved to a symlink, or because a filesystem error other
	// than "does not exist" was found while diagnosing an os.Root refusal.
	// os.Root itself is what actually blocks the escape, atomically, inside
	// the kernel; this sentinel only names which component classifyCollectionsRootError
	// found responsible, for an operator-facing message - it is never the
	// mechanism that prevents the write.
	ErrCollectionsPathEscape = errors.New("path escapes the collections directory")

	// ErrMalformedArtifactSHA256 indicates a value that was supposed to be an
	// artifact's sha256 digest is not a 64-character lowercase hex string.
	// This is deliberately distinct from ErrSHA256Mismatch: a mismatch means
	// two valid digests disagree, while a malformed digest is not a digest
	// at all - most often a poisoned snapshot or a lying server.
	//
	// This exclusion is specific to one arm of prepareWithRecovery, not a
	// global claim about retrying: that function's prepareInstall-error arm
	// classifies on ErrSHA256Mismatch and this sentinel is deliberately not
	// in that class, since retrying cannot repair the metadata cache entry
	// that produced a malformed value in the first place. Its separate
	// action-error arm evicts on every artifact-side failure, unclassified
	// among those, per the tradeoff documented on prepareWithRecovery itself
	// - and this sentinel sits outside that class for the identical reason:
	// eviction cannot repair a value that was never a valid digest to begin
	// with. That action arm also carries one further, destination-side
	// exclusion of its own (isDestinationSideFailure) that has nothing to do
	// with this sentinel; see prepareWithRecovery's doc comment for that one.
	ErrMalformedArtifactSHA256 = errors.New("artifact sha256 is not a 64-character lowercase hex digest")
)
