package solver

import (
	"fmt"
)

// resolveConflict resolves the conflict represented by the incompatibility
// stored at startIdx, backtracking the partial solution and returning
// (index, incompatibility) of the incompatibility that is now guaranteed to
// be almost satisfied - the "root cause" unit propagation continues from.
// If no solution can exist, it returns a *ConflictError.
//
// Satisfier-finding and merge arithmetic operate on signed exact terms
// (term.go), the same representation the partial solution's accumulations
// and relation() use, so a satisfier always exists for a genuinely
// satisfied incompatibility: nothing is ever vacuously true against the
// N({}) seed, and every judgment here is exact without any provider call.
func (s *solveState) resolveConflict(startIdx int) (int, *incompatibility, error) {
	inc := s.store.all[startIdx]
	curIdx := startIdx

	// conflictCount is bumped once per resolveConflict entry, for every
	// package named in the ORIGINAL conflicting incompatibility - not on
	// every iteration of the loop below, which works with increasingly
	// general derived incompatibilities.
	for _, t := range inc.Terms {
		s.conflictCounts[t.Package]++
	}

	incChanged := false
	// A defensive iteration cap: each iteration strictly generalizes inc via
	// resolution, which terminates by construction once it reaches root or a
	// backjump point, so exceeding this bound signals a defect rather than a
	// large input - mirroring the main solve loop's own fuel guard.
	for guard := range 10_000 {
		_ = guard
		if inc.isTerminal() {
			return 0, nil, s.buildConflictError(inc)
		}

		satisfier, term := s.earliestSatisfier(inc)
		if satisfier == nil {
			// The reference algorithm guarantees a satisfier exists for a
			// genuinely satisfied incompatibility, and signed exact terms
			// uphold that: no term is satisfied by the empty prefix (a
			// positive term needs a positive assignment, and tautological
			// negative terms never enter the store). Reaching this means the
			// assumption broke - a defect, not a legitimate proof - so fail
			// loudly instead of guessing.
			return 0, nil, fmt.Errorf("no satisfier found for a satisfied incompatibility: %w", errSolverBug)
		}
		prevLevel := s.prevSatisfierLevel(inc, satisfier)

		if shouldBackjump(satisfier, prevLevel) {
			return s.backjump(inc, curIdx, incChanged, prevLevel)
		}

		inc = s.mergeWithSatisfierCause(inc, satisfier, term)
		incChanged = true
	}
	return 0, nil, s.buildConflictError(inc)
}

// shouldBackjump decides resolveConflict's terminate-vs-resolve step: the
// reference rule verbatim, backjumping when the satisfier is a decision or
// comes from a different level than its own previous satisfier. No
// exception for no-versions/unknown-package leaves is needed under signed
// exact terms: after a plain backjump on such a leaf, propagation derives
// the leaf term's negation, the parent's dependency incompatibility then
// relates SATISFIED through the negative-negative entailment, and the
// ordinary propagate-resolve cycle merges through to "not parent@version" -
// the attribution the retired exception used to force inside a single
// resolveConflict call.
func shouldBackjump(satisfier *assignment, prevLevel int) bool {
	return satisfier.isDecision() || prevLevel != satisfier.DecisionLevel
}

// prevSatisfierLevel returns the decision level of the earliest assignment
// strictly before satisfier that, together with satisfier pinned in, still
// satisfies inc - or 0 if satisfier alone (with nothing before it) already
// suffices for every term.
func (s *solveState) prevSatisfierLevel(inc *incompatibility, satisfier *assignment) int {
	prevSatisfier := s.earliestSatisfierBefore(inc, satisfier)
	if prevSatisfier == nil {
		return 0
	}
	return prevSatisfier.DecisionLevel
}

// backjump implements resolveConflict's termination condition: the satisfier
// is itself a decision, or comes from a strictly different decision level
// than its own previous satisfier. It stores inc (only if this call's loop
// actually changed it from the original startIdx entry), backtracks the
// partial solution to prevLevel, and returns the (index, incompatibility)
// pair unit propagation continues from.
func (s *solveState) backjump(inc *incompatibility, curIdx int, incChanged bool, prevLevel int) (int, *incompatibility, error) {
	if incChanged {
		idx, _ := s.store.add(inc)
		curIdx = idx
	}
	if err := s.ps.backtrackTo(prevLevel); err != nil {
		return 0, nil, err
	}
	return curIdx, inc, nil
}

// mergeWithSatisfierCause performs one step of conflict resolution's
// generalized resolution rule: merges inc with the satisfier's own cause
// (excluding the satisfier's package), adding a partial-satisfier correction
// term when the satisfier's assignment does not, on its own, satisfy inc's
// term for that package (satisfierTerm).
func (s *solveState) mergeWithSatisfierCause(inc *incompatibility, satisfier *assignment, satisfierTerm term) *incompatibility {
	cause := s.store.all[satisfier.CauseIndex]
	prior := mergeTermsExcluding(inc, cause, satisfier.term.Package)
	if !termSubset(satisfier.term, satisfierTerm) {
		prior = append(prior, negatedDifferenceTerm(satisfier.term, satisfierTerm))
	}

	return &incompatibility{
		Terms: normalizeTerms(prior),
		Cause: causeConflict{Left: inc, Right: cause},
	}
}

