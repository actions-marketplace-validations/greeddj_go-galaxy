package solver

import (
	"errors"
	"testing"
)

func inconc184Provider() *fakeProvider {
	return newFakeProvider().
		withVersions("gen.p0", "1.2.5", "1.5.0").
		withVersions("gen.p1", "0.2.0", "1.2.0", "1.2.5").
		withVersions("gen.p2", "0.1.0", "1.2.0", "1.5.0").
		withVersions("gen.p3", "0.2.0", "0.2.3", "1.5.0").
		withVersions("gen.p4", "1.2.0").
		withVersions("gen.p5", "1.2.0", "1.5.0").
		withVersions("gen.p6", "0.1.0", "0.2.3", "2.0.0").
		withDeps("gen.p0", "1.2.5", map[string]string{"gen.p1": "1.x", "gen.p2": ">=1.0.0", "gen.p4": "^0.2.0"}).
		withDeps("gen.p0", "1.5.0", map[string]string{"gen.p2": "^1.0.0", "gen.p4": "^0.2.0"}).
		withDeps("gen.p1", "0.2.0", map[string]string{"gen.p2": "1.2.x", "gen.p3": ">=5.0.0", "gen.p6": ">=1.0.0-0"}).
		withDeps("gen.p1", "1.2.5", map[string]string{"gen.p3": ">=1.0.0", "gen.p5": ">=1.0.0-0"}).
		withDeps("gen.p2", "0.1.0", map[string]string{"gen.p6": ">=1.0.0"}).
		withDeps("gen.p2", "1.2.0", map[string]string{"gen.p3": "1.2.x", "gen.p5": "1.x", "gen.p6": "^0.2.0"}).
		withDeps("gen.p2", "1.5.0", map[string]string{"gen.p3": "^0.2.0", "gen.p5": "^1.0.0", "gen.p6": ">=5.0.0"}).
		withDeps("gen.p3", "0.2.0", map[string]string{"gen.p6": "^0.0.3"}).
		withDeps("gen.p3", "0.2.3", map[string]string{"gen.p4": ">=5.0.0", "gen.p6": "*"}).
		withDeps("gen.p3", "1.5.0", map[string]string{"gen.p4": ">=1.0.0-0"}).
		withDeps("gen.p4", "1.2.0", map[string]string{"gen.p6": "^0.2.0"}).
		withDeps("gen.p5", "1.2.0", map[string]string{"gen.p6": ">=1.0.0"})
}

func TestInconc184NoSpuriousInternalError(t *testing.T) {
	_, err := Solve([]Requirement{{Package: "gen.p0", Constraint: "1.2.x"}}, inconc184Provider())
	if errors.Is(err, errSolverBug) {
		t.Fatalf("conflict resolution returned a spurious internal error on a graph that must resolve or report a clean ConflictError: %v", err)
	}
	var ce *ConflictError
	if err != nil && !errors.As(err, &ce) {
		t.Fatalf("want nil or *ConflictError, got %v", err)
	}
}
