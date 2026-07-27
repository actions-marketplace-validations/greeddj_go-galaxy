package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// resolveCollectionsInternal resolves versions and dependencies for roots.
func resolveCollectionsInternal(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	allowSnapshot bool,
	record bool,
) (map[string]collection, map[string][]string, error) {
	cfg := deps.cfg
	st := deps.st

	reqSpec := buildRequirementsSpec(roots)
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps)

	snapshotAllowed := allowSnapshot && st != nil
	if snapshotAllowed {
		resolvedSnap, graphSnap, ok, err := resolveFromSnapshots(ctx, deps, roots, reqSpec, reqHash)
		if shouldReturnSnapshot(ok, err) {
			return resolvedSnap, graphSnap, err
		}
	}

	resolved, graph, err := solveCollections(ctx, deps, roots)
	if err != nil {
		return nil, nil, err
	}
	recordResolutionIfNeeded(st, record, resolved, graph, reqHash, cfg.Server, reqSpec)
	return resolved, graph, nil
}

func shouldReturnSnapshot(ok bool, err error) bool {
	return ok || err != nil
}

func recordResolutionIfNeeded(
	st *store.Store,
	record bool,
	resolved map[string]collection,
	graph map[string][]string,
	reqHash string,
	server string,
	reqSpec map[string]requirementSpec,
) {
	if !record || st == nil {
		return
	}
	recordResolution(st, resolved, graph, reqHash, server, reqSpec)
}

func resolveFromSnapshots(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	reqSpec map[string]requirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	cfg := deps.cfg
	st := deps.st

	if resolved, graph, ok := loadResolvedFromSnapshot(cfg, st, roots, reqHash); ok {
		return resolved, graph, true, nil
	}
	resolved, graph, ok, err := tryIncrementalResolve(ctx, deps, roots, reqSpec, reqHash)
	if err != nil {
		return nil, nil, false, err
	}
	return resolved, graph, ok, nil
}

func recordResolution(
	st *store.Store,
	resolved map[string]collection,
	graph map[string][]string,
	reqHash string,
	server string,
	reqSpec map[string]requirementSpec,
) {
	setResolvedAll(st, resolved)
	st.SetGraphSnapshot(graph)
	st.SetMetaRequirements(reqHash, server)
	st.SetRequirements(reqSpec)
}

func buildGraphFromDeps(resolved map[string]collection, depsByParent map[string]map[string]string) (map[string][]string, error) {
	graph := make(map[string][]string)
	for parentFQDN, deps := range depsByParent {
		parentCol, ok := resolved[parentFQDN]
		if !ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrMissingResolvedParent, parentFQDN)
		}
		parentKey := parentCol.key()
		depKeys := make([]string, 0, len(deps))
		for depFQDN := range deps {
			depCol, ok := resolved[depFQDN]
			if !ok {
				return nil, fmt.Errorf("%w: %s", helpers.ErrMissingResolvedDependency, depFQDN)
			}
			depKeys = append(depKeys, depCol.key())
		}
		graph[parentKey] = depKeys
	}
	return graph, nil
}

func ensureGraphNodes(resolved map[string]collection, graph map[string][]string) {
	for _, col := range resolved {
		key := col.key()
		if _, ok := graph[key]; !ok {
			graph[key] = nil
		}
	}
}

func cacheDeps(st *store.Store, policy cacheManager.Policy, cacheKey string, deps map[string]string) {
	if st == nil || !policy.Write {
		return
	}
	st.SetDepsCache(cacheKey, deps)
}

func cachedDeps(st *store.Store, policy cacheManager.Policy, cacheKey string) (map[string]string, bool) {
	if st == nil || !policy.Read {
		return nil, false
	}
	deps, ok := st.GetDepsCache(cacheKey)
	return deps, ok
}

