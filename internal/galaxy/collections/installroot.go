package collections

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// collectionsDirName is the one directory ansible looks for a collection
// under, and therefore the only entry of the collections path this tool
// writes into or reads back. Named once so a writer and a reader of the same
// tree cannot spell it differently.
const collectionsDirName = "ansible_collections"

// errEmptyDownloadPath names the misconfiguration directly rather than
// letting os.OpenRoot("") surface as a bare ENOENT, which tells an operator
// nothing about which setting to fix.
var errEmptyDownloadPath = errors.New("--download-path (or [defaults] collections_path) is empty")

// errCollectionsTreeNotUsable stands in for a real filesystem error that a
// dry-run probe never actually triggers: probeAnsibleCollectionsUsable and
// dryRunNamespaceProbe (dryrun.go) both classify a suspect path with
// classifyCollectionsRootError, which wants a genuine kernel error to embed
// in its own message, and a preview never calls the MkdirAll/RemoveAll that
// would produce one. This sentinel is what gets embedded instead, naming the
// observed condition - the target is not usable as a collections directory -
// rather than fabricating a fake mkdirat/removeall *fs.PathError that never
// happened.
var errCollectionsTreeNotUsable = errors.New("not usable as a collections directory")

// installTarget is the resolved, path-safe location of one collection's
// on-disk footprint: its install directory and its .info sidecar directory,
// both expressed relative to root so every write below funnels through the
// same os.Root that closes off a symlinked ansible_collections - or a
// symlinked namespace/name component beneath it - redirecting the write
// outside cfg.DownloadPath.
//
// rel and info are slash-separated (built with path.Join, never
// filepath.Join): they feed both *os.Root methods and root.FS(), and both
// APIs use the io/fs slash convention regardless of GOOS. path is the same
// install location rendered as a plain, OS-native absolute string - never
// used for a filesystem write itself, only for what genuinely needs a plain
// string: extractCollection's own unpack step, which operates on a directory
// this run just created empty through root immediately beforehand (see
// extractCollection's doc comment for why that step cannot itself be rooted),
// and operator-facing log lines.
//
// marker is the root-relative directory the extract marker lives in (see
// markerRel). For a collection that is info, not rel: `ansible-galaxy
// collection verify` reports every file in the collection's own directory
// that its FILES.json does not list, and exits 1 over it, so a marker there
// failed every verify run over a tree this tool installed. A role keeps its
// marker in rel, where cleanup and the directory-ownership check look for
// it, since ansible has no verify for a role. infoPrefix, set for a
// collection only, is the "<namespace>.<name>-" every version's .info
// directory of that collection starts with; see resetCollectionInfo.
type installTarget struct {
	root       *os.Root
	rel        string
	path       string
	info       string
	marker     string
	infoPrefix string
}

// newInstallTarget builds col's installTarget rooted at root, and whether
// col's identity was safe to use at all - the single validating chokepoint
// every construction of both of col's on-disk locations (the install
// directory and the .info sidecar) must go through, so they cannot be
// computed two different ways, one of them unguarded. One chokepoint for the
// two locations means both are validated by the identical rule, rather than
// by two rules maintained separately and free to drift apart.
//
// col.Namespace, col.Name, and col.Version must each be helpers.IsPathElement
// before they are ever joined: path.Join fuses a leading ".." in Version into
// the synthetic "<ns>.<name>-.." element, turning it into a real "up one
// directory" element, so the join absorbs Version's first ".." for free and
// every later ".." in Version pops a real path component off cfg.DownloadPath.
// Validating the joined result after the fact cannot close this - by the time
// a path exists to inspect, the escape has already happened - so the three
// components are what is validated, before any join is computed, and never
// the composed "<ns>.<name>-<version>.info" element: removeInstalled
// (internal/galaxy/cleanup/remove.go) validates the same three components
// with the same predicate, so the deleter never refuses a sidecar directory
// this function created as an unsafe identifier. That symmetry is specific to the sidecar
// path: the install directory itself needs no such synthetic-element
// reasoning, since "ansible_collections"/namespace/name never fuses a
// leading ".." the way "<ns>.<name>-<version>" can, but it is validated
// identically anyway, on the same three components, so one guard covers both
// locations.
//
// A nil root - warm's deliberate choice, since warm never touches the
// collections tree at all - fails closed here rather than by convention:
// every caller that might be handed a nil root gets ok=false instead of a
// nil-pointer panic on first use.
func newInstallTarget(root *os.Root, cfg *config.Config, col collection) (installTarget, bool) {
	if root == nil {
		return installTarget{}, false
	}
	if !helpers.IsPathElement(col.Namespace) || !helpers.IsPathElement(col.Name) || !helpers.IsPathElement(col.Version) {
		return installTarget{}, false
	}
	rel := path.Join(collectionsDirName, col.Namespace, col.Name)
	infoPrefix := col.Namespace + "." + col.Name + "-"
	info := path.Join(collectionsDirName, infoPrefix+col.Version+infoDirSuffix)
	return installTarget{
		root:       root,
		rel:        rel,
		path:       filepath.Join(cfg.DownloadPath, rel),
		info:       info,
		marker:     info,
		infoPrefix: infoPrefix,
	}, true
}

