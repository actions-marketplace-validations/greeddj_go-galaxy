package cleanup

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

func removeUnused(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) (int, error) {
	var removed int
	// Sorted rather than ranged directly: this loop returns on the first
	// removal failure below, so which subset of installedByKey was already
	// deleted before that return must not depend on Go's randomized map
	// iteration order - the same failure, on the same recorded keys, must
	// leave the same partial result on disk every time.
	for _, key := range slices.Sorted(maps.Keys(installedByKey)) {
		// Read before the reachable skip below, deliberately, and this is the
		// difference between a claim that holds and one that only looks like
		// it does: placed after the skip, the loop would stop only once it
		// reached a key it was actually going to remove, so a cancellation
		// landing in a long run of reachable keys would be honored an
		// arbitrary number of iterations late. Placed here, "this loop stops
		// at the next collection" is a property of every iteration.
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("cleanup stopped at %s: %w", key, err)
		}
		insts := installedByKey[key]
		if reachable[key] {
			continue
		}
		removed++
		// key is "<ns>.<name>@<version>", built by buildInstalledRecord only
		// after ns, name, and version each pass helpers.IsPathElement - which
		// rejects every control character, \n and \t included (see
		// IsPathElement's own doc comment). key therefore can never carry a
		// character able to forge an extra line or a terminal command, so
		// this line, the "removed" line below, and the stop error above all
		// print it with a bare %s rather than %q: quoting would change the
		// exact, greppable shape CI tooling matches against (e.g.
		// "🧹 removed ns.name@1.0.0").
		if cfg.DryRun {
			runtime.Output.Printf("Would remove %s", key)
			continue
		}
		// The persisted InstalledEntry's own Source - not any field on the
		// on-disk installedCollection scan, which never recorded a server at
		// all - is the only place the server this collection actually
		// resolved from is available, and it is what the current, scoped
		// artifact key (helpers.ArtifactKey) must be built from.
		source := installedSource(st, key)
		// The same key can be installed under more than one project's
		// collections path; every on-disk copy is removed in this single
		// run, and the snapshot is pruned exactly once afterward rather
		// than once per copy.
		for _, inst := range insts {
			if err := removeInstalled(ctx, inst, backend.Artifacts(), source); err != nil {
				return removed, err
			}
		}
		runtime.Output.Printf("Removed %s", key)
		if st != nil {
			st.DeleteInstalled(key)
			st.DeleteGraph(key)
			// The deps-cache entry for this key, if any, lives under the
			// server-scoped key (helpers.ScopedDepsCacheKey) the resolve path
			// wrote it under, not under key itself - a bare "ns.name@version"
			// delete would never match anything once every live entry carries
			// the server-base prefix. Without source (e.g. this collection's
			// InstalledEntry predates the multi-server work, or was never
			// recorded), there is no way to know which scoped key to target,
			// so the stale entry is simply left for CacheEntryMaxAge to evict.
			if source != "" {
				st.DeleteDepsCache(helpers.ScopedDepsCacheKey(source, key))
			}
		}
	}
	return removed, nil
}

// installedSource returns the server the collection recorded under key
// actually resolved from - the persisted InstalledEntry's own Source, the
// only place that survives from resolve time, since the on-disk manifest
// scan (installedCollection) never records a server at all. It returns ""
// when st is nil or has no entry for key, in which case the caller cannot
// build a correctly scoped artifact or deps-cache key and must skip that
// part of the purge rather than guess.
func installedSource(st *store.Store, key string) string {
	if st == nil {
		return ""
	}
	entry, ok := st.GetInstalled(key)
	if !ok {
		return ""
	}
	return entry.Source
}

// removeInstalled deletes collection files and cached artifacts.
//
// namespace and name below are read directly from inst.Namespace/inst.Name -
// the ansible_collections/<ns>/<name> directory pair the scan walked through
// to find this record's manifest, never anything the manifest itself
// declared - so nothing a hostile MANIFEST.json contains can retarget this
// removal at a different collection's directory.
//
// The WithinDir checks in removeInstallPath/removeInfoDir are
// defense-in-depth: buildInstalledRecord already rejects any
// namespace/name/version that cannot be safely used as a path element at
// ingestion, so these containment failures should never trigger on a record
// built through the normal scan. If they do, the in-memory state is
// anomalous (e.g. constructed directly rather than scanned), and the safest
// action is to abort rather than guess which part of the path is untrusted.
//
// The load-bearing escape prevention is os.Root, opened in
// removeWorkspaceFiles: both on-disk deletions are rooted at
// inst.CollectionsDir (the operator-configured, trusted collections_path)
// rather than at its ansible_collections subdirectory, so a local attacker
// who swaps either the ansible_collections directory or the namespace
// component for a symlink between the scan and this call cannot make either
// RemoveAll follow it out of the tree - os.Root refuses to traverse a
// symlink that escapes its root, closing a TOCTOU that a purely lexical
// WithinDir check cannot catch. A symlink placed deeper - e.g. redirecting
// the collection's own <ns>/<name> directory to another location still
// inside collections_path - is not defended here: the attacker already needs
// workspace write access to plant it, and could just as easily delete that
// same target directly, so os.Root's guarantee (no escape outside the root)
// is the property that actually matters.
//
// The artifact purge runs after removeWorkspaceFiles regardless of whether
// the workspace itself was present: the artifact store is independent of the
// on-disk collections tree, so an absent (or already-swept) workspace must
// not gate it - otherwise a cached tarball for an unreachable collection
// would leak whenever its project's workspace happens to be gone this run.
//
// source is the server this collection actually resolved from (the
// persisted InstalledEntry's own Source, via installedSource) - possibly ""
// when unknown - and is used to purge its current, server-scoped artifact
// cache entry (helpers.ArtifactKey). It does not purge the pre-multi-server
// flat-keyed entry (legacyArtifactKey): that purge runs unconditionally for
// every scanned collection, reachable or not, via sweepLegacyArtifacts,
// since a legacy-keyed entry can never be reached by a fresh install again
// regardless of whether its collection is being removed.
func removeInstalled(ctx context.Context, inst installedCollection, artifacts cacheManager.ArtifactStore, source string) error {
	namespace := inst.Namespace
	name := inst.Name
	if !helpers.IsPathElement(namespace) || !helpers.IsPathElement(name) || !helpers.IsPathElement(inst.Version) {
		return fmt.Errorf("%w: ns=%q name=%q version=%q", helpers.ErrUnsafeRemovalPath, namespace, name, inst.Version)
	}

	if err := removeWorkspaceFiles(inst, namespace, name); err != nil {
		return err
	}

	if artifacts != nil && strings.TrimSpace(source) != "" {
		filename := helpers.ArtifactFilename(namespace, name, inst.Version)
		_ = artifacts.Delete(ctx, helpers.ArtifactKey(source, filename))
	}
	return nil
}

