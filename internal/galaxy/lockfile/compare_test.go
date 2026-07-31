package lockfile

// This file covers Compare and the two types built around it, Diff and
// Change: the central Empty()-agrees-with-Hash invariant
// (TestCompareEmptyMatchesHashEquality), that a deps-only difference still
// counts as Updated (TestCompareDepsOnlyDifferenceIsUpdated), that neither
// collection order nor dep order matters and Compare never mutates its
// arguments (TestCompareIgnoresOrderAndDoesNotMutate), the nil-baseline
// contract (TestCompareNilBaselineIsAllAdded), Change.Fields' fixed field
// order and rendering (TestCompareFieldsReportsEveryChangedField), that
// duplicate dependencies are multiset-significant, not merely set-significant
// (TestCompareDuplicateDepsAreSignificant), and that an adversarial entry -
// a path-traversal name, control bytes, an oversized deps element - passes
// through unmutated and unescaped rather than panicking or being sanitized
// at this layer (TestCompareRendersHostileEntryVerbatim).

import (
	"strings"
	"testing"
)

// Fixed test digests, 64 hex characters like a real sha256 without needing
// to actually be valid hex (Compare never validates digest shape).
const (
	sha1 = "11111111111111111111111111111111111111111111111111111111111111aa"
	sha2 = "22222222222222222222222222222222222222222222222222222222222222bb"
)

// emptyMatchesHashEqualityCase is one row of
// TestCompareEmptyMatchesHashEquality's table.
type emptyMatchesHashEqualityCase struct {
	before    *File
	after     *File
	name      string
	wantEmpty bool
}

// eqWidgets and eqLegacy are the two entries every row in
// TestCompareEmptyMatchesHashEquality's table starts from, and eqFile builds
// a *File from them. Promoted to package level, rather than closures inside
// one table-building function, so no single function trips the funlen
// budget - the row data is what makes this table long, not any one
// function's own logic.
func eqWidgets() Entry {
	return Entry{
		Name: "acme.widgets", Version: "1.0.0", Source: "https://galaxy.example",
		SHA256: sha1, Deps: []string{"acme.dep1", "acme.dep2"},
	}
}

func eqLegacy() Entry {
	return Entry{Name: "acme.legacy", Version: "2.0.0", Source: "https://galaxy.example", SHA256: sha2}
}

func eqFile(server string, entries ...Entry) *File {
	return &File{SchemaVersion: SchemaVersion, Server: server, Collections: entries}
}

func withVersion(e Entry, v string) Entry { e.Version = v; return e }
func withSource(e Entry, s string) Entry  { e.Source = s; return e }
func withSHA(e Entry, s string) Entry     { e.SHA256 = s; return e }
func withDeps(e Entry, d []string) Entry  { e.Deps = d; return e }

// samePayloadCases is TestCompareEmptyMatchesHashEquality's own positive
// control: three rows the fixture must report Empty()==true for - identical,
// reordered collections, and reordered deps - proving it is capable of that
// outcome and not just of refusing.
func samePayloadCases() []emptyMatchesHashEqualityCase {
	return []emptyMatchesHashEqualityCase{
		{
			name:      "identical",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			wantEmpty: true,
		},
		{
			name:      "reordered_collections",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", eqLegacy(), eqWidgets()),
			wantEmpty: true,
		},
		{
			name:      "reordered_deps",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withDeps(eqWidgets(), []string{"acme.dep2", "acme.dep1"}), eqLegacy()),
			wantEmpty: true,
		},
	}
}

// fieldChangeCases covers a single-field change to the acme.widgets entry -
// version, source, sha256, or deps (changed or dropped entirely) - each
// against an otherwise-identical baseline, and each must report
// Empty()==false.
func fieldChangeCases() []emptyMatchesHashEqualityCase {
	return []emptyMatchesHashEqualityCase{
		{
			name:      "version_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withVersion(eqWidgets(), "1.1.0"), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "source_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withSource(eqWidgets(), "https://other.example"), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "sha_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withSHA(eqWidgets(), sha2), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "deps_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withDeps(eqWidgets(), []string{"acme.dep1", "acme.dep3"}), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "deps_dropped",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withDeps(eqWidgets(), nil), eqLegacy()),
			wantEmpty: false,
		},
	}
}

