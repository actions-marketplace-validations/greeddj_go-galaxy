package exitcode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// errTestGeneric is an unrecognized sentinel used to exercise the default
// ExitError fallback in TestFromError.
var errTestGeneric = errors.New("some unclassified error")

// errTestSaveFailure stands in for a snapshot-save failure in
// TestSaveFailureDoesNotMaskIntegrity. Declared as a static package-level
// sentinel, rather than an inline errors.New call, purely to satisfy err113 -
// production code never compares against it.
var errTestSaveFailure = errors.New("simulated save failure")

// exitCase is one FromError classification expectation.
type exitCase struct {
	err      error
	name     string
	wantCode int
}

// fromErrorCases is TestFromError's table, hoisted to package level so the
// test function itself stays within the complexity budget as classes are
// added. It checks one representative wrapped error per exit class, plus
// the nil/context/fs.ErrNotExist/unclassified edge cases; wrapping with
// fmt.Errorf("%w: ctx", sentinel) verifies FromError matches through
// errors.Is rather than requiring exact identity.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var fromErrorCases = []exitCase{
	{name: "nil error", err: nil, wantCode: ExitOK},
	{
		name:     "context canceled",
		err:      fmt.Errorf("%w: ctx", context.Canceled),
		wantCode: ExitInterrupt,
	},
	{
		name:     "lockfile mismatch",
		err:      fmt.Errorf("%w: ctx", helpers.ErrLockfileMismatch),
		wantCode: ExitLock,
	},
	{
		name:     "installation failed",
		err:      fmt.Errorf("%w: ctx", helpers.ErrInstallationFailed),
		wantCode: ExitInstall,
	},
	{
		name:     "artifact sha256 mismatch",
		err:      fmt.Errorf("%w: ctx", helpers.ErrSHA256Mismatch),
		wantCode: ExitIntegrity,
	},
	{
		// ErrCollectionsPathEscape is grouped with the archive/symlink sentinels
		// in isSymlinkError, since a symlinked ansible_collections (or a
		// namespace/name component beneath it) is the same class of unsafe
		// filesystem write as an unsafe symlink found inside an extracted
		// archive - both must exit ExitInstall, not the generic fallback.
		name:     "collections path escape",
		err:      fmt.Errorf("%w: ctx", helpers.ErrCollectionsPathEscape),
		wantCode: ExitInstall,
	},
	{
		name:     "context deadline exceeded",
		err:      fmt.Errorf("%w: ctx", context.DeadlineExceeded),
		wantCode: ExitNetwork,
	},
	{
		name:     "download failed",
		err:      fmt.Errorf("%w: ctx", helpers.ErrDownloadFailed),
		wantCode: ExitNetwork,
	},
	{
		name:     "galaxy server auth failed",
		err:      fmt.Errorf("%w: server a: ctx", helpers.ErrGalaxyAuthFailed),
		wantCode: ExitNetwork,
	},
	{
		name:     "galaxy server unavailable",
		err:      fmt.Errorf("%w: server a: ctx", helpers.ErrGalaxyServerUnavailable),
		wantCode: ExitNetwork,
	},
	{
		name:     "dependency graph has a cycle",
		err:      fmt.Errorf("%w: ctx", helpers.ErrDependencyGraphHasACycle),
		wantCode: ExitResolution,
	},
	{
		name:     "missing resolved root",
		err:      fmt.Errorf("%w: ctx", helpers.ErrMissingResolvedRoot),
		wantCode: ExitResolution,
	},
	{
		name:     "fs.ErrNotExist",
		err:      fmt.Errorf("%w: ctx", fs.ErrNotExist),
		wantCode: ExitUsage,
	},
	{
		name:     "invalid collection key",
		err:      fmt.Errorf("%w: ctx", helpers.ErrInvalidCollectionKey),
		wantCode: ExitUsage,
	},
	{
		// buildCollectionsMap raises this over a malformed resolved identity
		// (ns/name/version) before any install work starts, the same
		// plan-build-time class as ErrDuplicateCollectionKey right below it in
		// isCollectionListUsageError - not an install-time or a network failure.
		name:     "unsafe collection identifier",
		err:      fmt.Errorf("%w: ctx", helpers.ErrUnsafeCollectionIdentifier),
		wantCode: ExitUsage,
	},
	{
		name:     "duplicate collection key",
		err:      fmt.Errorf("%w: ctx", helpers.ErrDuplicateCollectionKey),
		wantCode: ExitUsage,
	},
	{
		name:     "invalid timeout",
		err:      fmt.Errorf("%w: ctx", helpers.ErrInvalidTimeout),
		wantCode: ExitUsage,
	},
	{
		name:     "ansible config not found",
		err:      fmt.Errorf("%w: ctx", helpers.ErrAnsibleConfigNotFound),
		wantCode: ExitUsage,
	},
	{
		name:     "warm cache disabled",
		err:      fmt.Errorf("%w: ctx", helpers.ErrWarmCacheDisabled),
		wantCode: ExitUsage,
	},
	{
		name:     "dry-run unsupported",
		err:      fmt.Errorf("%w: ctx", helpers.ErrDryRunUnsupported),
		wantCode: ExitUsage,
	},
	{
		name:     "unclassified error falls back to ExitError",
		err:      errTestGeneric,
		wantCode: ExitError,
	},
	{
		name:     "bare artifact download deadline",
		err:      helpers.ErrArtifactDownloadDeadline,
		wantCode: ExitNetwork,
	},
	{
		// The real shape downloadCollectionToCache/fetchArtifact produce:
		// helpers.ErrArtifactDownloadDeadline deliberately does not wrap its
		// context.DeadlineExceeded cause with %w (see the sentinel's own doc
		// comment), so this must classify as ExitNetwork through the
		// sentinel match alone, never as ExitInterrupt via a reachable
		// context.Canceled/context.DeadlineExceeded.
		name: "artifact download deadline wraps its cause with %v, not %w",
		// Pinning the real, deliberately non-wrapping shape; see
		// helpers.ErrArtifactDownloadDeadline's own doc comment for why.
		err:      fmt.Errorf("%w after 15m0s: %v", helpers.ErrArtifactDownloadDeadline, context.DeadlineExceeded), //nolint:errorlint
		wantCode: ExitNetwork,
	},
	{
		name:     "read stalled",
		err:      fmt.Errorf("%w: ctx", helpers.ErrReadStalled),
		wantCode: ExitNetwork,
	},
}

