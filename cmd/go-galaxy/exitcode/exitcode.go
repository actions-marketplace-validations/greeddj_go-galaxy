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
	// ExitCacheBusy indicates this run does not have the cache to itself.
	// Three of its producers say the cache could not be acquired in the first
	// place, because something else already holds it, across three shapes: a
	// local process's flock(2) refusing immediately with no wait at all
	// (helpers.ErrAnotherInstanceIsRunning - EWOULDBLOCK from a non-blocking
	// LOCK_EX|LOCK_NB, nothing "reached and answered" since there is no
	// remote party involved), a local Bolt file open timing out against
	// another process's held lock, or an S3 distributed-lock acquisition
	// exhausting its own wait ceiling against a live foreign holder. This is
	// a contention class, distinct from ExitNetwork: the operator's
	// actionable remedy is to retry, possibly after the other holder
	// finishes, rather than to treat it as a dead or misconfigured backend.
	// On the S3 backend an acquisition reaches this class only on positive
	// evidence: the run observed another acquirer holding the lock at least
	// once before its wait ceiling elapsed. A wait that never obtained that
	// evidence classifies as ExitNetwork instead, so an endpoint that
	// answered nothing is never reported as a busy one - see
	// internal/cache/s3/variables.go's partition doc and lock.go's
	// waitCeilingErr for the mechanism and its one disclosed residual.
	//
	// A fourth producer reaches this class from the other side of the same
	// question: helpers.ErrCacheLockLost, a lock this run DID acquire and then
	// had taken away by another holder. It shares the class because it shares
	// the remedy - rerun once nothing else holds the cache - and it
	// SUPERSEDES every other class rather than merely joining them: a run that
	// both lost the lock and failed an integrity check exits 8, not 7. That is
	// not a taxonomy bug. Once another holder is writing the same cache, this
	// run's own verdicts stop being trustworthy on their own terms - the
	// checksum mismatch it reports may be the other holder rewriting an
	// artifact underneath it - so the exclusivity failure is the actionable
	// fact and everything else is evidence for it. The supersession is
	// mechanical, not positional: cacheManager.LockLostError renders the run's
	// own error with %v, flattening its tree so no other class can match it
	// through errors.Is. This class's own position in the exitClasses table
	// carries none of that weight: the reasons it sits where it sits are
	// stated at that table's isCacheBusyError entry, and every one of them is
	// about the acquisition producers.
	ExitCacheBusy = 8
	// ExitCacheCorrupt indicates the persisted cache state itself - a project
	// registry that exists but fails to decode, a state object that could
	// not be read within its declared size ceiling, or (local backend only)
	// a Bolt snapshot file whose bytes fail one of bbolt's own corruption
	// checks - cannot be used by anyone and must be discarded before the run
	// can proceed. The remedy is mechanical and safe to automate: delete the
	// offending object (or the whole cache directory / bucket prefix) or
	// rerun with --clear-cache, then rerun the command.
	//
	// This is deliberately distinct from ExitUsage: helpers.ErrUnsupportedSchemaVersion
	// - a snapshot a newer binary wrote in a shape this one cannot safely
	// interpret - classifies ExitUsage instead of this class, because the
	// snapshot itself is not damaged, only unreadable by this particular
	// reader. Discarding it would destroy a shared cache the newer binary's
	// other runners still depend on, and the actual remedy is an environment
	// change (a newer binary, or pointing at a different cache), which is
	// what ExitUsage means. See isCacheCorruptError and
	// isRecordedStateUsageError below for the two classifiers this split
	// lives in.
	ExitCacheCorrupt = 9
	// ExitSignature indicates a collection's signatures did not satisfy the
	// policy in force: the artifact's bytes may authenticate perfectly against
	// the digest that named them, and this run still could not attribute them
	// to a publisher it was configured to accept.
	//
	// It is deliberately not ExitIntegrity, because the two authenticate
	// different actors: exit 7 says these are not the bytes that were named,
	// exit 10 says nobody this run trusts vouched for them. The remedies part
	// company with the actors - an integrity failure is a stop-and-alert about
	// the artifact or the server that served it, while a signature failure is
	// most often about this run's own keyring or its required-count policy,
	// which is a different CI branch and usually a different team. Folding both
	// into 7 would make that branch impossible to write.
	//
	// helpers.ErrManifestChainMismatch is the deliberate exception and
	// classifies ExitIntegrity rather than here, even though the chain it names
	// is exactly what a collection signature covers: what fails there is a file
	// against the digest MANIFEST.json or FILES.json names for it, which is
	// bytes against a digest - exit 7's own predicate - and no keyring or
	// policy change repairs it.
	ExitSignature = 10
	// ExitInterrupt indicates the run was canceled, either by a caught
	// signal falling back to this default or by context cancellation.
	ExitInterrupt = 130
)

