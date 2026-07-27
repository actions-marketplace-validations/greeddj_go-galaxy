package collections_test

// This file (continued from e2e_test.go and warm_e2e_test.go) exercises
// collections.Warm followed by cleanup.Start against the same real pipeline
// the rest of this package's e2e suite drives, proving the two commands
// cooperate correctly on a warm-only cache (no install ever ran): warm's
// extracted trees must survive a cleanup run even though warm never writes
// an InstalledEntry for cleanup's older, install-only keep-set logic to
// find.

import (
	"context"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
)

// TestWarmThenCleanupKeepsExtractedTrees is the end-to-end regression guard
// for this commit's fix. collections.Warm never calls recordInstall, so a
// warm-only project's ansible_collections workspace never exists on disk;
// cleanup.Start's project scan (pickCollectionsPath) skips such a project
// entirely, so it contributes nothing to installedByKey or reachable. Before
// this fix, extractedKeepSet only ever consulted the (now empty)
// InstalledArtifactSHAByKey, so a single Warm followed by a single
// cleanup.Start left 0 entries under <cacheDir>/extracted/ - wiping exactly
// the expensive work warm exists to produce.
func TestWarmThenCleanupKeepsExtractedTrees(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)

	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.libV1.SHA256)

	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.libV1.SHA256)

	// A second cleanup run must be just as non-destructive: SetWarmed's entry
	// persists in the snapshot across a cleanup run - cleanup never consumes
	// or deletes a warmed entry it decides to keep - rather than surviving
	// only once by accident.
	if err := cleanup.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second cleanup.Start: %v", err)
	}
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.appV1.SHA256)
	assertExtractedStorePresent(t, f.cfg.CacheDir, f.libV1.SHA256)

	// A real install against the same cache, after both warm and two cleanup
	// runs, must still succeed and find the extracted trees warm produced
	// still there to hardlink from - proving cleanup did not quietly corrupt
	// or remove anything a later install actually depends on.
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (install after warm+cleanup): %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
}
