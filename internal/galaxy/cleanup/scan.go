package cleanup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// scanProjectWorkspace opens projectPath's collections workspace and, if
// usable, scans it into index/byKey/deps. It is buildReachable's phase-1
// per-project step, and is deliberately independent of whether this
// project's own requirements load successfully - see projectRequirementRoots,
// buildReachable's phase-2 step, for that half. Its only signal to the
// caller is success or failure: it does not report whether anything was
// actually scanned, since phase 2 resolves every recorded project's roots
// regardless of what phase 1 found for that same project.
//
//   - No candidate has an ansible_collections directory: nil error, nothing
//     scanned - the ordinary skip, identical to today's absent-workspace
//     behavior.
//   - A candidate's ansible_collections entry escapes its root: a single
//     operator warning naming the project is emitted and nil is returned, so
//     nothing under this project is scanned or removed this run but every
//     other project's cleanup proceeds unaffected. This is phase 1 only: the
//     project's own requirements.yml is still resolved in phase 2
//     (projectRequirementRoots runs for every recorded project regardless of
//     this outcome), so its roots can still keep another project's on-disk
//     copy reachable even though nothing under this project itself was
//     scanned or removed.
//   - A usable workspace fails to scan (a genuine IO error, not an escape):
//     the error is wrapped with the collections path and returned, aborting
//     the whole run. Every path inside the scan is root-relative once ws is
//     in play, so the raw error carries no indication of which project it
//     came from without this wrap.
//
// ws.root is closed before returning in every case, releasing its file
// descriptor before the next project's workspace is opened rather than
// holding one open per project for the whole run.
//
// projectPath always derives from filepath.Dir of a requirements file path
// (store.RecordProject) that can itself sit inside a directory whose name a
// hostile checkout chose - a project subdirectory name is ordinary git tree
// content, not a value this tool ever validates. The collections path
// carries the same exposure whenever it too derives from projectPath - true
// of both scan fallback candidates and of the default, relative
// download-path, though not of an operator-configured absolute one.
//
// Every line this package emits goes through internal/progress, which
// applies safeout.Clean on every tier, so no control character except \n
// and \t reaches a terminal from either the Warnf here or the error wrapped
// further down this function. The %q on projectPath here, and on the path
// in openProjectWorkspace's own wrapped error, additionally escapes \n for
// those two operands specifically, before Clean ever sees it - the same
// overlap reportOutdated's doc (outdated.go) already describes for its own
// rendering. The residual that remains is Clean's own documented one: a
// *fs.PathError surfacing from the scan carries
// ansible_collections/<ns>/<name>/MANIFEST.json with ns and name straight
// off fs.ReadDir, and a \n in either survives Clean, so such a path can
// claim one extra plain-text line - never overwrite one already emitted,
// since \r does not survive. See safeout.Clean's own doc comment for why.
func scanProjectWorkspace(
	out output.Printer,
	projectPath string,
	project store.ProjectRecord,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	ws, err := openProjectWorkspace(projectPath, project)
	if err != nil {
		out.Warnf("skipping project %q: %v; nothing under it was scanned or removed", projectPath, err)
		return nil
	}
	if ws.root == nil {
		return nil
	}
	defer func() { _ = ws.root.Close() }()

	if err := scanInstalledCollections(out, ws, index, byKey, deps); err != nil {
		return fmt.Errorf("failed to scan %q: %w", ws.path, err)
	}
	return nil
}

