package solver

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
)

// sink receives every benchmark iteration's Result so the compiler cannot
// prove the call is dead and elide it. It is never read - only written - and
// is intentionally a package-level var rather than a local, which the Go
// compiler is otherwise free to optimize away as unused.
//
//nolint:gochecknoglobals // standard benchmark-result sink pattern, write-only
var sink *Result

// scaledGraphFanout is the branching factor of the synthetic dependency tree
// buildScaledGraph builds: package i's children are indices
// [i*fanout+1, i*fanout+fanout] (standard k-ary heap indexing), so every
// non-root package has exactly one parent. A single parent per child -
// unlike generateGraph's (property_test.go) convergent, multi-parent shape -
// means a constraint that admits at least one of the child's own published
// versions can never be contradicted by a second parent: the whole tree is
// guaranteed solvable by construction, so the benchmark measures Solve's
// per-package overhead at scale instead of occasionally timing a fast
// *ConflictError. It also keeps total edge count linear in n (each node has
// at most scaledGraphFanout children), unlike generateGraph's quadratic
// edge-candidate scan, which would make the 1000-package size pathological
// to even construct.
const scaledGraphFanout = 3

// safeConstraint returns a constraint drawn from pool that admits at least
// one of childVersions (checked via propCheck, the same independent
// membership authority property_test.go uses), falling back to "*" - which
// every published version admits - if none of pool's forms happen to. This
// is what lets buildScaledGraph and buildGalaxyShapeGraph draw real
// constraint forms (narrow enough to sometimes force materialization,
// unlike a blanket "*") while staying guaranteed-solvable.
func safeConstraint(rng *rand.Rand, pool, childVersions []string) string {
	candidates := make([]string, 0, len(pool))
	for _, c := range pool {
		for _, v := range childVersions {
			if propCheck(v, c) {
				candidates = append(candidates, c)
				break
			}
		}
	}
	if len(candidates) == 0 {
		return "*"
	}
	return candidates[rng.Intn(len(candidates))]
}

// buildScaledGraph builds a deterministic, guaranteed-solvable n-package
// dependency tree for the size-scaling benchmark: a k-ary tree (fanout
// scaledGraphFanout) of packages, each with 1-3 published versions drawn
// from sharpVersionPool, each edge constrained by a real form drawn from
// sharpConstraintPool via safeConstraint. It reuses the property-test
// generator's version/constraint alphabets for a realistic mix, but its own
// bounded-fanout tree shape (rather than generateGraph's all-pairs scan)
// keeps construction and solving linear in n.
func buildScaledGraph(seed int64, n int) ([]Requirement, *fakeProvider) {
	//nolint:gosec // G404: deterministic seeded PRNG for a reproducible benchmark corpus, not security-sensitive
	rng := rand.New(rand.NewSource(seed))
	cpool := sharpConstraintPool()
	vpool := sharpVersionPool()
	p := newFakeProvider()

	pkgs := make([]string, n)
	versions := make([][]string, n)
	for i := range pkgs {
		pkgs[i] = fmt.Sprintf("bench.p%d", i)
		perm := rng.Perm(len(vpool))
		vs := make([]string, 1+rng.Intn(3))
		for j := range vs {
			vs[j] = vpool[perm[j]]
		}
		versions[i] = vs
		p.withVersions(pkgs[i], vs...)
	}

	for i, pkg := range pkgs {
		first := i*scaledGraphFanout + 1
		children := make([]int, 0, scaledGraphFanout)
		for c := first; c < first+scaledGraphFanout && c < n; c++ {
			children = append(children, c)
		}
		if len(children) == 0 {
			continue
		}
		for _, v := range versions[i] {
			deps := make(map[string]string, len(children))
			for _, c := range children {
				deps[pkgs[c]] = safeConstraint(rng, cpool, versions[c])
			}
			p.withDeps(pkg, v, deps)
		}
	}

	return []Requirement{{Package: pkgs[0], Constraint: "*"}}, p
}

// BenchmarkSolve measures raw Solve time over the in-memory fakeProvider
// (no network, no disk) as the package count scales from a typical
// requirements.yml size up to a stress size well beyond any real Galaxy
// collection graph. Each subtest builds its graph once, before ResetTimer,
// so only Solve itself is measured.
func BenchmarkSolve(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			reqs, p := buildScaledGraph(int64(n), n)
			b.ResetTimer()
			for range b.N {
				res, err := Solve(reqs, p)
				if err != nil {
					b.Fatalf("Solve: %v", err)
				}
				sink = res
			}
		})
	}
}

