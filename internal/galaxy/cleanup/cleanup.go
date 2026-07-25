package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// installedCollection tracks an installed collection discovered on disk.
type installedCollection struct {
	Parsed         *semver.Version
	Key            string
	FQDN           string
	Version        string
	InstallPath    string
	CollectionsDir string
}

type cleanupState struct {
	backend  cacheManager.Backend
	store    *store.Store
	registry *store.ProjectRegistry
	release  func() error
}

// Start runs the cleanup process for unused collections.
func Start(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	var err error
	defer func() {
		if err != nil {
			runtime.Output.Errorf("Error: %s", err.Error())
		}
	}()

	state, err := initCleanup(ctx, cfg, runtime)
	if err != nil {
		return err
	}
	if state == nil {
		// initCleanup returns a non-nil state on success; this guard is
		// defensive and has nothing acquired to release.
		return nil
	}
	// Register the lock release and backend close before the empty-registry
	// check: initCleanup has already acquired the lock and opened the backend
	// by this point, so the no-op branch below must still release both instead
	// of leaking them.
	defer func() {
		if state.release != nil {
			_ = state.release()
		}
	}()
	defer func() {
		if state.backend != nil {
			_ = state.backend.Close(ctx)
		}
	}()

	if state.registry == nil || len(state.registry.Projects) == 0 {
		runtime.Output.Printf("ℹ️ No projects recorded for GC.")
		return nil
	}

	reachable, installedByKey, err := buildReachable(runtime, state.registry)
	if err != nil {
		return err
	}
	removed, err := removeUnused(ctx, cfg, runtime, state.backend, state.store, reachable, installedByKey)
	if err != nil {
		return err
	}
	sweepExtractedStore(cfg, runtime, state.store, reachable, installedByKey)
	return finalizeCleanup(ctx, cfg, runtime, state.backend, state.store, removed)
}

func initCleanup(ctx context.Context, cfg *config.Config, runtime *infra.Infra) (*cleanupState, error) {
	runtime.Output.Printf("🚀 init cache backend")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		return nil, err
	}
	if err := backend.Open(ctx); err != nil {
		return nil, err
	}
	releaseLock, err := backend.Lock(ctx)
	if err != nil {
		_ = backend.Close(ctx)
		return nil, err
	}
	runtime.Output.Printf("🚀 load storage")
	st, err := backend.LoadStore(ctx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return nil, err
	}
	runtime.Output.Printf("🚀 load projects registry")
	registry, err := backend.LoadProjectRegistry(ctx)
	if err != nil {
		_ = releaseLock()
		_ = backend.Close(ctx)
		return nil, err
	}
	return &cleanupState{
		backend:  backend,
		store:    st,
		registry: registry,
		release:  releaseLock,
	}, nil
}

