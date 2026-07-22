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
	// ExitInstall indicates an install-time or artifact-integrity failure
	// (checksum mismatch, unsafe archive/symlink content, empty file).
	ExitInstall = 5
	// ExitLock indicates a lockfile is missing, invalid, or does not match
	// the resolved requirements.
	ExitLock = 6
	// ExitInterrupt indicates the run was canceled, either by a caught
	// signal falling back to this default or by context cancellation.
	ExitInterrupt = 130
)

// signalExitBase is added to the numeric signal value to build a
// shell-convention exit code (128 + signal number), per FromSignal.
const signalExitBase = 128

// FromError classifies err into an exit code by matching it against the
// known sentinel errors declared in internal/galaxy/helpers, in priority
// order: cancellation, lock, install/integrity, network, resolution, then
// usage/config. The first matching class wins; unrecognized errors fall
// back to ExitError. The per-class checks are split into helpers below to
// keep this function short; each still short-circuits on the first match.
func FromError(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled):
		return ExitInterrupt
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

// isLockError reports whether err is a lockfile-related sentinel.
func isLockError(err error) bool {
	return errors.Is(err, helpers.ErrLockfileMismatch) ||
		errors.Is(err, helpers.ErrLockfileMissing) ||
		errors.Is(err, helpers.ErrLockfileInvalid)
}

// isInstallError reports whether err is an install-time or
// artifact-integrity sentinel (checksum mismatch, unsafe archive/symlink
// content, empty file, or a missing artifact cache). Split into three
// sub-checks purely to stay under the cyclomatic-complexity budget; the
// three together still cover the exact same sentinel set.
func isInstallError(err error) bool {
	return isFileIntegrityError(err) || isArchiveError(err) || isSymlinkError(err)
}

// isFileIntegrityError reports whether err is a checksum/empty-file/missing-cache sentinel.
func isFileIntegrityError(err error) bool {
	return errors.Is(err, helpers.ErrInstallationFailed) ||
		errors.Is(err, helpers.ErrSHA256Mismatch) ||
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

// isSymlinkError reports whether err is an unsafe-symlink sentinel.
func isSymlinkError(err error) bool {
	return errors.Is(err, helpers.ErrSymlinkTargetResolvesToSelf) ||
		errors.Is(err, helpers.ErrSymlinkTargetEscapesDestination) ||
		errors.Is(err, helpers.ErrSymlinkTarget) ||
		errors.Is(err, helpers.ErrSymlinkTargetResolvesToRoot) ||
		errors.Is(err, helpers.ErrSymlinkTargetIsAbsolute) ||
		errors.Is(err, helpers.ErrSymlinkTargetIsEmpty)
}

// isNetworkError reports whether err is a network or Galaxy API sentinel,
// including request timeouts and offline-mode violations.
func isNetworkError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, helpers.ErrOfflineMode) ||
		errors.Is(err, helpers.ErrDownloadFailed) ||
		errors.Is(err, helpers.ErrMetadataUnavailable) ||
		errors.Is(err, helpers.ErrMetadataIsNil) ||
		errors.Is(err, helpers.ErrMissingDownloadURL) ||
		errors.Is(err, helpers.ErrVersionsPayloadEmpty) ||
		errors.Is(err, helpers.ErrVersionsPayloadUnsupported)
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
// three sub-checks purely to stay under the cyclomatic-complexity budget;
// the three together still cover the exact same sentinel set.
func isUsageError(err error) bool {
	return isConfigUsageError(err) || isCollectionNameUsageError(err) || isCollectionListUsageError(err)
}

// isConfigUsageError reports whether err is a config/environment-level usage sentinel.
func isConfigUsageError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, helpers.ErrConfigIsNil) ||
		errors.Is(err, helpers.ErrS3EmptyCreds) ||
		errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) ||
		errors.Is(err, helpers.ErrCacheDirEmpty)
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

// isCollectionListUsageError reports whether err is an invalid-collections-list sentinel.
func isCollectionListUsageError(err error) bool {
	return errors.Is(err, helpers.ErrInvalidCollectionsList) ||
		errors.Is(err, helpers.ErrInvalidCollectionEntry) ||
		errors.Is(err, helpers.ErrMissingCollection) ||
		errors.Is(err, helpers.ErrDuplicateCollectionRequirement) ||
		errors.Is(err, helpers.ErrDuplicateCollectionKey)
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
