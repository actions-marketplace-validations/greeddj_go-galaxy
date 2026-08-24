package collections

import (
	"errors"
	"io/fs"
	"path"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// galaxyYAMLFileName is the sidecar file name inside a collection's .info
// directory. Both writeGalaxyInfo and matchingInstalledRecord join it onto
// the same target.info; a single constant keeps a typo in either literal from
// silently reproducing the disagreement this file's chokepoint closes.
const galaxyYAMLFileName = "GALAXY.yml"

// GalaxyYAML represents the GALAXY.yml metadata file. For a collection built
// from a git source, Server names the repository URL (credential-free by
// construction) rather than a Galaxy server, the two URL fields and the
// signatures are empty, and GitCommit records the commit the tree was built
// from; ansible-galaxy writes no sidecar at all for a git install, so the
// extra key is this tool's provenance, and ansible ignores it.
type GalaxyYAML struct {
	DownloadURL string `yaml:"download_url"`
	FormatVer   string `yaml:"format_version"`
	Name        string `yaml:"name"`
	Namespace   string `yaml:"namespace"`
	Server      string `yaml:"server"`
	Signatures  any    `yaml:"signatures"`
	Version     string `yaml:"version"`
	VersionURL  string `yaml:"version_url"`
	GitCommit   string `yaml:"git_commit,omitempty"`
	// URLSHA256 records a url collection's origin sha256, provenance the way
	// GitCommit is for a git collection; ansible ignores both.
	URLSHA256 string `yaml:"url_sha256,omitempty"`
}

// describes reports whether this document names the very collection col is,
// which is what the install-skip check asks of a sidecar before it accepts
// one as evidence of an install. The three identity fields are exactly what
// buildGalaxyYAML fills from col, so a document this tool wrote for col
// always passes and a document written for anything else - or a truncated
// one, whose fields are empty - always fails.
func (g GalaxyYAML) describes(col collection) bool {
	return g.Namespace == col.Namespace && g.Name == col.Name && g.Version == col.Version
}

// readGalaxyInfo reads and parses the sidecar beside target. It reports
// false for every outcome that is not a parseable document - absent,
// unreadable, not a regular file, or malformed - since none of them is
// evidence of anything about the collection.
func readGalaxyInfo(target installTarget) (GalaxyYAML, bool) {
	data, ok := readRegularFile(target.root, path.Join(target.info, galaxyYAMLFileName))
	if !ok {
		return GalaxyYAML{}, false
	}
	var doc GalaxyYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return GalaxyYAML{}, false
	}
	return doc, true
}