func buildReachable(runtime *infra.Infra, registry *store.ProjectRegistry) (map[string]bool, map[string][]installedCollection, error) {
	reachable := make(map[string]bool)
	installedIndex := make(map[string][]installedCollection)
	depsByKey := make(map[string]map[string]string)
	// installedByKey accumulates every on-disk copy of a given key
	// (ns.name@version): the same collection can be installed under more
	// than one project's collections path, and every copy must be found so
	// removeUnused can remove all of them in a single run rather than
	// overwriting earlier copies and only shedding one per run.
	installedByKey := make(map[string][]installedCollection)
	// constraints caches each raw requirement constraint string's parsed
	// *semver.Constraints (or nil for an unparseable one) across the whole
	// BFS below, so the same constraint evaluated on many edges is parsed
	// at most once instead of once per edge.
	constraints := make(map[string]*semver.Constraints)

	for projectPath, project := range registry.Projects {
		collectionsPath := pickCollectionsPath(projectPath, project)
		if collectionsPath == "" {
			continue
		}
		if err := scanInstalledCollections(runtime.Output, collectionsPath, installedIndex, installedByKey, depsByKey); err != nil {
			return nil, nil, err
		}
		// The project's workspace is present on disk (collectionsPath != "",
		// checked above) and scanInstalledCollections has already populated
		// installedByKey with its installed collections as deletion
		// candidates. A requirements file that cannot be read or parsed here
		// must abort the whole run rather than silently contributing zero
		// reachability roots: the latter would make every uniquely-installed
		// collection under this project look unreachable and get deleted.
		roots, err := loadRequirements(project.RequirementsFile, "")
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %w", helpers.ErrProjectRequirementsUnreadable, project.RequirementsFile, err)
		}
		for _, root := range roots {
			fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
			for _, inst := range selectInstalled(installedIndex, constraints, fqdn, root.Version) {
				markReachable(inst.Key, reachable, depsByKey, installedIndex, constraints)
			}
		}
	}
	return reachable, installedByKey, nil
}

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
	for key, insts := range installedByKey {
		if reachable[key] {
			continue
		}
		removed++
		if cfg.DryRun {
			runtime.Output.Printf("🧹 would remove %s", key)
			continue
		}
		// The same key can be installed under more than one project's
		// collections path; every on-disk copy is removed in this single
		// run, and the snapshot is pruned exactly once afterward rather
		// than once per copy.
		for _, inst := range insts {
			if err := removeInstalled(ctx, inst, backend.Artifacts()); err != nil {
				return removed, err
			}
		}
		runtime.Output.Printf("🧹 removed %s", key)
		if st != nil {
			st.DeleteInstalled(key)
			st.DeleteGraph(key)
			st.DeleteDepsCache(key)
		}
	}
	return removed, nil
}

func finalizeCleanup(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	removed int,
) error {
	if !cfg.DryRun {
		if err := backend.SaveStore(ctx, st); err != nil {
			return err
		}
	}
	if cfg.DryRun {
		runtime.Output.PersistentPrintf("🫡 Dry-run cleanup complete. Candidates: %d", removed)
		return nil
	}
	runtime.Output.PersistentPrintf("✨ Cleanup complete. Removed: %d", removed)
	return nil
}

