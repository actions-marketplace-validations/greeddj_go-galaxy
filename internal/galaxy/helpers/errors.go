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
	// ErrCacheBusy indicates the cache is held by another process and could
	// not be opened within the allotted timeout.
	ErrCacheBusy = errors.New("another process holds the cache")
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

	// ErrOfflineMode indicates a network operation was attempted in offline mode.
	ErrOfflineMode = errors.New("offline mode is enabled, network access is forbidden")
	// ErrLockfileMismatch indicates the lockfile content does not match the resolution.
	ErrLockfileMismatch = errors.New("lockfile does not match resolved requirements")
	// ErrLockfileMissing indicates a lockfile was required but not found.
	ErrLockfileMissing = errors.New("lockfile is required by --frozen but not found")
	// ErrLockfileInvalid indicates the lockfile is malformed or unsupported.
	ErrLockfileInvalid = errors.New("lockfile is invalid")

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
)