// TestFromError walks fromErrorCases, checking one representative error per
// exit class.
func TestFromError(t *testing.T) {
	for _, tt := range fromErrorCases {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromError(tt.err); got != tt.wantCode {
				t.Errorf("FromError(%v) = %d, want %d", tt.err, got, tt.wantCode)
			}
		})
	}
}

// galaxyServerConfigSentinels is every Galaxy server configuration sentinel
// this package must classify as ExitUsage. It is listed exhaustively rather
// than sampled: each one is raised only while building the config, and a
// missed entry would silently exit 1 - indistinguishable to a CI pipeline
// from a genuine runtime failure it should retry.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var galaxyServerConfigSentinels = []struct {
	err  error
	name string
}{
	{name: "unsupported galaxy_server key", err: helpers.ErrUnsupportedGalaxyServerKey},
	{name: "unsupported api_version", err: helpers.ErrUnsupportedGalaxyServerAPIVersion},
	{name: "missing server url", err: helpers.ErrMissingGalaxyServerURL},
	{name: "invalid server url", err: helpers.ErrInvalidGalaxyServerURL},
	{name: "invalid server id", err: helpers.ErrInvalidGalaxyServerID},
	{name: "duplicate server id", err: helpers.ErrDuplicateGalaxyServerID},
	{name: "invalid validate_certs", err: helpers.ErrInvalidValidateCerts},
	{name: "server url with userinfo", err: helpers.ErrGalaxyServerURLUserinfo},
	{name: "token over insecure transport", err: helpers.ErrInsecureTokenTransport},
	{name: "conflicting tls policy", err: helpers.ErrConflictingServerTLSPolicy},
	{name: "conflicting token", err: helpers.ErrConflictingServerToken},
	{name: "ambiguous --token", err: helpers.ErrAmbiguousGalaxyToken},
}