// openCollectionsRoot opens the single os.Root every install-side write
// funnels through, rooted at downloadPath itself rather than at its
// ansible_collections subdirectory - the same boundary removeWorkspaceFiles
// (internal/galaxy/cleanup/remove.go) draws (see removeInstalled's doc
// comment there) and for the same reason: a swap of ansible_collections itself, not just a
// component beneath it, must also be constrained.
//
// Two properties of os.Root are load-bearing for that boundary choice and
// easy to get wrong when touching this function. First, os.OpenRoot follows
// a symlink at downloadPath itself when establishing the root - the same as
// a plain os.Open would - which is exactly what keeps a symlinked
// DownloadPath (the drop-in-compat case documented below) working at all,
// and exactly what would make moving this call one level down catastrophic:
// opening ansible_collections itself as the root, instead of downloadPath,
// would establish the root at whatever ansible_collections resolves to
// today, symlink or not, with no os.Root protection left over that first
// hop - the very escape this function exists to close would already have
// happened before Root was ever consulted. Second, below the established
// root, a component symlink is allowed with a relative in-root target and
// refused with an absolute one, even when that absolute target
// geometrically resolves back inside the root, because os.Root never
// consults the absolute filesystem namespace at all - only ever a relative
// in-root target is followed. A hostile checkout can only ever plant the
// relative kind (an absolute host path does not survive a clone), which is
// exactly why that is the shape that matters here, not merely a
// theoretical case.
//
// When create is true (every real, non-dry-run command), downloadPath is
// created with os.MkdirAll first - a no-op when downloadPath is itself a
// symlink to an existing directory, so a symlinked DownloadPath, the
// drop-in-compat case, still opens cleanly - and ansible_collections is then
// created once, through root, classified on failure. Doing this once here,
// rather than once per collection, is load-bearing for the operator
// experience: ansible_collections is a prefix every collection in a level
// shares, so without this a 200-collection level would emit 200 identical
// escape failures instead of one (see installWithState's own doc comment).
//
// When create is false (a dry run), downloadPath is opened as-is: os.Root
// requires the directory to already exist, and a dry run must never create
// the directory it is only describing. A downloadPath that does not exist
// yet is reported as (nil, nil) - "no target to root at" - which every
// create=false caller (shouldSchedulePrefetch, installDryRunProbe) already
// treats as "not installed"/"absent" via newInstallTarget's own nil-root
// guard, rather than as an error. Once opened, this branch also runs
// probeAnsibleCollectionsUsable - see its own doc comment for the predicate
// and why it exists: without it, a preview against a symlinked or otherwise
// unusable ansible_collections would report success today where a real,
// non-dry-run run of the identical configuration would certainly fail.
func openCollectionsRoot(downloadPath string, create bool) (*os.Root, error) {
	if strings.TrimSpace(downloadPath) == "" {
		return nil, errEmptyDownloadPath
	}

	if !create {
		root, err := os.OpenRoot(downloadPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil //nolint:nilnil // absent DownloadPath on a dry run is "nothing to describe yet", not a failure.
			}
			return nil, err
		}
		if err := probeAnsibleCollectionsUsable(root); err != nil {
			_ = root.Close()
			return nil, err
		}
		return root, nil
	}

	if err := os.MkdirAll(downloadPath, helpers.DirMod); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(downloadPath)
	if err != nil {
		return nil, err
	}
	if err := root.MkdirAll(collectionsDirName, helpers.DirMod); err != nil {
		return nil, classifyCollectionsRootError(root, collectionsDirName, err)
	}
	return root, nil
}

