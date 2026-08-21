package lockfile

import (
	"slices"
	"strings"
)

// Field-name constants for FieldChange.Field. Fixed strings rather than an
// enum, since FieldChange is meant to be rendered directly by a caller (see
// internal/galaxy/collections/lock.go's renderFieldChanges).
const (
	fieldVersion = "version"
	fieldSource  = "source"
	fieldType    = "type"
	fieldRef     = "ref"
	fieldCommit  = "commit"
	fieldSubdir  = "subdir"
	fieldSHA256  = "sha256"
	fieldDeps    = "deps"
	fieldServer  = "server"
	// fieldGalaxy and fieldRepository belong to a role entry alone.
	fieldGalaxy     = "galaxy"
	fieldRepository = "repository"
)

// comparedFieldCount is the number of per-entry fields Change.Fields() can
// report (version, source, type, ref, commit, subdir, sha256, deps) - the
// server field lives at the file level, on Diff.Server, rather than per
// entry. Used only to pre-size Fields()'s return slice.
const comparedFieldCount = 8

// Diff is what Compare(before, after) found: which collections after would
// add, update, or remove relative to before, plus whether the file-level
// Server field itself changed.
type Diff struct {
	// Server is set only when before and after are both non-nil and their
	// Server fields differ; nil means the file-level server field is
	// unchanged (including when either input is nil, since there is then no
	// pair of file-level values to compare).
	Server  *FieldChange
	Added   []Entry
	Updated []Change
	Removed []Entry
	// RolesAdded, RolesUpdated and RolesRemoved are the roles list's half of
	// the diff, keyed by install name exactly as the collections are keyed
	// by fqdn.
	RolesAdded   []RoleEntry
	RolesUpdated []RoleChange
	RolesRemoved []RoleEntry
}

// Change is one collection present in both before and after whose pinned
// fields differ. From and To carry the whole entry, not just the differing
// fields, so a caller needing a value Compare did not itself compare (an
// unchanged field, for instance) still has it. Use Fields to see which
// fields actually differ.
type Change struct {
	From Entry
	To   Entry
}

// FieldChange names one field that differs between two values it was
// derived from, and what it changed from/to. Field is one of "version",
// "source", "type", "ref", "commit", "subdir", "sha256", "deps" (per-entry,
// via Change.Fields), or "server" (file-level, via Diff.Server).
type FieldChange struct {
	Field string
	From  string
	To    string
}

// Compare reports how after differs from before, at the granularity of one
// collection name each: a name only in after is Added, only in before is
// Removed, and in both but with a differing pinned field is Updated. It is a
// total function, not an error-returning one, and it never mutates or
// canonicalizes either argument.
//
// Compare(nil, x) reports every collection in x as Added; Compare(x, nil)
// reports every collection in x as Removed; Compare(nil, nil) is empty
// (Diff.Empty() == true). A nil before therefore reads as "no collection
// existed before", which is also what an up-to-date empty file would report:
// Compare(nil, emptyFile).Empty() is true, so a caller that must distinguish
// "no lockfile exists yet" from "the lockfile is already empty and up to
// date" has to make that distinction itself before calling Compare with a
// nil before - Compare cannot recover it from a nil argument alone.
//
// Load returns a file's collections in on-disk order, while Save and Hash
// canonicalize (sort by name, sort each entry's Deps) before writing or
// hashing. Compare requires neither: entries are keyed by name here, so
// collection order never matters, and sameDeps (see Change.Fields) makes Deps
// order not matter either, while still catching a duplicated dependency -
// requiring canonical input would report a merely reordered file as changed,
// which is not the question Compare answers.
//
// A duplicate collection name within one file resolves last-entry-wins,
// matching indexLockfile's own map build in internal/galaxy/collections. This
// is reachable only through a hand-built *File: Load rejects duplicate names
// outright, and buildLockfile derives its entries from an fqdn-keyed map, so
// neither producer can hand Compare a file with one.
//
// For two non-nil *File values sharing the same SchemaVersion, Compare(a,
// b).Empty() is true exactly when a.Hash() == b.Hash(): Updated already
// covers every per-entry field Hash covers, including Deps, and Server is
// carried on Diff for the identical reason - without it, a diff could read
// empty for a file Hash (and therefore a real `lock` run) would still
// consider different. SchemaVersion itself is deliberately not part of the
// diff: Load rejects a mismatched schema before Compare ever sees one, so a
// field for it would have no producer to report.
func Compare(before, after *File) Diff {
	beforeIdx := indexByName(before)
	afterIdx := indexByName(after)
	diff := diffIndexes(beforeIdx, afterIdx)
	diff.Server = serverFieldChange(before, after)
	diff.RolesAdded, diff.RolesUpdated, diff.RolesRemoved = diffRoles(indexRolesByName(before), indexRolesByName(after))
	return diff
}

