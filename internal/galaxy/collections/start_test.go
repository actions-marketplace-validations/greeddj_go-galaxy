package collections

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// capturingPrinter is an output.Printer stub that records Printf calls so a
// test can assert a best-effort sweep failure surfaced its warning line
// rather than being silently swallowed. It embeds noopPrinter (defined in
// lock_pin_test.go) for the other Printer methods.
type capturingPrinter struct {
	noopPrinter

	prints []string
	mu     sync.Mutex
}

func (p *capturingPrinter) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prints = append(p.prints, fmt.Sprintf(format, args...))
}

// hasPrintContaining reports whether any recorded Printf line contains substr.
func (p *capturingPrinter) hasPrintContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.prints {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// TestSweepDeadRunTempsIsBestEffort proves sweepDeadRunTemps never panics or
// aborts the install when a backend's SweepTemp fails: the failure is logged
// as a warning-tier Printf line and the call returns normally, mirroring
// initInstall's own best-effort handling of a RecordProject failure. A nil
// extracted store (the "caching disabled" case) must not panic either.
func TestSweepDeadRunTempsIsBestEffort(t *testing.T) {
	t.Parallel()

	// A regular file (not a directory) used as cacheDir makes the backend's
	// SweepTemp -> store.SweepDownloadTemps -> os.ReadDir call fail with a
	// real (non-not-exist) error, exercising the failure path without a
	// dedicated failing Backend stub.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	backend := local.New(notADir)
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	sweepDeadRunTemps(context.Background(), runtime, backend, nil)

	if !printer.hasPrintContaining("Failed to sweep leftover download temps") {
		t.Fatalf("expected a download-temp sweep warning to be recorded, got %v", printer.prints)
	}
}
