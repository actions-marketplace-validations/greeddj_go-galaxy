package commands

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// TestPrintExplainOrphan checks the orphan-in-lockfile case: a target that is
// neither a root requirement nor depended on by anything else must print the
// hyphen-minus message and must not contain an em dash (U+2014).
func TestPrintExplainOrphan(t *testing.T) {
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "ns.orphan", Version: "1.0.0"},
		},
	}
	roots := map[string]bool{}

	var buf strings.Builder
	if err := printExplain(&buf, lf, "ns.orphan", roots); err != nil {
		t.Fatalf("printExplain() error = %v, want nil", err)
	}

	out := buf.String()
	if !strings.Contains(out, "(no parents - orphan in lockfile)") {
		t.Errorf("printExplain() output missing orphan message; got:\n%s", out)
	}
	if strings.ContainsRune(out, '—') {
		t.Errorf("printExplain() output contains an em dash (U+2014); got:\n%s", out)
	}
}

// TestPrintExplainRequiredByAndDepends checks the normal case: a root
// requirement with its own dependency prints both the "required by" (root)
// line and the "depends on" line.
func TestPrintExplainRequiredByAndDepends(t *testing.T) {
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "community.general", Version: "11.5.0", Source: "galaxy", Deps: []string{"ansible.posix"}},
			{Name: "ansible.posix", Version: "2.0.0"},
		},
	}
	roots := map[string]bool{"community.general": true}

	var buf strings.Builder
	if err := printExplain(&buf, lf, "community.general", roots); err != nil {
		t.Fatalf("printExplain() error = %v, want nil", err)
	}

	out := buf.String()
	for _, want := range []string{
		"community.general 11.5.0",
		"required by:",
		"requirements.yml (root)",
		"depends on:",
		"ansible.posix",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printExplain() output missing %q; got:\n%s", want, out)
		}
	}
}

// TestPrintExplainNotFound checks that a target absent from the lockfile
// returns errExplainNotFound rather than printing anything misleading.
func TestPrintExplainNotFound(t *testing.T) {
	lf := &lockfile.File{SchemaVersion: lockfile.SchemaVersion}
	var buf strings.Builder
	err := printExplain(&buf, lf, "ns.missing", map[string]bool{})
	if err == nil {
		t.Fatal("printExplain() error = nil, want non-nil")
	}
}

// TestPrintExplainSanitizesLockfileText proves printExplain's
// safeout.NewWriter wrap (its first statement) reaches every write its
// helpers (printEntryHeader, printRequiredBy, printDepends) make: every
// rendered field - Name, Version, Source, SHA256, and the one Deps element
// - carries the hostileLockfileName/hostileLockfileSource shape (shared
// with tree_test.go, reusing the adversarial shape at
// internal/galaxy/lockfile/compare_test.go). No raw ESC/CR/NUL byte
// survives anywhere in the output, U+FFFD stands in for each of them, and
// the output stays valid UTF-8. The final assertion - both section headers
// are still present - is the positive control: it proves the writer
// sanitized the hostile text rather than discarding the whole report.
func TestPrintExplainSanitizesLockfileText(t *testing.T) {
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{
				Name:    hostileLockfileName,
				Version: hostileLockfileSource,
				Source:  hostileLockfileSource,
				SHA256:  hostileLockfileSource,
				Deps:    []string{hostileLockfileSource},
			},
		},
	}
	roots := map[string]bool{hostileLockfileName: true}

	var buf strings.Builder
	if err := printExplain(&buf, lf, hostileLockfileName, roots); err != nil {
		t.Fatalf("printExplain() error = %v, want nil", err)
	}
	out := buf.String()

	for _, b := range []byte{0x1b, '\r', 0x00} {
		if strings.IndexByte(out, b) != -1 {
			t.Fatalf("printExplain() output contains raw byte %#x; got:\n%s", b, out)
		}
	}
	if !strings.ContainsRune(out, '\ufffd') {
		t.Fatalf("printExplain() output missing U+FFFD replacement; got:\n%s", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("printExplain() output is not valid UTF-8; got:\n%s", out)
	}
	if !strings.Contains(out, "required by:") || !strings.Contains(out, "depends on:") {
		t.Fatalf("printExplain() output missing its own section headers; got:\n%s", out)
	}
}
