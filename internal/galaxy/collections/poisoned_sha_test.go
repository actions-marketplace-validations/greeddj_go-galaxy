package collections

// This file is the end-to-end proof that a poisoned artifact sha256 - a
// traversal string arriving via either of the two reachable sources
// resolveArtifactSHA and canSkipInstall read - never reaches the filesystem
// operations marker.go's markerRel guards, and, for the
// resolveArtifactSHA route, never gets persisted into the snapshot either.
// Every victim file below lives inside this test's own t.TempDir() sandbox,
// standing in for a real path outside the install root: each traversal sha
// is sized to land back inside the sandbox rather than escape it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// TestInstallRejectsPoisonedMetadataSHAOnCacheHit covers the full
// meta.Artifact.Sha256 route. The artifact cache is primed so isCacheHit is
// true, and installCollection is driven with a metaOverride whose
// Artifact.Sha256 is a traversal string - raw Galaxy API JSON a server
// controls.
//
// A traversal sha can never actually complete an install and reach
// recordInstall, in every configuration this pipeline can be in - not
// specifically via writeExtractMarker's hard error: extractCollection also
// fails via archive.ExtractTarGz on a non-gzip tarball (this fixture's own
// shape, seeded below), or via extracted.Ensure when an extract store is
// configured, or via writeExtractMarker's hard error on a valid tarball with
// no extract store. So "no installed entry" (assertion (c)) is a real
// invariant this test still asserts, but it holds regardless of whether
// resolveArtifactSHA's own guard is present, and is kept as a
// non-discriminating invariant guard rather than as proof of this specific
// guard - the discrepancy was found by actually removing the guard and
// observing (c) still pass.
//
// A working fakegalaxy origin is wired in (not just a stub) so the
// discriminating assertions below are meaningful: without
// resolveArtifactSHA's guard, the cache-hit path's extraction failure
// (ExtractTarGz on this fixture's non-gzip bytes) triggers
// prepareWithRecovery's evict-and-refetch recovery, which reaches the origin
// once and only fails there via a separate, unrelated check
// (verifyDownloadSHA comparing the freshly refetched bytes against the same
// poisoned value) - and, on the way, evicts the cached artifact this test
// seeded. With the guard present, none of that happens: rejection is
// immediate, before any refetch. Verified by mutation (guard removed vs
// present):
//
//	                      guard present              guard removed
//	error class           ErrMalformedArtifactSHA256  ErrSHA256Mismatch
//	cached artifact        survives                    evicted
//	origin requests        0                           1
//	installed entry        absent                      also absent
//
// The eviction is the real harm assertion (f) below guards: without the
// guard, evictCorruptCachedArtifact destroys a perfectly good cached
// tarball on the strength of a poisoned metadata entry, and since the
// metadata cache is what is poisoned, the eviction repairs nothing - it
// recurs every run, one destroyed cache entry and one wasted origin round
// trip per poisoned collection, forever. The error class (assertion (e)) is
// the other half: ErrSHA256Mismatch tells an operator two valid digests
// disagree and a refetch may help, when the truth is that the value was
// never a digest at all, sending them to hunt a corrupt artifact that is
// fine.
func TestInstallRejectsPoisonedMetadataSHAOnCacheHit(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	version := srv.AddVersion("acme", "widgets", "1.0.0", nil)

	sandbox := t.TempDir()
	cacheDir := filepath.Join(sandbox, "cache")
	downloadPath := filepath.Join(sandbox, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	// Seeded so artifacts.Has reports true and prepareInstall takes the
	// cache-hit path; its actual content is irrelevant, since a rejected
	// meta.Artifact.Sha256 must fail before this file is ever read for real.
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("cached tarball bytes, irrelevant to this test"))

	// The canary this test's assertion (b) protects: a file outside the
	// install root entirely, at a location the traversal sha below would
	// have reached had marker.go's own guard been the only defense in
	// place - see poisoned_sha_test.go's package doc for the arithmetic.
	victim := filepath.Join(downloadPath, "home", "ci", ".ssh", "authorized_keys")
	const victimContent = "ssh-ed25519 AAAA... ci@legit\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	cfg := &config.Config{
		Server:       srv.URL(),
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()
	artifacts := local.NewArtifacts(cacheDir)
	root := newTestCollectionsRoot(t, downloadPath)
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		root:           root,
	}

	// A real, working DownloadURL is deliberately wired in (not left empty):
	// with the guard removed, prepareWithRecovery's evict-and-refetch
	// recovery actually reaches this origin once, which is exactly the
	// srv.Count assertion below.
	metaOverride := &types.GalaxyCollectionVersionInfo{}
	metaOverride.DownloadURL = srv.URL() + "/download/" + fmt.Sprintf("%s-%s-%s.tar.gz", version.Namespace, version.Name, version.Version)
	metaOverride.Artifact.Sha256 = "../../../../../home/ci/.ssh/authorized_keys"

	err := installCollection(context.Background(), col, deps, nil, metaOverride, downloadResult{})
	if err == nil {
		t.Fatal("(a) expected installCollection to fail on a poisoned meta.Artifact.Sha256, got nil")
	}
	assertFileContent(t, victim, victimContent) // (b)
	if _, ok := st.GetInstalled(col.key()); ok {
		t.Fatal("(c) expected no installed entry: a poisoned sha must never reach the persisted snapshot")
	}
	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 0 {
		t.Errorf("(d) expected zero download attempts (rejected before any refetch), got %d", got)
	}
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Errorf("(e) installCollection error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	// (f): a poisoned metadata entry must never cost a good cached artifact.
	// Without resolveArtifactSHA's guard, the cache-hit extraction failure
	// triggers evictCorruptCachedArtifact, which deletes artifactPath (and
	// its sidecar) on the strength of a value that was never a digest -
	// destroying a real cache entry to "recover" from a metadata problem
	// eviction cannot fix.
	if _, statErr := os.Stat(artifactPath); statErr != nil {
		t.Errorf("(f) expected the cached artifact to survive, stat error: %v", statErr)
	}
}

