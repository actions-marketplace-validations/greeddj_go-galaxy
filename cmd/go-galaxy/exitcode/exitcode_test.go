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
