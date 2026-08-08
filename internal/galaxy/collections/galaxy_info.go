package collections

import (
	"path"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// galaxyYAMLFileName is the sidecar file name inside a collection's .info
// directory. Both writeGalaxyInfo and matchingInstalledRecord join it onto
// the same target.info; a single constant keeps a typo in either literal from
// silently reproducing the disagreement this file's chokepoint closes.
const galaxyYAMLFileName = "GALAXY.yml"

// GalaxyYAML represents the GALAXY.yml metadata file.
type GalaxyYAML struct {
	DownloadURL string `yaml:"download_url"`
	FormatVer   string `yaml:"format_version"`
	Name        string `yaml:"name"`
	Namespace   string `yaml:"namespace"`
	Server      string `yaml:"server"`
	Signatures  any    `yaml:"signatures"`
	Version     string `yaml:"version"`
	VersionURL  string `yaml:"version_url"`
}

// writeGalaxyInfo writes GALAXY.yml for the installed collection. When meta
// is nil (artifact-cache-hit fast path), a minimal GALAXY.yml is written
// using fields available from the collection identity.
//
// target's identity was already validated once, by newInstallTarget at the
// point installCollection built it - this function trusts that and does not
// re-derive it. The chokepoint call is still the first statement, before
// buildGalaxyYAML and before any other filesystem call: a guard whose
// refusal is deferred past a destructive operation is not a guard, even
// though here the "guard" is target.root itself refusing to traverse a
// symlink planted between newInstallTarget's validation and this call,
// rather than a fresh identity check.
//
// target.info is reset - RemoveAll then MkdirAll, both rooted and classified
// - before the write, exactly like extractCollection resets target.rel: an
// os.Root boundary only stops traversal, it says nothing about what already
// sits at the leaf name inside it. A bare MkdirAll (a no-op when the
// directory already exists) would leave a pre-planted GALAXY.yml at that
// leaf untouched, and the write that follows would go straight through it -
// including through a relative in-root symlink, which os.Root happily
// resolves as long as its target stays inside the root, and including a
// dangling in-root symlink, which the write would silently create the file
// at. Worse, a hardlink at that leaf is written through even when the linked
// inode lives outside the root entirely: os.Root is path-based, so it
// constrains which paths a method may traverse, but a hardlink is not a
// path, it is a second name for an inode the kernel already resolved before
// os.Root was ever involved - there is nothing for Root to see. The reset,
// not the root, is what closes all three: it guarantees the write always
// lands on a name this run just created, the same invariant
// extractCollection already relies on for target.rel, so both sinks now
// share one argument instead of one argument with an unstated exception.
func writeGalaxyInfo(target installTarget, cfg *config.Config, col collection, meta *types.GalaxyCollectionVersionInfo) error {
	if err := target.root.RemoveAll(target.info); err != nil {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	if err := target.root.MkdirAll(target.info, helpers.DirMod); err != nil {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	g := buildGalaxyYAML(cfg, col, meta)
	data, err := yaml.Marshal(&g)
	if err != nil {
		return err
	}
	if err := target.root.WriteFile(path.Join(target.info, galaxyYAMLFileName), data, helpers.FileMod); err != nil {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	return nil
}

// buildGalaxyYAML builds the GALAXY.yml document for col. The identity
// fields (namespace, name, version) always come from col, never from meta:
// col is the resolved identity - the same one the store key, the install
// path, the lockfile, and the artifact key already use - so the file name
// (target.info, derived from col by newInstallTarget) and the file body agree
// by construction rather than by coincidence. meta, when present, contributes
// only the informational fields col has no equivalent for: the download and
// version URLs (with any capability-bearing query string stripped, see
// withoutQuery) and the signatures Galaxy attached to this version.
func buildGalaxyYAML(cfg *config.Config, col collection, meta *types.GalaxyCollectionVersionInfo) GalaxyYAML {
	g := GalaxyYAML{
		FormatVer: "1.0.0",
		Name:      col.Name,
		Namespace: col.Namespace,
		Server:    cfg.Server,
		Version:   col.Version,
	}
	if meta != nil {
		g.DownloadURL = withoutQuery(meta.DownloadURL)
		g.Signatures = meta.Signatures
		g.VersionURL = withoutQuery(meta.Href)
	}
	return g
}

// withoutQuery returns raw up to, but not including, its first "?".
//
// A Galaxy NG or Automation Hub deployment backs its artifacts with object
// storage and answers with a presigned download URL, whose query string is
// a time-limited bearer capability: anyone holding that exact URL can fetch
// the artifact without credentials. GALAXY.yml is written into the
// collections tree, which routinely outlives the run and gets uploaded
// wholesale as a CI artifact, so persisting the query there hands that
// capability to everyone who can read the build's output. The scheme, host
// and path are kept, since they are the informational part - where this
// collection actually came from - and carry no capability on their own.
//
// The cut is textual rather than a url.Parse round trip precisely because
// it cannot fail: the first literal "?" always begins the query, so a URL
// this tool could not parse still gets stripped rather than silently
// written through intact.
func withoutQuery(raw string) string {
	before, _, _ := strings.Cut(raw, "?")
	return before
}