// signalExitBase is added to the numeric signal value to build a
// shell-convention exit code (128 + signal number), per FromSignal.
const signalExitBase = 128

// exitClass pairs one classification predicate with the exit code it yields.
type exitClass struct {
	match func(error) bool
	code  int
}

// exitClasses is the classification precedence FromError walks, top to
// bottom, returning the code of the first entry whose predicate matches it.
// The order is observable behavior rather than an implementation detail - it
// decides the exit code a CI branches on, and a single error tree routinely
// satisfies several of these predicates at once - so it is stated as data in
// one place, and each entry whose position is load-bearing carries the
// argument for that position in place.
//
//nolint:gochecknoglobals // a fixed, immutable ordered table, not mutable shared state.
var exitClasses = []exitClass{
	{match: isCanceled, code: ExitInterrupt},
	// isIntegrityError must be checked before isInstallError: after
	// collections.Start started joining per-collection causes behind
	// helpers.ErrInstallationFailed, every integrity failure's error tree
	// also contains that sentinel, so placing this entry any lower would make
	// it unreachable - isInstallError would already have claimed the error.
	// Cancellation still outranks it.
	{match: isIntegrityError, code: ExitIntegrity},
	// isSignatureError sits below isIntegrityError and above isInstallError,
	// and both bounds are load-bearing. Below isIntegrityError: an error tree
	// carrying both a digest failure and a signature verdict classifies
	// ExitIntegrity, because "these are not the bytes that were named" is the
	// more fundamental fact - who published an artifact is a question that only
	// arises once the artifact is the one it claims to be. Above isInstallError:
	// a verdict raised inside a per-collection worker reaches FromError joined
	// behind helpers.ErrInstallationFailed like every other per-collection
	// cause, so any position below that entry would make this class unreachable
	// for the shape aggregation produces - the identical trap isIntegrityError's
	// own entry above describes for itself. Its position relative to the
	// isLockError entry it now precedes carries no argument of its own: that
	// follows from the two bounds, and a tree carrying both a signature verdict
	// and a lockfile verdict resolves here.
	{match: isSignatureError, code: ExitSignature},
	{match: isLockError, code: ExitLock},
	{match: isInstallError, code: ExitInstall},
	{match: isNetworkError, code: ExitNetwork},
	// isCacheBusyError sits in exactly this one position, for four
	// independent reasons. Not above isCanceled: a Ctrl-C that races a
	// contention failure must still report as ExitInterrupt, not as
	// contention. Not above isIntegrityError/isLockError/isInstallError: if a
	// contention failure is ever folded behind helpers.ErrInstallationFailed
	// by annotateSaveFailure, it must classify ExitInstall like every other
	// per-collection cause, exactly as helpers.ErrStateObjectDeadline already
	// does - placing this entry any higher would invert that rule for
	// contention alone. Not above isNetworkError: a contention verdict
	// arriving joined with a genuine transport failure must keep the wire
	// failure as the actionable cause. Not below isUsageError: isUsageError's
	// fs.ErrNotExist arm is broad enough that any error tree carrying an os
	// path error would satisfy it, which would swallow a contention failure
	// that happens to wrap one.
	{match: isCacheBusyError, code: ExitCacheBusy},
	// isCacheCorruptError sits directly below isCacheBusyError and above
	// isResolutionError. It must stay below the isLockError and
	// isInstallError entries above, for the identical reason
	// isCacheBusyError's own comment gives: if either of this class's
	// sentinels is ever joined behind helpers.ErrInstallationFailed,
	// isInstallError must claim the tree first and classify it ExitInstall,
	// the same per-collection aggregation rule every class in this table
	// already follows. It sits above isResolutionError and isUsageError so
	// that isUsageError's broad fs.ErrNotExist arm can never claim a
	// state-object read that happens to wrap an os-level cause ahead of this
	// more specific verdict - the same defensive posture isCacheBusyError's
	// own position takes against that identical arm.
	{match: isCacheCorruptError, code: ExitCacheCorrupt},
	{match: isResolutionError, code: ExitResolution},
	{match: isUsageError, code: ExitUsage},
}

