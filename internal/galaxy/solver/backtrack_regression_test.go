package solver

import "testing"

// TestFullConstraintInvisibilityBacktrack pins the clear-if-full fix: a
// vacuously-true dependency term (one that classifies to the full
// boundary-extended universe, like an unconstrained "*") must still stay a
// visible contributor to its package's running intersection. gen.p0@1.5.0
// requires gen.p1 via a wildcard ("*") and gen.p2 via ^0.0.3 (which gen.p2
// cannot satisfy), so gen.p0@1.5.0 is unsatisfiable. Because the wildcard
// classifies to the full extended universe, it must still stay a visible
// contributor so the solver learns "not gen.p0@1.5.0" and backtracks to
// gen.p0@1.2.0 rather than falsely rejecting the graph.
func TestFullConstraintInvisibilityBacktrack(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().
		withVersions("gen.p0", "1.2.0", "1.5.0").
		withVersions("gen.p1", "1.5.0").
		withVersions("gen.p2", "0.2.5").
		withDeps("gen.p0", "1.5.0", map[string]string{"gen.p1": "*"}).
		withDeps("gen.p0", "1.2.0", map[string]string{"gen.p2": "*"}).
		withDeps("gen.p1", "1.5.0", map[string]string{"gen.p2": "^0.0.3"})
	res, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: "*"}}, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Versions["gen.p0"] != "1.2.0" || res.Versions["gen.p2"] != "0.2.5" {
		t.Fatalf("Versions = %v, want gen.p0=1.2.0 gen.p2=0.2.5", res.Versions)
	}
}

// TestResultExcludesBacktrackedOverInstall pins the reachable-closure fix:
// extractResult must return only the packages reachable from the root
// through decided dependency edges, not every package that was ever
// decided. gen.p0@2.0.0 and gen.p0@1.0.0 both pull in gen.p3 (directly or
// through gen.p1/gen.p2) but are unsatisfiable, so the solver backtracks to
// gen.p0@0.2.0, which depends on nothing. The result must contain only the
// reachable packages - a gen.p3 decision left over from an abandoned branch
// must not linger as an over-install.
func TestResultExcludesBacktrackedOverInstall(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().
		withVersions("gen.p0", "0.2.0", "1.0.0", "2.0.0").
		withVersions("gen.p1", "0.2.5").
		withVersions("gen.p2", "1.2.0").
		withVersions("gen.p3", "2.0.0").
		withDeps("gen.p0", "1.0.0", map[string]string{"gen.p1": "<2.0.0", "gen.p2": "^0.2.0"}).
		withDeps("gen.p0", "2.0.0", map[string]string{"gen.p1": ">=0.2.0", "gen.p3": "<2.0.0"}).
		withDeps("gen.p1", "0.2.5", map[string]string{"gen.p3": "<2.0.0"}).
		withDeps("gen.p2", "1.2.0", map[string]string{"gen.p3": "!=1.5.0"})
	res, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: ">=0.2.0"}}, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Versions) != 1 || res.Versions["gen.p0"] != "0.2.0" {
		t.Fatalf("Versions = %v, want exactly {gen.p0:0.2.0} (no over-installed gen.p3)", res.Versions)
	}
}