// galaxyShapeRoots and galaxyShapeHubs size the "galaxy shape" benchmark: a
// shallow forest of many top-level requirements (as a real requirements.yml
// lists many collections directly) converging on a small shared set of hub
// dependencies - the realistic Galaxy topology, in contrast to
// BenchmarkSolve's synthetic scaling tree.
const (
	galaxyShapeRoots = 60
	galaxyShapeHubs  = 5
)

// buildGalaxyShapeGraph builds a deterministic, guaranteed-solvable shallow
// forest: galaxyShapeRoots top-level packages, each depending on two of
// galaxyShapeHubs shared hub packages via an unconstrained "*" (so the
// convergent root->hub edges can never jointly conflict), and each hub
// chained to the next (hub[i] -> hub[i+1], the only non-trivial constraint
// any hub carries, via safeConstraint) to add a little shared depth without
// a cycle. This mirrors a realistic Galaxy install: many requested
// collections, few truly shared transitive dependencies.
func buildGalaxyShapeGraph() ([]Requirement, *fakeProvider) {
	//nolint:gosec // G404: deterministic seeded PRNG for a reproducible benchmark corpus, not security-sensitive
	rng := rand.New(rand.NewSource(1))
	cpool := sharpConstraintPool()
	vpool := sharpVersionPool()
	p := newFakeProvider()

	hubs := make([]string, galaxyShapeHubs)
	for i := range hubs {
		hubs[i] = fmt.Sprintf("galaxy.hub%d", i)
		p.withVersions(hubs[i], vpool...)
	}
	for i := 0; i+1 < galaxyShapeHubs; i++ {
		next := hubs[i+1]
		for _, v := range vpool {
			p.withDeps(hubs[i], v, map[string]string{next: safeConstraint(rng, cpool, vpool)})
		}
	}

	reqs := make([]Requirement, galaxyShapeRoots)
	for i := range reqs {
		root := fmt.Sprintf("galaxy.root%d", i)
		p.withVersions(root, "1.0.0")
		deps := make(map[string]string, 2)
		for range 2 {
			deps[hubs[rng.Intn(len(hubs))]] = "*"
		}
		p.withDeps(root, "1.0.0", deps)
		reqs[i] = Requirement{Package: root, Constraint: "*"}
	}
	return reqs, p
}

// BenchmarkSolveGalaxyShape measures Solve time over the realistic Galaxy
// topology: many independently requested top-level collections converging
// on a small shared set of hub dependencies.
func BenchmarkSolveGalaxyShape(b *testing.B) {
	reqs, p := buildGalaxyShapeGraph()
	b.ResetTimer()
	for range b.N {
		res, err := Solve(reqs, p)
		if err != nil {
			b.Fatalf("Solve: %v", err)
		}
		sink = res
	}
}

// deepBacktrackChainLength is the chain length for BenchmarkSolveDeepBacktrack.
const deepBacktrackChainLength = 200

// buildDeepBacktrackGraph builds a deliberately conflict-dense chain: package
// i's two higher versions ("2.0.0" and "3.0.0") each require package i+1 at
// ">=2.0.0", but the chain's terminal package publishes only "1.0.0". The
// chain is therefore only satisfiable if every package resolves to its
// lowest version, "1.0.0" - decision making always tries the highest allowed
// version first (decideFromAllowed), so conflict resolution must walk the
// chain and re-derive "not >=2.0.0" one link at a time. This exercises the
// backtracking/conflict-resolution path; it is deliberately not a realistic
// dependency shape.
func buildDeepBacktrackGraph(n int) ([]Requirement, *fakeProvider) {
	p := newFakeProvider()
	pkgs := make([]string, n)
	for i := range pkgs {
		pkgs[i] = fmt.Sprintf("bench.chain%d", i)
	}
	for i, pkg := range pkgs {
		if i == n-1 {
			p.withVersions(pkg, "1.0.0")
			continue
		}
		p.withVersions(pkg, "1.0.0", "2.0.0", "3.0.0")
		next := pkgs[i+1]
		p.withDeps(pkg, "2.0.0", map[string]string{next: ">=2.0.0"})
		p.withDeps(pkg, "3.0.0", map[string]string{next: ">=2.0.0"})
	}
	return []Requirement{{Package: pkgs[0], Constraint: "*"}}, p
}

// BenchmarkSolveDeepBacktrack measures Solve time on a conflict-dense chain
// that forces conflict resolution to backtrack through every link. Whether
// the chain ultimately resolves or reports a *ConflictError is incidental -
// the point is to measure the backtracking path itself, not the outcome.
func BenchmarkSolveDeepBacktrack(b *testing.B) {
	reqs, p := buildDeepBacktrackGraph(deepBacktrackChainLength)
	b.ResetTimer()
	for range b.N {
		res, _ := Solve(reqs, p)
		sink = res
	}
}
