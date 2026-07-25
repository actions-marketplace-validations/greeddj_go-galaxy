package solver

import (
	"testing"
)

// constraintCase mirrors the row shape of
// internal/galaxy/collections/semver_semantics_test.go's constraintCase, so
// the same ground-truth semantics rows can drive versionSet.Contains here.
type constraintCase struct {
	name       string
	constraint string
	version    string
	wantMatch  bool
	wantErr    bool
}

// symbolicSemanticsCases reuses the pinned semver semantics rows: basic
// operators, x-ranges, tilde, caret (stable and zero-major), prerelease
// exclusion/inclusion, the "==" -> "=" rewrite, and the over-normalization
// guards. versionSet.Contains must agree with every row, since it is built
// from the exact same NormalizeConstraint -> semver.NewConstraint -> Check
// pipeline.
func symbolicSemanticsCases() []constraintCase {
	return []constraintCase{
		{name: "bare exact match", constraint: "1.2.3", version: "1.2.3", wantMatch: true},
		{name: "bare exact no-match", constraint: "1.2.3", version: "1.2.4", wantMatch: false},
		{name: "= exact match", constraint: "=1.2.3", version: "1.2.3", wantMatch: true},
		{name: "!= match", constraint: "!=1.2.3", version: "1.2.4", wantMatch: true},
		{name: "!= no-match", constraint: "!=1.2.3", version: "1.2.3", wantMatch: false},
		{name: ">= match at floor", constraint: ">=1.0.0", version: "1.0.0", wantMatch: true},
		{name: ">= no-match below floor", constraint: ">=1.0.0", version: "0.9.0", wantMatch: false},
		{name: "range match inside", constraint: ">=1.0.0,<2.0.0", version: "1.5.0", wantMatch: true},
		{name: "range no-match at ceiling", constraint: ">=1.0.0,<2.0.0", version: "2.0.0", wantMatch: false},
		{name: "* matches anything", constraint: "*", version: "0.0.1", wantMatch: true},
		{name: "empty matches anything", constraint: "", version: "9.9.9", wantMatch: true},
		{name: "1.x match", constraint: "1.x", version: "1.9.9", wantMatch: true},
		{name: "1.x no-match", constraint: "1.x", version: "2.0.0", wantMatch: false},
		{name: "~1.2.3 match", constraint: "~1.2.3", version: "1.2.9", wantMatch: true},
		{name: "~1.2.3 no-match", constraint: "~1.2.3", version: "1.3.0", wantMatch: false},
		{name: "^1.2.3 match", constraint: "^1.2.3", version: "1.9.9", wantMatch: true},
		{name: "^1.2.3 no-match", constraint: "^1.2.3", version: "2.0.0", wantMatch: false},
		{name: "^0.2.3 caret-zero-minor match", constraint: "^0.2.3", version: "0.2.9", wantMatch: true},
		{name: "^0.2.3 caret-zero-minor no-match", constraint: "^0.2.3", version: "0.3.0", wantMatch: false},
		{name: "^0.0.3 caret-zero-patch match", constraint: "^0.0.3", version: "0.0.3", wantMatch: true},
		{name: "^0.0.3 caret-zero-patch no-match", constraint: "^0.0.3", version: "0.0.4", wantMatch: false},
		{name: "prerelease excluded above range", constraint: ">=1.0.0", version: "2.0.0-rc1", wantMatch: false},
		{name: "prerelease included via -0 floor", constraint: ">=1.0.0-0", version: "1.0.0-rc1", wantMatch: true},
		{name: "bare prerelease exact match", constraint: "1.0.0-rc1", version: "1.0.0-rc1", wantMatch: true},
		{name: "bare prerelease exact no-match", constraint: "1.0.0-rc1", version: "1.0.0", wantMatch: false},
		{name: "== rewritten to = match", constraint: "==1.2.3", version: "1.2.3", wantMatch: true},
		{name: "==,!= match", constraint: "==1.0.0,!=1.0.5", version: "1.0.0", wantMatch: true},
		{name: "==,!= excluded", constraint: "==1.0.0,!=1.0.5", version: "1.0.5", wantMatch: false},
		{name: "=== stays a parse error", constraint: "===1.2.3", version: "1.2.3", wantErr: true},
		{name: ">== stays a parse error", constraint: ">==1.2.3", version: "1.2.3", wantErr: true},
	}
}