// resolvedRoot bundles loadRootMetadataCached's result with the fallback
// versions URL resolveRootMetadata derives from it. It is a struct rather
// than a fourth return value: three of resolveRootMetadata's four callers
// only need two or three of these fields, and a five-value return signature
// fights this repo's lll/revive budget.
type resolvedRoot struct {
	meta        *types.GalaxyCollection
	versionsURL string
	// base is the server that actually answered col's root-metadata fetch -
	// loadRootMetadataCached's own winningBase - never col.Source, which may
	// be empty (an unpinned collection) or stale (a snapshot/legacy-lockfile
	// Source that disagrees with whichever server actually served it).
	base string
}

// resolveRootMetadata loads col's root metadata and derives the versions
// URL callers use to page through its published versions. It fetches first,
// then falls back to collectionVersionsURL built from the winning base
// (never col.Source), and finally overrides that fallback with the root
// metadata's own versions_url, normalized against the same winning base,
// when the metadata provides one - the fallback only matters for a root
// metadata document that omits versions_url.
func resolveRootMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
	label string,
) (resolvedRoot, error) {
	runtime := deps.runtime
	rootMeta, base, err := loadRootMetadataCached(ctx, deps, col, policy)
	if err != nil {
		return resolvedRoot{}, err
	}
	versionsURL := collectionVersionsURL(collection{Namespace: col.Namespace, Name: col.Name, Source: base})
	if rootMeta != nil && rootMeta.VersionsURL != "" {
		versionsURL = normalizeVersionsURL(base, rootMeta.VersionsURL)
		runtime.Output.Debugf("versions URL for %s: %s", label, versionsURL)
	}
	return resolvedRoot{meta: rootMeta, versionsURL: versionsURL, base: base}, nil
}

func extractDependencies(info *types.GalaxyCollectionVersionInfo) map[string]string {
	if len(info.Metadata.Dependencies) > 0 {
		return info.Metadata.Dependencies
	}
	return info.Manifest.CollectionInfo.Dependencies
}

func parseDependencies(deps map[string]string) (map[string]string, error) {
	parsedDeps := make(map[string]string, len(deps))
	for dep, constraint := range deps {
		if _, _, ok := helpers.SplitFQDN(dep); !ok {
			return nil, fmt.Errorf("%w: %q", helpers.ErrInvalidDependencyKey, dep)
		}
		parsedDeps[dep] = strings.TrimSpace(constraint)
	}
	return parsedDeps, nil
}

// loadVersionsListCached loads the available versions list with caching,
// paging through the upstream API in bounded offset increments of
// versionLimit entries per request. Each page still flows through
// fetchJSONWithCachePolicy, so per-page ETag/cache behavior is unchanged.
//
// Pagination is bounded by maxVersionPages: a server that keeps reporting a
// growing total forever (or lies about it) makes the loop fail hard via
// helpers.ErrVersionsPagingExceeded instead of looping unboundedly or
// silently truncating the list a caller then resolves constraints against.
func loadVersionsListCached(
	ctx context.Context,
	deps collectionDeps,
	versionsURL string,
	policy cacheManager.Policy,
) ([]string, error) {
	if versions, ok := cachedVersionsList(deps.st, policy, versionsURL); ok {
		return versions, nil
	}

	var all []string
	offset := 0
	for page := 0; ; page++ {
		versions, total, err := fetchVersionsPage(ctx, deps, policy, versionsURL, versionLimit, offset)
		if err != nil {
			return nil, err
		}
		if page == 0 {
			// Pre-size once, right after the first page is in: use the
			// server's declared total when it reports one, so the common
			// case (a handful of pages) never triggers append's internal
			// growth; otherwise fall back to a single page's worth.
			capacity := versionLimit
			if total > 0 {
				// Clamp to what the loop could ever collect before the
				// maxVersionPages ceiling stops it: the server-declared total
				// is untrusted, so a hostile or broken meta.count must not be
				// allowed to drive the allocation (a huge value would panic
				// make with "cap out of range" long before the ceiling fires).
				capacity = min(total, maxVersionPages*versionLimit)
			}
			all = make([]string, 0, capacity)
		}
		all = append(all, versions...)
		if len(versions) < versionLimit {
			break
		}
		offset += versionLimit
		if total > 0 && offset >= total {
			break
		}
		if page+1 >= maxVersionPages {
			return nil, fmt.Errorf("%w: %s", helpers.ErrVersionsPagingExceeded, versionsURL)
		}
	}

	cacheVersionsList(deps.st, policy, versionsURL, all)
	return all, nil
}