// scanInstalledCollections indexes installed collections under ws. Installed
// collections only ever live at the fixed
// <ws.path>/ansible_collections/<ns>/<name>/MANIFEST.json depth, so this
// walks exactly those two directory levels rather than the whole tree: a
// MANIFEST.json nested deeper (e.g. inside a collection's own test fixtures)
// is never mistaken for an installed collection, and the scan does not pay
// for descending into every file of every installed collection. Directory
// reads (ansible_collections itself, and each namespace/name listing) go
// through ws.fsys; the manifest file itself is read through ws.root directly
// (scanCollectionDir's call to readManifest). Both are bound by the same
// os.Root openProjectWorkspace established, so a symlink swap planted after
// that root was opened cannot make the scan follow it out of ws.path. Every
// component on this walk is closed by a checkpoint before it is ever opened,
// though the checkpoint's shape differs by how the scan reaches that
// component. ansible_collections is closed by openProjectWorkspace's own
// static probe, which already found it clean before this function ever ran.
// A namespace or a name component is closed by the parent fs.ReadDir call
// that listed it as a real directory: a component that is already a symlink
// at listing time is skipped by !IsDir() and never reaches a rooted read in
// the first place, so openProjectWorkspace never needs to inspect those
// components itself. The manifest leaf is the one component nothing lists on
// the way in - scanCollectionDir reaches it directly by name, not through a
// parent directory listing - so it carries its own static checkpoint
// instead: manifestIsRegularFile's Lstat gate. See scanProjectWorkspace's
// doc comment for what happens when a rooted read fails for a reason other
// than a symlink swap.
//
// Manifests whose namespace/name/version cannot be safely used as filesystem
// path elements are rejected at ingestion (see buildInstalledRecord): a
// warning is emitted via out and the scan continues rather than aborting the
// whole run or indexing the tainted record.
func scanInstalledCollections(
	out output.Printer,
	ws workspace,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	nsEntries, err := fs.ReadDir(ws.fsys, "ansible_collections")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, nsEntry := range nsEntries {
		if !nsEntry.IsDir() {
			continue
		}
		if err := scanNamespaceDir(out, ws, nsEntry.Name(), index, byKey, deps); err != nil {
			return err
		}
	}
	return nil
}

// scanNamespaceDir scans every ansible_collections/<ns>/<name> directory for
// a MANIFEST.json, one namespace at a time.
func scanNamespaceDir(
	out output.Printer,
	ws workspace,
	ns string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	nameEntries, err := fs.ReadDir(ws.fsys, path.Join("ansible_collections", ns))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The namespace directory vanished between the parent ReadDir
			// and this one (e.g. a concurrent cleanup or install run) -
			// skip it rather than aborting the whole scan.
			return nil
		}
		return err
	}
	for _, nameEntry := range nameEntries {
		if !nameEntry.IsDir() {
			continue
		}
		if err := scanCollectionDir(out, ws, ns, nameEntry.Name(), index, byKey, deps); err != nil {
			return err
		}
	}
	return nil
}

