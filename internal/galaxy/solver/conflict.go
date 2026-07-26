package solver

import "fmt"

// resolveConflict resolves the conflict represented by the incompatibility
// stored at startIdx, backtracking the partial solution and returning
// (index, incompatibility) of the incompatibility that is now guaranteed to
// be almost satisfied - the "root cause" unit propagation continues from.
// If no solution can exist, it returns a *ConflictError.
//
// Satisfier-finding and merge arithmetic inside this loop operate on the
// boundary-extended universe (term.go), the same representation the partial
// solution's running intersection and relation's Case C now use: a package
// whose published universe has few members otherwise looks tautologically
// true to a term that only actually holds because of a specific, informative
// assignment, and settling for that tautology as "the satisfier" would merge
// with a content-free external leaf and permanently lose the dependency edge
// that explains why the package was relevant at all (see term.go's
// boundary-extended-universe comment). Every incompatibility this function
// returns or stores stays in whatever representation it was built in
// (extended-universe terms flow straight into the store and into relation's
// Case C without any projection step); only decision making, which can only
// ever choose a published version, ever needs to collapse an extended
// running intersection down to published-only cells (pointCellsOf).
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
		if err := s.materializeTerms(inc.Terms); err != nil {
			return 0, nil, err
		}
		if inc.isTerminal() {
			return 0, nil, s.buildConflictError(inc)
		}

		satisfier, term := s.earliestSatisfier(inc)
		if satisfier == nil {
			// The reference algorithm assumes a satisfier always exists for
			// a genuinely satisfied incompatibility; under the
			// boundary-extended universe this holds for every package
			// (unlike the old published-only reading, where a one-version
			// package's term could be vacuously "true" from the very
			// start). Reaching this means the assumption broke - a defect,
			// not a legitimate proof - so fail loudly instead of guessing.
			return 0, nil, fmt.Errorf("no satisfier found for a satisfied incompatibility: %w", errSolverBug)
		}
		prevLevel := s.prevSatisfierLevel(inc, satisfier)

		// A no-versions/unknown-package leaf whose single term is satisfied by
		// a derivation (a dependency assignment from some parent version), not
		// a decision, is resolved rather than backjumped: backjumping learns
		// only the leaf itself and drops the attribution from the empty package
		// back to the parent version that required it, so a transitively
		// unsatisfiable higher version is never learned as "not parent version"
		// and the solvable graph is falsely rejected. Merging instead reaches
		// the parent version's dependency and lets conflict resolution learn
		// the real, conditional cause.
		if s.shouldBackjump(inc, satisfier, prevLevel) {
			return s.backjump(inc, curIdx, incChanged, prevLevel)
		}

		next, err := s.mergeWithSatisfierCause(inc, satisfier, term)
		if err != nil {
			return 0, nil, err
		}
		inc = next
		incChanged = true
	}
	return 0, nil, fmt.Errorf("resolveConflict did not converge: %w", errSolverBug)
}

// shouldBackjump decides resolveConflict's terminate-vs-resolve step. The
// reference rule backjumps when the satisfier is a decision or comes from a
// different level than its own previous satisfier; the one exception is a
// no-versions/unknown-package leaf satisfied by a derivation, which is resolved
// instead so the merge reaches the parent version that required the empty
// package and its attribution survives into the learned clause.
func (s *solveState) shouldBackjump(inc *incompatibility, satisfier *assignment, prevLevel int) bool {
	if s.isExternalLeaf(inc) && !satisfier.isDecision() {
		return false
	}
	return satisfier.isDecision() || prevLevel != satisfier.DecisionLevel
}

// isExternalLeaf reports whether inc is a single-term leaf recorded directly
// by decision making (a no-versions or unknown-package incompatibility).
func (s *solveState) isExternalLeaf(inc *incompatibility) bool {
	if len(inc.Terms) != 1 {
		return false
	}
	switch inc.Cause.(type) {
	case causeNoVersions, causeUnknownPackage:
		return true
	default:
		return false
	}
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
	s.ps.backtrackTo(prevLevel)
	return curIdx, inc, nil
}

// mergeWithSatisfierCause performs one step of conflict resolution's
// generalized resolution rule: merges inc with the satisfier's own cause
// (excluding the satisfier's package), adding a partial-satisfier correction
// term when the satisfier's assignment does not, on its own, satisfy inc's
// term for that package (satisfierTerm).
func (s *solveState) mergeWithSatisfierCause(inc *incompatibility, satisfier *assignment, satisfierTerm term) (*incompatibility, error) {
	cause := s.store.all[satisfier.CauseIndex]
	if err := s.materializeTerms(cause.Terms); err != nil {
		return nil, err
	}

	uni := s.uniFor(satisfier.term.Package)
	prior := mergeTermsExcluding(inc, cause, satisfier.term.Package)
	if !termSatisfies(satisfier.term, satisfierTerm, uni) {
		prior = append(prior, negatedDifferenceTerm(satisfier.term, satisfierTerm, uni))
	}

	return &incompatibility{
		Terms: normalizeTerms(prior, s.uniFor),
		Cause: causeConflict{Left: inc, Right: cause},
	}, nil
}

// materializeTerms materializes every package named in terms that is not
// already materialized.
func (s *solveState) materializeTerms(terms []term) error {
	for _, t := range terms {
		if err := s.materializePkg(t.Package); err != nil {
			return err
		}
	}
	return nil
}

