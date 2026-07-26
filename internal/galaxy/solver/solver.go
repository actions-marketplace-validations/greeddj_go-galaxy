package solver

import (
	"errors"
	"fmt"
	"slices"
)

// fuelLimit bounds the main loop: the algorithm terminates by construction,
// so exceeding this many iterations means a solver defect, not a large
// input. It is a var, not a const, purely so tests can lower it (saving and
// restoring around the change) to exercise the guard without constructing
// an input that would otherwise loop forever.
//
//nolint:gochecknoglobals // deliberately a var, not a const, so in-package tests can lower it to exercise the fuel guard
var fuelLimit = 1_000_000

// errSolverBug is the sentinel every internal invariant-violation error the
// solver core raises wraps. Each signals a defect in the algorithm itself -
// an assumption the reference algorithm guarantees but this implementation
// failed to uphold - never a caller error, an unsatisfiable input (that is a
// *ConflictError), or a provider failure (that is passed through wrapped on
// its own). None of these are part of Solve's documented exit-code taxonomy.
var errSolverBug = errors.New("solver: internal invariant violated")

// solveState is one Solve call's mutable state: the incompatibility store,
// the partial solution, per-package materialization/universe data, and the
// bookkeeping the decision heuristic and dependency injection need. It is
// never shared across goroutines and never persisted between calls.
type solveState struct {
	provider       Provider
	store          *incompatStore
	ps             *partialSolution
	universes      map[string]*packageUniverse
	conflictCounts map[string]int
	depsAdded      map[string]bool
	rootDeps       map[string]Constraint
}

// Solve resolves reqs against p, returning the chosen version per package
// (plus their dependency graph) on success, or a *ConflictError when no
// selection exists. Any other error is a provider failure passed through
// wrapped, never modeled as an incompatibility.
func Solve(reqs []Requirement, p Provider) (*Result, error) {
	s := newSolveState(p, reqs)

	rootInc := &incompatibility{
		Terms: []term{{Package: rootPkg, Set: singletonSet(rootVersion), Positive: false}},
		Cause: causeRoot{},
	}
	s.store.add(rootInc)

	next := rootPkg
	for fuel := 0; ; fuel++ {
		if fuel > fuelLimit {
			return nil, fmt.Errorf("iteration limit exceeded: %w", errSolverBug)
		}
		if err := s.unitPropagation(next); err != nil {
			return nil, err
		}
		pkg, done, err := s.makeDecision()
		if err != nil {
			return nil, err
		}
		if done {
			return s.extractResult()
		}
		next = pkg
	}
}

// newSolveState builds a fresh solve state, pre-populating the synthetic
// root package's universe (the constant {0.0.0}) so the core never calls
// the provider for it.
func newSolveState(p Provider, reqs []Requirement) *solveState {
	rootDeps := make(map[string]Constraint, len(reqs))
	for _, r := range reqs {
		rootDeps[r.Package] = r.Constraint
	}

	s := &solveState{
		provider:       p,
		store:          newIncompatStore(),
		universes:      make(map[string]*packageUniverse),
		conflictCounts: make(map[string]int),
		depsAdded:      make(map[string]bool),
		rootDeps:       rootDeps,
	}
	s.ps = newPartialSolution(s.uniFor)

	rootUni := newPackageUniverse()
	rootUni.setVersions([]Version{rootVersion})
	s.universes[rootPkg] = rootUni
	s.ps.materializePackage(rootPkg)

	return s
}

// uniFor returns pkg's materialization context, lazily creating an
// unmaterialized placeholder for a package that has never been touched yet.
func (s *solveState) uniFor(pkg string) *packageUniverse {
	u, ok := s.universes[pkg]
	if !ok {
		u = newPackageUniverse()
		s.universes[pkg] = u
	}
	return u
}

// materializePkg fetches pkg's universe from the provider (unless it is the
// synthetic root, which is pre-materialized and never fetched, or the
// universe was already fetched earlier) and brings the partial solution's
// running intersection for it up to date. The latter is always attempted,
// even when the universe fetch itself is skipped: a backtrack that wipes
// every one of pkg's assignments resets its partial-solution-level
// materialized flag (see backtrackTo) without forgetting the fetched
// universe, so a later re-materialization here is a cheap local replay, not
// a second provider call; ps.materializePackage is already a no-op if it is
// still up to date.
func (s *solveState) materializePkg(pkg string) error {
	if pkg == rootPkg {
		return nil
	}
	u := s.uniFor(pkg)
	if !u.materialized {
		versions, err := s.provider.Universe(pkg)
		if err != nil {
			return fmt.Errorf("fetching version universe for %s: %w", pkg, err)
		}
		u.setVersions(buildUniverse(versions))
	}
	s.ps.materializePackage(pkg)
	return nil
}

// extractResult builds the successful Result from every decision except
// root: Versions maps each package to its decided version's original
// string, and Graph maps each package to the sorted dependency names of its
// causeDependency incompatibilities for that decided (package, version).
//
// Before returning, it asserts every Graph edge target is itself a key in
// Versions - Result's own documented invariant ("every edge target is
// itself a key in Versions"). A miss means the solve finished with an edge
// pointing at a package that was never actually decided: a silent,
// incomplete resolution that would otherwise under-install a real
// dependency. Failing loudly here, rather than handing the caller a Result
// that violates its own contract, is strictly safer than staying silent.
func (s *solveState) extractResult() (*Result, error) {
	decidedVer := make(map[string]Version, len(s.ps.packages))
	decidedDeps := make(map[string][]string, len(s.ps.packages))
	for pkg, p := range s.ps.packages {
		if p.decisionIdx == -1 {
			continue
		}
		decidedVer[pkg] = p.decisionVersion
		decidedDeps[pkg] = s.decidedDependencyNames(pkg, p.decisionVersion)
	}

	// The result is the closure reachable from the root through the decided
	// dependency edges, not every decided package. A version decided inside a
	// branch the solver later backtracked away from can linger as a decision
	// with no path from any requirement; including it would install a package
	// nothing depends on. Walking the reachable closure keeps the result
	// minimal.
	reachable := make(map[string]bool, len(decidedVer))
	stack := []string{rootPkg}
	for len(stack) > 0 {
		pkg := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if reachable[pkg] {
			continue
		}
		reachable[pkg] = true
		stack = append(stack, decidedDeps[pkg]...)
	}

	versions := make(Resolution, len(reachable))
	graph := make(map[string][]string, len(reachable))
	for pkg := range reachable {
		if pkg == rootPkg {
			continue
		}
		v, ok := decidedVer[pkg]
		if !ok {
			return nil, fmt.Errorf("resolution reaches an undecided package %q: %w", pkg, errSolverBug)
		}
		versions[pkg] = v.Original()
		graph[pkg] = decidedDeps[pkg]
	}

	return &Result{Versions: versions, Graph: graph}, nil
}

// decidedDependencyNames returns the sorted dependency package names
// recorded by pkg@v's causeDependency incompatibilities.
func (s *solveState) decidedDependencyNames(pkg string, v Version) []string {
	names := make([]string, 0)
	for _, inc := range s.store.all {
		dep, ok := inc.Cause.(causeDependency)
		if !ok {
			continue
		}
		if dep.Parent != pkg || dep.ParentVersion.Original() != v.Original() {
			continue
		}
		names = append(names, dep.Dep)
	}
	slices.Sort(names)
	return names
}
