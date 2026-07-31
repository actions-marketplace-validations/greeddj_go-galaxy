package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Artifacts implements ArtifactStore for filesystem-backed artifacts.
type Artifacts struct {
	cacheDir string
}

// NewArtifacts returns a local artifact store rooted at cacheDir.
func NewArtifacts(cacheDir string) *Artifacts {
	return &Artifacts{cacheDir: cacheDir}
}

// Has reports whether the artifact exists in the local cache.
func (s *Artifacts) Has(_ context.Context, key string) (bool, error) {
	path, err := s.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Fetch returns a cached artifact file by key. When a sidecar written by a
// prior Commit holds a validly-shaped sha256 digest, it is surfaced via Meta
// so a non-pinned cache hit can reuse it instead of re-hashing the whole
// tarball. Any other sidecar state - missing, a torn/short write, or
// non-hex content - yields Meta == nil, which sends the caller down the
// hash-the-file fallback instead of trusting unverifiable bytes.
func (s *Artifacts) Fetch(_ context.Context, key string) (cacheManager.ArtifactFile, error) {
	path, err := s.path(key)
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	result := cacheManager.ArtifactFile{Path: path}
	//nolint:gosec // path is derived from the process-controlled artifact key, not user input.
	if data, readErr := os.ReadFile(path + helpers.ArtifactSHASidecarSuffix); readErr == nil {
		if sha := strings.TrimSpace(string(data)); helpers.IsSHA256Hex(sha) {
			result.Meta = map[string]string{"sha256": sha}
		}
	}
	return result, nil
}

// TempFile creates a temporary file for staging an artifact.
func (s *Artifacts) TempFile(_ context.Context, prefix string) (*os.File, func(), error) {
	dir, err := s.dir()
	if err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	return file, cleanup, nil
}

// Commit moves a temporary artifact into its final cache location. When meta
// carries a validly-shaped sha256 digest - as a freshly downloaded and
// verified artifact does - Commit also persists it to a sidecar file next to
// the tarball, so a later non-pinned cache hit can reuse it via Fetch
// instead of re-hashing the whole tarball. A sidecar write failure is
// swallowed rather than failing Commit: the artifact itself is already
// committed by the time the sidecar is written, and a missing sidecar just
// falls back to hashing on the next Fetch, so surfacing the write error here
// would turn a perf-only miss into a hard install failure.
func (s *Artifacts) Commit(_ context.Context, key, tmpPath string, meta map[string]string) (cacheManager.ArtifactFile, error) {
	path, err := s.path(key)
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	result := cacheManager.ArtifactFile{Path: path}
	if sha := strings.TrimSpace(meta["sha256"]); helpers.IsSHA256Hex(sha) {
		_ = os.WriteFile(path+helpers.ArtifactSHASidecarSuffix, []byte(sha), helpers.FileMod)
		result.Meta = map[string]string{"sha256": sha}
	}
	return result, nil
}

// Delete removes an artifact from the local cache, along with its sha256
// sidecar (if any), so eviction never leaves a sidecar orphaned next to a
// tarball that no longer exists.
func (s *Artifacts) Delete(_ context.Context, key string) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(path + helpers.ArtifactSHASidecarSuffix); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// dir returns the base cache directory for artifacts.
func (s *Artifacts) dir() (string, error) {
	trimmed := strings.TrimSpace(s.cacheDir)
	if trimmed == "" {
		return "", helpers.ErrCacheDirEmpty
	}
	return trimmed, nil
}

// path builds the full artifact path for a key.
func (s *Artifacts) path(key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", errArtifactKeyEmpty
	}
	dir, err := s.dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key), nil
}
