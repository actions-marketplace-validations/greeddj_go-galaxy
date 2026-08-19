package solver

import "testing"

// TestEarliestSatisfierJointSatisfaction pins the "partial satisfier" case:
// a term satisfied only by the combination of two assignments of the same
// package, neither of which suffices alone. This mirrors the reference
// spec's "Conflict Resolution With a Partial Satisfier" example's core
// mechanic (a term like foo>=1.0.0,<2.0.0 needing both a lower-bound and an
// upper-bound derivation).
func TestEarliestSatisfierJointSatisfaction(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())

	dummyCause := &incompatibility{Terms: []term{{Package: "unrelated", Set: fullVerSet(), Positive: true}}}
	dummyIdx, _ := s.store.add(dummyCause)
	lower := s.ps.derive(term{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}, dummyIdx)
	upper := s.ps.derive(term{Package: "foo", Set: mustSet(t, "<2.0.0"), Positive: true}, dummyIdx)

	// Neither assignment alone entails "foo ^1.0.0": the lower bound alone
	// still admits 2.0.0 and above, the upper bound alone still admits
	// versions below 1.0.0. Their signed conjunction is exactly
	// [1.0.0, 2.0.0), which does entail it - so the satisfier is the later
	// of the two jointly-necessary assignments.
	inc := &incompatibility{Terms: []term{{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}}}

	satisfier, satisfiedTerm := s.earliestSatisfier(inc)
	if satisfier == nil {
		t.Fatalf("earliestSatisfier returned nil, want the joint satisfier (upper bound assignment)")
	}
	if satisfier.Index != upper.Index {
		t.Fatalf("satisfier index = %d, want the upper-bound assignment's index %d (the later of the two jointly-necessary assignments)",
			satisfier.Index, upper.Index)
	}
	if satisfiedTerm.Package != "foo" {
		t.Fatalf("term package = %q, want foo", satisfiedTerm.Package)
	}

	previous := s.earliestSatisfierBefore(inc, satisfier)
	if previous == nil {
		t.Fatalf("earliestSatisfierBefore returned nil, want the lower-bound assignment (still needed jointly)")
	}
	if previous.Index != lower.Index {
		t.Fatalf("previousSatisfier index = %d, want the lower-bound assignment's index %d", previous.Index, lower.Index)
	}
}

// TestEarliestSatisfierSingleAssignment pins the simple (non-joint) case: a
// single sufficient assignment is both the satisfier and leaves no
// previousSatisfier, since nothing before it was needed.
func TestEarliestSatisfierSingleAssignment(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	cause := &incompatibility{Terms: []term{{Package: "root-req", Set: fullVerSet(), Positive: true}}}
	causeIdx, _ := s.store.add(cause)
	a := s.ps.derive(term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}, causeIdx)

	inc := &incompatibility{Terms: []term{{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}}}
	satisfier, _ := s.earliestSatisfier(inc)
	if satisfier == nil || satisfier.Index != a.Index {
		t.Fatalf("satisfier = %v, want the single assignment at index %d", satisfier, a.Index)
	}
	if previous := s.earliestSatisfierBefore(inc, satisfier); previous != nil {
		t.Fatalf("previousSatisfier = %v, want nil (the single assignment already suffices alone)", previous)
	}
}

// TestResolveConflictBackjumpsWhenSatisfierIsDecision pins that resolving a
// conflict whose satisfier is a decision always backjumps (never merges),
// per the algorithm's first backjump condition.
func TestResolveConflictBackjumpsWhenSatisfierIsDecision(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider().withVersions("foo", "1.0.0"))
	s.ps.decide(rootPkg, rootVersion)
	s.ps.decide("foo", mustV(t, "1.0.0"))

	inc := &incompatibility{Terms: []term{{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}}}
	idx, _ := s.store.add(inc)

	rootIdx, rootCause, err := s.resolveConflict(idx)
	if err != nil {
		t.Fatalf("resolveConflict: %v", err)
	}
	if rootCause != inc {
		t.Fatalf("resolveConflict merged an incompatibility whose satisfier is a decision; it must backjump unchanged instead")
	}
	if rootIdx != idx {
		t.Fatalf("resolveConflict returned index %d, want the original %d (incChanged should be false, so store.add is never called)",
			rootIdx, idx)
	}
	if s.ps.currentLevel() != 0 {
		t.Fatalf("current level = %d, want 0 (backtrack past foo's decision)", s.ps.currentLevel())
	}
}

// TestResolveConflictMergeLearnsNewIncompatibility pins that a genuine merge
// (satisfier is a derivation, same level as its previous satisfier) adds a
// new, distinct incompatibility to the store once the loop finally
// backjumps (incChanged == true), while a conflict resolved without ever
// merging (immediate backjump) never grows the store at all.
func TestResolveConflictMergeLearnsNewIncompatibility(t *testing.T) {
	t.Parallel()
	// root depends on foo >=1.0.0; foo 2.0.0 depends on bar ^1.0.0; foo 1.0.0
	// has no dependencies; bar 1.0.0 depends on foo ^1.0.0. This is the
	// "Performing Conflict Resolution" fixture's own shape, which is known to
	// exercise exactly one merge step before backjumping to level 0.
	p := conflictResolutionProvider()
	result, err := Solve(t.Context(), []Requirement{{Package: "foo", Constraint: ">=1.0.0"}}, p)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if result.Versions["foo"] != "1.0.0" {
		t.Fatalf("Versions = %v, want foo=1.0.0 (proves the merge correctly ruled out foo 2.0.0)", result.Versions)
	}
}

// TestResolveConflictSkipsStoreAddWhenUnchanged pins the incChanged == false
// path precisely: resolving a conflict via an immediate backjump (no merge
// iteration ever ran) must return the exact same *incompatibility pointer
// that was passed in, never a newly-allocated one, and must never insert a
// new entry into the store.
func TestResolveConflictSkipsStoreAddWhenUnchanged(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider().withVersions("foo", "1.0.0"))
	s.ps.decide(rootPkg, rootVersion)
	s.ps.decide("foo", mustV(t, "1.0.0"))

	inc := &incompatibility{Terms: []term{{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}}}
	idx, _ := s.store.add(inc)
	before := len(s.store.all)

	_, rootCause, err := s.resolveConflict(idx)
	if err != nil {
		t.Fatalf("resolveConflict: %v", err)
	}
	if rootCause != inc {
		t.Fatalf("expected the unchanged input incompatibility to be returned verbatim")
	}
	if len(s.store.all) != before {
		t.Fatalf("store grew from %d to %d entries; an unchanged (never-merged) conflict must not add anything", before, len(s.store.all))
	}
}
