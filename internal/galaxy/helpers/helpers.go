package helpers

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SplitFQDN splits a "namespace.collection" string into parts.
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