// diffIndexes builds Added/Updated/Removed from beforeIdx and afterIdx in one
// pass over their sorted name union, factored out of Compare so each of the
// two halves of Compare's work - the per-collection diff here and the
// file-level Server comparison in serverFieldChange - stays independently
// readable.
//
// Added/Updated/Removed are deliberately not pre-sized: they are mutually
// exclusive per name, so pre-sizing all three to len(names) would
// over-allocate for the overwhelmingly common case of a small diff against a
// large, mostly-unchanged file - and this pass runs once per lock run, after
// the N metadata round trips that built afterIdx.
func diffIndexes(beforeIdx, afterIdx map[string]Entry) Diff {
	var diff Diff
	for _, name := range unionSortedNames(beforeIdx, afterIdx) {
		a, inAfter := afterIdx[name]
		b, inBefore := beforeIdx[name]
		switch {
		case inAfter && !inBefore:
			diff.Added = append(diff.Added, a)
		case inAfter && inBefore:
			if !sameEntry(a, b) {
				diff.Updated = append(diff.Updated, Change{From: b, To: a})
			}
		case !inAfter && inBefore:
			diff.Removed = append(diff.Removed, b)
		}
	}
	return diff
}

// unionSortedNames returns the sorted union of beforeIdx's and afterIdx's
// keys: afters first, then befores not already present, which dedups for
// free since a name in both maps is only ever appended once, from afterIdx.
// Sorted once, after the union is built, rather than merging two
// already-sorted sequences: File's Collections order is whatever Load
// returned (on-disk order) or whatever a caller hand-built, neither of which
// is guaranteed sorted going in.
func unionSortedNames(beforeIdx, afterIdx map[string]Entry) []string {
	names := make([]string, 0, len(beforeIdx)+len(afterIdx))
	for name := range afterIdx {
		names = append(names, name)
	}
	for name := range beforeIdx {
		if _, ok := afterIdx[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// serverFieldChange returns the file-level Server FieldChange between before
// and after, or nil when either is nil (no pair of file-level values to
// compare) or their Server fields agree.
func serverFieldChange(before, after *File) *FieldChange {
	if before == nil || after == nil || before.Server == after.Server {
		return nil
	}
	return &FieldChange{Field: fieldServer, From: before.Server, To: after.Server}
}

// Empty reports whether d describes no difference at all: no collection or
// role added, updated, or removed, and no file-level server change.
func (d Diff) Empty() bool {
	return d.Server == nil && len(d.Added) == 0 && len(d.Updated) == 0 && len(d.Removed) == 0 &&
		len(d.RolesAdded) == 0 && len(d.RolesUpdated) == 0 && len(d.RolesRemoved) == 0
}

// HasRoles reports whether either side of the diff involved a role, for a
// report that names roles only when a run has any.
func (d Diff) HasRoles() bool {
	return len(d.RolesAdded) > 0 || len(d.RolesUpdated) > 0 || len(d.RolesRemoved) > 0
}

// Fields reports which of c's pinned fields differ between From and To, in a
// fixed order (version, source, type, ref, commit, subdir, sha256, deps), each carrying its old and new
// value. It is derived on demand from the same field set sameEntry compares,
// so "what makes an entry Updated" and "what Fields reports as changed"
// cannot drift apart - and Diff.Empty stays allocation-free per updated
// entry, since nothing here runs until a caller asks for it. Deps renders
// through renderDeps, the same sorted, comma-joined canonical form Save and
// Hash use - a comparison, not a presentation choice, since two
// differently-ordered but otherwise identical Deps slices must never appear
// to differ here.
func (c Change) Fields() []FieldChange {
	fields := make([]FieldChange, 0, comparedFieldCount)
	if c.From.Version != c.To.Version {
		fields = append(fields, FieldChange{Field: fieldVersion, From: c.From.Version, To: c.To.Version})
	}
	if c.From.Source != c.To.Source {
		fields = append(fields, FieldChange{Field: fieldSource, From: c.From.Source, To: c.To.Source})
	}
	if c.From.Type != c.To.Type {
		fields = append(fields, FieldChange{Field: fieldType, From: c.From.Type, To: c.To.Type})
	}
	if c.From.Ref != c.To.Ref {
		fields = append(fields, FieldChange{Field: fieldRef, From: c.From.Ref, To: c.To.Ref})
	}
	if c.From.Commit != c.To.Commit {
		fields = append(fields, FieldChange{Field: fieldCommit, From: c.From.Commit, To: c.To.Commit})
	}
	if c.From.Subdir != c.To.Subdir {
		fields = append(fields, FieldChange{Field: fieldSubdir, From: c.From.Subdir, To: c.To.Subdir})
	}
	if c.From.SHA256 != c.To.SHA256 {
		fields = append(fields, FieldChange{Field: fieldSHA256, From: c.From.SHA256, To: c.To.SHA256})
	}
	if !sameDeps(c.From.Deps, c.To.Deps) {
		fields = append(fields, FieldChange{Field: fieldDeps, From: renderDeps(c.From.Deps), To: renderDeps(c.To.Deps)})
	}
	return fields
}

// indexByName builds a name-keyed index of f's collections, pre-sized to its
// entry count. A nil f yields an empty, non-nil map rather than a nil one,
// matching Compare's contract of treating a nil *File as "no entries" rather
// than as an error. A duplicate name resolves last-entry-wins, exactly like
// indexLockfile's own map build.
func indexByName(f *File) map[string]Entry {
	if f == nil {
		return map[string]Entry{}
	}
	idx := make(map[string]Entry, len(f.Collections))
	for _, e := range f.Collections {
		idx[e.Name] = e
	}
	return idx
}

// sameEntry reports whether a and b pin the same collection: identical
// version, source, type, ref, commit, subdir and sha256, and equivalent
// (order-insensitive) deps. Name is deliberately not compared - sameEntry is
// only ever called on two entries Compare has already matched by name.
func sameEntry(a, b Entry) bool {
	return a.Version == b.Version && a.Source == b.Source && a.Type == b.Type &&
		a.Ref == b.Ref && a.Commit == b.Commit && a.Subdir == b.Subdir &&
		a.SHA256 == b.SHA256 && sameDeps(a.Deps, b.Deps)
}

// sameDeps reports whether a and b are the same multiset of dependency
// names: order-insensitive, like canonicalClone's sort makes Hash, but
// DUPLICATE-SENSITIVE - it sorts, it never deduplicates, so ["x.x", "x.x"]
// and ["x.x", "y.y"] are never equal even though both have length 2 and share
// an element. The length check makes the common "nothing changed" case cheap,
// slices.Equal is the fast path for an already-identically-ordered pair (the
// typical case, since both sides usually come from the same graph-walk
// order), and the sorted clones only run when both of those fail.
func sameDeps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if slices.Equal(a, b) {
		return true
	}
	ac := slices.Clone(a)
	bc := slices.Clone(b)
	slices.Sort(ac)
	slices.Sort(bc)
	return slices.Equal(ac, bc)
}

// renderDeps renders deps in the same canonical form Save/Hash commit to
// disk: sorted, comma-joined, with no deduplication - matching sameDeps'
// own multiset semantics rather than set semantics. An empty or nil deps
// renders as the empty string, which Change.Fields's caller (quoteEmpty in
// internal/galaxy/collections/lock.go) turns into "(none)" for display.
func renderDeps(deps []string) string {
	if len(deps) == 0 {
		return ""
	}
	sorted := slices.Clone(deps)
	slices.Sort(sorted)
	return strings.Join(sorted, ",")
}
