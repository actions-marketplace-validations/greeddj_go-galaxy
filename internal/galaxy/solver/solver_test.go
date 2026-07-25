package solver

import (
	"errors"
	"strings"
	"testing"
)

// TestFuelGuard lowers fuelLimit (saving and restoring it, since it is a
// package-level var precisely so a test can do this) and asserts Solve
// reports the distinct internal iteration-limit error rather than looping
// forever, on an input designed to need many decisions.
func TestFuelGuard(t *testing.T) {
	// Not parallel: fuelLimit is a shared package-level var.
	old := fuelLimit
	fuelLimit = 3
	defer func() { fuelLimit = old }()

	p := newFakeProvider().
		withVersions("a", "1.0.0").
		withVersions("b", "1.0.0").
		withVersions("c", "1.0.0").
		withVersions("d", "1.0.0").
		withDeps("a", "1.0.0", map[string]string{"b": "^1.0.0"}).
		withDeps("b", "1.0.0", map[string]string{"c": "^1.0.0"}).
		withDeps("c", "1.0.0", map[string]string{"d": "^1.0.0"})
	reqs := []Requirement{{Package: "a", Constraint: "^1.0.0"}}

	_, err := Solve(reqs, p)
	if err == nil {
		t.Fatalf("Solve succeeded despite a fuel limit of 3; want the iteration-limit error")
	}
	if errors.As(err, new(*ConflictError)) {
		t.Fatalf("fuel exhaustion must not surface as a *ConflictError: %v", err)
	}
	if !strings.Contains(err.Error(), "iteration limit exceeded") {
		t.Fatalf("error = %q, want it to mention the iteration limit", err.Error())
	}
}

// TestConservativeRelationFence targets section 7.3/7.2's Case D
// conservatism directly: two symbolic ranges on the SAME undecided package
// that are, in fact, disjoint (foo ^1.0.0 and foo ^2.0.0 can never both
// hold) are NOT detected as contradictory by relation()'s cheap key-identity
// check, since their keys differ - relation must answer INCONCLUSIVE for
// each against the other, deferring the conflict rather than inventing a
// false CONTRADICTED verdict Case D is not entitled to. The solve must still
// terminate correctly once a real decision (or materialization) makes the
// conflict concrete.
func TestConservativeRelationFence(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)
	rangeA := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}
	s.ps.derive(rangeA, mustDummyCause(t, s))

	rangeB := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: true}
	// relation(rangeB, ...) must be INCONCLUSIVE here: foo is unmaterialized,
	// and Case D's key-identity check cannot see that ^1.0.0 and ^2.0.0 are
	// disjoint (different keys, not the same key with opposite polarity) -
	// this is exactly the conservative deferral the design mandates, not a
	// defect someone might try to remove as a false optimization.
	if got := relation(rangeB, s.ps, s.uniFor); got != termInconclusive {
		t.Fatalf("relation(disjoint range against an unmaterialized package) = %v, want termInconclusive (deferred, per section 7.3)", got)
	}

	// The deferred conflict must still surface correctly, and the solve must
	// still terminate, once foo actually gets materialized and decision
	// making has to pick a real candidate: with both ^1.0.0 and ^2.0.0
	// required (through two independent dependers, since two root
	// requirements on the very same package would be rejected upstream by
	// the requirements parser before ever reaching the core) and no version
	// satisfying both, the empty intersection must be caught exactly (Case C
	// is exact bitset algebra, unlike Case D's conservative deferral).
	p2 := newFakeProvider().
		withVersions("mid1", "1.0.0").
		withVersions("mid2", "1.0.0").
		withVersions("foo", "1.0.0", "2.0.0").
		withDeps("mid1", "1.0.0", map[string]string{"foo": "^1.0.0"}).
		withDeps("mid2", "1.0.0", map[string]string{"foo": "^2.0.0"})
	_, err := Solve([]Requirement{
		{Package: "mid1", Constraint: "^1.0.0"},
		{Package: "mid2", Constraint: "^1.0.0"},
	}, p2)
	if err == nil {
		t.Fatalf("Solve: expected a conflict (foo cannot be both ^1.0.0 and ^2.0.0 at once)")
	}
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Solve error is not a *ConflictError: %v (%T)", err, err)
	}
}