// structuralChangeCases covers a change in what the file names, not just in
// one entry's pinned fields: an added or removed collection, or a changed or
// cleared file-level Server. Each must report Empty()==false.
func structuralChangeCases() []emptyMatchesHashEqualityCase {
	return []emptyMatchesHashEqualityCase{
		{
			name:   "entry_added",
			before: eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after: eqFile("https://galaxy.example", eqWidgets(), eqLegacy(),
				Entry{Name: "acme.extra", Version: "1.0.0", Source: "https://galaxy.example"}),
			wantEmpty: false,
		},
		{
			name:      "entry_removed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", eqWidgets()),
			wantEmpty: false,
		},
		{
			name:      "server_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://other-server.example", eqWidgets(), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "server_cleared",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("", eqWidgets(), eqLegacy()),
			wantEmpty: false,
		},
	}
}

// emptyMatchesHashEqualityCases concatenates the 12-row table
// TestCompareEmptyMatchesHashEquality checks, from its three topic-grouped
// halves.
func emptyMatchesHashEqualityCases() []emptyMatchesHashEqualityCase {
	cases := samePayloadCases()
	cases = append(cases, fieldChangeCases()...)
	cases = append(cases, structuralChangeCases()...)
	return cases
}

// TestCompareEmptyMatchesHashEquality is the 12-row table proving Compare's
// central invariant: for two non-nil *File sharing SchemaVersion,
// Compare(before, after).Empty() agrees with before.Hash() == after.Hash().
// Three rows (identical, reordered collections, reordered deps) are this
// table's own positive control, asserting Empty()==true alongside the nine
// rows asserting a real difference, so the fixture is shown capable of both
// outcomes rather than only ever refusing.
//
// Two mutations were run against this test, both confirmed to fail it. Both
// are caught by the same assertion - the hash-equality invariant, checked
// ahead of the row's own wantEmpty - because both mutations break that exact
// invariant rather than merely mis-classifying one row:
//
//   - Dropping sameDeps from sameEntry (comparing only Version/Source/SHA256)
//     makes deps_changed and deps_dropped report Empty()==true while Hash
//     disagrees - go test -run TestCompareEmptyMatchesHashEquality/deps_changed
//     -v fails with:
//     compare_test.go:201: Compare.Empty()=true but hash equality=false; diff={Server:<nil> Added:[] Updated:[] Removed:[]}
//   - Dropping the file-level Server comparison from Compare makes
//     server_changed and server_cleared report Empty()==true while Hash
//     disagrees - go test -run TestCompareEmptyMatchesHashEquality/server_changed
//     -v fails with the identical assertion, same line:
//     compare_test.go:201: Compare.Empty()=true but hash equality=false; diff={Server:<nil> Added:[] Updated:[] Removed:[]}
func TestCompareEmptyMatchesHashEquality(t *testing.T) {
	t.Parallel()
	for _, tc := range emptyMatchesHashEqualityCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkEmptyMatchesHashEquality(t, tc)
		})
	}
}

// checkEmptyMatchesHashEquality runs one row of
// TestCompareEmptyMatchesHashEquality's table: the hash-equality invariant is
// checked ahead of the row's own wantEmpty, since that invariant is what
// Compare exists to uphold, and checking it first is what lets a mutation
// that breaks only the invariant (dropping the Server comparison, in
// particular) surface its own message rather than being masked by the
// separate wantEmpty check below it.
//
// The second check, against wantEmpty, cannot catch such a mutation itself:
// every row's wantEmpty was chosen to agree with Hash, so for every row in
// this table the two checks are equivalent, and no mutation of Compare can
// pass the first while failing the second. What the second check actually
// guards is the table's own rows - it fires when a future row's declared
// wantEmpty disagrees with Hash, which is a bad fixture, not a Compare
// regression. A real Compare regression always surfaces through the
// invariant check above it.
func checkEmptyMatchesHashEquality(t *testing.T, tc emptyMatchesHashEqualityCase) {
	t.Helper()
	diff := Compare(tc.before, tc.after)
	beforeHash, err := tc.before.Hash()
	if err != nil {
		t.Fatalf("before.Hash: %v", err)
	}
	afterHash, err := tc.after.Hash()
	if err != nil {
		t.Fatalf("after.Hash: %v", err)
	}
	if diff.Empty() != (beforeHash == afterHash) {
		t.Fatalf("Compare.Empty()=%v but hash equality=%v; diff=%+v", diff.Empty(), beforeHash == afterHash, diff)
	}
	if diff.Empty() != tc.wantEmpty {
		t.Fatalf("Compare(...).Empty() = %v, want %v; diff=%+v", diff.Empty(), tc.wantEmpty, diff)
	}
}

