package collections

import (
	"context"
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// differentialCase is one corpus entry the differential harness drives
// through both the greedy resolver and the version solver.
type differentialCase struct {
	// setup registers every version/dependency the case needs on a fresh
	// fake Galaxy server.
	setup func(srv *fakegalaxy.Server)
	// constraints maps every fqdn this case cares about to the full set of
	// constraint strings that must hold for whatever version ends up
	// resolved for it - known ahead of time, since the case itself set them
	// up. Only consulted when wantErr is false.
	constraints map[string][]string
	name        string
	roots       []collection
	wantErr     bool
}

// differentialCorpus is the sharp-edge corpus: constraint operators that
// have their own semver semantics (!=, prerelease exclusion and its -0
// floor, zero-major caret locking at the minor and patch level, x-ranges at
// the major and minor level), a transitive diamond (two roots converging on
// one shared dependency), and a genuinely unsatisfiable conflict.
func differentialCorpus() []differentialCase {
	cases := make([]differentialCase, 0, 9)
	cases = append(cases, exclusionCases()...)
	cases = append(cases, rangeCases()...)
	cases = append(cases, structuralCases()...)
	return cases
}

// exclusionCases covers constraint operators that exclude specific versions
// or classes of version: !=, prerelease exclusion, and its -0 floor.
func exclusionCases() []differentialCase {
	return []differentialCase{
		{
			name: "not-equal-exclusion",
			setup: func(srv *fakegalaxy.Server) {
				srv.AddVersion("acme", "widget", "1.0.0", nil)
				srv.AddVersion("acme", "widget", "1.5.0", nil)
				srv.AddVersion("acme", "widget", "2.0.0", nil)
			},
			roots:       []collection{{Namespace: "acme", Name: "widget", Constraint: "!=1.5.0"}},
			constraints: map[string][]string{"acme.widget": {"!=1.5.0"}},
		},
		{
			name: "prerelease-exclusion",
			setup: func(srv *fakegalaxy.Server) {
				srv.AddVersion("acme", "widget", "1.0.0", nil)
				srv.AddVersion("acme", "widget", "2.0.0-rc1", nil)
			},
			roots:       []collection{{Namespace: "acme", Name: "widget", Constraint: ">=1.0.0"}},
			constraints: map[string][]string{"acme.widget": {">=1.0.0"}},
		},
		{
			name: "prerelease-zero-floor",
			setup: func(srv *fakegalaxy.Server) {
				srv.AddVersion("acme", "widget", "1.0.0", nil)
				srv.AddVersion("acme", "widget", "2.0.0-rc1", nil)
			},
			roots:       []collection{{Namespace: "acme", Name: "widget", Constraint: ">=1.0.0-0"}},
			constraints: map[string][]string{"acme.widget": {">=1.0.0-0"}},
		},
	}
}

// rangeCases covers range-shaped constraint operators with their own
// semver semantics: zero-major caret locking at the minor and patch level,
// and x-ranges at the major and minor level.
func rangeCases() []differentialCase {
	xRangeVersions := []string{"0.9.0", "1.0.0", "1.2.0", "1.2.5", "1.3.0", "2.0.0"}
	return []differentialCase{
		{
			name: "caret-zero-minor",
			setup: func(srv *fakegalaxy.Server) {
				srv.AddVersion("acme", "widget", "0.2.3", nil)
				srv.AddVersion("acme", "widget", "0.2.9", nil)
				srv.AddVersion("acme", "widget", "0.3.0", nil)
			},
			roots:       []collection{{Namespace: "acme", Name: "widget", Constraint: "^0.2.3"}},
			constraints: map[string][]string{"acme.widget": {"^0.2.3"}},
		},
		{
			name: "caret-zero-patch",
			setup: func(srv *fakegalaxy.Server) {
				srv.AddVersion("acme", "widget", "0.0.3", nil)
				srv.AddVersion("acme", "widget", "0.0.4", nil)
			},
			roots:       []collection{{Namespace: "acme", Name: "widget", Constraint: "^0.0.3"}},
			constraints: map[string][]string{"acme.widget": {"^0.0.3"}},
		},
		{
			name: "x-range-major",
			setup: func(srv *fakegalaxy.Server) {
				for _, v := range xRangeVersions {
					srv.AddVersion("acme", "widget", v, nil)
				}
			},
			roots:       []collection{{Namespace: "acme", Name: "widget", Constraint: "1.x"}},
			constraints: map[string][]string{"acme.widget": {"1.x"}},
		},
		{
			name: "x-range-minor",
			setup: func(srv *fakegalaxy.Server) {
				for _, v := range xRangeVersions {
					srv.AddVersion("acme", "widget", v, nil)
				}
			},
			roots:       []collection{{Namespace: "acme", Name: "widget", Constraint: "1.2.x"}},
			constraints: map[string][]string{"acme.widget": {"1.2.x"}},
		},
	}
}

// structuralCases covers dependency-graph shape rather than a single
// constraint operator: a transitive diamond (two roots converging on one
// shared dependency) and a genuinely unsatisfiable conflict.
func structuralCases() []differentialCase {
	return []differentialCase{
		{
			name: "transitive-diamond",
			setup: func(srv *fakegalaxy.Server) {
				srv.AddVersion("acme", "a", "1.0.0", map[string]string{"acme.shared": "^1.0.0"})
				srv.AddVersion("acme", "b", "1.0.0", map[string]string{"acme.shared": "^1.0.0"})
				srv.AddVersion("acme", "shared", "1.0.0", nil)
				srv.AddVersion("acme", "shared", "1.5.0", nil)
			},
			roots: []collection{
				{Namespace: "acme", Name: "a", Constraint: "^1.0.0"},
				{Namespace: "acme", Name: "b", Constraint: "^1.0.0"},
			},
			constraints: map[string][]string{
				"acme.a":      {"^1.0.0"},
				"acme.b":      {"^1.0.0"},
				"acme.shared": {"^1.0.0", "^1.0.0"},
			},
		},
		{
			name: "unsatisfiable-conflict",
			setup: func(srv *fakegalaxy.Server) {
				srv.AddVersion("acme", "x", "1.0.0", map[string]string{"acme.z": "^1.0.0"})
				srv.AddVersion("acme", "y", "1.0.0", map[string]string{"acme.z": "^2.0.0"})
				srv.AddVersion("acme", "z", "1.0.0", nil)
				srv.AddVersion("acme", "z", "2.0.0", nil)
			},
			roots: []collection{
				{Namespace: "acme", Name: "x", Constraint: "^1.0.0"},
				{Namespace: "acme", Name: "y", Constraint: "^1.0.0"},
			},
			wantErr: true,
		},
	}
}

// TestSolverGreedyDifferential drives every differentialCorpus case through
// both the greedy resolver and the version solver against the same fake
// server, and asserts they agree on solvability: when the requirement set
// is satisfiable, both must produce a valid resolution (every resolved
// version checked against every constraint known to name it via
// constraintSatisfied); when it is not, greedy must fail and the solver
// must fail with a *solver.ConflictError specifically. A differing but
// individually valid version choice between the two (BC7) is logged, not
// failed - the solver is not required to reproduce greedy's exact tie-break,
// only to stay within the constraints.
func TestSolverGreedyDifferential(t *testing.T) {
	t.Parallel()
	for _, tc := range differentialCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := fakegalaxy.New(t)
			tc.setup(srv)
			cfg := &config.Config{Server: srv.URL()}
			runtime := infra.New(noopPrinter{}, srv.Client())
			ctx := context.Background()

			greedyResolved, _, greedyErr := resolveCollectionsInternal(
				ctx, newCollectionDeps(cfg, runtime, store.New()), tc.roots, false, false,
			)
			solverResolved, _, solverErr := solveCollections(
				ctx, newCollectionDeps(cfg, runtime, store.New()), tc.roots,
			)

			if tc.wantErr {
				assertBothConflict(t, greedyErr, solverErr)
				return
			}

			if greedyErr != nil {
				t.Fatalf("greedy: unexpected error: %v", greedyErr)
			}
			if solverErr != nil {
				t.Fatalf("solver: unexpected error: %v", solverErr)
			}
			assertValidResolution(t, "greedy", greedyResolved, tc.constraints)
			assertValidResolution(t, "solver", solverResolved, tc.constraints)
			logVersionDivergences(t, greedyResolved, solverResolved, tc.constraints)
		})
	}
}