// manifestIsRegularFile reports whether ansible_collections/<ns>/<name>/
// MANIFEST.json at rel is a regular file - the only shape scanCollectionDir
// ever treats as an installed collection's manifest. Its error return is the
// raw Lstat error, unwrapped: the caller distinguishes fs.ErrNotExist (no
// MANIFEST.json here, the existing silent skip) from every other Lstat
// failure (a genuine IO error, which aborts) the same way it already
// distinguishes those two outcomes for readManifest's own error below.
//
// IsRegular is deliberately a predicate, not a symlink blocklist: it rejects
// a symlink, a directory named MANIFEST.json, a fifo, a socket, and a device
// in the same check, instead of enumerating shapes one at a time and risking
// a future addition being missed. The fifo case is not academic -
// root.ReadFile opens with O_RDONLY, which blocks on a fifo until a writer
// appears, so a symlink-only blocklist would trade an abort for a hang. Lstat
// (not Stat) is what makes a symlink itself the thing being classified rather
// than whatever it points at.
func manifestIsRegularFile(root *os.Root, rel string) (bool, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// scanCollectionDir probes ansible_collections/<ns>/<name>/MANIFEST.json and,
// if present, regular, and parseable, indexes the installed collection it
// describes.
func scanCollectionDir(
	out output.Printer,
	ws workspace,
	ns, name string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	rel := path.Join("ansible_collections", ns, name, "MANIFEST.json")
	manifestPath := filepath.Join(ws.path, filepath.FromSlash(rel))

	regular, err := manifestIsRegularFile(ws.root, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A <ns>/<name> directory without a MANIFEST.json is not an
			// installed collection - skip it silently rather than aborting.
			return nil
		}
		return err
	}
	if !regular {
		// A MANIFEST.json entry that is not a regular file - a symlink, a
		// directory, a fifo, or anything else - identifies no collection at
		// all: it is neither a reachability source nor a deletion candidate,
		// so it is reported (visible warning) but never opened, and the scan
		// continues rather than aborting.
		out.Warnf("skipping non-regular manifest at %q", manifestPath)
		return nil
	}

	manifest, err := readManifest(ws.root, rel, manifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The entry vanished between the Lstat gate above and this read
			// (e.g. a concurrent cleanup or install run) - skip it silently
			// rather than aborting, the same benign race scanNamespaceDir's
			// own vanished-namespace comment describes.
			//
			// Documented uncovered: reaching this arm requires winning a
			// race between manifestIsRegularFile's Lstat above and this
			// ReadFile - the entry must exist as a regular file at the
			// first syscall and be gone by the second - and this test suite
			// has no seam to force that timing. If this arm were removed, a
			// manifest that vanishes mid-scan would abort the whole run
			// instead of being skipped, which is exactly the "one bad
			// project kills every project" class the rest of this scan is
			// written to avoid.
			return nil
		}
		if errors.Is(err, helpers.ErrCorruptManifest) {
			// A manifest that cannot be parsed identifies no collection at
			// all: it is neither a reachability source nor a deletion
			// candidate, so it is reported (visible warning) but its
			// on-disk tree is left untouched rather than aborting the scan.
			// manifestPath is built from ns/name straight off fs.ReadDir,
			// before either has ever reached an IsPathElement check (that
			// check runs inside buildInstalledRecord, which this branch
			// never calls), so it is rendered %q rather than %s: unlike
			// the identifiers this package prints elsewhere, it is not yet
			// known to be free of a line-forging character.
			out.Warnf("skipping corrupt manifest at %q: %v", manifestPath, err)
			return nil
		}
		return err
	}
	record, key, ok, err := buildInstalledRecord(ws.path, manifestPath, ns, name, manifest)
	if err != nil {
		// manifestPath carries the identical exposure the ErrCorruptManifest
		// branch above documents - built from raw, unvalidated ns/name - and
		// err's own %q rendering of ns/name/version (buildInstalledRecord's
		// own error) does not cover manifestPath, a separately built string.
		// %q for the same reason.
		out.Warnf("skipping install with unsafe identifier at %q: %v", manifestPath, err)
		return nil
	}
	if !ok {
		return nil
	}
	index[record.FQDN] = append(index[record.FQDN], record)
	byKey[key] = append(byKey[key], record)
	deps[key] = extractDeps(manifest)
	return nil
}

// readManifest reads and parses a MANIFEST.json at rel (slash-separated,
// relative to root) through root. A read failure (including fs.ErrNotExist
// for a missing file, which the caller checks for) is returned as-is.
// display is the same location rendered as a plain, OS-native absolute
// string, used only in the helpers.ErrCorruptManifest message: rel alone
// would not tell an operator which project's manifest failed to parse. A
// file that exists but fails to parse as JSON is reported as
// helpers.ErrCorruptManifest wrapping the underlying decode error, rather
// than silently discarding it: the caller decides how to surface that.
func readManifest(root *os.Root, rel, display string) (types.GalaxyCollectionVersionInfoManifest, error) {
	data, err := root.ReadFile(rel)
	if err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, err
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, fmt.Errorf("%w at %s: %w", helpers.ErrCorruptManifest, display, err)
	}
	return manifest, nil
}

