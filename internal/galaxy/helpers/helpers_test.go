package helpers

import "testing"

// TestSplitFQDNDoesNotValidatePathSafety pins that SplitFQDN is a pure
// exactly-one-dot string split with no path-safety opinion at all: a
// namespace containing a path separator still parses successfully as long as
// the whole value contains exactly one ".", exactly as a clean "ns.name"
// value would. This is why every caller that later uses the parsed
// namespace/name as a filesystem path element (buildCollectionsMap,
// newInstallTarget, cleanup.removeInstalled) must run its own IsPathElement
// check - SplitFQDN itself is not, and was never meant to be, that guard.
func TestSplitFQDNDoesNotValidatePathSafety(t *testing.T) {
	t.Parallel()
	ns, name, ok := SplitFQDN("foo/bar.baz")
	if !ok {
		t.Fatalf(`SplitFQDN("foo/bar.baz") ok = false, want true (SplitFQDN performs no path-safety validation)`)
	}
	if ns != "foo/bar" || name != "baz" {
		t.Fatalf(`SplitFQDN("foo/bar.baz") = (%q, %q), want ("foo/bar", "baz")`, ns, name)
	}
	if IsPathElement(ns) {
		t.Fatalf("test setup bug: %q must itself be unsafe (contain a path separator) for this pinning to matter", ns)
	}
}

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