// TestGalaxyServerConfigErrorsMapToUsage pins every Galaxy server
// configuration sentinel to ExitUsage. These are raised before any request
// is made, so a pipeline that branches on the exit code must be able to
// tell "your ansible.cfg is wrong, editing it is the only fix" apart from
// a transient failure worth retrying.
func TestGalaxyServerConfigErrorsMapToUsage(t *testing.T) {
	for _, tt := range galaxyServerConfigSentinels {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := fmt.Errorf("%w: ctx", tt.err)
			if got := FromError(wrapped); got != ExitUsage {
				t.Errorf("FromError(%v) = %d, want %d", wrapped, got, ExitUsage)
			}
		})
	}
}

// integritySentinels is every artifact-digest-authentication sentinel this
// package must classify as ExitIntegrity. Listed exhaustively, not sampled,
// mirroring galaxyServerConfigSentinels's own convention: a missed entry
// would silently fall back to a different exit class, indistinguishable to a
// CI pipeline from a class where retrying the same run might actually help.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var integritySentinels = []struct {
	err  error
	name string
}{
	{name: "sha256 mismatch", err: helpers.ErrSHA256Mismatch},
	{name: "malformed artifact sha256", err: helpers.ErrMalformedArtifactSHA256},
}

// TestIntegritySentinelsMapToExitIntegrity pins every artifact-digest
// sentinel to ExitIntegrity, wrapped so the check goes through errors.Is
// rather than requiring exact identity.
func TestIntegritySentinelsMapToExitIntegrity(t *testing.T) {
	for _, tt := range integritySentinels {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := fmt.Errorf("%w: ctx", tt.err)
			if got := FromError(wrapped); got != ExitIntegrity {
				t.Errorf("FromError(%v) = %d, want %d", wrapped, got, ExitIntegrity)
			}
		})
	}
}

// TestIntegrityOutranksInstallFailureHeadline proves that once
// collections.Start starts joining a per-collection integrity cause behind
// the helpers.ErrInstallationFailed headline, FromError still classifies the
// combined tree as ExitIntegrity rather than stopping at the headline it
// finds first in the tree. The positive control in the same test - the
// identical headline joined with helpers.ErrDownloadFailed instead - proves
// this is not just "any joined error becomes ExitIntegrity": only the
// presence of an actual integrity sentinel does. The killing mutation is
// moving the isIntegrityError case below isLockError/isInstallError in
// FromError, which makes isInstallError claim the headline first; verified,
// that mutation makes this test fail with:
// "exitcode_test.go:246: FromError(integrity join) = 5, want 7".
func TestIntegrityOutranksInstallFailureHeadline(t *testing.T) {
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)

	integrity := errors.Join(headline, fmt.Errorf("%w: acme.app@1.0.0", helpers.ErrSHA256Mismatch))
	if got := FromError(integrity); got != ExitIntegrity {
		t.Errorf("FromError(integrity join) = %d, want %d", got, ExitIntegrity)
	}

	notIntegrity := errors.Join(headline, helpers.ErrDownloadFailed)
	if got := FromError(notIntegrity); got != ExitInstall {
		t.Errorf("FromError(non-integrity join) = %d, want %d", got, ExitInstall)
	}
}

// TestIntegrityOutranksNetworkCause proves the same precedence holds when the
// joined tree also contains a network-class sentinel: the integrity cause
// still wins.
func TestIntegrityOutranksNetworkCause(t *testing.T) {
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joined := errors.Join(headline, helpers.ErrDownloadFailed, helpers.ErrSHA256Mismatch)
	if got := FromError(joined); got != ExitIntegrity {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitIntegrity)
	}
}

