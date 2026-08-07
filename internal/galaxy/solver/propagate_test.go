package solver

import (
	"testing"
)

// newTestState builds a bare solveState with no requirements, for tests that
// drive relation()/propagation machinery directly rather than through Solve.
func newTestState(p Provider) *solveState {
	return newSolveState(p, nil)
}

// materializeForTest fetches and materializes "foo" against s, mirroring
// what materializePkg does, for tests that need that package already
// materialized before exercising relation() Case C without going through
// decision making. Every caller happens to use the same package name, so
// this hard-codes it rather than threading an always-identical parameter.
func materializeForTest(t *testing.T, s *solveState) {
	t.Helper()
	if err := s.materializePkg(t.Context(), "foo"); err != nil {
		t.Fatalf("materializePkg(foo): %v", err)
	}
}

func mustSet(t *testing.T, raw string) versionSet {
	t.Helper()
	set, err := newSymbolicSet(raw)
	if err != nil {
		t.Fatalf("newSymbolicSet(%q): %v", raw, err)
	}
	return set
}

// TestRelationCaseA covers Case A: a decision pins an exact version, so
// membership is exact regardless of materialization, for both polarities.
func TestRelationCaseA(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider().withVersions("foo", "1.0.0", "2.0.0"))
	s.ps.decide("foo", mustV(t, "1.0.0"))

	positiveMatching := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}
	if got := relation(positiveMatching, s.ps, s.uniFor); got != termSatisfied {
		t.Fatalf("positive matching term = %v, want termSatisfied", got)
	}
	positiveNonMatching := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: true}
	if got := relation(positiveNonMatching, s.ps, s.uniFor); got != termContradicted {
		t.Fatalf("positive non-matching term = %v, want termContradicted", got)
	}
	negativeMatching := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: false}
	if got := relation(negativeMatching, s.ps, s.uniFor); got != termContradicted {
		t.Fatalf("negative matching term = %v, want termContradicted", got)
	}
	negativeNonMatching := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: false}
	if got := relation(negativeNonMatching, s.ps, s.uniFor); got != termSatisfied {
		t.Fatalf("negative non-matching term = %v, want termSatisfied", got)
	}
}

// TestRelationCaseB covers Case B: zero assignments for the package is
// always INCONCLUSIVE, regardless of the term's own content.
func TestRelationCaseB(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	term := term{Package: "untouched", Set: anySet, Positive: true}
	if got := relation(term, s.ps, s.uniFor); got != termInconclusive {
		t.Fatalf("relation on an untouched package = %v, want termInconclusive", got)
	}
}

// TestRelationCaseC covers Case C: exact bitset algebra once a package is
// materialized, including the subset/disjoint/inconclusive three-way split.
func TestRelationCaseC(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider().withVersions("foo", "1.0.0", "1.5.0", "2.0.0"))
	materializeForTest(t, s)
	s.ps.materializePackage("foo")
	// Derive foo ^1.0.0 (a real, non-decision assignment) so the running
	// intersection narrows to {1.0.0, 1.5.0} without pinning a decision.
	s.ps.derive(term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}, 0)

	subsetTerm := term{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}
	if got := relation(subsetTerm, s.ps, s.uniFor); got != termSatisfied {
		t.Fatalf("I subset T = %v, want termSatisfied", got)
	}
	disjointTerm := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: true}
	if got := relation(disjointTerm, s.ps, s.uniFor); got != termContradicted {
		t.Fatalf("I disjoint T = %v, want termContradicted", got)
	}
	inconclusiveTerm := term{Package: "foo", Set: mustSet(t, "=1.0.0"), Positive: true}
	if got := relation(inconclusiveTerm, s.ps, s.uniFor); got != termInconclusive {
		t.Fatalf("I partially overlapping T = %v, want termInconclusive", got)
	}
}

