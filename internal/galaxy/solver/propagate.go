package solver

import (
	"slices"
)

// termRelation is the three-valued result of relating a single term to the
// partial solution.
type termRelation uint8

const (
	termInconclusive termRelation = iota
	termSatisfied
	termContradicted
)

// incRelation is the four-valued result of relating a whole incompatibility
// to the partial solution.
type incRelation uint8

const (
	incInconclusive incRelation = iota
	incSatisfied
	incAlmostSatisfied
	incContradicted
)

// relation answers, conservatively, whether the partial solution satisfies,
// contradicts, or is inconclusive for term. It never materializes a
// package: an unmaterialized package's assignments can only be compared by
// exact key identity (Case D below), falling back to INCONCLUSIVE whenever
// that is not enough to decide. Section 7.3 explains why this conservatism
// is sound: it only ever defers a derivation or a conflict, never fabricates
// a false one, and decision making re-verifies every candidate by direct
// membership regardless.
func relation(term term, ps *partialSolution, uniFor func(string) *packageUniverse) termRelation {
	p, ok := ps.packages[term.Package]
	if !ok || len(p.indices) == 0 {
		// Case B: no assignments at all for this package.
		return termInconclusive
	}
	if p.decisionIdx != -1 {
		return relateDecision(term, p, uniFor)
	}
	if p.materialized {
		return relateMaterialized(term, p, uniFor)
	}
	return relateUnmaterialized(term, p, ps)
}

// relateDecision implements Case A: a decision pins an exact version, so
// membership is exact regardless of materialization.
func relateDecision(term term, p *packageAssignments, uniFor func(string) *packageUniverse) termRelation {
	in := setContains(term.Set, p.decisionVersion, uniFor(term.Package))
	if in == term.Positive {
		return termSatisfied
	}
	return termContradicted
}

// relateMaterialized implements Case C: exact bitset algebra against the
// running intersection, which is kept over the package's boundary-extended
// universe (see partial.go and term.go) so this stays exact even for a
// package whose published universe has shrunk to a handful of versions.
func relateMaterialized(term term, p *packageAssignments, uniFor func(string) *packageUniverse) termRelation {
	uni := uniFor(term.Package)
	if subsetOfPermittedExt(p.running, term, uni) {
		return termSatisfied
	}
	if isEmptyBits(permittedExtBits(term, uni)) {
		// A structurally-false term (its permitted set spans no cell of the
		// extended universe) cannot be satisfied by any assignment. Inside a
		// dependency incompatibility {parent, not P in C} - where C matches
		// every published version of P, so "not P in C" is empty-permitted -
		// reading it as contradicted would discard the dependency and drop P.
		// Treat it as the single open term instead, so the satisfied parent
		// derives its negation ("P required") and P is still decided.
		return termInconclusive
	}
	if disjointFromPermittedExt(p.running, term, uni) {
		return termContradicted
	}
	return termInconclusive
}

// relateUnmaterialized implements Case D: unmaterialized, symbolic
// derivations only. Cheap sound checks via exact key identity - defined only
// for a symbolic query term against symbolic assignments; a materialized
// (bitset) query term against an unmaterialized package always falls
// through to INCONCLUSIVE here; anything else is INCONCLUSIVE and deferred
// too.
func relateUnmaterialized(term term, p *packageAssignments, ps *partialSolution) termRelation {
	for _, idx := range p.indices {
		a := &ps.assignments[idx]
		if a.term.Set.kind != setSymbolic || term.Set.kind != setSymbolic {
			continue
		}
		if a.term.Set.key != term.Set.key {
			continue
		}
		if a.term.Positive == term.Positive {
			return termSatisfied
		}
		return termContradicted
	}
	return termInconclusive
}

// relate answers whether the partial solution satisfies, contradicts, is
// inconclusive for, or almost satisfies inc (all terms but one are
// satisfied, the remaining one inconclusive - that remaining term is
// returned alongside).
func relate(inc *incompatibility, ps *partialSolution, uniFor func(string) *packageUniverse) (incRelation, term) {
	var unsat term
	hasUnsat := false
	for _, t := range inc.Terms {
		switch relation(t, ps, uniFor) {
		case termContradicted:
			return incContradicted, term{}
		case termInconclusive:
			if hasUnsat {
				return incInconclusive, term{}
			}
			unsat = t
			hasUnsat = true
		case termSatisfied:
			// Continue scanning the remaining terms.
		}
	}
	if !hasUnsat {
		return incSatisfied, term{}
	}
	return incAlmostSatisfied, unsat
}

// unitPropagation derives new assignments from pkg's incompatibilities until
// no more can be found, resolving any conflict it encounters along the way.
// changed's pop order is deterministic (ascending package name), and each
// package's incompatibilities are scanned newest to oldest, since conflict
// resolution tends to produce more general incompatibilities later on.
func (s *solveState) unitPropagation(pkg string) error {
	changed := map[string]bool{pkg: true}
	for len(changed) > 0 {
		p := popSmallest(changed)
		if _, err := s.propagatePackage(p, changed); err != nil {
			return err
		}
	}
	return nil
}

