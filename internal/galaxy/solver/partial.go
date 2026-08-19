package solver

import "fmt"

// assignment is one entry in the partial solution's ordered list: either a
// decision (a concrete chosen version, CauseIndex == -1) or a derivation (a
// term forced by an incompatibility, CauseIndex pointing at that
// incompatibility in the store).
type assignment struct {
	term          term
	DecisionLevel int
	CauseIndex    int
	Index         int
}

// isDecision reports whether a is a decision rather than a derivation.
func (a *assignment) isDecision() bool {
	return a.CauseIndex == -1
}

// decisionVersionOf extracts the concrete version a decision's term denotes.
// Every decision's term is built by singletonVerSet, which carries the
// version alongside its set for exactly this purpose. It returns an error
// wrapping errSolverBug when t carries none, as an invariant assertion
// about how a decision term is built rather than a check against reachable
// input.
func decisionVersionOf(t term) (Version, error) {
	v, ok := t.Set.decidedVersion()
	if !ok {
		return Version{}, fmt.Errorf("decision term for %q does not carry a singleton version: %w", t.Package, errSolverBug)
	}
	return v, nil
}

// packageAssignments is one package's bookkeeping inside a partialSolution:
// the append-order list of its assignment indices, its decision (if any),
// and accum, the signed conjunction of every one of its assignments' terms
// (term.go), maintained incrementally from the very first assignment on.
// accum is exact regardless of whether the package's universe has been
// fetched - the property the whole migration to exact sets exists for.
type packageAssignments struct {
	decisionVersion Version
	indices         []int32
	accum           term
	decisionIdx     int32
}

// partialSolution is Pubgrub's ordered assignment list plus the per-package
// indices and signed accumulations that make propagation and decision
// making efficient.
type partialSolution struct {
	packages    map[string]*packageAssignments
	assignments []assignment
	decisions   int
}

func newPartialSolution() *partialSolution {
	return &partialSolution{
		packages: make(map[string]*packageAssignments),
	}
}

// currentLevel returns the current decision level: the number of real
// (non-root) decisions made so far.
func (ps *partialSolution) currentLevel() int {
	return ps.decisions
}

func (ps *partialSolution) pkgState(pkg string) *packageAssignments {
	p, ok := ps.packages[pkg]
	if !ok {
		p = &packageAssignments{decisionIdx: -1, accum: accumSeed(pkg)}
		ps.packages[pkg] = p
	}
	return p
}

// append adds one assignment to the partial solution and updates the
// affected package's bookkeeping, folding the term into its signed
// accumulation. It is the single mutation point decide and derive funnel
// through.
func (ps *partialSolution) append(term term, level, causeIndex int) *assignment {
	idx := len(ps.assignments)
	ps.assignments = append(ps.assignments, assignment{
		term:          term,
		DecisionLevel: level,
		CauseIndex:    causeIndex,
		Index:         idx,
	})
	a := &ps.assignments[idx]
	p := ps.pkgState(term.Package)
	//nolint:gosec // G115: assignment indices are small, bounded by the fuel limit
	p.indices = append(p.indices, int32(idx))
	if causeIndex == -1 {
		p.decisionIdx = int32(idx) //nolint:gosec // G115: bounded by the fuel limit
	}
	p.accum = termIntersect(p.accum, term)
	return a
}

// decide records pkg@v as a decision. The root package is always decided at
// level 0 and never advances the decision counter; every other package's
// first decision is level 1, its next is level 2, and so on. No caller needs
// the resulting assignment, only the side effect of it being recorded.
func (ps *partialSolution) decide(pkg string, v Version) {
	level := ps.decisions
	if pkg != rootPkg {
		ps.decisions++
		level = ps.decisions
	}
	term := term{Package: pkg, Set: singletonVerSet(v), Positive: true}
	ps.append(term, level, -1)
	ps.pkgState(pkg).decisionVersion = v
}

// derive records term as a derivation caused by the incompatibility at
// causeIndex, at the current decision level.
func (ps *partialSolution) derive(term term, causeIndex int) *assignment {
	return ps.append(term, ps.decisions, causeIndex)
}

// hasEquivalentAssignment reports whether term's package already carries an
// assignment content-identical to it (same polarity, same set). Deriving an
// already-recorded fact again would be a pure duplicate: it can never
// change what relation() concludes about that package, so skipping it is
// always sound and keeps any accidental re-derivation from appending
// identical assignments without ever making progress.
func (ps *partialSolution) hasEquivalentAssignment(term term) bool {
	p, ok := ps.packages[term.Package]
	if !ok {
		return false
	}
	for _, idx := range p.indices {
		if sameTerm(ps.assignments[idx].term, term) {
			return true
		}
	}
	return false
}

// rebuildPackageAssignments replays ps.assignments front to back, building
// each named package's index list, signed accumulation, and - for a
// decision-shaped assignment (CauseIndex == -1) - its decisionVersion. It
// is backtrackTo's rebuild step, and it owns the partially built map for
// that map's whole lifetime: the first decisionVersionOf failure returns a
// nil map alongside the error, so a map built from only a prefix of the
// assignments can never reach a caller. ps.packages sizes the result -
// backtracking only ever drops assignments, so the packages the survivors
// name are a subset of the ones tracked there.
func (ps *partialSolution) rebuildPackageAssignments() (map[string]*packageAssignments, error) {
	rebuilt := make(map[string]*packageAssignments, len(ps.packages))
	for i := range ps.assignments {
		a := &ps.assignments[i]
		p, ok := rebuilt[a.term.Package]
		if !ok {
			p = &packageAssignments{decisionIdx: -1, accum: accumSeed(a.term.Package)}
			rebuilt[a.term.Package] = p
		}
		p.indices = append(p.indices, int32(i))
		p.accum = termIntersect(p.accum, a.term)
		if a.CauseIndex == -1 {
			p.decisionIdx = int32(i)
			v, err := decisionVersionOf(a.term)
			if err != nil {
				return nil, err
			}
			p.decisionVersion = v
		}
	}
	return rebuilt, nil
}

// backtrackTo truncates the assignment list to drop every assignment whose
// decision level exceeds level, then rebuilds every package's bookkeeping
// (including signed accumulations) by replaying the survivors front to
// back. At Galaxy scale this full rebuild is microseconds, so no per-level
// snapshot machinery is kept around for it. It returns the first error
// decisionVersionOf reports; the truncation above has already happened by
// then, while ps.packages is left untouched, so a map rebuilt from only a
// prefix of the survivors never replaces the live one. A caller that gets
// an error must abandon this partial solution rather than continue against
// it. A package that loses every one of its assignments simply drops out of
// the rebuilt map: with exact sets there is nothing to preserve for it -
// two symbolically different constraints that denote the same set ARE the
// same set, so no state distinguishes it from a never-touched package.
func (ps *partialSolution) backtrackTo(level int) error {
	cut := len(ps.assignments)
	for cut > 0 && ps.assignments[cut-1].DecisionLevel > level {
		cut--
	}
	ps.assignments = ps.assignments[:cut]
	ps.decisions = level

	rebuilt, err := ps.rebuildPackageAssignments()
	if err != nil {
		return err
	}
	ps.packages = rebuilt
	return nil
}
