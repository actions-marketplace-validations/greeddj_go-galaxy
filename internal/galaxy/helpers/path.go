package helpers

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

// IsPathElement reports whether name is safe to use as a single path
// element (a directory or file name segment) rather than a path. It is true
// only if name is non-empty, is not "." or "..", contains no path separator
// of either kind, filepath.Base(name) does not change it, and name contains
// no rune for which safeout.IsUnsafeRune is true. Callers use this to
// validate untrusted identifiers (e.g. namespace/name/version parsed from a
// manifest) before joining them into a filesystem path.
//
// The control-character check deliberately does not carry safeout.Clean's
// \n/\t exception: Clean keeps those two because it sanitizes whole lines of
// already-decided output, where \n is how errors.Join separates joined
// causes and \t only ever moves a cursor forward. Neither justification
// applies to a single path element - a namespace, name, or version that
// will itself become one component of a printed identifier - so both are
// rejected here, alongside U+2028/U+2029, which terminate a line for a
// Unicode-aware consumer (e.g. Python's str.splitlines()) the same way \n
// does for a terminal. Left deliberately unmirrored is Clean's separate
// invalid-UTF-8 rule: an invalid byte renders as a visible U+FFFD, which can
// neither forge a line nor command a terminal, and rejecting it would make
// a filesystem-facing check like this one refuse a name that is merely
// non-UTF-8 - the wrong direction for a tool whose job includes reclaiming
// whatever a filesystem actually holds, encoding aside.
//
// This is the structural closure for every value this program prints that
// is assembled solely from IsPathElement-validated components: such a value
// can never carry a character able to forge a printed line or drive a
// terminal, so it may be printed with a bare %s rather than needing a %q at
// each call site - a convention the next author would otherwise have to
// remember to repeat, and would eventually forget.
func IsPathElement(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsRune(name, '/') {
		return false
	}
	// Covers the Windows backslash separator; unreachable on POSIX, where
	// os.PathSeparator is '/' and the check above already handles it.
	if os.PathSeparator != '/' && strings.ContainsRune(name, os.PathSeparator) {
		return false
	}
	// Unreachable on POSIX given the empty/"."/".."/separator guards above:
	// filepath.Base(x) == x always holds for what remains, so this branch
	// exists only for a filepath.Base behavior this function does not rely
	// on today.
	if filepath.Base(name) != name {
		return false
	}
	return !strings.ContainsFunc(name, safeout.IsUnsafeRune)
}

// WithinDir reports whether target is base itself or lies inside base once
// both are cleaned. It is used as a defense-in-depth check right before a
// destructive filesystem operation, re-verifying containment even when the
// path was already built from validated elements.
func WithinDir(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
