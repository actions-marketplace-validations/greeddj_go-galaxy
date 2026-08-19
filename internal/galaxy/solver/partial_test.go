package solver

import (
	"errors"
	"testing"
)

// backtrackToFixture builds a partial solution carrying a single
// decision-shaped assignment for "foo" (level 1, CauseIndex -1) whose term
// wraps set, appended directly via ps.append rather than ps.decide - so
// decisionVersion starts at its zero value, never set by the append itself.
// It returns the solve state alongside the *packageAssignments recorded for
// "foo" right after the append, captured before backtrackTo ever runs, so a
// caller can assert that pointer is untouched when backtrackTo returns an
// error.
func backtrackToFixture(t *testing.T, set verSet) (*solveState, *packageAssignments) {
	t.Helper()
	s := newTestState(newFakeProvider())
	s.ps.append(term{Package: "foo", Set: set, Positive: true}, 1, -1)
	return s, s.ps.packages["foo"]
}

// TestPartialSolutionBacktrackTo covers backtrackTo's rebuild loop for a
// decision-shaped assignment: a singleton term rebuilds decisionVersion
// cleanly (the positive control proving the fixture is capable of
// succeeding), while a non-singleton term - decide never builds one, but
// append is package-visible and nothing stops a caller from handing it a
// decision-shaped assignment carrying an unconstrained or ranged set - is
// refused with an error wrapping errSolverBug, leaving the package map
// exactly as it was before the call.
func TestPartialSolutionBacktrackTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		set     verSet
		wantErr bool
	}{
		{
			name: "singleton decision term rebuilds decisionVersion",
			set:  singletonVerSet(mustV(t, "1.0.0")),
		},
		{
			name:    "non-singleton decision term reports an invariant error",
			set:     mustSet(t, ">=1.0.0"),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, before := backtrackToFixture(t, tc.set)

			if tc.wantErr {
				err := s.ps.backtrackTo(1)
				// Mutation: swallow the rebuild error in backtrackTo (change
				// `rebuilt, err := ps.rebuildPackageAssignments(); if err != nil
				// { return err }` to `rebuilt, err :=
				// ps.rebuildPackageAssignments(); _ = err`) fails with:
				//
				//	partial_test.go:63: backtrackTo(1) = nil, want an error wrapping errSolverBug
				if err == nil {
					t.Fatalf("backtrackTo(1) = nil, want an error wrapping errSolverBug")
				}
				if !errors.Is(err, errSolverBug) {
					t.Fatalf("backtrackTo(1) error = %v, want one wrapping errSolverBug", err)
				}
				// This assertion is genuinely pinned, not merely reachable: the
				// mutation above (swallowing the rebuild error) makes the first
				// assertion fail instead, so only a mutation that keeps the error
				// non-nil while still replacing the package map can reach this
				// line. Mutation: hoist `ps.packages = rebuilt` above
				// backtrackTo's `if err != nil { return err }` check, leaving the
				// error itself intact - fails with:
				//
				//	partial_test.go:78: backtrackTo(1) replaced packages["foo"] despite returning an error
				if s.ps.packages["foo"] != before {
					t.Fatalf("backtrackTo(1) replaced packages[%q] despite returning an error", "foo")
				}
				return
			}

			// append does not set decisionVersion (only decide does), so
			// asserting it is still empty here proves the rebuild loop below is
			// what actually establishes it, not some earlier step of the
			// fixture.
			if got := s.ps.pkgState("foo").decisionVersion.Original(); got != "" {
				t.Fatalf("decisionVersion before backtrackTo = %q, want empty", got)
			}
			if err := s.ps.backtrackTo(1); err != nil {
				t.Fatalf("backtrackTo(1): %v", err)
			}
			if got := s.ps.pkgState("foo").decisionVersion.Original(); got != "1.0.0" {
				t.Fatalf("decisionVersion after backtrackTo = %q, want %q", got, "1.0.0")
			}
		})
	}
}
