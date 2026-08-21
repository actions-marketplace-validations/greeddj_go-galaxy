package gitsource

import (
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// LocatorPrefix marks a persisted source string as a git locator. It is the
// one durable signal a consumer holding only a Source string has that the
// string is not a Galaxy server base.
const LocatorPrefix = "git+"

const (
	locatorSubdirSep = "#"
	locatorCommitSep = "@"
	gitDirName       = ".git"
)

// Locator identifies a git collection source: the canonical repository URL,
// the subdir inside it ("" for the root), and the commit it resolved to ("" for
// a requirement that has not been resolved yet). Its String form is what the
// program persists wherever a source is recorded.
type Locator struct {
	URL    string
	Subdir string
	Commit string
}

// String renders git+<url>#<subdir>[@<commit>]. The "#" is always written so
// the grammar stays unambiguous: a canonical URL never contains "#", and a
// subdir never contains "@" (ParseSubdir refuses it), so the commit is exactly
// the suffix after the last "@".
func (l Locator) String() string {
	var b strings.Builder
	b.WriteString(LocatorPrefix)
	b.WriteString(l.URL)
	b.WriteString(locatorSubdirSep)
	b.WriteString(l.Subdir)
	if l.Commit != "" {
		b.WriteString(locatorCommitSep)
		b.WriteString(l.Commit)
	}
	return b.String()
}

// Pinned reports whether the locator carries a commit.
func (l Locator) Pinned() bool { return l.Commit != "" }

// IsLocator reports whether s is a git locator rather than a Galaxy server
// base: the cheap prefix test every Source-holding consumer dispatches on.
func IsLocator(s string) bool {
	return strings.HasPrefix(s, LocatorPrefix)
}

// ParseLocator parses the String form back. It accepts only the canonical
// spelling: the URL part must round-trip through ParseURL unchanged, the
// subdir must satisfy ParseSubdir, and a commit, when present, must be forty
// lowercase hex digits. Anything else is helpers.ErrInvalidGitLocator, which
// is reachable only through a hand-edited record.
func ParseLocator(s string) (Locator, error) {
	if !IsLocator(s) {
		return Locator{}, fmt.Errorf("%w: missing %q prefix", helpers.ErrInvalidGitLocator, LocatorPrefix)
	}
	rest := s[len(LocatorPrefix):]
	rawURL, remainder, ok := strings.Cut(rest, locatorSubdirSep)
	if !ok {
		return Locator{}, fmt.Errorf("%w: missing %q separator", helpers.ErrInvalidGitLocator, locatorSubdirSep)
	}
	u, err := ParseURL(rawURL)
	if err != nil || u.String() != rawURL {
		return Locator{}, fmt.Errorf("%w: url part is not canonical", helpers.ErrInvalidGitLocator)
	}
	subdir, commit := remainder, ""
	if i := strings.LastIndex(remainder, locatorCommitSep); i >= 0 {
		subdir, commit = remainder[:i], remainder[i+1:]
		if !IsCommitHash(commit) {
			return Locator{}, fmt.Errorf("%w: commit is not a lowercase 40-hex hash", helpers.ErrInvalidGitLocator)
		}
	}
	if subdir != "" {
		if _, err := ParseSubdir(subdir); err != nil {
			return Locator{}, fmt.Errorf("%w: %w", helpers.ErrInvalidGitLocator, err)
		}
	}
	return Locator{URL: rawURL, Subdir: subdir, Commit: commit}, nil
}

// ParseSubdir validates a #subdir fragment and returns it with surrounding
// slashes stripped. Every "/"-separated element must be a safe path element
// (helpers.IsPathElement), must not be ".git", and must not contain "@" or a
// backslash: "@" is the locator's commit separator, and a backslash is a path
// separator on the one platform helpers.IsPathElement does not cover it for.
func ParseSubdir(s string) (string, error) {
	subdir := strings.Trim(strings.TrimSpace(s), "/")
	if subdir == "" {
		return "", nil
	}
	for element := range strings.SplitSeq(subdir, "/") {
		switch {
		case !helpers.IsPathElement(element):
			return "", fmt.Errorf("%w: %q is not a safe path element", helpers.ErrInvalidGitSubdir, element)
		case strings.EqualFold(element, gitDirName):
			return "", fmt.Errorf("%w: %q names a git directory", helpers.ErrInvalidGitSubdir, element)
		case strings.ContainsAny(element, locatorCommitSep+`\`):
			return "", fmt.Errorf("%w: %q contains a character a subdir cannot carry", helpers.ErrInvalidGitSubdir, element)
		}
	}
	return subdir, nil
}

// PinKey is the store key of a git pin: which commit a (url, ref, subdir)
// requirement last resolved to. Newlines separate the parts because none of
// the three can contain one once validated.
func PinKey(url, ref, subdir string) string {
	return url + "\n" + ref + "\n" + subdir
}