func TestVersionSetSymbolicContains(t *testing.T) {
	t.Parallel()
	for _, tt := range symbolicSemanticsCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			set, err := newSymbolicSet(tt.constraint)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newSymbolicSet(%q) expected an error", tt.constraint)
				}
				return
			}
			if err != nil {
				t.Fatalf("newSymbolicSet(%q) unexpected error: %v", tt.constraint, err)
			}
			v, err := NewVersion(tt.version)
			if err != nil {
				t.Fatalf("NewVersion(%q): %v", tt.version, err)
			}
			if got := set.Contains(v); got != tt.wantMatch {
				t.Fatalf("Contains(%q) against %q = %v, want %v", tt.version, tt.constraint, got, tt.wantMatch)
			}
		})
	}
}

// TestAnySetIsSingleIdentity pins the single-anySet-identity invariant:
// every unconstrained input (raw "*", raw empty, and their normalized form)
// must produce the exact same Key ("") as the package-level anySet value.
func TestAnySetIsSingleIdentity(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "*", "  ", " * "} {
		set, err := newSymbolicSet(raw)
		if err != nil {
			t.Fatalf("newSymbolicSet(%q): %v", raw, err)
		}
		if set.Key() != "" {
			t.Fatalf("newSymbolicSet(%q).Key() = %q, want \"\"", raw, set.Key())
		}
		if !set.isAny() {
			t.Fatalf("newSymbolicSet(%q) is not recognized as anySet", raw)
		}
		if !set.Contains(mustV(t, "0.0.1")) || !set.Contains(mustV(t, "999.999.999-rc1")) {
			t.Fatalf("newSymbolicSet(%q) does not admit every version, including prereleases", raw)
		}
	}
	if anySet.Key() != "" {
		t.Fatalf("anySet.Key() = %q, want \"\"", anySet.Key())
	}
}

func mustV(t *testing.T, raw string) Version {
	t.Helper()
	v, err := NewVersion(raw)
	if err != nil {
		t.Fatalf("NewVersion(%q): %v", raw, err)
	}
	return v
}

// buildTestUniverse parses and orders raw version strings the same way the
// core does when materializing a package's universe.
func buildTestUniverse(t *testing.T, raw ...string) []Version {
	t.Helper()
	versions := make([]Version, 0, len(raw))
	for _, r := range raw {
		versions = append(versions, mustV(t, r))
	}
	return buildUniverse(versions)
}

// TestUniverseTotalOrder pins the two-level total order: semver precedence
// descending, tie-broken by the original string byte-wise descending - the
// case a single-level precedence comparator gets wrong (equal precedence,
// different original strings).
func TestUniverseTotalOrder(t *testing.T) {
	t.Parallel()
	got := buildTestUniverse(t, "1.0.0", "2.0.0", "1.0.0+build", "1.5.0")
	want := []string{"2.0.0", "1.5.0", "1.0.0+build", "1.0.0"}
	if len(got) != len(want) {
		t.Fatalf("buildUniverse length = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Original() != w {
			t.Fatalf("buildUniverse[%d] = %q, want %q (full: %v)", i, got[i].Original(), w, originals(got))
		}
	}
}

// TestUniverseDedup pins deduplication by exact original string.
func TestUniverseDedup(t *testing.T) {
	t.Parallel()
	got := buildTestUniverse(t, "1.0.0", "1.0.0", "2.0.0")
	if len(got) != 2 {
		t.Fatalf("buildUniverse length = %d, want 2 (dedup by original string): %v", len(got), originals(got))
	}
}

func originals(vs []Version) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Original()
	}
	return out
}

// ---- bitset algebra, property-checked against direct enumeration ---------

func enumerateBits(b []uint64, n int) []int {
	var out []int
	for i := range n {
		if testBit(b, i) {
			out = append(out, i)
		}
	}
	return out
}

