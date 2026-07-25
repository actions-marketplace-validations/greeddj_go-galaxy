package helpers

import "testing"

// TestNormalizeConstraint pins NormalizeConstraint's pure-string contract:
// trim/match-all handling, byte-identical passthrough for any constraint that
// never uses "==", the clause-level "==" -> "=" rewrite (including when the
// "==" clause is not the first one, and when whitespace surrounds a clause),
// and the "===" / ">==" guards that must never be coerced since they are not
// ansible's exact-match operator.
func TestNormalizeConstraint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"empty string", "", ""},
		{"whitespace only", "   ", ""},
		{"match-all sentinel", "*", ""},
		{"match-all sentinel with surrounding whitespace", " * ", ""},
		{"passthrough without ==", ">=1.0.0,<2.0.0", ">=1.0.0,<2.0.0"},
		{"passthrough trims surrounding whitespace", "  >=1.0.0  ", ">=1.0.0"},
		{"== rewritten to =", "==1.2.3", "=1.2.3"},
		{"== in a later clause is rewritten", "!=1.0.5,==1.2.3", "!=1.0.5,=1.2.3"},
		{"== in the first clause, other clause preserved", "==1.0.0,!=1.0.5", "=1.0.0,!=1.0.5"},
		{"=== guard is left untouched", "===1.2.3", "===1.2.3"},
		{"does not start with == is left untouched", ">==1.2.3", ">==1.2.3"},
		{"per-clause whitespace is normalized around a == rewrite", " ==1.2.3 , !=1.0.5 ", "=1.2.3,!=1.0.5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeConstraint(tc.value); got != tc.want {
				t.Errorf("NormalizeConstraint(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}
