package solver

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
// Every decision's term is built by singletonSet, which carries the version
// alongside its constraint representation for exactly this purpose.
func decisionVersionOf(t term) Version {
	if !t.Set.isSingleton {
		panic("solver: decision term does not carry a singleton version")
	}
	return t.Set.singleton
}

// packageAssignments is one package's bookkeeping inside a partialSolution:
// the append-order list of its assignment indices, its decision (if any),
// and - once the package is materialized - the incrementally maintained
// running intersection bitset of all its assignments' permitted sets, kept
// over the package's boundary-extended universe (term.go) rather than its
// published one. Decision making projects it down to published-only point
// cells (pointCellsOf) whenever it needs to pick or count actual published
// candidates; relation's Case C and conflict resolution's own satisfier
// search both read it directly in its extended form.
type packageAssignments struct {
	decisionVersion Version
	running         []uint64
	indices         []int32
	decisionIdx     int32
	materialized    bool
}

// partialSolution is Pubgrub's ordered assignment list plus the per-package
// indices and caches that make propagation and decision making efficient.
// uniFor resolves a package name to its materialization context; it is set
// once at construction by the owning solve state.
type partialSolution struct {
	uniFor      func(string) *packageUniverse
	packages    map[string]*packageAssignments
	assignments []assignment
	decisions   int
}

func newPartialSolution(uniFor func(string) *packageUniverse) *partialSolution {
	return &partialSolution{
		uniFor:   uniFor,
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
		p = &packageAssignments{decisionIdx: -1}
		ps.packages[pkg] = p
	}
	return p
}

// append adds one assignment to the partial solution and updates the
// affected package's bookkeeping, including the running intersection if
// that package is already materialized. It is the single mutation point
// decide and derive funnel through.
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
	if p.materialized {
		intersectExtAssignmentInto(p.running, term, ps.uniFor(term.Package))
	}
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
	term := term{Package: pkg, Set: singletonSet(v), Positive: true}
	ps.append(term, level, -1)
	ps.pkgState(pkg).decisionVersion = v
}

// derive records term as a derivation caused by the incompatibility at
// causeIndex, at the current decision level.
func (ps *partialSolution) derive(term term, causeIndex int) *assignment {
	return ps.append(term, ps.decisions, causeIndex)
}

// hasEquivalentAssignment reports whether term's package already carries an
// assignment content-identical to it (same polarity, same set identity).
// Deriving an already-recorded fact again would be a pure duplicate: unlike
// a genuinely new derivation, it can never change what relation() concludes
// about that package, so a caller that keeps re-deriving the same
// unsatisfied term (a materialized, bitset-typed external incompatibility
// like a no-versions leaf is never observable as satisfied by an
// unmaterialized package's Case D symbolic-key comparison, since that case
// is defined only for symbolic-vs-symbolic terms) would otherwise loop
// forever appending identical assignments without ever making progress.
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

// materializePackage builds pkg's running intersection bitset - over the
// boundary-extended universe, not the published one, so it stays exact for
// relation's Case C even when a package's published universe has shrunk to
// a handful of versions (see term.go's extended-universe comment) - by
// replaying every surviving assignment for it against its now-available
// universe. It is a no-op if pkg is already materialized (materialization
// only ever moves forward, never reverts).
func (ps *partialSolution) materializePackage(pkg string) {
	p := ps.pkgState(pkg)
	if p.materialized {
		return
	}
	u := ps.uniFor(pkg)
	p.running = fullExtBits(u)
	for _, idx := range p.indices {
		intersectExtAssignmentInto(p.running, ps.assignments[idx].term, u)
	}
	p.materialized = true
}

// backtrackTo truncates the assignment list to drop every assignment whose
// decision level exceeds level, then rebuilds every package's bookkeeping
// (including materialized running intersections) by replaying the survivors
// front to back. At Galaxy scale this full rebuild is microseconds, so no
// per-level snapshot machinery is kept around for it.
func (ps *partialSolution) backtrackTo(level int) {
	cut := len(ps.assignments)
	for cut > 0 && ps.assignments[cut-1].DecisionLevel > level {
		cut--
	}
	ps.assignments = ps.assignments[:cut]
	ps.decisions = level

	rebuilt := make(map[string]*packageAssignments, len(ps.packages))
	for i := range ps.assignments {
		a := &ps.assignments[i]
		p, ok := rebuilt[a.term.Package]
		if !ok {
			p = &packageAssignments{decisionIdx: -1}
			rebuilt[a.term.Package] = p
		}
		p.indices = append(p.indices, int32(i))
		if a.CauseIndex == -1 {
			p.decisionIdx = int32(i)
			p.decisionVersion = decisionVersionOf(a.term)
		}
	}

	// A package that was materialized before the backtrack, and still has at
	// least one surviving assignment, stays materialized (materialization
	// tracks universe availability, which backtracking does not undo); its
	// running intersection is rebuilt from the surviving assignments only.
	//
	// A package that loses every one of its assignments falls back to an
	// unmaterialized state instead of a freshly rebuilt "full universe, no
	// constraints" one: with zero real assignments there is nothing to
	// distinguish it from a package that was never touched, and staying
	// materialized would let its very next derivation (still symbolic, from
	// fresh dependency metadata) skip straight to exact bitset comparison
	// (Case C) - for a package whose universe happens to be very small, a
	// materialized comparison can find two symbolically-different
	// constraints bitwise identical (e.g. "<2.0.0" and "!=1.0.0" coincide
	// when 1.0.0 and 2.0.0 are the package's only two versions) before
	// there is enough real context to justify treating that coincidence as
	// meaningful. Falling back to symbolic Case D comparisons defers that
	// judgment exactly as it would for a never-yet-materialized package,
	// which is conservative (never unsound) and cheap to recover from: the
	// package's fetched universe and classification cache are untouched, so
	// the next materialization is a local replay, not a re-fetch.
	for pkg, old := range ps.packages {
		if !old.materialized {
			continue
		}
		p, ok := rebuilt[pkg]
		if !ok {
			continue
		}
		u := ps.uniFor(pkg)
		p.materialized = true
		p.running = fullExtBits(u)
		for _, idx := range p.indices {
			intersectExtAssignmentInto(p.running, ps.assignments[idx].term, u)
		}
	}
	ps.packages = rebuilt
}
