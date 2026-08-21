package helpers

import (
	"fmt"
	"strings"
)

// Role identifiers are judged by three predicates, one per boundary, none
// of them shared with the collection alphabet: what a Galaxy role may be
// called (IsRoleName), what directory a role may install into
// (IsRoleInstallName), and what a role's version may read as
// (IsRoleVersion). They are deliberately not expressed through
// IsCollectionNamePart, which refuses a hyphen, an upper-case letter and a
// leading digit - all three occur in real role names on galaxy.ansible.com
// (geerlingguy.php-versions, 6connect.x) - nor through IsPathElement alone,
// for the reason IsCollectionNamePart's own doc comment gives: a product
// rule about what this tool installs must not be tied to a filesystem rule
// about what it may write.

const (
	// roleNamePartMaxLen bounds one half of a Galaxy role name. GitHub caps a
	// login at 39 characters and a repository name at 100; the Galaxy name is
	// the login and a short role name, so 64 leaves room without admitting a
	// value no server could have minted.
	roleNamePartMaxLen = 64
	// roleInstallNameMaxLen bounds the directory a role installs into. A
	// Galaxy name is at most 2*roleNamePartMaxLen+1; a git role's default
	// name is a repository name, which GitHub caps at 100.
	roleInstallNameMaxLen = 128
	// roleVersionMaxLen bounds a role version, which is a git ref name or a
	// tag; git itself imposes no cap, this tool does so a version fits in a
	// message and a filename.
	roleVersionMaxLen = 128
	// roleNameParts is the two halves of a Galaxy role name, owner.role.
	roleNameParts = 2
)

// runesMatch reports whether s is non-empty, its first rune satisfies
// first and every later rune satisfies rest.
func runesMatch(s string, first, rest func(r rune) bool) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && !first(r) || i > 0 && !rest(r) {
			return false
		}
	}
	return true
}

func isASCIIAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// IsRoleNamePart reports whether part is a valid half of a Galaxy role
// name - the owner or the role - under ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$:
// the GitHub login alphabet (letters, digits, hyphen, no leading hyphen),
// widened by the underscore legacy Galaxy namespaces carry. The dot is
// excluded from a part so the whole name splits in exactly one place.
func IsRoleNamePart(part string) bool {
	return len(part) <= roleNamePartMaxLen && runesMatch(part, isASCIIAlnum,
		func(r rune) bool { return isASCIIAlnum(r) || r == '_' || r == '-' })
}

// SplitRoleName splits a Galaxy role name into its owner and role halves.
// It requires exactly one dot, with both halves satisfying IsRoleNamePart.
// ansible splits at the last dot and lets an owner carry dots of its own; no
// GitHub login can, so this tool asks for the one unambiguous spelling and
// refuses the rest at the boundary rather than guessing which dot was meant.
func SplitRoleName(value string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != roleNameParts || !IsRoleNamePart(parts[0]) || !IsRoleNamePart(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsRoleName reports whether value is a well-formed Galaxy role name,
// owner.role, as SplitRoleName accepts it.
func IsRoleName(value string) bool {
	_, _, ok := SplitRoleName(value)
	return ok
}

// IsRoleInstallName reports whether name may be the directory a role
// installs into under roles_path and the identifier a playbook names it by:
// IsPathElement, so it is one safe path component, and
// ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ on top, so it is neither hidden (a
// leading dot would collide with the dotfiles this tool and ansible keep
// beside the roles) nor option-shaped (a leading hyphen). Every Galaxy role
// name satisfies it; a git role's name: is judged by it at load.
// "ansible_collections" is refused outright: a roles_path that is also the
// collections path must never let a role replace the collections tree.
func IsRoleInstallName(name string) bool {
	if !IsPathElement(name) || len(name) > roleInstallNameMaxLen || name == "ansible_collections" {
		return false
	}
	return runesMatch(name, func(r rune) bool { return isASCIIAlnum(r) || r == '_' },
		func(r rune) bool { return isASCIIAlnum(r) || r == '_' || r == '.' || r == '-' })
}

// IsRoleVersion reports whether s has the shape of a role version as this
// tool persists one - a tag name, a branch name or a commit hash - under
// ^[A-Za-z0-9][A-Za-z0-9._/+-]{0,127}$ with no "..", no "//", no trailing
// "/" and no ".lock" suffix: a strict subset of git's own ref-name rules,
// applied wherever a version is read back (a store record, a lockfile entry)
// rather than parsed from a requirements file, where gitsource.ParseRef
// judges the full grammar. Unlike the other two predicates it is not a path
// element rule: "/" is legal, because release/1.x is a branch name, and the
// artifact key builder escapes the version before it becomes part of a
// filename.
func IsRoleVersion(s string) bool {
	if len(s) > roleVersionMaxLen || strings.Contains(s, "..") || strings.Contains(s, "//") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".lock") {
		return false
	}
	return runesMatch(s, isASCIIAlnum,
		func(r rune) bool { return isASCIIAlnum(r) || strings.ContainsRune("._/+-", r) })
}

// RoleArtifactFilename composes the cache filename for a role artifact
// tarball: "role.<name>-<version>.tar.gz". It is the role counterpart of
// ArtifactFilename and shares its contract: composed identically by the
// writer (the role install) and the deleter (cleanup). The "role." prefix is
// what keeps a role and a collection built from one repository and commit -
// and therefore cached under one locator scope - apart: the text before the
// first "-" of a collection filename is a namespace, whose alphabet has no
// dot, and here that text always carries one. The version is escaped by the
// key builder, so a branch name with a slash is a legal version here.
func RoleArtifactFilename(name, version string) string {
	return fmt.Sprintf("role.%s-%s.tar.gz", name, version)
}
