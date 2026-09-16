package collections

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// infoDirSuffix ends the name of the version-scoped sidecar directory
// `<namespace>.<name>-<version>.info` that sits beside the installed tree.
// The layout is ansible's own, not this tool's invention, so a tree
// ansible-galaxy installed is scanned exactly as one this tool installed is.
const infoDirSuffix = ".info"

// installedScan is what one walk of a collections tree found: the entries
// outdated can look up, and the installs it deliberately left out because
// nothing on disk says what to ask about them.
type installedScan struct {
	entries []lockfile.Entry
	// skippedGit names every git-sourced install found, so the run can say
	// which collections its report does not cover rather than counting them
	// as current. See scanInstalledCollection for why a git install cannot
	// be judged from the tree alone.
	skippedGit []string
}

// scanInstalledTree builds outdated's current side from the collections tree
// at cfg.DownloadPath - `[defaults] collections_path`, `ANSIBLE_COLLECTIONS_PATH`
// or `--download-path`, already resolved to one directory by config - instead
// of from a lockfile, so a project that never locks can still be checked.
//
// It reads the tree and nothing else: no cache backend is opened here any
// more than on the lockfile path, which is what keeps outdated lock-free and
// its answers live.
//
// The walk is driven from the sidecar directories rather than from the
// `<namespace>/<name>` tree, because the sidecar is the only place on disk
// that records which server a collection came from, and a collection whose
// server is unknown is one this command cannot ask about. Every sidecar is
// still cross-checked against the installed tree before it is trusted (see
// scanInstalledCollection), so a stale one left behind by an upgrade cannot
// contribute a version that is no longer installed.
func scanInstalledTree(cfg *config.Config, runtime *infra.Infra) (installedScan, error) {
	root, err := os.OpenRoot(cfg.DownloadPath)
	if err != nil {
		return installedScan{}, err
	}
	defer func() { _ = root.Close() }()

	infos, err := fs.ReadDir(root.FS(), collectionsDirName)
	if err != nil {
		return installedScan{}, err
	}

	var scan installedScan
	for _, info := range infos {
		if !info.IsDir() || !strings.HasSuffix(info.Name(), infoDirSuffix) {
			continue
		}
		entry, kind := scanInstalledCollection(root, cfg, runtime, info.Name())
		switch kind {
		case installedGalaxy, installedURL:
			scan.entries = append(scan.entries, entry)
		case installedGit:
			scan.skippedGit = append(scan.skippedGit, entry.Name)
		case installedUnusable:
		}
	}
	// Sorted here rather than left in readdir order: the report is sorted by
	// name downstream, but the warning naming the skipped git installs is
	// not, and a warning whose order changes between two runs over an
	// unchanged tree reads as if the tree changed.
	slices.Sort(scan.skippedGit)
	slices.SortFunc(scan.entries, func(a, b lockfile.Entry) int { return strings.Compare(a.Name, b.Name) })
	return scan, nil
}

// installedKind classifies what one sidecar describes, which decides whether
// its collection can be looked up at all.
type installedKind int

const (
	// installedUnusable is a sidecar this scan will not build an entry from:
	// missing, unreadable, self-inconsistent, or naming a version the
	// installed tree does not hold.
	installedUnusable installedKind = iota
	installedGalaxy
	installedURL
	installedGit
)

