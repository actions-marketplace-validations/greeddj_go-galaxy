package collections

import (
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// collection represents a resolved collection with metadata. For a Galaxy
// collection Source is the server base that owns it; for a git collection it
// is the gitsource locator (git+<url>#<subdir>@<commit>) and for a url
// collection the urlsource locator (url+<url>#sha256:<hex>), which is what
// every downstream consumer keys on - the artifact cache, the installed
// record, the resolved snapshot - so a commit or content change reads as a
// source change everywhere without any of them knowing what the source kind
// is. Type survives resolution for every kind; Ref is set only for a git
// collection and is the ref the requirements file asked for, carried to the
// lockfile.
type collection struct {
	Namespace  string `yaml:"namespace"`
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	Source     string `yaml:"source"`
	Constraint string `yaml:"-"`
	Type       string `yaml:"-"`
	Ref        string `yaml:"-"`
	// SHA256 is the artifact pin the install enforces byte for byte: the
	// frozen-lockfile pin (see materializeLockfile), and for a url collection
	// the locator's own digest, stamped on every resolution. It is a
	// runtime-only value and is never (de)serialized.
	SHA256     string   `yaml:"-"`
	Signatures []string `yaml:"signatures"`
}

const (
	typeGalaxy = "galaxy"
	typeGit    = "git"
	typeURL    = "url"
)

// fqdn returns the collection's namespace.name.
func (c collection) fqdn() string {
	return c.Namespace + "." + c.Name
}

// key returns the unique key for the collection.
func (c collection) key() string {
	return fmt.Sprintf("%s.%s@%s", c.Namespace, c.Name, c.Version)
}

// isGit reports whether the collection comes from a git source. It reads the
// Source prefix rather than Type because Type is not carried through every
// path a collection value travels (a lockfile entry, a snapshot entry), while
// the locator is.
func (c collection) isGit() bool {
	return gitsource.IsLocator(c.Source)
}

// gitLocator parses the collection's locator. It is only meaningful when
// isGit reports true.
func (c collection) gitLocator() (gitsource.Locator, error) {
	return gitsource.ParseLocator(c.Source)
}

// isURL reports whether the collection comes from a url source, by the same
// Source-prefix dispatch isGit uses and for the same reason.
func (c collection) isURL() bool {
	return urlsource.IsLocator(c.Source)
}

// urlLocator parses the collection's locator. It is only meaningful when
// isURL reports true.
func (c collection) urlLocator() (urlsource.Locator, error) {
	return urlsource.ParseLocator(c.Source)
}
