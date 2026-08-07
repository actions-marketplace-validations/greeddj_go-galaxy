package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestHandleResult covers the four ways app.Run can finish: clean success, a
// flag-parse usage error urfave already printed itself, an action error
// captured via ExitErrHandler that gets classified through exitcode.FromError
// and still needs printing exactly once, and a caught signal that must take
// precedence over any captured error since the process is being told to stop.
func TestHandleResult(t *testing.T) {
	tests := []struct {
		runErr       error
		capturedErr  error
		sig          os.Signal
		wantPrintErr error
		name         string
		wantCode     int
	}{
		{
			name:         "success",
			runErr:       nil,
			capturedErr:  nil,
			sig:          nil,
			wantCode:     exitcode.ExitOK,
			wantPrintErr: nil,
		},
		{
			name:         "usage error already printed by urfave",
			runErr:       errTestUsage,
			capturedErr:  nil,
			sig:          nil,
			wantCode:     exitcode.ExitUsage,
			wantPrintErr: nil,
		},
		{
			name:         "action error captured and classified via FromError",
			runErr:       helpers.ErrInstallationFailed,
			capturedErr:  helpers.ErrInstallationFailed,
			sig:          nil,
			wantCode:     exitcode.ExitInstall,
			wantPrintErr: helpers.ErrInstallationFailed,
		},
		{
			name:         "caught signal takes precedence over a captured network error",
			runErr:       helpers.ErrDownloadFailed,
			capturedErr:  helpers.ErrDownloadFailed,
			sig:          syscall.SIGINT,
			wantCode:     130,
			wantPrintErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, printErr := handleResult(tt.runErr, tt.capturedErr, tt.sig)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d", code, tt.wantCode)
			}
			if tt.wantPrintErr == nil {
				if printErr != nil {
					t.Errorf("printErr = %v, want nil", printErr)
				}
				return
			}
			if !errors.Is(printErr, tt.wantPrintErr) {
				t.Errorf("printErr = %v, want errors.Is match with %v", printErr, tt.wantPrintErr)
			}
		})
	}
}

// errTestUsage is a static sentinel for the usage-only case, kept as a
// package-level var to satisfy the err113 linter (no inline errors.New in
// test bodies).
var errTestUsage = errors.New("flag parse error")

// TestRootCommandDisclosesDefaultCommandAndExitCodes pins the two facts the
// binary now tells a CI author about itself. A bare go-galaxy installs, which
// DefaultCommand has always made true and nothing printed ever said; and the
// exit-code classes, which exist to be branched on and until now lived only in
// the README, out of reach of anyone reading --help.
//
// The rows are built from the exitcode constants rather than from literals
// repeated here, so the pin is on the numbers themselves: renumbering a class
// without updating the help text fails this test, which is the failure mode
// worth catching - a help list naming a wrong number is worse than no list.
// The phrases stay literals, since they are prose the constants do not carry.
//
// KILLING MUTATION, run and reverted, on newRootCommand's Description in
// main.go - change the line for exit 8 to read "  9    Cache contention",
// which is what a careless renumbering looks like:
//
//	main_test.go:133: Description is missing the line for exit 8: "  8    Cache contention"
func TestRootCommandDisclosesDefaultCommandAndExitCodes(t *testing.T) {
	cmd := newRootCommand(nil)

	if cmd.DefaultCommand != "install" {
		t.Fatalf("DefaultCommand = %q, want %q", cmd.DefaultCommand, "install")
	}
	if !strings.Contains(cmd.Usage, "install") {
		t.Errorf("Usage = %q, want it to name the default command", cmd.Usage)
	}

	rows := []struct {
		phrase string
		code   int
	}{
		{"Success", exitcode.ExitOK},
		{"Generic failure", exitcode.ExitError},
		{"Usage or configuration error", exitcode.ExitUsage},
		{"Dependency resolution failure", exitcode.ExitResolution},
		{"Network or Galaxy API failure", exitcode.ExitNetwork},
		{"Install-time failure", exitcode.ExitInstall},
		{"Lockfile error", exitcode.ExitLock},
		{"Artifact-integrity failure", exitcode.ExitIntegrity},
		{"Cache contention", exitcode.ExitCacheBusy},
		{"Persisted cache state is corrupt or oversized", exitcode.ExitCacheCorrupt},
		{"Interrupted", exitcode.ExitInterrupt},
	}
	for _, row := range rows {
		line := fmt.Sprintf("  %-5d%s", row.code, row.phrase)
		if !strings.Contains(cmd.Description, line+"\n") {
			t.Errorf("Description is missing the line for exit %d: %q", row.code, line)
		}
	}
}
