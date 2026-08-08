package solver

import (
	"errors"
	"testing"
)

// mustNotInvariantError asserts Solve did not surface an internal-invariant
// error: it either resolved or failed with a clean *ConflictError.
func mustNotInvariantError(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, errSolverBug) {
		t.Fatalf("internal-invariant error (want nil or *ConflictError): %v", err)
	}
	var ce *ConflictError
	if err != nil && !errors.As(err, &ce) {
		t.Fatalf("want nil or *ConflictError, got %v", err)
	}
}

// TestVacuouslySatisfiedConflictIsCleanFailure covers the conflict-resolution
// dead end where a learned clause reduces to a single always-satisfiable term:
// it must fail as a clean *ConflictError, never an internal-invariant error.
func TestVacuouslySatisfiedConflictIsCleanFailure(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().
		withVersions("gen.p0", "0.2.0").
		withVersions("gen.p1", "0.0.3", "0.2.0", "0.2.5", "1.0.0", "1.0.0-rc.1", "1.2.0", "1.5.0").
		withVersions("gen.p2", "1.0.0-rc.1").
		withVersions("gen.p3", "0.2.0", "0.2.5", "1.0.0", "1.0.0-rc.1", "1.2.0", "1.5.0").
		withVersions("gen.p4", "0.2.5", "1.0.0-rc.1").
		withDeps("gen.p0", "0.2.0", map[string]string{"gen.p1": "*", "gen.p4": "<2.0.0"}).
		withDeps("gen.p1", "0.0.3", map[string]string{"gen.p2": "!=1.5.0"}).
		withDeps("gen.p1", "0.2.0", map[string]string{"gen.p3": "<2.0.0", "gen.p4": "1.x"}).
		withDeps("gen.p1", "0.2.5", map[string]string{"gen.p2": "!=1.5.0"}).
		withDeps("gen.p1", "1.0.0", map[string]string{"gen.p3": "<2.0.0", "gen.p4": "1.x"}).
		withDeps("gen.p1", "1.0.0-rc.1", map[string]string{"gen.p2": "!=1.5.0"}).
		withDeps("gen.p1", "1.2.0", map[string]string{"gen.p3": "<2.0.0", "gen.p4": "1.x"}).
		withDeps("gen.p1", "1.5.0", map[string]string{"gen.p2": "!=1.5.0"}).
		withDeps("gen.p2", "1.0.0-rc.1", map[string]string{"gen.p4": "1.x"}).
		withDeps("gen.p3", "1.0.0-rc.1", map[string]string{"gen.p4": "<2.0.0"}).
		withDeps("gen.p3", "1.2.0", map[string]string{"gen.p4": "1.x"}).
		withDeps("gen.p3", "1.5.0", map[string]string{"gen.p4": "!=1.5.0"})
	_, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: ">=0.2.0"}}, p)
	mustNotInvariantError(t, err)
}

// TestNonConvergingConflictIsCleanFailure covers the conflict-resolution dead
// end where resolution would not converge: it must fail as a clean
// *ConflictError, never an internal-invariant error.
func TestNonConvergingConflictIsCleanFailure(t *testing.T) {
	t.Parallel()
	g := fuzzDecodeGraph([]byte("A'2'00"))
	_, err := Solve(t.Context(), g.roots, g.provider())
	mustNotInvariantError(t, err)
}
