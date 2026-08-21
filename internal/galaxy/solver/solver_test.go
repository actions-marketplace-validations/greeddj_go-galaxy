package solver

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestFuelGuard lowers fuelLimit (saving and restoring it, since it is a
// package-level var precisely so a test can do this) and asserts Solve
// reports the distinct internal iteration-limit error rather than looping
// forever, on an input designed to need many decisions.
func TestFuelGuard(t *testing.T) {
	// Not parallel: fuelLimit is a shared package-level var.
	old := fuelLimit
	fuelLimit = 3
	defer func() { fuelLimit = old }()

	p := newFakeProvider().
		withVersions("a", "1.0.0").
		withVersions("b", "1.0.0").
		withVersions("c", "1.0.0").
		withVersions("d", "1.0.0").
		withDeps("a", "1.0.0", map[string]string{"b": "^1.0.0"}).
		withDeps("b", "1.0.0", map[string]string{"c": "^1.0.0"}).
		withDeps("c", "1.0.0", map[string]string{"d": "^1.0.0"})
	reqs := []Requirement{{Package: "a", Constraint: "^1.0.0"}}

	_, err := Solve(t.Context(), reqs, p)
	if err == nil {
		t.Fatalf("Solve succeeded despite a fuel limit of 3; want the iteration-limit error")
	}
	if errors.As(err, new(*ConflictError)) {
		t.Fatalf("fuel exhaustion must not surface as a *ConflictError: %v", err)
	}
	if !strings.Contains(err.Error(), "iteration limit exceeded") {
		t.Fatalf("error = %q, want it to mention the iteration limit", err.Error())
	}
}

// TestExactRelationDetectsDisjointRanges pins the exactness the signed
// term algebra provides where the retired symbolic key-identity comparison
// had to defer: two positive ranges on the SAME undecided package that are,
// in fact, disjoint (foo ^1.0.0 and foo ^2.0.0 can never both hold) are
// detected as an outright CONTRADICTED the moment the second is related,
// with the package's universe never fetched - the conjunction of the two
// exact sets is the unsatisfiable positive empty set, and relation reports
// exactly that instead of deferring to a later materialization the exact
// representation no longer needs.
func TestExactRelationDetectsDisjointRanges(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)
	rangeA := term{Package: "foo", Set: mustSet(t, "^1.0.0"), Positive: true}
	s.ps.derive(rangeA, mustDummyCause(t, s))

	rangeB := term{Package: "foo", Set: mustSet(t, "^2.0.0"), Positive: true}
	// relation(rangeB) must be CONTRADICTED here even though foo's universe
	// was never fetched: P(^1.0.0) conjoined with P(^2.0.0) is P({}), the
	// statement no selection can satisfy. The old representation could only
	// see this after materialization; exactness is the point of the signed
	// verSet algebra, so this assertion pins it directly.
	if got := relation(rangeB, s.ps); got != termContradicted {
		t.Fatalf("relation(disjoint range against an unfetched package) = %v, want termContradicted (exact algebra)", got)
	}

	// The conflict must also surface correctly through a whole solve, and
	// the solve must still terminate, when decision making has to pick a
	// real candidate: with both ^1.0.0 and ^2.0.0 required (through two
	// independent dependers, since two root requirements on the very same
	// package would be rejected upstream by the requirements parser before
	// ever reaching the core) and no version satisfying both, the empty
	// conjunction must be caught and reported as a clean conflict.
	p2 := newFakeProvider().
		withVersions("mid1", "1.0.0").
		withVersions("mid2", "1.0.0").
		withVersions("foo", "1.0.0", "2.0.0").
		withDeps("mid1", "1.0.0", map[string]string{"foo": "^1.0.0"}).
		withDeps("mid2", "1.0.0", map[string]string{"foo": "^2.0.0"})
	_, err := Solve(t.Context(), []Requirement{
		{Package: "mid1", Constraint: "^1.0.0"},
		{Package: "mid2", Constraint: "^1.0.0"},
	}, p2)
	if err == nil {
		t.Fatalf("Solve: expected a conflict (foo cannot be both ^1.0.0 and ^2.0.0 at once)")
	}
	if _, ok := errors.AsType[*ConflictError](err); !ok {
		t.Fatalf("Solve error is not a *ConflictError: %v (%T)", err, err)
	}
}

