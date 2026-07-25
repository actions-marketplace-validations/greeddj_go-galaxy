package collections

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// constraintCase is one row driven through constraintSatisfied by
// TestConstraintSemanticsV3. The case table is built by several small
// per-topic functions rather than one large literal so that no single
// function is long enough to trip funlen; TestConstraintSemanticsV3 itself
// just concatenates them and runs the shared subtest loop.
type constraintCase struct {
	name       string
	constraint string
	version    string
	wantMatch  bool
	wantErr    bool
}

// basicOperatorCases covers bare-exact, "=", "!=", ">=", ">", "<=", "<", a
// two-clause range, and the match-all "*" sentinel.
func basicOperatorCases() []constraintCase {
	return []constraintCase{
		{name: "bare exact match", constraint: "1.2.3", version: "1.2.3", wantMatch: true},
		{name: "bare exact no-match", constraint: "1.2.3", version: "1.2.4", wantMatch: false},
		{name: "= exact match", constraint: "=1.2.3", version: "1.2.3", wantMatch: true},
		{name: "= exact no-match", constraint: "=1.2.3", version: "1.2.4", wantMatch: false},
		{name: "!= match", constraint: "!=1.2.3", version: "1.2.4", wantMatch: true},
		{name: "!= no-match", constraint: "!=1.2.3", version: "1.2.3", wantMatch: false},
		{name: ">= match at floor", constraint: ">=1.0.0", version: "1.0.0", wantMatch: true},
		{name: ">= match above floor", constraint: ">=1.0.0", version: "2.0.0", wantMatch: true},
		{name: ">= no-match below floor", constraint: ">=1.0.0", version: "0.9.0", wantMatch: false},
		{name: "> match", constraint: ">1.0.0", version: "1.0.1", wantMatch: true},
		{name: "> no-match at boundary", constraint: ">1.0.0", version: "1.0.0", wantMatch: false},
		{name: "<= match at ceiling", constraint: "<=2.0.0", version: "2.0.0", wantMatch: true},
		{name: "<= no-match above ceiling", constraint: "<=2.0.0", version: "2.0.1", wantMatch: false},
		{name: "< match", constraint: "<2.0.0", version: "1.9.9", wantMatch: true},
		{name: "< no-match at boundary", constraint: "<2.0.0", version: "2.0.0", wantMatch: false},
		{name: "range match inside", constraint: ">=1.0.0,<2.0.0", version: "1.5.0", wantMatch: true},
		{name: "range no-match below", constraint: ">=1.0.0,<2.0.0", version: "0.9.0", wantMatch: false},
		{name: "range no-match at ceiling", constraint: ">=1.0.0,<2.0.0", version: "2.0.0", wantMatch: false},
		{name: "* matches anything low", constraint: "*", version: "0.0.1", wantMatch: true},
		{name: "* matches anything high", constraint: "*", version: "1.0.0", wantMatch: true},
	}
}

// xRangeCases covers the "1.x" and "1.2.x" wildcard-segment forms.
func xRangeCases() []constraintCase {
	return []constraintCase{
		{name: "1.x match low", constraint: "1.x", version: "1.0.0", wantMatch: true},
		{name: "1.x match high", constraint: "1.x", version: "1.9.9", wantMatch: true},
		{name: "1.x no-match below", constraint: "1.x", version: "0.9.0", wantMatch: false},
		{name: "1.x no-match above", constraint: "1.x", version: "2.0.0", wantMatch: false},
		{name: "1.2.x match low", constraint: "1.2.x", version: "1.2.0", wantMatch: true},
		{name: "1.2.x match high", constraint: "1.2.x", version: "1.2.9", wantMatch: true},
		{name: "1.2.x no-match", constraint: "1.2.x", version: "1.3.0", wantMatch: false},
	}
}

// tildeCases covers the "~1.2.3" and "~1.2" patch/minor-lock forms.
func tildeCases() []constraintCase {
	return []constraintCase{
		{name: "~1.2.3 match same patch series low", constraint: "~1.2.3", version: "1.2.3", wantMatch: true},
		{name: "~1.2.3 match same patch series high", constraint: "~1.2.3", version: "1.2.9", wantMatch: true},
		{name: "~1.2.3 no-match below", constraint: "~1.2.3", version: "1.2.2", wantMatch: false},
		{name: "~1.2.3 no-match next minor", constraint: "~1.2.3", version: "1.3.0", wantMatch: false},
		{name: "~1.2 match low", constraint: "~1.2", version: "1.2.0", wantMatch: true},
		{name: "~1.2 match high", constraint: "~1.2", version: "1.2.9", wantMatch: true},
		{name: "~1.2 no-match next minor", constraint: "~1.2", version: "1.3.0", wantMatch: false},
	}
}