// FromError classifies err into an exit code by matching it against the known
// sentinel errors declared in internal/galaxy/helpers. A nil error is ExitOK;
// otherwise exitClasses is walked top to bottom and the first class whose
// predicate matches wins, with an error no class recognizes falling back to
// ExitError. The priority order is that table's own order - cancellation,
// integrity, signature, lock, install, network, cache contention, cache
// corruption, resolution, then usage/config - and the reason a class sits
// where it sits is stated on its entry there.
func FromError(err error) int {
	if err == nil {
		return ExitOK
	}
	for _, class := range exitClasses {
		if class.match(err) {
			return class.code
		}
	}
	return ExitError
}

// isCanceled reports whether err carries a caller's own cancellation.
//
// Classifying it ahead of every other class is correct only because no
// sentinel this program raises to describe why work ended leaves a context
// sentinel reachable through errors.Is - see helpers.ErrReadStalled's and
// helpers.ErrArtifactDownloadDeadline's doc comments. A future sentinel that
// wraps a context.Canceled/context.DeadlineExceeded cause with %w would
// silently steal this class instead of being caught by it, since this class
// is checked first and cannot distinguish "the sentinel's cause happens to be
// a context error" from "the caller genuinely canceled".
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// isCacheCorruptError reports whether err says the persisted cache state
// itself - not this reader's ability to interpret it, and not a backend's
// ability to reach it - cannot be used by anyone and must be discarded
// before the run can proceed: a project registry that exists but fails to
// decode (helpers.ErrCorruptProjectRegistry), a state object that could not
// be read within its declared size ceiling (helpers.ErrStateObjectTooLarge),
// or the local Bolt snapshot file itself failing one of bbolt's own
// corruption checks (helpers.ErrCorruptSnapshotStore - see openBolt in
// internal/galaxy/store for the closed set of bbolt sentinels this maps
// from). helpers.ErrUnsupportedSchemaVersion is deliberately not a member: a
// schema version newer than this binary understands means the snapshot was
// written correctly by a newer binary, not that its bytes are damaged, so
// isRecordedStateUsageError classifies it instead - see that function's own
// doc comment for the flip side of this distinction. This class is scoped to
// the local backend only for helpers.ErrCorruptSnapshotStore specifically:
// the S3 backend's state object is gzipped JSON with its own sentinels
// (helpers.ErrCorruptProjectRegistry, helpers.ErrStateObjectTooLarge), so it
// reaches this class through those two instead, never through
// helpers.ErrCorruptSnapshotStore, which only ever originates from opening a
// local Bolt file.
//
// No error tree produced by this program carries both this class and the
// network class today: a size-ceiling failure is only ever detected after
// its GET has already answered with a 200 (Client.getObject returns a
// response to readObject only on that status; every other status or
// transport failure returns an error before readAllCapped ever runs), and
// neither a decode failure nor a local Bolt file open ever touches the
// network at all. No member of this class carries a
// context.DeadlineExceeded/context.Canceled cause either, so
// cache.deadlineError (the StateObjectDeadline decorator's normalizer) never
// relabels one into helpers.ErrStateObjectDeadline - its own precondition
// requires the error being normalized to already carry one of those two
// signals, which none of a size-cap rejection, a corrupt-registry decode
// error, or a local Bolt open failure ever does.
func isCacheCorruptError(err error) bool {
	return errors.Is(err, helpers.ErrCorruptProjectRegistry) ||
		errors.Is(err, helpers.ErrStateObjectTooLarge) ||
		errors.Is(err, helpers.ErrCorruptSnapshotStore)
}

