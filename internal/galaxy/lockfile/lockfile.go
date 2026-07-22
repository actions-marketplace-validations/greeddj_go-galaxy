// Package lockfile reads and writes go-galaxy lockfiles. The lockfile pins
// every transitive collection to an exact version with a SHA256 so CI runs
// are reproducible and hermetic - once a lockfile exists, install only
// reads the cache, never the Galaxy API.
package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"gopkg.in/yaml.v3"
)

var errNilFile = errors.New("lockfile: nil File")

// SchemaVersion is the current lockfile schema. Bumping requires migration.
const SchemaVersion = 1

// DefaultName is the conventional lockfile name beside requirements.yml.
const DefaultName = "requirements.lock.yml"

// Entry is a single pinned collection in the lockfile.
type Entry struct {
	Name    string   `yaml:"name"`
	Version string   `yaml:"version"`
	Source  string   `yaml:"source"`
	SHA256  string   `yaml:"sha256,omitempty"`
	Deps    []string `yaml:"deps,omitempty"`
}

// File is the on-disk lockfile structure.
type File struct {
	Server        string  `yaml:"server,omitempty"`
	Collections   []Entry `yaml:"collections"`
	SchemaVersion int     `yaml:"schema_version"`
}

// ResolveDefaultPath returns the lockfile path. If override is set, that
// path is used. Otherwise DefaultName is placed next to the requirements
// file (or in cwd if requirements path is empty).
func ResolveDefaultPath(requirementsFile, override string) string {
	if override != "" {
		return override
	}
	if requirementsFile == "" {
		return DefaultName
	}
	return filepath.Join(filepath.Dir(requirementsFile), DefaultName)
}

// Load parses a lockfile from disk. Returns (nil, fs.ErrNotExist) if the
// file does not exist so callers can branch on absence.
func Load(path string) (*File, error) {
	//nolint:gosec // path is user-provided lockfile location.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: %s", helpers.ErrLockfileInvalid, err.Error())
	}
	if f.SchemaVersion == 0 {
		return nil, fmt.Errorf("%w: missing schema_version", helpers.ErrLockfileInvalid)
	}
	if f.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: schema_version=%d, supported=%d", helpers.ErrLockfileInvalid, f.SchemaVersion, SchemaVersion)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Save writes the lockfile to disk in canonical form.
func Save(path string, f *File) error {
	if f == nil {
		return errNilFile
	}
	canonicalize(f)
	data, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		return err
	}
	return writeFileAtomic(dir, path, data)
}

// writeFileAtomic writes data to a temporary file in dir and renames it onto
// path. Rename within a single directory is atomic, so a reader sees either
// the previous file or the fully written new one, never a truncated one. The
// temp file is removed on any error so a failed write leaves nothing behind. A
// directory fsync is intentionally omitted: rename atomicity plus the content
// Sync deliver the no-truncated-file guarantee, and a dir-open would add a
// portability wrinkle for no benefit here.
//
//nolint:nonamedreturns // the named err return lets the deferred cleanup see the final error and remove the temp file only on failure.
func writeFileAtomic(dir, path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, DefaultName+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Chmod(helpers.FileMod); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	err = os.Rename(tmpName, path)
	return err
}

// Hash returns a stable SHA256 hex of the canonical lockfile bytes.
func (f *File) Hash() (string, error) {
	clone := f.canonicalClone()
	data, err := yaml.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalClone returns a canonicalized deep copy of f. Only the slices that
// canonicalize sorts are copied - the Collections slice and each entry's Deps
// slice - so Hash stays allocation-light while never mutating the receiver.
func (f *File) canonicalClone() *File {
	clone := *f
	clone.Collections = make([]Entry, len(f.Collections))
	copy(clone.Collections, f.Collections)
	for i := range clone.Collections {
		src := f.Collections[i].Deps
		if len(src) == 0 {
			continue
		}
		deps := make([]string, len(src))
		copy(deps, src)
		clone.Collections[i].Deps = deps
	}
	canonicalize(&clone)
	return &clone
}

// validate rejects lockfiles that are internally inconsistent. Duplicate
// collection names would otherwise let indexLockfile silently drop an entry
// and make a frozen install ambiguous. Save does not call this: buildLockfile
// derives entries from an fqdn-keyed map, so it cannot produce duplicates.
func (f *File) validate() error {
	seen := make(map[string]struct{}, len(f.Collections))
	for _, e := range f.Collections {
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("%w: duplicate collection name %q", helpers.ErrLockfileInvalid, e.Name)
		}
		seen[e.Name] = struct{}{}
	}
	return nil
}

// IsNotExist reports whether err indicates the lockfile is missing.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

func canonicalize(f *File) {
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	sort.Slice(f.Collections, func(i, j int) bool {
		return f.Collections[i].Name < f.Collections[j].Name
	})
	for i := range f.Collections {
		sort.Strings(f.Collections[i].Deps)
	}
}
