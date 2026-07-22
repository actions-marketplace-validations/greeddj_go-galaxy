package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestLoadProjectRegistryRejectsCorruptFile confirms a project registry
// file that fails to decode is reported as an error rather than silently
// replaced by an empty registry, since cleanup uses the registry to decide
// what is still reachable and an empty registry would make it delete
// everything.
//
// cleanup.Start never gets a chance to act on a bad registry in the first
// place: initCleanup (internal/galaxy/cleanup/cleanup.go) calls
// backend.LoadProjectRegistry and returns any error immediately, before
// buildReachable or removeUnused run, so a corrupt registry aborts cleanup
// before any reachability computation or deletion is attempted.
func TestLoadProjectRegistryRejectsCorruptFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRegistryFile(t, dir, []byte("{invalid"))

	_, err := LoadProjectRegistry(dir)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
}

// TestLoadProjectRegistryRejectsTruncatedFile covers a non-empty but
// truncated JSON payload, distinct from the wholly-invalid case above.
func TestLoadProjectRegistryRejectsTruncatedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRegistryFile(t, dir, []byte(`{"projects": {"foo": {"last_run": "2024-01-02T03:04:05Z"`))

	_, err := LoadProjectRegistry(dir)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
}

// TestLoadProjectRegistryMissingFileReturnsEmpty is a regression guard: a
// cache directory that has never recorded a project must still return an
// empty, initialized registry with a nil error.
func TestLoadProjectRegistryMissingFileReturnsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	registry, err := LoadProjectRegistry(dir)
	if err != nil {
		t.Fatalf("expected nil error for a missing registry file, got %v", err)
	}
	if registry == nil || registry.Projects == nil {
		t.Fatalf("expected an initialized empty registry, got %#v", registry)
	}
	if len(registry.Projects) != 0 {
		t.Fatalf("expected no projects, got %#v", registry.Projects)
	}
}

// writeRegistryFile writes raw bytes at the project registry path under
// dir, failing the test on error.
func writeRegistryFile(t *testing.T, dir string, data []byte) {
	t.Helper()
	path := filepath.Join(dir, helpers.StoreDBProjects)
	if err := os.WriteFile(path, data, helpers.FileMod); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}
