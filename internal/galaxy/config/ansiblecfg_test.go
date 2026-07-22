package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseAnsibleConfigCase is one table-driven case shared by the
// TestParseAnsibleConfig* functions below.
type parseAnsibleConfigCase struct {
	name  string
	input string
	want  ansibleConfig
}

// runParseAnsibleConfigCases feeds each case's input through
// parseAnsibleConfig and compares the result to want.
func runParseAnsibleConfigCases(t *testing.T, cases []parseAnsibleConfigCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseAnsibleConfig(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parseAnsibleConfig() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("parseAnsibleConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseAnsibleConfigDelimiters checks that both '=' and ':' are
// accepted as key/value delimiters and that the first delimiter in the
// line wins, so a URL value's own colon is never mistaken for one.
func TestParseAnsibleConfigDelimiters(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "unquoted value",
			input: "[defaults]\ncollections_path = ./collections",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "./collections"}},
		},
		{
			name:  "equals separator",
			input: "[galaxy]\nserver = https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "colon separator",
			input: "[galaxy]\nserver : https://x",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "url keeps its colon",
			input: "[galaxy]\nserver = https://example.com:8080/path",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://example.com:8080/path"}},
		},
	})
}

// TestParseAnsibleConfigValueFidelity checks that values are stored
// verbatim: quotes and inline comments are not stripped. ansible.cfg is
// read by CPython's configparser, not a TOML parser, so drop-in fidelity
// requires reproducing that behavior rather than "helpfully" cleaning it up.
func TestParseAnsibleConfigValueFidelity(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "quoted value preserved verbatim",
			input: `[defaults]` + "\n" + `collections_path = "./c"`,
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: `"./c"`}},
		},
		{
			name:  "inline trailing hash comment preserved",
			input: "[galaxy]\nserver = https://x # prod",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x # prod"}},
		},
		{
			name:  "inline trailing semicolon comment preserved",
			input: "[galaxy]\ncache_dir = /c ; note",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{CacheDir: "/c ; note"}},
		},
	})
}

// TestParseAnsibleConfigCommentsAndBlanks checks that full-line comments
// (both '#' and ';' markers, indented or not) and blank lines are skipped
// without affecting subsequently parsed keys.
func TestParseAnsibleConfigCommentsAndBlanks(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "full line hash comment skipped",
			input: "[galaxy]\n# server = https://x\nserver = https://y",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://y"}},
		},
		{
			name:  "full line semicolon comment skipped",
			input: "[galaxy]\n; server = https://x\nserver = https://y",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://y"}},
		},
		{
			name:  "indented full line comment skipped",
			input: "[galaxy]\n   # server = https://x\nserver = https://y",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://y"}},
		},
		{
			name:  "blank lines ignored",
			input: "[galaxy]\n\n\nserver = https://x\n\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
	})
}

// TestParseAnsibleConfigSections checks section- and key-scoping rules:
// keys before any section header, unknown sections, and unknown keys
// within a known section are all ignored; duplicate keys resolve to the
// last occurrence; and section names are matched case-sensitively.
func TestParseAnsibleConfigSections(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "key before any section ignored",
			input: "server = https://x\n[galaxy]\n",
			want:  ansibleConfig{},
		},
		{
			name:  "unknown section ignored",
			input: "[colors]\nhighlight = yes\n",
			want:  ansibleConfig{},
		},
		{
			name:  "unknown key in known section ignored",
			input: "[galaxy]\ntoken = secret\n",
			want:  ansibleConfig{},
		},
		{
			name:  "duplicate key last wins",
			input: "[galaxy]\nserver = https://x\nserver = https://y\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://y"}},
		},
		{
			// Section names are matched case-sensitively; "[Defaults]" is
			// not the same section as "[defaults]".
			name:  "section case is not normalized",
			input: "[Defaults]\ncollections_path = x",
			want:  ansibleConfig{},
		},
	})
}

// TestParseAnsibleConfigLexicalQuirks checks line-ending, BOM, whitespace,
// key-case, and empty-input handling.
func TestParseAnsibleConfigLexicalQuirks(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "crlf line endings",
			input: "[galaxy]\r\nserver = https://x\r\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "leading BOM stripped",
			input: "\uFEFF[defaults]\ncollections_path=x",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "x"}},
		},
		{
			name:  "surrounding whitespace trimmed",
			input: "[defaults]\n  collections_path   =   ./c   ",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "./c"}},
		},
		{
			name:  "key case is normalized",
			input: "[defaults]\nCollections_Path = x",
			want:  ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "x"}},
		},
		{
			name:  "empty input",
			input: "",
			want:  ansibleConfig{},
		},
	})
}

// TestLoadAnsibleConfig checks that loadAnsibleConfig opens and parses a
// real file from disk, and surfaces a wrapped os.ErrNotExist for a missing
// path (the existing swallow-and-continue behavior in
// loadAnsibleConfigFromCLI depends on this).
func TestLoadAnsibleConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ansible.cfg")
	content := "[defaults]\ncollections_path = ./collections\n\n[galaxy]\nserver = https://x\ncache_dir = /c\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v, want nil", err)
	}

	got, gotPath, err := loadAnsibleConfig(path)
	if err != nil {
		t.Fatalf("loadAnsibleConfig() error = %v, want nil", err)
	}
	if gotPath != path {
		t.Errorf("loadAnsibleConfig() path = %q, want %q", gotPath, path)
	}
	want := ansibleConfig{
		Defaults: ansibleDefaultsConfig{CollectionsPath: "./collections"},
		Galaxy:   ansibleGalaxyConfig{Server: "https://x", CacheDir: "/c"},
	}
	if got != want {
		t.Errorf("loadAnsibleConfig() = %+v, want %+v", got, want)
	}

	_, _, err = loadAnsibleConfig(filepath.Join(dir, "missing.cfg"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("loadAnsibleConfig() error = %v, want os.ErrNotExist", err)
	}
}
