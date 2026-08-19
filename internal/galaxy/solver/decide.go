package solver

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// exactPinVersion reports whether set denotes an exact single version -
// mirroring exactVersionFromConstraints's classification: strip one leading
// "=" (helpers.NormalizeConstraint has already rewritten "==" to "="), then
// the result is exact only if it parses as a bare semver.Version.
func exactPinVersion(set versionSet) (Version, bool) {
	if set.kind != setSymbolic || set.key == "" {
		return Version{}, false
	}
	candidate := set.key
	if after, ok := strings.CutPrefix(candidate, "="); ok {
		candidate = strings.TrimSpace(after)
	}
	v, err := semver.NewVersion(candidate)
	if err != nil {
		return Version{}, false
	}
	return Version{parsed: v, original: candidate}, true
}

// packageIsExactPin reports whether pkg currently has an exact pin: either a
// positive symbolic assignment classified as an exact version, or (once
// materialized) a running intersection whose published-candidate projection
// has a population count of 1. The running intersection itself is kept over
// the boundary-extended universe (partial.go), so it is projected down to
// published-only point cells (pointCellsOf) before counting - a published
// candidate is the only kind decision making can ever select.
func (s *solveState) packageIsExactPin(pkg string) (Version, bool) {
	p := s.ps.pkgState(pkg)
	if p.materialized {
		pts := pointCellsOf(p.running, s.uniFor(pkg))
		if popcount(pts) != 1 {
			return Version{}, false
		}
		idx, ok := lowestSetBit(pts)
		if !ok {
			return Version{}, false
		}
		return s.uniFor(pkg).versions[idx], true
	}
	for _, idx := range p.indices {
		t := s.ps.assignments[idx].term
		if !t.Positive {
			continue
		}
		if v, ok := exactPinVersion(t.Set); ok {
			return v, true
		}
	}
	return Version{}, false
}

// versionPassesAssignments reports whether v satisfies every assignment
// currently recorded for pkg, honoring each assignment's polarity. It is
// only used while pkg is unmaterialized (all its assignments symbolic), so
// membership goes straight through Contains.
func versionPassesAssignments(pkg string, v Version, ps *partialSolution) bool {
	p := ps.pkgState(pkg)
	for _, idx := range p.indices {
		t := ps.assignments[idx].term
		if t.Set.Contains(v) != t.Positive {
			return false
		}
	}
	return true
}

// allPositiveSymbolic reports whether pkg has at least one assignment and
// every one of them is a positive, still-symbolic term - the situation in
// which the provider.Highest probe is worth trying before ever fetching a
// full universe.
func allPositiveSymbolic(pkg string, ps *partialSolution) bool {
	p := ps.pkgState(pkg)
	if len(p.indices) == 0 {
		return false
	}
	for _, idx := range p.indices {
		t := ps.assignments[idx].term
		if !t.Positive || t.Set.kind != setSymbolic {
			return false
		}
	}
	return true
}

// candidatePackages returns, sorted ascending by name, every package that
// has at least one positive derivation and no decision yet - the pool
// package prioritization (9.1) chooses from.
func (s *solveState) candidatePackages() []string {
	names := make([]string, 0, len(s.ps.packages))
	for pkg, p := range s.ps.packages {
		if p.decisionIdx != -1 {
			continue
		}
		for _, idx := range p.indices {
			if s.ps.assignments[idx].term.Positive {
				names = append(names, pkg)
				break
			}
		}
	}
	slices.Sort(names)
	return names
}

// pickPackage selects the next package to decide per the frozen
// prioritization: exact pins first, then packages that have caused a
// conflict (higher conflict count first), then materialized packages with
// the fewest allowed candidates, then everything else - ties broken
// ascending by name throughout, which candidatePackages' own sort already
// guarantees as the stable base order every class filter preserves.
func (s *solveState) pickPackage() (string, bool) {
	names := s.candidatePackages()
	if len(names) == 0 {
		return "", false
	}

	for _, n := range names {
		if _, ok := s.packageIsExactPin(n); ok {
			return n, true
		}
	}

	if pkg, ok := s.pickByConflictCount(names); ok {
		return pkg, true
	}

	if pkg, ok := s.pickByFewestCandidates(names); ok {
		return pkg, true
	}

	return names[0], true
}

