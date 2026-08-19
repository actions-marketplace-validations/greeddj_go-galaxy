package solver

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// packageIsExactPin reports whether pkg currently has an exact pin: before
// its universe is fetched, a positive accumulation that still carries a
// singleton's concrete version (a "=X" or bare-version constraint folded
// over the identity seed); after the fetch, an accumulation permitting
// exactly one published candidate. The pre-fetch form matters because it
// carries the original registry spelling, which the decision and its
// dependency fetches key on.
func (s *solveState) packageIsExactPin(pkg string) (Version, bool) {
	p := s.ps.pkgState(pkg)
	u := s.uniFor(pkg)
	if u.fetched {
		only := Version{}
		count := 0
		for _, v := range u.versions {
			if termPermits(p.accum, v) {
				only = v
				count++
			}
		}
		if count == 1 {
			return only, true
		}
		return Version{}, false
	}
	if !p.accum.Positive {
		return Version{}, false
	}
	if v, ok := p.accum.Set.decidedVersion(); ok {
		return v, true
	}
	return Version{}, false
}

// versionPassesAccum reports whether v satisfies everything currently known
// about pkg: a single signed membership test against the accumulation,
// which is the exact conjunction of every assignment on record.
func (s *solveState) versionPassesAccum(pkg string, v Version) bool {
	return termPermits(s.ps.pkgState(pkg).accum, v)
}

// probeWorthTrying reports whether the provider.Highest probe can settle
// pkg without ever fetching its full universe: pkg must carry a positive
// requirement (a negative-only accumulation never asserts selection) and
// its universe must not already be fetched (once it is, deciding from the
// allowed candidates is both exact and free).
func (s *solveState) probeWorthTrying(pkg string) bool {
	p := s.ps.pkgState(pkg)
	return len(p.indices) > 0 && p.accum.Positive
}

// candidatePackages returns, sorted ascending by name, every package that
// has at least one positive derivation and no decision yet - the pool
// package prioritization (9.1) chooses from. The accumulation's sign is
// that test: it flips positive on the first positive assignment and never
// flips back.
func (s *solveState) candidatePackages() []string {
	names := make([]string, 0, len(s.ps.packages))
	for pkg, p := range s.ps.packages {
		if p.decisionIdx != -1 || len(p.indices) == 0 {
			continue
		}
		if p.accum.Positive {
			names = append(names, pkg)
		}
	}
	slices.Sort(names)
	return names
}

// pickPackage selects the next package to decide per the frozen
// prioritization: exact pins first, then packages that have caused a
// conflict (higher conflict count first), then fetched packages with the
// fewest allowed candidates, then everything else - ties broken ascending
// by name throughout, which candidatePackages' own sort already guarantees
// as the stable base order every class filter preserves.
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
		u := s.uniFor(n)
		if !u.fetched {
			continue
		}
		c := s.countAllowed(n, u)
		if bestCount == -1 || c < bestCount {
			best = n
			bestCount = c
		}
	}
	return best, best != ""
}

// countAllowed counts pkg's published versions its accumulation permits.
func (s *solveState) countAllowed(pkg string, u *packageUniverse) int {
	accum := s.ps.pkgState(pkg).accum
	count := 0
	for _, v := range u.versions {
		if termPermits(accum, v) {
			count++
		}
	}
	return count
}

// pickRequiredDependencyTarget is decision making's fallback for when the
// positive-derivation candidate pool is empty: it returns the lowest-named
// undecided package that a currently-decided parent depends on. Such a
// package is required by that decided parent, yet can carry only a negative
// assignment - a residual left by an earlier, backtracked parent version
// whose own dependency on it had no matching version makes its later
// dependency term read as already satisfied rather than deriving a fresh
// positive requirement. Deciding it here honors the same dependency edges
// the completeness guard enforces, and the store scan stays off the
// decision hot path since it runs only once the ordinary candidate pool is
// exhausted.
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
// pin, then the provider.Highest probe), fetching pkg's universe along the
// way when a fast path cannot be confirmed cheaply. Returns nil when no
// fast path applied and the caller should fall through to
// decideFromAllowed.
func (s *solveState) tryFastDecide(ctx context.Context, pkg string) *decisionOutcome {
	u := s.uniFor(pkg)
	if vp, isPin := s.packageIsExactPin(pkg); isPin {
		return s.tryDecidePin(ctx, pkg, u, vp)
	}
	if !u.fetched && s.probeWorthTrying(pkg) {
		return s.tryDecideByProbe(ctx, pkg)
	}
	if !u.fetched {
		if err := s.ensureUniverse(ctx, pkg); err != nil {
			return &decisionOutcome{err: err}
		}
	}
	return nil
}

