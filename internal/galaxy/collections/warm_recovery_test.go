package collections

// This file covers warmOne's corruption-recovery behavior: routing warm
// through the same prepareWithRecovery helper the install path uses means a
// cached tarball that has rotted or been tampered with - so its bytes no
// longer hash to the sha its sidecar (or a lockfile pin) claims - is evicted
// and refetched exactly once, and that an offline run instead surfaces the
// mismatch without evicting the only local copy.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestWarmOneEvictsAndRefetchesCorruptCacheHit is the load-bearing proof that
// warmOne, via prepareWithRecovery, evicts and refetches a cache-hit artifact
// whose bytes do not hash to the sha its sidecar names - the CAS-poisoning
// scenario Ensure's guard now rejects - exactly once, healing both the
// artifact cache and the content-addressable extracted store.
func TestWarmOneEvictsAndRefetchesCorruptCacheHit(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	version := srv.AddVersion("acme", "widgets", "1.0.0", nil)

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	// Not the bytes that produced version.SHA256: this is what makes the
	// cached tarball rotted/tampered even though its sidecar (below) still
	// claims the real sha.
	corruptBytes := []byte("not a gzip stream, despite what the sidecar claims")
	mustWriteFile(t, artifactPath, corruptBytes)
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(version.SHA256))

	cfg := &config.Config{
		Server:       srv.URL(),
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	deps := newTestInstallDepsWithExtractStore(t, cfg)

	// nil meta and a zero handoff: driving warmOne directly stands in for a
	// key the run's prefetcher never scheduled, which is exactly this
	// scenario's shape - a cache hit is never a prefetch task.
	if err := warmOne(context.Background(), deps, col, nil, downloadResult{}); err != nil {
		t.Fatalf("expected the corrupt cache hit to recover via a single refetch, got %v", err)
	}
	if got := srv.Count(fakegalaxy.EndpointArtifact); got != 1 {
		t.Fatalf("EndpointArtifact count = %d, want 1 (a single bounded refetch)", got)
	}

	assertExists(t, filepath.Join(cacheDir, extracted.RootDirName, version.SHA256, extracted.ReadyMarker))
	assertFileSHA256(t, artifactPath, version.SHA256)
	assertFileContent(t, sidecarPath, version.SHA256)
}

// TestWarmOneOfflineCorruptSurfacesMismatch proves that offline, a cache-hit
// artifact whose bytes do not hash to its sidecar's claimed sha - the same
// poisoning scenario as above - surfaces a helpers.ErrSHA256Mismatch through
// warmOne's guard classification without evicting the only local copy, since
// there is no way to refetch it.
func TestWarmOneOfflineCorruptSurfacesMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "gremlins", Version: "1.0.0"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	corruptBytes := []byte("still not a gzip stream, and the sidecar sha does not match either")
	mustWriteFile(t, artifactPath, corruptBytes)
	sidecarSHA := sha256Hex([]byte("bytes the cached tarball does not actually contain"))
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(sidecarSHA))

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      true,
		NoCache:      false,
	}
	deps := newTestInstallDepsWithExtractStore(t, cfg)

	err := warmOne(context.Background(), deps, col, nil, downloadResult{})
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	// No eviction: the offline no-refetch rule must leave the only local copy
	// (tarball and sidecar) untouched.
	assertExists(t, artifactPath)
	assertExists(t, sidecarPath)
}