// isIntegrityError reports whether err is an artifact-digest authentication
// failure: content that did not hash to the digest that named it, or a value
// that was supposed to be such a digest and was not.
//
// helpers.ErrManifestChainMismatch is a member on that predicate rather than
// by association with the check it belongs to. It names content that did not
// match a digest naming it - FILES.json against MANIFEST.json's pointer, or a
// listed file against FILES.json's - which is this class's own question,
// whatever a signature check wrapped around it concluded; isSignatureError's
// one member asks who vouched for the bytes instead.
func isIntegrityError(err error) bool {
	return errors.Is(err, helpers.ErrSHA256Mismatch) ||
		errors.Is(err, helpers.ErrMalformedArtifactSHA256) ||
		errors.Is(err, helpers.ErrManifestChainMismatch)
}

// isSignatureError reports whether err carries one collection's signature
// verdict: the signatures in hand did not satisfy the policy in force. It
// matches helpers.ErrSignatureVerificationFailed and nothing else, and that is
// the whole class rather than a sample of it - every other signature-related
// sentinel classifies elsewhere, on its own predicate rather than by
// association. A source that could not be fetched, or a fetch budget that
// expired, is a wire failure (isSignatureTransportError); a keyring or a
// policy value this tool refuses is a configuration failure
// (isSignatureConfigError); an artifact naming no manifest is an
// artifact-shape failure (isArtifactShapeError); and a broken manifest chain
// is bytes against a digest (isIntegrityError).
func isSignatureError(err error) bool {
	return errors.Is(err, helpers.ErrSignatureVerificationFailed)
}

// isLockError reports whether err is a lockfile-related sentinel:
// helpers.ErrLockfileMismatch (a lockfile that does not cover the
// requirements roots), helpers.ErrLockfileMissing, helpers.ErrLockfileInvalid,
// or helpers.ErrLockfileDrift - lock --frozen's own verdict that a fresh
// resolve disagrees with the lockfile already on disk. The last one is kept
// distinct from ErrLockfileMismatch even though both name a lockfile that
// disagrees with reality, because they describe different disagreements
// (missing coverage vs. stale content) - matching the precedent that keeps
// ErrMalformedArtifactSHA256 and ErrSHA256Mismatch apart under one exit
// class rather than folding them into a single sentinel.
func isLockError(err error) bool {
	return errors.Is(err, helpers.ErrLockfileMismatch) ||
		errors.Is(err, helpers.ErrLockfileMissing) ||
		errors.Is(err, helpers.ErrLockfileInvalid) ||
		errors.Is(err, helpers.ErrLockfileDrift)
}

// isCacheBusyError reports whether err says this run does not have the cache
// to itself, in either of the two ways that can be true: the lock was refused
// because another holder had it (helpers.ErrCacheBusy past a backend's own
// wait ceiling, or helpers.ErrAnotherInstanceIsRunning with no wait at all),
// or the lock was granted and then taken away mid-run
// (helpers.ErrCacheLockLost). Both share one remedy - rerun once nothing else
// holds the cache - which is what makes them one exit class rather than two.
func isCacheBusyError(err error) bool {
	return errors.Is(err, helpers.ErrCacheBusy) ||
		errors.Is(err, helpers.ErrAnotherInstanceIsRunning) ||
		errors.Is(err, helpers.ErrCacheLockLost)
}

// isInstallError reports whether err is an install-time sentinel (unsafe
// archive/symlink content, bytes that are not an archive at all, an empty
// file, or a missing artifact cache). Split into sub-checks purely to stay
// under the cyclomatic-complexity budget; together they still cover the exact
// same sentinel set.
func isInstallError(err error) bool {
	return isFileIntegrityError(err) || isArchiveError(err) ||
		isSymlinkError(err) || isArtifactShapeError(err)
}

