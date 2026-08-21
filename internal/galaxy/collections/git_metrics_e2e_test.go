package collections_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// fakeGitBytesPerCollection is what the in-memory client reports as fetched
// for each collection it builds (see fakeGitClient.Acquire).
const fakeGitBytesPerCollection = 1024

// TestGitArtifactMetricsColdInstall pins what a cold git install contributes
// to the run's counters. The fixture's graph is acme.app from git, depending
// on acme.lib from Galaxy, so the expected numbers follow from it:
//
//   - CacheMisses = 2: one for the git artifact committed to the store at
//     discovery time, one for the lib tarball downloaded from Galaxy. The
//     install phase then finds the git artifact already in the store, which
//     is the run's single CacheHit rather than a second miss.
//   - BytesDownloaded = 1024 + len(lib tarball): the fake client reports
//     fakeGitBytesPerCollection per built collection, and the lib bytes are
//     whatever Galaxy served, read back from the cached artifact rather than
//     guessed.
func TestGitArtifactMetricsColdInstall(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n")
	f.mustInstall(t)

	libVersion := readManifestVersion(t, f.downloadPath, "lib")
	libKey := helpers.ArtifactKey(f.galaxy.URL(), acmeArtifactFilename("lib", libVersion))
	libInfo, err := os.Stat(filepath.Join(f.cacheDir, libKey))
	if err != nil {
		t.Fatalf("cached lib artifact: %v", err)
	}
	wantBytes := int64(fakeGitBytesPerCollection) + libInfo.Size()

	totals := f.runtime.Metrics.Totals()
	if totals.CacheMisses != 2 {
		t.Errorf("CacheMisses = %d, want 2 (acme.app built from git and committed once, acme.lib downloaded from Galaxy once)",
			totals.CacheMisses)
	}
	if totals.CacheHits != 1 {
		t.Errorf("CacheHits = %d, want 1 (the install phase serves the git artifact discovery committed)", totals.CacheHits)
	}
	if totals.BytesDownloaded != wantBytes {
		t.Errorf("BytesDownloaded = %d, want %d (%d reported by the fake git client for one built collection + %d bytes of the "+
			"acme.lib tarball served by Galaxy)", totals.BytesDownloaded, wantBytes, fakeGitBytesPerCollection, libInfo.Size())
	}
}

// TestGitDuplicateFQDNIsRefused pins that one fqdn may have one root: the
// same collection reached through two repositories, or through a repository
// and Galaxy, is helpers.ErrDuplicateCollectionRequirement - the refusal two
// Galaxy roots already get - rather than a silent winner.
func TestGitDuplicateFQDNIsRefused(t *testing.T) {
	t.Parallel()
	const mirrorURL = "https://git.example/acme/app-mirror.git"
	f := newGitFixture(t)
	f.git.add(mirrorURL, f.git.repos[gitAppURL])
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+"\n  - git+"+mirrorURL+"\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrDuplicateCollectionRequirement) {
		t.Fatalf("two repositories for acme.app: %v, want ErrDuplicateCollectionRequirement", err)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))

	g := newGitFixture(t)
	g.galaxy.AddVersion("acme", "app", "9.9.9", nil)
	g.writeRequirements(t, "collections:\n  - acme.app\n  - git+"+gitAppURL+"\n")
	if err := g.install(t); !errors.Is(err, helpers.ErrDuplicateCollectionRequirement) {
		t.Fatalf("a git root and a Galaxy root for acme.app: %v, want ErrDuplicateCollectionRequirement", err)
	}
	assertPathAbsent(t, installPathFor(g.downloadPath, "app"))
}
