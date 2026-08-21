package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestLoadProjectRegistryRestoresANullProjectsMap pins the loader's own
// postcondition - on a successful return, Projects is never nil - against a
// shape the missing-file branch never reaches: the file exists, decodes
// cleanly, and carries an explicit JSON null under the projects key. Callers
// read that map straight back from the load, cleanup's reachability pass
// among them.
//
// Deleting the registry.Projects = ensureMap(registry.Projects) line from
// LoadProjectRegistry fails the null subtest with:
//
//	expected an initialized registry, got &store.ProjectRegistry{Projects:map[string]store.ProjectRecord(nil)}
//
// The populated subtest keeps passing under that same mutation, and that is
// its job: it proves the fixture reaches the decode at all rather than
// tripping the missing-file branch TestLoadProjectRegistryMissingFileReturnsEmpty
// already covers.
func TestLoadProjectRegistryRestoresANullProjectsMap(t *testing.T) {
	t.Parallel()

	t.Run("explicit null projects map", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeRegistryFile(t, dir, []byte(`{"projects": null}`))

		registry, err := LoadProjectRegistry(dir)
		if err != nil {
			t.Fatalf("expected nil error for a null projects map, got %v", err)
		}
		if registry == nil || registry.Projects == nil {
			t.Fatalf("expected an initialized registry, got %#v", registry)
		}
		if len(registry.Projects) != 0 {
			t.Fatalf("expected no projects, got %#v", registry.Projects)
		}
	})

	t.Run("populated projects map", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeRegistryFile(t, dir, []byte(`{"projects": {"/p": {"requirements_file": "/p/requirements.yml", `+
			`"collections_path": "/p/collections", "last_run": "2024-01-02T03:04:05Z"}}}`))

		registry, err := LoadProjectRegistry(dir)
		if err != nil {
			t.Fatalf("expected nil error for a populated registry, got %v", err)
		}
		record, ok := registry.Projects["/p"]
		if !ok {
			t.Fatalf("expected an entry under /p, got %#v", registry.Projects)
		}
		if record.RequirementsFile != "/p/requirements.yml" {
			t.Fatalf("expected the decoded requirements file, got %q", record.RequirementsFile)
		}
	})
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

// TestLoadProjectRegistryWithoutRolesPathDecodesEmpty proves a registry an
// older binary wrote - no roles_path key at all - decodes with RolesPath "",
// the value cleanup reads as "no roles path recorded, do not scan". The
// registry is unversioned, so this absent-key shape is the only migration
// path there is, and it must stay the conservative one.
func TestLoadProjectRegistryWithoutRolesPathDecodesEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRegistryFile(t, dir, []byte(`{"projects": {"/p": {"requirements_file": "/p/requirements.yml", `+
		`"collections_path": "/p/collections", "last_run": "2024-01-02T03:04:05Z"}}}`))

	registry, err := LoadProjectRegistry(dir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	record, ok := registry.Projects["/p"]
	if !ok {
		t.Fatalf("expected an entry under /p, got %#v", registry.Projects)
	}
	if record.RolesPath != "" {
		t.Fatalf("RolesPath = %q, want empty for a record written without one", record.RolesPath)
	}
	if record.CollectionsPath != "/p/collections" {
		t.Fatalf("CollectionsPath = %q, want /p/collections", record.CollectionsPath)
	}
}

// TestRecordProjectWritesRolesPath proves RecordProject resolves the roles
// path against the project directory by the same rule as the collections
// path - relative joins, absolute stays - and that it survives a reload.
func TestRecordProjectWritesRolesPath(t *testing.T) {
	t.Parallel()

	t.Run("relative", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		projectDir := t.TempDir()
		reqPath := filepath.Join(projectDir, "requirements.yml")

		if err := RecordProject(cacheDir, reqPath, "collections", "roles"); err != nil {
			t.Fatalf("RecordProject: %v", err)
		}
		record := mustLoadProjectRecord(t, cacheDir, projectDir)
		if want := filepath.Join(projectDir, "roles"); record.RolesPath != want {
			t.Fatalf("RolesPath = %q, want %q", record.RolesPath, want)
		}
		if want := filepath.Join(projectDir, "collections"); record.CollectionsPath != want {
			t.Fatalf("CollectionsPath = %q, want %q", record.CollectionsPath, want)
		}
	})

	t.Run("absolute", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		projectDir := t.TempDir()
		rolesDir := t.TempDir()
		reqPath := filepath.Join(projectDir, "requirements.yml")

		if err := RecordProject(cacheDir, reqPath, "collections", rolesDir); err != nil {
			t.Fatalf("RecordProject: %v", err)
		}
		if record := mustLoadProjectRecord(t, cacheDir, projectDir); record.RolesPath != rolesDir {
			t.Fatalf("RolesPath = %q, want %q", record.RolesPath, rolesDir)
		}
	})
}

// TestRecordProjectWithoutRolesPathStaysLegacyShape proves omitempty holds: a
// collections-only record marshals byte for byte as it did before RolesPath
// existed, so a registry no run ever gave a roles path is indistinguishable
// from one an older binary wrote.
func TestRecordProjectWithoutRolesPathStaysLegacyShape(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	projectDir := t.TempDir()
	reqPath := filepath.Join(projectDir, "requirements.yml")

	if err := RecordProject(cacheDir, reqPath, "collections", ""); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}
	record := mustLoadProjectRecord(t, cacheDir, projectDir)
	if record.RolesPath != "" {
		t.Fatalf("RolesPath = %q, want empty", record.RolesPath)
	}

	// legacyRecord is the record's shape before RolesPath existed, tag for
	// tag; the two encodings must agree byte for byte.
	type legacyRecord struct {
		LastRun          time.Time `json:"last_run"`
		RequirementsFile string    `json:"requirements_file"`
		CollectionsPath  string    `json:"collections_path"`
	}
	got, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	want, err := json.Marshal(legacyRecord{
		LastRun:          record.LastRun,
		RequirementsFile: record.RequirementsFile,
		CollectionsPath:  record.CollectionsPath,
	})
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("a collections-only record changed shape:\n got %s\nwant %s", got, want)
	}
	if bytes.Contains(got, []byte("roles_path")) {
		t.Fatalf("a collections-only record carries roles_path: %s", got)
	}
}

// mustLoadProjectRecord reloads the registry under cacheDir and returns the
// record keyed by projectDir, failing the test when it is absent.
func mustLoadProjectRecord(t *testing.T, cacheDir, projectDir string) ProjectRecord {
	t.Helper()
	registry, err := LoadProjectRegistry(cacheDir)
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	record, ok := registry.Projects[projectDir]
	if !ok {
		t.Fatalf("no record under %q, got %#v", projectDir, registry.Projects)
	}
	return record
}
