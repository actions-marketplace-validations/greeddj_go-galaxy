// Package exitcode maps sentinel errors and OS signals to process exit codes,
// giving go-galaxy a stable, documented exit-code taxonomy that scripts and CI
// pipelines can branch on instead of treating every failure as a flat 1.
package exitcode

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// ExitOK indicates the command completed successfully.
	ExitOK = 0
	// ExitError is the generic fallback for errors that do not map to a
	// more specific class below.
	ExitError = 1
	// ExitUsage indicates invalid configuration, flags, or requirements input.
	ExitUsage = 2
	// ExitResolution indicates dependency resolution failed (conflicts,
	// missing candidates, or a cycle in the dependency graph).
	ExitResolution = 3
	// ExitNetwork indicates a network or Galaxy API failure, including
	// timeouts and offline-mode violations.
	ExitNetwork = 4
	// ExitInstall indicates an install-time failure (unsafe archive/symlink
	// content, empty file). A checksum mismatch or malformed digest is
	// ExitIntegrity below instead.
	ExitInstall = 5
	// ExitLock indicates a lockfile is missing, invalid, or does not match
	// the resolved requirements.
	ExitLock = 6
	// ExitIntegrity indicates artifact content failed to authenticate against
	// the sha256 that named it - a lockfile pin, a Galaxy server's declared
	// digest, a cache sidecar, or the extracted store's content-address key -
	// or that such a digest was structurally malformed. This is a stop-and-alert
	// class, deliberately separate from ExitInstall and ExitNetwork: retrying
	// the same run cannot repair it, because the bytes or the digest are wrong
	// at the source.
	ExitIntegrity = 7
	// ExitInterrupt indicates the run was canceled, either by a caught
	// signal falling back to this default or by context cancellation.
	ExitInterrupt = 130
)

// signalExitBase is added to the numeric signal value to build a
// shell-convention exit code (128 + signal number), per FromSignal.
const signalExitBase = 128

// FromError classifies err into an exit code by matching it against the
// known sentinel errors declared in internal/galaxy/helpers, in priority
// order: cancellation, integrity, lock, install, network, resolution, then
// usage/config. The first matching class wins; unrecognized errors fall
// back to ExitError. The per-class checks are split into helpers below to
// keep this function short; each still short-circuits on the first match.
func FromError(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled):
		return ExitInterrupt
	// isIntegrityError must be checked before isInstallError: after
	// collections.Start started joining per-collection causes behind
	// helpers.ErrInstallationFailed, every integrity failure's error tree
	// also contains that sentinel, so placing this case any lower would make
	// it unreachable - isInstallError would already have claimed the error.
	// Cancellation still outranks it.
	case isIntegrityError(err):
		return ExitIntegrity
	case isLockError(err):
		return ExitLock
	case isInstallError(err):
		return ExitInstall
	case isNetworkError(err):
		return ExitNetwork
	case isResolutionError(err):
		return ExitResolution
	case isUsageError(err):
		return ExitUsage
	default:
		return ExitError
	}
}

// isIntegrityError reports whether err is an artifact-digest authentication
// failure: content that did not hash to the digest that named it, or a value
// that was supposed to be such a digest and was not.
func isIntegrityError(err error) bool {
	return errors.Is(err, helpers.ErrSHA256Mismatch) ||
		errors.Is(err, helpers.ErrMalformedArtifactSHA256)
}

// isLockError reports whether err is a lockfile-related sentinel.
func isLockError(err error) bool {
	return errors.Is(err, helpers.ErrLockfileMismatch) ||
		errors.Is(err, helpers.ErrLockfileMissing) ||
		errors.Is(err, helpers.ErrLockfileInvalid)
}

// isInstallError reports whether err is an install-time sentinel (unsafe
// archive/symlink content, an empty file, or a missing artifact cache).
// Split into three sub-checks purely to stay under the cyclomatic-complexity
// budget; the three together still cover the exact same sentinel set.
func isInstallError(err error) bool {
	return isFileIntegrityError(err) || isArchiveError(err) || isSymlinkError(err)
}