// TestRelationCaseD covers Case D's exact key-identity fast paths (equal
// key/same polarity -> satisfied, equal key/opposite polarity ->
// contradicted) and its INCONCLUSIVE fallback for anything else, on an
// unmaterialized package.
func TestRelationCaseD(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.derive(term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}, 0)

	sameKeySamePolarity := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}
	if got := relation(sameKeySamePolarity, s.ps, s.uniFor); got != termSatisfied {
		t.Fatalf("same key, same polarity = %v, want termSatisfied", got)
	}
	sameKeyOppositePolarity := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: false}
	if got := relation(sameKeyOppositePolarity, s.ps, s.uniFor); got != termContradicted {
		t.Fatalf("same key, opposite polarity = %v, want termContradicted", got)
	}
	differentKey := term{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}
	if got := relation(differentKey, s.ps, s.uniFor); got != termInconclusive {
		t.Fatalf("different key = %v, want termInconclusive (deferred, per section 7.3)", got)
	}
}

// TestRelateAlmostSatisfied covers relate()'s aggregate logic: all terms but
// one satisfied, the remaining one inconclusive, is ALMOST_SATISFIED with
// that term surfaced.
func TestRelateAlmostSatisfied(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)
	inc := &incompatibility{
		Terms: []term{
			{Package: rootPkg, Set: singletonSet(rootVersion), Positive: true},
			{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: false},
		},
	}
	rel, unsat := relate(inc, s.ps, s.uniFor)
	if rel != incAlmostSatisfied {
		t.Fatalf("relate = %v, want incAlmostSatisfied", rel)
	}
	if unsat.Package != "foo" {
		t.Fatalf("unsat term package = %q, want foo", unsat.Package)
	}
}

// TestUnitPropagationNewestToOldest pins that a package's incompatibilities
// are scanned newest to oldest: given two incompatibilities that could both
// derive something, the one added LAST must be the one whose derivation
// wins (observable via which CauseIndex the resulting derivation records).
func TestUnitPropagationNewestToOldest(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)

	oldInc := &incompatibility{Terms: []term{
		{Package: rootPkg, Set: singletonSet(rootVersion), Positive: true},
		{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: false},
	}}
	oldIdx, _ := s.store.add(oldInc)

	newInc := &incompatibility{Terms: []term{
		{Package: rootPkg, Set: singletonSet(rootVersion), Positive: true},
		{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: false},
	}}
	newIdx, _ := s.store.add(newInc)

	if err := s.unitPropagation(t.Context(), rootPkg); err != nil {
		t.Fatalf("unitPropagation: %v", err)
	}

	// Both incompatibilities are almost satisfied (each names a different
	// foo constraint, so neither derivation is a duplicate of the other),
	// so scanning them all produces two derivations - but the newest-first
	// scan order means the derivation caused by newInc must be appended
	// before the one caused by oldInc.
	p := s.ps.pkgState("foo")
	if len(p.indices) != 2 {
		t.Fatalf("expected two derivations for foo, got %d", len(p.indices))
	}
	first := s.ps.assignments[p.indices[0]]
	second := s.ps.assignments[p.indices[1]]
	if first.CauseIndex != newIdx {
		t.Fatalf("first derivation's cause = %d, want the newest incompatibility (%d)", first.CauseIndex, newIdx)
	}
	if second.CauseIndex != oldIdx {
		t.Fatalf("second derivation's cause = %d, want the oldest incompatibility (%d)", second.CauseIndex, oldIdx)
	}
}

// TestPopSmallestOrder pins changed's deterministic pop order: ascending
// package name, regardless of insertion order.
func TestPopSmallestOrder(t *testing.T) {
	t.Parallel()
	changed := map[string]bool{"zebra": true, "alpha": true, "middle": true}
	var order []string
	for len(changed) > 0 {
		order = append(order, popSmallest(changed))
	}
	want := []string{"alpha", "middle", "zebra"}
	for i, w := range want {
		if order[i] != w {
			t.Fatalf("pop order = %v, want %v", order, want)
		}
	}
}