func (s *solveState) pickByConflictCount(names []string) (string, bool) {
	best := ""
	bestCount := 0
	for _, n := range names {
		c := s.conflictCounts[n]
		if c > bestCount {
			best = n
			bestCount = c
		}
	}
	return best, best != ""
}

func (s *solveState) pickByFewestCandidates(names []string) (string, bool) {
	best := ""
	bestCount := -1
	for _, n := range names {
		p := s.ps.pkgState(n)
		if !p.materialized {
			continue
		}
		c := popcount(pointCellsOf(p.running, s.uniFor(n)))
		if bestCount == -1 || c < bestCount {
			best = n
			bestCount = c
		}
	}
	return best, best != ""
}

// pickRequiredDependencyTarget is decision making's fallback for when the
// positive-derivation candidate pool is empty: it returns the lowest-named
// undecided package that a currently-decided parent depends on. Such a
// package is required by that decided parent, yet can carry only a negative
// assignment - a residual left by an earlier, backtracked parent version
// whose own dependency on it had no matching version makes its later
// dependency term read as already satisfied (running subset of the required
// range) rather than deriving a fresh positive requirement. Deciding it here
// honors the same dependency edges the completeness guard enforces, and the
// store scan stays off the decision hot path since it runs only once the
// ordinary candidate pool is exhausted.
func (s *solveState) pickRequiredDependencyTarget() (string, bool) {
	best := ""
	for _, inc := range s.store.all {
		dep, ok := inc.Cause.(causeDependency)
		if !ok {
			continue
		}
		if best != "" && dep.Dep >= best {
			continue
		}
		parent := s.ps.packages[dep.Parent]
		if parent == nil || parent.decisionIdx == -1 ||
			parent.decisionVersion.Original() != dep.ParentVersion.Original() {
			continue
		}
		child := s.ps.packages[dep.Dep]
		if child != nil && child.decisionIdx != -1 {
			continue
		}
		best = dep.Dep
	}
	return best, best != ""
}

// decisionOutcome carries makeDecision's early-exit result: a fast-path
// helper returns a non-nil outcome when the caller should return it as-is,
// or nil when the caller should fall through to decideFromAllowed instead.
type decisionOutcome struct {
	err  error
	pkg  string
	done bool
}

// makeDecision implements decision making: pick a package, resolve it to a
// concrete version as cheaply as possible (the exact-pin and highest-probe
// fast paths avoid ever fetching a universe when they can), and either
// decide it or report an empty-candidate/unknown-package incompatibility so
// the next propagation round can act on it.
func (s *solveState) makeDecision(ctx context.Context) (string, bool, error) {
	pkg, ok := s.pickPackage()
	if !ok {
		pkg, ok = s.pickRequiredDependencyTarget()
		if !ok {
			return "", true, nil
		}
	}
	if out := s.tryFastDecide(ctx, pkg); out != nil {
		return out.pkg, out.done, out.err
	}
	return s.decideFromAllowed(ctx, pkg)
}

// tryFastDecide attempts pkg's decision fast paths in priority order (exact
// pin, then the provider.Highest probe), materializing pkg along the way
// when a fast path cannot be confirmed cheaply. Returns nil when no fast
// path applied and the caller should fall through to decideFromAllowed.
func (s *solveState) tryFastDecide(ctx context.Context, pkg string) *decisionOutcome {
	p := s.ps.pkgState(pkg)
	if vp, isPin := s.packageIsExactPin(pkg); isPin {
		return s.tryDecidePin(ctx, pkg, p, vp)
	}
	if !p.materialized && allPositiveSymbolic(pkg, s.ps) {
		return s.tryDecideByProbe(ctx, pkg)
	}
	if !p.materialized {
		if err := s.materializePkg(ctx, pkg); err != nil {
			return &decisionOutcome{err: err}
		}
	}
	return nil
}

