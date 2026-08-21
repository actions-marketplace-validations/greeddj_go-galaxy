package collectionbuild

import (
	"strings"
	"testing"
)

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
			if got := rules.Skip(tt.rel, tt.isDir); got != tt.want {
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
	if strings.Join(rules.Patterns, "|") != strings.Join(want, "|") {
		t.Fatalf("patterns = %q, want %q", rules.Patterns, want)
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
		if got := rules.Excludes(rel, false); got != want {
			t.Errorf("excludes(%q) = %v, want %v", rel, got, want)
		}
	}
}
