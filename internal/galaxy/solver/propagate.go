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

// relation answers whether the partial solution satisfies, contradicts, or
// is inconclusive for term. With exact signed accumulations the answer is
// exact from the first assignment on and never consults a package's
// published universe - matching the reference algorithm, where
// unsatisfiability against the published universe enters the derivation
// graph only through decision making's no-versions incompatibilities,
// never through relation itself. A package with no assignments at all is
// inconclusive: its vacuous N({}) seed would judge negative terms
// satisfied by nothing, and the reference semantics require a real
// assignment before anything is derived about it.
func relation(term term, ps *partialSolution) termRelation {
	p, ok := ps.packages[term.Package]
	if !ok || len(p.indices) == 0 {
		return termInconclusive
	}
	return relateAccum(p.accum, term)
}

// relate answers whether the partial solution satisfies, contradicts, is
// inconclusive for, or almost satisfies inc (all terms but one are
// satisfied, the remaining one inconclusive - that remaining term is
// returned alongside).
func relate(inc *incompatibility, ps *partialSolution) (incRelation, term) {
	var unsat term
	hasUnsat := false
	for _, t := range inc.Terms {
		switch relation(t, ps) {
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
// Propagation performs no I/O: term arithmetic is exact without any
// universe, so the provider is reached only from decision making.
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
		rel, unsat := relate(inc, s.ps)
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
// re-derivation regardless of cause.
func (s *solveState) deriveOnce(term term, causeIdx int, changed map[string]bool) {
	if s.ps.hasEquivalentAssignment(term) {
		return
	}
	s.ps.derive(term, causeIdx)
	changed[term.Package] = true
}

// resolveAndDerive runs resolveConflict on the satisfied incompatibility at
// idx and derives the resulting root cause's almost-satisfied term's
// negation into changed. Under signed exact terms the root cause a backjump
// returns is ALMOST_SATISFIED by construction, exactly as the reference
// algorithm states: the backtrack keeps every assignment at or below the
// previous satisfier's level, which keeps every non-satisfier term
// satisfied, while the satisfier's own term is neither satisfied (the
// satisfier, the first assignment to complete it, is dropped) nor
// contradicted (a prefix that contradicted it could never have been
// completed into satisfaction without the accumulation reaching the
// unsatisfiable P({}), which no derivation or decision can produce - a
// derivation's negation is only ever folded into an accumulation it was
// inconclusive against, and a decision is always drawn from the allowed
// candidates).
//
// Any other relation therefore signals a defect in this package's own
// bookkeeping, presented as a clean resolution failure rather than a panic
// or a hang: buildConflictError hands back whatever invariant violation the
// report walk recorded, and a plain *ConflictError when it recorded none.
// That is deliberate for the CI consumer this tool serves - a loud refusal
// costs a pipeline one red run, while a panic or a hang costs it the
// diagnosis - and it ends the run in an error rather than in an install of
// something the solver never proved.
// TestNonConvergingConflictIsCleanFailure pins that presentation.
func (s *solveState) resolveAndDerive(idx int, changed map[string]bool) error {
	rootIdx, rootCause, err := s.resolveConflict(idx)
	if err != nil {
		return err
	}
	rel, t := relate(rootCause, s.ps)
	if rel != incAlmostSatisfied {
		return s.buildConflictError(rootCause)
	}
	clear(changed)
	s.deriveOnce(t.Negate(), rootIdx, changed)
	return nil
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
