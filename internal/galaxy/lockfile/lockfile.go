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
	// Server records the Galaxy server this lockfile was generated against -
	// provenance for a human reviewing a committed lockfile, never a source
	// of truth an entry's own resolution defers to: indexLockfile defaults a
	// source-less entry's Source from the consuming RUN's own cfg.Server, not
	// from this field. It is nonetheless part of the file's identity: Hash
	// covers it and Compare reports a change to it via Diff.Server, so a
	// server_list reorder or edit that changes the effective default server
	// counts as drift the same way a changed pin does, even when every
	// collection entry is otherwise untouched.
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

// Load parses a lockfile from disk. Every error it returns means exactly one
// of two things, and the two are mutually exclusive by construction: either
// the file is not there (IsNotExist(err) is true - the original
// fs.ErrNotExist, or an OS-specific variant like ENOTDIR that also satisfies
// errors.Is(err, fs.ErrNotExist)), or the file is there and unusable in some
// way (errors.Is(err, helpers.ErrLockfileInvalid) is true - unreadable,
// unparseable, or internally inconsistent). Three consumers depend on that
// dichotomy being exhaustive: resolveOrLoadLockfile and lockFrozen both
// classify --frozen's behavior by which arm holds, and lockDryRunBaseline
// treats anything that is not IsNotExist the same way (warn, then proceed as
// if no baseline existed) - none of the three have a third case to fall
// into.
func Load(path string) (*File, error) {
	//nolint:gosec // path is user-provided lockfile location.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		// Wrapped with %s, not %w: the fs.ErrNotExist guard above already
		// makes this arm and the one above mutually exclusive, so %w buys no
		// additional exclusivity here. %s is chosen instead because it
		// matches the sibling YAML-unmarshal arm two lines below, and
		// because nothing in this program branches on the underlying errno,
		// so keeping it reachable through errors.Is would buy nothing.
		// err.Error() still carries the real OS error text for a human
		// reading the message; it is simply not reachable through errors.Is.
		return nil, fmt.Errorf("%w: %s", helpers.ErrLockfileInvalid, err.Error())
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

// Save writes the lockfile to disk in canonical form. It canonicalizes a copy
// so a caller that keeps using f after Save never sees its entries reordered.
func Save(path string, f *File) error {
	if f == nil {
		return errNilFile
	}
	data, err := yaml.Marshal(f.canonicalClone())
	if err != nil {
		return err
	}
	return helpers.WriteFileAtomic(path, data)
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

// validate rejects lockfiles that are internally inconsistent, on two
// independent grounds: a duplicate collection name, which would otherwise
// let indexLockfile silently drop an entry and make a frozen install
// ambiguous, and an entry whose pinned version is not helpers.IsExactVersion
// - a constraint string like "*" rather than a version anything can install.
// The latter closes the route a --frozen install would otherwise take when a
// lockfile entry carried an unresolved constraint: resolveFromLockfile would
// build a collection from it, exactVersionFromConstraints would treat it as
// unpinned, and the run would silently install the server's highest version
// under a path built from the literal constraint text. Save does not call
// this, and the two shapes it would have caught are ruled out at the
// producer by two different mechanisms, not one: buildLockfile carries its
// own helpers.IsExactVersion guard over the resolved map it builds a File
// from, so it cannot hand Save a non-exact version; a duplicate name simply
// cannot arise in the first place, because that same map is keyed by fqdn,
// so two entries can never share a name to begin with.
func (f *File) validate() error {
	seen := make(map[string]struct{}, len(f.Collections))
	for _, e := range f.Collections {
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("%w: duplicate collection name %q", helpers.ErrLockfileInvalid, e.Name)
		}
		seen[e.Name] = struct{}{}
		if !helpers.IsExactVersion(e.Version) {
			return fmt.Errorf("%w: %s: version %q is not an exact version", helpers.ErrLockfileInvalid, e.Name, e.Version)
		}
	}
	return nil
}

// IsNotExist reports whether err indicates the lockfile is missing.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

// LoadRequired is Load for a caller that cannot proceed without the lockfile:
// an absent file becomes helpers.ErrLockfileMissing naming the path, instead
// of the bare fs.ErrNotExist Load returns.
//
// The distinction it draws is between commands, not between failures. "The
// lockfile you asked me to read is not there" is a fact about the lockfile,
// which is why it classifies as the lockfile exit class; reaching a command
// through a bare fs.ErrNotExist instead lands it in the environment-usage
// class, which is where the same absence used to send `tree`, `explain` and
// `outdated` while it already sent `install --frozen` and its two siblings to
// the lockfile class. Every command that requires a lockfile goes through
// here, so the classification is a property of the loader rather than
// something each call site has to remember.
//
// hash is the one deliberate exception and does not call this: a missing
// lockfile there is not a failure at all, since it falls back to hashing the
// requirements file for repositories that do not lock. It stays on Load and
// branches on IsNotExist itself.
func LoadRequired(path string) (*File, error) {
	f, err := Load(path)
	switch {
	case err == nil:
		return f, nil
	case IsNotExist(err):
		return nil, fmt.Errorf("%w: %s", helpers.ErrLockfileMissing, path)
	default:
		return nil, err
	}
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
