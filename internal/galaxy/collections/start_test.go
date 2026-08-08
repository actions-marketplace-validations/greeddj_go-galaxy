package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
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

// callLogBackend is a cacheManager.Backend that records nothing but its Close
// call. The embedded interface is nil on purpose: unwindBackend must touch
// Close and nothing else, so any other method it grew a call to would panic
// on a nil interface rather than pass unnoticed.
type callLogBackend struct {
	cacheManager.Backend

	calls *[]string
}

// Close records the call and succeeds.
func (b callLogBackend) Close(context.Context) error {
	*b.calls = append(*b.calls, "close")
	return nil
}

// unwindCase is one row of TestUnwindBackendReleasesBeforeClose: whether a
// lock was granted, and the call sequence the unwind must produce.
type unwindCase struct {
	name      string
	wantCalls []string
	granted   bool
}

// unwindCases covers both shapes initInstall can hand the unwind. The granted
// row is the ordering assertion; the ungranted row is the arm where
// backend.Lock itself failed, which must still close the backend while
// calling nothing in place of the closure it never received.
func unwindCases() []unwindCase {
	return []unwindCase{
		{name: "lock granted", granted: true, wantCalls: []string{"release", "close"}},
		{name: "lock never granted", granted: false, wantCalls: []string{"close"}},
	}
}

// TestUnwindBackendReleasesBeforeClose pins the order initInstall gives back
// what it took, directly rather than through a run: the lock first, the
// backend second. Nothing observable depends on that order on either backend
// shipped today - the S3 backend's Close returns nil without doing anything,
// and the local backend's closes Bolt files the lock release never touches -
// so it is pinned here because it is the order the code this replaced used,
// and changing it is a decision to make deliberately rather than by accident.
func TestUnwindBackendReleasesBeforeClose(t *testing.T) {
	t.Parallel()

	for _, tc := range unwindCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls []string
			var release func() error
			if tc.granted {
				release = func() error {
					calls = append(calls, "release")
					return nil
				}
			}

			unwindBackend(context.Background(), callLogBackend{calls: &calls}, release)

			if !slices.Equal(calls, tc.wantCalls) {
				t.Fatalf("unwindBackend made calls %v, want %v", calls, tc.wantCalls)
			}
		})
	}
}

// newLockContentionFixture builds a config over a fresh cache directory whose
// exclusive lock is already held, and returns the closure that gives it back.
// flock(2) conflicts between file descriptions rather than processes, so
// taking it here, in the test's own process, is enough to make initInstall's
// own acquisition fail - no subprocess, no goroutine, no timing dependency,
// the same fixture shape cleanup's TestInitCleanupLockFailure uses.
func newLockContentionFixture(t *testing.T) (*config.Config, func() error) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	release, err := store.AcquireLock(cacheDir)
	if err != nil {
		t.Fatalf("hold the cache lock: %v", err)
	}
	return &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}, release
}

// TestInitInstallLockFailureUnwindsWithNothingToRelease pins the one arm of
// initInstall's unwind that has no lock to give back: backend.Lock itself
// failing, which leaves the release closure nil while the backend is already
// open. That arm must still close the backend, must return no holder context
// at all, and must not call the nil closure on its way out.
//
// The holder context is the assertion that no error-shaped check can make:
// this arm never held the lock, so there is nothing to judge the run against,
// and cacheManager.LockLostError leaves an error untouched when the context
// it is handed is nil. An arm that started handing back a non-nil context
// here would be judging a run against a lock it never had.
//
// KILLING MUTATION, run through go test -overlay so the tree stayed
// untouched: dropping the nil check from that unwind's release branch makes
// this arm call a closure Lock never handed back, and no assertion below is
// reached at all - the run dies at the initInstall call itself:
//
//	--- FAIL: TestInitInstallLockFailureUnwindsWithNothingToRelease (0.00s)
//	panic: runtime error: invalid memory address or nil pointer dereference [recovered, repanicked]
//
// The positive control releases the held lock and re-runs initInstall against
// the identical cache directory: it must then succeed, which is what proves
// the refusal above is this fixture's held lock specifically rather than some
// other property of the directory.
func TestInitInstallLockFailureUnwindsWithNothingToRelease(t *testing.T) {
	t.Parallel()
	cfg, release := newLockContentionFixture(t)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	lockCtx, state, err := initInstall(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatalf("expected initInstall to fail while another acquirer holds the cache lock")
	}
	if !errors.Is(err, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("initInstall error = %v, want errors.Is helpers.ErrAnotherInstanceIsRunning", err)
	}
	if state != nil {
		t.Fatalf("initInstall returned a state alongside its failure: %+v", state)
	}
	if lockCtx != nil {
		t.Fatalf("initInstall returned holder context %v from an arm that never took the lock, want nil", lockCtx)
	}

	if err := release(); err != nil {
		t.Fatalf("release the held cache lock: %v", err)
	}
	assertInitInstallSucceedsOnFreeLock(t, cfg, runtime)
}

// assertInitInstallSucceedsOnFreeLock is the positive control for
// TestInitInstallLockFailureUnwindsWithNothingToRelease: with the contending
// lock given back, the same cache directory must let initInstall through. It
// reads t.Context() itself rather than taking one, so *testing.T stays the
// first parameter without tripping revive's context-as-argument rule.
func assertInitInstallSucceedsOnFreeLock(t *testing.T, cfg *config.Config, runtime *infra.Infra) {
	t.Helper()
	lockCtx, state, err := initInstall(t.Context(), cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall once the cache lock is free: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})
	if lockCtx == nil {
		t.Fatalf("initInstall returned no holder context on success")
	}
}
