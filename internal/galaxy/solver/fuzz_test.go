package solver

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// fuzzMaxPackages bounds the package count a fuzz input can decode to: large
// enough to let the fuzzer build a real multi-hop dependency chain, small
// enough that a single corpus entry never dominates the fuzzing budget.
const fuzzMaxPackages = 6

// fuzzDecodeGraph decodes data into a small acyclic dependency graph, reusing
// sharpVersionPool/sharpConstraintPool (the same alphabet generateGraph
// draws from) so a fuzz input exercises the same sharp-edge constraint forms
// the property/oracle suites do. It is a pure function of data: every size
// derived from a byte is clamped or taken modulo into the bounded range the
// generator itself uses, and reading past the end of data wraps back to the
// start (or yields a fixed 0 for an empty input) rather than panicking, so
// no byte slice - including nil or empty - can ever crash the decoder
// itself. Dependencies only ever point from a lower package index to a
// higher one, keeping the graph acyclic exactly as generateGraph does.
func fuzzDecodeGraph(data []byte) generatedGraph {
	pos := 0
	next := func() byte {
		if len(data) == 0 {
			return 0
		}
		b := data[pos%len(data)]
		pos++
		return b
	}

	vpool := sharpVersionPool()
	cpool := sharpConstraintPool()

	n := 1 + int(next())%fuzzMaxPackages
	maxVersions := 1 + int(next())%len(vpool)

	g := generatedGraph{
		versions: make(map[string][]string, n),
		deps:     make(map[string]map[string]string, n),
		pkgs:     make([]string, n),
	}
	for i := range g.pkgs {
		g.pkgs[i] = fmt.Sprintf("gen.p%d", i)
	}

	for i, pkg := range g.pkgs {
		vc := 1 + int(next())%maxVersions
		start := int(next()) % len(vpool)
		vs := make([]string, 0, vc)
		for k := 0; k < len(vpool) && len(vs) < vc; k++ {
			vs = append(vs, vpool[(start+k)%len(vpool)])
		}
		slices.Sort(vs)
		g.versions[pkg] = vs

		for _, v := range vs {
			dm := make(map[string]string)
			for j := i + 1; j < n; j++ {
				if int(next())%3 == 0 {
					dm[g.pkgs[j]] = cpool[int(next())%len(cpool)]
				}
			}
			if len(dm) > 0 {
				g.deps[pkg+"@"+v] = dm
			}
		}
	}

	g.roots = []Requirement{{Package: g.pkgs[0], Constraint: cpool[int(next())%len(cpool)]}}
	return g
}

// fuzzOracleCap bounds the assignment space (the product over packages of
// versions+1) a fuzz iteration is willing to brute-force for the oracle
// check below: fuzzMaxPackages=6 with up to 8 versions each can reach 9^6
// assignments, and enumerating that unconditionally would starve the
// fuzzing budget, so only decoded graphs under this cap get the full
// membership check.
const fuzzOracleCap = 4096

// FuzzSolve checks the same universal properties TestPropertyResolutionSatisfiesConstraints
// does, but over the fuzzer's own adversarial corpus rather than a fixed seed
// sweep: Solve must never panic (enforced by the fuzzing framework itself
// around this function), a successful resolution must satisfy every
// constraint that names a resolved package (checked independently via
// propCheck, never the resolver's own set algebra), and a failure must
// always be a *ConflictError - never errSolverBug, which would mean the
// fuzzed input tripped a genuine internal invariant violation. On decoded
// graphs whose assignment space fits under fuzzOracleCap, the brute-force
// oracle additionally gates completeness and membership: a ConflictError on
// a graph the oracle can solve, or a resolution outside the oracle's valid
// set, fails the fuzz - the exact signed-set algebra claims completeness,
// so a false rejection is a bug here exactly as it is in oracle_test.go.
func FuzzSolve(f *testing.F) {
	for _, seed := range fuzzSeedCorpus() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		g := fuzzDecodeGraph(data)
		res, err := Solve(t.Context(), g.roots, g.provider())
		if err != nil {
			checkFuzzConflict(t, g, err)
			return
		}
		if v := g.constraintViolation(res); v != "" {
			t.Fatalf("%s (result=%v, roots=%v)", v, res.Versions, g.roots)
		}
		if g.enumerationSpace() <= fuzzOracleCap {
			if valid := g.bruteForceResolutions(); !containsResolution(valid, res.Versions) {
				t.Fatalf("resolution %v is not a member of the oracle's %d valid resolutions (roots=%v)",
					res.Versions, len(valid), g.roots)
			}
		}
	})
}