// isArtifactShapeError reports whether err says what arrived does not have the
// outer shape of a collection artifact: it is not a gzip-compressed tar at all
// (helpers.ErrArtifactNotTarGz, from the download path's shape probe), or it is
// one that names no MANIFEST.json within its scan bound
// (helpers.ErrManifestNotFound). It is its own predicate rather than a member
// of isArchiveError because it answers a different question: every sentinel
// there is raised by the extractor about an archive's contents, while these
// two are raised outside it about whether there is a collection artifact to
// speak of. They classify alongside them, and never as a transport failure -
// the transfer succeeded, and no retry turns an error page into an archive or
// puts a manifest into one that has none.
func isArtifactShapeError(err error) bool {
	return errors.Is(err, helpers.ErrArtifactNotTarGz) ||
		errors.Is(err, helpers.ErrManifestNotFound)
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
// helpers.ErrArchiveTooManyEntries and helpers.ErrArchiveDuplicateEntry are
// listed here alongside every other archive sentinel for consistency, not
// because either changes classification: both are raised only from inside
// the archive extractor, which is reached only from a per-collection install
// or warm worker (extractCollection's unpack, directly or through the
// extracted store's ingest, and warmVerifyAndEnsure on the warm path), so
// both always reach isInstallError already joined behind
// helpers.ErrInstallationFailed - isFileIntegrityError's match on
// that headline alone already classifies the tree ExitInstall, via
// short-circuit evaluation, without isInstallError ever calling into this
// function for it.
func isArchiveError(err error) bool {
	return errors.Is(err, helpers.ErrArchivePathContainsSymlinkComponent) ||
		errors.Is(err, helpers.ErrArchiveExceedsMaxSize) ||
		errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) ||
		errors.Is(err, helpers.ErrArchiveEntryHasNegativeSize) ||
		errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) ||
		errors.Is(err, helpers.ErrArchiveEntryEscapesDestination) ||
		errors.Is(err, helpers.ErrArchiveEntryIsAbsolutePath) ||
		errors.Is(err, helpers.ErrArchiveEntryHasEmptyName) ||
		errors.Is(err, helpers.ErrArchiveTooManyEntries) ||
		errors.Is(err, helpers.ErrArchiveDuplicateEntry)
}

// isSymlinkError reports whether err is an unsafe-symlink sentinel. This
// group covers both an unsafe symlink found inside an extracted archive
// (the original set below) and an unsafe symlink found in the destination
// tree itself - a component of cfg.DownloadPath, most dangerously
// ansible_collections or a namespace/name directory beneath it, that
// resolves outside the collections root os.Root enforces.
// helpers.ErrUnsafeRemovalPath sits here too: cleanup's removeInstalled
// raises it for the identical meaning on the delete side - a computed
// removal path that failed a containment check against its expected root -
// so an install-side escape and a cleanup-side one classify identically.
func isSymlinkError(err error) bool {
	return errors.Is(err, helpers.ErrSymlinkTargetResolvesToSelf) ||
		errors.Is(err, helpers.ErrSymlinkTargetEscapesDestination) ||
		errors.Is(err, helpers.ErrSymlinkTarget) ||
		errors.Is(err, helpers.ErrSymlinkTargetResolvesToRoot) ||
		errors.Is(err, helpers.ErrSymlinkTargetIsAbsolute) ||
		errors.Is(err, helpers.ErrSymlinkTargetIsEmpty) ||
		errors.Is(err, helpers.ErrCollectionsPathEscape) ||
		errors.Is(err, helpers.ErrUnsafeRemovalPath)
}

// isNetworkError reports whether err is a network or Galaxy API sentinel,
// including request timeouts, offline-mode violations, and a server-list
// walk aborting on a credential failure or an exhausted retry budget.
// helpers.ErrArtifactDownloadDeadline classifies here, unaggregated, as
// ExitNetwork; once collections.Start joins it behind
// helpers.ErrInstallationFailed the isInstallError entry above claims it
// first, as ExitInstall - identical to every other per-collection failure,
// helpers.ErrDownloadFailed included. It
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
// Split into three sub-checks purely to stay under the cyclomatic-complexity
// budget; the three together still cover the exact same sentinel set.
func isNetworkError(err error) bool {
	return isTransportError(err) || isMetadataFetchError(err) || isSignatureTransportError(err)
}

