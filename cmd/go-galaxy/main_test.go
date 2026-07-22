package main

import (
	"errors"
	"os"
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
