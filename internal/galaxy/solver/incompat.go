package solver

import (
	"hash/fnv"
	"slices"
	"strconv"
)

// cause is the closed sum of reasons an incompatibility exists. External
// incompatibilities (every variant but causeConflict) track the external
// fact that produced them; causeConflict is the derivation-graph edge
// produced during conflict resolution.
type cause interface {
	isCause()
}

// causeRoot is the initial incompatibility "not root in {0.0.0}".
type causeRoot struct{}

func (causeRoot) isCause() {}

// causeDependency records "Parent@ParentVersion depends on Dep Constraint",
// which produces the two-term incompatibility
// {Parent in {ParentVersion}, not Dep in Constraint}.
type causeDependency struct {
	ParentVersion Version
	Parent        string
	Dep           string
	Constraint    string
}

func (causeDependency) isCause() {}

// causeNoVersions records that decision making found an empty candidate set
// for term's package.
type causeNoVersions struct {
	term term
}

func (causeNoVersions) isCause() {}

// causeUnknownPackage records that the provider reported zero published
// versions for Package.
type causeUnknownPackage struct {
	Package string
}

func (causeUnknownPackage) isCause() {}

// causeConflict is a derived incompatibility's derivation-graph edge: Left
// is the previously conflicting incompatibility, Right is the satisfier's
// cause, from a single step of conflict resolution's generalized resolution
// rule.
type causeConflict struct {
	Left  *incompatibility
	Right *incompatibility
}

func (causeConflict) isCause() {}

// incompatibility is a set of terms that cannot all be true in a solution.
// Terms is normalized: at most one term per package, sorted by package
// ascending.
type incompatibility struct {
	Cause cause
	Terms []term
}

// isTerminal reports whether inc's terms are empty, or a single positive
// term referring to the root package - the terminal condition that proves
// no solution exists.
func (inc *incompatibility) isTerminal() bool {
	if len(inc.Terms) == 0 {
		return true
	}
	return len(inc.Terms) == 1 && inc.Terms[0].Positive && inc.Terms[0].Package == rootPkg
}

// termForPackage returns inc's term naming pkg, if any. Incompatibilities
// are normalized to at most one term per package, so this is unambiguous.
func (inc *incompatibility) termForPackage(pkg string) (term, bool) {
	for _, t := range inc.Terms {
		if t.Package == pkg {
			return t, true
		}
	}
	return term{}, false
}

// normalizeTerms merges multiple terms for the same package into one
// (intersecting their permitted-sets via boundary-extended bitset algebra -
// the only producer of multi-term-per-package input is conflict resolution,
// by which point every named package is materialized), sorts the result by
// package ascending, and - when more than one term remains - drops positive
// terms naming the root package. Root is the one package structurally
// guaranteed to be part of every solution (it is always decided first,
// unconditionally, regardless of any other package's availability), so its
// own singleton term is always true and contributes nothing to whether the
// incompatibility can fire. This does NOT generalize to any other package
// whose universe happens to have shrunk to one version: an ordinary
// package's term is only trivially true IF that package ends up selected at
// all, which is conditional on something actually depending on it -
// dropping it would silently discard that conditionality. uni resolves a
// package name to its materialization context. The merge itself is called
// only from conflict resolution, so it always operates over the
// boundary-extended universe (term.go); the result stays in that
// representation, since the partial solution's running intersection and
// relation's Case C now read extended-universe terms directly.
func normalizeTerms(terms []term, uni func(string) *packageUniverse) []term {
	byPkg := make(map[string][]term, len(terms))
	order := make([]string, 0, len(terms))
	for _, t := range terms {
		if _, ok := byPkg[t.Package]; !ok {
			order = append(order, t.Package)
		}
		byPkg[t.Package] = append(byPkg[t.Package], t)
	}

	merged := make([]term, 0, len(order))
	for _, pkg := range order {
		merged = append(merged, mergeTermGroup(pkg, byPkg[pkg], uni))
	}

	slices.SortFunc(merged, comparePackageAsc)
	if len(merged) > 1 {
		merged = dropPositiveRoot(merged)
	}
	return merged
}