// TestCompareDepsOnlyDifferenceIsUpdated proves a deps-only difference -
// every pin field identical, only Deps differing - is reported as Updated,
// not silently treated as no change. Deps is load-bearing for a --frozen
// install (materializeLockfile -> lockfileDepsToKeys rebuilds the install
// graph from it), so it must count as a real difference even though it pins
// nothing about the artifact itself.
//
// Mutation: dropping sameDeps from sameEntry (comparing only
// Version/Source/SHA256) makes this fail with:
//
//	compare_test.go:263: deps-only difference reported as no change: diff={Server:<nil> Added:[] Updated:[] Removed:[]}
func TestCompareDepsOnlyDifferenceIsUpdated(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://galaxy.example", SHA256: sha1, Deps: []string{"acme.dep1"}},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://galaxy.example", SHA256: sha1, Deps: []string{"acme.dep1", "acme.dep2"}},
	}}

	diff := Compare(before, after)
	if len(diff.Updated) != 1 {
		t.Fatalf("deps-only difference reported as no change: diff=%+v", diff)
	}
	fields := diff.Updated[0].Fields()
	if len(fields) != 1 || fields[0].Field != fieldDeps {
		t.Fatalf("Fields() = %+v, want exactly one deps field change", fields)
	}
}

// TestCompareIgnoresOrderAndDoesNotMutate proves two independently pinnable
// properties on one fixture that differs from a reordered-but-otherwise-
// identical counterpart only in collection order and dep order: (1) Compare
// reports no difference, and (2) Compare never mutates either input's order
// while computing that verdict. Both are reachable from the same passing
// fixture, so neither assertion is documentary.
//
// Mutation 1, dropping the sorted-clone fallback from sameDeps (leaving only
// the length check and slices.Equal fast path), fails assertion (1) with:
//
//	compare_test.go:311: reordered but identical files reported as changed:
//	diff={Server:<nil> Added:[] Updated:[{From:{acme.widgets 1.0.0 ...
//
// Mutation 2, adding canonicalize(before)/canonicalize(after) at the top of
// Compare (an attempt to "simplify" order-insensitivity by sorting the inputs
// instead of comparing order-insensitively), makes assertion (1) PASS - the
// now-identically-sorted inputs compare equal - while failing assertion (2),
// since canonicalize sorts its argument's Collections and each entry's Deps
// in place:
//
//	compare_test.go:315: Compare mutated before: collections order changed from [acme.widgets acme.legacy] to [acme.legacy acme.widgets]
//
// which is exactly what the chain-pinnability rule requires: a mutation that
// makes an earlier assertion pass while a later one in the same test still
// fails.
func TestCompareIgnoresOrderAndDoesNotMutate(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Deps: []string{"acme.dep1", "acme.dep2"}},
		{Name: "acme.legacy", Version: "2.0.0"},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.legacy", Version: "2.0.0"},
		{Name: "acme.widgets", Version: "1.0.0", Deps: []string{"acme.dep2", "acme.dep1"}},
	}}
	beforeNames, beforeDeps := collectionNames(before), collectionDeps(before)
	afterNames, afterDeps := collectionNames(after), collectionDeps(after)

	diff := Compare(before, after)
	if !diff.Empty() { // assertion (1)
		t.Fatalf("reordered but identical files reported as changed: diff=%+v", diff)
	}

	if got := collectionNames(before); !equalStrings(got, beforeNames) { // assertion (2)
		t.Fatalf("Compare mutated before: collections order changed from %v to %v", beforeNames, got)
	}
	if got := collectionNames(after); !equalStrings(got, afterNames) {
		t.Fatalf("Compare mutated after: collections order changed from %v to %v", afterNames, got)
	}
	if got := collectionDeps(before); !equalDeps(got, beforeDeps) {
		t.Fatalf("Compare mutated before: deps order changed from %v to %v", beforeDeps, got)
	}
	if got := collectionDeps(after); !equalDeps(got, afterDeps) {
		t.Fatalf("Compare mutated after: deps order changed from %v to %v", afterDeps, got)
	}
}

