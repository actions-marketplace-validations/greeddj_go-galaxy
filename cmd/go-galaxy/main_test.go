package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestHandleResult covers every way app.Run can finish: clean success, a flag
// failure in each of its two shapes, an action error captured via
// ExitErrHandler that gets classified through exitcode.FromError and still
// needs printing exactly once, and a caught signal that must take precedence
// over any captured error since the process is being told to stop.
//
// "usage error urfave reported itself" and "usage error nothing reported" are
// one fixture differing in exactly the reported bit, and they are required to
// produce opposite printErr values. The first is the positive control: it
// shows this fixture can reach the already-told answer at all. The second is
// the pin on the defect - a flag value urfave parsed in silence, which exited
// with nothing on either stream. Neither row proves anything alone: without
// the first, "it stayed quiet" is indistinguishable from "it stays quiet for
// every bare runErr"; without the second, nothing forbids that silence.
//
// KILLING MUTATION, run and reverted, in handleResult (main.go) - return
// exitcode.ExitUsage, nil from the silent arm, which is the behavior this
// test exists to forbid. Only "usage error nothing reported" fails:
//
//	main_test.go:69: printErr = <nil>, want errors.Is match with flag parse error
//
// KILLING MUTATION, run and reverted, in handleResult (main.go) - invert the
// arm's condition to "if !reported". Both rows fail, with opposite messages,
// which is what shows the control and the pin are reading the same bit from
// opposite sides:
//
//	main_test.go:64: printErr = flag parse error, want nil
//	main_test.go:69: printErr = <nil>, want errors.Is match with flag parse error
//
// KILLING MUTATION, run and reverted, in handleResult (main.go) - hoist the
// reported check above the capturedErr branch. Only "captured error outranks
// a usage report" fails:
//
//	main_test.go:60: code = 2, want 5
//	main_test.go:69: printErr = <nil>, want errors.Is match with installation failed
func TestHandleResult(t *testing.T) {
	t.Parallel()
	for _, tt := range handleResultCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, printErr := handleResult(tt.runErr, tt.capturedErr, tt.sig, tt.reported)
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

// handleResultCase is one row of TestHandleResult: the three values app.Run
// can leave behind, the observation of whether urfave has already reported,
// and the answer handleResult must give for them.
type handleResultCase struct {
	runErr       error
	capturedErr  error
	sig          os.Signal
	wantPrintErr error
	name         string
	wantCode     int
	reported     bool
}

// handleResultCases builds TestHandleResult's table, split out from the test
// function itself purely to stay under the funlen budget - the same split
// internal/cache/s3's newS3RetryableCases makes for the identical reason.
//
// A zero-valued field is left off a row, except reported on the two usage
// rows: those two are one fixture differing in exactly that bit, so it is
// written on both rather than inferred from an absence.
func handleResultCases() []handleResultCase {
	return []handleResultCase{
		{name: "success", wantCode: exitcode.ExitOK},
		{
			name:     "usage error urfave reported itself",
			runErr:   errTestUsage,
			reported: true,
			wantCode: exitcode.ExitUsage,
		},
		{
			name:         "usage error nothing reported",
			runErr:       errTestUsage,
			reported:     false,
			wantCode:     exitcode.ExitUsage,
			wantPrintErr: errTestUsage,
		},
		{
			name:         "action error captured and classified via FromError",
			runErr:       helpers.ErrInstallationFailed,
			capturedErr:  helpers.ErrInstallationFailed,
			wantCode:     exitcode.ExitInstall,
			wantPrintErr: helpers.ErrInstallationFailed,
		},
		// This input combination is not reachable from run() today: urfave
		// reports an argv usage error and returns without ever calling
		// ExitErrHandler, so no run sets reported and capturedErr together.
		// The row pins branch order - capturedErr is consulted ahead of the
		// reported check - and is not a scenario an operator can produce.
		{
			name:         "captured error outranks a usage report",
			runErr:       errTestUsage,
			capturedErr:  helpers.ErrInstallationFailed,
			reported:     true,
			wantCode:     exitcode.ExitInstall,
			wantPrintErr: helpers.ErrInstallationFailed,
		},
		{
			name:        "caught signal takes precedence over a captured network error",
			runErr:      helpers.ErrDownloadFailed,
			capturedErr: helpers.ErrDownloadFailed,
			sig:         syscall.SIGINT,
			wantCode:    130,
		},
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
//	main_test.go:192: Description is missing the line for exit 8: "  8    Cache contention"
func TestRootCommandDisclosesDefaultCommandAndExitCodes(t *testing.T) {
	cmd, _ := newRootCommand(nil, io.Discard)

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

// TestRootCommandRecordsUrfaveUsageReports pins the observation handleResult
// reads: whether urfave reported a flag failure itself. The two subtests are
// each other's positive control - one fixture per shape, differing only in
// where the unparseable value came from - so a recorder that saw nothing
// cannot be mistaken for a writer nothing ever reaches.
//
// Each subtest builds its own root command, since urfave mutates command and
// flag state across a Run.
//
// The err != nil check is a guard, not the pin: both inputs are structurally
// unparseable for a cli.IntFlag, which keeps the install action unreachable in
// a unit test, and the check makes that assumption fail loudly if it stops
// holding.
//
// KILLING MUTATION, run and reverted, in newRootCommand (main.go) - delete the
// ErrWriter: report line, so urfave reports to the default writer and the
// recorder never sees it. The argv subtest fails, and the line it was meant to
// capture leaks to the test run's own stderr:
//
//	main_test.go:228: rec.written = false, want true; errOut = ""
func TestRootCommandRecordsUrfaveUsageReports(t *testing.T) {
	t.Run("argv value urfave reports itself", func(t *testing.T) {
		var errOut bytes.Buffer
		cmd, rec := newRootCommand(nil, &errOut)
		cmd.Writer = io.Discard

		err := cmd.Run(context.Background(), []string{"go-galaxy", "install", "--workers=abc"})
		if err == nil {
			t.Fatalf("Run() = nil, want a flag-parse error; errOut = %q", errOut.String())
		}
		if !rec.written {
			t.Errorf("rec.written = false, want true; errOut = %q", errOut.String())
		}
	})

	t.Run("environment value urfave reports nowhere", func(t *testing.T) {
		t.Setenv("GO_GALAXY_WORKERS", "abc")

		var errOut bytes.Buffer
		cmd, rec := newRootCommand(nil, &errOut)
		cmd.Writer = io.Discard

		err := cmd.Run(context.Background(), []string{"go-galaxy", "install"})
		if err == nil {
			t.Fatalf("Run() = nil, want a flag-parse error; errOut = %q", errOut.String())
		}
		if rec.written {
			t.Errorf("rec.written = true, want false; errOut = %q", errOut.String())
		}
	})
}