// TestUnsatisfiableDependencyBacktrackResolves pins the backtrack this input
// forces: acme.foo@2.0.0's only dependency (acme.bar>=5.0.0) is satisfied by
// no registered acme.bar version, so the solver must learn "not
// acme.foo@2.0.0", fall back to acme.foo@1.0.0, and resolve its "*"
// dependency on acme.bar, with acme.bar present in both Versions and Graph.
// A since-fixed defect once ended this exact input with a Graph edge to
// acme.bar while acme.bar itself never landed in Versions; while that
// defect lived, this test pinned extractResult's completeness guard
// catching it as a loud errSolverBug rather than letting the incomplete
// resolution under-install a real dependency. With the defect gone, the
// test asserts the clean resolution; the guard itself stays exercised by
// TestExtractResultGuardFiresOnConstructedIncompleteResolution.
func TestUnsatisfiableDependencyBacktrackResolves(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().
		withVersions("acme.foo", "1.0.0", "2.0.0").
		withVersions("acme.bar", "1.0.0").
		withDeps("acme.foo", "2.0.0", map[string]string{"acme.bar": ">=5.0.0"}).
		withDeps("acme.foo", "1.0.0", map[string]string{"acme.bar": "*"})

	res, err := Solve(t.Context(), []Requirement{{Package: "acme.foo", Constraint: ">=1.0.0"}}, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Versions["acme.foo"] != testVersion100 || res.Versions["acme.bar"] != testVersion100 {
		t.Fatalf("Versions = %v, want foo=1.0.0 bar=1.0.0 (the * dependency must resolve after the unsatisfiable-2.0.0 backtrack)", res.Versions)
	}
}

// TestExtractResultGuardFiresOnConstructedIncompleteResolution exercises the
// completeness guard directly: a decided package with a dependency edge to a
// package that was never decided must surface a loud internal-bug error.
func TestExtractResultGuardFiresOnConstructedIncompleteResolution(t *testing.T) {
	t.Parallel()
	s := newTestState(newFakeProvider())
	s.ps.decide(rootPkg, rootVersion)
	fooV := mustNewVersion(testVersion100)
	s.store.add(&incompatibility{
		Terms: []term{
			{Package: rootPkg, Set: singletonVerSet(rootVersion), Positive: true},
			{Package: "acme.foo", Set: fullVerSet(), Positive: false},
		},
		Cause: causeDependency{Parent: rootPkg, ParentVersion: rootVersion, Dep: "acme.foo", Constraint: "*"},
	})
	s.ps.decide("acme.foo", fooV)
	s.store.add(&incompatibility{
		Terms: []term{
			{Package: "acme.foo", Set: singletonVerSet(fooV), Positive: true},
			{Package: "acme.bar", Set: fullVerSet(), Positive: false},
		},
		Cause: causeDependency{Parent: "acme.foo", ParentVersion: fooV, Dep: "acme.bar", Constraint: "*"},
	})
	if _, err := s.extractResult(); err == nil || !errors.Is(err, errSolverBug) ||
		!strings.Contains(err.Error(), "acme.bar") {
		t.Fatalf("guard did not fire on a constructed incomplete resolution: err=%v", err)
	}
}

// conditionalityDropCase is one conditionality-drop-family regression: foo's
// highest version 2.0.0 depends on bar via the unsatisfiable constraintHi
// (leaving a residual on bar after backtrack), and its surviving version
// 1.0.0 depends on bar via constraintLo - bar must still resolve to wantBar.
type conditionalityDropCase struct {
	name         string
	constraintHi string
	constraintLo string
	wantBar      string
	barVersions  []string
}

func conditionalityDropCases() []conditionalityDropCase {
	return []conditionalityDropCase{
		{"star-after-empty-collapse", ">=5.0.0", "*", testVersion100, []string{testVersion100}},
		{"star-after-boundary-collapse", "1.x", "*", "0.2.0", []string{"0.2.0"}},
		{"vacuous-neq-survivor", ">=5.0.0", "!=1.5.0", "1.2.5", []string{"0.2.3", "1.2.5"}},
		{"point-pinned-residual-survivor", "!=1.5.0", ">=1.0.0-0", "1.5.0", []string{"1.5.0"}},
	}
}

// TestConditionalityDropFamily covers the resolutions that a residual left by
// a backtracked, unsatisfiable parent version must still constrain, rather
// than being dropped once the parent version that introduced it is rejected.
func TestConditionalityDropFamily(t *testing.T) {
	t.Parallel()
	root := []Requirement{{Package: "acme.foo", Constraint: ">=1.0.0"}}
	for _, tc := range conditionalityDropCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newFakeProvider().
				withVersions("acme.foo", testVersion100, "2.0.0").
				withVersions("acme.bar", tc.barVersions...).
				withDeps("acme.foo", "2.0.0", map[string]string{"acme.bar": tc.constraintHi}).
				withDeps("acme.foo", testVersion100, map[string]string{"acme.bar": tc.constraintLo})
			res, err := Solve(t.Context(), root, p)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Versions["acme.foo"] != testVersion100 || res.Versions["acme.bar"] != tc.wantBar {
				t.Fatalf("Versions = %v, want foo=1.0.0 bar=%s", res.Versions, tc.wantBar)
			}
		})
	}
}