func cachedVersionsList(st *store.Store, policy cacheManager.Policy, versionsURL string) ([]string, bool) {
	if st == nil || !policy.Read || policy.TTL != 0 {
		return nil, false
	}
	versions, ok := st.GetVersionsCache(versionsURL)
	if !ok || len(versions) == 0 {
		return nil, false
	}
	return versions, true
}

// fetchVersionsPage fetches one limit/offset page of the versions list and
// returns its version strings alongside the server's declared total (from
// meta.count or count, depending on payload shape), so the caller can decide
// whether more pages remain.
func fetchVersionsPage(
	ctx context.Context,
	deps collectionDeps,
	policy cacheManager.Policy,
	versionsURL string,
	limit, offset int,
) ([]string, int, error) {
	url := fmt.Sprintf("%s?limit=%d&offset=%d", versionsURL, limit, offset)
	var payload map[string]any
	if err := fetchJSONWithCachePolicy(ctx, deps.runtime.HTTP, url, deps.st, &payload, policy); err != nil {
		return nil, 0, err
	}
	return parseVersionsPayload(payload)
}

func cacheVersionsList(st *store.Store, policy cacheManager.Policy, versionsURL string, versions []string) {
	if st == nil || !policy.Write || policy.TTL != 0 {
		return
	}
	st.SetVersionsCache(versionsURL, versions)
}

// collectionVersionsURL builds the versions API URL for a collection.
func collectionVersionsURL(col collection) string {
	base := strings.TrimRight(col.Source, "/")
	return fmt.Sprintf("%s/api/v3/collections/%s/%s/versions/", base, col.Namespace, col.Name)
}

// normalizeSignatures trims, sorts, and filters signatures.
func normalizeSignatures(signatures []string) []string {
	if len(signatures) == 0 {
		return nil
	}
	out := make([]string, 0, len(signatures))
	for _, value := range signatures {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// normalizeRequirementConstraint normalizes a constraint for hashing.
func normalizeRequirementConstraint(value string) string {
	normalized := helpers.NormalizeConstraint(value)
	if normalized == "" {
		return "*"
	}
	return normalized
}

// requirementSpecEqual reports whether two requirement specs are equal.
func requirementSpecEqual(a, b requirementSpec) bool {
	if a.Constraint != b.Constraint || a.Source != b.Source || a.Type != b.Type {
		return false
	}
	left := normalizeSignatures(a.Signatures)
	right := normalizeSignatures(b.Signatures)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// exactVersionFromConstraints returns a single exact version if specified.
//
// Classification is delegated to the semver library rather than a
// hand-maintained character guard: a constraint is exact only if it parses
// as a bare semver.Version once a single leading "=" is stripped. Anything
// that fails as a version but succeeds as semver.NewConstraint is a genuine
// range or wildcard (including ansible's "1.x" / "1.2.x" x-ranges, which a
// char guard cannot recognize) and contributes no exact pin. A string that
// is neither a valid version nor a valid constraint is malformed.
func exactVersionFromConstraints(constraints []string) (string, bool, error) {
	exact := ""
	for _, raw := range constraints {
		normalized := helpers.NormalizeConstraint(raw)
		if normalized == "" {
			continue
		}
		candidate := normalized
		if after, hasPrefix := strings.CutPrefix(normalized, "="); hasPrefix {
			candidate = strings.TrimSpace(after)
		}
		if _, err := semver.NewVersion(candidate); err != nil {
			if _, cErr := semver.NewConstraint(normalized); cErr != nil {
				return "", false, fmt.Errorf("invalid version constraint %q: %w", raw, cErr)
			}
			// A valid range/wildcard constraint (e.g. ">=1.0.0" or "1.x") is
			// non-exact by definition; it contributes no pin.
			continue
		}
		if exact == "" {
			exact = candidate
			continue
		}
		if exact != candidate {
			return "", false, fmt.Errorf("%w: %s vs %s", helpers.ErrConflictingExactVersions, exact, candidate)
		}
	}
	if exact == "" {
		return "", false, nil
	}
	return exact, true, nil
}

// splitCollectionKey splits a key of the form "ns.name@version".
func splitCollectionKey(key string) (string, string, error) {
	parts := strings.SplitN(key, "@", helpers.CollectionNameParts)
	if len(parts) != helpers.CollectionNameParts || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionKey, key)
	}
	return parts[0], parts[1], nil
}

// collectGraphKeysFromKeys walks the graph starting from root keys.
func collectGraphKeysFromKeys(graph map[string][]string, roots []string) map[string]bool {
	visited := make(map[string]bool)
	queue := make([]string, 0, len(roots))
	for _, key := range roots {
		if !visited[key] {
			visited[key] = true
			queue = append(queue, key)
		}
	}

	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		deps := graph[key]
		for _, dep := range deps {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return visited
}

// sameDeps reports whether two dependency slices contain the same items.
func sameDeps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		if counts[v] == 0 {
			return false
		}
		counts[v]--
	}
	return true
}

