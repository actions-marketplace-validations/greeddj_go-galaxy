package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// parseAnsibleConfigCase is one table-driven case shared by the
// TestParseAnsibleConfig* functions below.
type parseAnsibleConfigCase struct {
	want  ansibleConfig
	name  string
	input string
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
			// ansibleConfig now carries GalaxyServers, a map field, so it is
			// no longer comparable with !=; reflect.DeepEqual is the
			// equivalent structural check.
			if !reflect.DeepEqual(got, tt.want) {
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

// TestParseAnsibleConfigServerList checks that [galaxy] server_list is
// captured as a plain string, with the same BC2 fidelity (verbatim value,
// last-occurrence-wins) as every other [galaxy] key.
func TestParseAnsibleConfigServerList(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "server_list captured verbatim",
			input: "[galaxy]\nserver_list = prod, staging",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "prod, staging"}},
		},
		{
			name:  "duplicate key last wins",
			input: "[galaxy]\nserver_list = a\nserver_list = b\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "b"}},
		},
	})
}

// TestParseAnsibleConfigGalaxyServerSections checks that every
// [galaxy_server.<id>] section is captured in full into GalaxyServers,
// keyed by the id exactly as written after the dot, with the outer map
// staying nil when no such section is present (the overwhelmingly common
// case must not pay for an allocation it never uses).
func TestParseAnsibleConfigGalaxyServerSections(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name:  "no galaxy_server section: outer map stays nil",
			input: "[galaxy]\nserver = https://x\n",
			want:  ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://x"}},
		},
		{
			name:  "single section, single key",
			input: "[galaxy_server.prod]\nurl = https://prod.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://prod.example"},
			}},
		},
		{
			name: "single section, multiple keys",
			input: "[galaxy_server.prod]\n" +
				"url = https://prod.example\n" +
				"token = abc123\n" +
				"validate_certs = false\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://prod.example", "token": "abc123", "validate_certs": "false"},
			}},
		},
		{
			name: "multiple sections coexist",
			input: "[galaxy_server.prod]\n" +
				"url = https://prod.example\n" +
				"[galaxy_server.staging]\n" +
				"url = https://staging.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod":    {"url": "https://prod.example"},
				"staging": {"url": "https://staging.example"},
			}},
		},
		{
			name: "duplicate key within a section: last wins",
			input: "[galaxy_server.prod]\n" +
				"url = https://one.example\n" +
				"url = https://two.example\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://two.example"},
			}},
		},
		{
			name:  "id containing a dot is captured verbatim",
			input: "[galaxy_server.my.hub]\nurl = https://x\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"my.hub": {"url": "https://x"},
			}},
		},
		{
			// "galaxy_server" without a trailing dot is not a per-server
			// section at all - the fixed prefix requires the dot.
			name:  "bare galaxy_server section without a dot is ignored",
			input: "[galaxy_server]\nurl = https://x\n",
			want:  ansibleConfig{},
		},
	})
}

// TestParseAnsibleConfigGalaxyServerSectionsFidelity checks that a
// [galaxy_server.<id>] section gets the same BC2 fidelity (verbatim
// values, case-sensitive section matching, lowercased keys) as every other
// section this parser tracks.
func TestParseAnsibleConfigGalaxyServerSectionsFidelity(t *testing.T) {
	t.Parallel()
	runParseAnsibleConfigCases(t, []parseAnsibleConfigCase{
		{
			name: "quoted value and inline comment preserved verbatim (BC2 fidelity)",
			input: "[galaxy_server.prod]\n" +
				`url = "https://prod.example"` + "\n" +
				"token = abc123 # comment\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": `"https://prod.example"`, "token": "abc123 # comment"},
			}},
		},
		{
			// Section names (including the galaxy_server.<id> prefix match)
			// are case-sensitive, same as every other section.
			name:  "section name case is not normalized",
			input: "[Galaxy_Server.prod]\nurl = https://x\n",
			want:  ansibleConfig{},
		},
		{
			// Keys within a galaxy_server section are lowercased, same as
			// every other section.
			name:  "keys within galaxy_server section are lowercased",
			input: "[galaxy_server.prod]\nURL = https://x\n",
			want: ansibleConfig{GalaxyServers: map[string]map[string]string{
				"prod": {"url": "https://x"},
			}},
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadAnsibleConfig() = %+v, want %+v", got, want)
	}

	_, _, err = loadAnsibleConfig(filepath.Join(dir, "missing.cfg"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("loadAnsibleConfig() error = %v, want os.ErrNotExist", err)
	}
}