// TestIntegrityPrecedenceIndependentOfCauseOrder proves the ExitIntegrity
// classification does not depend on where, in the joined tree, the integrity
// sentinel sits - both orderings of the same three causes, and a nested join
// of them, all classify identically.
func TestIntegrityPrecedenceIndependentOfCauseOrder(t *testing.T) {
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)

	forward := errors.Join(headline, helpers.ErrDownloadFailed, helpers.ErrSHA256Mismatch)
	if got := FromError(forward); got != ExitIntegrity {
		t.Errorf("FromError(forward order) = %d, want %d", got, ExitIntegrity)
	}

	reversed := errors.Join(headline, helpers.ErrSHA256Mismatch, helpers.ErrDownloadFailed)
	if got := FromError(reversed); got != ExitIntegrity {
		t.Errorf("FromError(reversed order) = %d, want %d", got, ExitIntegrity)
	}

	nested := errors.Join(headline, errors.Join(helpers.ErrDownloadFailed, helpers.ErrSHA256Mismatch))
	if got := FromError(nested); got != ExitIntegrity {
		t.Errorf("FromError(nested) = %d, want %d", got, ExitIntegrity)
	}
}

// TestSaveFailureDoesNotMaskIntegrity proves the annotateSaveFailure-shaped
// wrap collections.Start builds ("%w; snapshot save failed: %w") still
// classifies as ExitIntegrity when its primary side already carries an
// integrity cause, matching the real shape a frozen install with a corrupted
// pin and a failing snapshot save would produce.
func TestSaveFailureDoesNotMaskIntegrity(t *testing.T) {
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joinedInstall := errors.Join(headline, helpers.ErrSHA256Mismatch)

	err := fmt.Errorf("%w; snapshot save failed: %w", joinedInstall, errTestSaveFailure)
	if got := FromError(err); got != ExitIntegrity {
		t.Errorf("FromError(err) = %d, want %d", got, ExitIntegrity)
	}
}

// TestCanceledOutranksIntegrity proves context.Canceled still outranks
// ExitIntegrity, matching FromError's documented priority order: cancellation
// first, integrity second.
func TestCanceledOutranksIntegrity(t *testing.T) {
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	integrity := errors.Join(headline, helpers.ErrSHA256Mismatch)

	err := errors.Join(integrity, context.Canceled)
	if got := FromError(err); got != ExitInterrupt {
		t.Errorf("FromError(err) = %d, want %d", got, ExitInterrupt)
	}
}

// TestSolverConflictMapsToResolution pins that a *solver.ConflictError, the
// error type the version solver returns for an unsatisfiable requirement
// set, classifies as ExitResolution - both bare and wrapped, matching
// through errors.Is via ConflictError's own Is method.
func TestSolverConflictMapsToResolution(t *testing.T) {
	err := error(&solver.ConflictError{})
	if got := FromError(err); got != ExitResolution {
		t.Fatalf("FromError(*solver.ConflictError) = %d, want ExitResolution (%d)", got, ExitResolution)
	}
	if got := FromError(fmt.Errorf("resolve: %w", err)); got != ExitResolution {
		t.Fatalf("FromError(wrapped) = %d, want ExitResolution (%d)", got, ExitResolution)
	}
}