// tryIncrementalResolve reuses snapshot data when only some roots changed.
func tryIncrementalResolve(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	currentSpec map[string]requirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	prevSpec := deps.st.RequirementsSnapshot()
	if len(prevSpec) == 0 {
		return nil, nil, false, nil
	}

	// The stored spec alone does not carry the --no-deps mode it was resolved
	// under (RequirementsSnapshot is just the per-root spec map), so recompute
	// its signature in the CURRENT run's mode and require it to match the
	// persisted hash. A --no-deps snapshot (roots only, nil graph edges) then
	// never matches a deps-following recompute, and vice versa: the mismatch
	// falls through to a fresh resolve instead of silently preserving a graph
	// shape from the other mode.
	if requirementsSignatureFromSpec(prevSpec, deps.cfg.NoDeps) != deps.st.MetaSnapshot().RequirementsHash {
		return nil, nil, false, nil
	}

	unchangedRoots, changedRoots := splitRootsByChange(roots, currentSpec, prevSpec)
	if len(unchangedRoots) == 0 || len(changedRoots) == 0 {
		return nil, nil, false, nil
	}

	return tryIncrementalResolveWithSnapshot(ctx, deps, unchangedRoots, changedRoots, currentSpec, reqHash)
}

func tryIncrementalResolveWithSnapshot(
	ctx context.Context,
	deps collectionDeps,
	unchangedRoots []collection,
	changedRoots []collection,
	currentSpec map[string]requirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	resolvedSnap, graphSnap, ok := loadSnapshotData(deps.st)
	if !ok {
		return nil, nil, false, nil
	}

	preservedResolved, preservedGraph, ok := buildPreservedSnapshot(deps.cfg, unchangedRoots, resolvedSnap, graphSnap)
	if !ok {
		return nil, nil, false, nil
	}

	resolvedNew, graphNew, err := resolveCollectionsInternal(ctx, deps, changedRoots, false, false)
	if err != nil {
		return nil, nil, false, err
	}

	mergedResolved, mergedGraph, ok := mergeResolvedGraphs(preservedResolved, preservedGraph, resolvedNew, graphNew)
	if !ok {
		return nil, nil, false, nil
	}

	if !expandGraphFromSnapshot(deps.cfg, mergedResolved, mergedGraph, resolvedSnap, graphSnap) {
		return nil, nil, false, nil
	}
	if !validateMergedGraph(mergedResolved, mergedGraph) {
		return nil, nil, false, nil
	}

	if deps.st != nil {
		recordResolution(deps.st, mergedResolved, mergedGraph, reqHash, deps.cfg.Server, currentSpec)
	}

	return mergedResolved, mergedGraph, true, nil
}

func splitRootsByChange(roots []collection, currentSpec, prevSpec map[string]requirementSpec) ([]collection, []collection) {
	unchangedRoots := make([]collection, 0, len(roots))
	changedRoots := make([]collection, 0, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		current, ok := currentSpec[fqdn]
		if ok {
			if prev, ok := prevSpec[fqdn]; ok && requirementSpecEqual(current, prev) {
				unchangedRoots = append(unchangedRoots, root)
				continue
			}
		}
		changedRoots = append(changedRoots, root)
	}
	return unchangedRoots, changedRoots
}