// TestCompareNilBaselineIsAllAdded proves Compare(nil, f) reports every entry
// as Added, sorted by name; Compare(nil, nil) is empty; and Compare(f, nil)
// reports every entry as Removed, also sorted by name. f is built in reverse
// name order specifically so a passing sorted-order assertion actually pins
// the sort in Compare rather than coincidentally matching Collections' own
// declaration order.
func TestCompareNilBaselineIsAllAdded(t *testing.T) {
	t.Parallel()
	f := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.zebra", Version: "1.0.0"},
		{Name: "acme.mid", Version: "1.0.0"},
		{Name: "acme.alpha", Version: "1.0.0"},
	}}
	wantNames := []string{"acme.alpha", "acme.mid", "acme.zebra"}

	added := Compare(nil, f)
	assertSortedEntryNames(t, added.Added, wantNames, "Compare(nil, f).Added")
	if len(added.Updated) != 0 || len(added.Removed) != 0 || added.Server != nil {
		t.Fatalf("Compare(nil, f) reported more than Added: %+v", added)
	}

	if empty := Compare(nil, nil); !empty.Empty() {
		t.Fatalf("Compare(nil, nil) = %+v, want empty", empty)
	}

	removed := Compare(f, nil)
	assertSortedEntryNames(t, removed.Removed, wantNames, "Compare(f, nil).Removed")
}

// assertSortedEntryNames fails the test unless got's entries are named want,
// in order - the one assertion shape TestCompareNilBaselineIsAllAdded applies
// to both Compare(nil, f).Added and Compare(f, nil).Removed, so the sorted-
// order check is written and pinned once rather than duplicated per call.
func assertSortedEntryNames(t *testing.T, got []Entry, want []string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %+v, want %d entries", label, got, len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("%s[%d].Name = %q, want %q (sorted by name)", label, i, got[i].Name, name)
		}
	}
}

// TestCompareFieldsReportsEveryChangedField changes all four per-entry fields
// at once (including clearing Deps entirely) and asserts Fields() reports
// all four, in the fixed version/source/sha256/deps order, each carrying its
// real from/to value - including the deps-cleared case rendering To as the
// empty string, which internal/galaxy/collections's quoteEmpty turns into
// "(none)" for display; Fields() itself does no such substitution.
func TestCompareFieldsReportsEveryChangedField(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{
			Name: "acme.widgets", Version: "1.0.0", Source: "https://old.example",
			SHA256: sha1, Deps: []string{"acme.dep1", "acme.dep2"},
		},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.1.0", Source: "https://new.example", SHA256: sha2, Deps: nil},
	}}

	diff := Compare(before, after)
	if len(diff.Updated) != 1 {
		t.Fatalf("expected exactly one updated entry, got %+v", diff.Updated)
	}
	got := diff.Updated[0].Fields()
	want := []FieldChange{
		{Field: fieldVersion, From: "1.0.0", To: "1.1.0"},
		{Field: fieldSource, From: "https://old.example", To: "https://new.example"},
		{Field: fieldSHA256, From: sha1, To: sha2},
		{Field: fieldDeps, From: "acme.dep1,acme.dep2", To: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("Fields() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Fields()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestCompareDuplicateDepsAreSignificant pins that sameDeps is a multiset
// comparison, not a set comparison: [x.x, x.x] and [x.x, y.y] both have
// length 2 and share an element, but are not the same dependency list, and
// Hash (which sorts without deduplicating) agrees they are different files.
//
// Mutation: making sameEntry ignore Deps entirely (comparing only
// Version/Source/SHA256, which are identical here) fails with:
//
//	compare_test.go:432: duplicate-vs-distinct deps must differ: diff={Server:<nil> Added:[] Updated:[] Removed:[]}
func TestCompareDuplicateDepsAreSignificant(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Deps: []string{"x.x", "x.x"}},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Deps: []string{"x.x", "y.y"}},
	}}

	diff := Compare(before, after)
	if len(diff.Updated) != 1 {
		t.Fatalf("duplicate-vs-distinct deps must differ: diff=%+v", diff)
	}

	beforeHash, err := before.Hash()
	if err != nil {
		t.Fatalf("before.Hash: %v", err)
	}
	afterHash, err := after.Hash()
	if err != nil {
		t.Fatalf("after.Hash: %v", err)
	}
	if beforeHash == afterHash {
		t.Fatalf("Hash agrees that [x.x, x.x] and [x.x, y.y] are the same file, hash=%q", beforeHash)
	}
}

