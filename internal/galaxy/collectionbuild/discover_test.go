package collectionbuild

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const manifestOnlyJSON = `{"collection_info": {"namespace": "acme", "name": "built", "version": "3.0.0",
 "readme": "README.md", "authors": ["a"], "dependencies": {}}, "format": 1}`

func galaxyYMLFor(name string) string {
	return "namespace: acme\nname: " + name + "\nversion: 1.0.0\nreadme: README.md\nauthors: [a]\n"
}

type discoverCase struct {
	wantErr      error
	source       func() *memSource
	name         string
	subdir       string
	wantMessage  string
	wantSubdirs  []string
	wantWarnings []string
	wantManifest []string
}

func discoverCases() []discoverCase {
	return slices.Concat(discoverFoundCases(), discoverNotFoundCases(), discoverRefusedCases())
}

// discoverFoundCases are trees discovery answers with candidates.
func discoverFoundCases() []discoverCase {
	return []discoverCase{
		{
			name:        "root galaxy.yml",
			source:      func() *memSource { return newMemSource().file("galaxy.yml", minimalGalaxyYML).file("README.md", "r") },
			wantSubdirs: []string{""},
		},
		{
			name: "root galaxy.yml wins over children",
			source: func() *memSource {
				return newMemSource().file("galaxy.yml", minimalGalaxyYML).file("child/galaxy.yml", galaxyYMLFor("child"))
			},
			wantSubdirs: []string{""},
		},
		{
			name:        "subdir",
			source:      func() *memSource { return newMemSource().file("collections/app/galaxy.yml", minimalGalaxyYML) },
			subdir:      "collections/app",
			wantSubdirs: []string{"collections/app"},
		},
		{
			name: "immediate children in tree order",
			source: func() *memSource {
				return newMemSource().
					file("README.md", "top").
					file("zeta/galaxy.yml", galaxyYMLFor("zeta")).
					file("alpha/galaxy.yml", galaxyYMLFor("alpha")).
					file("plain/README.md", "no metadata").
					file(".hidden/galaxy.yml", galaxyYMLFor("hidden"))
			},
			wantSubdirs: []string{"alpha", "zeta"},
		},
		{
			name: "children under a subdir",
			source: func() *memSource {
				return newMemSource().
					file("ns/a/galaxy.yml", galaxyYMLFor("a")).
					file("ns/b/MANIFEST.json", manifestOnlyJSON)
			},
			subdir:       "ns",
			wantSubdirs:  []string{"ns/a", "ns/b"},
			wantManifest: []string{"ns/b"},
		},
		{
			name: "manifest only",
			source: func() *memSource {
				return newMemSource().file("MANIFEST.json", manifestOnlyJSON).file("FILES.json", "{}")
			},
			wantSubdirs:  []string{""},
			wantManifest: []string{""},
		},
		{
			name:         "warnings carry the directory",
			source:       func() *memSource { return newMemSource().file("c/galaxy.yml", minimalGalaxyYML+"extra: 1\n") },
			wantSubdirs:  []string{"c"},
			wantWarnings: []string{"c: Found unknown keys in galaxy.yml: extra"},
		},
	}
}

// discoverNotFoundCases are trees and subdirs holding no collection.
func discoverNotFoundCases() []discoverCase {
	return []discoverCase{
		{
			name:    "grandchild only is not found",
			source:  func() *memSource { return newMemSource().file("ns/app/galaxy.yml", minimalGalaxyYML) },
			wantErr: helpers.ErrGitCollectionNotFound,
		},
		{
			name:    "nothing at all",
			source:  func() *memSource { return newMemSource().file("README.md", "r") },
			wantErr: helpers.ErrGitCollectionNotFound,
		},
		{
			name:    "galaxy.yaml spelling is not metadata",
			source:  func() *memSource { return newMemSource().file("galaxy.yaml", minimalGalaxyYML) },
			wantErr: helpers.ErrGitCollectionNotFound,
		},
		{
			name:    "galaxy.yml as a directory is not metadata",
			source:  func() *memSource { return newMemSource().dir("galaxy.yml") },
			wantErr: helpers.ErrGitCollectionNotFound,
		},
		{
			name:    "subdir missing",
			source:  func() *memSource { return newMemSource().file("galaxy.yml", minimalGalaxyYML) },
			subdir:  "nope/deeper",
			wantErr: helpers.ErrGitCollectionNotFound, wantMessage: "nope does not exist",
		},
		{
			name:    "subdir component is a file",
			source:  func() *memSource { return newMemSource().file("README.md", "r") },
			subdir:  "README.md",
			wantErr: helpers.ErrGitCollectionNotFound, wantMessage: "not a directory",
		},
		{
			name:    "subdir with dot-dot",
			source:  func() *memSource { return newMemSource().file("galaxy.yml", minimalGalaxyYML) },
			subdir:  "../x",
			wantErr: helpers.ErrGitCollectionNotFound,
		},
	}
}

