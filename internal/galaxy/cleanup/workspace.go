package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// errWorkspaceUnrooted names a project's ansible_collections entry whose
// probe (openProjectWorkspace's root.Stat call) returned a non-nil error
// other than fs.ErrNotExist, as distinct from the entry simply not existing
// or existing as something other than a directory - both ordinary skips,
// neither evidence of an escape. That covers more than os.Root's own
// refusal to resolve an escaping symlink: it also covers any other stat
// failure the probe does not distinguish from one, such as a symlink loop
// (ELOOP) reported directly by the kernel. It never leaves buildReachable:
// openProjectWorkspace's caller renders it into an operator warning and
// moves on, so it carries no exit code and is deliberately not
// helpers.ErrCollectionsPathEscape, which is reserved for install-side
// writes and is mapped to a real exit class in cmd/go-galaxy/exitcode.
var errWorkspaceUnrooted = errors.New("ansible_collections does not resolve inside the project collections path")

// workspace is one project's opened, rooted collections workspace: root and
// fsys are both anchored at the collections path itself, never one level
// down at its ansible_collections subdirectory, because os.OpenRoot follows
// a symlink when establishing the root - rooting at ansible_collections
// would simply adopt whatever it points at, leaving nothing left to refuse.
// This is the same boundary openCollectionsRoot and removeWorkspaceFiles
// already draw on the install and removal sides respectively.
//
// path stays an OS-native absolute string: it becomes an installedCollection's
// CollectionsDir and is what removeWorkspaceFiles later opens its own,
// separate os.Root at. Every path handed to root or fsys, by contrast, is
// slash-separated (built with path.Join, never filepath.Join), matching the
// io/fs convention both APIs use regardless of GOOS.
//
// That handoff is a string, not a live root, and the gap between the two
// opens is real: ws.root itself is closed per project at the end of
// scanProjectWorkspace, well before removeUnused ever runs, while
// removeWorkspaceFiles re-opens its own, separate os.Root from this path
// string alone, only once buildReachable has finished scanning every
// project. os.OpenRoot follows a symlink when establishing a root - the same
// property that lets a symlinked collections path work at all - so a local
// writer able to replace the collections path itself in that window
// redirects both of removeWorkspaceFiles's RemoveAll calls into a tree of
// the writer's own choosing; the deletion stays contained relative to
// whatever that second os.OpenRoot resolves to, but the scan no longer
// controls which tree that turns out to be. This is the same class of
// live-writer residual already accepted for a symlink planted deeper in the
// tree (see removeInstalled's own doc comment below): the attacker already
// needs write access to the collections path to win this race, and gains
// nothing from it that writing there directly would not. Threading ws.root
// through to the removal side instead of re-opening it would trade this
// residual for a wider file-descriptor lifetime and a scan/removal ownership
// split this package does not otherwise have.
type workspace struct {
	root *os.Root
	fsys fs.FS
	path string
}

// maxCollectionsPathCandidates is the number of collections-path candidates
// collectionsPathCandidates can ever produce for one project: the recorded
// CollectionsPath, plus a project-relative ".collections" and "collections"
// fallback.
const maxCollectionsPathCandidates = 3

// collectionsPathCandidates builds the ordered list of collections-path
// candidates for a project: its recorded CollectionsPath first (when set),
// then the project-relative ".collections" and "collections" fallbacks
// (when projectPath is known) - independently of each other, so a project
// can contribute anywhere from zero to three candidates. Which candidate, if
// any, is actually usable is openProjectWorkspace's decision, not this
// function's.
func collectionsPathCandidates(projectPath string, project store.ProjectRecord) []string {
	candidates := make([]string, 0, maxCollectionsPathCandidates)
	if project.CollectionsPath != "" {
		candidates = append(candidates, project.CollectionsPath)
	}
	if projectPath != "" {
		candidates = append(candidates, filepath.Join(projectPath, ".collections"), filepath.Join(projectPath, "collections"))
	}
	return candidates
}

// openProjectWorkspace opens the first usable collections-path candidate for
// a project, rooted at that candidate via os.Root, and returns exactly one of
// three outcomes distinguished without inspecting any error string:
//
//   - (workspace{}, nil): no candidate has an ansible_collections directory -
//     the project has no workspace this run, the ordinary skip case.
//   - (workspace{}, err): a candidate has an ansible_collections entry whose
//     resolution os.Root refuses (errWorkspaceUnrooted) - a symlink escaping
//     that candidate's root, most commonly. The caller must warn and skip the
//     whole project rather than fall through to a later candidate: falling
//     through to, say, a valid sibling ".collections" would silently retarget
//     cleanup at a directory the operator never configured for this project.
//   - (ws, nil): a usable workspace. The caller owns ws.root and must close
//     it once done scanning.
//
// A non-nil statErr other than fs.ErrNotExist - including a symlink loop -
// lands in the escape outcome: os.Root's own refusal is an unexported error
// value matched by no exported sentinel, which is exactly why the split is
// made on the probe's outcome (a non-nil, non-ENOENT statErr, or not) rather
// than by inspecting the error's shape. A successful Stat on an
// ansible_collections entry that turns out not to be a directory is the
// ordinary skip instead, exactly like fs.ErrNotExist: neither is evidence of
// an escape, so this function falls through and tries the next candidate for
// both. This also means the escape is only caught statically, at this
// probe: a local writer that swaps ansible_collections for an escaping
// symlink between this probe and the later directory read
// (scanInstalledCollections) makes that later read fail instead, and that
// failure still aborts the whole run (wrapped by
// scanProjectWorkspace as "failed to scan <path>") rather than being
// downgraded to a skip - separating os.Root's static refusal from a genuine
// IO failure at the read site would require matching that same unexported
// error value, and treating every non-ENOENT read failure as a skip would
// silently swallow real IO errors instead. Aborting on evidence of an active
// local writer is the conservative outcome; the ansible_collections
// misconfiguration this function exists to catch is closed deterministically
// right here. The manifest leaf, further down the same walk, has its own
// static checkpoint for the identical reason (scanCollectionDir's
// manifestIsRegularFile gate) - what still aborts past either checkpoint is
// always a genuine IO failure or a live-writer swap, never a static shape
// either gate was built to catch.
func openProjectWorkspace(projectPath string, project store.ProjectRecord) (workspace, error) {
	for _, candidate := range collectionsPathCandidates(projectPath, project) {
		root, err := os.OpenRoot(candidate)
		if err != nil {
			continue
		}
		info, statErr := root.Stat("ansible_collections")
		switch {
		case statErr == nil && info.IsDir():
			return workspace{root: root, fsys: root.FS(), path: candidate}, nil
		case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
			_ = root.Close()
			return workspace{}, fmt.Errorf("%w: %q: %w", errWorkspaceUnrooted, filepath.Join(candidate, "ansible_collections"), statErr)
		}
		_ = root.Close()
	}
	return workspace{}, nil
}
