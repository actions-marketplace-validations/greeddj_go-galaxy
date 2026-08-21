package rolebuild

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

type shapeCase struct {
	name string
	yaml string
	want []gitsource.RoleDependency
}

// shapeCases are the spec shapes role_yaml_parse accepts, with the
// normalized value each one yields.
func shapeCases() []shapeCase {
	return []shapeCase{
		{name: "absent key", yaml: "galaxy_info: {}\n", want: nil},
		{name: "null list", yaml: "dependencies:\n", want: nil},
		{name: "empty list", yaml: "dependencies: []\n", want: []gitsource.RoleDependency{}},
		{
			name: "plain string", yaml: "dependencies:\n  - geerlingguy.docker\n",
			want: []gitsource.RoleDependency{{Src: "geerlingguy.docker"}},
		},
		{
			name: "comma string is not split", yaml: "dependencies:\n  - 'geerlingguy.docker,1.2.3,docker'\n",
			want: []gitsource.RoleDependency{{Src: "geerlingguy.docker,1.2.3,docker"}},
		},
		{
			name: "old style role", yaml: "dependencies:\n  - role: acme.base\n",
			want: []gitsource.RoleDependency{{Src: "acme.base", Name: "acme.base"}},
		},
		{
			name: "old style role with src and version",
			yaml: "dependencies:\n  - role: base\n    src: git+https://example.com/base.git\n    version: v2\n",
			want: []gitsource.RoleDependency{{Src: "git+https://example.com/base.git", Version: "v2", Name: "base"}},
		},
		{
			name: "name only", yaml: "dependencies:\n  - name: acme.base\n",
			want: []gitsource.RoleDependency{{Src: "acme.base", Name: "acme.base"}},
		},
		{
			name: "src only", yaml: "dependencies:\n  - src: https://example.com/base.git\n",
			want: []gitsource.RoleDependency{{Src: "https://example.com/base.git"}},
		},
		{
			name: "every key", yaml: "dependencies:\n  - src: https://example.com/base.git\n    scm: git\n    version: main\n    name: base\n",
			want: []gitsource.RoleDependency{{Src: "https://example.com/base.git", Scm: "git", Version: "main", Name: "base"}},
		},
		{
			name: "role parameters are dropped silently",
			yaml: "dependencies:\n  - role: acme.base\n    when: ansible_os_family == 'Debian'\n    base_port: 8080\n",
			want: []gitsource.RoleDependency{{Src: "acme.base", Name: "acme.base"}},
		},
		{
			name: "number version is the text written", yaml: "dependencies:\n  - src: acme.base\n    version: 2.0\n",
			want: []gitsource.RoleDependency{{Src: "acme.base", Version: "2.0"}},
		},
		{
			name: "null version was not written", yaml: "dependencies:\n  - src: acme.base\n    version: ~\n",
			want: []gitsource.RoleDependency{{Src: "acme.base"}},
		},
		{
			name: "mixed list keeps order", yaml: "dependencies:\n  - acme.first\n  - role: acme.second\n  - src: acme.third\n",
			want: []gitsource.RoleDependency{{Src: "acme.first"}, {Src: "acme.second", Name: "acme.second"}, {Src: "acme.third"}},
		},
	}
}

// TestParseMetaMainDependencyShapes drives every spec shape role_yaml_parse
// accepts through the parser and pins the normalized value each one yields.
func TestParseMetaMainDependencyShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range shapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta, err := ParseMetaMain([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("ParseMetaMain: %v", err)
			}
			if !reflect.DeepEqual(meta.Dependencies, tc.want) {
				t.Fatalf("Dependencies = %#v, want %#v", meta.Dependencies, tc.want)
			}
			if len(meta.Warnings) != 0 {
				t.Fatalf("warnings = %q, want none", meta.Warnings)
			}
		})
	}
}

// TestParseMetaMainRefusals pins every defect reported under
// ErrRoleMetaInvalid and the text that names it.
func TestParseMetaMainRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		yaml     string
		wantText string
	}{
		{name: "document is a list", yaml: "- a\n", wantText: "is not a mapping"},
		{name: "document is a scalar", yaml: "just text\n", wantText: "is not a mapping"},
		{name: "does not parse", yaml: "dependencies: [\n", wantText: "does not parse"},
		{name: "duplicate key", yaml: "dependencies: []\ndependencies: []\n", wantText: "does not parse"},
		{name: "dependencies is a mapping", yaml: "dependencies:\n  acme.base: '1.0'\n", wantText: "must be a list, got a YAML map"},
		{name: "dependencies is a string", yaml: "dependencies: acme.base\n", wantText: "must be a list, got a YAML str"},
		{name: "item is a number", yaml: "dependencies:\n  - 5\n", wantText: "dependencies[0] must be a string or a mapping, got a YAML int"},
		{name: "item is null", yaml: "dependencies:\n  - ~\n", wantText: "dependencies[0] must be a string or a mapping, got a YAML null"},
		{name: "item is a list", yaml: "dependencies:\n  - acme.base\n  - [a, b]\n", wantText: "dependencies[1] must be a string or a mapping"},
		{
			name: "mapping without a role name or src", yaml: "dependencies:\n  - version: 1.0\n",
			wantText: "dependencies[0] names no role, name or src",
		},
		{
			name: "old style role with a comma", yaml: "dependencies:\n  - role: 'acme.base,1.0'\n",
			wantText: "dependencies[0] is an invalid old style role requirement",
		},
		{
			name: "version is a list", yaml: "dependencies:\n  - src: acme.base\n    version: [1, 2]\n",
			wantText: "dependencies[0].version must be a scalar, got a YAML seq",
		},
		{name: "src is a mapping", yaml: "dependencies:\n  - src: {a: b}\n", wantText: "dependencies[0].src must be a scalar, got a YAML map"},
		{name: "duplicate key in an item", yaml: "dependencies:\n  - src: a\n    src: b\n", wantText: "dependencies[0] does not parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseMetaMain([]byte(tc.yaml))
			if !errors.Is(err, helpers.ErrRoleMetaInvalid) {
				t.Fatalf("error = %v, want ErrRoleMetaInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q does not name %q", err.Error(), tc.wantText)
			}
		})
	}
}