// tryDecidePin attempts to decide pkg at its already-known exact pin vp
// without a universe fetch when possible (either pkg is already
// materialized, or the pin already passes every symbolic assignment on
// record), materializing pkg otherwise so decideFromAllowed can re-verify
// the pin for real. Returns nil when the caller should fall through to
// decideFromAllowed itself.
func (s *solveState) tryDecidePin(ctx context.Context, pkg string, p *packageAssignments, vp Version) *decisionOutcome {
	if p.materialized || versionPassesAssignments(pkg, vp, s.ps) {
		pkg, done, err := s.decideVersion(ctx, pkg, vp)
		return &decisionOutcome{pkg: pkg, done: done, err: err}
	}
	if err := s.materializePkg(ctx, pkg); err != nil {
		return &decisionOutcome{err: err}
	}
	return nil
}

// tryDecideByProbe attempts the provider.Highest probe fast path before ever
// materializing pkg's full universe, materializing it (but not deciding) if
// the probe is unavailable or its candidate does not satisfy the
// accumulated assignments. Returns nil when the caller should fall through
// to decideFromAllowed itself.
func (s *solveState) tryDecideByProbe(ctx context.Context, pkg string) *decisionOutcome {
	v, ok, err := s.provider.Highest(ctx, pkg)
	if err != nil {
		return &decisionOutcome{err: fmt.Errorf("probing highest version of %s: %w", pkg, err)}
	}
	if ok && versionPassesAssignments(pkg, v, s.ps) {
		decidedPkg, done, decErr := s.decideVersion(ctx, pkg, v)
		return &decisionOutcome{pkg: decidedPkg, done: done, err: decErr}
	}
	if err := s.materializePkg(ctx, pkg); err != nil {
		return &decisionOutcome{err: err}
	}
	return nil
}

// decideFromAllowed handles the materialized path: an empty allowed set
// reports either an unknown-package or a no-versions incompatibility and
// asks for another propagation round on pkg; otherwise the highest allowed
// version is decided. allowed is the published-only projection of the
// running intersection (pointCellsOf), since a decision can only ever be a
// published version; the no-versions term itself, though, is built from the
// running intersection's own extended snapshot (not the published
// projection), so it carries the same boundary-cell information conflict
// resolution's satisfier search needs - collapsing it to published-only
// would silently discard exactly that information.
func (s *solveState) decideFromAllowed(ctx context.Context, pkg string) (string, bool, error) {
	p := s.ps.pkgState(pkg)
	u := s.uniFor(pkg)
	allowed := pointCellsOf(p.running, u)

	if isEmptyBits(allowed) {
		if len(u.versions) == 0 {
			s.store.add(&incompatibility{
				Terms: []term{{Package: pkg, Set: u.asExtBitsetSet(fullExtBits(u), "any"), Positive: true}},
				Cause: causeUnknownPackage{Package: pkg},
			})
			return pkg, false, nil
		}
		extSnapshot := slices.Clone(p.running)
		noVersionsTerm := term{Package: pkg, Set: u.asExtBitsetSet(extSnapshot, s.describeAllowed(pkg)), Positive: true}
		s.store.add(&incompatibility{Terms: []term{noVersionsTerm}, Cause: causeNoVersions{term: noVersionsTerm}})
		return pkg, false, nil
	}

	idx, _ := lowestSetBit(allowed)
	return s.decideVersion(ctx, pkg, u.versions[idx])
}

// describeAllowed renders a human-readable label for pkg's current
// constraint, joining every contributing assignment's own display string.
// It is cosmetic only, used solely for error-reporting text.
func (s *solveState) describeAllowed(pkg string) string {
	p := s.ps.pkgState(pkg)
	parts := make([]string, 0, len(p.indices))
	for _, idx := range p.indices {
		t := s.ps.assignments[idx].term
		label := t.Set.display
		if !t.Positive {
			label = "not " + label
		}
		parts = append(parts, label)
	}
	if len(parts) == 0 {
		return "any"
	}
	return strings.Join(parts, ",")
}