// bitsFromTestUniverseSize is the fixed universe size every TestBitsetAlgebra*
// subtest below shares.
const bitsFromTestUniverseSize = 10

// bitsFrom builds a bitsFromTestUniverseSize-cell test bitset with bit i set
// for every i in on. Every caller works within the same fixed test universe,
// so this hardcodes that size rather than threading an always-identical
// parameter.
func bitsFrom(on ...int) []uint64 {
	b := newBits(bitsFromTestUniverseSize)
	for _, i := range on {
		setBit(b, i)
	}
	return b
}

// TestBitsetAlgebraSetOps covers the binary set-combinator operations:
// intersect, union, difference, and complement.
func TestBitsetAlgebraSetOps(t *testing.T) {
	t.Parallel()
	const n = bitsFromTestUniverseSize
	a := bitsFrom(1, 3, 5, 7)
	b := bitsFrom(3, 5, 9)

	t.Run("intersect", func(t *testing.T) {
		t.Parallel()
		got := enumerateBits(intersectNew(a, b), n)
		want := []int{3, 5}
		if !slicesEqual(got, want) {
			t.Fatalf("intersect = %v, want %v", got, want)
		}
	})
	t.Run("union", func(t *testing.T) {
		t.Parallel()
		got := enumerateBits(unionNew(a, b), n)
		want := []int{1, 3, 5, 7, 9}
		if !slicesEqual(got, want) {
			t.Fatalf("union = %v, want %v", got, want)
		}
	})
	t.Run("difference", func(t *testing.T) {
		t.Parallel()
		got := enumerateBits(differenceNew(a, b), n)
		want := []int{1, 7}
		if !slicesEqual(got, want) {
			t.Fatalf("difference a\\b = %v, want %v", got, want)
		}
	})
	t.Run("complement", func(t *testing.T) {
		t.Parallel()
		got := enumerateBits(complementNew(a, n), n)
		want := []int{0, 2, 4, 6, 8, 9}
		if !slicesEqual(got, want) {
			t.Fatalf("complement = %v, want %v", got, want)
		}
	})
}

