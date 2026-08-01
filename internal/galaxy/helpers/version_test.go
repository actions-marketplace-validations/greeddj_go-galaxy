package helpers

import (
	"testing"

	"github.com/Masterminds/semver/v3"
)

// TestIsExactVersion pins the accept/reject boundary IsExactVersion draws:
// it accepts every lenient-but-harmless shape a registry may legitimately
// publish (a short version, a leading "v", a leading zero) and rejects every
// shape that names a range or carries something other than version
// characters, including a traversal fragment and an embedded newline.
func TestIsExactVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"exact three-part version", "1.0.0", true},
		{"short two-part version", "1.0", true},
		{"bare major version", "1", true},
		{"leading v prefix", "v1.0.0", true},
		{"leading zero in major", "01.0.0", true},
		{"wildcard constraint", "*", false},
		{"bare letter", "x", false},
		{"range constraint", ">=1.0.0", false},
		{"named tag", "latest", false},
		{"whitespace padded", " 1.0.0 ", false},
		{"empty", "", false},
		{"parent directory", "..", false},
		{"embedded traversal", "1.0.0/../x", false},
		{"trailing newline", "1.0.0\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsExactVersion(tc.value); got != tc.want {
				t.Errorf("IsExactVersion(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// exactVersionPathAlphabet is the rune set
// TestIsExactVersionImpliesIsPathElementExhaustive walks: every character
// class the vendored semver grammars (semVerRegex and looseSemVerRegex
// alike) treat specially - digits, the leading "v", and the "." "-" "+"
// separators - alongside the characters that would actually matter if the
// implication failed: the POSIX and Windows path separators, a NUL byte, a
// control byte, a space, and "*" - the shape a poisoned resolved-version
// entry carries in this project's own fixtures.
//
//nolint:gochecknoglobals // a fixed, immutable alphabet consumed by one test, not mutable shared state.
var exactVersionPathAlphabet = []rune{'0', '1', 'v', '.', '-', '+', '/', '\\', ' ', '\n', '\x00', '*'}

// exactVersionPathAlphabetMaxLen bounds
// TestIsExactVersionImpliesIsPathElementExhaustive's search: chosen so the
// full walk (every string up to this length, over exactVersionPathAlphabet,
// under both semver.CoerceNewVersion settings) finishes in a small fraction
// of a second, so it runs in every `go test` invocation rather than being an
// opt-in fuzz pass.
const exactVersionPathAlphabetMaxLen = 5

// TestIsExactVersionImpliesIsPathElementExhaustive proves the implication
// IsExactVersion's own doc comment claims - not by sampling a handful of
// fixtures, but by generating every string up to
// exactVersionPathAlphabetMaxLen runes long over exactVersionPathAlphabet
// and checking each one: IsExactVersion(s) true must never coexist with
// IsPathElement(s) false. This is what licenses
// collections.buildCollectionsMap replacing its version-side IsPathElement
// check with IsExactVersion rather than stacking both - dropping the
// redundant check cannot reopen a path-safety hole only as long as this
// property holds, and a property resting on a third-party regex and a
// third-party mutable package global (semver.CoerceNewVersion) needs more
// than six fixed strings to stand on. The walk runs under both
// CoerceNewVersion settings, since NewVersion's parse path - and therefore
// which of the two vendored grammars actually decides acceptance - depends
// on it. FuzzIsExactVersionImpliesIsPathElement below is the unbounded
// complement: this test's alphabet and length are both fixed and finite, so
// it can never itself be the sole net for a shape outside them.
//
// Deliberately no t.Parallel() here, unlike every other test in this file:
// this test flips the package-global semver.CoerceNewVersion for the
// duration of its run, and t.Parallel's guarantee that a parallel test never
// runs concurrently with a non-parallel one is the only thing keeping that
// flip from racing TestIsExactVersion above, which reads IsExactVersion (and
// therefore CoerceNewVersion) while itself running in parallel.
func TestIsExactVersionImpliesIsPathElementExhaustive(t *testing.T) {
	origCoerce := semver.CoerceNewVersion
	defer func() { semver.CoerceNewVersion = origCoerce }()

	var checked int
	for _, coerce := range []bool{true, false} {
		semver.CoerceNewVersion = coerce
		for length := 1; length <= exactVersionPathAlphabetMaxLen; length++ {
			walkAlphabetStrings(exactVersionPathAlphabet, length, func(s string) {
				checked++
				if IsExactVersion(s) && !IsPathElement(s) {
					t.Fatalf("CoerceNewVersion=%v: IsExactVersion(%q) is true but IsPathElement(%q) is false: the implication does not hold",
						coerce, s, s)
				}
			})
		}
	}
	t.Logf("checked %d strings (both CoerceNewVersion settings, up to length %d over a %d-rune alphabet)",
		checked, exactVersionPathAlphabetMaxLen, len(exactVersionPathAlphabet))
}

// walkAlphabetStrings calls visit once for every string of exactly length
// runes drawn, with repetition, from alphabet - every point in
// len(alphabet)^length space, in a fixed but otherwise unspecified order
// (an odometer over per-position indices). length == 0 calls visit once
// with "".
func walkAlphabetStrings(alphabet []rune, length int, visit func(string)) {
	if length == 0 {
		visit("")
		return
	}
	indices := make([]int, length)
	buf := make([]rune, length)
	for {
		for i, idx := range indices {
			buf[i] = alphabet[idx]
		}
		visit(string(buf))
		pos := length - 1
		for pos >= 0 {
			indices[pos]++
			if indices[pos] < len(alphabet) {
				break
			}
			indices[pos] = 0
			pos--
		}
		if pos < 0 {
			return
		}
	}
}

// FuzzIsExactVersionImpliesIsPathElement is the unbounded complement to
// TestIsExactVersionImpliesIsPathElementExhaustive: seeded with the same
// adversarial shapes exercised elsewhere in this package plus a few this
// file's own exhaustive alphabet cannot reach (a multi-byte rune, a
// standalone combining mark), so `go test
// -fuzz=FuzzIsExactVersionImpliesIsPathElement -fuzztime=<budget>` can
// search arbitrarily far past the exhaustive test's fixed alphabet and
// length. An ordinary `go test` run only replays this seed corpus (fast,
// not a fuzzing pass), so it adds no meaningful time to the suite; the
// exhaustive test above is what every run actually checks by default.
func FuzzIsExactVersionImpliesIsPathElement(f *testing.F) {
	seeds := []string{
		"1.0.0", "1.0", "1", "v1.0.0", "01.0.0", "1.2.3-beta.1+build.5",
		"*", "x", ">=1.0.0", "latest", " 1.0.0 ", "", "..", "1.0.0/../x", "1.0.0\n",
		"../etc/passwd", "1.0.0\r", "1.0.0/", "/1.0.0", "1.0.0\\x",
		"\x00", "\x1b", "\x7f", "é", "1.0.0é", "~", ":", "1:0:0",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if IsExactVersion(s) && !IsPathElement(s) {
			t.Fatalf("IsExactVersion(%q) is true but IsPathElement(%q) is false: the implication does not hold", s, s)
		}
	})
}
