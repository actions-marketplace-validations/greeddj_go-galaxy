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
//
// --refresh bypasses exactly those cached answers that name a collection
// WITHOUT naming a version - "which versions exist", "which is highest", and
// "given these requirements, which versions did the last run pick". It does
// not bypass an answer that already names an exact version: that version's
// metadata document, its declared dependency map, its artifact bytes, or its
// extracted tree - all of those are still served from cache under --refresh,
// exactly as without it. A version-scoped answer is treated as fixed, and
// for a PINNED collection a server that changes one anyway is caught by the
// pin/hash path (verifyPinnedSHA, resolveArtifactSHA re-hashes the actual
// bytes whenever a pin is present), not by cache freshness. For an unpinned
// collection that guarantee does not hold: resolveArtifactSHA trusts the
// cached metadata sha (or the store's recorded sidecar sha) instead of
// re-hashing, so a republished version's changed bytes are never even
// requested, and canSkipInstall/installEntryMatches accepts the cached
// install outright once a matching hash is on record - --refresh changes
// none of that, since the mechanism it bypasses lives entirely upstream of
// this exact-version fetch. Reusing a cached artifact and its cached
// exact-version metadata this way is the safe direction (an unpinned run
// keeps its first-seen bytes rather than adopting new ones sight unseen),
// but it is a real remediation gap, not merely a cache-freshness one: after
// a "we republished this version with a fix" advisory, --refresh alone does
// not force a re-fetch of it. --no-cache (or --clear-cache) is what forces
// that. This function's own snapshotAllowed guard (see
// refreshBypassesSnapshot) is the version-free half of the cache split for
// the resolve snapshot specifically - "given these requirements, which
// versions did the last run pick" is exactly what
// loadResolvedFromSnapshot/tryIncrementalResolve answer from st, below;
// cache.PolicyForConstraint's own exact/non-exact split (policy.go) is the
// identical predicate applied to every HTTP-layer metadata fetch this
// function's fallback solve makes.
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
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps, serversSignature(cfg))

	// The veto is layered over allowSnapshot, not merged into a single
	// expression callers compute themselves: allowSnapshot keeps its
	// existing meaning ("this caller permits reuse"), while
	// refreshBypassesSnapshot is a run-wide policy this function itself
	// enforces regardless of caller, so a future fourth caller of this
	// function cannot forget it - the same structural argument
	// cacheManager.WithStateDeadline's own doc comment makes for wrapping a
	// Backend once instead of guarding nine call sites.
	//
	// Documented-uncovered: warm has no --refresh e2e test of its own, and
	// none is needed to trust this veto for warm specifically. warmWithState
	// reaches this exact call through resolveOrLoadLockfile, the single
	// un-branched entry point install's prepareInstallPlan goes through too;
	// neither caller wraps or special-cases refreshBypassesSnapshot, and the
	// check itself is made once, here, not per caller. A warm-specific test
	// would therefore exercise the identical code path
	// TestRefreshReSolvesInsteadOfReplayingTheSnapshot (e2e_test.go) already
	// does for install, proving nothing a shared call site does not already
	// establish structurally.
	snapshotAllowed := allowSnapshot && st != nil && !refreshBypassesSnapshot(cfg)
	if snapshotAllowed {
		resolvedSnap, graphSnap, ok, err := resolveFromSnapshots(ctx, deps, roots, reqSpec, reqHash)
		if shouldReturnSnapshot(ok, err) {
			return resolvedSnap, graphSnap, err
		}
	}

	// Best-effort, cfg.Workers-bounded warm of the root-metadata documents the
	// sequential solve below is about to request one at a time - see
	// prewarmRootMetadata's own doc comment for the full argument. Its
	// position is load-bearing in one direction: it must stay below the
	// snapshot-replay return above, since a run that replays the snapshot has
	// to keep issuing zero metadata requests, and a prewarm hoisted over that
	// return would issue one per root before the snapshot was ever consulted.
	prewarmRootMetadata(ctx, deps, roots)
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

