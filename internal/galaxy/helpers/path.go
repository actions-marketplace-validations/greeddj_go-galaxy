package helpers

import (
	"os"
	"path/filepath"
	"strings"
)

// IsPathElement reports whether name is safe to use as a single path
// element (a directory or file name segment) rather than a path. It is true
// only if name is non-empty, is not "." or "..", contains no path separator
// of either kind, and filepath.Base(name) does not change it. Callers use
// this to validate untrusted identifiers (e.g. namespace/name/version parsed
// from a manifest) before joining them into a filesystem path.
func IsPathElement(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsRune(name, '/') {
		return false
	}
	if os.PathSeparator != '/' && strings.ContainsRune(name, os.PathSeparator) {
		return false
	}
	return filepath.Base(name) == name
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