func loadSnapshotData(st *store.Store) (map[string]store.ResolvedEntry, map[string][]string, bool) {
	resolvedSnap := st.ResolvedSnapshot()
	graphSnap := st.GraphSnapshot()
	if len(resolvedSnap) == 0 || len(graphSnap) == 0 {
		return nil, nil, false
	}
	return resolvedSnap, graphSnap, true
}

func buildPreservedSnapshot(
	cfg *config.Config,
	unchangedRoots []collection,
	resolvedSnap map[string]store.ResolvedEntry,
	graphSnap map[string][]string,
) (map[string]collection, map[string][]string, bool) {
	rootKeys, ok := preservedRootKeys(unchangedRoots, resolvedSnap)
	if !ok {
		return nil, nil, false
	}
	preservedKeys := collectGraphKeysFromKeys(graphSnap, rootKeys)
	preservedGraph := make(map[string][]string, len(preservedKeys))
	preservedResolved := make(map[string]collection)
	for key := range preservedKeys {
		deps, ok := graphSnap[key]
		if !ok {
			return nil, nil, false
		}
		preservedGraph[key] = deps
		if !addPreservedEntry(cfg, preservedResolved, resolvedSnap, key) {
			return nil, nil, false
		}
	}
	return preservedResolved, preservedGraph, true
}

func preservedRootKeys(unchangedRoots []collection, resolvedSnap map[string]store.ResolvedEntry) ([]string, bool) {
	rootKeys := make([]string, 0, len(unchangedRoots))
	for _, root := range unchangedRoots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		entry, ok := resolvedSnap[fqdn]
		if !ok || entry.Version == "" {
			return nil, false
		}
		rootKeys = append(rootKeys, fmt.Sprintf("%s@%s", fqdn, entry.Version))
	}
	return rootKeys, true
}

func addPreservedEntry(
	cfg *config.Config,
	preservedResolved map[string]collection,
	resolvedSnap map[string]store.ResolvedEntry,
	key string,
) bool {
	fqdn, version, err := splitCollectionKey(key)
	if err != nil {
		return false
	}
	entry, ok := resolvedSnap[fqdn]
	if !ok || entry.Version != version {
		return false
	}
	namespace, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return false
	}
	source := entry.Source
	if source == "" {
		source = cfg.Server
	}
	preservedResolved[fqdn] = collection{
		Namespace: namespace,
		Name:      name,
		Version:   version,
		Source:    source,
	}
	return true
}

func mergeResolvedGraphs(
	preservedResolved map[string]collection,
	preservedGraph map[string][]string,
	resolvedNew map[string]collection,
	graphNew map[string][]string,
) (map[string]collection, map[string][]string, bool) {
	mergedResolved := make(map[string]collection, len(preservedResolved)+len(resolvedNew))
	maps.Copy(mergedResolved, preservedResolved)
	for fqdn, col := range resolvedNew {
		if existing, ok := mergedResolved[fqdn]; ok && existing.Version != col.Version {
			return nil, nil, false
		}
		mergedResolved[fqdn] = col
	}

	mergedGraph := make(map[string][]string, len(preservedGraph)+len(graphNew))
	maps.Copy(mergedGraph, preservedGraph)
	for key, deps := range graphNew {
		if existing, ok := mergedGraph[key]; ok {
			if !sameDeps(existing, deps) {
				return nil, nil, false
			}
			continue
		}
		mergedGraph[key] = deps
	}
	return mergedResolved, mergedGraph, true
}