// decideVersion is Pubgrub's DECIDE(v) step: add v's dependency
// incompatibilities, then - unless doing so would immediately relate one of
// them as satisfied against the tentative decision (the conservative
// decision-time conflict check) - record v as pkg's decision.
func (s *solveState) decideVersion(ctx context.Context, pkg string, v Version) (string, bool, error) {
	newIdx, err := s.dependencyIncompatibilities(ctx, pkg, v)
	if err != nil {
		return "", false, err
	}
	for _, idx := range newIdx {
		if relateTentative(s.store.all[idx], s.ps, s.uniFor, pkg, v) == incSatisfied {
			return pkg, false, nil
		}
	}
	s.ps.decide(pkg, v)
	return pkg, false, nil
}

// relateTentative evaluates relate() as if pkg were decided at v, without
// mutating the partial solution: pkg's own term is judged by exact
// membership (as Case A would once the decision is real), every other
// package's term goes through the ordinary relation().
func relateTentative(inc *incompatibility, ps *partialSolution, uniFor func(string) *packageUniverse, pkg string, v Version) incRelation {
	hasUnsat := false
	for _, t := range inc.Terms {
		var r termRelation
		if t.Package == pkg {
			in := setContains(t.Set, v, uniFor(pkg))
			if in == t.Positive {
				r = termSatisfied
			} else {
				r = termContradicted
			}
		} else {
			r = relation(t, ps, uniFor)
		}
		switch r {
		case termContradicted:
			return incContradicted
		case termInconclusive:
			if hasUnsat {
				return incInconclusive
			}
			hasUnsat = true
		case termSatisfied:
			// Continue scanning the remaining terms.
		}
	}
	if !hasUnsat {
		return incSatisfied
	}
	return incAlmostSatisfied
}

// dependencyIncompatibilities adds pkg@v's dependency incompatibilities to
// the store (deduped by (Parent, ParentVersion): a repeat decide-attempt on
// the same pair - e.g. after a deferred decision-time conflict - never
// re-fetches or re-adds them) and returns the indices of whatever it added
// (empty if this (pkg, v) pair was already processed).
func (s *solveState) dependencyIncompatibilities(ctx context.Context, pkg string, v Version) ([]int, error) {
	key := pkg + "\x00" + v.Original()
	if s.depsAdded[key] {
		return nil, nil
	}

	deps, err := s.dependenciesOf(ctx, pkg, v)
	if err != nil {
		return nil, err
	}
	s.depsAdded[key] = true

	names := slices.Sorted(maps.Keys(deps))

	parentTerm := term{Package: pkg, Set: singletonSet(v), Positive: true}
	added := make([]int, 0, len(names))
	for _, dep := range names {
		constraint := deps[dep]
		set, err := newSymbolicSet(constraint)
		if err != nil {
			return nil, fmt.Errorf("invalid dependency constraint %q for %s -> %s: %w", constraint, pkg, dep, err)
		}
		inc := &incompatibility{
			Terms: []term{parentTerm, {Package: dep, Set: set, Positive: false}},
			Cause: causeDependency{Parent: pkg, ParentVersion: v, Dep: dep, Constraint: constraint},
		}
		idx, _ := s.store.add(inc)
		added = append(added, idx)
	}
	return added, nil
}

// dependenciesOf returns pkg@v's dependency map: root's synthetic
// "dependencies" are the solve's root requirements, injected here rather
// than through any provider call; every other package goes through the
// provider.
func (s *solveState) dependenciesOf(ctx context.Context, pkg string, v Version) (map[string]Constraint, error) {
	if pkg == rootPkg {
		return s.rootDeps, nil
	}
	deps, err := s.provider.Dependencies(ctx, pkg, v)
	if err != nil {
		return nil, fmt.Errorf("fetching dependencies of %s@%s: %w", pkg, v.Original(), err)
	}
	return deps, nil
}