// isSignatureTransportError reports whether err says a signature could not be
// obtained over the wire: the source was unreachable, unreadable, or refused
// by offline mode (helpers.ErrSignatureSourceUnavailable), or the collection's
// whole signature phase overran helpers.SignatureFetchDeadline. Both classify
// exactly like their siblings in isTransportError - ExitNetwork unaggregated,
// ExitInstall once joined behind helpers.ErrInstallationFailed by a
// per-collection worker - and neither ever classifies ExitInterrupt: the
// deadline sentinel's own doc comment binds its producer to render the context
// cause with %v rather than wrap it with %w, so no context sentinel stays
// reachable through errors.Is.
//
// It is a sibling of isTransportError rather than two more lines inside it
// purely to stay under the cyclomatic-complexity budget, the same reason
// isNetworkError was split in the first place. Neither of these two is a
// signature VERDICT: nothing was verified, so isSignatureError deliberately
// does not match them and this class is where they belong.
func isSignatureTransportError(err error) bool {
	return errors.Is(err, helpers.ErrSignatureSourceUnavailable) ||
		errors.Is(err, helpers.ErrSignatureFetchDeadline)
}

// isTransportError reports whether err is a request-level network sentinel:
// a timeout, an offline-mode violation, this acquisition's own artifact
// download deadline, a persisted cache-state operation's own deadline, a bare
// download failure, a cache backend that could not be reached or answered
// with a failure that is not this program's own doing
// (helpers.ErrCacheBackendUnavailable), a server-list walk aborting on a
// credential failure or an exhausted retry budget, or any capped response
// body that overran its ceiling before it finished streaming
// (helpers.ErrResponseTooLarge - helpers.NewSizeLimitedReader raises it for
// an artifact download, a Galaxy metadata document over MetadataMaxSize, and
// an S3 list or batch-delete response over S3ListMaxSize alike). The one
// capped body that does not land here is a persisted cache-state object,
// which carries helpers.ErrStateObjectTooLarge instead, since a state object
// this program cannot read is a corrupt-cache condition rather than a
// network one. The size-ceiling class is deliberately not
// ExitIntegrity: it is a size ceiling, not a digest that failed to match -
// the same shape as the deadline sentinels above it, unaggregated here and
// ExitInstall once joined behind helpers.ErrInstallationFailed by
// isFileIntegrityError's match on that headline, identical to every other
// per-collection cause.
func isTransportError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, helpers.ErrArtifactDownloadDeadline) ||
		errors.Is(err, helpers.ErrStateObjectDeadline) ||
		errors.Is(err, helpers.ErrReadStalled) ||
		errors.Is(err, helpers.ErrOfflineMode) ||
		errors.Is(err, helpers.ErrDownloadFailed) ||
		errors.Is(err, helpers.ErrCacheBackendUnavailable) ||
		errors.Is(err, helpers.ErrGalaxyAuthFailed) ||
		errors.Is(err, helpers.ErrGalaxyServerUnavailable) ||
		errors.Is(err, helpers.ErrResponseTooLarge)
}

// isMetadataFetchError reports whether err is a Galaxy metadata-response
// sentinel: metadata that could not be fetched (including one whose fetch
// exceeded its own deadline, or whose versions list kept reporting more
// pages than the page ceiling allows) or parsed into the shape this tool
// expects. helpers.ErrLatestVersionLookupFailed belongs here on the same
// basis as every other member: `outdated`'s own per-entry lookup is itself a
// root-metadata fetch, so the headline's bare, unaggregated shape - no cause
// joined behind it - names a metadata-fetch failure.
//
// helpers.ErrUnsupportedDownloadURLScheme belongs here for the same reason
// helpers.ErrMissingDownloadURL does: metadata that parsed into the expected
// shape and still cannot yield a fetchable artifact is the same defect as
// metadata carrying no download URL at all, so it classifies alike rather
// than earning an exit class of its own.
//
// helpers.ErrLatestVersionLookupFailed is also an aggregation headline whose
// per-entry causes are joined behind it via errors.Join, exactly like
// helpers.ErrInstallationFailed elsewhere in this package - so this function
// matching the bare headline is not the last word once a cause is joined
// in. Whenever a joined cause must classify differently, the rule is a
// predicate on the error tree, not an enumeration of which command produced
// it: a lockfile entry whose name is not a "namespace.name" FQDN carries
// helpers.ErrLockfileInvalid instead, and isLockError is checked ahead of
// isNetworkError in the exitClasses table, so errors.Is walking the joined
// tree lets isLockError claim it first regardless of what else is joined
// alongside it.
func isMetadataFetchError(err error) bool {
	return errors.Is(err, helpers.ErrMetadataUnavailable) ||
		errors.Is(err, helpers.ErrMetadataIsNil) ||
		errors.Is(err, helpers.ErrMissingDownloadURL) ||
		errors.Is(err, helpers.ErrUnsupportedDownloadURLScheme) ||
		errors.Is(err, helpers.ErrVersionsPayloadEmpty) ||
		errors.Is(err, helpers.ErrVersionsPayloadUnsupported) ||
		errors.Is(err, helpers.ErrVersionsPagingExceeded) ||
		errors.Is(err, helpers.ErrMetadataFetchDeadline) ||
		errors.Is(err, helpers.ErrLatestVersionLookupFailed)
}