// removeWorkspaceFiles removes the collection's on-disk install directory
// and .info sidecar, both rooted at inst.CollectionsDir via a single
// os.Root (see removeInstalled's doc comment for why that root prevents a
// symlink-swap escape). namespace and name are the already-validated path
// elements from removeInstalled's caller.
func removeWorkspaceFiles(inst installedCollection, namespace, name string) error {
	root, err := os.OpenRoot(inst.CollectionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No on-disk workspace files to remove: the collections dir this
			// record refers to is already gone. The caller still proceeds to
			// purge the cached artifact - that store is independent of this
			// workspace's presence.
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()

	if err := removeInstallPath(root, inst, namespace, name); err != nil {
		return err
	}
	return removeInfoDir(root, inst, namespace, name)
}

// removeInstallPath removes the collection's own install directory
// (ansible_collections/<namespace>/<name>) through root, rooted at
// inst.CollectionsDir, so a symlink swap of either the ansible_collections
// directory or the namespace component cannot make the removal escape the
// collections tree. namespace and name are the already-validated path
// elements from removeInstalled's caller - the same ansible_collections/<ns>/
// <name> pair buildInstalledRecord read inst.InstallPath from in the first
// place (installPath := filepath.Dir(manifestPath), where manifestPath is
// exactly ansible_collections/<ns>/<name>/MANIFEST.json). The walked identity
// and the recorded InstallPath name the identical on-disk location by
// construction, not two independently-derived values that merely happen to
// agree.
//
// The InstallPath == "" and WithinDir guards below are belt-and-suspenders
// over that construction, not the load-bearing check: they exist for a
// record that never went through the normal scan at all - one built
// directly, as several tests in this package do - where InstallPath could
// disagree with namespace/name.
// TestRemoveInstalledRejectsInstallPathEscape pins exactly that
// directly-constructed case: the last lexical check before a RemoveAll that
// a record produced by the real scan can never actually trigger.
func removeInstallPath(root *os.Root, inst installedCollection, namespace, name string) error {
	if inst.InstallPath == "" {
		return nil
	}
	if !helpers.WithinDir(inst.CollectionsDir, inst.InstallPath) {
		return fmt.Errorf("%w: install path %q escapes %q", helpers.ErrUnsafeRemovalPath, inst.InstallPath, inst.CollectionsDir)
	}
	installRel := filepath.Join("ansible_collections", namespace, name)
	return root.RemoveAll(installRel)
}

// removeInfoDir best-effort removes the collection's .info sidecar
// directory (ansible_collections/<namespace>.<name>-<version>.info) through
// root, the same way removeInstallPath does. Only the WithinDir containment
// failure is surfaced as an error; a RemoveAll failure itself is ignored,
// preserving this call's pre-existing best-effort semantics.
func removeInfoDir(root *os.Root, inst installedCollection, namespace, name string) error {
	acRoot := filepath.Join(inst.CollectionsDir, "ansible_collections")
	infoName := fmt.Sprintf("%s.%s-%s.info", namespace, name, inst.Version)
	infoDir := filepath.Join(acRoot, infoName)
	// Unreachable by construction (belt-and-suspenders): namespace, name, and
	// inst.Version were already IsPathElement-checked by removeInstalled, so
	// infoDir is always a clean single element under acRoot; kept because a
	// future refactor breaking that containment right before RemoveAll would
	// be catastrophic.
	if !helpers.WithinDir(acRoot, infoDir) {
		return fmt.Errorf("%w: info dir %q escapes %q", helpers.ErrUnsafeRemovalPath, infoDir, acRoot)
	}
	_ = root.RemoveAll(filepath.Join("ansible_collections", infoName))
	return nil
}
