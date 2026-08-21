package collections_test

// This file (continued from e2e_test.go) covers the non-pinned cache-hit
// path's use of the local artifact cache's sha256 sidecar: a warm reinstall
// must serve entirely from the extracted-tree cache and record the sidecar's
// original digest, never a hash of tarball bytes that have since been
// corrupted on disk.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// corruptedTarballContent replaces a cached tarball on disk in this file's
// tests: deliberately not a valid gzip+tar stream, so any code path that
// still tried to read it as an artifact (a re-hash, or a direct extraction)
// would fail loudly rather than silently producing different bytes.
const corruptedTarballContent = "this is not a valid tar.gz artifact - the cached tarball was corrupted on disk"

// acmeArtifactFilename returns the on-disk cache filename for one "acme"
// collection version's tarball (every e2e fixture's namespace). It mirrors the collections package's own
// unexported artifactKey (filename plus url.QueryEscape) so this external
// test package can locate - and deliberately corrupt - the cached artifact
// file directly.
func acmeArtifactFilename(name, version string) string {
	return url.QueryEscape(fmt.Sprintf("acme-%s-%s.tar.gz", name, version))
}

// loadInstalledEntry opens cfg's cache backend, loads its persisted
// snapshot, and returns the installed entry recorded for key, failing the
// test if the entry is absent. It always closes the backend before
// returning, so it never holds the backend's lock past this call - safe to
// use only after any collections.Start run that touched the same cache
// directory has already completed and released its own lock.
func loadInstalledEntry(t *testing.T, cfg *config.Config, runtime *infra.Infra, key string) store.InstalledEntry {
	t.Helper()
	ctx := context.Background()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("cacheBackend.New: %v", err)
	}
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() {
		_ = backend.Close(ctx)
	}()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	entry, ok := st.GetInstalled(key)
	if !ok {
		t.Fatalf("no installed entry recorded for %s", key)
	}
	return entry
}

// TestWarmInstallCacheHitDoesNotRehashCorruptedTarball is the load-bearing
// proof that a non-pinned cache hit trusts the sha256 sidecar written next
// to a cached tarball instead of re-hashing the whole file: it corrupts the
// cached tarball's bytes on disk while leaving the sidecar and the extracted
// store's warm tree untouched, wipes the install workspace (and with it the
// .extract-done marker) to force a fresh extraction, and reinstalls without
// any lockfile pin. The reinstall must succeed - served entirely from the
// warm extracted tree, never reading the corrupted tarball bytes - and must
// record the ORIGINAL sidecar sha256. If the code re-hashed the corrupted
// tarball instead of trusting the sidecar, it would either fail outright (the
// corrupted bytes are not a valid gzip stream) or record a different sha256,
// either of which this test catches.
func TestWarmInstallCacheHitDoesNotRehashCorruptedTarball(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache, sidecar, and extracted store): %v", err)
	}

	tarballPath := filepath.Join(f.cfg.CacheDir, acmeArtifactFilename("lib", "1.0.0"))
	if err := os.WriteFile(tarballPath, []byte(corruptedTarballContent), helpers.FileMod); err != nil {
		t.Fatalf("corrupt the cached tarball: %v", err)
	}

	f.server.ResetCounts()
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("wipe the install workspace (and its .extract-done marker): %v", err)
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("reinstall from the corrupted-tarball cache hit: %v", err)
	}

	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := f.server.Total(); got != 0 {
		t.Errorf("Total() after the reinstall = %d, want 0 (served from the warm cache/extracted tree, not the network)", got)
	}

	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.lib@1.0.0")
	if entry.ArtifactSHA256 != f.libV1.SHA256 {
		t.Fatalf(
			"recorded ArtifactSHA256 = %q, want the original sidecar sha %q (a re-hash of the corrupted tarball would never produce this value)",
			entry.ArtifactSHA256, f.libV1.SHA256,
		)
	}
}
