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

// errEmptyDownloadPath names the misconfiguration directly rather than
// letting os.OpenRoot("") surface as a bare ENOENT, which tells an operator
// nothing about which setting to fix.
var errEmptyDownloadPath = errors.New("--download-path (or [defaults] collections_path) is empty")

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
type installTarget struct {
	root *os.Root
	rel  string
	path string
	info string
}

// newInstallTarget builds col's installTarget rooted at root, and whether
// col's identity was safe to use at all - the single validating chokepoint
// every construction of both of col's on-disk locations (the install
// directory and the .info sidecar) must go through, so they cannot be
// computed two different ways, one of them unguarded. This replaces the
// former collectionInstallPath and collectionInfoDir: one chokepoint instead
// of two, and the install path now gets the identifier validation only the
// sidecar path previously had.
//
// col.Namespace, col.Name, and col.Version must each be helpers.IsPathElement
// before they are ever joined: path.Join fuses a leading ".." in Version into
// the synthetic "<ns>.<name>-.." element, turning it into a real "up one
// directory" element, so the join absorbs Version's first ".." for free and
// every later ".." in Version pops a real path component off cfg.DownloadPath.
// Validating the joined result after the fact cannot close this - by the time
// a path exists to inspect, the escape has already happened - so the three
// components are what is validated, before any join is computed, and never
// the composed "<ns>.<name>-<version>.info" element: cleanup.go's
// removeInstalled validates the same three components with the same
// predicate, so the deleter never refuses a sidecar directory this function
// created as an unsafe identifier. That symmetry is specific to the sidecar
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
	rel := path.Join("ansible_collections", col.Namespace, col.Name)
	info := path.Join("ansible_collections", fmt.Sprintf("%s.%s-%s.info", col.Namespace, col.Name, col.Version))
	return installTarget{
		root: root,
		rel:  rel,
		path: filepath.Join(cfg.DownloadPath, rel),
		info: info,
	}, true
}

// openCollectionsRoot opens the single os.Root every install-side write
// funnels through, rooted at downloadPath itself rather than at its
// ansible_collections subdirectory - the same boundary cleanup.go's own
// removeWorkspaceFiles draws (see removeInstalled's doc comment there) and
// for the same reason: a swap of ansible_collections itself, not just a
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
// guard, rather than as an error.
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
		return root, nil
	}

	if err := os.MkdirAll(downloadPath, helpers.DirMod); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(downloadPath)
	if err != nil {
		return nil, err
	}
	if err := root.MkdirAll("ansible_collections", helpers.DirMod); err != nil {
		return nil, classifyCollectionsRootError(root, "ansible_collections", err)
	}
	return root, nil
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