func TestParseMetaMainOverTheCap(t *testing.T) {
	t.Parallel()
	data := []byte("# " + strings.Repeat("x", metadataMaxBytes) + "\n")
	_, err := ParseMetaMain(data)
	if !errors.Is(err, helpers.ErrRoleMetaInvalid) || !strings.Contains(err.Error(), "the limit is") {
		t.Fatalf("error = %v, want ErrRoleMetaInvalid naming the cap", err)
	}
	if _, _, err := ParseMetaRequirements(data); !errors.Is(err, helpers.ErrRoleMetaInvalid) {
		t.Fatalf("requirements error = %v, want ErrRoleMetaInvalid", err)
	}
}

// TestParseMetaMainRoleNameAndWarnings covers galaxy_info: the role name is
// read when it is there, and every shape it cannot be read from is a warning
// rather than a refusal, as is an empty meta file.
func TestParseMetaMainRoleNameAndWarnings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		yaml        string
		wantName    string
		wantWarning string
		wantDeps    int
	}{
		{name: "role name read", yaml: "galaxy_info:\n  author: me\n  role_name: docker\ndependencies: [a.b]\n", wantName: "docker", wantDeps: 1},
		{name: "role name absent", yaml: "galaxy_info:\n  author: me\n"},
		{name: "role name null", yaml: "galaxy_info:\n  role_name:\n"},
		{name: "galaxy_info null", yaml: "galaxy_info:\n"},
		{name: "galaxy_info is a list", yaml: "galaxy_info: [a]\n", wantWarning: "galaxy_info is a YAML seq, not a mapping"},
		{name: "galaxy_info is a string", yaml: "galaxy_info: text\n", wantWarning: "galaxy_info is a YAML str, not a mapping"},
		{name: "role name is a list", yaml: "galaxy_info:\n  role_name: [a]\n", wantWarning: "galaxy_info.role_name is a YAML seq, not a string"},
		{name: "galaxy_info with a duplicate key", yaml: "galaxy_info:\n  a: 1\n  a: 2\n", wantWarning: "galaxy_info does not parse"},
		{name: "empty document", yaml: "", wantWarning: "is empty; skipping dependencies"},
		{name: "comment only", yaml: "# nothing\n", wantWarning: "is empty; skipping dependencies"},
		{name: "explicit null document", yaml: "---\n", wantWarning: "is empty; skipping dependencies"},
		{
			name: "name beside role is overridden", yaml: "dependencies:\n  - role: acme.base\n    name: other\n",
			wantWarning: `dependencies[0]: name "other" is ignored in favor of role "acme.base"`, wantDeps: 1,
		},
		{name: "name equal to role is silent", yaml: "dependencies:\n  - role: acme.base\n    name: acme.base\n", wantDeps: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta, err := ParseMetaMain([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("ParseMetaMain: %v", err)
			}
			if meta.RoleName != tc.wantName {
				t.Fatalf("RoleName = %q, want %q", meta.RoleName, tc.wantName)
			}
			if len(meta.Dependencies) != tc.wantDeps {
				t.Fatalf("Dependencies = %#v, want %d", meta.Dependencies, tc.wantDeps)
			}
			joined := strings.Join(meta.Warnings, "\n")
			switch {
			case tc.wantWarning == "" && len(meta.Warnings) != 0:
				t.Fatalf("warnings = %q, want none", meta.Warnings)
			case tc.wantWarning != "" && !strings.Contains(joined, tc.wantWarning):
				t.Fatalf("warnings %q do not name %q", meta.Warnings, tc.wantWarning)
			}
		})
	}
}

func TestParseMetaRequirements(t *testing.T) {
	t.Parallel()
	deps, warnings, err := ParseMetaRequirements([]byte("- acme.one\n- src: https://example.com/two.git\n  name: two\n- role: acme.three\n"))
	if err != nil {
		t.Fatalf("ParseMetaRequirements: %v", err)
	}
	want := []gitsource.RoleDependency{
		{Src: "acme.one"},
		{Src: "https://example.com/two.git", Name: "two"},
		{Src: "acme.three", Name: "acme.three"},
	}
	if !reflect.DeepEqual(deps, want) {
		t.Fatalf("deps = %#v, want %#v", deps, want)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q", warnings)
	}
	for _, empty := range []string{"", "# nothing\n", "---\n", "[]\n"} {
		deps, _, err := ParseMetaRequirements([]byte(empty))
		if err != nil || len(deps) != 0 {
			t.Fatalf("ParseMetaRequirements(%q) = %#v, %v", empty, deps, err)
		}
	}
	refusals := map[string]string{
		"roles:\n  - acme.one\n": "meta/requirements.yml must be a list, got a YAML map",
		"text\n":                 "must be a list, got a YAML str",
		"- 5\n":                  "meta/requirements.yml[0] must be a string or a mapping",
		"- [\n":                  "does not parse",
	}
	for yaml, text := range refusals {
		_, _, err := ParseMetaRequirements([]byte(yaml))
		if !errors.Is(err, helpers.ErrRoleMetaInvalid) || !strings.Contains(err.Error(), text) {
			t.Fatalf("ParseMetaRequirements(%q) = %v, want ErrRoleMetaInvalid naming %q", yaml, err, text)
		}
	}
}
