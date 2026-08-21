package collectionbuild

import (
	"context"
	"os"
)

// GalaxyYML is the build metadata of one collection, read from its galaxy.yml
// or, for a source directory that ships a built tree, from its MANIFEST.json
// collection_info. Field names follow the galaxy.yml keys.
type GalaxyYML struct {
	Dependencies  map[string]string
	Namespace     string
	Name          string
	Version       string
	Readme        string
	Description   string
	LicenseFile   string
	Repository    string
	Documentation string
	Homepage      string
	Issues        string
	Authors       []string
	License       []string
	Tags          []string
	BuildIgnore   []string
}

// Candidate is one collection directory Discover found: its build metadata,
// its subdir relative to the repository root ("" for the root), and whether
// the metadata came from a MANIFEST.json rather than a galaxy.yml (in which
// case the tree is rebuilt from scratch, exactly as ansible-galaxy does for an
// installed tree handed to it as a source).
type Candidate struct {
	Subdir       string
	Meta         GalaxyYML
	FromManifest bool
}

// Built is one artifact Build produced: the identity and raw dependencies
// from its metadata, the subdir it was built from, the temp path of the
// tar.gz, its sha256 computed while writing, the warnings raised along the
// way, and a Cleanup that removes the temp file.
type Built struct {
	Cleanup      func()
	Dependencies map[string]string
	Namespace    string
	Name         string
	Version      string
	Subdir       string
	ArtifactPath string
	SHA256       string
	Warnings     []string
}

// TempFileFunc hands Build the file it writes the artifact into; the cleanup
// removes it. See gitsource.TempFileFunc for why the caller supplies it.
type TempFileFunc func(ctx context.Context) (*os.File, func(), error)
