package solver

// term is a statement about a package that may be true or false for a given
// selection of versions: "Package in Set" (Positive) or "not (Package in
// Set)" (!Positive). Sets are exact verSet values (verset.go), so every
// judgment about terms below is universe-independent: it never needs to
// know which versions of the package are published, and it is exact from
// the first assignment on - the reference algorithm's term arithmetic, not
// an approximation of it.
//
// The signed algebra matters as much as set exactness: a package's
// accumulated state is a signed term, and the sign is what distinguishes
// "required to lie in a" (Positive) from "merely known not to lie in a"
// (Negative). A negative accumulation never satisfies a positive term and
// never contradicts a negative one - the two asymmetries that make a
// package with only negative assignments correctly read as still
// undetermined rather than as vacuously settled.

import (
	"slices"
)

// term pairs a package name with a signed exact version set.
type term struct {
	Package  string
	Set      verSet
	Positive bool
}

// Negate returns the logical negation of t. It only flips Positive; the
// underlying set is never touched (complementing a set is a distinct
// operation, used via the signed intersection below).
func (t term) Negate() term {
	return term{Package: t.Package, Set: t.Set, Positive: !t.Positive}
}

// accumSeed is the identity of signed intersection for pkg: "not (pkg in
// {})", the vacuously true statement every package's accumulation starts
// from.
func accumSeed(pkg string) term {
	return term{Package: pkg, Set: emptyVerSet(), Positive: false}
}

// termIntersect returns the conjunction of two statements about the same
// package, per the reference term arithmetic:
//
//	P(a) and P(b) = P(a intersect b)
//	P(a) and N(b) = P(a minus b)
//	N(a) and P(b) = P(b minus a)
//	N(a) and N(b) = N(a union b)
//
// The identity operand N({}) returns the other term verbatim, which both
// avoids an allocation and preserves the cosmetic carriers (a singleton's
// decided version, a constraint's display) through the seed fold.
func termIntersect(a, b term) term {
	if !a.Positive && a.Set.isEmpty() {
		return b
	}
	if !b.Positive && b.Set.isEmpty() {
		return a
	}
	switch {
	case a.Positive && b.Positive:
		return term{Package: a.Package, Set: a.Set.intersect(b.Set), Positive: true}
	case a.Positive:
		return term{Package: a.Package, Set: a.Set.difference(b.Set), Positive: true}
	case b.Positive:
		return term{Package: a.Package, Set: b.Set.difference(a.Set), Positive: true}
	default:
		return term{Package: a.Package, Set: a.Set.union(b.Set), Positive: false}
	}
}

// termSubset reports whether a on its own entails b (every selection
// satisfying a satisfies b):
//
//	P(a) entails P(b) iff a is a subset of b
//	P(a) entails N(b) iff a and b are disjoint
//	N(a) entails P(b): never - a negative statement cannot assert selection
//	N(a) entails N(b) iff b is a subset of a
func termSubset(a, b term) bool {
	switch {
	case a.Positive && b.Positive:
		return a.Set.subsetOf(b.Set)
	case a.Positive:
		return a.Set.disjointFrom(b.Set)
	case b.Positive:
		return false
	default:
		return b.Set.subsetOf(a.Set)
	}
}

// relateAccum relates one term to a package's accumulated signed state:
// satisfied when the accumulation entails the term, contradicted when their
// conjunction is the unsatisfiable P({}), inconclusive otherwise. A
// negative-negative conjunction is never P({}), so a negative accumulation
// never contradicts a negative term - the asymmetry that keeps a
// dependency's "not (dep in C)" term derivable (as "dep in C") for a
// package that so far carries only negative facts.
func relateAccum(accum, t term) termRelation {
	if termSubset(accum, t) {
		return termSatisfied
	}
	if joint := termIntersect(accum, t); joint.Positive && joint.Set.isEmpty() {
		return termContradicted
	}
	return termInconclusive
}

// termPermits reports whether selecting v would keep t true.
func termPermits(t term, v Version) bool {
	return t.Set.contains(v) == t.Positive
}

// termIsTautological reports whether t is true for every selection: exactly
// N({}), the negation of an unsatisfiable positive statement. A positive
// term is never tautological - even P(full) asserts that the package is
// selected at all, which is real information.
func termIsTautological(t term) bool {
	return !t.Positive && t.Set.isEmpty()
}

// sameVersionSet reports whether two sets denote the same set of versions.
// Exact sets make this total: canonical structural equality is set
// equality, with no representation caveats.
func sameVersionSet(a, b verSet) bool {
	return a.equalSet(b)
}

// sameTerm reports whether two terms are content-identical: same package,
// same polarity, same set.
func sameTerm(a, b term) bool {
	return a.Package == b.Package && a.Positive == b.Positive && sameVersionSet(a.Set, b.Set)
}

// compareVersionsDescending implements the universe total order: semver
// precedence descending, tie-broken by the original string descending
// (byte-wise). This is stricter than a single-level precedence comparator
// (which is not a total order for strings of equal precedence, e.g.
// "1.0.0" vs "1.0.0+build") and is what makes "the highest version" and
// "index 0" unambiguous. Sets cannot separate equal-precedence versions
// (membership ignores build metadata, exactly as Check does), so this
// order matters only for choosing among members, never for set identity.
func compareVersionsDescending(a, b Version) int {
	if c := b.sv().Compare(a.sv()); c != 0 {
		return c
	}
	switch {
	case a.original > b.original:
		return -1
	case a.original < b.original:
		return 1
	default:
		return 0
	}
}

// buildUniverse deduplicates versions by original string and sorts the
// result into the universe total order (descending).
func buildUniverse(versions []Version) []Version {
	seen := make(map[string]bool, len(versions))
	out := make([]Version, 0, len(versions))
	for _, v := range versions {
		if seen[v.original] {
			continue
		}
		seen[v.original] = true
		out = append(out, v)
	}
	slices.SortFunc(out, compareVersionsDescending)
	return out
}

// packageUniverse holds one package's published version list, once fetched
// from the provider. With exact sets it plays no part in relating terms or
// resolving conflicts - decision making consults it to pick a concrete
// published candidate, and error reporting consults it for cosmetic
// phrasing and prerelease hints; nothing else needs it.
type packageUniverse struct {
	versions []Version
	fetched  bool
}

// newPackageUniverse returns an unfetched universe placeholder.
func newPackageUniverse() *packageUniverse {
	return &packageUniverse{}
}

// setVersions installs versions (already sorted into total order) as u's
// universe and marks it fetched.
func (u *packageUniverse) setVersions(versions []Version) {
	u.versions = versions
	u.fetched = true
}