// TestArtifactDownloadDeadlineClassification pins helpers.ErrArtifactDownloadDeadline's
// full classification story beyond the single representative case already
// covered in fromErrorCases: unaggregated it is ExitNetwork, joined behind
// collections.Start's helpers.ErrInstallationFailed headline it is ExitInstall
// (identical to every other per-collection failure, helpers.ErrDownloadFailed
// included - this is not a behavior change), and it is never ExitInterrupt in
// either shape, which is exactly what the sentinel's deliberate %v-not-%w
// cause rendering buys: leaving context.DeadlineExceeded or context.Canceled
// reachable via errors.Is would let FromError's cancellation check (checked
// first, ahead of every other class) misclassify a hostile or slow server as
// a caught Ctrl-C.
func TestArtifactDownloadDeadlineClassification(t *testing.T) {
	bare := helpers.ErrArtifactDownloadDeadline
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}
	if got := FromError(bare); got == ExitInterrupt {
		t.Errorf("FromError(bare sentinel) = ExitInterrupt, want anything else")
	}

	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape; see ErrArtifactDownloadDeadline's doc comment.
	deadlineCause := fmt.Errorf("%w after 15m0s: %v", helpers.ErrArtifactDownloadDeadline, context.DeadlineExceeded)
	joined := errors.Join(headline, deadlineCause)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
	if got := FromError(joined); got == ExitInterrupt {
		t.Errorf("FromError(joined) = ExitInterrupt, want anything else")
	}
}

// TestReadStalledClassification pins helpers.ErrReadStalled's full
// classification story beyond the single representative case already
// covered in fromErrorCases, mirroring TestArtifactDownloadDeadlineClassification:
// (a) the bare sentinel is ExitNetwork; (b) the real production shape
// watchdogBody.Read builds - the cause rendered with %v, not wrapped with %w
// - is also ExitNetwork; (c) joined behind collections.Start's
// helpers.ErrInstallationFailed headline it is ExitInstall, identical to
// every other per-collection failure. This test does NOT prove the watchdog
// fix itself: it builds its own error shape rather than calling into
// package fetch, so reverting internal/galaxy/fetch/watchdog.go's %v back to
// %w does not make this test fail - only TestMixedDripAndStallDoesNotClassifyAsInterrupt
// in package collections exercises the real producer end to end.
//
// The got != ExitInterrupt checks below are documentary, not independent
// pins: they are implied by the preceding got == ExitNetwork/ExitInstall
// checks on the same value, the same redundancy TestArtifactDownloadDeadlineClassification
// already carries. They are kept anyway because they name the security
// property this test exists to cover.
func TestReadStalledClassification(t *testing.T) {
	bare := helpers.ErrReadStalled
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}
	if got := FromError(bare); got == ExitInterrupt {
		t.Errorf("FromError(bare sentinel) = ExitInterrupt, want anything else")
	}

	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	production := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, 30*time.Second, context.Canceled)
	if got := FromError(production); got != ExitNetwork {
		t.Errorf("FromError(production shape) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}
	if got := FromError(production); got == ExitInterrupt {
		t.Errorf("FromError(production shape) = ExitInterrupt, want anything else")
	}

	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joined := errors.Join(headline, production)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
	if got := FromError(joined); got == ExitInterrupt {
		t.Errorf("FromError(joined) = ExitInterrupt, want anything else")
	}
}

// TestMixedDeadlineAndStallShapeIsNotInterrupt is a deliberately synthetic
// shape contract: it joins one collection's real deadline-cause rendering
// with a second collection's real stall-cause rendering behind a single
// installation headline - the shape a mixed byte-dripped/stalled run
// produces - and asserts the combination still classifies as ExitInstall,
// never ExitInterrupt, even though both causes carry a rendered
// context.Canceled/context.DeadlineExceeded that is unreachable through
// errors.Is.
func TestMixedDeadlineAndStallShapeIsNotInterrupt(t *testing.T) {
	headline := fmt.Errorf("%w for 2 collections", helpers.ErrInstallationFailed)
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape; see ErrArtifactDownloadDeadline's doc comment.
	deadlineCause := fmt.Errorf("%w after 3s: %v", helpers.ErrArtifactDownloadDeadline, context.DeadlineExceeded)
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stallCause := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, 30*time.Second, context.Canceled)

	joined := errors.Join(headline, deadlineCause, stallCause)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
	if got := FromError(joined); got == ExitInterrupt {
		t.Errorf("FromError(joined) = ExitInterrupt, want anything else")
	}
}