// propagatePackage scans p's incompatibilities newest to oldest, folding any
// derivations into changed. If it hits a satisfied incompatibility it
// resolves the conflict, replaces changed with the resulting derivation's
// package, and reports conflicted = true so the caller re-enters the outer
// while loop instead of continuing this scan (the partial solution just
// backtracked, so the remaining incompatibilities in this scan are stale).
func (s *solveState) propagatePackage(p string, changed map[string]bool) (bool, error) {
	for _, idx := range s.store.byPackageNewestFirst(p) {
		inc := s.store.all[idx]
		rel, unsat := relate(inc, s.ps, s.uniFor)
		switch rel {
		case incSatisfied:
			if err := s.resolveAndDerive(idx, changed); err != nil {
				return false, err
			}
			return true, nil
		case incAlmostSatisfied:
			s.deriveOnce(unsat.Negate(), idx, changed)
		case incContradicted, incInconclusive:
			// Nothing to derive from this incompatibility right now.
		}
	}
	return false, nil
}

// deriveOnce derives term (caused by causeIdx) and marks its package changed,
// unless an equivalent assignment for that package already exists. This is
// a defensive dedup, not a correctness crutch: deriving a content-identical
// fact twice is always sound to skip (a repeated derivation never carries
// new information), so this guard costs nothing and catches any accidental
// re-derivation regardless of cause - it is not what makes the solver
// terminate: the boundary-extended universe fix is what prevents the
// specific infinite re-derivation cycle this guard alone cannot stop; see
// conflict.go and term.go's extended-universe comments.
// withoutTautologicalTerms returns inc with any always-satisfiable
// (full-permitted) term removed, so conflict resolution's returned root cause
// relates as the unit clause it logically is: an always-true term is redundant
// inside an incompatibility ({A, always-true} is equivalent to {A}), and
// leaving it in would read inconclusive and break the learned-clause unit
// invariant after a backjump. A single-term or all-tautological incompatibility
// clause is returned unchanged, and dropTautological never returns the empty
// slice, so a genuine tautological leaf (the unknown-package "in any"
// incompatibility) is preserved rather than emptied here.
func (s *solveState) withoutTautologicalTerms(inc *incompatibility) *incompatibility {
	kept := dropTautological(inc.Terms, s.uniFor)
	if len(kept) == len(inc.Terms) {
		return inc
	}
	return &incompatibility{Terms: kept, Cause: inc.Cause}
}

func (s *solveState) deriveOnce(term term, causeIdx int, changed map[string]bool) {
	if s.ps.hasEquivalentAssignment(term) {
		return
	}
	s.ps.derive(term, causeIdx)
	changed[term.Package] = true
}

// resolveAndDerive drives resolveConflict to completion starting from the
// satisfied incompatibility at idx, deriving the resulting almost-satisfied
// term's negation into changed.
//
// A single resolveConflict call is not always enough: relation's Case C and
// conflict resolution's own satisfier search now share the same
// boundary-extended running intersection, but the incompatibility a
// backjump returns can still legitimately come back CONTRADICTED (the
// backjump undid exactly the assignment(s) that had made it satisfied,
// which can happen with a per-version-singleton dependency incompatibility
// or a no-versions leaf) or SATISFIED again (a completely different
// package's own state went empty in the meantime, which - over the extended
// universe - vacuously satisfies any positive term about it). Neither of
// those is a bug: they are legitimate intermediate states, and the loop
// below keeps calling resolveConflict until it lands on ALMOST_SATISFIED
// (derive the negation) or CONTRADICTED (nothing to derive - the backtrack
// alone resolved it). Only INCONCLUSIVE, or exceeding the iteration cap,
// signals a genuine defect.
func (s *solveState) resolveAndDerive(idx int, changed map[string]bool) error {
	for guard := range 10_000 {
		_ = guard
		rootIdx, rootCause, err := s.resolveConflict(idx)
		if err != nil {
			return err
		}
		rel, t := relate(s.withoutTautologicalTerms(rootCause), s.ps, s.uniFor)
		switch rel {
		case incAlmostSatisfied:
			clear(changed)
			s.deriveOnce(t.Negate(), rootIdx, changed)
			return nil
		case incContradicted:
			clear(changed)
			return nil
		case incSatisfied:
			idx = rootIdx
		case incInconclusive:
			return s.buildConflictError(rootCause)
		}
	}
	return s.buildConflictError(s.store.all[idx])
}

// popSmallest removes and returns the lexicographically smallest key from
// set, giving unit propagation's changed-set processing a deterministic,
// total-order pop sequence.
func popSmallest(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	smallest := slices.Min(keys)
	delete(set, smallest)
	return smallest
}