// pickCollectionsPath chooses the collections path for a project.
func pickCollectionsPath(projectPath string, project store.ProjectRecord) string {
	candidates := []string{}
	if project.CollectionsPath != "" {
		candidates = append(candidates, project.CollectionsPath)
	}
	if projectPath != "" {
		candidates = append(candidates, filepath.Join(projectPath, ".collections"), filepath.Join(projectPath, "collections"))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if dirExists(filepath.Join(candidate, "ansible_collections")) {
			return candidate
		}
	}
	return ""
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// scanInstalledCollections indexes installed collections under collectionsPath.
// Installed collections only ever live at the fixed
// <collectionsPath>/ansible_collections/<ns>/<name>/MANIFEST.json depth, so
// this walks exactly those two directory levels rather than the whole tree:
// a MANIFEST.json nested deeper (e.g. inside a collection's own test
// fixtures) is never mistaken for an installed collection, and the scan does
// not pay for descending into every file of every installed collection.
//
// Manifests whose namespace/name/version cannot be safely used as filesystem
// path elements are rejected at ingestion (see buildInstalledRecord): a
// warning is emitted via out and the scan continues rather than aborting the
// whole run or indexing the tainted record.
func scanInstalledCollections(
	out output.Printer,
	collectionsPath string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	root := filepath.Join(collectionsPath, "ansible_collections")
	nsEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, nsEntry := range nsEntries {
		if !nsEntry.IsDir() {
			continue
		}
		if err := scanNamespaceDir(out, collectionsPath, root, nsEntry.Name(), index, byKey, deps); err != nil {
			return err
		}
	}
	return nil
}

// scanNamespaceDir scans every <root>/<ns>/<name> directory for a
// MANIFEST.json, one namespace at a time.
func scanNamespaceDir(
	out output.Printer,
	collectionsPath, root, ns string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	nameEntries, err := os.ReadDir(filepath.Join(root, ns))
	if err != nil {
		if os.IsNotExist(err) {
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
		if err := scanCollectionDir(out, collectionsPath, root, ns, nameEntry.Name(), index, byKey, deps); err != nil {
			return err
		}
	}
	return nil
}

// scanCollectionDir probes <root>/<ns>/<name>/MANIFEST.json and, if present
// and parseable, indexes the installed collection it describes.
func scanCollectionDir(
	out output.Printer,
	collectionsPath, root, ns, name string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	manifestPath := filepath.Join(root, ns, name, "MANIFEST.json")
	manifest, err := readManifest(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			// A <ns>/<name> directory without a MANIFEST.json is not an
			// installed collection - skip it silently rather than aborting.
			return nil
		}
		if errors.Is(err, helpers.ErrCorruptManifest) {
			// A manifest that cannot be parsed identifies no collection at
			// all: it is neither a reachability source nor a deletion
			// candidate, so it is reported (visible warning) but its
			// on-disk tree is left untouched rather than aborting the scan.
			out.Warnf("skipping corrupt manifest at %s: %v", manifestPath, err)
			return nil
		}
		return err
	}
	record, key, ok, err := buildInstalledRecord(collectionsPath, manifestPath, manifest)
	if err != nil {
		out.Warnf("skipping install with unsafe identifier at %s: %v", manifestPath, err)
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

// readManifest reads and parses a MANIFEST.json. A read failure (including
// os.IsNotExist for a missing file, which the caller checks for) is returned
// as-is. A file that exists but fails to parse as JSON is reported as
// helpers.ErrCorruptManifest wrapping the underlying decode error, rather
// than silently discarding it: the caller decides how to surface that.
func readManifest(path string) (types.GalaxyCollectionVersionInfoManifest, error) {
	//nolint:gosec // path is built from a fixed <ansible_collections>/<ns>/<name>/MANIFEST.json probe.
	data, err := os.ReadFile(path)
	if err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, err
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, fmt.Errorf("%w at %s: %w", helpers.ErrCorruptManifest, path, err)
	}
	return manifest, nil
}

// buildInstalledRecord builds an installedCollection from a parsed manifest.
// An incomplete manifest (missing namespace, name, or version) is a benign
// skip: ok is false and err is nil. A manifest with all three fields present
// but where any of them cannot be safely used as a single filesystem path
// element (e.g. it contains "/" or is "..") is rejected with
// helpers.ErrUnsafeCollectionIdentifier rather than silently building a
// record whose Version would later escape the collections tree in
// removeInstalled.
func buildInstalledRecord(
	collectionsPath string,
	manifestPath string,
	manifest types.GalaxyCollectionVersionInfoManifest,
) (installedCollection, string, bool, error) {
	ns := manifest.CollectionInfo.Namespace
	name := manifest.CollectionInfo.Name
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

// selectInstalled filters installed collections by constraint. constraints
// caches each raw constraint string's parsed *semver.Constraints (or nil for
// an unparseable one) across the whole BFS, so the same requirement string
// evaluated against many edges is parsed at most once. A cache miss is
// distinguished from a cached-nil (parse failure) via the two-value map
// read, so an unparseable constraint is itself parsed only once too.
func selectInstalled(
	index map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
	fqdn, constraint string,
) []installedCollection {
	items := index[fqdn]
	if len(items) == 0 {
		return nil
	}
	normalized := helpers.NormalizeConstraint(constraint)
	if normalized == "" {
		return items
	}
	c, ok := constraints[normalized]
	if !ok {
		c, _ = semver.NewConstraint(normalized)
		constraints[normalized] = c
	}
	if c == nil {
		return items
	}
	out := make([]installedCollection, 0, len(items))
	for _, item := range items {
		if item.Parsed == nil {
			continue
		}
		if c.Check(item.Parsed) {
			out = append(out, item)
		}
	}
	return out
}

// markReachable marks all reachable dependencies starting at key.
func markReachable(
	key string,
	reachable map[string]bool,
	deps map[string]map[string]string,
	index map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
) {
	queue := []string{key}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if reachable[current] {
			continue
		}
		reachable[current] = true
		for depFQDN, constraint := range deps[current] {
			for _, inst := range selectInstalled(index, constraints, depFQDN, constraint) {
				if !reachable[inst.Key] {
					queue = append(queue, inst.Key)
				}
			}
		}
	}
}

// removeInstalled deletes collection files and cached artifacts.
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
func removeInstalled(ctx context.Context, inst installedCollection, artifacts cacheManager.ArtifactStore) error {
	parts := strings.Split(inst.FQDN, ".")
	if len(parts) != helpers.CollectionNameParts {
		return nil
	}
	namespace := parts[0]
	name := parts[1]
	if !helpers.IsPathElement(namespace) || !helpers.IsPathElement(name) || !helpers.IsPathElement(inst.Version) {
		return fmt.Errorf("%w: ns=%q name=%q version=%q", helpers.ErrUnsafeRemovalPath, namespace, name, inst.Version)
	}

	if err := removeWorkspaceFiles(inst, namespace, name); err != nil {
		return err
	}

	if artifacts != nil {
		key := artifactKey(namespace, name, inst.Version)
		_ = artifacts.Delete(ctx, key)
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
// elements from removeInstalled's caller, not derived from inst.InstallPath,
// so this is what root actually resolves regardless of what InstallPath's
// raw string looks like.
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

// artifactKey builds the cache key for an artifact.
func artifactKey(namespace, name, version string) string {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
	return url.QueryEscape(filename)
}

// sweepExtractedStore drops content-addressable extracted entries whose SHA
// is not referenced by any entry in the persisted snapshot's Installed set.
// The snapshot - not an on-disk workspace scan - is the correct source of
// truth here: in a real run, removeUnused has already pruned it down to
// installed entries that are either still reachable or belong to a project
// whose workspace was absent this run (and so was never scanned or pruned at
// all). An on-disk scan would see an empty keep set for every absent
// workspace, which is the normal ephemeral-CI state, and would wipe the
// entire extracted cache.
//
// In a dry run, removeUnused does not prune the snapshot (it only reports
// what it would remove), so the snapshot still contains the about-to-be-
// removed keys. To report the sweep accurately, their SHAs are excluded from
// keep here via reachable/installedByKey - the same two values removeUnused
// used to decide what it would remove - so the reported plan matches what a
// real run would actually do. This exclusion is a no-op in a real run, since
// those keys are already absent from the snapshot by the time this runs.
func sweepExtractedStore(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) {
	if cfg == nil || cfg.CacheDir == "" || st == nil {
		return
	}
	extractedStore := extracted.NewStore(cfg.CacheDir)
	if extractedStore == nil {
		return
	}
	keep := extractedKeepSet(st, reachable, installedByKey)

	if cfg.DryRun {
		reportExtractedSweepPlan(runtime, extractedStore, keep)
		return
	}
	_ = extractedStore.Sweep(keep)
}

// extractedKeepSet builds the set of extracted-store SHAs to keep from the
// persisted snapshot, excluding any key that removeUnused would remove (or
// already removed, in a real run) so a dry-run report matches what a real
// run would actually sweep.
func extractedKeepSet(
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) map[string]bool {
	wouldRemove := make(map[string]bool, len(installedByKey))
	for key := range installedByKey {
		if !reachable[key] {
			wouldRemove[key] = true
		}
	}

	shaByKey := st.InstalledArtifactSHAByKey()
	keep := make(map[string]bool, len(shaByKey))
	for key, sha := range shaByKey {
		if wouldRemove[key] {
			continue
		}
		keep[sha] = true
	}
	return keep
}

// reportExtractedSweepPlan prints, without deleting anything, the extracted
// entries a real run would sweep given keep.
func reportExtractedSweepPlan(runtime *infra.Infra, extractedStore *extracted.Store, keep map[string]bool) {
	plan, err := extractedStore.SweepPlan(keep)
	if err != nil {
		runtime.Output.Errorf("failed to plan extracted cache sweep: %v", err)
		return
	}
	for _, name := range plan {
		runtime.Output.Printf("🧹 would sweep extracted %s", name)
	}
}