// isResolutionError reports whether err is a dependency-resolution sentinel
// (conflicting constraints, missing candidates, a cycle in the graph, or a
// dependency map key that is not a valid "namespace.name" FQDN). The last
// one, helpers.ErrInvalidDependencyKey, is malformed graph input discovered
// while walking a collection's declared dependencies - no retry repairs it,
// the same reasoning that makes every other member of this class a
// resolution failure rather than a network or install one.
func isResolutionError(err error) bool {
	return errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) ||
		errors.Is(err, helpers.ErrConflictingRootConstraints) ||
		errors.Is(err, helpers.ErrConflictingExactVersions) ||
		errors.Is(err, helpers.ErrDependencyGraphHasACycle) ||
		errors.Is(err, helpers.ErrNoSemverCandidates) ||
		errors.Is(err, helpers.ErrMissingResolvedParent) ||
		errors.Is(err, helpers.ErrMissingResolvedDependency) ||
		errors.Is(err, helpers.ErrMissingResolvedRoot) ||
		errors.Is(err, helpers.ErrLoadMetadataFailed) ||
		errors.Is(err, helpers.ErrInvalidDependencyKey)
}

// isUsageError reports whether err is a configuration or CLI-input sentinel
// (invalid flags, malformed requirements, or a missing path). Split into
// five sub-checks purely to stay under the cyclomatic-complexity budget;
// the five together still cover the exact same sentinel set.
func isUsageError(err error) bool {
	return isConfigUsageError(err) ||
		isGalaxyServerConfigError(err) ||
		isCollectionNameUsageError(err) ||
		isCollectionListUsageError(err) ||
		isSignatureConfigError(err)
}

// isSignatureConfigError reports whether err says this run's signature
// configuration cannot be used as given: a source naming a scheme this tool
// does not fetch, a keyring it cannot read or whose container format it does
// not open, a requirements file declaring signatures with no keyring
// configured, or a required-count or ignored-status-code value it does not
// accept. The predicate every member shares is the one every other usage
// sentinel shares: an operator has to change something - a flag, an
// environment value, ansible.cfg, or the requirements file - and no retry
// repairs it.
//
// None of them is a signature verdict, which is what keeps them out of
// isSignatureError and its exit class: a run that hits one of these never
// checked a signature at all, so reporting it as a verification failure would
// tell a pipeline the artifact was rejected when the configuration was.
func isSignatureConfigError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedSignatureSource) ||
		errors.Is(err, helpers.ErrKeyringUnreadable) ||
		errors.Is(err, helpers.ErrKeyringIsKeybox) ||
		errors.Is(err, helpers.ErrKeyringRequired) ||
		errors.Is(err, helpers.ErrInvalidSignatureCount) ||
		errors.Is(err, helpers.ErrUnknownSignatureStatusCode)
}

// isConfigUsageError reports whether err is a config/environment-level usage
// sentinel or a recorded-state usage sentinel. Split into two sub-checks
// purely to stay under the cyclomatic-complexity budget; the two together
// still cover the exact same sentinel set.
func isConfigUsageError(err error) bool {
	return isEnvironmentUsageError(err) || isRecordedStateUsageError(err)
}