// mergeTermsExcluding collects the terms of a and b except any naming
// exclude, ready for normalizeTerms to merge duplicate packages (via
// extended-universe bitset intersection) and drop redundant positive root
// terms. This is the "priorCause" step of conflict resolution's generalized
// resolution rule.
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

// termSatisfies reports whether a's permitted set is a subset of b's over
// the boundary-extended universe - i.e. whether a on its own, without
// anything else, already satisfies b. a and b must name the same package.
func termSatisfies(a, b term, uni *packageUniverse) bool {
	return subset(permittedExtBits(a, uni), permittedExtBits(b, uni))
}

// negatedDifferenceTerm returns "not (satisfierTerm \ term)" for
// satisfierTerm's package, computed over the boundary-extended universe:
// the partial-satisfier correction term added to the prior cause when the
// satisfier's assignment does not, on its own, satisfy the incompatibility's
// term for that package.
func negatedDifferenceTerm(satisfierTerm, incTerm term, uni *packageUniverse) term {
	diff := differenceNew(permittedExtBits(satisfierTerm, uni), permittedExtBits(incTerm, uni))
	return term{Package: satisfierTerm.Package, Set: uni.asExtBitsetSet(diff, "(difference)"), Positive: false}
}

// computeFirstSatisfied scans assignments[0:limit] forward, maintaining a
// per-package running intersection bitset over the boundary-extended
// universe (seeded with seed's contribution for its own package, if seed is
// non-nil - representing a satisfier pinned in regardless of prefix
// length), and returns, for every package named in inc, the index of the
// first assignment after which that package's running intersection
// satisfies inc's term for it. When seed is non-nil (the previousSatisfier
// computation), a package already satisfied by the seed alone - before any
// prefix assignment is scanned - maps to the -1 sentinel directly: the seed
// is a real assignment, so "satisfied by the seed alone" is the legitimate
// no-earlier-satisfier answer. When seed is nil (the forward satisfier
// scan), a term the full extended universe already satisfies before any
// real assignment exists is only ever true for a tautological term (one
// whose permitted set spans every cell, as causeUnknownPackage's "in any"
// leaf does) - such a term still needs a real assignment establishing it
// before it can map to -1, so it is tracked separately and only falls back
// to the sentinel once the forward scan finishes without ever satisfying it
// through a real assignment. A package genuinely never satisfied within the
// scanned prefix, and never vacuous, is absent from the result.
func (s *solveState) computeFirstSatisfied(inc *incompatibility, seed *term, limit int) map[string]int {
	firstIdx := make(map[string]int, len(inc.Terms))
	done := make(map[string]bool, len(inc.Terms))
	running := make(map[string][]uint64, len(inc.Terms))
	vacuous := s.seedRunningIntersections(inc, seed, running, firstIdx, done)

	for i := range limit {
		a := &s.ps.assignments[i]
		t, ok := inc.termForPackage(a.term.Package)
		if !ok || done[t.Package] {
			continue
		}
		u := s.uniFor(t.Package)
		rb := running[t.Package]
		intersectExtAssignmentInto(rb, a.term, u)
		if subsetOfPermittedExt(rb, t, u) {
			firstIdx[t.Package] = i
			done[t.Package] = true
		}
	}
	for pkg := range vacuous {
		if !done[pkg] {
			firstIdx[pkg] = -1
		}
	}
	return firstIdx
}

// seedRunningIntersections initializes running with every inc term's
// package's full-extended-universe intersection (folding in seed's own
// contribution, if seed names that package), and reports which packages are
// vacuous: already satisfied before any real assignment is scanned, purely
// by the full universe's own permitted set.
//
// A positive term is only vacuously satisfiable when it is tautological
// (its permitted set spans every cell, as causeUnknownPackage's "in any"
// leaf does), and the reference algorithm's semantics require a positive
// term to be satisfied by a real positive assignment, not by the empty
// prefix. So when seed is nil (the forward satisfier scan), a vacuous
// package is only recorded in the returned set, not settled into
// firstIdx/done at once: computeFirstSatisfied's forward scan still gets a
// chance to find a real assignment establishing the term, falling back to
// the -1 sentinel only once that scan finishes without ever satisfying it.
// When seed is non-nil (the previousSatisfier computation), a vacuous
// package is instead recorded into firstIdx/done immediately: the seed
// itself is a real assignment, so "satisfied by the seed alone" is the
// legitimate no-earlier-satisfier answer that computation exists to report.
func (s *solveState) seedRunningIntersections(
	inc *incompatibility,
	seed *term,
	running map[string][]uint64,
	firstIdx map[string]int,
	done map[string]bool,
) map[string]bool {
	vacuous := make(map[string]bool, len(inc.Terms))
	for _, t := range inc.Terms {
		u := s.uniFor(t.Package)
		rb := fullExtBits(u)
		running[t.Package] = rb
		if seed != nil && seed.Package == t.Package {
			intersectExtAssignmentInto(rb, *seed, u)
		}
		if !subsetOfPermittedExt(rb, t, u) {
			continue
		}
		if seed != nil {
			firstIdx[t.Package] = -1
			done[t.Package] = true
		} else {
			vacuous[t.Package] = true
		}
	}
	return vacuous
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
