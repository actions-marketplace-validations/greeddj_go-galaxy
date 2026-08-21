package collectionbuild

import (
	"strings"
	"testing"
)

type fnmatchCase struct {
	name    string
	pattern string
	input   string
	want    bool
}

// fnmatchCases pin Python's fnmatch.fnmatch semantics. Each row's comment
// is the Python expression whose value the row asserts, derived by hand
// from fnmatch.translate's rules rather than run.
func fnmatchCases() []fnmatchCase {
	return []fnmatchCase{
		// fnmatch.fnmatch('galaxy.yml', 'galaxy.yml') -> True
		{name: "literal", pattern: "galaxy.yml", input: "galaxy.yml", want: true},
		// fnmatch.fnmatch('Galaxy.yml', 'galaxy.yml') -> False (normcase is identity on POSIX)
		{name: "case sensitive", pattern: "galaxy.yml", input: "Galaxy.yml", want: false},
		// fnmatch.fnmatch('docs/galaxy.yml', 'galaxy.yml') -> False (anchored)
		{name: "anchored at start", pattern: "galaxy.yml", input: "docs/galaxy.yml", want: false},
		// fnmatch.fnmatch('galaxy.yml.bak', 'galaxy.yml') -> False (anchored)
		{name: "anchored at end", pattern: "galaxy.yml", input: "galaxy.yml.bak", want: false},
		// fnmatch.fnmatch('a/b/c.pyc', '*.pyc') -> True ('*' crosses '/')
		{name: "star crosses slash", pattern: "*.pyc", input: "a/b/c.pyc", want: true},
		// fnmatch.fnmatch('a/b', 'a?b') -> True ('?' matches '/')
		{name: "question crosses slash", pattern: "a?b", input: "a/b", want: true},
		// fnmatch.fnmatch('ab', 'a?b') -> False
		{name: "question needs one", pattern: "a?b", input: "ab", want: false},
		// fnmatch.fnmatch('tests/output', 'tests/output') -> True
		{name: "path literal", pattern: "tests/output", input: "tests/output", want: true},
		// fnmatch.fnmatch('tests/output/x', 'tests/output') -> False
		{name: "path literal is anchored", pattern: "tests/output", input: "tests/output/x", want: false},
		// fnmatch.fnmatch('', '*') -> True
		{name: "star matches empty", pattern: "*", input: "", want: true},
		// fnmatch.fnmatch('', '') -> True
		{name: "empty matches empty", pattern: "", input: "", want: true},
		// fnmatch.fnmatch('x', '') -> False
		{name: "empty pattern", pattern: "", input: "x", want: false},
		// fnmatch.fnmatch('acme-app-1.0.0.tar.gz', 'acme-app-*.tar.gz') -> True
		{name: "tarball", pattern: "acme-app-*.tar.gz", input: "acme-app-1.0.0.tar.gz", want: true},
		// fnmatch.fnmatch('acme-app-.tar.gz', 'acme-app-*.tar.gz') -> True
		{name: "tarball star empty", pattern: "acme-app-*.tar.gz", input: "acme-app-.tar.gz", want: true},
		// fnmatch.fnmatch('other-app-1.0.0.tar.gz', 'acme-app-*.tar.gz') -> False
		{name: "tarball other", pattern: "acme-app-*.tar.gz", input: "other-app-1.0.0.tar.gz", want: false},
		// fnmatch.fnmatch('b', '[abc]') -> True
		{name: "set", pattern: "[abc]", input: "b", want: true},
		// fnmatch.fnmatch('d', '[abc]') -> False
		{name: "set miss", pattern: "[abc]", input: "d", want: false},
		// fnmatch.fnmatch('d', '[!abc]') -> True
		{name: "negated set", pattern: "[!abc]", input: "d", want: true},
		// fnmatch.fnmatch('a', '[!abc]') -> False
		{name: "negated set miss", pattern: "[!abc]", input: "a", want: false},
		// fnmatch.fnmatch('/', '[!abc]') -> True (a set matches '/')
		{name: "negated set matches slash", pattern: "[!abc]", input: "/", want: true},
		// fnmatch.fnmatch('m', '[a-z]') -> True
		{name: "range", pattern: "[a-z]", input: "m", want: true},
		// fnmatch.fnmatch('M', '[a-z]') -> False
		{name: "range miss", pattern: "[a-z]", input: "M", want: false},
		// fnmatch.fnmatch('5', '[0-9]') -> True
		{name: "digit range", pattern: "[0-9]", input: "5", want: true},
		// fnmatch.fnmatch(']', '[]]') -> True (leading ']' is literal)
		{name: "leading bracket literal", pattern: "[]]", input: "]", want: true},
		// fnmatch.fnmatch(']', '[!]]') -> False
		{name: "negated leading bracket", pattern: "[!]]", input: "]", want: false},
		// fnmatch.fnmatch('x', '[!]]') -> True
		{name: "negated leading bracket other", pattern: "[!]]", input: "x", want: true},
		// fnmatch.fnmatch('[', '[') -> True (unterminated '[' is literal)
		{name: "unterminated bracket", pattern: "[", input: "[", want: true},
		// fnmatch.fnmatch('[a', '[a') -> True
		{name: "unterminated bracket with body", pattern: "[a", input: "[a", want: true},
		// fnmatch.fnmatch('a', '[a') -> False
		{name: "unterminated bracket is not a set", pattern: "[a", input: "a", want: false},
		// fnmatch.fnmatch('b', '[z-a]') -> False (reversed range matches nothing)
		{name: "reversed range", pattern: "[z-a]", input: "b", want: false},
		// fnmatch.fnmatch('z', '[z-a]') -> False
		{name: "reversed range endpoint", pattern: "[z-a]", input: "z", want: false},
		// fnmatch.fnmatch('\\', '\\') -> True (no escaping: backslash is literal)
		{name: "backslash literal", pattern: `\`, input: `\`, want: true},
		// fnmatch.fnmatch('*', '\\*') -> False (backslash does not escape; '\\' must match itself)
		{name: "backslash does not escape", pattern: `\*`, input: "*", want: false},
		// fnmatch.fnmatch('\\x', '\\*') -> True
		{name: "backslash then star", pattern: `\*`, input: `\x`, want: true},
		// fnmatch.fnmatch('-', '[a-]') -> True (trailing '-' is literal)
		{name: "trailing dash literal", pattern: "[a-]", input: "-", want: true},
		// fnmatch.fnmatch('-', '[-a]') -> True (leading '-' is literal)
		{name: "leading dash literal", pattern: "[-a]", input: "-", want: true},
		// fnmatch.fnmatch('aXXXb', 'a*b') -> True
		{name: "star in middle", pattern: "a*b", input: "aXXXb", want: true},
		// fnmatch.fnmatch('ab', 'a*b') -> True
		{name: "star in middle empty", pattern: "a*b", input: "ab", want: true},
		// fnmatch.fnmatch('abcabc', 'a*c*c') -> True
		{name: "two stars backtrack", pattern: "a*c*c", input: "abcabc", want: true},
		// fnmatch.fnmatch('abcabd', 'a*c*c') -> False
		{name: "two stars no match", pattern: "a*c*c", input: "abcabd", want: false},
		// fnmatch.fnmatch('a', '**') -> True
		{name: "double star", pattern: "**", input: "a", want: true},
		// fnmatch.fnmatch('ab', '*?') -> True
		{name: "star then question", pattern: "*?", input: "ab", want: true},
		// fnmatch.fnmatch('', '*?') -> False
		{name: "star then question on empty", pattern: "*?", input: "", want: false},
		// fnmatch.fnmatch(b'docs/\xc3\xa9.md', b'docs/??.md') -> True (ansible matches bytes: 'é' is two)
		{name: "question is one byte", pattern: "docs/??.md", input: "docs/\u00e9.md", want: true},
		// fnmatch.fnmatch(b'docs/\xc3\xa9.md', b'docs/?.md') -> False (one '?' is one byte of the rune)
		{name: "question does not span a rune", pattern: "docs/?.md", input: "docs/\u00e9.md", want: false},
		// fnmatch.fnmatch(b'\xc3\xa9', b'[a-z]') -> False (a set compares single bytes)
		{name: "set does not match a multi-byte rune", pattern: "[a-z]", input: "\u00e9", want: false},
		// fnmatch.fnmatch(b'\xc3\xa9', b'[\xc3][\xa9]') -> True (two sets, one byte each)
		{name: "sets match the bytes of a rune", pattern: "[\xc3][\xa9]", input: "\u00e9", want: true},
		// fnmatch.fnmatch(b'docs/\xc3\xa9/\xc3\xa8.md', b'docs/*.md') -> True ('*' crosses runes and '/')
		{name: "star crosses multi-byte runes", pattern: "docs/*.md", input: "docs/\u00e9/\u00e8.md", want: true},
		// fnmatch.fnmatch('.git', '.git') -> True
		{name: "dot git", pattern: ".git", input: ".git", want: true},
		// fnmatch.fnmatch('a/.git', '.git') -> False (root-anchored; the basename rule catches it)
		{name: "dot git nested", pattern: ".git", input: "a/.git", want: false},
		// fnmatch.fnmatch('a[b', 'a[b') -> True
		{name: "bracket literal inside", pattern: "a[b", input: "a[b", want: true},
		// fnmatch.fnmatch('a!b', 'a[!]b') -> ... '[!]b]' would need a ']' after: pattern 'a[!]b' has
		// no closing bracket after the stepped-over ']', so '[' is literal: 'a[!]b' == 'a[!]b' -> True
		{name: "unterminated negated set", pattern: "a[!]b", input: "a[!]b", want: true},
	}
}

func TestFnmatch(t *testing.T) {
	t.Parallel()
	for _, tt := range fnmatchCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Fnmatch(tt.pattern, tt.input); got != tt.want {
				t.Fatalf("Fnmatch(%q, %q) = %v, want %v", tt.pattern, tt.input, got, tt.want)
			}
		})
	}
}

// TestFnmatchStarChainIsBounded proves the memo: a pattern of many stars
// against a long non-matching name finishes, where a naive backtracker
// would not.
func TestFnmatchStarChainIsBounded(t *testing.T) {
	t.Parallel()
	pattern := strings.Repeat("*a", 30) + "b"
	input := strings.Repeat("a", 200)
	if Fnmatch(pattern, input) {
		t.Fatal("pattern ending in b matched an input of a's")
	}
}

type ignoreCase struct {
	name        string
	rel         string
	buildIgnore []string
	isDir       bool
	want        bool
}

func ignoreCases() []ignoreCase {
	return []ignoreCase{
		{name: "galaxy.yml at root", rel: "galaxy.yml", want: true},
		{name: "galaxy.yaml at root", rel: "galaxy.yaml", want: true},
		{name: "galaxy.yml nested is kept", rel: "docs/galaxy.yml", want: false},
		{name: "MANIFEST.json at root", rel: "MANIFEST.json", want: true},
		{name: "FILES.json at root", rel: "FILES.json", want: true},
		{name: "FILES.json nested is kept", rel: "a/FILES.json", want: false},
		{name: "pyc anywhere", rel: "plugins/modules/x.pyc", want: true},
		{name: "retry anywhere", rel: "site.retry", want: true},
		{name: "tests/output at root", rel: "tests/output", isDir: true, want: true},
		{name: "tests/output nested is kept", rel: "x/tests/output", isDir: true, want: false},
		{name: "tests/unit is kept", rel: "tests/unit", isDir: true, want: false},
		{name: "own tarball", rel: "acme-app-1.2.3.tar.gz", want: true},
		{name: "other tarball is kept", rel: "other-app-1.2.3.tar.gz", want: false},
		{name: ".git dir at root", rel: ".git", isDir: true, want: true},
		{name: ".git file at root", rel: ".git", want: true},
		{name: ".git dir nested", rel: "a/b/.git", isDir: true, want: true},
		{name: ".git file nested is kept", rel: "a/.git", want: false},
		{name: "__pycache__ nested dir", rel: "plugins/__pycache__", isDir: true, want: true},
		{name: "__pycache__ as file is kept", rel: "plugins/__pycache__", want: false},
		{name: "CVS dir", rel: "x/CVS", isDir: true, want: true},
		{name: ".tox dir", rel: ".tox", isDir: true, want: true},
		{name: ".svn .hg .bzr", rel: "a/.svn", isDir: true, want: true},
		{name: "build_ignore pattern", rel: "notes.bak", buildIgnore: []string{"*.bak"}, want: true},
		{name: "build_ignore dir", rel: "changelogs/fragments", buildIgnore: []string{"changelogs/fragments"}, isDir: true, want: true},
		{name: "build_ignore does not rescue defaults", rel: "x.pyc", buildIgnore: []string{"keep"}, want: true},
		{name: "build_ignore root anchored", rel: "a/notes.bak", buildIgnore: []string{"notes.bak"}, want: false},
		{name: "build_ignore star crosses slash", rel: "a/notes.bak", buildIgnore: []string{"*notes.bak"}, want: true},
		{name: "plain file kept", rel: "README.md", want: false},
	}
}

func TestIgnoreRules(t *testing.T) {
	t.Parallel()
	for _, tt := range ignoreCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rules := newIgnoreRules("acme", "app", tt.buildIgnore)
			if got := rules.skip(tt.rel, tt.isDir); got != tt.want {
				t.Fatalf("skip(%q, dir=%v) = %v, want %v", tt.rel, tt.isDir, got, tt.want)
			}
		})
	}
}

func TestIgnoreRulesOrder(t *testing.T) {
	t.Parallel()
	rules := newIgnoreRules("acme", "app", []string{"custom/*", "second"})
	want := []string{"MANIFEST.json", "FILES.json", "galaxy.yml", "galaxy.yaml", ".git", "*.pyc", "*.retry",
		"tests/output", "acme-app-*.tar.gz", "custom/*", "second"}
	if strings.Join(rules.patterns, "|") != strings.Join(want, "|") {
		t.Fatalf("patterns = %q, want %q", rules.patterns, want)
	}
}

func TestIgnoreRulesExcludes(t *testing.T) {
	t.Parallel()
	rules := newIgnoreRules("acme", "app", []string{"private"})
	for rel, want := range map[string]bool{
		"private/x":            true,
		"a/__pycache__/y":      true,
		"tests/output/z":       true,
		"tests/unit/z":         false,
		"a/b/c.pyc":            true,
		"a/b/c.py":             false,
		"plugins/modules/.git": false,
	} {
		if got := rules.excludes(rel, false); got != want {
			t.Errorf("excludes(%q) = %v, want %v", rel, got, want)
		}
	}
}
