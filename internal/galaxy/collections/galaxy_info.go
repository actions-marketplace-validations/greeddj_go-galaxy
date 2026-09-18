package collections

import (
	"bytes"
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

// provenanceFileName is the one file this tool adds to ansible's .info
// layout, written beside GALAXY.yml for a git or url install only: it holds
// the provenance GALAXY.yml has no key for (see sidecarProvenance). ansible
// reads nothing from it, and removes it with the rest of the directory when
// it reinstalls the collection, as this tool's own install and cleanup do - so
// it cannot outlive the install it describes.
const provenanceFileName = "go-galaxy.yml"

// GalaxyYAML represents the GALAXY.yml metadata file, held to exactly the
// schema ansible-core validates it against when it reads an installed
// collection (_validate_v1_source_info_schema): these eight keys and no
// other. That schema is closed, not advisory - one key outside it and ansible
// warns and discards the whole document, on every command that reads the
// installed tree - so a fact this tool wants to record beyond it goes into
// provenanceFileName instead.
//
// For a collection built from a git source, Server names the repository URL
// (credential-free by construction) rather than a Galaxy server; for a url
// source, Server and DownloadURL both name the tarball URL. ansible-galaxy
// writes no sidecar at all for either kind of install.
type GalaxyYAML struct {
	DownloadURL string           `yaml:"download_url"`
	FormatVer   string           `yaml:"format_version"`
	Name        string           `yaml:"name"`
	Namespace   string           `yaml:"namespace"`
	Server      string           `yaml:"server"`
	Version     string           `yaml:"version"`
	VersionURL  string           `yaml:"version_url"`
	Signatures  galaxySignatures `yaml:"signatures"`
}

// galaxySignature is one entry of GALAXY.yml's signatures list, with exactly
// the keys ansible's schema admits there. ansible reads Signature by indexing
// the entry, so an entry without one is never kept.
type galaxySignature struct {
	PubkeyFingerprint string `yaml:"pubkey_fingerprint,omitempty"`
	PulpCreated       string `yaml:"pulp_created,omitempty"`
	Signature         string `yaml:"signature"`
	SigningService    string `yaml:"signing_service,omitempty"`
}

// galaxySignatures is GALAXY.yml's signatures list. It renders as a list even
// when empty - yaml.v3 renders a nil slice as [] - and that is load-bearing:
// ansible's schema lets a null through, and `ansible-galaxy collection verify
// --offline` then fails with a Python TypeError when it iterates the value.
//
// Decoding it never fails. A value that is not a list reads as no signatures,
// and an entry that is not a mapping, has a value that is not a scalar, or
// carries no signature text is dropped, as is an entry holding anything but a
// scalar under one of the four keys galaxySignature names; every other key is
// dropped from the entry. A sidecar is read to prove an install and to find
// its server, and neither depends on the signatures, so an odd shape there
// costs the signatures rather than the whole document. What was read then
// renders in the conforming shape, which is what lets the skip path repair a
// document instead of reinstalling the collection.
type galaxySignatures []galaxySignature

// UnmarshalYAML implements yaml.Unmarshaler; see galaxySignatures for the
// rule it applies.
func (s *galaxySignatures) UnmarshalYAML(node *yaml.Node) error {
	*s = nil
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range node.Content {
		var entry galaxySignature
		if item.Kind != yaml.MappingNode || item.Decode(&entry) != nil || entry.Signature == "" {
			continue
		}
		*s = append(*s, entry)
	}
	return nil
}

// sidecarSignatures converts the signatures a server attached to a version
// document - a decoded JSON value of whatever shape the server chose - into
// the list GALAXY.yml carries. It goes through the same decode a sidecar read
// does, so what an entry has to be is one rule rather than one per source.
func sidecarSignatures(raw any) galaxySignatures {
	var node yaml.Node
	if err := node.Encode(raw); err != nil {
		return nil
	}
	var signatures galaxySignatures
	if err := signatures.UnmarshalYAML(&node); err != nil {
		return nil
	}
	return signatures
}

// sidecarProvenance is what provenanceFileName holds: the commit a git
// install's tree was built from, or the sha256 of the bytes a url install was
// fetched as. Which of the two is set is also what says which kind of install
// the directory describes, and that is what outdated reads it for (see
// installedKindOf). A Galaxy install has neither, and no such file.
//
// The keys are the ones releases before this file existed wrote into
// GALAXY.yml itself, so a sidecar written by one of them decodes into this
// same type; see readInstalledSidecar for the fallback that relies on it.
type sidecarProvenance struct {
	GitCommit string `yaml:"git_commit,omitempty"`
	URLSHA256 string `yaml:"url_sha256,omitempty"`
}

// buildProvenance builds col's provenance document, which is empty for a
// collection from a Galaxy server.
func buildProvenance(col collection) sidecarProvenance {
	if loc, err := col.gitLocator(); err == nil && col.isGit() {
		return sidecarProvenance{GitCommit: loc.Commit}
	}
	if loc, err := col.urlLocator(); err == nil && col.isURL() {
		return sidecarProvenance{URLSHA256: loc.SHA256}
	}
	return sidecarProvenance{}
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

// readGalaxyInfo reads and parses the sidecar beside target, returning the
// bytes it parsed alongside the document so the skip path can tell whether a
// re-rendering would change the file. It reports false for every outcome that
// is not a parseable document - absent, unreadable, not a regular file, or
// malformed - since none of them is evidence of anything about the
// collection.
func readGalaxyInfo(target installTarget) (GalaxyYAML, []byte, bool) {
	data, ok := readRegularFile(target.root, path.Join(target.info, galaxyYAMLFileName))
	if !ok {
		return GalaxyYAML{}, nil, false
	}
	var doc GalaxyYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return GalaxyYAML{}, nil, false
	}
	return doc, data, true
}

// reconcileGalaxyInfo repairs what can drift in an already-installed
// collection's .info directory while the install itself stays valid. It is
// called on the skip path, where the store's record has just been shown to
// name this collection, this path and this source, so the record is the
// authority and the documents beside it are copies that fell behind. Earlier
// versions of this tool left two such copies: a GALAXY.yml naming the run's
// default server rather than the one the collection resolved from, and a
// GALAXY.yml outside ansible's schema - carrying a git or url install's
// provenance keys, or a null signatures value - which ansible answers by
// discarding the document or, for the null, by failing `collection verify
// --offline`. A tree an earlier release installed never reaches this path
// itself: its extract marker sits in the collection directory rather than in
// .info, so the install extracts it again and writes both documents anew.
// Those shapes arrive here only as a sidecar replaced from outside beside an
// install this release made - restored from an older copy of the tree, say.
//
// Repairing rather than re-installing is the proportionate answer: nothing
// about the artifact or the extracted tree is in question, so a re-download
// would change no byte of what is installed. That is also why it is not the
// same judgment the identity check makes - a document naming a different
// collection says the tree's provenance is unknown, which only a real
// install can settle.
//
// GALAXY.yml is rewritten whole from what was read, with the server replaced
// and the rest re-rendered through GalaxyYAML, so download_url, version_url
// and the signatures survive - none of them is available on this path to
// rebuild - and nothing outside the schema does. Whether to rewrite is
// decided by comparing that rendering with the bytes read rather than field
// by field, since the question is whether the file is exactly what this tool
// writes; a file that already is costs no write, so a skip over a tree that
// needs no repair touches nothing.
//
// The provenance file is settled first, and from col rather than from what
// was read, because a GALAXY.yml an earlier release wrote can hold the only
// copy of that provenance on disk: rewriting it and then failing the
// provenance write would erase the one record that tells outdated a git or
// url install from a Galaxy one. A provenance write that fails therefore
// leaves GALAXY.yml as it was.
//
// Each file is removed before it is written rather than truncated in place,
// the same discipline writeGalaxyInstallInfo follows for a role: an in-root
// symlink at that name is a path os.Root will happily resolve, and writing
// through it would land the document somewhere this run never created.
//
// A failure to repair is a warning, never an error: the run's actual product
// is installed and correct, and refusing to proceed over a metadata field
// would turn a self-heal into an outage.
func reconcileGalaxyInfo(runtime *infra.Infra, target installTarget, cfg *config.Config, col collection, state installedState) {
	if !reconcileProvenance(runtime, target, col) {
		return
	}
	doc := state.info
	doc.Server = buildGalaxyYAML(cfg, col, nil).Server
	data, err := yaml.Marshal(&doc)
	if err != nil {
		runtime.Output.Warnf("%s: cannot rebuild %s: %v", col.key(), galaxyYAMLFileName, err)
		return
	}
	if bytes.Equal(data, state.infoData) {
		return
	}
	if replaceInfoFile(runtime, target, col, galaxyYAMLFileName, data) {
		runtime.Output.Debugf("%s: %s rewritten to match the install record and ansible's schema", col.key(), galaxyYAMLFileName)
	}
}

// reconcileProvenance makes provenanceFileName hold what col's install
// record implies, and reports whether it does. A collection from a Galaxy
// server has nothing to hold, and no stray file is looked for on its behalf:
// an install resets the whole .info directory, so no run of this tool leaves
// one behind, and a skip should not pay a stat to find what nothing writes.
func reconcileProvenance(runtime *infra.Infra, target installTarget, col collection) bool {
	prov := buildProvenance(col)
	if prov == (sidecarProvenance{}) {
		return true
	}
	data, err := yaml.Marshal(&prov)
	if err != nil {
		runtime.Output.Warnf("%s: cannot rebuild %s: %v", col.key(), provenanceFileName, err)
		return false
	}
	if have, ok := readRegularFile(target.root, path.Join(target.info, provenanceFileName)); ok && bytes.Equal(have, data) {
		return true
	}
	if !replaceInfoFile(runtime, target, col, provenanceFileName, data) {
		return false
	}
	runtime.Output.Debugf("%s: %s rewritten to match the install record", col.key(), provenanceFileName)
	return true
}

// replaceInfoFile is writeInfoFile for the skip path, where a failure is a
// warning rather than an error (see reconcileGalaxyInfo), reporting whether
// the file was written.
func replaceInfoFile(runtime *infra.Infra, target installTarget, col collection, name string, data []byte) bool {
	if err := writeInfoFile(target, name, data); err != nil {
		runtime.Output.Warnf("%s: cannot write %s: %v", col.key(), name, err)
		return false
	}
	return true
}

// writeInfoFile replaces name in target's .info directory with data: whatever
// sits at that name is removed first, and only then is the file written, so
// the write lands on a name this call just created. See writeGalaxyInfo for
// the three shapes that removal closes.
func writeInfoFile(target installTarget, name string, data []byte) error {
	rel := path.Join(target.info, name)
	if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	if err := target.root.WriteFile(rel, data, helpers.FileMod); err != nil {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	return nil
}

// writeGalaxyInfo writes GALAXY.yml for the installed collection, and for a
// git or url collection its provenanceFileName beside it, removing a
// provenance file a different source's install of the same version left.
// When meta is nil (artifact-cache-hit fast path), a minimal GALAXY.yml is
// written using fields available from the collection identity.
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
// The directory is not reset here, because the extract marker lives in it:
// extractTree wipes and recreates it with the tree (resetCollectionInfo) and
// writes the marker, and a reset here would erase that marker on every
// install and force a full re-extraction on every run after. What a reset
// used to buy is kept file by file instead. An os.Root boundary only stops
// traversal, it says nothing about what already sits at a leaf name inside
// it, so writing to an existing GALAXY.yml would go straight through a
// relative in-root symlink planted there, which os.Root happily resolves as
// long as its target stays inside the root, and through a dangling in-root
// symlink, which the write would silently create the file at. Worse, a
// hardlink at that leaf is written through even when the linked inode lives
// outside the root entirely: os.Root is path-based, so it constrains which
// paths a method may traverse, but a hardlink is not a path, it is a second
// name for an inode the kernel already resolved before os.Root was ever
// involved - there is nothing for Root to see. writeInfoFile removes the
// name before writing, which closes all three: the write always lands on a
// name this run just created, the same invariant extractTree relies on for
// target.rel.
//
// The directory's own name gets the same treatment one level up. Anything at
// target.info that is not a real directory - most pointedly an in-root
// symlink to some other directory, which os.Root would follow - is removed
// and a real directory made in its place, before anything is written into
// it. That costs the extract marker the symlink led to, so the next run
// re-extracts rather than trusting a record it reached through a link.
func writeGalaxyInfo(target installTarget, cfg *config.Config, col collection, meta *types.GalaxyCollectionVersionInfo) error {
	if err := ensureInfoDir(target); err != nil {
		return err
	}
	g := buildGalaxyYAML(cfg, col, meta)
	data, err := yaml.Marshal(&g)
	if err != nil {
		return err
	}
	if err := writeInfoFile(target, galaxyYAMLFileName, data); err != nil {
		return err
	}
	prov := buildProvenance(col)
	if prov == (sidecarProvenance{}) {
		rel := path.Join(target.info, provenanceFileName)
		if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return classifyCollectionsRootError(target.root, target.info, err)
		}
		return nil
	}
	data, err = yaml.Marshal(&prov)
	if err != nil {
		return err
	}
	return writeInfoFile(target, provenanceFileName, data)
}

// ensureInfoDir makes target.info a real directory, leaving one that already
// is untouched; see writeGalaxyInfo for why anything else at that name is
// removed first.
func ensureInfoDir(target installTarget) error {
	info, err := target.root.Lstat(target.info)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		if err := target.root.RemoveAll(target.info); err != nil {
			return classifyCollectionsRootError(target.root, target.info, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	if err := target.root.MkdirAll(target.info, helpers.DirMod); err != nil {
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
// signatures value alongside them in that same sentence. meta.Signatures is
// reshaped into the list ansible's schema admits (see sidecarSignatures), which
// decides which entries and keys are kept, but every string it keeps is copied
// uncut, exactly as the server sent it. No cut is available for those the way
// helpers.WithoutCredentials is for a URL: they are signature text, a key
// fingerprint, a signing-service name and a timestamp, none of which has a
// part a cut could recognize. Whether one of them can itself carry something
// worth stripping is a question this comment does not answer, only discloses.
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
		return g
	}
	if loc, err := col.urlLocator(); err == nil && col.isURL() {
		g.Server = helpers.WithoutCredentials(loc.URL)
		g.DownloadURL = helpers.WithoutCredentials(loc.URL)
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
		g.Signatures = sidecarSignatures(meta.Signatures)
		g.VersionURL = helpers.WithoutCredentials(meta.Href)
	}
	return g
}