// mergeTermsExcluding collects the terms of a and b except any naming
// exclude, ready for normalizeTerms to merge duplicate packages (via signed
// term intersection) and drop redundant positive root terms. This is the
// "priorCause" step of conflict resolution's generalized resolution rule.
func mergeTermsExcluding(a, b *incompatibility, exclude string) []term {
	collected := make([]term, 0, len(a.Terms)+len(b.Terms))
	for _, t := range a.Terms {
		if t.Package != exclude {
			collected = append(collected, t)
		}
	}
	for _, t := range b.Terms {
		if t.Package != exclude {
			collected = append(collected, t)
		}
	}
	return collected
}

// negatedDifferenceTerm returns "not (satisfierTerm minus incTerm)": the
// partial-satisfier correction term added to the prior cause when the
// satisfier's assignment does not, on its own, satisfy the incompatibility's
// term for its package. The subtraction is the signed conjunction of the
// satisfier's term with the incompatibility term's negation.
func negatedDifferenceTerm(satisfierTerm, incTerm term) term {
	return termIntersect(satisfierTerm, incTerm.Negate()).Negate()
}

// computeFirstSatisfied scans assignments[0:limit] forward, maintaining a
// per-package signed accumulation (seeded with seed's contribution for its
// own package, if seed is non-nil - representing a satisfier pinned in
// regardless of prefix length), and returns, for every package named in
// inc, the index of the first assignment after which that package's
// accumulation satisfies inc's term for it. When seed is non-nil (the
// previousSatisfier computation), a package already satisfied by the seed
// alone - before any prefix assignment is scanned - maps to the -1 sentinel
// directly: the seed is a real assignment, so "satisfied by the seed alone"
// is the legitimate no-earlier-satisfier answer. When seed is nil, nothing
// is satisfied before a real assignment is folded in: the N({}) seed never
// entails a positive term, and tautological negative terms never enter the
// store. A package never satisfied within the scanned prefix is absent from
// the result.
func (s *solveState) computeFirstSatisfied(inc *incompatibility, seed *term, limit int) map[string]int {
	firstIdx := make(map[string]int, len(inc.Terms))
	done := make(map[string]bool, len(inc.Terms))
	running := make(map[string]term, len(inc.Terms))
	for _, t := range inc.Terms {
		acc := accumSeed(t.Package)
		if seed != nil && seed.Package == t.Package {
			acc = termIntersect(acc, *seed)
			if termSubset(acc, t) {
				firstIdx[t.Package] = -1
				done[t.Package] = true
			}
		}
		running[t.Package] = acc
	}

	for i := range limit {
		a := &s.ps.assignments[i]
		t, ok := inc.termForPackage(a.term.Package)
		if !ok || done[t.Package] {
			continue
		}
		acc := termIntersect(running[t.Package], a.term)
		running[t.Package] = acc
		if termSubset(acc, t) {
			firstIdx[t.Package] = i
			done[t.Package] = true
		}
	}
	return firstIdx
}

// earliestSatisfier finds the earliest assignment such that the partial
// solution up to and including it satisfies inc, plus inc's term for the
// same package.
func (s *solveState) earliestSatisfier(inc *incompatibility) (*assignment, term) {
	firstIdx := s.computeFirstSatisfied(inc, nil, len(s.ps.assignments))
	maxIdx := -1
	maxPkg := ""
	for _, t := range inc.Terms {
		idx, ok := firstIdx[t.Package]
		if !ok {
			idx = -1
		}
		if idx > maxIdx {
			maxIdx = idx
			maxPkg = t.Package
		}
	}
	term, _ := inc.termForPackage(maxPkg)
	if maxIdx < 0 {
		return nil, term
	}
	return &s.ps.assignments[maxIdx], term
}

// earliestSatisfierBefore finds the earliest assignment strictly before
// satisfier such that the partial solution up to and including it, plus
// satisfier itself pinned in, satisfies inc. It returns nil if satisfier
// alone (with nothing before it) already suffices for every term.
func (s *solveState) earliestSatisfierBefore(inc *incompatibility, satisfier *assignment) *assignment {
	if satisfier == nil {
		return nil
	}
	firstIdx := s.computeFirstSatisfied(inc, &satisfier.term, satisfier.Index)
	maxIdx := -1
	for _, t := range inc.Terms {
		idx, ok := firstIdx[t.Package]
		if !ok {
			idx = -1
		}
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	if maxIdx < 0 {
		return nil
	}
	return &s.ps.assignments[maxIdx]
}