// isEnvironmentUsageError reports whether err is a config/environment-level
// usage sentinel. helpers.ErrCacheBackendUnusable belongs here on the same
// predicate as every other member: the remedy is a configuration change and
// no retry can succeed, since the configured backend cannot provide a
// guarantee this tool requires (or cannot be addressed at all) regardless of
// how many times the same request is retried.
func isEnvironmentUsageError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, helpers.ErrConfigIsNil) ||
		errors.Is(err, helpers.ErrS3EmptyCreds) ||
		errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) ||
		errors.Is(err, helpers.ErrCacheDirEmpty) ||
		errors.Is(err, helpers.ErrInvalidTimeout) ||
		errors.Is(err, helpers.ErrInvalidWorkers) ||
		errors.Is(err, helpers.ErrAnsibleConfigNotFound) ||
		errors.Is(err, helpers.ErrWarmCacheDisabled) ||
		errors.Is(err, helpers.ErrCacheBackendUnusable)
}

// isRecordedStateUsageError reports whether err is a usage sentinel about
// something this program persisted or tracks on a caller's behalf and cannot
// use as recorded, where the fix is an operator action, never a retry.
//
// helpers.ErrUnsupportedSchemaVersion belongs here rather than in
// isCacheCorruptError for the identical reason that function's own doc
// comment states from the other side: a schema version newer than this
// binary understands describes this reader, not the snapshot - the object
// was written correctly by a newer binary this one cannot safely interpret,
// so the remedy is an environment change (a newer binary, or a different
// cache), not discarding data a newer runner still depends on.
//
// helpers.ErrProjectRequirementsUnreadable belongs here on the same
// predicate: a recorded project's requirements file this program cannot
// load, whatever the underlying cause, is something an operator must fix by
// hand - editing or restoring the file - not something retrying the same
// run can repair. A load failure matching errors.Is(err, fs.ErrNotExist) is
// not this sentinel's concern at all: cleanup treats a recorded
// requirements file that fails to load that way as a stale registry entry -
// a single warning, contributing no reachability roots - rather than a load
// failure, so that case never reaches FromError as an error in the first
// place. This sentinel, and therefore this exit code, is reserved for any
// other load failure.
func isRecordedStateUsageError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedSchemaVersion) ||
		errors.Is(err, helpers.ErrProjectRequirementsUnreadable)
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

// isCollectionNameUsageError reports whether err is an invalid-collection-name/format
// sentinel. helpers.ErrConflictingNamespaceName joins this class for the
// same reason: an explicit namespace given alongside a dotted collection
// name is a malformed requirements.yml entry, raised while parsing
// requirements before any resolution or network work starts, exactly like
// every other member here.
func isCollectionNameUsageError(err error) bool {
	return errors.Is(err, helpers.ErrEmptyCollectionName) ||
		errors.Is(err, helpers.ErrInvalidCollectionName) ||
		errors.Is(err, helpers.ErrInvalidCollectionKey) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionSource) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionType) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionFormat) ||
		errors.Is(err, helpers.ErrConflictingNamespaceName)
}

// isCollectionListUsageError reports whether err is an invalid-collections-list
// sentinel. helpers.ErrUnsafeCollectionIdentifier and
// helpers.ErrInvalidCollectionVersion sit here alongside
// ErrDuplicateCollectionKey: buildCollectionsMap raises all three while
// folding a resolved set into a key-addressed identity map, before any
// install or warm work starts, over malformed resolution input rather than
// anything the network or the install pipeline did.
// helpers.ErrInvalidCollectionVersion has a second producer with the
// identical shape but a different caller: buildLockfile
// (internal/galaxy/collections/lock.go) raises it while assembling a
// lockfile from an already-resolved map, checked before that entry's own
// metadata fetch runs. Both producers run before any of their command's own
// per-collection work begins - a worker in install/warm's case, buildLockfile's
// own iteration in lock's case, which has no worker pool at all - which is
// what keeps the sentinel out of helpers.ErrInstallationFailed's aggregation
// and therefore out of isInstallError above, checked earlier in the
// exitClasses table.
func isCollectionListUsageError(err error) bool {
	return errors.Is(err, helpers.ErrInvalidCollectionsList) ||
		errors.Is(err, helpers.ErrInvalidCollectionEntry) ||
		errors.Is(err, helpers.ErrMissingCollection) ||
		errors.Is(err, helpers.ErrDuplicateCollectionRequirement) ||
		errors.Is(err, helpers.ErrDuplicateCollectionKey) ||
		errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) ||
		errors.Is(err, helpers.ErrInvalidCollectionVersion)
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
