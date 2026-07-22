package cleanup

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// noopPrinter is a minimal output.Printer stub for tests that need an Infra
// but do not care about the rendered progress output. output.Printer
// implementations are package-private test doubles rather than a shared
// helper, so this duplicates the identical type already used by
// internal/galaxy/collections/lock_pin_test.go instead of trying to share it
// across packages.
type noopPrinter struct{}

func (noopPrinter) Printf(string, ...any)                 {}
func (noopPrinter) PersistentPrintf(string, ...any)       {}
func (noopPrinter) Okf(string, ...any)                    {}
func (noopPrinter) Errorf(string, ...any)                 {}
func (noopPrinter) Warnf(string, ...any)                  {}
func (noopPrinter) Debugf(string, ...any)                 {}
func (noopPrinter) DebugSincef(time.Time, string, ...any) {}

// testManifestJSON is a minimal, valid MANIFEST.json body for ns.name@1.0.0.
// scanInstalledCollections/buildInstalledRecord only read collection_info's
// namespace, name, and version to build an installedCollection record, so
// the other manifest fields are omitted.
const testManifestJSON = `{
	"collection_info": {
		"namespace": "ns",
		"name": "name",
		"version": "1.0.0"
	}
}`

// TestStartAbortsOnCorruptRegistryBeforeDeletion proves that a project
// registry file which fails to decode aborts cleanup.Start with a wrapped
// helpers.ErrCorruptProjectRegistry, through the real local cache backend,
// lock, and Bolt store - not just at the LoadProjectRegistry unit level.
//
// A registry file has to be unparseable JSON to exercise this path at all,
// so it cannot also encode a valid project entry pointing at the seeded
// install tree in the same file: initCleanup calls
// backend.LoadProjectRegistry and returns its error immediately, before
// state.registry is ever read by Start, so buildReachable/removeUnused are
// structurally unreachable here regardless of what the install tree
// contains - there is no code path between a corrupt-registry error and any
// deletion logic. The install tree is still seeded so "it still exists
// after Start" is a real assertion against on-disk state (not a vacuous
// one), and so this test proves the full production pipeline - cache
// backend selection, lock acquisition, store load, project registry load,
// error propagation - reports the failure through Start's return value
// rather than silently continuing to delete anything.
func TestStartAbortsOnCorruptRegistryBeforeDeletion(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	writeCorruptRegistry(t, cacheDir)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
	assertManifestSurvives(t, installDir)
}

// TestStartTakesNoOpPathOnEmptyRegistry proves that a missing (equivalently,
// empty) project registry takes cleanup.Start's explicit no-op branch
// (len(state.registry.Projects) == 0) rather than attempting any
// reachability computation or deletion, again through the real local cache
// backend rather than a unit-level check.
//
// As with the corrupt-registry case, zero recorded projects means
// buildReachable/removeUnused never run regardless of what is on disk, so
// this proves the guard fires rather than that a specific collection was
// individually preserved by reachability logic. Per the architect's
// instruction, this test intentionally does not assert anything about lock
// or backend release: Start's early no-op return does not currently release
// the lock or close the backend (tracked separately), and asserting on that
// here would either fail today or bake the leak in as expected behavior.
func TestStartTakesNoOpPathOnEmptyRegistry(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected nil error for an empty registry, got %v", err)
	}
	assertManifestSurvives(t, installDir)
}

// newTestRuntime builds an Infra wired with a no-op printer and the default
// HTTP client, matching the pattern used by the collections package's own
// tests for constructing an Infra without caring about rendered output.
func newTestRuntime() *infra.Infra {
	return infra.New(noopPrinter{}, http.DefaultClient)
}

// seedInstallTree creates a MANIFEST.json for ns.name@1.0.0 under
// <downloadPath>/ansible_collections/ns/name, mirroring the on-disk layout
// scanInstalledCollections walks and removeInstalled deletes. It returns
// the manifest's directory (the install path a real deletion would remove).
func seedInstallTree(t *testing.T, downloadPath string) string {
	t.Helper()
	installDir := filepath.Join(downloadPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir: %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}
	return installDir
}

// writeCorruptRegistry writes unparseable bytes directly at the project
// registry path, bypassing store.RecordProject (which only ever round-trips
// valid JSON) so the file is corrupt from the very first read.
func writeCorruptRegistry(t *testing.T, cacheDir string) {
	t.Helper()
	path := filepath.Join(cacheDir, helpers.StoreDBProjects)
	if err := os.WriteFile(path, []byte("{invalid"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write corrupt registry: %v", err)
	}
}

// assertManifestSurvives fails the test unless installDir's MANIFEST.json
// is still present, proving no deletion touched it.
func assertManifestSurvives(t *testing.T, installDir string) {
	t.Helper()
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected the pre-seeded install tree to survive, stat error: %v", err)
	}
}