// assertBothConflict asserts greedy failed somehow (any error signals its
// own conflict/backtrack exhaustion) and the solver failed specifically
// with a *solver.ConflictError.
func assertBothConflict(t *testing.T, greedyErr, solverErr error) {
	t.Helper()
	if greedyErr == nil {
		t.Fatalf("greedy: expected a conflict, got a resolution")
	}
	var conflictErr *solver.ConflictError
	if !errors.As(solverErr, &conflictErr) {
		t.Fatalf("solver: expected a *solver.ConflictError, got %v (%T)", solverErr, solverErr)
	}
}

// assertValidResolution checks every fqdn named in constraints resolved to
// a version satisfying every one of its own known constraints.
func assertValidResolution(t *testing.T, label string, resolved map[string]collection, constraints map[string][]string) {
	t.Helper()
	for fqdn, cs := range constraints {
		col, ok := resolved[fqdn]
		if !ok {
			t.Fatalf("%s: resolved is missing %q: %v", label, fqdn, resolved)
		}
		for _, c := range cs {
			satisfied, err := constraintSatisfied(col.Version, c)
			if err != nil {
				t.Fatalf("%s: constraintSatisfied(%q, %q): %v", label, col.Version, c, err)
			}
			if !satisfied {
				t.Fatalf("%s: %s@%s does not satisfy %q", label, fqdn, col.Version, c)
			}
		}
	}
}

// logVersionDivergences logs (without failing) every fqdn where greedy and
// the solver picked a different, individually valid version - a BC7
// divergence worth knowing about even though neither choice is wrong.
func logVersionDivergences(t *testing.T, greedyResolved, solverResolved map[string]collection, constraints map[string][]string) {
	t.Helper()
	for fqdn := range constraints {
		g, gok := greedyResolved[fqdn]
		s, sok := solverResolved[fqdn]
		if gok && sok && g.Version != s.Version {
			t.Logf("version choice diverges for %s: greedy picked %s, solver picked %s (both valid)", fqdn, g.Version, s.Version)
		}
	}
}