// hostileEntryName and hostileEntrySource are shared by
// TestCompareRendersHostileEntryVerbatim and its mutation-focused sibling
// TestCompareRendersHostileEntryVerbatimDoesNotMutate: a path-traversal name
// and a Source embedding a NUL byte, an ANSI escape, and a CRLF.
const (
	hostileEntryName   = "../../../../etc/passwd"
	hostileEntrySource = "https://x.example\x00\x1b[31m\r\nInstalled: totally.fine"
)

// TestCompareRendersHostileEntryVerbatim proves Compare, Change.Fields, and
// renderDeps treat an adversarial Entry exactly like any other one: no
// panic, no error, and the hostile values pass through into
// FieldChange.From/To exactly as given - neither escaped, sanitized, nor
// mutated. That guarantee is not a runtime check this test itself performs;
// it is a structural property of this package's import list, which this
// test depends on rather than proves: compare.go imports only slices, sort,
// and strings (see its own import block), so nothing in this package can
// reach the filesystem or spawn a process in the first place, regardless of
// what an Entry's fields contain. The companion property - that Compare
// never mutates either input - is TestCompareRendersHostileEntryVerbatimDoesNotMutate,
// split into its own function to keep this one's cyclomatic complexity
// within budget.
//
// The hostile entry, present under the identical name on both sides so
// Compare reports it as Updated, carries hostileEntryName, hostileEntrySource,
// and an oversized Deps element - one row exercising all three surfaces
// this package's rendering touches: Entry fields themselves, FieldChange.
// From/To, and renderDeps' comma-join. A second, benign acme.widgets entry
// shares the same table as this test's positive control: asserting
// diff.Updated has exactly one entry, and that its Name is the hostile one,
// already proves the benign entry was correctly left out of Updated - a
// separate loop over diff.Updated to check for it would only re-assert what
// the count and name checks already establish.
func TestCompareRendersHostileEntryVerbatim(t *testing.T) {
	t.Parallel()
	oversizedDep := strings.Repeat("d", 10000)

	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: hostileEntryName, Version: "1.0.0", Source: "https://safe.example", SHA256: sha1, Deps: []string{"a.a"}},
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://safe.example", SHA256: sha1},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: hostileEntryName, Version: "1.0.0", Source: hostileEntrySource, SHA256: sha2, Deps: []string{oversizedDep}},
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://safe.example", SHA256: sha1},
	}}

	diff := Compare(before, after)

	if len(diff.Added) != 0 || len(diff.Removed) != 0 {
		t.Fatalf("expected no Added/Removed, got diff=%+v", diff)
	}
	if len(diff.Updated) != 1 {
		t.Fatalf("expected exactly one updated entry (the hostile one; acme.widgets is unchanged), got %+v", diff.Updated)
	}
	assertHostileFieldsRendered(t, diff.Updated[0], oversizedDep)
}

// assertHostileFieldsRendered checks that change - the hostile entry's own
// Updated pair - carries its name unmangled and its three changed fields
// (source, sha256, deps) exactly as given, in Fields()'s fixed order.
// Factored out of TestCompareRendersHostileEntryVerbatim to keep that
// function's own cyclomatic complexity within budget.
func assertHostileFieldsRendered(t *testing.T, change Change, oversizedDep string) {
	t.Helper()
	if change.To.Name != hostileEntryName {
		t.Fatalf("Updated[0].To.Name = %q, want the hostile name unmangled", change.To.Name)
	}
	fields := change.Fields()
	want := []FieldChange{
		{Field: fieldSource, From: "https://safe.example", To: hostileEntrySource},
		{Field: fieldSHA256, From: sha1, To: sha2},
		{Field: fieldDeps, From: "a.a", To: oversizedDep},
	}
	if len(fields) != len(want) {
		t.Fatalf("Fields() = %+v, want %+v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("Fields()[%d] = %+v, want %+v", i, fields[i], want[i])
		}
	}
}