// TestCanSkipInstallRefusesPoisonedSnapshotSHA covers the first reachable
// route, canSkipInstall reading entry.ArtifactSHA256 straight from a
// (possibly poisoned) persisted snapshot. A real victim is seeded at the
// location the traversal sha would reach, and canSkipInstall must both
// report false (so installCollection reinstalls rather than trusting the
// poisoned record) and never touch the victim on the way to that answer.
//
// The GALAXY.yml sidecar installRecordMatches also checks is seeded here,
// deliberately: without it, installRecordMatches would already return false
// for that unrelated reason, letting a regression in its own
// markerRel guard hide behind the missing sidecar instead of being
// caught by this test.
func TestCanSkipInstallRefusesPoisonedSnapshotSHA(t *testing.T) {
	t.Parallel()
	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	cfg := &config.Config{DownloadPath: downloadPath}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)

	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, "GALAXY.yml"), []byte("format_version: 1.0.0\n"))

	// installPath is DownloadPath/ansible_collections/acme/widgets - three
	// real path elements under downloadPath - so five ".." segments (no
	// leading dot) land exactly at downloadPath itself, which keeps this
	// test's victim inside its own sandbox at this shallower depth.
	const traversalSHA = "../../../../../home/ci/.ssh/authorized_keys"
	victim := filepath.Join(downloadPath, "home", "ci", ".ssh", "authorized_keys")
	const victimContent = "ssh-ed25519 AAAA... ci@legit\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: traversalSHA,
		InstalledAt:    time.Now().UTC(),
	})

	if canSkipInstall(target, col, st, noopPrinter{}) {
		t.Fatal("expected canSkipInstall to refuse a poisoned snapshot sha, not report it as already installed")
	}
	assertFileContent(t, victim, victimContent)
}

// TestInstallRecordMatchesRefusesUnsafeMarkerSHA proves installRecordMatches's
// own markerRel call specifically, independent of canSkipInstall's
// separate verifyExtractMarker backstop: a file is seeded at exactly the
// location a naive filepath.Join(installPath,
// helpers.ExtractMarkerPrefix+entry.ArtifactSHA256) would find present - so
// a bare os.Stat-based check using that join would coincidentally succeed
// and falsely report a match - and installRecordMatches must still return
// false, because it never even reaches that os.Stat call for an unsafe sha.
func TestInstallRecordMatchesRefusesUnsafeMarkerSHA(t *testing.T) {
	t.Parallel()
	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	cfg := &config.Config{DownloadPath: downloadPath}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)

	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, filepath.Join(infoDir, "GALAXY.yml"), []byte("format_version: 1.0.0\n"))

	// The coincidental file: exactly where the old, unguarded
	// filepath.Join(installPath, helpers.ExtractMarkerPrefix+sha) would land
	// for this traversal sha, so a bare os.Stat would find it and (absent
	// the fix) treat it as "marker present".
	const traversalSHA = "../../../../../home/ci/.ssh/authorized_keys"
	coincidental := filepath.Join(downloadPath, "home", "ci", ".ssh", "authorized_keys")
	mustMkdirAll(t, filepath.Dir(coincidental))
	mustWriteFile(t, coincidental, []byte("not actually an extract marker"))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: traversalSHA,
		InstalledAt:    time.Now().UTC(),
	})

	if installRecordMatches(target, col, st) {
		t.Fatal("expected installRecordMatches to refuse an unsafe marker sha rather than coincidentally match an unrelated file")
	}
}