// isFileIntegrityError reports whether err is an empty-file/missing-cache
// sentinel. helpers.ErrSHA256Mismatch is deliberately not here: it is
// isIntegrityError's alone, checked ahead of this function in FromError, so
// leaving it in both classes would be shadowed dead code.
func isFileIntegrityError(err error) bool {
	return errors.Is(err, helpers.ErrInstallationFailed) ||
		errors.Is(err, helpers.ErrFileIsEmpty) ||
		errors.Is(err, helpers.ErrHardlinkTargetIsEmpty) ||
		errors.Is(err, helpers.ErrArtifactCacheNotConfigured)
}

// isArchiveError reports whether err is an unsafe-archive sentinel.
func isArchiveError(err error) bool {
	return errors.Is(err, helpers.ErrArchivePathContainsSymlinkComponent) ||
		errors.Is(err, helpers.ErrArchiveExceedsMaxSize) ||
		errors.Is(err, helpers.ErrArchiveEntryHasNegativeSize) ||
		errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) ||
		errors.Is(err, helpers.ErrArchiveEntryEscapesDestination) ||
		errors.Is(err, helpers.ErrArchiveEntryIsAbsolutePath) ||
		errors.Is(err, helpers.ErrArchiveEntryHasEmptyName)
}

// isSymlinkError reports whether err is an unsafe-symlink sentinel. This
// group covers both an unsafe symlink found inside an extracted archive
// (the original set below) and an unsafe symlink found in the destination
// tree itself - a component of cfg.DownloadPath, most dangerously
// ansible_collections or a namespace/name directory beneath it, that
// resolves outside the collections root os.Root enforces.
func isSymlinkError(err error) bool {
	return errors.Is(err, helpers.ErrSymlinkTargetResolvesToSelf) ||
		errors.Is(err, helpers.ErrSymlinkTargetEscapesDestination) ||
		errors.Is(err, helpers.ErrSymlinkTarget) ||
		errors.Is(err, helpers.ErrSymlinkTargetResolvesToRoot) ||
		errors.Is(err, helpers.ErrSymlinkTargetIsAbsolute) ||
		errors.Is(err, helpers.ErrSymlinkTargetIsEmpty) ||
		errors.Is(err, helpers.ErrCollectionsPathEscape)
}

// isNetworkError reports whether err is a network or Galaxy API sentinel,
// including request timeouts, offline-mode violations, and a server-list
// walk aborting on a credential failure or an exhausted retry budget.
func isNetworkError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, helpers.ErrOfflineMode) ||
		errors.Is(err, helpers.ErrDownloadFailed) ||
		errors.Is(err, helpers.ErrMetadataUnavailable) ||
		errors.Is(err, helpers.ErrMetadataIsNil) ||
		errors.Is(err, helpers.ErrMissingDownloadURL) ||
		errors.Is(err, helpers.ErrVersionsPayloadEmpty) ||
		errors.Is(err, helpers.ErrVersionsPayloadUnsupported) ||
		errors.Is(err, helpers.ErrGalaxyAuthFailed) ||
		errors.Is(err, helpers.ErrGalaxyServerUnavailable)
}

// isResolutionError reports whether err is a dependency-resolution sentinel
// (conflicting constraints, missing candidates, or a cycle in the graph).
func isResolutionError(err error) bool {
	return errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) ||
		errors.Is(err, helpers.ErrConflictingRootConstraints) ||
		errors.Is(err, helpers.ErrConflictingExactVersions) ||
		errors.Is(err, helpers.ErrDependencyGraphHasACycle) ||
		errors.Is(err, helpers.ErrNoSemverCandidates) ||
		errors.Is(err, helpers.ErrMissingResolvedParent) ||
		errors.Is(err, helpers.ErrMissingResolvedDependency) ||
		errors.Is(err, helpers.ErrMissingResolvedRoot) ||
		errors.Is(err, helpers.ErrLoadMetadataFailed)
}

// isUsageError reports whether err is a configuration or CLI-input sentinel
// (invalid flags, malformed requirements, or a missing path). Split into
// four sub-checks purely to stay under the cyclomatic-complexity budget;
// the four together still cover the exact same sentinel set.
func isUsageError(err error) bool {
	return isConfigUsageError(err) ||
		isGalaxyServerConfigError(err) ||
		isCollectionNameUsageError(err) ||
		isCollectionListUsageError(err)
}

