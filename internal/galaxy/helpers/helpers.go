package helpers

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsCollectionNamePart reports whether part is a valid half of a collection
// name - a namespace or a name - under the alphabet ^[a-z][a-z0-9_]*$.
//
// That is the alphabet galaxy.ansible.com and Automation Hub themselves
// accept, so a half outside it cannot name a collection any Galaxy server
// could serve. Enforcing it on the way in is what turns a hostile identifier
// into a classified failure at the boundary it entered through, instead of an
// unclassified one much later: a name carrying a newline used to pass every
// check, be printed into reports as extra lines of its own choosing, and
// finally fail while a URL was being built - reported as a network failure,
// the one class a CI is most likely to retry forever.
//
// It is deliberately NOT expressed through IsPathElement, even though that
// predicate would reject the same hostile inputs today. The two answer
// different questions: IsPathElement asks whether a value can safely become
// one component of a filesystem path, and is applied to versions and to
// values walked off disk as well; this asks what a collection may be called,
// which is a product decision about the ecosystem this tool installs from. A
// third consumer sharing IsPathElement would have tied all three sides -
// install's writes, cleanup's deletes, and this read boundary - to one
// alphabet that no longer serves any of them exactly.
func IsCollectionNamePart(part string) bool {
	if part == "" {
		return false
	}
	for i, r := range part {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_'):
		default:
			return false
		}
	}
	return true
}

// IsCollectionName reports whether value is a well-formed collection name:
// exactly two dot-separated halves, each satisfying IsCollectionNamePart. It
// is the whole-identifier form of that predicate, for a caller holding the
// combined "namespace.name" string rather than its halves.
func IsCollectionName(value string) bool {
	namespace, name, ok := SplitFQDN(value)
	return ok && IsCollectionNamePart(namespace) && IsCollectionNamePart(name)
}

// SplitFQDN splits a "namespace.collection" string into parts. It validates
// shape only - exactly one dot, two non-empty halves - and deliberately no
// alphabet: see IsCollectionName for the check a caller reading a name from
// outside this program applies on top.
func SplitFQDN(value string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != CollectionNameParts {
		return "", "", false
	}
	if parts[0] == "" || parts[1] == "" {
		return "", "", false
	}

	return parts[0], parts[1], true
}

// UpperFirstRune returns s with the first rune converted to upper case.
func UpperFirstRune(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// NormalizeConstraint trims and normalizes a version constraint string for
// Masterminds/semver v3 parsing.
//
// Beyond the pre-existing trim/match-all handling, this rewrites ansible's
// `==` exact-match operator to v3's `=` per comma-separated clause:
// Masterminds/semver v3 does not accept ansible's == operator -
// NewConstraint("==1.2.3") fails to parse - so a == constraint from a
// requirements.yml or a dependency manifest must be rewritten to = to be
// usable. The rewrite is scoped to strings containing "==" so every
// constraint that does not use it is returned byte-identical to its input.
func NormalizeConstraint(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "*" {
		return ""
	}
	if !strings.Contains(trimmed, "==") {
		return trimmed
	}
	parts := strings.Split(trimmed, ",")
	for i, part := range parts {
		p := strings.TrimSpace(part)
		// "===" (or any run of more than two '=') is left untouched: it is not
		// ansible's exact-match operator and must still fail to parse as an
		// unrecognized operator rather than be silently coerced into "=".
		if strings.HasPrefix(p, "==") && !strings.HasPrefix(p, "===") {
			p = "=" + p[2:]
		}
		parts[i] = p
	}
	return strings.Join(parts, ",")
}