// scanInstalledCollection reads one `<namespace>.<name>-<version>.info`
// sidecar and turns it into the lockfile entry outdated's lookup already
// knows how to handle, so the tree path and the lockfile path share one
// comparison rather than two.
//
// Identity never comes from the sidecar's word alone. The document names its
// own namespace, name and version, and this function accepts them only when
// re-composing them yields the very directory name the walk arrived through
// and when each component passes the alphabet it enters by - so a planted
// sidecar can describe itself, and nothing else. The installed tree is then
// consulted for the same version: `<namespace>/<name>/MANIFEST.json` has to
// exist and declare it. That check is what discards a stale sidecar, which is
// a real shape rather than a hypothetical one - install resets only the
// sidecar of the version it is installing, and only cleanup ever removes the
// one an earlier version left behind, so an upgraded tree holds two.
//
// A git install is classified rather than converted, because the sidecar
// records the repository and the commit but no ref, and the question outdated
// asks a git source is what the ref points at now. Turning the commit into a
// ref would make every git install report as current by definition, which is
// a verdict, not an absence of one; the caller names them instead.
func scanInstalledCollection(
	root *os.Root, cfg *config.Config, runtime *infra.Infra, infoName string,
) (lockfile.Entry, installedKind) {
	rel := path.Join(collectionsDirName, infoName, galaxyYAMLFileName)
	doc, prov, ok := readInstalledSidecar(root, cfg, runtime, infoName)
	if !ok {
		return lockfile.Entry{}, installedUnusable
	}
	kind := installedKindOf(prov)
	name := doc.Namespace + "." + doc.Name
	if !installedNameUsable(kind, doc) || !helpers.IsExactVersion(doc.Version) {
		runtime.Output.Warnf("Skipping sidecar %s: it names no collection this tool can look up", displayPath(cfg, rel))
		return lockfile.Entry{}, installedUnusable
	}
	if infoName != fmt.Sprintf("%s-%s%s", name, doc.Version, infoDirSuffix) {
		runtime.Output.Warnf("Skipping sidecar %s: it describes %s@%s, not the collection it is filed under",
			displayPath(cfg, rel), name, doc.Version)
		return lockfile.Entry{}, installedUnusable
	}
	if !installedVersionMatches(root, doc) {
		return lockfile.Entry{}, installedUnusable
	}
	entry := lockfile.Entry{Name: name, Version: doc.Version, Source: doc.Server}
	switch kind {
	case installedGit:
		return entry, installedGit
	case installedURL:
		entry.Type = lockfile.TypeURL
		return entry, installedURL
	case installedGalaxy, installedUnusable:
	}
	if entry.Source == "" {
		entry.Source = cfg.Server
	}
	return entry, installedGalaxy
}

// installedKindOf reads which source a sidecar describes off the one field
// only that source writes: a git install records the commit its tree was
// built from and a url install the sha256 of the bytes it was fetched as,
// while a Galaxy install writes neither.
func installedKindOf(prov sidecarProvenance) installedKind {
	switch {
	case prov.GitCommit != "":
		return installedGit
	case prov.URLSHA256 != "":
		return installedURL
	default:
		return installedGalaxy
	}
}

// installedNameUsable holds a sidecar's identity to the alphabet its own
// source is judged by, which is not one alphabet for all three. A collection
// a Galaxy server resolved is held to the Galaxy alphabet, lower case only,
// because that is what the servers themselves accept and what this tool
// refuses at every other boundary. A url collection is held to ansible's
// wider runtime FQCN rule instead: its identity comes from a MANIFEST.json
// authored outside any Galaxy server, real release artifacts carry
// mixed-case namespaces, and both ansible-galaxy and this tool install them
// - so judging one by the Galaxy alphabet here would drop from the report a
// collection the install accepted, which is the one thing a report about
// what is installed must not do. See helpers.IsURLCollectionNamePart for the
// same split stated from the other side.
func installedNameUsable(kind installedKind, doc GalaxyYAML) bool {
	if kind == installedURL {
		return helpers.IsURLCollectionNamePart(doc.Namespace) && helpers.IsURLCollectionNamePart(doc.Name)
	}
	return helpers.IsCollectionNamePart(doc.Namespace) && helpers.IsCollectionNamePart(doc.Name)
}

// installedVersionMatches reports whether the installed tree holds the very
// version doc describes. A sidecar with no tree under it, or one naming a
// version the tree's own MANIFEST.json does not, is stale: it is what an
// upgrade leaves behind, and reporting from it would name a version nothing
// is running.
//
// A parse failure and a missing file are the same answer here - this
// collection contributes nothing - and neither is worth a warning: an
// unreadable manifest is cleanup's finding to report, not this command's,
// and outdated writes nothing that could act on it.
func installedVersionMatches(root *os.Root, doc GalaxyYAML) bool {
	rel := path.Join(collectionsDirName, doc.Namespace, doc.Name, helpers.ManifestFileName)
	data, ok := readRegularFile(root, rel)
	if !ok {
		return false
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false
	}
	return manifest.CollectionInfo.Version == doc.Version
}

