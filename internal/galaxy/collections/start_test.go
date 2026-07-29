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

// capturingPrinter is an output.Printer stub that records Printf,
// PersistentPrintf, Warnf, and Debugf calls into separate slices, one per
// method, so a test can assert a best-effort failure (Printf, suppressed in
// quiet mode), a user-facing result announcement (PersistentPrintf, always
// emitted to stdout), a security-relevant warning (Warnf, always emitted to
// stderr - PersistentPrintf and Warnf both route through Progress.persist and
// are both always-emitted, differing only in which stream they write to, per
// the stdout-purity rule), or a debug-only signal (Debugf, verbose mode only)
// surfaced on the expected channel rather than being silently swallowed or
// emitted on the wrong one. It embeds noopPrinter (defined in
// lock_pin_test.go) for the other Printer methods.
type capturingPrinter struct {
	noopPrinter

	prints   []string
	persists []string
	warns    []string
	debugs   []string
	oks      []string
	errs     []string
	mu       sync.Mutex
}

func (p *capturingPrinter) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prints = append(p.prints, fmt.Sprintf(format, args...))
}

// Okf records a success-tier line, the same tier canSkipInstall's
// "Installed:"/"Cached:" lines use and classifyDryRun's "Would install:"
// lines share.
func (p *capturingPrinter) Okf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.oks = append(p.oks, fmt.Sprintf(format, args...))
}

// PersistentPrintf records a result-tier line: output that must survive even
// in quiet mode (see the Printer interface doc comment for the tier split).
func (p *capturingPrinter) PersistentPrintf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persists = append(p.persists, fmt.Sprintf(format, args...))
}

func (p *capturingPrinter) Warnf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warns = append(p.warns, fmt.Sprintf(format, args...))
}

// Errorf records an error-tier line, the tier classifyDryRun's "Would fail:"
// lines share with a real install's own "Failed:" lines.
func (p *capturingPrinter) Errorf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, fmt.Sprintf(format, args...))
}

func (p *capturingPrinter) Debugf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.debugs = append(p.debugs, fmt.Sprintf(format, args...))
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

// hasPersistentPrintContaining reports whether any recorded PersistentPrintf
// line contains substr.
func (p *capturingPrinter) hasPersistentPrintContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.persists {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasWarnContaining reports whether any recorded Warnf line contains substr.
func (p *capturingPrinter) hasWarnContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.warns {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasDebugContaining reports whether any recorded Debugf line contains substr.
func (p *capturingPrinter) hasDebugContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.debugs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasOkContaining reports whether any recorded Okf line contains substr.
func (p *capturingPrinter) hasOkContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.oks {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasErrContaining reports whether any recorded Errorf line contains substr.
func (p *capturingPrinter) hasErrContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.errs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// okLines returns a snapshot copy of every recorded Okf line, in recording
// order - used by tests asserting classifyDryRun's per-collection report
// order rather than just membership.
func (p *capturingPrinter) okLines() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.oks))
	copy(out, p.oks)
	return out
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