// TestFromErrorInterruptSurvivesStallSentinel is what makes the rejected
// alternative fix (checking helpers.ErrReadStalled ahead of the
// context.Canceled case in FromError) enforceable: joining the production
// stall shape with a genuine, separate context.Canceled - the shape a real
// Ctrl-C produces alongside an in-flight stall - must still classify as
// ExitInterrupt. Any stall case placed above the cancellation case in
// FromError's switch would make this test fail.
func TestFromErrorInterruptSurvivesStallSentinel(t *testing.T) {
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stallCause := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, 30*time.Second, context.Canceled)

	joined := errors.Join(stallCause, context.Canceled)
	if got := FromError(joined); got != ExitInterrupt {
		t.Errorf("FromError(joined) = %d, want ExitInterrupt (%d)", got, ExitInterrupt)
	}
}

// TestMetadataFetchDeadlineClassification pins helpers.ErrMetadataFetchDeadline's
// full classification story, mirroring TestArtifactDownloadDeadlineClassification:
// the bare sentinel is ExitNetwork; joined behind collections.Start's
// helpers.ErrInstallationFailed headline it is ExitInstall, identical to
// every other per-collection failure; and the real rendered shape (%v, not
// %w) is ExitNetwork, never ExitInterrupt. Killing mutation: removing the
// errors.Is(err, helpers.ErrMetadataFetchDeadline) check from
// isMetadataFetchError makes the bare-sentinel assertion fail with
// "FromError(bare sentinel) = 1, want ExitNetwork (4)".
//
// FALSIFIABILITY CONTROL: the "not ExitInterrupt" assertion on the
// %v-rendered shape is unfalsifiable on its own - a %v-rendered cause can
// never match context.Canceled through errors.Is, so that check would pass
// even against a classifier that always returns something other than
// ExitInterrupt. TestReadStalledClassification and
// TestArtifactDownloadDeadlineClassification carry the identical trap; this
// sibling row closes it the same way they do: the identical message shape,
// but %w-wrapping context.Canceled instead of %v-rendering it, DOES
// classify as ExitInterrupt, proving the %v row's assertion actually
// discriminates rather than passing vacuously.
func TestMetadataFetchDeadlineClassification(t *testing.T) {
	bare := helpers.ErrMetadataFetchDeadline
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}

	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape; see ErrMetadataFetchDeadline's own doc comment.
	renderedCause := fmt.Errorf("%w after 2m0s: %v", helpers.ErrMetadataFetchDeadline, context.DeadlineExceeded)
	joined := errors.Join(headline, renderedCause)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want ExitInstall (%d)", got, ExitInstall)
	}

	if got := FromError(renderedCause); got != ExitNetwork {
		t.Errorf("FromError(rendered cause) = %d, want ExitNetwork (%d), not ExitInterrupt", got, ExitNetwork)
	}

	// Control: the identical shape, %w-wrapping context.Canceled instead of
	// %v-rendering context.DeadlineExceeded, DOES classify as ExitInterrupt.
	wrappedCause := fmt.Errorf("%w after 2m0s: %w", helpers.ErrMetadataFetchDeadline, context.Canceled)
	if got := FromError(wrappedCause); got != ExitInterrupt {
		t.Errorf("control: FromError(%%w-wrapped cause) = %d, want ExitInterrupt (%d)", got, ExitInterrupt)
	}
}