// tryDecidePin attempts to decide pkg at its already-known exact pin vp
// without a universe fetch when possible (either pkg's universe is already
// fetched, or the pin already passes the accumulated assignments), fetching
// the universe otherwise so decideFromAllowed can re-verify the pin for
// real. Returns nil when the caller should fall through to
// decideFromAllowed itself.
func (s *solveState) tryDecidePin(ctx context.Context, pkg string, u *packageUniverse, vp Version) *decisionOutcome {
	if u.fetched || s.versionPassesAccum(pkg, vp) {
		pkg, done, err := s.decideVersion(ctx, pkg, vp)
		return &decisionOutcome{pkg: pkg, done: done, err: err}
	}
	if err := s.ensureUniverse(ctx, pkg); err != nil {
		return &decisionOutcome{err: err}
	}
	return nil
}

// tryDecideByProbe attempts the provider.Highest probe fast path before ever
// fetching pkg's full universe, fetching it (but not deciding) if the probe
// is unavailable or its candidate does not satisfy the accumulated
// assignments. Returns nil when the caller should fall through to
// decideFromAllowed itself.
func (s *solveState) tryDecideByProbe(ctx context.Context, pkg string) *decisionOutcome {
	v, ok, err := s.provider.Highest(ctx, pkg)
	if err != nil {
		return &decisionOutcome{err: fmt.Errorf("probing highest version of %s: %w", pkg, err)}
	}
	if ok && s.versionPassesAccum(pkg, v) {
		decidedPkg, done, decErr := s.decideVersion(ctx, pkg, v)
		return &decisionOutcome{pkg: decidedPkg, done: done, err: decErr}
	}
	if err := s.ensureUniverse(ctx, pkg); err != nil {
		return &decisionOutcome{err: err}
	}
	return nil
}

// decideFromAllowed handles the fetched path: an empty allowed set reports
// either an unknown-package or a no-versions incompatibility and asks for
// another propagation round on pkg; otherwise the highest allowed version
// is decided. The no-versions term is the accumulation's positive form (the
// exact region every assignment jointly permits), so the leaf states
// precisely which requirement the published universe cannot meet.
func (s *solveState) decideFromAllowed(ctx context.Context, pkg string) (string, bool, error) {
	p := s.ps.pkgState(pkg)
	u := s.uniFor(pkg)

	for _, v := range u.versions {
		if termPermits(p.accum, v) {
			return s.decideVersion(ctx, pkg, v)
		}
	}

	if len(u.versions) == 0 {
		s.store.add(&incompatibility{
			Terms: []term{{Package: pkg, Set: fullVerSet(), Positive: true}},
			Cause: causeUnknownPackage{Package: pkg},
		})
		return pkg, false, nil
	}
	noVersionsTerm := term{Package: pkg, Set: positiveFormSet(p.accum), Positive: true}
	s.store.add(&incompatibility{Terms: []term{noVersionsTerm}, Cause: causeNoVersions{term: noVersionsTerm}})
	return pkg, false, nil
}

// positiveFormSet returns the set of versions accum actually permits, as a
// plain positive set: the accumulation's own set when it is positive, its
// complement otherwise.
func positiveFormSet(accum term) verSet {
	if accum.Positive {
		return accum.Set
	}
	return accum.Set.complement()
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
		if relateTentative(s.store.all[idx], s.ps, pkg, v) == incSatisfied {
			return pkg, false, nil
		}
	}
	s.ps.decide(pkg, v)
	return pkg, false, nil
}

// relateTentative evaluates relate() as if pkg were decided at v, without
// mutating the partial solution: pkg's own term is judged by exact
// membership (as it would be once the decision is folded in), every other
// package's term goes through the ordinary relation().
func relateTentative(inc *incompatibility, ps *partialSolution, pkg string, v Version) incRelation {
	hasUnsat := false
	for _, t := range inc.Terms {
		var r termRelation
		if t.Package == pkg {
			if termPermits(t, v) {
				r = termSatisfied
			} else {
				r = termContradicted
			}
		} else {
			r = relation(t, ps)
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
// (empty if this (pkg, v) pair was already processed). A dependency whose
// constraint denotes the empty set is recorded as the single-term
// incompatibility {parent@v} with the same dependency cause: "not (dep in
// {})" is tautological, so the two-term form would be equivalent but carry
// a term that is true for every selection, which no stored incompatibility
// may contain (dropTautological's invariant).
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

	parentTerm := term{Package: pkg, Set: singletonVerSet(v), Positive: true}
	added := make([]int, 0, len(names))
	for _, dep := range names {
		constraint := deps[dep]
		set, err := newVerSet(constraint)
		if err != nil {
			return nil, fmt.Errorf("invalid dependency constraint %q for %s -> %s: %w", constraint, pkg, dep, err)
		}
		terms := []term{parentTerm, {Package: dep, Set: set, Positive: false}}
		if set.isEmpty() {
			terms = []term{parentTerm}
		}
		inc := &incompatibility{
			Terms: terms,
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
