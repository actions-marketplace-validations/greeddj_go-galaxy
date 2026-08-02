package proseaudit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The UTF-8 encodings of the two banned characters share a two-byte prefix;
// the third byte separates them. They are spelled as bytes rather than as
// rune literals so that this file is not itself an offender, which is the
// same trick cmd/go-galaxy/commands/explain_test.go uses for its assertion
// about em dashes in rendered output.
const (
	dashPrefixFirst  = 0xE2
	dashPrefixSecond = 0x80
	enDashFinal      = 0x93
	emDashFinal      = 0x94
)

// TestCommittedTextUsesHyphenMinus is the gate for the repository's
// hyphen-minus-only rule: no committed file may carry U+2014 or U+2013.
//
// It enumerates through `git ls-files` rather than walking the filesystem,
// because the rule is about committed text. Walking would sweep in working
// files that are deliberately outside it and, worse, would silently stop
// covering the tracked prose under dot-directories the moment a walk learned
// to skip them - which is where four of this rule's five surviving violations
// were found.
//
// Run against the state before the sweep that introduced it, this failed with
//
//	forbidden dashes in committed text (use hyphen-minus):
//	    .claude/skills/go-galaxy-build/SKILL.md:3: U+2014 (em dash)
//
// as the first of 29 lines, one per occurrence, across five files.
func TestCommittedTextUsesHyphenMinus(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)

	var problems []string
	for _, rel := range trackedFiles(t, root) {
		// #nosec G304 -- rel comes from git's own listing of this repository
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			// A tracked path with nothing on disk is a deletion staged but not
			// committed, which is not this gate's business.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			t.Fatalf("reading %s: %v", rel, err)
		}
		problems = append(problems, dashHits(rel, data)...)
	}

	if len(problems) > 0 {
		t.Fatalf("forbidden dashes in committed text (use hyphen-minus):\n\t%s", strings.Join(problems, "\n\t"))
	}
}

// TestDashHitsReportsBothDashes pins the scanner against a fixture that is
// shown capable of passing: the same sentence is scanned three times,
// differing only in the character joining its two halves.
func TestDashHitsReportsBothDashes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		wantName string
		joiner   []byte
		wantHits int
	}{
		{name: "hyphen_minus", joiner: []byte{'-'}, wantHits: 0},
		{name: "em_dash", joiner: []byte{dashPrefixFirst, dashPrefixSecond, emDashFinal}, wantHits: 1, wantName: "U+2014"},
		{name: "en_dash", joiner: []byte{dashPrefixFirst, dashPrefixSecond, enDashFinal}, wantHits: 1, wantName: "U+2013"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fixture := append([]byte("first line\nsecond half "), tc.joiner...)
			fixture = append(fixture, []byte(" third half\n")...)

			hits := dashHits("fixture.md", fixture)
			if len(hits) != tc.wantHits {
				t.Fatalf("dashHits reported %d hits, want %d: %v", len(hits), tc.wantHits, hits)
			}
			if tc.wantHits == 0 {
				return
			}
			if !strings.Contains(hits[0], tc.wantName) {
				t.Fatalf("hit %q does not name %s", hits[0], tc.wantName)
			}
			// The fixture's dash sits on the second line, so a scanner that
			// counted bytes instead of newlines would report line 1 here.
			if !strings.Contains(hits[0], "fixture.md:2:") {
				t.Fatalf("hit %q does not name the dash's own line", hits[0])
			}
		})
	}
}

// dashHits reports every banned dash in data, as `name:line: which`, in the
// order they appear.
func dashHits(name string, data []byte) []string {
	var hits []string
	for at := 0; at+2 < len(data); at++ {
		if data[at] != dashPrefixFirst || data[at+1] != dashPrefixSecond {
			continue
		}
		var which string
		switch data[at+2] {
		case emDashFinal:
			which = "U+2014 (em dash)"
		case enDashFinal:
			which = "U+2013 (en dash)"
		default:
			continue
		}
		line := 1 + bytes.Count(data[:at], []byte{'\n'})
		hits = append(hits, fmt.Sprintf("%s:%d: %s", name, line, which))
	}
	return hits
}

// trackedFiles returns every path git has under root, as slash paths.
//
// A tree git cannot enumerate skips the gate rather than failing it: the rule
// is about committed text, and a module extracted from a tarball into the
// build cache has none to check. That is a real way `go test` runs on this
// package and the only one where git is absent, so the skip cannot hide a
// violation in a repository where the rule applies.
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()

	// #nosec G204 -- root is this module's own directory, resolved from the
	// working directory rather than from any input.
	out, err := exec.CommandContext(t.Context(), "git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("git ls-files in %s: %v", root, err)
	}

	names := strings.Split(string(out), "\x00")
	files := make([]string, 0, len(names))
	for _, name := range names {
		if name != "" {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		t.Fatalf("git tracks no files under %s", root)
	}
	return files
}