// probeAnsibleCollectionsUsable checks, without creating, modifying, or
// removing anything, whether a real (non-dry-run) run's own
// root.MkdirAll("ansible_collections") would succeed against root's
// ansible_collections entry. Measured directly against a real install, every
// shape agrees between the real run and this probe:
//
//	ansible_collections shape                         | real run | this probe
//	--------------------------------------------------|----------|-----------
//	absent                                            | ok       | ok (nil)
//	real directory                                    | ok       | ok (nil)
//	symlink, relative, in-root, to a directory        | ok       | ok (nil)
//	symlink, escaping (relative or absolute)          | fails    | fails (ErrCollectionsPathEscape)
//	symlink, dangling                                 | fails    | fails (ErrCollectionsPathEscape)
//	regular file                                      | fails    | fails (unclassified, matching the real run's own bare mkdirat failure)
//
// Two entries in that table are the reason this needs both Stat and Lstat,
// not one alone. Stat is required for the dangling case: root.MkdirAll
// itself fails against a dangling ansible_collections symlink (there is
// nothing valid for MkdirAll to no-op against), but Stat alone reports
// fs.ErrNotExist for that exact shape - the symlink follows to a target that
// does not exist - which would misreport a real run's failure as this
// probe's success. Lstat is what tells a dangling symlink (Lstat succeeds,
// reporting the symlink entry itself) apart from a genuinely absent entry
// (Lstat also reports fs.ErrNotExist, the ordinary case root.MkdirAll would
// create from scratch). And "is it a symlink" is not enough on its own
// either: an in-root relative symlink to a real directory is ACCEPTED by a
// real run (root.MkdirAll no-ops against an existing directory, symlink or
// not), so a bare Lstat-mode-bit check would misreport that accepted shape
// as unusable.
//
// The exact predicate, therefore: Stat succeeds and reports a directory
// (covers the real-directory and in-root-symlink-to-a-directory rows), OR
// Lstat reports fs.ErrNotExist (covers the absent row - nothing there at
// all). Either one is "ok" and returns nil. Anything else - Stat failing for
// a reason Lstat did not already excuse, or Stat succeeding on something
// that is not a directory - is classified through classifyCollectionsRootError,
// the identical classifier openCollectionsRoot's own create=true branch
// already uses on the identical path, so a symlink classifies
// helpers.ErrCollectionsPathEscape (matching the real run's own
// classification of the identical shape) and a regular file stays
// unclassified (matching the real run's own bare, unclassified mkdirat
// failure). errCollectionsTreeNotUsable stands in for the genuine kernel
// error classifyCollectionsRootError wants to embed, since this probe never
// actually calls MkdirAll to produce one.
func probeAnsibleCollectionsUsable(root *os.Root) error {
	if info, err := root.Stat("ansible_collections"); err == nil && info.IsDir() {
		return nil
	}
	if _, err := root.Lstat("ansible_collections"); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return classifyCollectionsRootError(root, "ansible_collections", errCollectionsTreeNotUsable)
}