// readInstalledSidecar reads one sidecar directory: its GALAXY.yml, and the
// provenance that says which kind of install it describes. A GALAXY.yml that
// is absent is silent - a directory ending in .info need not be one - while
// a file that exists and does not parse is named, since an operator who sees
// a collection missing from the report deserves to know which file this
// command could not read.
//
// The provenance comes from provenanceFileName when that file is there, and
// from GALAXY.yml itself otherwise, which is where every release before that
// file existed wrote the same two keys. Without the fallback, a tree one of
// those releases installed and nothing has reinstalled since would report its
// git and url installs as Galaxy ones and send a repository or tarball URL a
// Galaxy API lookup. An install's skip path moves the keys into their own file
// (see reconcileGalaxyInfo), so the fallback serves only a tree no install
// has run over yet.
func readInstalledSidecar(
	root *os.Root, cfg *config.Config, runtime *infra.Infra, infoName string,
) (GalaxyYAML, sidecarProvenance, bool) {
	rel := path.Join(collectionsDirName, infoName, galaxyYAMLFileName)
	data, ok := readRegularFile(root, rel)
	if !ok {
		return GalaxyYAML{}, sidecarProvenance{}, false
	}
	var doc GalaxyYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		runtime.Output.Warnf("Skipping sidecar %s: %v", displayPath(cfg, rel), err)
		return GalaxyYAML{}, sidecarProvenance{}, false
	}
	provRel := path.Join(collectionsDirName, infoName, provenanceFileName)
	if provData, ok := readRegularFile(root, provRel); ok {
		data, rel = provData, provRel
	}
	var prov sidecarProvenance
	if err := yaml.Unmarshal(data, &prov); err != nil {
		runtime.Output.Warnf("Skipping sidecar %s: %v", displayPath(cfg, rel), err)
		return GalaxyYAML{}, sidecarProvenance{}, false
	}
	return doc, prov, true
}

// readRegularFile reads rel through root when it is a regular file, and
// reports false for every other outcome - absent, unreadable, or a name that
// is not a regular file at all.
//
// The Lstat is not a formality standing in front of a read that would fail
// anyway. It is a predicate rather than a symlink blocklist, so it refuses a
// symlink, a directory, a socket and a device in one check, and the fifo it
// also refuses is why the check has to come first: root.ReadFile opens
// O_RDONLY, which on a fifo blocks until somebody writes to it, so reading
// first would trade a skipped file for a hung command. os.Root bounds where a
// path may lead, which is a different question from what sits at the end of
// it - the same distinction scanning for a manifest already draws in
// internal/galaxy/cleanup.
func readRegularFile(root *os.Root, rel string) ([]byte, bool) {
	info, err := root.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return nil, false
	}
	return data, true
}

// displayPath renders a tree-relative path as the OS-native absolute
// location an operator can act on, since a slash-separated path relative to a
// root nobody named tells them nothing about which tree it was in.
func displayPath(cfg *config.Config, rel string) string {
	return filepath.Join(cfg.DownloadPath, filepath.FromSlash(rel))
}

// installedRoleCount counts the roles installed under cfg.RolesPath, which
// outdated cannot check without a lockfile: ansible's
// meta/.galaxy_install_info - the only per-role record on disk - carries the
// version and the install date and no source at all, so nothing there says
// whether a role came from a Galaxy server or from a repository, let alone
// which one. The count exists to be disclosed rather than acted on: a run
// that silently skipped a project's roles would read as a clean report.
//
// Every failure counts as zero. This is a disclosure, and a disclosure that
// aborts the command it decorates is worse than one that is absent.
func installedRoleCount(cfg *config.Config) int {
	root, err := os.OpenRoot(cfg.RolesPath)
	if err != nil {
		return 0
	}
	defer func() { _ = root.Close() }()
	dirs, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0
	}
	count := 0
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		if _, ok := readRegularFile(root, path.Join(dir.Name(), galaxyInstallInfoRel)); ok {
			count++
		}
	}
	return count
}

// reportInstalledGaps prints, once, everything the tree-driven report does
// not cover: the git-sourced collections it could not ask about and the
// roles it cannot check at all. It is one line rather than one per subject,
// so a project with many of either does not bury its own report.
func reportInstalledGaps(runtime *infra.Infra, scan installedScan, roles int) {
	if len(scan.skippedGit) > 0 {
		runtime.Output.Warnf(
			"not checked, installed from git and the tree records no ref to compare: %s; run lock to cover them",
			strings.Join(scan.skippedGit, ", "))
	}
	if roles > 0 {
		runtime.Output.Warnf(
			"not checked, %d installed role(s): meta/.galaxy_install_info records no source; run lock to cover them", roles)
	}
}

// errNoInstalledCollections is what a run with neither a lockfile nor a
// readable collections tree fails with. It carries helpers.ErrLockfileMissing
// so the exit code is the one every other command already gives for a
// lockfile that is not there: the fallback widens where the current side may
// come from, and does not add a class for a run that finds it nowhere.
func errNoInstalledCollections(lockPath, treePath string, cause error) error {
	return fmt.Errorf("%w: %s, and no collections installed at %s: %w",
		helpers.ErrLockfileMissing, lockPath, treePath, cause)
}

// isTreeAbsent reports whether err says the collections tree simply is not
// there, which is the one failure the caller turns into
// errNoInstalledCollections rather than propagating.
func isTreeAbsent(err error) bool { return errors.Is(err, fs.ErrNotExist) }
