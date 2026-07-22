package cleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver"
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
	sweepExtractedStore(cfg, state.store)
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

func buildReachable(runtime *infra.Infra, registry *store.ProjectRegistry) (map[string]bool, map[string]installedCollection, error) {
	reachable := make(map[string]bool)
	installedIndex := make(map[string][]installedCollection)
	depsByKey := make(map[string]map[string]string)
	installedByKey := make(map[string]installedCollection)

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
			for _, inst := range selectInstalled(installedIndex, fqdn, root.Version) {
				markReachable(inst.Key, reachable, depsByKey, installedIndex)
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
	installedByKey map[string]installedCollection,
) (int, error) {
	var removed int
	for key, inst := range installedByKey {
		if reachable[key] {
			continue
		}
		removed++
		if cfg.DryRun {
			runtime.Output.Printf("🧹 would remove %s", key)
			continue
		}
		if err := removeInstalled(ctx, inst, backend.Artifacts()); err != nil {
			return removed, err
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
// Manifests whose namespace/name/version cannot be safely used as filesystem
// path elements are rejected at ingestion (see buildInstalledRecord): a
// warning is emitted via out and the scan continues rather than aborting the
// whole run or indexing the tainted record.
func scanInstalledCollections(
	out output.Printer,
	collectionsPath string,
	index map[string][]installedCollection,
	byKey map[string]installedCollection,
	deps map[string]map[string]string,
) error {
	root := filepath.Join(collectionsPath, "ansible_collections")
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Name() != "MANIFEST.json" {
			return nil
		}
		manifest, ok, err := readManifest(path)
		if err != nil || !ok {
			return err
		}
		record, key, ok, err := buildInstalledRecord(collectionsPath, path, manifest)
		if err != nil {
			out.Warnf("skipping install with unsafe identifier at %s: %v", path, err)
			return nil
		}
		if !ok {
			return nil
		}
		index[record.FQDN] = append(index[record.FQDN], record)
		byKey[key] = record
		deps[key] = extractDeps(manifest)
		return nil
	})
}

func readManifest(path string) (types.GalaxyCollectionVersionInfoManifest, bool, error) {
	//nolint:gosec // path comes from WalkDir rooted at collectionsPath.
	data, err := os.ReadFile(path)
	if err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, false, err
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, false, nil
	}
	return manifest, true, nil
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
	return installedCollection{
		Key:            key,
		FQDN:           fqdn,
		Version:        version,
		InstallPath:    installPath,
		CollectionsDir: collectionsPath,
	}, key, true, nil
}

func extractDeps(manifest types.GalaxyCollectionVersionInfoManifest) map[string]string {
	if manifest.CollectionInfo.Dependencies != nil {
		return manifest.CollectionInfo.Dependencies
	}
	return map[string]string{}
}

// normalizeConstraint normalizes semver constraints for matching.
func normalizeConstraint(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "*" {
		return ""
	}
	return trimmed
}

// selectInstalled filters installed collections by constraint.
func selectInstalled(index map[string][]installedCollection, fqdn, constraint string) []installedCollection {
	items := index[fqdn]
	if len(items) == 0 {
		return nil
	}
	normalized := normalizeConstraint(constraint)
	if normalized == "" {
		return items
	}
	c, err := semver.NewConstraint(normalized)
	if err != nil {
		return items
	}
	out := make([]installedCollection, 0, len(items))
	for _, item := range items {
		v, err := semver.NewVersion(item.Version)
		if err != nil {
			continue
		}
		if c.Check(v) {
			out = append(out, item)
		}
	}
	return out
}

// markReachable marks all reachable dependencies starting at key.
func markReachable(key string, reachable map[string]bool, deps map[string]map[string]string, index map[string][]installedCollection) {
	queue := []string{key}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if reachable[current] {
			continue
		}
		reachable[current] = true
		for depFQDN, constraint := range deps[current] {
			for _, inst := range selectInstalled(index, depFQDN, constraint) {
				if !reachable[inst.Key] {
					queue = append(queue, inst.Key)
				}
			}
		}
	}
}

// removeInstalled deletes collection files and cached artifacts.
//
// This is a defense-in-depth check: buildInstalledRecord already rejects any
// namespace/name/version that cannot be safely used as a path element at
// ingestion, so the containment failures below should never trigger on a
// record built through the normal scan. If they do, the in-memory state is
// anomalous (e.g. constructed directly rather than scanned), and the safest
// action is to abort rather than guess which part of the path is untrusted.
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

	acRoot := filepath.Join(inst.CollectionsDir, "ansible_collections")
	if inst.InstallPath != "" {
		if !helpers.WithinDir(inst.CollectionsDir, inst.InstallPath) {
			return fmt.Errorf("%w: install path %q escapes %q", helpers.ErrUnsafeRemovalPath, inst.InstallPath, inst.CollectionsDir)
		}
		if err := os.RemoveAll(inst.InstallPath); err != nil {
			return err
		}
	}

	infoDir := filepath.Join(acRoot, fmt.Sprintf("%s.%s-%s.info", namespace, name, inst.Version))
	if !helpers.WithinDir(acRoot, infoDir) {
		return fmt.Errorf("%w: info dir %q escapes %q", helpers.ErrUnsafeRemovalPath, infoDir, acRoot)
	}
	_ = os.RemoveAll(infoDir)

	if artifacts != nil {
		key := artifactKey(namespace, name, inst.Version)
		_ = artifacts.Delete(ctx, key)
	}
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
// truth here: removeUnused has already pruned it down to installed entries
// that are either still reachable or belong to a project whose workspace was
// absent this run (and so was never scanned or pruned at all). An on-disk
// scan would see an empty keep set for every absent workspace, which is the
// normal ephemeral-CI state, and would wipe the entire extracted cache.
func sweepExtractedStore(cfg *config.Config, st *store.Store) {
	if cfg == nil || cfg.DryRun || cfg.CacheDir == "" || st == nil {
		return
	}
	extractedStore := extracted.NewStore(cfg.CacheDir)
	if extractedStore == nil {
		return
	}
	shaByKey := st.InstalledArtifactSHAByKey()
	keep := make(map[string]bool, len(shaByKey))
	for _, sha := range shaByKey {
		keep[sha] = true
	}
	_ = extractedStore.Sweep(keep)
}
