package collectionbuild

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

type galaxyYMLCase struct {
	wantErr     error
	check       func(t *testing.T, meta GalaxyYML)
	name        string
	yml         string
	wantMessage string
	wantWarning string
}

func acceptedGalaxyYMLCases() []galaxyYMLCase {
	return []galaxyYMLCase{
		{name: "minimal", yml: minimalGalaxyYML, check: checkMinimalMeta},
		{
			name: "full",
			yml: minimalGalaxyYML + "description: An app\nlicense:\n  - MIT\nlicense_file: LICENSE\ntags: [web, app]\n" +
				"dependencies:\n  acme.base: '>=1.0.0'\n  other.thing: '*'\nrepository: https://r\ndocumentation: https://d\n" +
				"homepage: https://h\nissues: https://i\nbuild_ignore:\n  - '*.bak'\n  - changelogs/fragments\n",
			check: checkFullMeta,
		},
		{
			name:  "scalar wrapped into list",
			yml:   "namespace: acme\nname: app\nversion: 1.2.3\nreadme: README.md\nauthors: Solo\ntags: one\n",
			check: expectLists("Solo", "one"),
		},
		{
			name:  "null string keys read as empty",
			yml:   minimalGalaxyYML + "description:\nlicense_file: ~\ndependencies:\nlicense:\n",
			check: checkNullsReadAsEmpty,
		},
		{
			name:        "unknown keys warn sorted",
			yml:         minimalGalaxyYML + "zeta: 1\nalpha: 2\n",
			wantWarning: "Found unknown keys in galaxy.yml: alpha, zeta",
		},
		{
			name:  "version with prerelease",
			yml:   "namespace: acme\nname: app\nversion: 1.2.3-rc.1+build\nreadme: README.md\nauthors: [x]\n",
			check: expectVersion("1.2.3-rc.1+build"),
		},
	}
}

// identityOf renders the four identity strings for a one-line comparison.
func identityOf(meta GalaxyYML) string {
	return strings.Join([]string{meta.Namespace, meta.Name, meta.Version, meta.Readme}, "|")
}

func checkMinimalMeta(t *testing.T, meta GalaxyYML) {
	t.Helper()
	if got := identityOf(meta); got != "acme|app|1.2.3|README.md" {
		t.Fatalf("identity = %q", got)
	}
	if strings.Join(meta.Authors, ",") != "A. Author" {
		t.Fatalf("authors = %q", meta.Authors)
	}
	for name, list := range map[string][]string{"license": meta.License, "tags": meta.Tags, "build_ignore": meta.BuildIgnore} {
		if list == nil || len(list) != 0 {
			t.Fatalf("%s = %#v, want an empty non-nil list", name, list)
		}
	}
	if meta.Dependencies == nil || len(meta.Dependencies) != 0 {
		t.Fatalf("dependencies = %#v, want an empty non-nil map", meta.Dependencies)
	}
}

func checkFullMeta(t *testing.T, meta GalaxyYML) {
	t.Helper()
	strs := strings.Join([]string{meta.Description, meta.LicenseFile, meta.Repository, meta.Documentation, meta.Homepage, meta.Issues}, "|")
	if strs != "An app|LICENSE|https://r|https://d|https://h|https://i" {
		t.Fatalf("strings = %q", strs)
	}
	lists := strings.Join(meta.Tags, ",") + "|" + strings.Join(meta.License, ",") + "|" + strings.Join(meta.BuildIgnore, ",")
	if lists != "web,app|MIT|*.bak,changelogs/fragments" {
		t.Fatalf("lists = %q", lists)
	}
	if meta.Dependencies["acme.base"] != baseConstraint || meta.Dependencies["other.thing"] != "*" {
		t.Fatalf("dependencies = %v", meta.Dependencies)
	}
}

func expectLists(authors, tags string) func(t *testing.T, meta GalaxyYML) {
	return func(t *testing.T, meta GalaxyYML) {
		t.Helper()
		if strings.Join(meta.Authors, ",") != authors || strings.Join(meta.Tags, ",") != tags {
			t.Fatalf("lists = %+v", meta)
		}
	}
}

func checkNullsReadAsEmpty(t *testing.T, meta GalaxyYML) {
	t.Helper()
	if meta.Description != "" || meta.LicenseFile != "" || len(meta.Dependencies) != 0 || len(meta.License) != 0 {
		t.Fatalf("meta = %+v", meta)
	}
}

