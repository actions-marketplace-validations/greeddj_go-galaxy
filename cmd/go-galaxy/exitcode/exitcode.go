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
	// the resolved requirements. It names the requirements lockfile only,
	// never the distributed cache lock: a cache-lock contention failure
	// classifies as ExitCacheBusy instead.
	ExitLock = 6
	// ExitIntegrity indicates artifact content failed to authenticate against
	// the sha256 that named it - a lockfile pin, a Galaxy server's declared
	// digest, a cache sidecar, or the extracted store's content-address key -
	// or that such a digest was structurally malformed. This is a stop-and-alert
	// class, deliberately separate from ExitInstall and ExitNetwork: retrying
	// the same run cannot repair it, because the bytes or the digest are wrong
	// at the source.
	ExitIntegrity = 7
	// ExitCacheBusy indicates the cache could not be acquired because
	// something else already holds it, across three producers with different
	// shapes: a local process's flock(2) refusing immediately with no wait at
	// all (helpers.ErrAnotherInstanceIsRunning - EWOULDBLOCK from a
	// non-blocking LOCK_EX|LOCK_NB, nothing "reached and answered" since
	// there is no remote party involved), a local Bolt file open timing out
	// against another process's held lock, or an S3 distributed-lock
	// acquisition exhausting its own wait ceiling against a live foreign
	// holder. This is a contention class, distinct from ExitNetwork: the
	// operator's actionable remedy is to retry, possibly after the other
	// holder finishes, rather than to treat it as a dead or misconfigured
	// backend. Disclosed, not hidden: the S3 producer's wait-ceiling arm can
	// report this class even when the attempt that ended the wait actually
	// failed for a live-backend transport reason racing the ceiling's own
	// expiry - see internal/cache/s3/variables.go's partition doc for the
	// mechanism and what bounds how narrow that window is.
	ExitCacheBusy = 8
	// ExitInterrupt indicates the run was canceled, either by a caught
	// signal falling back to this default or by context cancellation.
	ExitInterrupt = 130
)

// signalExitBase is added to the numeric signal value to build a
// shell-convention exit code (128 + signal number), per FromSignal.
const signalExitBase = 128

// FromError classifies err into an exit code by matching it against the
// known sentinel errors declared in internal/galaxy/helpers, in priority
// order: cancellation, integrity, lock, install, network, cache contention,
// resolution, then usage/config. The first matching class wins; unrecognized
// errors fall back to ExitError. The per-class checks are split into helpers
// below to keep this function short; each still short-circuits on the first
// match.
func FromError(err error) int {
	switch {
	case err == nil:
		return ExitOK
	// This case is correct only because no sentinel this program raises to
	// describe why work ended leaves a context sentinel reachable through
	// errors.Is - see helpers.ErrReadStalled's and
	// helpers.ErrArtifactDownloadDeadline's doc comments. A future sentinel
	// that wraps a context.Canceled/context.DeadlineExceeded cause with %w
	// would silently steal this case instead of being caught by it, since
	// this case is checked first and cannot distinguish "the sentinel's cause
	// happens to be a context error" from "the caller genuinely canceled".
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
	default:
		return fromErrorTail(err)
	}
}