// caretStableCases covers "^1.2.3" against a stable (>=1.0.0) base, where v3
// matches v1's whole-major-locked behavior.
func caretStableCases() []constraintCase {
	return []constraintCase{
		{name: "^1.2.3 match same major low", constraint: "^1.2.3", version: "1.2.3", wantMatch: true},
		{name: "^1.2.3 match same major high", constraint: "^1.2.3", version: "1.9.9", wantMatch: true},
		{name: "^1.2.3 no-match below", constraint: "^1.2.3", version: "1.2.2", wantMatch: false},
		{name: "^1.2.3 no-match next major", constraint: "^1.2.3", version: "2.0.0", wantMatch: false},
	}
}

// caretZeroMinorCases covers the v3-specific narrowing of a "^0.y.z" caret
// range (y > 0): v3 narrows it to <0.(y+1).0 instead of treating the whole
// 0.x major as equivalent to a stable major, unlike v1.
func caretZeroMinorCases() []constraintCase {
	return []constraintCase{
		{name: "^0.2.3 match same minor low", constraint: "^0.2.3", version: "0.2.3", wantMatch: true},
		{name: "^0.2.3 match same minor high", constraint: "^0.2.3", version: "0.2.9", wantMatch: true},
		{name: "^0.2.3 no-match next minor", constraint: "^0.2.3", version: "0.3.0", wantMatch: false},
		{name: "^0.2.3 no-match next major", constraint: "^0.2.3", version: "1.0.0", wantMatch: false},
	}
}

// caretZeroPatchCases covers the v3-specific narrowing of a "^0.0.z" caret
// range: v3 narrows it further still, to <0.0.(z+1) rather than <0.1.0.
func caretZeroPatchCases() []constraintCase {
	return []constraintCase{
		{name: "^0.0.3 match exact patch", constraint: "^0.0.3", version: "0.0.3", wantMatch: true},
		{name: "^0.0.3 no-match next patch", constraint: "^0.0.3", version: "0.0.4", wantMatch: false},
		{name: "^0.0.3 no-match next minor", constraint: "^0.0.3", version: "0.1.0", wantMatch: false},
	}
}

// prereleaseCases covers prerelease exclusion by a plain constraint,
// inclusion via the "-0" floor idiom, and bare prerelease exact matching.
func prereleaseCases() []constraintCase {
	return []constraintCase{
		// Exclusion: a constraint without its own prerelease component never
		// matches a prerelease version, even one that would otherwise satisfy
		// the numeric range.
		{name: "prerelease excluded above range", constraint: ">=1.0.0", version: "2.0.0-rc1", wantMatch: false},
		{name: "prerelease exclusion control: stable matches", constraint: ">=1.0.0", version: "1.0.0", wantMatch: true},
		{name: "prerelease excluded at floor", constraint: ">=1.0.0", version: "1.0.0-rc1", wantMatch: false},
		// Inclusion: a constraint whose own floor carries a prerelease
		// component (the "-0" idiom) opts in to matching any prerelease of
		// that version.
		{name: "prerelease included via -0 floor", constraint: ">=1.0.0-0", version: "1.0.0-rc1", wantMatch: true},
		// Bare prerelease exact match: an exact prerelease version only
		// matches that identical prerelease, not its stable release.
		{name: "bare prerelease exact match", constraint: "1.0.0-rc1", version: "1.0.0-rc1", wantMatch: true},
		{name: "bare prerelease exact no-match against stable", constraint: "1.0.0-rc1", version: "1.0.0", wantMatch: false},
	}
}