func expandGraphFromSnapshot(
	cfg *config.Config,
	mergedResolved map[string]collection,
	mergedGraph map[string][]string,
	resolvedSnap map[string]store.ResolvedEntry,
	graphSnap map[string][]string,
) bool {
	queue := make([]string, 0, len(mergedGraph))
	for key := range mergedGraph {
		queue = append(queue, key)
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		deps := mergedGraph[key]
		for _, dep := range deps {
			if _, ok := mergedGraph[dep]; ok {
				continue
			}
			depDeps, ok := graphSnap[dep]
			if !ok {
				return false
			}
			mergedGraph[dep] = depDeps
			queue = append(queue, dep)
			if !ensureResolvedFromSnapshot(cfg, mergedResolved, resolvedSnap, dep) {
				return false
			}
		}
	}
	return true
}

func ensureResolvedFromSnapshot(
	cfg *config.Config,
	mergedResolved map[string]collection,
	resolvedSnap map[string]store.ResolvedEntry,
	key string,
) bool {
	fqdn, version, err := splitCollectionKey(key)
	if err != nil {
		return false
	}
	if existing, ok := mergedResolved[fqdn]; ok {
		return existing.Version == version
	}
	entry, ok := resolvedSnap[fqdn]
	if !ok || entry.Version != version {
		return false
	}
	namespace, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return false
	}
	source := entry.Source
	if source == "" {
		source = cfg.Server
	}
	mergedResolved[fqdn] = collection{
		Namespace: namespace,
		Name:      name,
		Version:   version,
		Source:    source,
	}
	return true
}

func validateMergedGraph(mergedResolved map[string]collection, mergedGraph map[string][]string) bool {
	for key := range mergedGraph {
		fqdn, version, err := splitCollectionKey(key)
		if err != nil {
			return false
		}
		entry, ok := mergedResolved[fqdn]
		if !ok || entry.Version != version {
			return false
		}
	}
	return true
}

// buildRequirementsSpec builds a normalized requirement spec map. An
// unpinned root's Source stays "" here rather than defaulting to cfg.Server:
// unpinned is now a distinct, stable spec value (the root walks the
// configured server list instead of being nailed to one), and folding it
// into cfg.Server would make two roots that mean different things ("no
// preference" vs "pinned to the default server") hash identically.
func buildRequirementsSpec(roots []collection) map[string]requirementSpec {
	spec := make(map[string]requirementSpec, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		constraint = normalizeRequirementConstraint(constraint)
		spec[fqdn] = requirementSpec{
			Constraint: constraint,
			Source:     root.Source,
			Type:       root.Type,
			Signatures: normalizeSignatures(root.Signatures),
		}
	}
	return spec
}