// TestBitsetAlgebraPredicates covers the boolean predicates: subset,
// disjointness, popcount, and emptiness.
func TestBitsetAlgebraPredicates(t *testing.T) {
	t.Parallel()
	const n = bitsFromTestUniverseSize
	a := bitsFrom(1, 3, 5, 7)
	b := bitsFrom(3, 5, 9)

	t.Run("subset true", func(t *testing.T) {
		t.Parallel()
		sub := bitsFrom(3, 5)
		if !subset(sub, a) {
			t.Fatalf("expected %v to be a subset of %v", enumerateBits(sub, n), enumerateBits(a, n))
		}
	})
	t.Run("subset false", func(t *testing.T) {
		t.Parallel()
		if subset(a, bitsFrom(3, 5)) {
			t.Fatalf("did not expect %v to be a subset of {3,5}", enumerateBits(a, n))
		}
	})
	t.Run("disjoint true", func(t *testing.T) {
		t.Parallel()
		if !disjointBits(a, bitsFrom(0, 2, 4)) {
			t.Fatalf("expected disjoint sets to be reported disjoint")
		}
	})
	t.Run("disjoint false", func(t *testing.T) {
		t.Parallel()
		if disjointBits(a, b) {
			t.Fatalf("did not expect overlapping sets to be reported disjoint")
		}
	})
	t.Run("popcount", func(t *testing.T) {
		t.Parallel()
		if got := popcount(a); got != 4 {
			t.Fatalf("popcount = %d, want 4", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		if !isEmptyBits(newBits(n)) {
			t.Fatalf("expected a freshly allocated bitset to be empty")
		}
		if isEmptyBits(a) {
			t.Fatalf("did not expect a non-empty bitset to be reported empty")
		}
	})
}

// TestBitsetAlgebraOrdering covers lowestSetBit (the "highest version"
// query) and fullBits' trailing-bit masking.
func TestBitsetAlgebraOrdering(t *testing.T) {
	t.Parallel()
	const n = bitsFromTestUniverseSize
	a := bitsFrom(1, 3, 5, 7)

	t.Run("highest member is the lowest set bit", func(t *testing.T) {
		t.Parallel()
		idx, ok := lowestSetBit(a)
		if !ok || idx != 1 {
			t.Fatalf("lowestSetBit(a) = (%d, %v), want (1, true)", idx, ok)
		}
	})
	t.Run("highest member of empty set is absent", func(t *testing.T) {
		t.Parallel()
		if _, ok := lowestSetBit(newBits(n)); ok {
			t.Fatalf("lowestSetBit of an empty set should report ok=false")
		}
	})
	t.Run("full bits are exactly n members", func(t *testing.T) {
		t.Parallel()
		full := fullBits(n)
		if popcount(full) != n {
			t.Fatalf("popcount(fullBits(%d)) = %d, want %d", n, popcount(full), n)
		}
		// Trailing bits beyond n in the last word must stay masked to zero,
		// since complement and other whole-word operations flip them.
		comp := complementNew(full, n)
		if !isEmptyBits(comp) {
			t.Fatalf("complement of a full n-bit set should be empty, got %v", enumerateBits(comp, 64))
		}
	})
}

func slicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- materialization -------------------------------------------------------

// TestMaterializationEquivalence pins the core correctness invariant of
// materialization: for every universe member, the bit in the materialized
// set must equal what symbolic Contains would say directly.
func TestMaterializationEquivalence(t *testing.T) {
	t.Parallel()
	u := newPackageUniverse()
	u.setVersions(buildTestUniverse(t, "1.0.0", "1.5.0", "2.0.0", "2.0.0-rc1", "0.9.0"))

	for _, raw := range []string{"^1.0.0", ">=1.0.0,<2.0.0", "*", "", "=1.5.0", ">=1.0.0-0"} {
		set, err := newSymbolicSet(raw)
		if err != nil {
			t.Fatalf("newSymbolicSet(%q): %v", raw, err)
		}
		bits := u.materializeSet(set)
		for i, v := range u.versions {
			want := set.Contains(v)
			got := testBit(bits, i)
			if got != want {
				t.Fatalf("constraint %q: materialized bit[%d] (%s) = %v, want %v", raw, i, v.Original(), got, want)
			}
		}
	}
}

// TestClassificationCacheReuse pins that materializing the same symbolic key
// twice returns the exact same cached bitset (identity reuse, not just
// value equality), and that the cache is keyed by symbolic key rather than
// re-running Contains again for a distinct versionSet value with the same
// key.
func TestClassificationCacheReuse(t *testing.T) {
	t.Parallel()
	u := newPackageUniverse()
	u.setVersions(buildTestUniverse(t, "1.0.0", "2.0.0", "3.0.0"))

	setA, err := newSymbolicSet("^1.0.0")
	if err != nil {
		t.Fatalf("newSymbolicSet: %v", err)
	}
	setB, err := newSymbolicSet("^1.0.0")
	if err != nil {
		t.Fatalf("newSymbolicSet: %v", err)
	}

	bitsA := u.materializeSet(setA)
	bitsB := u.materializeSet(setB)
	if &bitsA[0] != &bitsB[0] {
		t.Fatalf("materializeSet did not reuse the cached bitset for an identical symbolic key")
	}
	if len(u.classCache) != 1 {
		t.Fatalf("classCache has %d entries, want exactly 1 for one distinct key", len(u.classCache))
	}
}

// TestSingletonSetIsExactMembership pins that a decision's singleton set
// (built by singletonSet) matches exactly one version and nothing else,
// through the same Masterminds Check authority as any other set.
func TestSingletonSetIsExactMembership(t *testing.T) {
	t.Parallel()
	v := mustV(t, "1.2.3")
	set := singletonSet(v)
	if !set.Contains(v) {
		t.Fatalf("singletonSet(%s) does not contain itself", v.Original())
	}
	if set.Contains(mustV(t, "1.2.4")) {
		t.Fatalf("singletonSet(%s) unexpectedly contains 1.2.4", v.Original())
	}
	if !set.isSingleton {
		t.Fatalf("singletonSet(%s).isSingleton = false", v.Original())
	}
}