// buildInstalledRecord builds an installedCollection from ns and name - the
// ansible_collections/<ns>/<name> directory pair scanCollectionDir just
// walked to reach manifestPath - and the version parsed out of manifest.
// This is the governing identity invariant for this package: a scanned
// collection's namespace and name are always the two path components the
// scan walked through, never a value the manifest declares, so no MANIFEST.json
// content - however it got there - can ever redirect this record, or any
// deletion built from it, at a different collection's directory. ns and name
// are single filesystem path elements by construction, since fs.ReadDir
// never yields an entry containing "/" or a bare "." / "..", so the
// IsPathElement checks below are defensive-only for those two arguments in
// every call this package itself makes; they are kept anyway as
// belt-and-suspenders, and TestBuildInstalledRecordRejectsSeparators is what
// actually exercises that arm, by calling this function directly with values
// the real scan could never produce.
//
// version is the one identity component still read from the manifest:
// nothing else on disk records it. Recovering it from the persisted
// snapshot's InstalledEntry.InstallPath instead was considered and rejected:
// Store.SetInstalled keys by ns.name@version and never prunes an older entry
// on upgrade, so two entries can legitimately share one InstallPath, making
// the reverse (path -> version) lookup ambiguous. The .info sidecar
// directory name was rejected too, as a third source of truth for a value
// the manifest already names.
//
// A lying version's blast radius is bounded to a fixed prefix, not to a
// unique target: ns and name are fixed by the walk before version is ever
// consulted, so the two names a lying version can mistarget are always
// <ns>.<name>-<lying-version>.info (the sidecar) and
// <ns>-<name>-<lying-version>.tar.gz (the artifact filename) - never some
// other collection's own ns/name. But "-" and "." are both legal inside a
// walked path element, which makes both of those concatenations ambiguous:
// walking namespace "a", name "b", with a manifest lying that its version is
// "c-1.0.0" produces the sidecar name "a.b-c-1.0.0.info" and the artifact
// filename "a-b-c-1.0.0.tar.gz" - byte-identical to what a genuinely,
// legitimately installed a.b-c@1.0.0 would itself produce. The impact stays
// narrow regardless: the sidecar RemoveAll failure this could cause is
// already ignored (removeInfoDir is best-effort), and an artifact cache-slot
// eviction self-heals on the next refetch. Containment is unaffected either
// way - removeInstallPath joins only Namespace and Name, so the version
// never participates in a directory path at all.
//
// An incomplete manifest (missing namespace, name, or version) is a benign
// skip: ok is false and err is nil. A manifest whose namespace, name, or
// version cannot be safely used as a single filesystem path element (e.g. it
// contains "/" or is "..") is rejected with
// helpers.ErrUnsafeCollectionIdentifier rather than silently building a
// record whose Version would later escape the collections tree in
// removeInstalled.
func buildInstalledRecord(
	collectionsPath string,
	manifestPath string,
	ns, name string,
	manifest types.GalaxyCollectionVersionInfoManifest,
) (installedCollection, string, bool, error) {
	version := manifest.CollectionInfo.Version
	if ns == "" || name == "" || version == "" {
		return installedCollection{}, "", false, nil
	}
	if !helpers.IsPathElement(ns) || !helpers.IsPathElement(name) || !helpers.IsPathElement(version) {
		return installedCollection{}, "", false, fmt.Errorf(
			"%w: ns=%q name=%q version=%q", helpers.ErrUnsafeCollectionIdentifier, ns, name, version,
		)
	}
	installPath := filepath.Dir(manifestPath)
	key := fmt.Sprintf("%s.%s@%s", ns, name, version)
	fqdn := fmt.Sprintf("%s.%s", ns, name)
	// A parse failure here is not this function's concern to reject: an
	// identifier that passed the path-element safety check above can still
	// be non-semver (e.g. a git ref), and selectInstalled's existing
	// unparseable-version handling (skip the item under a real constraint)
	// is preserved by simply caching nil in that case.
	parsed, _ := semver.NewVersion(version)
	return installedCollection{
		Key:            key,
		FQDN:           fqdn,
		Namespace:      ns,
		Name:           name,
		Version:        version,
		InstallPath:    installPath,
		CollectionsDir: collectionsPath,
		Parsed:         parsed,
	}, key, true, nil
}

func extractDeps(manifest types.GalaxyCollectionVersionInfoManifest) map[string]string {
	if manifest.CollectionInfo.Dependencies != nil {
		return manifest.CollectionInfo.Dependencies
	}
	return map[string]string{}
}
