package solver

import (
	"fmt"
	"math/bits"
	"slices"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// wordBits is the number of bits held by a single bitset word (a uint64).
const wordBits = 64

// boundaryCells is the number of synthetic cells the boundary-extended
// universe adds beyond a package's published version count: one below-all
// cell and one above-all cell (see the boundary-extended universe comment
// below).
const boundaryCells = 2

// versionSetKind distinguishes the two states a versionSet can be in.
type versionSetKind uint8

const (
	// setSymbolic denotes a set expressed as a conjunction of Masterminds
	// constraints, not yet resolved against any concrete universe.
	setSymbolic versionSetKind = iota
	// setBitset denotes a set materialized against a specific package's
	// published-version universe: one bit per universe index, index 0 =
	// highest version. This is the representation the entire hot path
	// (relation's Case C, the partial solution's running intersections,
	// decision making's candidate selection) reads and writes; a decision
	// only ever selects a published version, so it never needs anything
	// else.
	setBitset
	// setExtBitset denotes a set materialized against a package's
	// boundary-extended universe (one below-all cell, one cell per
	// published version, one above-all cell), to avoid the false
	// tautologies a single- or few-version package's published-only bitset
	// would otherwise produce (see packageUniverse's extended
	// materialization). The partial solution's running intersection and
	// relation's Case C are kept in this representation too, so a learned
	// incompatibility from conflict resolution's own merge/difference
	// arithmetic flows straight into the hot path without ever needing a
	// projection step back to setBitset; decision making is the only
	// consumer that still projects down to published-only point cells
	// (pointCellsOf), since a decision can only ever select a published
	// version.
	setExtBitset
)

// versionSet denotes a subset of one package's versions. It is unexported:
// callers outside the package never construct or inspect one directly.
//
// A symbolic set carries the parsed constraint plus a canonical key derived
// from the normalized constraint source string; two symbolic sets with equal
// keys are considered the same set without ever comparing anything else.
// There is exactly one canonical unconstrained set (anySet, key ""), reused
// for every unconstrained term regardless of package.
//
// A bitset set carries one bit per index of a specific package's
// materialized universe. Conversion only ever happens symbolic -> bitset,
// triggered when a package's universe becomes available; it never happens in
// the other direction.
//
// display is a cosmetic, human-readable label used only by error reporting;
// it must never be branched on for any logic decision.
type versionSet struct {
	constraint  *semver.Constraints
	singleton   Version
	key         string
	display     string
	bits        []uint64
	universeN   int
	kind        versionSetKind
	isSingleton bool
}

// anySet is the single canonical unconstrained versionSet. Every symbolic
// input that normalizes to "" (raw "*", raw empty, or any equivalent through
// helpers.NormalizeConstraint) maps to this exact value.
//
//nolint:gochecknoglobals // a canonical, immutable singleton of the algorithm itself, not mutable shared state
var anySet = versionSet{kind: setSymbolic, key: "", display: "any"}

// newSymbolicSet parses raw as a canonical constraint expression (already
// expected to be, or normalize to, helpers.NormalizeConstraint's output) and
// returns the corresponding symbolic versionSet. An empty normalized form
// returns the single anySet identity.
func newSymbolicSet(raw string) (versionSet, error) {
	normalized := helpers.NormalizeConstraint(raw)
	if normalized == "" {
		return anySet, nil
	}
	c, err := semver.NewConstraint(normalized)
	if err != nil {
		return versionSet{}, fmt.Errorf("invalid version constraint %q: %w", raw, err)
	}
	return versionSet{kind: setSymbolic, constraint: c, key: normalized, display: normalized}, nil
}

// singletonSet returns the symbolic versionSet denoting exactly {v}. It
// reuses Masterminds' own exact-match operator ("=") to build the set, so
// membership on it goes through the same sole authority (Check) as every
// other set - never a hand-rolled equality comparison. errSolverBug's own
// doc comment (solver.go) is the home of the panic-vs-error rule this
// function's own panic falls under.
func singletonSet(v Version) versionSet {
	key := "=" + v.Original()
	c, err := semver.NewConstraint(key)
	if err != nil {
		// v.Original() only ever came from a string that already parsed
		// successfully via semver.NewVersion (that is the only way to build a
		// Version), so prefixing it with "=" cannot fail in practice; a
		// failure here is a solver invariant violation, not a data problem.
		panic(fmt.Sprintf("solver: exact constraint for %q failed to parse: %v", v.Original(), err))
	}
	return versionSet{kind: setSymbolic, constraint: c, key: key, display: v.Original(), singleton: v, isSingleton: true}
}

// Contains reports whether v satisfies s. It is only meaningful for a
// symbolic set (a bitset set has no notion of a version's identity without
// its owning package's universe, so membership on one goes through
// setContains instead). Masterminds' Check is the sole satisfaction
// authority: this mirrors constraintSatisfied's pipeline (normalize, parse,
// Check, vacuously true when unconstrained) and never re-implements any part
// of constraint semantics.
func (s versionSet) Contains(v Version) bool {
	if s.constraint == nil {
		return true
	}
	return s.constraint.Check(v.sv())
}

// Key returns the canonical identity of the constraint expression alone,
// independent of any package: two symbolic sets built from the same
// normalized constraint string share a Key, and the single anySet's Key is
// the empty string. Key is meaningless for a bitset set.
func (s versionSet) Key() string {
	return s.key
}

// isAny reports whether s is the canonical unconstrained set.
func (s versionSet) isAny() bool {
	return s.kind == setSymbolic && s.constraint == nil
}

// setContains tests membership of v in s given uni, the owning package's
// materialization context (needed only when s is a bitset: a symbolic set
// answers directly via Contains). A published-universe bitset indexes v
// directly; a boundary-extended one offsets by one cell, since point cell
// i+1 is where published index i lives (cell 0 is the below-all boundary).
func setContains(s versionSet, v Version, uni *packageUniverse) bool {
	if s.kind == setSymbolic {
		return s.Contains(v)
	}
	idx, ok := uni.indexOf(v)
	if !ok {
		return false
	}
	if s.kind == setExtBitset {
		return testBit(s.bits, idx+1)
	}
	return testBit(s.bits, idx)
}

// ---- bitset algebra -------------------------------------------------------
//
// Bitsets are plain []uint64, word 0 covering universe indices [0,64), and so
// on. Every bitset is pre-sized to wordsFor(n) and every operation that can
// produce set bits beyond the tracked universe length masks them back to
// zero, so popcount/emptiness/subset checks never need to know n separately
// from len(bits)*64 in the interior algebra - only construction and
// complement need it explicitly.

// wordsFor returns the number of uint64 words needed to hold n bits.
func wordsFor(n int) int {
	return (n + wordBits - 1) / wordBits
}

// newBits returns a zeroed bitset sized for n universe indices.
func newBits(n int) []uint64 {
	return make([]uint64, wordsFor(n))
}

// fullBits returns a bitset with exactly the first n bits set (a full
// universe reading), used as the vacuous "no constraint yet" starting point
// for running intersections and satisfier scans.
func fullBits(n int) []uint64 {
	b := make([]uint64, wordsFor(n))
	for i := range b {
		b[i] = ^uint64(0)
	}
	maskTrailing(b, n)
	return b
}

// maskTrailing zeroes any bits at or beyond index n in the last word of b,
// so operations like complement (which flips every bit including padding)
// never leave spurious high bits set.
func maskTrailing(b []uint64, n int) {
	if len(b) == 0 {
		return
	}
	extra := len(b)*wordBits - n
	if extra <= 0 {
		return
	}
	last := len(b) - 1
	b[last] &^= (^uint64(0)) << (wordBits - extra)
}

func setBit(b []uint64, i int) {
	b[i/wordBits] |= 1 << (i % wordBits)
}

func clearBit(b []uint64, i int) {
	b[i/wordBits] &^= 1 << (i % wordBits)
}

func testBit(b []uint64, i int) bool {
	w := i / wordBits
	if w >= len(b) {
		return false
	}
	return b[w]&(1<<(i%wordBits)) != 0
}

func popcount(b []uint64) int {
	n := 0
	for _, w := range b {
		n += bits.OnesCount64(w)
	}
	return n
}

func isEmptyBits(b []uint64) bool {
	for _, w := range b {
		if w != 0 {
			return false
		}
	}
	return true
}

// subset reports whether a is a subset of b (a ⊆ b).
func subset(a, b []uint64) bool {
	for i := range a {
		if a[i]&^b[i] != 0 {
			return false
		}
	}
	return true
}

// disjointBits reports whether a and b share no set bit (a ∩ b = ∅).
func disjointBits(a, b []uint64) bool {
	for i := range a {
		if a[i]&b[i] != 0 {
			return false
		}
	}
	return true
}

func intersectNew(a, b []uint64) []uint64 {
	out := make([]uint64, len(a))
	for i := range a {
		out[i] = a[i] & b[i]
	}
	return out
}

func unionNew(a, b []uint64) []uint64 {
	out := make([]uint64, len(a))
	for i := range a {
		out[i] = a[i] | b[i]
	}
	return out
}

// differenceNew computes a \ b (a AND NOT b).
func differenceNew(a, b []uint64) []uint64 {
	out := make([]uint64, len(a))
	for i := range a {
		out[i] = a[i] &^ b[i]
	}
	return out
}

// complementNew computes the complement of a within a universe of n
// versions.
func complementNew(a []uint64, n int) []uint64 {
	out := make([]uint64, len(a))
	for i := range a {
		out[i] = ^a[i]
	}
	maskTrailing(out, n)
	return out
}

// lowestSetBit returns the index of the lowest set bit in b (index 0 =
// highest version, so this is "the highest-precedence member").
func lowestSetBit(b []uint64) (int, bool) {
	for w, word := range b {
		if word != 0 {
			return w*64 + bits.TrailingZeros64(word), true
		}
	}
	return 0, false
}

// bitsEqual reports whether a and b are word-for-word identical.
func bitsEqual(a, b []uint64) bool {
	return slices.Equal(a, b)
}

// ---- package universe and materialization ---------------------------------

// packageUniverse holds one package's materialization state: its ordered
// version list (once fetched), the classification cache that backs
// symbolic-to-bitset conversion against the published universe, and a
// second, separate cache plus a pair of cached probe versions for the
// boundary-extended universe conflict resolution uses internally.
type packageUniverse struct {
	byOriginal     map[string]int
	classCache     map[string][]uint64
	extClassCache  map[string][]uint64
	extAbove       Version
	extBelow       Version
	versions       []Version
	materialized   bool
	extProbesReady bool
}

// newPackageUniverse returns an unmaterialized universe placeholder.
func newPackageUniverse() *packageUniverse {
	return &packageUniverse{
		classCache:    make(map[string][]uint64),
		extClassCache: make(map[string][]uint64),
	}
}

// setVersions installs versions (already sorted into total order) as u's
// universe and marks u materialized. It builds the original-string index
// used by indexOf and setContains.
func (u *packageUniverse) setVersions(versions []Version) {
	u.versions = versions
	u.materialized = true
	u.byOriginal = make(map[string]int, len(versions))
	for i, v := range versions {
		u.byOriginal[v.Original()] = i
	}
}

// indexOf returns v's index in u's universe, if present.
func (u *packageUniverse) indexOf(v Version) (int, bool) {
	i, ok := u.byOriginal[v.Original()]
	return i, ok
}

// materializeSet returns the bitset reading of set against u, computing and
// caching a fresh classification for a not-yet-seen symbolic key. A set that
// is already a bitset is returned unchanged (materialization is idempotent).
func (u *packageUniverse) materializeSet(set versionSet) []uint64 {
	if set.kind == setBitset {
		return set.bits
	}
	if bits, ok := u.classCache[set.key]; ok {
		return bits
	}
	bits := newBits(len(u.versions))
	if set.isAny() {
		for i := range u.versions {
			setBit(bits, i)
		}
	} else {
		for i, v := range u.versions {
			if set.Contains(v) {
				setBit(bits, i)
			}
		}
	}
	u.classCache[set.key] = bits
	return bits
}

// asBitsetSet wraps bits materialized against u into a display-carrying
// versionSet value, for cases that need a first-class materialized set
// rather than a raw []uint64 (e.g. a decision's or a conflict-resolution
// derived term's Set).
func (u *packageUniverse) asBitsetSet(bits []uint64, display string) versionSet {
	return versionSet{kind: setBitset, bits: bits, universeN: len(u.versions), display: display}
}

// rawBits returns the POSITIVE-reading bitset backing a term's set (ignoring
// the term's own polarity): for a bitset set this is its bits verbatim, for
// a symbolic set this is its materialized classification against uni.
func rawBits(set versionSet, uni *packageUniverse) []uint64 {
	if set.kind == setBitset {
		return set.bits
	}
	return uni.materializeSet(set)
}

// permittedBits returns the bitset of versions t actually permits (a fresh
// allocation only when t is negative, since the positive case is exactly the
// raw classification/cache bits). Production code never calls it - the
// propagation fast path, conflict resolution, and Case C all use the
// polarity-aware helpers below instead, to avoid this allocation - it exists
// as an oracle for test cross-checks of merged terms' permitted sets.
func permittedBits(t term, uni *packageUniverse) []uint64 {
	raw := rawBits(t.Set, uni)
	if t.Positive {
		return raw
	}
	return complementNew(raw, len(uni.versions))
}

// ---- boundary-extended universe ---------------------------------------
//
// A package whose published universe has few members - most acutely, one -
// makes an ordinary constraint look tautologically true against the
// published-only bitset (e.g. "not X == 1.0.0" permits "every other
// published version", which for a one-version package is the empty set,
// same as "no versions at all" would read). That false tautology is
// harmless for decision making and relation's Case C (a decision only ever
// picks a published version, so the published-only reading is always exact
// there), but it breaks conflict resolution's satisfier search: a
// trivially-"always true" term can be satisfied without needing the real
// assignment that actually forced it, discarding the dependency edge that
// explains why the package was ever relevant.
//
// The fix is discretized interval algebra: classify every set the partial
// solution's running intersection, relation's Case C, and conflict
// resolution touch over an m+2 cell universe instead (one below-all cell,
// one point cell per published version, one above-all cell), so a
// one-version package's exact-match term no longer spans every cell. The
// bitset algebra itself does not change at all - every op above is already
// generic over cell count - only the universe the sets are classified
// against does. Membership at every cell, including the two boundary
// probes, is still decided by Masterminds' Check alone; the probe versions
// are built by integer major-bump and the fixed literal minima 0.0.0 /
// 0.0.0-0, never by parsing or manipulating the constraint expression, so
// prerelease semantics (exclusion, -0 floors, exact prerelease pins) are
// inherited from Check exactly as for any real version.
//
// A single boundary cell stands for an unbounded, possibly non-homogeneous
// region, so a set can be over- or under-approximated there (e.g. "not
// X<=1.0.0" may carry a spurious below-all bit); this is sound by
// construction and never produces a wrong solution, since a boundary cell
// is a fictional version that decision making can never select - it is
// always excluded by pointCellsOf's projection before a candidate is ever
// chosen. Interior gaps between adjacent published versions are likewise not
// modeled as their own cells: a constraint matching only unpublished
// versions strictly between two published ones still classifies to empty,
// which is solution-sound (no published candidate exists there anyway).
//
// projectTermToPublished, kept for symmetry and test use, is otherwise
// unused in this design: nothing on the hot path ever needs to collapse an
// extended term back to published-only cells, since the running
// intersection and Case C read the extended representation directly.

// extendedLen returns the number of cells in u's boundary-extended
// universe.
func (u *packageUniverse) extendedLen() int {
	return len(u.versions) + boundaryCells
}

// ensureExtendedProbes computes and caches u's two boundary probe versions,
// pure functions of the deterministically-sorted published universe alone
// (no map iteration, no provider-order dependence): p_above is a release
// one major version beyond the highest published version (or 1.0.0 if the
// package has none), always strictly greater than every published version;
// p_below is the fixed literal 0.0.0 when the lowest published version is
// itself greater than 0.0.0, or the fixed literal 0.0.0-0 (a prerelease
// floor below even 0.0.0) when the lowest published version is exactly
// 0.0.0.
func (u *packageUniverse) ensureExtendedProbes() {
	if u.extProbesReady {
		return
	}
	var highMajor uint64
	if len(u.versions) > 0 {
		highMajor = u.versions[0].sv().Major()
	}
	u.extAbove = mustNewVersion(fmt.Sprintf("%d.0.0", highMajor+1))

	lowIsZero := true
	if len(u.versions) > 0 {
		low := u.versions[len(u.versions)-1].sv()
		lowIsZero = low.Major() == 0 && low.Minor() == 0 && low.Patch() == 0 && low.Prerelease() == ""
	}
	if lowIsZero {
		u.extBelow = mustNewVersion("0.0.0-0")
	} else {
		u.extBelow = mustNewVersion("0.0.0")
	}
	u.extProbesReady = true
}

// embedPublishedIntoExtended lifts a published-universe bitset into the
// extended cell layout by aligning each published index i to point cell
// i+1, leaving both boundary cells unset: an already-materialized
// published-only term (the no-versions leaf is the only producer of one)
// carries no boundary information of its own.
func embedPublishedIntoExtended(published []uint64, extLen int) []uint64 {
	out := newBits(extLen)
	m := extLen - boundaryCells
	for i := range m {
		if testBit(published, i) {
			setBit(out, i+1)
		}
	}
	return out
}

// classifyIntoExtended sets, in the caller-provided bits (of extended
// length n), every cell of u's boundary-extended universe whose probe
// version set contains: the below-boundary cell, each published version's
// cell, and the above-boundary cell.
func (u *packageUniverse) classifyIntoExtended(set versionSet, bits []uint64, n int) {
	if set.Contains(u.extBelow) {
		setBit(bits, 0)
	}
	for i, v := range u.versions {
		if set.Contains(v) {
			setBit(bits, i+1)
		}
	}
	if set.Contains(u.extAbove) {
		setBit(bits, n-1)
	}
}

// materializeExtendedSet returns set's classification against u's
// boundary-extended universe, computing and caching a fresh one for a
// not-yet-seen symbolic key (in a cache separate from the published one).
// A published bitset embeds via embedPublishedIntoExtended; an
// already-extended bitset (produced by conflict resolution's own merge or
// difference arithmetic) is returned as-is.
func (u *packageUniverse) materializeExtendedSet(set versionSet) []uint64 {
	if set.kind == setExtBitset {
		return set.bits
	}
	n := u.extendedLen()
	if set.kind == setBitset {
		return embedPublishedIntoExtended(set.bits, n)
	}
	if bits, ok := u.extClassCache[set.key]; ok {
		return bits
	}
	u.ensureExtendedProbes()
	bits := newBits(n)
	if set.isAny() {
		for i := range n {
			setBit(bits, i)
		}
	} else {
		u.classifyIntoExtended(set, bits, n)
	}
	// A constraint that matches every published version and both boundary
	// probes classifies to the full extended universe, which makes it
	// invisible in a package's running intersection (full is the identity of
	// intersection). Its requiring dependency's attribution is then lost when
	// the package is later emptied by an exclusion, and a transitively
	// unsatisfiable graph is falsely rejected. Retract the two boundary cells
	// for such a vacuously-true constraint (an unconstrained "*", or a "!=X"
	// whose X is unpublished) so it constrains the running to its published
	// point cells and stays a resolvable contributor.
	if popcount(bits) == n {
		clearBit(bits, 0)
		clearBit(bits, n-1)
	}
	u.extClassCache[set.key] = bits
	return bits
}

// asExtBitsetSet wraps bits materialized against u's extended universe into
// a display-carrying versionSet, for conflict resolution's merge and
// difference results.
func (u *packageUniverse) asExtBitsetSet(bits []uint64, display string) versionSet {
	return versionSet{kind: setExtBitset, bits: bits, universeN: u.extendedLen(), display: display}
}

// rawExtBits is materializeExtendedSet's free-function mirror of rawBits,
// for symmetry with the published-universe helpers below it.
func rawExtBits(set versionSet, uni *packageUniverse) []uint64 {
	return uni.materializeExtendedSet(set)
}

// permittedExtBits is permittedBits' extended-universe counterpart: the
// bitset of extended cells t actually permits.
func permittedExtBits(t term, uni *packageUniverse) []uint64 {
	raw := rawExtBits(t.Set, uni)
	if t.Positive {
		return raw
	}
	return complementNew(raw, uni.extendedLen())
}

// fullExtBits returns a bitset with every one of uni's extended cells set -
// the vacuous "no constraint yet" starting point for conflict resolution's
// own running intersections, mirroring fullBits for the published universe.
func fullExtBits(uni *packageUniverse) []uint64 {
	return fullBits(uni.extendedLen())
}

// intersectExtAssignmentInto folds t's contribution into running,
// intersecting over uni's boundary-extended universe (a positive term
// intersects with its raw cells, a negative term clears them, so no
// complement is allocated). Two consumers drive it, not one: conflict resolution's
// satisfier search, which folds assignments into a throwaway running
// intersection while it walks an incompatibility, and the partial solution's
// own live running intersections, which it seeds when a package is first
// materialized, extends as each further assignment is recorded, and replays
// from scratch when a backtrack rebuilds them.
func intersectExtAssignmentInto(running []uint64, t term, uni *packageUniverse) {
	raw := rawExtBits(t.Set, uni)
	if t.Positive {
		for i := range running {
			running[i] &= raw[i]
		}
		return
	}
	for i := range running {
		running[i] &^= raw[i]
	}
}

// subsetOfPermittedExt reports whether i (an extended-universe intersection
// bitset) is a subset of the cells t permits, without allocating a
// complement: for a positive term this is i ⊆ raw; for a negative term this
// is disjointness with raw (i ⊆ complement(raw) iff i ∩ raw = ∅).
func subsetOfPermittedExt(i []uint64, t term, uni *packageUniverse) bool {
	raw := rawExtBits(t.Set, uni)
	if t.Positive {
		return subset(i, raw)
	}
	return disjointBits(i, raw)
}

// disjointFromPermittedExt reports whether i (an extended-universe
// intersection bitset) shares no cell with the cells t permits: for a
// positive term this is disjointness with raw; for a negative term this is i
// ⊆ raw (i ∩ complement(raw) = ∅ iff i ⊆ raw).
func disjointFromPermittedExt(i []uint64, t term, uni *packageUniverse) bool {
	raw := rawExtBits(t.Set, uni)
	if t.Positive {
		return disjointBits(i, raw)
	}
	return subset(i, raw)
}

// pointCellsOf projects an extended-universe bitset down to published-only
// point cells (dropping both boundary cells), for decision making's own use:
// a decision can only ever select a published version, so every candidate
// count, exact-pin check, and allowed-version pick reads this projection
// rather than the raw extended running intersection.
func pointCellsOf(ext []uint64, uni *packageUniverse) []uint64 {
	m := len(uni.versions)
	out := newBits(m)
	for i := range m {
		if testBit(ext, i+1) {
			setBit(out, i)
		}
	}
	return out
}

// projectTermToPublished collapses t - if and only if its Set is an
// extended-universe bitset (the only kind conflict resolution's own merge
// and difference arithmetic ever produces) - down to a published-universe
// term: a POSITIVE term whose bits are the point-cell portion of t's
// PERMITTED set (permittedExtBits, which already folds in t's own polarity
// via complementing when t is negative). Projecting the raw, still-signed
// bits instead of the permitted ones would be wrong: a negative extended
// term can legitimately carry an empty point-cell portion in its raw bits
// while its actual permitted set (the complement) is a small, perfectly
// meaningful non-empty published set - collapsing the unsigned raw bits
// would silently discard that and reintroduce the very
// published-universe-tautology-over-a-tiny-package problem the extended
// universe exists to avoid. Always re-emitting a positive term keeps this
// consistent with every other producer of a merged term (normalizeTerms,
// which already computes and stores a positive intersection of permitted
// sets). This projection is always exactly Check-correct for anything
// downstream of conflict resolution (relation's Case C, error reporting): a
// published running intersection is built entirely from published versions
// and can never overlap a boundary cell, so the two discarded boundary
// bits are pure noise there regardless of their value. Symbolic and
// already-published-bitset terms pass through unchanged - the common case,
// since most incompatibilities conflict resolution touches were never
// rewritten by a merge.
func projectTermToPublished(t term, uni *packageUniverse) term {
	if t.Set.kind != setExtBitset {
		return t
	}
	permitted := permittedExtBits(t, uni)
	m := len(uni.versions)
	bits := newBits(m)
	for i := range m {
		if testBit(permitted, i+1) {
			setBit(bits, i)
		}
	}
	return term{Package: t.Package, Set: uni.asBitsetSet(bits, t.Set.display), Positive: true}
}

// term is a statement about a package that may be true or false for a given
// selection of versions: "Package in Set" (Positive) or "not (Package in
// Set)" (!Positive).
type term struct {
	Package  string
	Set      versionSet
	Positive bool
}

// Negate returns the logical negation of t. It only flips Positive; the
// underlying set is never touched (complementing a set is a distinct bitset
// operation used only inside conflict resolution).
func (t term) Negate() term {
	return term{Package: t.Package, Set: t.Set, Positive: !t.Positive}
}

// sameVersionSet reports whether a and b denote the same identity: symbolic
// sets compare by Key, bitset sets compare word-for-word. Mixed-kind
// comparisons never legitimately arise (a package's assignments and
// incompatibility terms are homogeneously symbolic before materialization
// and homogeneously bitset afterward), so they are conservatively unequal.
func sameVersionSet(a, b versionSet) bool {
	if a.kind != b.kind {
		return false
	}
	if a.kind == setSymbolic {
		return a.key == b.key
	}
	return bitsEqual(a.bits, b.bits)
}

// sameTerm reports whether two terms are content-identical: same package,
// same polarity, same set identity.
func sameTerm(a, b term) bool {
	return a.Package == b.Package && a.Positive == b.Positive && sameVersionSet(a.Set, b.Set)
}

// compareVersionsDescending implements the universe total order: semver
// precedence descending, tie-broken by the original string descending
// (byte-wise). This is stricter than a single-level precedence comparator
// (which is not a total order for strings of equal precedence, e.g.
// "1.0.0" vs "1.0.0+build") and is what makes "the highest version" and
// "index 0" unambiguous.
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
