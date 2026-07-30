package collections_test

// TestFrozenInstallCorruptedPinPropagatesBothSentinels is the end-to-end
// proof that failureRecorder/failureSummary's per-collection cause actually
// reaches collections.Start's own boundary, not just the package-internal
// callers pinned in failures_test.go and the exitcode-level precedence
// pinned in cmd/go-galaxy/exitcode's own tests.
//
// Verified against a real revert of the production change it pins:
// reverting failureSummary.wrap to always return headline unchanged
// (dropping the per-collection cause, the same mutation save_failure_test.go
// documents) makes this test fail with:
// "expected errors.Is(startErr, helpers.ErrSHA256Mismatch), got installation
// failed for 1 collections"

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// TestFrozenInstallCorruptedPinPropagatesBothSentinels asserts that a frozen
// collections.Start run against a lockfile whose acme.app pin no longer
// matches the real artifact fails with an error that matches BOTH
// helpers.ErrSHA256Mismatch (the actual triggering cause) and
// helpers.ErrInstallationFailed (the run's aggregate classification)
// simultaneously, at the Start boundary - not just somewhere further down the
// call stack. The positive control, run against the identical fixture with
// the true pin restored, proves the corrupted-pin run's failure is really
// caused by the pin mismatch and not some unrelated fixture defect: the same
// setup with a correct pin returns nil and actually installs.
func TestFrozenInstallCorruptedPinPropagatesBothSentinels(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	lockPath, lf := newFrozenPinFixture(t, f)

	setAppPin(lf, corruptedAppSHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrupted lockfile: %v", err)
	}

	startErr := collections.Start(context.Background(), f.cfg, f.runtime)
	if startErr == nil {
		t.Fatal("expected an error from a corrupted lockfile pin, got nil")
	}
	if !errors.Is(startErr, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is(startErr, helpers.ErrSHA256Mismatch), got %v", startErr)
	}
	if !errors.Is(startErr, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is(startErr, helpers.ErrInstallationFailed), got %v", startErr)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))

	// Positive control: restore the true pin and rerun. A nil error here, with
	// the collection actually installed, is what proves the corrupted-pin
	// failure above genuinely came from the pin mismatch this test
	// introduced, not from some other defect in the shared fixture.
	setAppPin(lf, f.appV1.SHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("restore the true pin in the lockfile: %v", err)
	}
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the recovery run: %v", err)
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("recovery Start with the true pin restored: %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
}