func expectVersion(version string) func(t *testing.T, meta GalaxyYML) {
	return func(t *testing.T, meta GalaxyYML) {
		t.Helper()
		if meta.Version != version {
			t.Fatalf("version = %q", meta.Version)
		}
	}
}

func refusedGalaxyYMLCases() []galaxyYMLCase {
	return []galaxyYMLCase{
		{name: "not yaml", yml: "namespace: [", wantErr: helpers.ErrGalaxyYMLInvalid, wantMessage: "does not parse"},
		{name: "a list", yml: "- a\n- b\n", wantErr: helpers.ErrGalaxyYMLInvalid},
		{name: "empty document", yml: "", wantErr: helpers.ErrGalaxyYMLInvalid, wantMessage: "not a mapping"},
		{name: "scalar document", yml: "hello\n", wantErr: helpers.ErrGalaxyYMLInvalid},
		{name: "over the cap", yml: "namespace: " + strings.Repeat("a", metadataMaxBytes), wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "the limit is"},
		{name: "manifest key", yml: minimalGalaxyYML + "manifest:\n  directives: []\n", wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "use build_ignore"},
		{name: "manifest key null", yml: minimalGalaxyYML + "manifest:\n", wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "use build_ignore"},
		{name: "missing mandatory keys", yml: "namespace: acme\nversion: 1.0.0\n", wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "missing the following mandatory keys: name, readme, authors"},
		{name: "missing version", yml: "namespace: acme\nname: app\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrGitCollectionVersionNotExact, wantMessage: versionRemedy},
		{name: "null version", yml: "namespace: acme\nname: app\nversion:\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrGitCollectionVersionNotExact, wantMessage: versionRemedy},
		{name: "empty version", yml: "namespace: acme\nname: app\nversion: ''\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrGitCollectionVersionNotExact, wantMessage: versionRemedy},
		{name: "float version", yml: "namespace: acme\nname: app\nversion: 1.0\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrGitCollectionVersionNotExact, wantMessage: "float64"},
		{name: "int version", yml: "namespace: acme\nname: app\nversion: 1\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrGitCollectionVersionNotExact, wantMessage: "quote it"},
		{name: "not a version", yml: "namespace: acme\nname: app\nversion: latest\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrGitCollectionVersionNotExact, wantMessage: versionRemedy},
		{name: "namespace alphabet", yml: "namespace: Acme\nname: app\nversion: 1.0.0\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrInvalidCollectionName},
		{name: "name alphabet", yml: "namespace: acme\nname: my-app\nversion: 1.0.0\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrInvalidCollectionName},
		{name: "null namespace", yml: "namespace:\nname: app\nversion: 1.0.0\nreadme: R\nauthors: [a]\n",
			wantErr: helpers.ErrInvalidCollectionName},
		{name: "string key wrong type", yml: minimalGalaxyYML + "description: [a]\n", wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "description must be a string"},
		{name: "list key wrong type", yml: minimalGalaxyYML + "tags: {a: b}\n", wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "tags must be a list"},
		{name: "list element wrong type", yml: minimalGalaxyYML + "tags: [a, 1]\n", wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "tags[1] must be a string"},
		{name: "dependencies not a mapping", yml: minimalGalaxyYML + "dependencies: [a.b]\n", wantErr: helpers.ErrGalaxyYMLInvalid},
		{name: "dependency constraint number", yml: minimalGalaxyYML + "dependencies:\n  acme.base: 1.0\n",
			wantErr: helpers.ErrGalaxyYMLInvalid, wantMessage: "constraint of dependency"},
		{name: "dependency key", yml: minimalGalaxyYML + "dependencies:\n  not-a-name: '*'\n", wantErr: helpers.ErrInvalidDependencyKey},
		{name: "dependency key three parts", yml: minimalGalaxyYML + "dependencies:\n  a.b.c: '*'\n", wantErr: helpers.ErrInvalidDependencyKey},
		{name: "build_ignore empty entry", yml: minimalGalaxyYML + "build_ignore: ['']\n", wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "build_ignore[0] is empty"},
		{name: "build_ignore too long", yml: minimalGalaxyYML + "build_ignore: ['" + strings.Repeat("x", buildIgnoreMaxLen+1) + "']\n",
			wantErr: helpers.ErrGalaxyYMLInvalid, wantMessage: "build_ignore[0] is"},
		{name: "build_ignore NUL", yml: minimalGalaxyYML + "build_ignore: [\"a\\0b\"]\n", wantErr: helpers.ErrGalaxyYMLInvalid,
			wantMessage: "NUL"},
		{name: "build_ignore element not a string", yml: minimalGalaxyYML + "build_ignore: [1]\n", wantErr: helpers.ErrGalaxyYMLInvalid},
		{name: "duplicate key", yml: minimalGalaxyYML + "name: other\n", wantErr: helpers.ErrGalaxyYMLInvalid},
	}
}

func TestParseGalaxyYMLAccepted(t *testing.T) {
	t.Parallel()
	for _, tt := range acceptedGalaxyYMLCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			meta, warnings, err := ParseGalaxyYML([]byte(tt.yml))
			if err != nil {
				t.Fatalf("ParseGalaxyYML: %v", err)
			}
			if tt.check != nil {
				tt.check(t, meta)
			}
			var want []string
			if tt.wantWarning != "" {
				want = []string{tt.wantWarning}
			}
			if strings.Join(warnings, "|") != strings.Join(want, "|") {
				t.Fatalf("warnings = %q, want %q", warnings, want)
			}
		})
	}
}

func TestParseGalaxyYMLRefused(t *testing.T) {
	t.Parallel()
	for _, tt := range refusedGalaxyYMLCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ParseGalaxyYML([]byte(tt.yml))
			assertRefused(t, err, tt.wantErr, tt.wantMessage)
		})
	}
}

type manifestInfoCase struct {
	wantErr error
	name    string
	json    string
}

func manifestInfoCases() []manifestInfoCase {
	return []manifestInfoCase{
		{name: "not json", json: "{", wantErr: helpers.ErrGalaxyYMLInvalid},
		{name: "no collection_info", json: `{"format": 1}`, wantErr: helpers.ErrGalaxyYMLInvalid},
		{name: "no version", json: `{"collection_info": {"namespace": "acme", "name": "app"}}`,
			wantErr: helpers.ErrGitCollectionVersionNotExact},
		{name: "loose version", json: `{"collection_info": {"namespace": "acme", "name": "app", "version": "latest"}}`,
			wantErr: helpers.ErrGitCollectionVersionNotExact},
		{name: "bad name", json: `{"collection_info": {"namespace": "acme", "name": "App", "version": "1.0.0"}}`,
			wantErr: helpers.ErrInvalidCollectionName},
		{name: "wrong type", json: `{"collection_info": {"namespace": "acme", "name": "app", "version": 1}}`,
			wantErr: helpers.ErrGalaxyYMLInvalid},
		{name: "bad dependency key", json: `{"collection_info": {"namespace": "acme", "name": "app", "version": "1.0.0",
			"dependencies": {"x": "*"}}}`, wantErr: helpers.ErrInvalidDependencyKey},
		{name: "over the cap", json: `{"collection_info": {"namespace": "acme", "name": "app", "version": "1.0.0",
			"description": "` + strings.Repeat("a", metadataMaxBytes) + `"}}`, wantErr: helpers.ErrGalaxyYMLInvalid},
	}
}

func TestParseManifestInfoAccepted(t *testing.T) {
	t.Parallel()
	meta, err := parseManifestInfo([]byte(`{"collection_info": {"namespace": "acme", "name": "app", "version": "2.0.0",
			"readme": "README.md", "authors": ["a"], "license_file": null, "dependencies": {"acme.base": ">=1.0.0"},
			"tags": ["t"]}, "format": 1}`))
	if err != nil {
		t.Fatalf("parseManifestInfo: %v", err)
	}
	if identityOf(meta) != "acme|app|2.0.0|README.md" || meta.LicenseFile != "" {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Dependencies["acme.base"] != baseConstraint || len(meta.BuildIgnore) != 0 || meta.BuildIgnore == nil || meta.License == nil {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestParseManifestInfoRefused(t *testing.T) {
	t.Parallel()
	for _, tt := range manifestInfoCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseManifestInfo([]byte(tt.json))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("parseManifestInfo error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
