package exitcode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"

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