// TestStateObjectDeadlineClassification pins helpers.ErrStateObjectDeadline's
// full classification story, mirroring
// TestMetadataFetchDeadlineClassification: the bare sentinel is ExitNetwork;
// the real rendered shape (%v, not %w) is ExitNetwork, never ExitInterrupt,
// with the identical %v/%w falsifiability control; and it CAN be aggregated,
// since WithStateDeadline bounds SaveStore as well as the init-time reads,
// and SaveStore is called well past init too: finalizeInstall and
// warmWithState fold a save failure in through annotateSaveFailure
// ("%w; snapshot save failed: %w"), so a tail save failure joined behind
// helpers.ErrInstallationFailed classifies ExitInstall, identical to every
// other per-collection failure and to how helpers.ErrMetadataFetchDeadline
// classifies once joined the same way.
//
// Killing mutations, both verified: removing the
// errors.Is(err, helpers.ErrStateObjectDeadline) check from isTransportError
// makes the bare-sentinel assertion fail with "FromError(bare sentinel) = 1,
// want ExitNetwork (4)"; removing isFileIntegrityError from isInstallError's
// checks (the sub-check that matches helpers.ErrInstallationFailed itself)
// makes the tail-save-failure assertion fail with
// "FromError(tail save failure) = 4, want ExitInstall (5)" - the joined tree
// falls through to isNetworkError instead, since nothing left in
// isInstallError still matches the headline.
func TestStateObjectDeadlineClassification(t *testing.T) {
	bare := helpers.ErrStateObjectDeadline
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}

	// This bare shape is not only "the sentinel hit at init": it is the exact
	// tree finalizeInstall/warmWithState return for a byte-dripped SaveStore
	// on a run that recorded zero collection failures (summary.count == 0,
	// so annotateSaveFailure is never reached) - the common case, since most
	// runs have no collection failures. No separate row is needed for that
	// case; this one already covers it.
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape; see ErrStateObjectDeadline's own doc comment.
	renderedCause := fmt.Errorf("%w after 1m0s: %v", helpers.ErrStateObjectDeadline, context.DeadlineExceeded)
	if got := FromError(renderedCause); got != ExitNetwork {
		t.Errorf("FromError(rendered cause) = %d, want ExitNetwork (%d), not ExitInterrupt", got, ExitNetwork)
	}

	// Control: the identical shape, %w-wrapping context.Canceled instead of
	// %v-rendering context.DeadlineExceeded, DOES classify as ExitInterrupt.
	wrappedCause := fmt.Errorf("%w after 1m0s: %w", helpers.ErrStateObjectDeadline, context.Canceled)
	if got := FromError(wrappedCause); got != ExitInterrupt {
		t.Errorf("control: FromError(%%w-wrapped cause) = %d, want ExitInterrupt (%d)", got, ExitInterrupt)
	}

	// A tail SaveStore failure (finalizeInstall/warmWithState, well past
	// init) joins the same way a per-collection failure does:
	// annotateSaveFailure's exact wrap shape, "%w; snapshot save failed: %w",
	// around a headline that already carries helpers.ErrInstallationFailed.
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	tailSaveFailure := fmt.Errorf("%w; snapshot save failed: %w", headline, renderedCause)
	if got := FromError(tailSaveFailure); got != ExitInstall {
		t.Errorf("FromError(tail save failure) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
}

// fakeSignal is a non-syscall.Signal os.Signal implementation used to verify
// FromSignal's fallback path.
type fakeSignal struct{}

func (fakeSignal) String() string { return "fake" }
func (fakeSignal) Signal()        {}

// TestFromSignal checks the shell-convention 128+signal mapping for the
// signals go-galaxy handles (plus SIGQUIT, still a valid direct FromSignal
// input even though main.go no longer subscribes to it) and the
// non-syscall.Signal fallback.
func TestFromSignal(t *testing.T) {
	tests := []struct {
		sig      os.Signal
		name     string
		wantCode int
	}{
		{name: "SIGINT", sig: syscall.SIGINT, wantCode: 130},
		{name: "SIGTERM", sig: syscall.SIGTERM, wantCode: 143},
		{name: "SIGHUP", sig: syscall.SIGHUP, wantCode: 129},
		{name: "SIGQUIT", sig: syscall.SIGQUIT, wantCode: 131},
		{name: "non-syscall.Signal falls back to ExitError", sig: fakeSignal{}, wantCode: ExitError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromSignal(tt.sig); got != tt.wantCode {
				t.Errorf("FromSignal(%v) = %d, want %d", tt.sig, got, tt.wantCode)
			}
		})
	}
}
