package cleanup

// This file proves the WIRING of the clean-save skip decorator through
// cleanup.Start's real initCleanup, end to end against a real local backend.
// Nothing in internal/galaxy/store's or internal/galaxy/cache's own unit
// tests for the flag and the decorator drives Start at all, so nothing there
// proves initCleanup actually wraps its backend with
// cacheManager.WithCleanSaveSkip.
//
// Unlike collections.Start, a cleanup run over a genuinely cold cache can
// never be the one to first stamp Meta.LastSnapshot: finalizeCleanup's own
// !cfg.DryRun && st.WasPersisted() guard (see its own doc comment) means the
// very first cleanup run against a cache with no prior persisted snapshot
// always skips the save on its own, regardless of this decorator - cleanup
// never writes an installed entry itself, so removeUnused's Delete* calls
// against an empty in-memory map are no-ops either way. The fixture below
// therefore seeds a persisted snapshot directly (seedSnapshotInstalled)
// before the first real Start call under test, exactly the way a prior real
// install run would have left one behind, so the scenario under test - a
// cleanup run against an already-persisted snapshot that removes nothing -
// is reachable at all.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// reloadCleanupLastSnapshot opens a fresh backend for cfg, loads the
// persisted store, and returns its Meta.LastSnapshot, closing the backend
// before returning. It never reuses a backend Start itself touched, so each
// call is an independent observation of what is actually on disk after that
// run finished.
func reloadCleanupLastSnapshot(t *testing.T, cfg *config.Config, runtime *infra.Infra) time.Time {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("cacheBackend.New: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("backend.Close: %v", err)
		}
	}()

	st, err := backend.LoadStore(t.Context())
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return st.MetaSnapshot().LastSnapshot
}

// TestCleanupThatRemovesNothingDoesNotRewriteTheSnapshot proves the second
// wiring site TestIdleInstallDoesNotRewriteTheSnapshot
// (internal/galaxy/collections) cannot reach: a cleanup run that removes
// nothing must leave the persisted snapshot's LastSnapshot untouched, and a
// cleanup run that does remove something must advance it. ns.name's install
// stays reachable (its project's requirements.yml names it) across the first
// two runs, then the requirements file is rewritten to name nothing, making
// the exact same on-disk collection unreachable for the third run - the
// positive control.
func TestCleanupThatRemovesNothingDoesNotRewriteTheSnapshot(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	writeReachableRequirements(t, reqPath)
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	// Seed a persisted snapshot recording ns.name@1.0.0 as installed, exactly
	// as a prior real install would have left one - see this file's own
	// package doc comment for why cleanup itself can never be the first
	// writer of a persisted snapshot.
	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
		"ns.name@1.0.0": {Source: "https://galaxy.example.com/api", ArtifactSHA256: "deadbeef"},
	})
	seedStamp := reloadCleanupLastSnapshot(t, cfg, runtime)
	if seedStamp.IsZero() {
		t.Fatal("expected a non-zero LastSnapshot after seeding the persisted snapshot")
	}

	firstStamp := runCleanupAndReload(t, cfg, runtime, "first Start (removes nothing, ns.name is reachable)")
	assertLastSnapshotEqual(t, firstStamp, seedStamp, "a no-op cleanup")

	idleStamp := runCleanupAndReload(t, cfg, runtime, "second Start (still removes nothing)")
	assertLastSnapshotEqual(t, idleStamp, seedStamp, "a second no-op cleanup")

	// Positive control: rewriting the project's requirements.yml to name
	// nothing makes ns.name@1.0.0 unreachable, so the third run actually
	// removes it - a real Delete* call, which must advance the stamp.
	writeUnreachableRequirements(t, reqPath)
	changedStamp := runCleanupAndReload(t, cfg, runtime, "third Start (a real removal)")
	if !changedStamp.After(idleStamp) {
		t.Fatalf("LastSnapshot after a real removal = %v, want strictly after %v", changedStamp, idleStamp)
	}

	manifestPath := filepath.Join(downloadPath, "ansible_collections", "ns", "name", "MANIFEST.json")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("expected the now-unreferenced collection to actually be removed, stat error: %v", err)
	}
}

// writeReachableRequirements writes a requirements.yml at path naming
// ns.name, the collection seedInstallTree always seeds - so a project
// pointed at it treats that collection as reachable.
func writeReachableRequirements(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("collections:\n  - name: ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
}

// writeUnreachableRequirements overwrites path with an empty requirements
// file, making whatever it previously named unreachable.
func writeUnreachableRequirements(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("rewrite requirements.yml: %v", err)
	}
}

// runCleanupAndReload runs Start, failing the test with label on error, then
// reloads and returns the persisted snapshot's LastSnapshot. Factored out of
// TestCleanupThatRemovesNothingDoesNotRewriteTheSnapshot purely to stay under
// this repository's cyclomatic-complexity budget.
func runCleanupAndReload(t *testing.T, cfg *config.Config, runtime *infra.Infra, label string) time.Time {
	t.Helper()
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	return reloadCleanupLastSnapshot(t, cfg, runtime)
}

// assertLastSnapshotEqual fails the test unless got equals want, naming label
// in the failure message.
func assertLastSnapshotEqual(t *testing.T, got, want time.Time, label string) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("LastSnapshot after %s = %v, want unchanged from %v", label, got, want)
	}
}