// classifyCollectionsRootError runs only on the failure path of a root
// operation against rel - the happy path pays nothing for this, exactly like
// archive.classifyOpenRegularFileError, whose own doc comment states the same
// pattern. On failure it walks rel's components top-down with root.Lstat, and
// the first component that is a symlink is named in the returned error as
// helpers.ErrCollectionsPathEscape, along with a suggestion to point
// --download-path/collections_path at the real directory instead of a
// symlink to it. When no such component is found, err is returned unchanged.
//
// A component's own Lstat failing with anything other than fs.ErrNotExist or
// syscall.ENOTDIR is treated the same as finding a symlink: os.Root's escape
// refusal (Go's unexported errPathEscapes, surfacing as "path escapes from
// parent") is exactly this shape - some other, unrecognized failure on a
// component whose every ancestor was already confirmed a real, non-symlink
// entry - so it is the closest thing to a positive signal available without
// importing an unexported sentinel. syscall.ENOTDIR is carved out from that
// catch-all deliberately: it means a component is a plain file blocking a
// path that needed it to be a directory (see
// TestClassifyCollectionsRootErrorReturnsRawErrorForNonEscapeFailure's
// "openat ...: not a directory" case), a structural, non-symlink failure the
// kernel reports through a genuinely different errno than the one os.Root's
// own escape refusal uses - conflating the two would misreport an ordinary
// naming conflict as a security-relevant escape.
//
// This function is not load-bearing for containment: os.Root already refused
// the operation atomically, inside the kernel, before this function is ever
// called, so a wrong diagnosis here can never turn a refused write into one
// that succeeds. It IS load-bearing for classification, though, and that half
// is not free of consequence: measured directly, root.MkdirAll("ansible_collections")
// against a symlinked ansible_collections returns a bare "mkdirat
// ansible_collections: file exists" from the kernel - not anything mentioning
// an escape - while the identical symlink one component deeper does surface
// as "path escapes from parent". Without this walk, that shallowest and most
// common case (a symlinked ansible_collections itself) would return err
// unchanged, an fs.PathError no caller recognizes, and the whole chain that
// depends on the sentinel - exitcode's ExitInstall mapping, the
// operator-facing "point --download-path/collections_path at the real
// directory" message, and every run-level test asserting on
// helpers.ErrCollectionsPathEscape - would silently stop firing for exactly
// the case that matters most.
//
// Documented-uncovered: the branch below returning an escape error for a
// component's own Lstat failing with neither fs.ErrNotExist nor
// syscall.ENOTDIR has no test reaching it, and this file's two existing
// tests do not come close - TestClassifyCollectionsRootErrorReturnsEscapeForSymlinkComponent
// exercises the direct symlink-found branch beneath it, and
// TestClassifyCollectionsRootErrorReturnsRawErrorForNonEscapeFailure
// exercises the syscall.ENOTDIR carve-out this branch sits next to. There is
// no deterministic, portable, non-racy way to trigger os.Root's own internal
// escape detection (the unexported "path escapes from parent" refusal named
// two paragraphs up) on a component this walk has already Lstat-ed and
// already confirmed is not itself a symlink.
//
// What would be lost if this branch were removed depends on which of this
// function's three callers hits it. For openCollectionsRoot's create=true
// branch, nothing operationally: the write (root.MkdirAll) has already been
// refused, atomically, by the kernel through os.Root's own containment
// before this function is ever reached, so an unclassified error there only
// changes a real run's error TEXT on a failure that was already certain -
// the "not load-bearing for containment" paragraph above already says this
// for the function as a whole. For dryRunNamespaceProbe (dryrun.go), also
// little: its own doc comment already establishes that an escape-classified
// and an unclassified failure fold identically behind
// helpers.ErrInstallationFailed there, so the collection's own would-fail
// verdict and exit class are unaffected either way - only the per-collection
// cause's identity (whether errors.Is matches helpers.ErrCollectionsPathEscape)
// and the "Would fail" reason text printed for it would change. For
// probeAnsibleCollectionsUsable, by contrast, the stakes are real: unlike
// the other two callers, it has no os.Root write of its own underneath it to
// have already settled anything, so this classification is the only signal
// available for a --dry-run run hitting this exact probe path, and an
// unclassified result there falls through cmd/go-galaxy/exitcode's own
// classification to a generic exit code instead of ExitInstall - fail-safe
// (treat an unrecognized failure as a possible escape) trading for fail-open
// (let it through unclassified) in a way that is not merely cosmetic there.
func classifyCollectionsRootError(root *os.Root, rel string, err error) error {
	var walked string
	for component := range strings.SplitSeq(rel, "/") {
		if walked == "" {
			walked = component
		} else {
			walked = walked + "/" + component
		}
		info, statErr := root.Lstat(walked)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) || errors.Is(statErr, syscall.ENOTDIR) {
				continue
			}
			return fmt.Errorf("%w: %q: point --download-path/collections_path at the real directory instead of a symlink: %w",
				helpers.ErrCollectionsPathEscape, walked, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q: point --download-path/collections_path at the real directory instead of a symlink: %w",
				helpers.ErrCollectionsPathEscape, walked, err)
		}
	}
	return err
}
