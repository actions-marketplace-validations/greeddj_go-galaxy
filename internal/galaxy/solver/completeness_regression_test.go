package solver

import "testing"

// The two fixtures below are hand-expanded from the brute-force oracle's
// generated corpus (generateGraph seeds 1623 and 2373 at n=4,
// maxVersions=3): the exact graphs the retired bitset representation
// false-rejected on a wide oracle sweep, pinned here as concrete named
// providers so the completeness claim never depends on any seed-count knob.
// Each graph has exactly one valid resolution, which the oracle confirms
// and the assertions below spell out.

// falseReject1623Provider: only gen.p0@0.2.0 (dependency-free) resolves.
// gen.p0@1.0.0 needs gen.p3 ^0.0.3, which no gen.p3 version satisfies;
// gen.p0@0.2.5 pulls gen.p2, whose only version needs gen.p3 ^0.2.0, which
// no gen.p3 version satisfies either - so the solver must backtrack through
// both higher versions of gen.p0 instead of declaring the graph unsolvable.
func falseReject1623Provider() *fakeProvider {
	return newFakeProvider().
		withVersions("gen.p0", "0.2.0", "0.2.5", testVersion100).
		withVersions("gen.p1", "0.0.3").
		withVersions("gen.p2", "1.0.0-rc.1").
		withVersions("gen.p3", testVersion100, "1.0.0-rc.1", "1.5.0").
		withDeps("gen.p0", "0.2.5", map[string]string{"gen.p1": "!=1.5.0", "gen.p2": "*", "gen.p3": "!=1.5.0"}).
		withDeps("gen.p0", testVersion100, map[string]string{"gen.p3": "^0.0.3"}).
		withDeps("gen.p2", "1.0.0-rc.1", map[string]string{"gen.p3": "^0.2.0"})
}

func TestCompletenessRegressionSeed1623(t *testing.T) {
	t.Parallel()
	res, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: ">=0.2.0"}}, falseReject1623Provider())
	if err != nil {
		t.Fatalf("Solve false-rejected the solvable graph: %v", err)
	}
	if res.Versions["gen.p0"] != "0.2.0" || len(res.Versions) != 1 {
		t.Fatalf("Versions = %v, want exactly gen.p0=0.2.0 (the graph's only valid resolution)", res.Versions)
	}
}

// falseReject2373Provider: only gen.p0@1.0.0 (dependency-free) resolves.
// gen.p0@1.2.0 pulls gen.p2, both of whose versions need gen.p3 1.x - and
// gen.p3 publishes no 1.x release (1.0.0-rc.1 is a prerelease that a plain
// 1.x excludes) - so the solver must learn "not gen.p0@1.2.0" and settle on
// the lower root version.
func falseReject2373Provider() *fakeProvider {
	return newFakeProvider().
		withVersions("gen.p0", testVersion100, "1.2.0").
		withVersions("gen.p1", "0.0.3", "0.2.0", "1.0.0-rc.1").
		withVersions("gen.p2", testVersion100, "1.2.0").
		withVersions("gen.p3", "0.2.0", "0.2.5", "1.0.0-rc.1").
		withDeps("gen.p0", "1.2.0", map[string]string{"gen.p1": ">=1.0.0-0", "gen.p2": ">=0.2.0"}).
		withDeps("gen.p1", "0.0.3", map[string]string{"gen.p3": ">=1.0.0-0"}).
		withDeps("gen.p1", "0.2.0", map[string]string{"gen.p3": "*"}).
		withDeps("gen.p1", "1.0.0-rc.1", map[string]string{"gen.p3": "<2.0.0"}).
		withDeps("gen.p2", testVersion100, map[string]string{"gen.p3": "1.x"}).
		withDeps("gen.p2", "1.2.0", map[string]string{"gen.p3": "1.x"})
}

func TestCompletenessRegressionSeed2373(t *testing.T) {
	t.Parallel()
	res, err := Solve(t.Context(), []Requirement{{Package: "gen.p0", Constraint: ">=1.0.0-0"}}, falseReject2373Provider())
	if err != nil {
		t.Fatalf("Solve false-rejected the solvable graph: %v", err)
	}
	if res.Versions["gen.p0"] != testVersion100 || len(res.Versions) != 1 {
		t.Fatalf("Versions = %v, want exactly gen.p0=1.0.0 (the graph's only valid resolution)", res.Versions)
	}
}