// refreshBypassesSnapshot reports whether cfg's --refresh should veto
// resolveCollectionsInternal's resolve-snapshot reuse path: the persisted
// "given these requirements, which versions did the last run pick" answer -
// see that function's own doc comment for the full version-free/
// version-scoped predicate this implements one half of. A nil cfg never
// vetoes: resolveCollectionsInternal's own callers always hand it a real
// *config.Config, but this predicate is cheap to make total anyway, rather
// than adding a nil check at its one call site.
//
// --offline outranks --refresh here, not merely by incidental short-circuit
// order: cache.PolicyForConstraint (policy.go) checks IsOffline() before
// IsRefresh() for the identical reason - offline, cached state is the only
// source of truth there is, so refresh has nothing left to re-resolve
// against - and this veto has to agree with that precedence, or the two
// halves of one flag (this snapshot veto and the HTTP-layer policy refresh
// already governs) would disagree about what --refresh --offline means.
func refreshBypassesSnapshot(cfg *config.Config) bool {
	return cfg != nil && cfg.Refresh && !cfg.Offline
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
		// A dependency key is a collection name a Galaxy server chose, and
		// this is the boundary it enters through. Checked for alphabet, not
		// just shape: a key like "evil.pkg\n[CRITICAL] ..." satisfies the
		// shape check, and the solver prints it - on an ordinary run, with no
		// flags - long before anything else would look at it.
		if !helpers.IsCollectionName(dep) {
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
//
// The whole loop - every page, not one budget per page - runs under one
// shared deps.runtime.MetadataDeadline() budget, established once here
// around the `for page := 0; ; page++` loop. This is the one metadata call
// site outside a single fetchJSONBody call where a per-request budget alone
// is not enough, and the reason is not that the request count elsewhere is
// operator- or program-chosen - it usually is not. MetadataProvider's own
// Universe/Dependencies/resolveRoot (internal/galaxy/collections/provider.go)
// issue one root-metadata fetch plus one version-detail fetch per
// (package, version) the solver explores. That multiplier spans two axes -
// which packages get explored, and how many versions each explored package
// has - and both are effectively uncapped. The package axis: the packages
// the solver explores are seeded by the operator's own roots, parsed out of
// requirements.yml by buildSolverRequirements, and then extended
// transitively, without limit, by extractDependencies(info) -
// server-declared metadata naming further packages to fetch. The version
// axis: parseVersionsPayload returns every entry a page's data/results array
// carries, with no truncation to the versionLimit requested, so a server
// that answers a limit=100 request with far more than 100 entries has all
// of them collected regardless. maxVersionPages caps something narrower
// than "how many versions a package can have": it is the number of
// REQUESTS loadVersionsListCached will issue enumerating one package's
// versions (100, after which the loop fails hard via
// helpers.ErrVersionsPagingExceeded rather than truncating), not the
// version count those requests carry. The solver does have a step bound,
// fuelLimit, but at 1,000,000 iterations it is far too large to bound
// network work in any practical sense - it exists to catch an algorithm
// defect, not to cap an input. The request count MetadataProvider issues is
// still server-chosen and effectively unbounded on both axes. What actually
// distinguishes this loop is that it is the one place a SINGLE LOGICAL
// metadata operation (fetch the whole versions list) is split into a
// server-chosen NUMBER of requests against ONE URL - a shared budget is
// coherent there, because it is still bounding one operation. The
// resolver's request count is also server-chosen, but it is a sequence of
// DISTINCT operations (a different package or version each time), for which
// a shared budget is not defensible and is deliberately not applied - each
// of those requests pays its own separate helpers.MetadataFetchDeadline
// instead, and the walk aborts on the very first one that expires.
//
// This leaves a residual: a run at the front of that server-chosen request
// sequence can hold the backend's whole-run exclusive lock (see "Cache
// backend abstraction" in CLAUDE.md) for as long as the sequence takes, and
// that residual is known and deliberately accepted rather than capped. A
// request-count cap bounds the wrong quantity: the server chooses each
// request's own duration within helpers.MetadataFetchDeadline regardless of
// how many requests are allowed, so any count generous enough not to break a
// legitimate large dependency graph still concedes hours of lock hold to a
// server that stalls every request right up to that per-request ceiling.
// Capping the resolve would not even bound the hold on its own: installLevels
// runs under the same lock afterward, paying up to
// helpers.ArtifactDownloadDeadline per artifact for a collection count
// written directly into requirements.yml - an independent contributor to the
// hold, just as uncapped as the resolve. On the shared-cache (S3) backend,
// reaching the lock at all already requires bucket write access -
// Backend.Open's conditional-PUT probe and Lock's own acquireLock both write
// objects, the same trust boundary Backend.LoadStore/LoadProjectRegistry
// establishes in "Cache backend abstraction" - so a principal holding it can
// poison the snapshot outright, which is worse than a denial of service. The
// local backend has no equivalent waiter to starve in the first place: its
// Lock is a non-blocking flock, and a second run fails immediately with
// helpers.ErrAnotherInstanceIsRunning rather than waiting for the first to
// finish. A cap added here would break resolves that work today without
// bounding the hold time it is meant to fix.
//
// Here, the multiplier is bounded at least: maxVersionPages (100) requests
// at up to helpers.MetadataFetchDeadline (2 minutes) each would be 200
// minutes of drip tolerated for a single collection's version list if each
// page paid for its own budget. One shared budget collapses that to 1x at no
// realistic cost: a legitimate server completing all maxVersionPages pages
// inside this one budget needs each page to average well under a second,
// and this test suite's own margin (versions_paging_test.go) demonstrates
// over 3x headroom at a fraction of this budget.
//
// Nesting is safe: fetchJSONBody (via fetchVersionsPage) establishes its own
// per-request context.WithTimeout derived from the dlCtx built here, so its
// effective deadline becomes min(helpers.MetadataFetchDeadline, whatever is
// left of this loop's budget) - the per-request ceiling still holds, and is
// only ever tightened, never loosened, by the outer budget. If this loop's
// budget expires while a page is in flight, that inner fetchJSONBody's own
// deadlineError sees a parent (this loop's dlCtx) whose Err() is already
// non-nil and passes its error through unchanged (idempotence rule 2); the
// single cacheManager.MetadataDeadlineError call below then normalizes it
// into exactly one sentinel, never two.
func loadVersionsListCached(
	ctx context.Context,
	deps collectionDeps,
	versionsURL string,
	policy cacheManager.Policy,
) ([]string, error) {
	if versions, ok := cachedVersionsList(deps.st, policy, versionsURL); ok {
		return versions, nil
	}

	budget := deps.runtime.MetadataDeadline()
	dlCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var all []string
	offset := 0
	for page := 0; ; page++ {
		versions, total, err := fetchVersionsPage(dlCtx, deps, policy, versionsURL, versionLimit, offset)
		if err != nil {
			return nil, cacheManager.MetadataDeadlineError(ctx, dlCtx, budget, err)
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
	if err := fetchJSONWithCachePolicy(ctx, deps.runtime, url, deps.st, &payload, policy); err != nil {
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
	if requirementsSignatureFromSpec(prevSpec, deps.cfg.NoDeps, serversSignature(deps.cfg)) != deps.st.MetaSnapshot().RequirementsHash {
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
//
// Two fixed-position header lines precede the sorted per-root lines. noDeps
// folds the --no-deps resolution mode in, so a snapshot resolved without
// following dependencies (roots only, nil graph edges) can never match - and
// therefore never be reused by - a later run that resolves the full graph,
// and vice versa. serversSig folds in the effective server list, because
// under first-match ownership a root with no source: of its own resolves
// against whichever configured server answers first: the same requirements
// against a different list, or the same list in a different order, are a
// different resolution problem and must not reuse each other's answer.
//
// Both headers have zero "|" separators, unlike every per-root line (which
// has exactly four), so neither can collide with one; their distinct literal
// prefixes keep them from colliding with each other. They are prepended
// rather than sorted into parts, so the per-root ordering stays
// deterministic.
func requirementsSignatureFromSpec(spec map[string]requirementSpec, noDeps bool, serversSig string) string {
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
	header := fmt.Sprintf("no-deps=%t\nservers=%s", noDeps, serversSig)
	sum := sha256.Sum256([]byte(header + "\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// serversSignature hashes cfg's effective server list for the "servers="
// header of requirementsSignatureFromSpec.
//
// Order is preserved, never sorted: under first-match ownership the list
// order decides which server owns a collection, so a reorder is a genuine
// change of meaning. Fields are NUL-separated, a byte no URL or id can
// contain, so no value can forge a field boundary; the result is hex, which
// is what guarantees the header line it feeds can never contain a "|" and
// therefore can never be mistaken for a per-root line.
//
// Only whether a server carries a credential is hashed, never the token and
// never any value derived from it - the signature is persisted in the
// snapshot, and no token-derived value may ever land there. A token rotated
// to a different value therefore leaves the signature untouched, which is
// correct: the same server still serves the same collections. Going from no
// token to a token does flip the bit, and that is the case that matters,
// since an authenticated read can reveal collections an anonymous one could
// not see.
//
// A configured-but-unused server also changes the signature. That is
// deliberately conservative: whether a server is "unused" is not knowable
// without resolving, so the cost is one cold resolve and the benefit is that
// nobody has to reason about which additions could matter.
func serversSignature(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}

	servers := cfg.Servers
	if len(servers) == 0 {
		// The shape a hand-built config (and every caller predating
		// multi-server support) still has: one effective server, no
		// credential, matching what unpinnedServerCandidates falls back to.
		servers = []config.Server{{URL: cfg.Server}}
	}

	entries := make([]string, 0, len(servers))
	for _, srv := range servers {
		auth := "0"
		if srv.Token.IsSet() {
			auth = "1"
		}
		entries = append(entries, srv.ID+"\x00"+srv.URL+"\x00"+auth)
	}
	sum := sha256.Sum256([]byte(strings.Join(entries, ",")))
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