// exactMatchOperatorCases covers ansible's "==" exact-match operator, now
// rewritten to v3's "=" by helpers.NormalizeConstraint, including a
// clause-level rewrite proof (the "==" clause is not required to be first)
// and a "==" prerelease.
func exactMatchOperatorCases() []constraintCase {
	return []constraintCase{
		{name: "== exact match", constraint: "==1.2.3", version: "1.2.3", wantMatch: true},
		{name: "== exact no-match", constraint: "==1.2.3", version: "1.2.4", wantMatch: false},
		{name: "==,!= match", constraint: "==1.0.0,!=1.0.5", version: "1.0.0", wantMatch: true},
		{name: "==,!= no-match on excluded value", constraint: "==1.0.0,!=1.0.5", version: "1.0.5", wantMatch: false},
		{
			name:       "!=,== match proves clause-level rewrite, not prefix-only",
			constraint: "!=1.0.5,==1.2.3", version: "1.2.3", wantMatch: true,
		},
		{name: "!=,== no-match on excluded value", constraint: "!=1.0.5,==1.2.3", version: "1.0.5", wantMatch: false},
		{name: "== prerelease exact match", constraint: "==1.0.0-rc1", version: "1.0.0-rc1", wantMatch: true},
		{name: "== prerelease no-match against stable", constraint: "==1.0.0-rc1", version: "1.0.0", wantMatch: false},
	}
}

// overNormalizationGuardCases covers the malformed forms that must remain
// parse errors: "===" is not ansible's exact-match operator and must not be
// coerced to "="; ">==" does not start with "==" at all, so it is never
// touched by the rewrite and still fails as an unrecognized operator
// combination.
func overNormalizationGuardCases() []constraintCase {
	return []constraintCase{
		{name: "=== is not rewritten and stays a parse error", constraint: "===1.2.3", version: "1.2.3", wantErr: true},
		{name: ">== is not rewritten and stays a parse error", constraint: ">==1.2.3", version: "1.2.3", wantErr: true},
	}
}

// TestConstraintSemanticsV3 pins constraintSatisfied's exact behavior on
// Masterminds/semver v3, covering every operator form ansible-galaxy
// requirement files can express. These rows are the ground truth for v3:
// they were verified empirically against the vendored library before being
// written here, not derived from v1 behavior or wishful thinking.
func TestConstraintSemanticsV3(t *testing.T) {
	t.Parallel()
	sections := [][]constraintCase{
		basicOperatorCases(),
		xRangeCases(),
		tildeCases(),
		caretStableCases(),
		caretZeroMinorCases(),
		caretZeroPatchCases(),
		prereleaseCases(),
		exactMatchOperatorCases(),
		overNormalizationGuardCases(),
	}
	total := 0
	for _, section := range sections {
		total += len(section)
	}
	tests := make([]constraintCase, 0, total)
	for _, section := range sections {
		tests = append(tests, section...)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := constraintSatisfied(tt.version, tt.constraint)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("constraintSatisfied(%q, %q) expected an error, got match=%v", tt.version, tt.constraint, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("constraintSatisfied(%q, %q) unexpected error: %v", tt.version, tt.constraint, err)
			}
			if got != tt.wantMatch {
				t.Fatalf("constraintSatisfied(%q, %q) = %v, want %v", tt.version, tt.constraint, got, tt.wantMatch)
			}
		})
	}
}

// TestVersionOrderingV3 pins isNewerVersion's ordering behavior on
// Masterminds/semver v3: numeric precedence, prerelease-before-release
// ordering, prerelease identifier comparison, build metadata being ignored for
// ordering purposes, and a parse failure being reported as an error rather
// than a silent false.
func TestVersionOrderingV3(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		latest    string
		locked    string
		wantNewer bool
		wantErr   bool
	}{
		{name: "patch bump is newer", latest: "1.0.1", locked: "1.0.0", wantNewer: true},
		{name: "patch downgrade is not newer", latest: "1.0.0", locked: "1.0.1", wantNewer: false},
		{name: "major bump is newer", latest: "2.0.0", locked: "1.9.9", wantNewer: true},
		{name: "release is newer than its own prerelease", latest: "1.0.0", locked: "1.0.0-rc1", wantNewer: true},
		{name: "prerelease is not newer than its own release", latest: "1.0.0-rc1", locked: "1.0.0", wantNewer: false},
		{name: "later prerelease is newer", latest: "1.0.0-rc2", locked: "1.0.0-rc1", wantNewer: true},
		{name: "numeric prerelease identifier outranks alpha alone", latest: "1.0.0-alpha.1", locked: "1.0.0-alpha", wantNewer: true},
		{
			name:   "alphabetic prerelease identifier outranks numeric one",
			latest: "1.0.0-alpha.beta", locked: "1.0.0-alpha.1", wantNewer: true,
		},
		{name: "build metadata is ignored for ordering", latest: "1.0.0+build", locked: "1.0.0", wantNewer: false},
		{name: "unparseable latest is an error", latest: "not-a-version", locked: "1.0.0", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := isNewerVersion(tt.latest, tt.locked)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("isNewerVersion(%q, %q) expected an error, got %v", tt.latest, tt.locked, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("isNewerVersion(%q, %q) unexpected error: %v", tt.latest, tt.locked, err)
			}
			if got != tt.wantNewer {
				t.Fatalf("isNewerVersion(%q, %q) = %v, want %v", tt.latest, tt.locked, got, tt.wantNewer)
			}
		})
	}
}