// reconcileGalaxyInfo repairs the one field of an already-installed
// collection's sidecar that can drift while the install itself stays valid:
// the server. It is called on the skip path, where the store's record has
// just been shown to name this collection, this path and this source, so the
// record is the authority and the document beside it is a copy that fell
// behind - a copy written by a version of this tool that recorded the run's
// default server rather than the one the collection resolved from.
//
// Repairing rather than re-installing is the proportionate answer: nothing
// about the artifact or the extracted tree is in question, so a re-download
// would change no byte of what is installed. That is also why it is not the
// same judgment the identity check makes - a document naming a different
// collection says the tree's provenance is unknown, which only a real
// install can settle.
//
// The document is rewritten whole from what was read, with one field
// replaced, so download_url, version_url, signatures and the git or url
// provenance all survive - none of them is available on this path to
// rebuild. The file is removed before it is written rather than truncated in
// place, the same discipline writeGalaxyInstallInfo follows for a role: an
// in-root symlink at that name is a path os.Root will happily resolve, and
// writing through it would land the document somewhere this run never
// created.
//
// A failure to repair is a warning, never an error: the run's actual product
// is installed and correct, and refusing to proceed over a metadata field
// would turn a self-heal into an outage.
func reconcileGalaxyInfo(runtime *infra.Infra, target installTarget, cfg *config.Config, col collection, doc GalaxyYAML) {
	want := buildGalaxyYAML(cfg, col, nil).Server
	if doc.Server == want {
		return
	}
	doc.Server = want
	data, err := yaml.Marshal(&doc)
	if err != nil {
		runtime.Output.Warnf("%s: cannot rebuild %s: %v", col.key(), galaxyYAMLFileName, err)
		return
	}
	rel := path.Join(target.info, galaxyYAMLFileName)
	if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		runtime.Output.Warnf("%s: cannot replace %s: %v", col.key(), galaxyYAMLFileName, err)
		return
	}
	if err := target.root.WriteFile(rel, data, helpers.FileMod); err != nil {
		runtime.Output.Warnf("%s: cannot write %s: %v", col.key(), galaxyYAMLFileName, err)
		return
	}
	runtime.Output.Debugf("%s: recorded server corrected to %s in %s", col.key(), want, galaxyYAMLFileName)
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
// version URLs (each with both of the parts of a URL that can carry a
// credential stripped - a capability-bearing query string and userinfo - see
// helpers.WithoutCredentials) and the signatures Galaxy attached to this
// version.
//
// Neither cut is redundant with the fetch-side refusals that reject a
// userinfo-bearing URL before any request is built, because this sink is
// reachable on a path where neither of them ran. On a run with signature
// verification enabled, servableFromCacheAlone gives up the metadata-free
// cache-hit fast path, so an already-cached artifact is served through
// ArtifactStore.Fetch while its version metadata is still resolved:
// validateDownloadInputs never runs on that path, and meta.DownloadURL reaches
// this document unchecked. meta.Href is one step further out still, since no
// fetch-side check judges it at all - normalizeVersionsURL guards the metadata
// URLs a request is built from, not the href a version document declares about
// itself.
//
// The parenthetical above covers only the download and version URLs, not the
// signatures value alongside them in that same sentence: meta.Signatures is
// copied through untyped and uncut, exactly as the server sent it. No cut is
// available for it the way helpers.WithoutCredentials is for a URL, because the
// field is `any` - a JSON shape the server picks, not this program - and
// walking an arbitrary decoded tree to rewrite every string inside it would be
// a sanitizer whose coverage depends on guessing that shape right; a partial
// one would read as a guarantee this comment cannot make. Whether that value
// can itself carry something worth stripping is a question this comment does
// not answer, only discloses.
func buildGalaxyYAML(cfg *config.Config, col collection, meta *types.GalaxyCollectionVersionInfo) GalaxyYAML {
	g := GalaxyYAML{
		FormatVer: "1.0.0",
		Name:      col.Name,
		Namespace: col.Namespace,
		Server:    cfg.Server,
		Version:   col.Version,
	}
	if loc, err := col.gitLocator(); err == nil && col.isGit() {
		g.Server = helpers.WithoutCredentials(loc.URL)
		g.GitCommit = loc.Commit
		return g
	}
	if loc, err := col.urlLocator(); err == nil && col.isURL() {
		g.Server = helpers.WithoutCredentials(loc.URL)
		g.DownloadURL = helpers.WithoutCredentials(loc.URL)
		g.URLSHA256 = loc.SHA256
		return g
	}
	// The server this collection actually resolved from, which is not always
	// the run's own default: an entry with its own `source:` resolves from
	// that one, and on a multi-server run the solver picks a winner per
	// collection. col.Source carries that winner (see
	// warnIfOffServerDownloadHost, which relies on the same stamping), while
	// cfg.Server is only the fallback for a collection that never got one -
	// so recording cfg.Server here made the document state something about
	// this collection that is true only of the run. It stayed invisible while
	// nothing read the field back; outdated's tree-driven report reads it to
	// decide which server to ask, and a wrong one there is a 404 an operator
	// cannot explain from what the file says.
	if col.Source != "" {
		g.Server = col.Source
	}
	if meta != nil {
		g.DownloadURL = helpers.WithoutCredentials(meta.DownloadURL)
		g.Signatures = meta.Signatures
		g.VersionURL = helpers.WithoutCredentials(meta.Href)
	}
	return g
}
