package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ProjectRecord describes a project and its last run metadata.
//
// The registry JSON carries no schema version and is decoded by
// encoding/json, which ignores a field it does not know, so a binary
// predating a field reads a registry that carries it without complaint.
// The cost runs the other way: that older binary re-recording the same
// project writes the record without the field, and the field's reader must
// treat its absence as the conservative answer.
type ProjectRecord struct {
	LastRun          time.Time `json:"last_run"`
	RequirementsFile string    `json:"requirements_file"`
	CollectionsPath  string    `json:"collections_path"`
	// RolesPath is the absolute directory the project's roles install into,
	// or "" when the run configured none. omitempty keeps a collections-only
	// record byte-identical to what every earlier binary wrote. An older
	// binary re-recording this project drops the field, which cleanup reads
	// as "no roles path recorded, do not scan it" - the direction that can
	// only make a destructive pass do less, never more.
	RolesPath string `json:"roles_path,omitempty"`
}

// ProjectRegistry stores known projects keyed by path.
type ProjectRegistry struct {
	Projects map[string]ProjectRecord `json:"projects"`
}

// RecordProject records or updates a project entry in the registry.
func RecordProject(cacheDir, requirementsFile, downloadPath, rolesPath string) error {
	if cacheDir == "" {
		return nil
	}
	projectPath, record := NewProjectRecord(requirementsFile, downloadPath, rolesPath)

	registry, err := LoadProjectRegistry(cacheDir)
	if err != nil {
		return err
	}
	// Defensive only and unreachable today: LoadProjectRegistry initializes
	// Projects on every successful exit, so the write below already has a map
	// to write into. It is kept so this function stands on its own rather than
	// resting on that postcondition holding forever.
	registry.Projects = ensureMap(registry.Projects)
	registry.Projects[projectPath] = record
	return saveProjectRegistry(cacheDir, registry)
}

// NewProjectRecord builds the registry entry a run records, stamped with the
// current time, and returns it with the project path it is keyed by: the
// directory of the absolute requirements file. The collections and roles
// paths are resolved against that directory by one rule (see
// resolveProjectPath), so the two cannot drift. Both backends build their
// record through this function rather than each assembling its own, which
// is what keeps the local registry file and the S3 registry object the same
// shape.
func NewProjectRecord(requirementsFile, downloadPath, rolesPath string) (string, ProjectRecord) {
	absReq, err := filepath.Abs(requirementsFile)
	if err != nil {
		absReq = requirementsFile
	}
	projectPath := filepath.Dir(absReq)
	return projectPath, ProjectRecord{
		RequirementsFile: absReq,
		CollectionsPath:  resolveProjectPath(projectPath, downloadPath),
		RolesPath:        resolveProjectPath(projectPath, rolesPath),
		LastRun:          time.Now().UTC(),
	}
}

// LoadProjectRegistry loads the project registry from cacheDir. A missing
// file is treated as an empty, freshly-initialized registry, but a file
// that exists and fails to decode is reported as an error rather than
// silently replaced by an empty registry: cleanup relies on the registry to
// compute which installed collections are still reachable, so an empty
// registry would make it believe nothing is reachable and delete
// everything.
//
// On every successful return Projects is non-nil, whether the file was
// absent, decoded into entries, or decoded a projects key that was an
// explicit JSON null.
func LoadProjectRegistry(cacheDir string) (*ProjectRegistry, error) {
	path := projectRegistryPath(cacheDir)
	//nolint:gosec // path is derived from cacheDir and is intended for project registry IO.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectRegistry{Projects: make(map[string]ProjectRecord)}, nil
		}
		return nil, err
	}
	var registry ProjectRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("%w at %s: %w (remove the file or clear the cache to rebuild the registry)",
			helpers.ErrCorruptProjectRegistry, path, err)
	}
	registry.Projects = ensureMap(registry.Projects)
	return &registry, nil
}

// saveProjectRegistry writes the registry atomically to disk.
func saveProjectRegistry(cacheDir string, registry *ProjectRegistry) error {
	if registry == nil {
		return nil
	}
	path := projectRegistryPath(cacheDir)
	if err := os.MkdirAll(filepath.Dir(path), helpers.DirMod); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".projects-")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(payload); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// projectRegistryPath returns the registry path under cacheDir.
func projectRegistryPath(cacheDir string) string {
	return filepath.Join(cacheDir, helpers.StoreDBProjects)
}

// resolveProjectPath returns p as an absolute path for a project: an
// absolute p is returned as is, a relative one is joined under projectPath,
// and an empty one stays empty so "not configured" survives the round trip
// rather than turning into the project directory itself. The collections
// path and the roles path both go through it.
func resolveProjectPath(projectPath, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(projectPath, p)
}