// isConfigUsageError reports whether err is a config/environment-level usage sentinel.
func isConfigUsageError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, helpers.ErrConfigIsNil) ||
		errors.Is(err, helpers.ErrS3EmptyCreds) ||
		errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) ||
		errors.Is(err, helpers.ErrCacheDirEmpty) ||
		errors.Is(err, helpers.ErrInvalidTimeout) ||
		errors.Is(err, helpers.ErrAnsibleConfigNotFound) ||
		errors.Is(err, helpers.ErrWarmCacheDisabled) ||
		errors.Is(err, helpers.ErrDryRunUnsupported)
}

// isGalaxyServerConfigError reports whether err is a Galaxy server
// configuration sentinel: a bad [galaxy_server.<id>] section, a bad
// server_list id, or a server whose URL/credential/TLS combination this
// tool refuses. Every one of these is raised while building the config,
// before a single request is made, which is what makes them usage errors
// rather than network ones - the operator has to edit ansible.cfg or an
// environment variable, not retry. Split into two sub-checks purely to
// stay under the cyclomatic-complexity budget; the two together still
// cover the exact same sentinel set.
func isGalaxyServerConfigError(err error) bool {
	return isGalaxyServerSectionError(err) || isGalaxyServerPolicyError(err)
}

// isGalaxyServerSectionError reports whether err says the configuration
// itself is malformed: an unsupported or unparseable key, a missing or
// syntactically invalid url, or an id that cannot be represented.
func isGalaxyServerSectionError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedGalaxyServerKey) ||
		errors.Is(err, helpers.ErrUnsupportedGalaxyServerAPIVersion) ||
		errors.Is(err, helpers.ErrMissingGalaxyServerURL) ||
		errors.Is(err, helpers.ErrInvalidGalaxyServerURL) ||
		errors.Is(err, helpers.ErrInvalidGalaxyServerID) ||
		errors.Is(err, helpers.ErrDuplicateGalaxyServerID) ||
		errors.Is(err, helpers.ErrInvalidValidateCerts)
}

// isGalaxyServerPolicyError reports whether err says the configuration
// parses but describes something this tool refuses to do: leak a
// credential through a URL or a plaintext transport, or give one origin
// two different credential/TLS policies.
func isGalaxyServerPolicyError(err error) bool {
	return errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) ||
		errors.Is(err, helpers.ErrInsecureTokenTransport) ||
		errors.Is(err, helpers.ErrConflictingServerTLSPolicy) ||
		errors.Is(err, helpers.ErrConflictingServerToken) ||
		errors.Is(err, helpers.ErrAmbiguousGalaxyToken)
}

// isCollectionNameUsageError reports whether err is an invalid-collection-name/format sentinel.
func isCollectionNameUsageError(err error) bool {
	return errors.Is(err, helpers.ErrEmptyCollectionName) ||
		errors.Is(err, helpers.ErrInvalidCollectionName) ||
		errors.Is(err, helpers.ErrInvalidCollectionKey) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionSource) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionType) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionFormat)
}

// isCollectionListUsageError reports whether err is an invalid-collections-list
// sentinel. helpers.ErrUnsafeCollectionIdentifier sits here alongside
// ErrDuplicateCollectionKey: buildCollectionsMap raises both while building
// the collections map from resolved requirements, before any install work
// starts, over malformed resolution input rather than anything the network or
// the install pipeline did - the same reasoning that makes an empty
// col.Version (never rejected by lockfile.validate) a usage error too, not a
// crash or a silent install-time surprise.
func isCollectionListUsageError(err error) bool {
	return errors.Is(err, helpers.ErrInvalidCollectionsList) ||
		errors.Is(err, helpers.ErrInvalidCollectionEntry) ||
		errors.Is(err, helpers.ErrMissingCollection) ||
		errors.Is(err, helpers.ErrDuplicateCollectionRequirement) ||
		errors.Is(err, helpers.ErrDuplicateCollectionKey) ||
		errors.Is(err, helpers.ErrUnsafeCollectionIdentifier)
}

// FromSignal converts an OS signal into a shell-convention exit code
// (128 + signal number). Signals that are not syscall.Signal (unusual on
// supported platforms) fall back to ExitError.
func FromSignal(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return signalExitBase + int(s)
	}
	return ExitError
}
