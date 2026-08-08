package commands

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// hostileLockfileName and hostileLockfileSource mirror the adversarial
// shape at internal/galaxy/lockfile/compare_test.go's own
// hostileEntryName/hostileEntrySource: a path-traversal name and a value
// carrying a NUL byte, an ANSI escape, and a CRLF. A lockfile is data this
// printer trusts no more than the S3 error text safeout.Clean was written
// for - a hand-edited or otherwise untrusted lockfile can carry the same
// bytes into any of an Entry's string fields.
const (
	hostileLockfileName   = "../../../../etc/passwd"
	hostileLockfileSource = "https://x.example\x00\x1b[31m\r\nInstalled: totally.fine"
)

// TestPrintTree checks that the header line prints the actual requirements
// path passed in (not a hardcoded "requirements.yml"), and that the tree body
// reflects the dependency structure.
func TestPrintTree(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "community.general", Version: "11.5.0", Deps: []string{"ansible.posix"}},
			{Name: "ansible.posix", Version: "2.0.0"},
			{Name: "ansible.utils", Version: "6.0.2"},
		},
	}
	roots := []string{"community.general", "ansible.utils"}
	reqPath := "custom/dir/req.yml"

	var buf strings.Builder
	printTree(&buf, reqPath, lf, roots)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("printTree produced no output")
	}
	if lines[0] != reqPath {
		t.Errorf("first output line = %q, want %q", lines[0], reqPath)
	}

	out := buf.String()
	wantRows := []string{
		"community.general 11.5.0",
		"ansible.posix 2.0.0",
		"ansible.utils 6.0.2",
	}
	for _, want := range wantRows {
		if !strings.Contains(out, want) {
			t.Errorf("printTree() output missing row %q; got:\n%s", want, out)
		}
	}
}

// TestPrintTreeMissingDependency checks that a dependency absent from the
// lockfile is flagged inline instead of being silently skipped.
func TestPrintTreeMissingDependency(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "community.general", Version: "11.5.0", Deps: []string{"ns.ghost"}},
		},
	}
	roots := []string{"community.general"}

	var buf strings.Builder
	printTree(&buf, "requirements.yml", lf, roots)

	if !strings.Contains(buf.String(), "ns.ghost (missing in lockfile)") {
		t.Errorf("printTree() output missing dependency marker; got:\n%s", buf.String())
	}
}

// TestPrintTreeSanitizesLockfileText proves printTree's safeout.NewWriter
// wrap (its first statement) reaches every write walkTree makes: no raw
// ESC/CR/NUL byte survives anywhere in the output, U+FFFD stands in for
// each of them, and the output stays valid UTF-8. The root entry's Name
// carries the path-traversal shape and its one dependency carries the
// control-byte shape, so both the root line and the "(missing in
// lockfile)" branch each render a hostile value. The final assertion - the
// tree's own branch decoration is still present - is the positive control:
// it proves the writer sanitized the hostile text rather than discarding
// the whole line or the whole tree.
func TestPrintTreeSanitizesLockfileText(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: hostileLockfileName, Version: "1.0.0", Deps: []string{hostileLockfileSource}},
		},
	}
	roots := []string{hostileLockfileName}

	var buf strings.Builder
	printTree(&buf, "requirements.yml", lf, roots)
	out := buf.String()

	for _, b := range []byte{0x1b, '\r', 0x00} {
		if strings.IndexByte(out, b) != -1 {
			t.Fatalf("printTree() output contains raw byte %#x; got:\n%s", b, out)
		}
	}
	if !strings.ContainsRune(out, '\ufffd') {
		t.Fatalf("printTree() output missing U+FFFD replacement; got:\n%s", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("printTree() output is not valid UTF-8; got:\n%s", out)
	}
	if !strings.Contains(out, "\u2514\u2500\u2500 ") {
		t.Fatalf("printTree() output missing its own branch decoration; got:\n%s", out)
	}
}