// TestExactVersionFromConstraints pins exactVersionFromConstraints's
// exact-version detection, including the "==" -> "=" normalization's
// interaction with the function's own single-"=" CutPrefix strip: bare,
// "=", and "==" forms of the same version must all converge on the identical
// exact string, a repeated identical "==" constraint is not a conflict, a
// genuinely conflicting pair of exact versions surfaces
// helpers.ErrConflictingExactVersions, a mixed exact-plus-exclusion
// constraint is correctly reported as non-exact rather than mis-parsed, and
// "===" (never rewritten) still fails as a malformed version/constraint. It
// also covers the library-based classification's x-range and wildcard
// handling ("1.x", "1.2.x", uppercase "1.X", and ">=1.0.0"), which a
// hand-maintained character guard could not recognize as non-exact.
func TestExactVersionFromConstraints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		wantErr     error
		name        string
		wantVersion string
		constraints []string
		wantExact   bool
		wantAnyErr  bool
	}{
		{name: "== form", constraints: []string{"==1.2.3"}, wantVersion: "1.2.3", wantExact: true},
		{name: "= form", constraints: []string{"=1.2.3"}, wantVersion: "1.2.3", wantExact: true},
		{name: "bare form", constraints: []string{"1.2.3"}, wantVersion: "1.2.3", wantExact: true},
		{
			name:        "repeated identical == is not a conflict",
			constraints: []string{"==1.2.3", "==1.2.3"},
			wantVersion: "1.2.3", wantExact: true,
		},
		{
			name:        "conflicting == versions",
			constraints: []string{"==1.2.3", "==1.2.4"},
			wantErr:     helpers.ErrConflictingExactVersions,
		},
		{
			name:        "exact plus exclusion is not exact",
			constraints: []string{"==1.0.0,!=1.0.5"},
			wantVersion: "", wantExact: false,
		},
		{name: "=== is a malformed guard", constraints: []string{"===1.2.3"}, wantAnyErr: true},
		{name: "1.x is a non-exact x-range", constraints: []string{"1.x"}, wantVersion: "", wantExact: false},
		{name: "1.2.x is a non-exact x-range", constraints: []string{"1.2.x"}, wantVersion: "", wantExact: false},
		{name: "1.X uppercase is a non-exact x-range", constraints: []string{"1.X"}, wantVersion: "", wantExact: false},
		{name: ">=1.0.0 stays non-exact", constraints: []string{">=1.0.0"}, wantVersion: "", wantExact: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			version, exact, err := exactVersionFromConstraints(tt.constraints)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("exactVersionFromConstraints(%v) error = %v, want errors.Is match for %v", tt.constraints, err, tt.wantErr)
				}
				return
			}
			if tt.wantAnyErr {
				if err == nil {
					t.Fatalf("exactVersionFromConstraints(%v) expected an error, got version=%q exact=%v", tt.constraints, version, exact)
				}
				return
			}
			if err != nil {
				t.Fatalf("exactVersionFromConstraints(%v) unexpected error: %v", tt.constraints, err)
			}
			if version != tt.wantVersion || exact != tt.wantExact {
				t.Fatalf("exactVersionFromConstraints(%v) = (%q, %v), want (%q, %v)",
					tt.constraints, version, exact, tt.wantVersion, tt.wantExact)
			}
		})
	}
}