// discoverRefusedCases are trees discovery refuses for what they declare.
func discoverRefusedCases() []discoverCase {
	return []discoverCase{
		{
			name: "both metadata files",
			source: func() *memSource {
				return newMemSource().file("galaxy.yml", minimalGalaxyYML).file("MANIFEST.json", manifestOnlyJSON)
			},
			wantErr: helpers.ErrGalaxyYMLInvalid, wantMessage: "has both a MANIFEST.json and a galaxy.yml",
		},
		{
			name: "both metadata files in a child",
			source: func() *memSource {
				return newMemSource().file("c/galaxy.yml", minimalGalaxyYML).file("c/MANIFEST.json", manifestOnlyJSON)
			},
			wantErr: helpers.ErrGalaxyYMLInvalid,
		},
		{
			name: "duplicate identity",
			source: func() *memSource {
				return newMemSource().file("a/galaxy.yml", minimalGalaxyYML).file("b/galaxy.yml", minimalGalaxyYML)
			},
			wantErr: helpers.ErrGitDuplicateCollection, wantMessage: "acme.app is declared by both a and b",
		},
		{
			name:    "invalid galaxy.yml names its directory",
			source:  func() *memSource { return newMemSource().file("c/galaxy.yml", "namespace: acme\n") },
			wantErr: helpers.ErrGalaxyYMLInvalid, wantMessage: "c: ",
		},
		{
			name: "version not exact",
			source: func() *memSource {
				return newMemSource().file("galaxy.yml", strings.Replace(minimalGalaxyYML, "1.2.3", "1.x", 1))
			},
			wantErr: helpers.ErrGitCollectionVersionNotExact,
		},
		{
			name:    "metadata over the cap",
			source:  func() *memSource { return newMemSource().file("galaxy.yml", strings.Repeat("#", metadataMaxBytes+1)) },
			wantErr: helpers.ErrGalaxyYMLInvalid, wantMessage: "larger than",
		},
	}
}

func TestDiscover(t *testing.T) {
	t.Parallel()
	for _, tt := range discoverCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cands, warnings, err := Discover(tt.source(), tt.subdir)
			if tt.wantErr != nil {
				assertRefused(t, err, tt.wantErr, tt.wantMessage)
				return
			}
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			subdirs := make([]string, 0, len(cands))
			for _, c := range cands {
				subdirs = append(subdirs, c.Subdir)
				if want := slices.Contains(tt.wantManifest, c.Subdir); c.FromManifest != want {
					t.Fatalf("FromManifest = %v for %q, want %v", c.FromManifest, c.Subdir, want)
				}
			}
			if strings.Join(subdirs, "|") != strings.Join(tt.wantSubdirs, "|") {
				t.Fatalf("subdirs = %q, want %q", subdirs, tt.wantSubdirs)
			}
			if strings.Join(warnings, "|") != strings.Join(tt.wantWarnings, "|") {
				t.Fatalf("warnings = %q, want %q", warnings, tt.wantWarnings)
			}
		})
	}
}

// assertRefused checks err against the sentinel and the message fragment a
// refusing case names.
func assertRefused(t *testing.T, err, want error, fragment string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if fragment != "" && !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error %q does not mention %q", err, fragment)
	}
}

func TestDiscoverControlRunesInSubdirAreCleaned(t *testing.T) {
	t.Parallel()
	_, _, err := Discover(newMemSource().file("galaxy.yml", minimalGalaxyYML), "a\x1b[31mb")
	if !errors.Is(err, helpers.ErrGitCollectionNotFound) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("error %q carries an escape byte", err)
	}
}