// requirementsSignatureFromSpec returns a stable signature of requirements.
// noDeps folds the --no-deps resolution mode into the signature via a
// fixed-position header line, so a snapshot resolved without following
// dependencies (roots only, nil graph edges) can never match - and therefore
// never be reused by - a later run that resolves the full dependency graph,
// and vice versa. The header has zero "|" separators, unlike every per-root
// line (which has four), so it cannot collide with one; it is prepended
// rather than sorted into parts so the per-root ordering stays deterministic.
func requirementsSignatureFromSpec(spec map[string]requirementSpec, noDeps bool) string {
	parts := make([]string, 0, len(spec))
	for fqdn, entry := range spec {
		constraint := entry.Constraint
		if constraint == "" {
			constraint = "*"
		}
		signatureKey := strings.Join(normalizeSignatures(entry.Signatures), ",")
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s", fqdn, constraint, entry.Source, entry.Type, signatureKey))
	}
	sort.Strings(parts)
	header := fmt.Sprintf("no-deps=%t", noDeps)
	sum := sha256.Sum256([]byte(header + "\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// loadResolvedFromSnapshot loads resolved data when requirements match.
func loadResolvedFromSnapshot(
	cfg *config.Config,
	st *store.Store,
	roots []collection,
	reqHash string,
) (map[string]collection, map[string][]string, bool) {
	if !snapshotMatchesRequirements(st, reqHash) {
		return nil, nil, false
	}
	resolvedSnapshot, graphSnapshot, ok := loadSnapshotData(st)
	if !ok {
		return nil, nil, false
	}
	resolved, ok := buildResolvedSnapshot(cfg, resolvedSnapshot)
	if !ok {
		return nil, nil, false
	}
	if !rootsMatchSnapshot(roots, resolved, graphSnapshot) {
		return nil, nil, false
	}
	filtered := filterGraphSnapshot(graphSnapshot, resolved)
	return resolved, filtered, true
}

func snapshotMatchesRequirements(st *store.Store, reqHash string) bool {
	meta := st.MetaSnapshot()
	return meta.RequirementsHash != "" && meta.RequirementsHash == reqHash
}

func buildResolvedSnapshot(cfg *config.Config, resolvedSnapshot map[string]store.ResolvedEntry) (map[string]collection, bool) {
	resolved := make(map[string]collection, len(resolvedSnapshot))
	for fqdn, entry := range resolvedSnapshot {
		if entry.Version == "" {
			return nil, false
		}
		namespace, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, false
		}
		source := entry.Source
		if source == "" {
			source = cfg.Server
		}
		resolved[fqdn] = collection{
			Namespace: namespace,
			Name:      name,
			Version:   entry.Version,
			Source:    source,
		}
	}
	return resolved, true
}

func rootsMatchSnapshot(roots []collection, resolved map[string]collection, graphSnapshot map[string][]string) bool {
	for _, root := range roots {
		if !isGalaxyType(root.Type) {
			return false
		}
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		col, ok := resolved[fqdn]
		if !ok {
			return false
		}
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		ok, err := constraintSatisfied(col.Version, constraint)
		if err != nil || !ok {
			return false
		}
		if _, ok := graphSnapshot[col.key()]; !ok {
			return false
		}
	}
	return true
}

func filterGraphSnapshot(graphSnapshot map[string][]string, resolved map[string]collection) map[string][]string {
	validKeys := make(map[string]bool, len(resolved))
	for _, col := range resolved {
		validKeys[col.key()] = true
	}
	filtered := make(map[string][]string, len(graphSnapshot))
	for key, deps := range graphSnapshot {
		if !validKeys[key] {
			continue
		}
		out := make([]string, 0, len(deps))
		for _, dep := range deps {
			if validKeys[dep] {
				out = append(out, dep)
			}
		}
		filtered[key] = out
	}
	return filtered
}

// constraintSatisfied reports whether version satisfies constraint.
func constraintSatisfied(version, constraint string) (bool, error) {
	normalized := helpers.NormalizeConstraint(constraint)
	if normalized == "" {
		return true, nil
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %w", version, err)
	}
	c, err := semver.NewConstraint(normalized)
	if err != nil {
		return false, fmt.Errorf("invalid constraint %q: %w", normalized, err)
	}
	return c.Check(v), nil
}

// buildInstallLevels topologically groups nodes for installation order.
func buildInstallLevels(graph map[string][]string) ([][]string, error) {
	indegree, reverse := buildDependencyIndex(graph)
	return topologicalLevels(indegree, reverse)
}

func buildDependencyIndex(graph map[string][]string) (map[string]int, map[string][]string) {
	indegree := make(map[string]int)
	reverse := make(map[string][]string)
	for node, deps := range graph {
		if _, ok := indegree[node]; !ok {
			indegree[node] = 0
		}
		for _, dep := range deps {
			indegree[node]++
			reverse[dep] = append(reverse[dep], node)
			if _, ok := indegree[dep]; !ok {
				indegree[dep] = 0
			}
		}
	}
	return indegree, reverse
}

func topologicalLevels(indegree map[string]int, reverse map[string][]string) ([][]string, error) {
	levels := make([][]string, 0)
	for len(indegree) > 0 {
		level := nextLevel(indegree)
		if len(level) == 0 {
			return nil, helpers.ErrDependencyGraphHasACycle
		}
		levels = append(levels, level)
		applyLevel(indegree, reverse, level)
	}
	return levels, nil
}

func nextLevel(indegree map[string]int) []string {
	level := make([]string, 0)
	for node, deg := range indegree {
		if deg == 0 {
			level = append(level, node)
		}
	}
	return level
}

func applyLevel(indegree map[string]int, reverse map[string][]string, level []string) {
	for _, node := range level {
		delete(indegree, node)
		for _, child := range reverse[node] {
			indegree[child]--
		}
	}
}
