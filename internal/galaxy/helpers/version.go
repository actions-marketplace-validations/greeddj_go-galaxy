package helpers

import "github.com/Masterminds/semver/v3"

// IsExactVersion reports whether s parses as a semantic version - the only
// shape this project ever writes or trusts for a resolved collection's
// pinned version, as opposed to a constraint string like "*" or ">=1.0.0"
// that describes a range rather than naming one release.
//
// This is the identical parse solver.NewVersion already requires of every
// candidate a fresh solve produces, so a version that reached this project
// through resolution satisfies IsExactVersion by construction; the predicate
// only ever rejects a value that arrived by some other route - a lockfile
// entry, a cached snapshot record - without having passed through that
// solve.
//
// Acceptance also implies IsPathElement. This is a claim about a third-party
// grammar behind a third-party mutable package global, so it is checked
// against both settings of that global, not just the one this project
// happens to run with: semver.CoerceNewVersion defaults to true, which
// routes NewVersion through coerceNewVersion and its looseSemVerRegex - not
// the stricter semVerRegex a reader would find first - but the property
// holds under CoerceNewVersion false too
// (TestIsExactVersionImpliesIsPathElementExhaustive exercises both
// settings, exhaustively, over an alphabet covering every character class
// either grammar treats specially). Both grammars are anchored `^...$` with
// RE2's default (non-multiline) semantics, under which `$` behaves like
// `\z`, so a trailing "\n" is rejected rather than tolerated before it;
// every literal "." in both is escaped, never left as a wildcard; and both
// admit only [0-9A-Za-z.+-] plus an optional leading "v" in the substrings
// NewVersion's two entry points actually accept, none of which can form
// "..", a path separator, or anything filepath.Base would rewrite.
// Version.Original() storing the checked string verbatim (both entry points
// set it to v, not a reconstruction) is not a gap either: no caller here
// ever calls String() - IsExactVersion returns only a bool, so the caller
// always goes on to use the same string it already passed in. That
// implication is what lets a caller building a filesystem path from an
// already-IsExactVersion-checked version skip a second, redundant
// IsPathElement check on the same value.
func IsExactVersion(s string) bool {
	_, err := semver.NewVersion(s)
	return err == nil
}