// TestTransitiveUnsatisfiableBacktrack is the regression for the transitive
// false-rejection: a higher root version whose transitive dependency is
// unsatisfiable must be learned as "not foo@2.0.0" and the solver must
// backtrack to foo@1.2.0, not falsely declare the solvable graph unsolvable.
func TestTransitiveUnsatisfiableBacktrack(t *testing.T) {
	t.Parallel()
	const survivor = "1.2.0"
	p := newFakeProvider().
		withVersions("acme.foo", survivor, "2.0.0").
		withVersions("acme.mid", "1.5.0").
		withVersions("acme.leaf", survivor).
		withDeps("acme.foo", "2.0.0", map[string]string{"acme.mid": "1.x"}).
		withDeps("acme.foo", survivor, map[string]string{"acme.leaf": "<2.0.0"}).
		withDeps("acme.mid", "1.5.0", map[string]string{"acme.leaf": "^0.0.3"})
	res, err := Solve(t.Context(), []Requirement{{Package: "acme.foo", Constraint: "*"}}, p)
	if err != nil {
		t.Fatalf("unexpected error (was a false ConflictError before stage 1): %v", err)
	}
	if res.Versions["acme.foo"] != survivor || res.Versions["acme.leaf"] != survivor {
		t.Fatalf("Versions = %v, want foo=1.2.0 leaf=1.2.0", res.Versions)
	}
}

// TestTransitiveUnknownPackageBacktrack is the neighbor above's
// unknown-package twin: acme.foo@2.0.0 depends on a package the provider
// has never heard of, and the solvable graph must settle on acme.foo@1.2.0
// rather than be falsely rejected. Under the retired bitset representation
// this path needed a special backjump exception for unknown-package leaves
// (this test was written to pin that exception's causeUnknownPackage arm);
// under signed exact terms the ordinary propagate-resolve cycle carries the
// leaf's attribution back to the parent version on its own, and this test
// stays as the end-to-end pin that it keeps doing so. Solving at all is
// this test's own positive control - a fixture that failed to solve would
// prove nothing about the attribution path.
func TestTransitiveUnknownPackageBacktrack(t *testing.T) {
	t.Parallel()
	const survivor = "1.2.0"
	p := newFakeProvider().
		withVersions("acme.foo", survivor, "2.0.0").
		withDeps("acme.foo", "2.0.0", map[string]string{"acme.ghost": "^1.0.0"})
	res, err := Solve(t.Context(), []Requirement{{Package: "acme.foo", Constraint: "*"}}, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Versions["acme.foo"] != survivor {
		t.Fatalf("Versions = %v, want acme.foo=%s", res.Versions, survivor)
	}
}

// TestSolveStopsOnCanceledContext asserts Solve checks ctx for cancellation
// ahead of the first unitPropagation call in its main loop, refusing to make
// even one provider call once ctx is already canceled - and, as the
// mandatory positive control on the identical fixture, that a live context
// still resolves normally, so "canceled produced nothing" cannot be mistaken
// for "the fixture was never solvable".
func TestSolveStopsOnCanceledContext(t *testing.T) {
	t.Parallel()
	p := newFakeProvider().withVersions("acme.foo", "1.0.0", "2.0.0")
	reqs := []Requirement{{Package: "acme.foo", Constraint: ">=1.0.0"}}

	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, err := Solve(ctx, reqs, p)

		// Killing mutation: deleting the `if err := ctx.Err(); err != nil {
		// return nil, err }` block from Solve's main loop (solver.go) and
		// running `go test ./internal/galaxy/solver/ -run
		// TestSolveStopsOnCanceledContext -race -v -count=1` makes this exact
		// assertion fail with:
		// "solver_test.go:271: Solve returned a non-nil result on an
		// already-canceled context: map[acme.foo:2.0.0]"
		if res != nil {
			t.Fatalf("Solve returned a non-nil result on an already-canceled context: %v", res.Versions)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Solve error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, errSolverBug) {
			t.Fatalf("Solve error = %v, wraps errSolverBug; caller cancellation must never look like an internal-invariant defect", err)
		}
		if got := p.totalHighestCalls(); got != 0 {
			t.Fatalf("totalHighestCalls() = %d, want 0 (Solve must never reach the provider on an already-canceled context)", got)
		}
		// Documentary, not pinned: the two counters below cannot be this
		// chain's first failing line. Highest answers 2.0.0 for this fixture,
		// so tryDecideByProbe decides acme.foo and ensureUniverse is never
		// reached - Universe is unreachable on every path here - and
		// Dependencies is reached only from decideVersion inside that same
		// probe branch, i.e. always after the Highest call the assertion above
		// already counts. They are kept as a shape assertion that Solve
		// reached no provider at all, not as checks a mutation of the ctx
		// guard can kill.
		if got := p.totalUniverseCalls(); got != 0 {
			t.Fatalf("totalUniverseCalls() = %d, want 0", got)
		}
		if got := p.totalDepsCalls(); got != 0 {
			t.Fatalf("totalDepsCalls() = %d, want 0", got)
		}
	})

	// "live" is the mandatory positive control: the identical requirement set,
	// solved against a live context and a fresh provider instance, must
	// actually resolve - proving the "canceled" subtest's zero-calls outcome
	// comes from the cancellation check, not from a fixture that could never
	// be solved at all.
	t.Run("live", func(t *testing.T) {
		t.Parallel()
		res, err := Solve(t.Context(), reqs, newFakeProvider().withVersions("acme.foo", "1.0.0", "2.0.0"))
		if err != nil {
			t.Fatalf("Solve: unexpected error: %v", err)
		}
		if res.Versions["acme.foo"] != "2.0.0" {
			t.Fatalf("Versions[acme.foo] = %q, want 2.0.0", res.Versions["acme.foo"])
		}
	})
}