// mergeTermGroup collapses one package's group of terms into a single term:
// the sole member unchanged if the group has exactly one, otherwise the
// boundary-extended bitset intersection of every member's permitted set,
// always re-emitted as a positive term (mirroring projectTermToPublished's
// own polarity convention for a merged result).
func mergeTermGroup(pkg string, group []term, uni func(string) *packageUniverse) term {
	if len(group) == 1 {
		return group[0]
	}
	u := uni(pkg)
	bits := slices.Clone(permittedExtBits(group[0], u)) // own copy: never alias the classification cache
	for _, t := range group[1:] {
		bits = intersectNew(bits, permittedExtBits(t, u))
	}
	return term{Package: pkg, Set: u.asExtBitsetSet(bits, "(merged)"), Positive: true}
}

// comparePackageAsc orders terms by package name ascending, the canonical
// incompatibility term order normalizeTerms produces.
func comparePackageAsc(a, b term) int {
	switch {
	case a.Package < b.Package:
		return -1
	case a.Package > b.Package:
		return 1
	default:
		return 0
	}
}

// dropPositiveRoot removes any positive term naming the root package from
// terms - sound only once more than one term remains (see normalizeTerms'
// doc comment).
func dropPositiveRoot(terms []term) []term {
	out := make([]term, 0, len(terms))
	for _, t := range terms {
		if t.Positive && t.Package == rootPkg {
			continue
		}
		out = append(out, t)
	}
	return out
}

// incompatStore is the append-only collection of known incompatibilities,
// indexed by package name in append order (the order unit propagation's
// newest-to-oldest scan relies on) and content-deduplicated so a repeat
// derivation never grows the store or the per-package scan.
type incompatStore struct {
	byPkg   map[string][]int
	content map[uint64][]int
	all     []*incompatibility
}

func newIncompatStore() *incompatStore {
	return &incompatStore{
		byPkg:   make(map[string][]int),
		content: make(map[uint64][]int),
	}
}

// add inserts inc unless content-identical to an already-stored
// incompatibility (same terms: package, polarity, and set identity, in
// order), in which case it returns the existing entry's index. The hash is a
// prefilter only; equality is always decided by direct term comparison.
func (s *incompatStore) add(inc *incompatibility) (int, bool) {
	h := hashIncompat(inc)
	for _, cand := range s.content[h] {
		if incompatEqual(s.all[cand], inc) {
			return cand, false
		}
	}
	idx := len(s.all)
	s.all = append(s.all, inc)
	s.content[h] = append(s.content[h], idx)
	seenPkg := make(map[string]bool, len(inc.Terms))
	for _, t := range inc.Terms {
		if seenPkg[t.Package] {
			continue
		}
		seenPkg[t.Package] = true
		s.byPkg[t.Package] = append(s.byPkg[t.Package], idx)
	}
	return idx, true
}

// byPackageNewestFirst returns the indices of incompatibilities naming pkg,
// newest (most recently added) first - the scan order unit propagation uses.
func (s *incompatStore) byPackageNewestFirst(pkg string) []int {
	indices := s.byPkg[pkg]
	out := make([]int, len(indices))
	for i, idx := range indices {
		out[len(indices)-1-i] = idx
	}
	return out
}

// incompatEqual reports whether two incompatibilities have identical term
// lists (same length, same content at each position - both are already
// normalized into the same package-ascending order, so positional
// comparison is sound).
func incompatEqual(a, b *incompatibility) bool {
	if len(a.Terms) != len(b.Terms) {
		return false
	}
	for i := range a.Terms {
		if !sameTerm(a.Terms[i], b.Terms[i]) {
			return false
		}
	}
	return true
}

// hashIncompat computes a prefilter hash over an incompatibility's terms.
// Collisions only cost an extra incompatEqual comparison; they can never
// cause a real duplicate to be dropped, since incompatEqual always decides
// final equality.
func hashIncompat(inc *incompatibility) uint64 {
	h := fnv.New64a()
	for _, t := range inc.Terms {
		_, _ = h.Write([]byte(t.Package))
		_, _ = h.Write([]byte{0})
		if t.Positive {
			_, _ = h.Write([]byte{1})
		} else {
			_, _ = h.Write([]byte{0})
		}
		if t.Set.kind == setSymbolic {
			_, _ = h.Write([]byte(t.Set.key))
		} else {
			for _, w := range t.Set.bits {
				_, _ = h.Write([]byte(strconv.FormatUint(w, 16)))
			}
		}
		_, _ = h.Write([]byte{0xff})
	}
	return h.Sum64()
}
