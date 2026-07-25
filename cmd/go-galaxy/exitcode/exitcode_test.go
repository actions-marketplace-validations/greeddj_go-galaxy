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

// TestFromError checks one representative wrapped error per exit class, plus
// the nil/context/fs.ErrNotExist/unclassified edge cases. Wrapping with
// fmt.Errorf("%w: ctx", sentinel) verifies FromError matches through
// errors.Is rather than requiring exact identity.
func TestFromError(t *testing.T) {
	tests := []struct {
		err      error
		name     string
		wantCode int
	}{
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
			name:     "dependency graph has a cycle",
			err:      fmt.Errorf("%w: ctx", helpers.ErrDependencyGraphHasACycle),
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
			name:     "unclassified error falls back to ExitError",
			err:      errTestGeneric,
			wantCode: ExitError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromError(tt.err); got != tt.wantCode {
				t.Errorf("FromError(%v) = %d, want %d", tt.err, got, tt.wantCode)
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
