package collections

// This file is the end-to-end proof for the second defect a poisoned
// snapshot's resolved version can cause, independent of any lockfile:
// buildResolvedSnapshot (resolve.go) only rejects an entry.Version that is
// empty, deliberately leaving a constraint string like "*" to reach the
// pipeline unchanged - see its own doc comment for why that leniency is
// deliberate rather than an oversight. buildCollectionsMap's own
// helpers.IsExactVersion guard is what actually stops it: a snapshot whose
// Resolved bucket carries "*" for a collection, with a matching Graph
// entry, must never install anything.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// poisonedVersionFixture builds a cold cache directory, a fakegalaxy server
// with acme.widgets@1.0.0 registered, and a requirements.yml requiring
// acme.widgets unpinned.
func poisonedVersionFixture(t *testing.T) (*config.Config, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}
	return cfg, srv
}

// seedResolvedSnapshot writes a persisted store, through the real local
// backend, whose Resolved bucket pins acme.widgets at version with a
// matching Graph entry, and whose Meta.RequirementsHash is computed the
// identical way resolveCollectionsInternal computes it for cfg's own
// requirements file - so a later collections.Start against the same cfg
// takes the snapshot-reuse path (loadResolvedFromSnapshot) instead of
// resolving fresh. version is deliberately unvalidated here: this is the
// seeding mechanism for both the poisoned ("*") and the unpoisoned
// (testVersion100) case, and the point of the poisoned case is that nothing
// upstream of buildCollectionsMap rejects it.
func seedResolvedSnapshot(t *testing.T, runtime *infra.Infra, cfg *config.Config, version string) {
	t.Helper()
	roots, err := loadRoots(cfg, runtime)
	if err != nil {
		t.Fatalf("loadRoots: %v", err)
	}
	reqSpec := buildRequirementsSpec(roots)
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps, serversSignature(cfg))

	st := store.New()
	st.SetResolvedAll(map[string]store.ResolvedEntry{
		"acme.widgets": {Version: version, Source: cfg.Server},
	})
	st.SetGraphSnapshot(map[string][]string{"acme.widgets@" + version: {}})
	st.SetMetaRequirements(reqHash, cfg.Server)
	st.SetRequirements(reqSpec)

	ctx := context.Background()
	backend := local.New(cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()
	if err := backend.SaveStore(ctx, st); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
}

// widgetsManifestPath returns where a real acme.widgets install would land
// its MANIFEST.json under cfg.DownloadPath.
func widgetsManifestPath(cfg *config.Config) string {
	return filepath.Join(cfg.DownloadPath, "ansible_collections", "acme", "widgets", "MANIFEST.json")
}

// TestPoisonedResolvedSnapshotVersionRejectsInstall proves a poisoned
// persisted snapshot's resolved version can never install: seeding the
// exact shape described in this file's own header, then driving the real
// Start entry point, fails with helpers.ErrInvalidCollectionVersion before
// any per-collection install work starts (never joined behind
// helpers.ErrInstallationFailed, so it is not folded into an install-failure
// count), and installs nothing.
//
// TestPoisonedResolvedSnapshotVersionAcceptsInstall below is this test's
// positive control on the same fixture and seeding mechanism: an unpoisoned
// snapshot (a real exact version) installs cleanly.
func TestPoisonedResolvedSnapshotVersionRejectsInstall(t *testing.T) {
	t.Parallel()
	cfg, srv := poisonedVersionFixture(t)
	runtime := infra.New(noopPrinter{}, srv.Client())
	seedResolvedSnapshot(t, runtime, cfg, "*")

	err := Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a poisoned resolved snapshot version, got nil")
	}
	if !errors.Is(err, helpers.ErrInvalidCollectionVersion) {
		t.Fatalf("Start error = %v, want errors.Is helpers.ErrInvalidCollectionVersion", err)
	}
	if errors.Is(err, helpers.ErrInstallationFailed) {
		t.Errorf("Start error = %v, unexpectedly joined behind helpers.ErrInstallationFailed: "+
			"this sentinel is raised while building the resolved identity set, before any "+
			"per-collection worker starts", err)
	}
	if _, statErr := os.Stat(widgetsManifestPath(cfg)); !os.IsNotExist(statErr) {
		t.Fatalf("expected nothing installed, manifest stat err = %v", statErr)
	}
}

// TestPoisonedResolvedSnapshotVersionAcceptsInstall is
// TestPoisonedResolvedSnapshotVersionRejectsInstall's positive control: the
// identical seeding mechanism, with an exact version in place of "*",
// reaches a real install through the same snapshot-reuse path.
func TestPoisonedResolvedSnapshotVersionAcceptsInstall(t *testing.T) {
	t.Parallel()
	cfg, srv := poisonedVersionFixture(t)
	runtime := infra.New(noopPrinter{}, srv.Client())
	seedResolvedSnapshot(t, runtime, cfg, testVersion100)

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, statErr := os.Stat(widgetsManifestPath(cfg)); statErr != nil {
		t.Fatalf("expected acme.widgets installed, manifest stat err = %v", statErr)
	}
}
