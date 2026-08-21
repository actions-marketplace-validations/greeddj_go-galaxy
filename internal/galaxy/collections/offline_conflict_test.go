package collections

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// errTestUnrelatedFailure is an unclassified sentinel used only to prove
// annotateOfflineConflict leaves a non-conflict error alone.
var errTestUnrelatedFailure = errors.New("some unrelated failure")

// TestAnnotateOfflineConflictAddsNoteButStaysClassifiable pins the fence
// this feature depends on: wrapping a *solver.ConflictError under --offline
// must never break errors.Is/errors.As/exitcode.FromError classification,
// even though the rendered message gains an extra line. If the note were
// ever appended by flattening the error into a new string (%v/%s) instead
// of wrapping it via Unwrap, every one of these three checks would fail.
func TestAnnotateOfflineConflictAddsNoteButStaysClassifiable(t *testing.T) {
	t.Parallel()
	base := &solver.ConflictError{}
	wrapped := fmt.Errorf("resolve: %w", base)

	cfg := &config.Config{Offline: true}
	got := annotateOfflineConflict(cfg, wrapped)

	if !strings.Contains(got.Error(), offlineConflictNote) {
		t.Fatalf("Error() = %q, want it to contain the offline note %q", got.Error(), offlineConflictNote)
	}
	if _, ok := errors.AsType[*solver.ConflictError](got); !ok {
		t.Fatalf("errors.As(got, &*solver.ConflictError) = false, want true")
	}
	if !errors.Is(got, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(got, ErrNoVersionSatisfiesConstraints) = false, want true")
	}
	if code := exitcode.FromError(got); code != exitcode.ExitResolution {
		t.Fatalf("exitcode.FromError(got) = %d, want ExitResolution (%d)", code, exitcode.ExitResolution)
	}
}

// TestAnnotateOfflineConflictPassesThroughUnaffectedErrors asserts that
// annotateOfflineConflict only ever touches an error when BOTH conditions
// hold: --offline is set AND the error is a *solver.ConflictError. A
// conflict resolved online, and any non-conflict error even when offline,
// pass through as the exact same error value, unchanged.
func TestAnnotateOfflineConflictPassesThroughUnaffectedErrors(t *testing.T) {
	t.Parallel()
	conflictErr := fmt.Errorf("resolve: %w", &solver.ConflictError{})

	// A direct != comparison is intentional here, not a stand-in for
	// errors.Is: the assertion is that annotateOfflineConflict returns the
	// exact same error value byte-for-byte, never a new wrapper - errors.Is
	// would still pass even if it wrapped the value again, which is exactly
	// the regression this test exists to catch.
	//nolint:err113,errorlint // intentional identity comparison, not error-equality checking; see comment above
	if got := annotateOfflineConflict(&config.Config{Offline: false}, conflictErr); got != conflictErr {
		t.Fatalf("annotateOfflineConflict changed an online conflict error: got %v, want it unchanged", got)
	}
	//nolint:err113,errorlint // intentional identity comparison, not error-equality checking; see comment above
	if got := annotateOfflineConflict(&config.Config{Offline: true}, errTestUnrelatedFailure); got != errTestUnrelatedFailure {
		t.Fatalf("annotateOfflineConflict changed a non-conflict error under --offline: got %v, want it unchanged", got)
	}
	if got := annotateOfflineConflict(&config.Config{Offline: false}, nil); got != nil {
		t.Fatalf("annotateOfflineConflict(cfg, nil) = %v, want nil", got)
	}
}