// checkFuzzConflict asserts a failed solve's error shape (a clean
// *ConflictError, never errSolverBug) and, when the graph's assignment
// space fits under fuzzOracleCap, its completeness (the oracle must agree
// nothing was solvable).
func checkFuzzConflict(t *testing.T, g generatedGraph, err error) {
	t.Helper()
	if errors.Is(err, errSolverBug) {
		t.Fatalf("internal-bug error on a fuzzed input: %v (roots=%v)", err, g.roots)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("want success or *ConflictError, got %v (roots=%v)", err, g.roots)
	}
	if g.enumerationSpace() <= fuzzOracleCap {
		if valid := g.bruteForceResolutions(); len(valid) != 0 {
			t.Fatalf("false rejection: resolver conflicted but the oracle has %d valid resolutions, e.g. %v (roots=%v)",
				len(valid), valid[0], g.roots)
		}
	}
}

// enumerationSpace returns the size of g's brute-force assignment space:
// the product over packages of (published versions + 1 for absence). The
// bounded multiply cannot overflow at fuzz scale (at most 9^6), so no
// saturation guard is needed beyond the cap comparison itself.
func (g generatedGraph) enumerationSpace() int {
	space := 1
	for _, pkg := range g.pkgs {
		space *= len(g.versions[pkg]) + 1
	}
	return space
}

// fuzzSeedCorpus returns the curated starting points FuzzSolve registers via
// f.Add: a couple of degenerate inputs (empty and a single zero byte, so the
// decoder's wraparound/default path is exercised directly), plus three
// hand-verified byte sequences whose decoded graphs reproduce the sharp
// structural shapes past solver defects were found on - a plain diamond
// (two independent paths converging on a shared dependency), a transitively
// unsatisfiable higher root version that must force a backtrack to a lower
// one, and a wildcard ("*") dependency term that must stay a visible
// contributor even though it constrains nothing by itself. Each
// decoded shape was confirmed once by hand against fuzzDecodeGraph's own
// output before being pinned here; the byte values themselves have no
// meaning beyond what they decode to.
func fuzzSeedCorpus() [][]byte {
	return [][]byte{
		{},
		{0},
		// Diamond: gen.p0 depends on both gen.p1 and gen.p2, which both
		// depend on gen.p3 - two independent paths converging on one target.
		{3, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 1, 0, 0, 0, 2, 0, 0, 0, 4, 0},
		// Transitive backtrack: gen.p0's higher version (1.2.0) depends on
		// gen.p1 via a constraint gen.p1's only version cannot satisfy; its
		// lower version (1.0.0) depends on gen.p2, satisfiably. The solver
		// must learn "not gen.p0@1.2.0" and backtrack to gen.p0@1.0.0.
		{2, 1, 1, 4, 1, 0, 0, 0, 1, 1, 0, 0, 1, 0, 2, 0},
		// Wildcard invisibility: gen.p0's higher version (1.5.0) depends on
		// gen.p1 via "*" (an always-true, full-extended-universe term),
		// which in turn depends on gen.p2 via a constraint gen.p2's only
		// version cannot satisfy; gen.p0's lower version (1.2.0) depends on
		// gen.p2 directly via "*", satisfiably. The wildcard term must stay
		// visible so the solver backtracks to gen.p0@1.2.0 instead of
		// falsely rejecting the whole graph.
		{3, 1, 1, 5, 1, 0, 0, 1, 0, 0, 1, 1, 0, 4, 0, 5, 1, 0, 2, 1, 0, 0, 0},
	}
}