// TestCompareRendersHostileEntryVerbatimDoesNotMutate is
// TestCompareRendersHostileEntryVerbatim's mutation-focused sibling: Compare
// must leave both input files' own Collections order and each entry's Deps
// order untouched. The fixture mirrors TestCompareIgnoresOrderAndDoesNotMutate's
// own two-collection, multi-element-Deps shape rather than the hostile entry
// alone: a single collection with a single-element Deps makes an in-place
// canonicalize mutation structurally unobservable, since sorting one
// collection - or one dep - is a no-op regardless of what the sort does, so a
// non-mutation assertion against that fixture can never fail no matter what
// Compare actually does to it.
//
// acme.widgets carries the two-element, non-sorted Deps ["z.z", "a.a"], and
// is placed BEFORE hostileEntryName in Collections - the reverse of
// canonical sorted order, since "../../../../etc/passwd" sorts ahead of
// "acme.widgets" ('.' is 0x2E, 'a' is 0x61) - so an in-place Collections sort
// would swap them.
//
// Killing mutation: adding canonicalize(before)/canonicalize(after) at the
// top of Compare - the same mutation TestCompareIgnoresOrderAndDoesNotMutate
// itself is killed by - fails this test with:
//
//	compare_test.go:597: Compare mutated before: names changed from
//	[acme.widgets ../../../../etc/passwd] to [../../../../etc/passwd acme.widgets]
//
// That is the first of the four checks below, so it is the only one a
// single run of this exact mutation can observe failing - the deps-order
// checks after it are documentary under this specific mutation: canonicalize
// sorts Collections and every entry's Deps together in one pass, and the
// collection-order check's own t.Fatalf halts the test before either deps
// check is reached. The collection-order check above is what this mutation
// actually pins; the acme.widgets Deps are still non-sorted-order on
// purpose, matching TestCompareIgnoresOrderAndDoesNotMutate's own fixture
// shape, but no mutation isolating just the deps-sort has been run against
// this test.
//
// A more surgical mutation was run instead: dropping the defensive
// slices.Clone from sameDeps's sorted-clone fallback (see sameDeps itself for
// why that clone exists). It is caught, but not here: acme.widgets's deps are
// bit-identical on both sides of this fixture, so sameDeps's slices.Equal
// fast path returns before the sort is ever reached, and hostileEntryName's
// own Deps are single-element, where sorting is a no-op regardless of the
// mutation. It is caught instead by TestCompareIgnoresOrderAndDoesNotMutate's
// own fixture, whose acme.widgets entry carries a genuinely reordered
// two-element Deps on each side, which reaches the sort this mutation
// removes the safety of. That leaves this test's own deps-order coverage
// documentary but not unguarded: the property it does not itself pin is
// pinned by its sibling.
func TestCompareRendersHostileEntryVerbatimDoesNotMutate(t *testing.T) {
	t.Parallel()
	oversizedDep := strings.Repeat("d", 10000)
	unsortedDeps := []string{"z.z", "a.a"}

	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://safe.example", SHA256: sha1, Deps: append([]string(nil), unsortedDeps...)},
		{Name: hostileEntryName, Version: "1.0.0", Source: "https://safe.example", SHA256: sha1},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://safe.example", SHA256: sha1, Deps: append([]string(nil), unsortedDeps...)},
		{Name: hostileEntryName, Version: "1.0.0", Source: hostileEntrySource, SHA256: sha2, Deps: []string{oversizedDep}},
	}}
	beforeNames, beforeDeps := collectionNames(before), collectionDeps(before)
	afterNames, afterDeps := collectionNames(after), collectionDeps(after)

	_ = Compare(before, after)

	if got := collectionNames(before); !equalStrings(got, beforeNames) {
		t.Fatalf("Compare mutated before: names changed from %v to %v", beforeNames, got)
	}
	if got := collectionNames(after); !equalStrings(got, afterNames) {
		t.Fatalf("Compare mutated after: names changed from %v to %v", afterNames, got)
	}
	if got := collectionDeps(before); !equalDeps(got, beforeDeps) {
		t.Fatalf("Compare mutated before: deps changed from %v to %v", beforeDeps, got)
	}
	if got := collectionDeps(after); !equalDeps(got, afterDeps) {
		t.Fatalf("Compare mutated after: deps changed from %v to %v", afterDeps, got)
	}
}
