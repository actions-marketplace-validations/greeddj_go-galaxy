package solver

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// TestRenderNodeRecordsNonDerivedIncompatibilityAsSolverBug covers
// renderNode's two branches on one shared pair of external incompatibilities,
// extA and extB. A causeConflict of the two - the ordinary derived shape
// renderNode is meant to walk - renders one proof line and leaves b.bug nil,
// which is also the positive control: it shows extA reaching the walk and
// being accepted into a rendered line. extA handed to renderNode directly -
// the shape it never expects, since every real caller only reaches it through
// an already-established derived node - is refused as an invariant violation
// instead.
func TestRenderNodeRecordsNonDerivedIncompatibilityAsSolverBug(t *testing.T) {
	t.Parallel()
	tm := term{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}
	extA := &incompatibility{Terms: []term{tm}, Cause: causeNoVersions{term: tm}}
	extB := &incompatibility{Terms: []term{tm}, Cause: causeNoVersions{term: tm}}

	cases := []struct {
		inc       *incompatibility
		name      string
		wantLines int
		wantBug   bool
	}{
		{
			name:      "two distinct external causes render as an ordinary derived node",
			inc:       &incompatibility{Terms: []term{tm}, Cause: causeConflict{Left: extA, Right: extB}},
			wantLines: 1,
		},
		{
			name:    "an external cause reached directly is a solver invariant violation",
			inc:     extA,
			wantBug: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestState(newFakeProvider())
			b := newReportBuilder()
			b.renderNode(s, tc.inc, false)

			if tc.wantBug {
				// Mutation: make recordBug a no-op (drop `if b.bug == nil { b.bug =
				// err }`, leaving its body empty) so renderNode's call never sets
				// b.bug - fails with:
				//
				//	report_test.go:57: renderNode on a non-derived incompatibility left b.bug nil
				if b.bug == nil {
					t.Fatalf("renderNode on a non-derived incompatibility left b.bug nil")
				}
				// Mutation: drop the errSolverBug wrap from the error renderNode
				// constructs (fmt.Errorf without the trailing `: %w", errSolverBug`
				// operand) - b.bug is still set but no longer wraps errSolverBug -
				// fails with, one output line wrapped here for width:
				//
				//	report_test.go:68: b.bug = renderNode called on a non-derived
				//	incompatibility (cause solver.causeNoVersions), want an error
				//	wrapping errSolverBug
				if !errors.Is(b.bug, errSolverBug) {
					t.Fatalf("b.bug = %v, want an error wrapping errSolverBug", b.bug)
				}
				// Documentary, not pinned: in the branch that sets b.bug,
				// renderNode returns before any statement that appends to
				// b.lines, so no state satisfying the two assertions above
				// can also leave a line in b.lines.
				if len(b.lines) != 0 {
					t.Fatalf("b.lines = %v, want empty", b.lines)
				}
				return
			}

			if b.bug != nil {
				t.Fatalf("renderNode on a derived incompatibility set b.bug = %v, want nil", b.bug)
			}
			if len(b.lines) != tc.wantLines {
				t.Fatalf("len(b.lines) = %d, want %d", len(b.lines), tc.wantLines)
			}
		})
	}
}

// TestReportBuilderOutcomePrefersRecordedBug covers outcome's own dispatch:
// with no recorded bug it returns the ConflictError built from b.lines (the
// positive control proving the fixture can reach that arm at all), and with
// one recorded it returns that error instead, discarding the built proof
// entirely - never both.
func TestReportBuilderOutcomePrefersRecordedBug(t *testing.T) {
	t.Parallel()
	tm := term{Package: "foo", Set: mustSet(t, ">=1.0.0"), Positive: true}
	inc := &incompatibility{Terms: []term{tm}, Cause: causeNoVersions{term: tm}}

	t.Run("no recorded bug returns the built ConflictError", func(t *testing.T) {
		t.Parallel()
		s := newTestState(newFakeProvider())
		b := newReportBuilder()
		b.lines = []string{"line"}

		err := b.outcome(s, inc)

		var ce *ConflictError
		if !errors.As(err, &ce) {
			t.Fatalf("outcome() = %v, want a *ConflictError", err)
		}
		if !slices.Equal(ce.ProofLines(), b.lines) {
			t.Fatalf("ProofLines() = %v, want %v", ce.ProofLines(), b.lines)
		}
	})

	t.Run("a recorded bug is returned instead of the built ConflictError", func(t *testing.T) {
		t.Parallel()
		s := newTestState(newFakeProvider())
		b := newReportBuilder()
		b.lines = []string{"line"}
		b.bug = fmt.Errorf("x: %w", errSolverBug)

		err := b.outcome(s, inc)

		// Mutation: delete `if b.bug != nil { return b.bug }` from outcome, so
		// it always returns the built *ConflictError even when b.bug is set -
		// fails with:
		//
		//	report_test.go:132: outcome() = line, want an error wrapping errSolverBug
		if !errors.Is(err, errSolverBug) {
			t.Fatalf("outcome() = %v, want an error wrapping errSolverBug", err)
		}
		if _, ok := errors.AsType[*ConflictError](err); ok {
			t.Fatalf("outcome() = %v, want not a *ConflictError", err)
		}
	})
}
