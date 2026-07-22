package main

import (
	"errors"
	"testing"
)

// Static sentinel errors for TestHandleResult, kept as package-level vars to
// satisfy the err113 linter (no inline errors.New in test bodies).
var (
	errTestAction = errors.New("action failed")
	errTestUsage  = errors.New("flag parse error")
)

// TestHandleResult covers the three ways app.Run can finish: clean success,
// a flag-parse usage error urfave already printed itself, and an
// action/before/after error captured via ExitErrHandler that still needs
// printing exactly once by the caller.
func TestHandleResult(t *testing.T) {
	tests := []struct {
		runErr       error
		capturedErr  error
		wantPrintErr error
		name         string
		wantCode     int
	}{
		{
			name:         "success",
			runErr:       nil,
			capturedErr:  nil,
			wantCode:     0,
			wantPrintErr: nil,
		},
		{
			name:         "usage error already printed by urfave",
			runErr:       errTestUsage,
			capturedErr:  nil,
			wantCode:     1,
			wantPrintErr: nil,
		},
		{
			name:         "action error captured for single print",
			runErr:       errTestAction,
			capturedErr:  errTestAction,
			wantCode:     1,
			wantPrintErr: errTestAction,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, printErr := handleResult(tt.runErr, tt.capturedErr)
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