// fromErrorTail classifies err by the rule FromError's own doc comment
// states - network, cache contention, resolution, then usage/config, in that
// order - falling back to ExitError when nothing matches. Split out of
// FromError purely to stay under the cyclomatic-complexity budget:
// golangci-lint's cyclop reported "calculated cyclomatic complexity for
// function FromError is 11, max is 10" once the cache-contention case was
// added as an eleventh branch.
//
// The split carries a footgun for a future editor: Go evaluates a switch's
// cases in source order, and a switch's default case - which is what routes
// here - is reached only once none of the preceding cases matched. So ANY
// case added anywhere in FromError's own switch, even one written textually
// below its `default:` line, outranks every class fromErrorTail classifies,
// regardless of where in this function's own switch that class's check sits.
func fromErrorTail(err error) int {
	switch {
	case isNetworkError(err):
		return ExitNetwork
	// isCacheBusyError sits in exactly this one position, for four
	// independent reasons. Not above context.Canceled (checked in FromError,
	// ahead of this function): a Ctrl-C that races a contention failure must
	// still report as ExitInterrupt, not as contention. Not above
	// isIntegrityError/isLockError/isInstallError (also checked in FromError,
	// ahead of this function): if a contention failure is ever folded behind
	// helpers.ErrInstallationFailed by annotateSaveFailure, it must classify
	// ExitInstall like every other per-collection cause, exactly as
	// helpers.ErrStateObjectDeadline already does - placing this case any
	// higher would invert that rule for contention alone. Not above
	// isNetworkError: a contention verdict arriving joined with a genuine
	// transport failure must keep the wire failure as the actionable cause.
	// Not below isUsageError: isUsageError's fs.ErrNotExist arm is broad
	// enough that any error tree carrying an os path error would satisfy
	// it, which would swallow a contention failure that happens to wrap one.
	case isCacheBusyError(err):
		return ExitCacheBusy
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

// isCacheBusyError reports whether err is a cache-contention sentinel: the
// cache backend (a local Bolt file or the S3 distributed lock) was reached
// and answered, but held by another holder past this backend's own wait
// ceiling, or another go-galaxy instance was already running.
func isCacheBusyError(err error) bool {
	return errors.Is(err, helpers.ErrCacheBusy) ||
		errors.Is(err, helpers.ErrAnotherInstanceIsRunning)
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
// helpers.ErrArtifactDownloadDeadline classifies here, unaggregated, as
// ExitNetwork; once collections.Start joins it behind
// helpers.ErrInstallationFailed the isInstallError case above claims it
// first, as ExitInstall - identical to every other per-collection failure,
// helpers.ErrDownloadFailed included, so this is not a behavior change. It
// never classifies as ExitInterrupt: the sentinel deliberately does not wrap
// its context.DeadlineExceeded/context.Canceled cause with %w (see its own
// doc comment), so that raw signal never reaches errors.Is(err,
// context.Canceled) above.
//
// helpers.ErrReadStalled follows the identical shape: unaggregated, it
// classifies here as ExitNetwork; once collections.Start joins it behind
// helpers.ErrInstallationFailed for a per-collection stall, isInstallError
// claims it first, as ExitInstall, the same as every other per-collection
// failure. It never classifies as ExitInterrupt either: the watchdog aborts a
// stall by canceling its own derived context, so the cause is
// context.Canceled, but the producer renders that cause with %v rather than
// wrapping it with %w (see helpers.ErrReadStalled's own doc comment), so it
// never reaches errors.Is(err, context.Canceled) above.
//
// helpers.ErrMetadataFetchDeadline follows the same shape as
// helpers.ErrArtifactDownloadDeadline: unaggregated (hit resolving, before
// any collection-level work starts) it classifies here as ExitNetwork - which
// also outranks isResolutionError below, and is the right answer, since the
// cause is the wire, not an unsatisfiable constraint; hit inside an install
// worker (metadata re-resolution during install.go's per-collection path) it
// is instead joined behind helpers.ErrInstallationFailed and isInstallError
// claims it first, as ExitInstall, identical to every other per-collection
// failure. It never classifies as ExitInterrupt, for the identical %v-not-%w
// reason.
//
// helpers.ErrStateObjectDeadline follows the identical shape too, and CAN be
// aggregated - but the rule is WHEN, not WHERE it was hit: it classifies
// ExitInstall only when it reaches FromError already joined behind
// helpers.ErrInstallationFailed, and it is joined there in exactly one
// circumstance - finalizeInstall/warmWithState fold a SaveStore failure in
// through annotateSaveFailure ("%w; snapshot save failed: %w") only when that
// run also recorded at least one per-collection failure (summary.count > 0);
// isInstallError then claims the joined tree, as ExitInstall, the same as
// every other per-collection failure. Every other path returns it bare, and
// therefore unaggregated, classifying ExitNetwork here: every init-time
// operation (LoadStore, LoadProjectRegistry, RecordProject), lockWithState
// and saveDryRunSnapshotIfPersisted (neither of which ever joins a SaveStore
// failure behind anything), and - the case an enumeration of "where" would
// miss - a tail SaveStore failure from finalizeInstall/warmWithState on a run
// that recorded zero collection failures, which is the common case: most
// runs have none. It never classifies as ExitInterrupt, for the identical
// %v-not-%w reason.
//
// Split into two sub-checks purely to stay under the cyclomatic-complexity
// budget; the two together still cover the exact same sentinel set.
func isNetworkError(err error) bool {
	return isTransportError(err) || isMetadataFetchError(err)
}

// isTransportError reports whether err is a request-level network sentinel:
// a timeout, an offline-mode violation, this acquisition's own artifact
// download deadline, a persisted cache-state operation's own deadline, a bare
// download failure, a cache backend that could not be reached or answered
// with a failure that is not this program's own doing
// (helpers.ErrCacheBackendUnavailable), or a server-list walk aborting on a
// credential failure or an exhausted retry budget.
func isTransportError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, helpers.ErrArtifactDownloadDeadline) ||
		errors.Is(err, helpers.ErrStateObjectDeadline) ||
		errors.Is(err, helpers.ErrReadStalled) ||
		errors.Is(err, helpers.ErrOfflineMode) ||
		errors.Is(err, helpers.ErrDownloadFailed) ||
		errors.Is(err, helpers.ErrCacheBackendUnavailable) ||
		errors.Is(err, helpers.ErrGalaxyAuthFailed) ||
		errors.Is(err, helpers.ErrGalaxyServerUnavailable)
}

// isMetadataFetchError reports whether err is a Galaxy metadata-response
// sentinel: metadata that could not be fetched (including one whose fetch
// exceeded its own deadline) or parsed into the shape this tool expects.
func isMetadataFetchError(err error) bool {
	return errors.Is(err, helpers.ErrMetadataUnavailable) ||
		errors.Is(err, helpers.ErrMetadataIsNil) ||
		errors.Is(err, helpers.ErrMissingDownloadURL) ||
		errors.Is(err, helpers.ErrVersionsPayloadEmpty) ||
		errors.Is(err, helpers.ErrVersionsPayloadUnsupported) ||
		errors.Is(err, helpers.ErrMetadataFetchDeadline)
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

// isConfigUsageError reports whether err is a config/environment-level usage
// sentinel. helpers.ErrCacheBackendUnusable belongs here on the same
// predicate as every other member: the remedy is a configuration change and
// no retry can succeed, since the configured backend cannot provide a
// guarantee this tool requires (or cannot be addressed at all) regardless of
// how many times the same request is retried.
func isConfigUsageError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, helpers.ErrConfigIsNil) ||
		errors.Is(err, helpers.ErrS3EmptyCreds) ||
		errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) ||
		errors.Is(err, helpers.ErrCacheDirEmpty) ||
		errors.Is(err, helpers.ErrInvalidTimeout) ||
		errors.Is(err, helpers.ErrAnsibleConfigNotFound) ||
		errors.Is(err, helpers.ErrWarmCacheDisabled) ||
		errors.Is(err, helpers.ErrDryRunUnsupported) ||
		errors.Is(err, helpers.ErrCacheBackendUnusable)
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
