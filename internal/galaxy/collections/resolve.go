package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// resolveMode selects how much of the persisted resolve state a
// resolveCollectionsInternal call may touch. Snapshot reuse and result
// recording always move together, which is why this is one two-valued mode
// rather than two independent booleans: a call that may replay the persisted
// whole-requirements snapshot is exactly the call whose result is that
// snapshot's next value, and a call resolving only a subset of the run's
// requirements must do neither.
type resolveMode int

const (
	// resolveTopLevel is a whole-requirements resolve: it may reuse the
	// persisted resolve snapshot (still subject to refreshBypassesSnapshot's
	// run-wide veto) and records its result back into the store.
	resolveTopLevel resolveMode = iota
	// resolveNestedPartial is the changed-roots-only re-solve inside
	// tryIncrementalResolveWithSnapshot: its roots are a subset of the run's
	// requirements, so the whole-requirements snapshot must not answer for
	// them, and its partial result must not be recorded as if it were the
	// whole resolution - the caller records the merged graph itself.
	resolveNestedPartial
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
	mode resolveMode,
) (map[string]collection, map[string][]string, error) {
	cfg := deps.cfg
	st := deps.st
	allowSnapshot := mode == resolveTopLevel

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
	recordResolutionIfNeeded(st, mode == resolveTopLevel, resolved, graph, reqHash, cfg.Server, reqSpec)
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
	reqSpec map[string]store.RequirementSpec,
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
	reqSpec map[string]store.RequirementSpec,
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
	reqSpec map[string]store.RequirementSpec,
) {
	setResolvedAll(st, resolved)
	st.SetGraphSnapshot(graph)
	st.SetMetaRequirements(reqHash, server)
	st.SetRequirements(reqSpec)
}

// setResolvedAll stores resolved collection versions in the snapshot.
func setResolvedAll(st *store.Store, resolved map[string]collection) {
	if st == nil {
		return
	}
	entries := make(map[string]store.ResolvedEntry, len(resolved))
	for fqdn, col := range resolved {
		entries[fqdn] = store.ResolvedEntry{Version: col.Version, Source: col.Source}
	}
	st.SetResolvedAll(entries)
}

func buildGraphFromDeps(resolved map[string]collection, depsByParent map[string]map[string]string) (map[string][]string, error) {
	graph := make(map[string][]string, len(depsByParent))
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
//
// It fails, rather than resolving, when that normalization refuses the
// server's own versions_url: normalizeVersionsURL guards what it returns,
// so a value carrying userinfo never becomes a request URL here. The
// fallback this function builds itself is not subject to that, since it is
// this program's own construction rather than the server's.
//
// Surviving that guard is not what makes the debug line below safe to print,
// and the line does not rely on it: checkMetadataURLUserinfo passes through
// every value url.Parse refuses, so the render is cut through
// helpers.WithoutCredentials, which needs no successful parse to cut.
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
		versionsURL, err = normalizeVersionsURL(base, rootMeta.VersionsURL)
		if err != nil {
			return resolvedRoot{}, err
		}
		runtime.Output.Debugf("versions URL for %s: %s", label, helpers.WithoutCredentials(versionsURL))
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
// growing total forever (or lies about it) makes the walk fail hard via
// helpers.ErrVersionsPagingExceeded instead of paging unboundedly or
// silently truncating the list a caller then resolves constraints against.
//
// Page 0 is fetched first, alone. When it reports a positive total, that
// total schedules the remaining, now-bounded page offsets, which are
// fetched concurrently (bounded by cfg.DownloadWorkers, the same
// network-bound sizing the artifact prefetcher's pool uses) and consumed
// strictly in offset order, so the list this function returns - and
// cacheVersionsList persists - is the one an offset-ordered walk of the
// same responses produces, regardless of arrival order. The declared total
// bounds only that SCHEDULING, never trust: the walk re-judges termination
// page by page from what each page actually returned (a short or empty page
// ends the list there, discarding every later offset's result, content and
// error alike; a page's own reported total ends it once the next offset
// would pass that total), so a server whose total lied - promising pages it
// then serves short, empty, or not at all - yields exactly the list a
// strictly sequential walk would have collected, and a page past the
// schedule that the walk still wants is fetched sequentially, on demand,
// under the same ceiling. A page-0 total implying more than maxVersionPages
// pages fails hard up front, before any further request - the same
// never-truncate verdict the walk itself reaches at the ceiling - and a
// total of 0 (unreported) schedules nothing: every page after the first is
// then fetched sequentially, terminating on the identical conditions.
//
// The whole operation - every page, not one budget per page - runs under
// one shared deps.runtime.MetadataDeadline() budget, established once here
// around page 0 and every page after it. This is the one metadata call
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
// left of this operation's budget) - the per-request ceiling still holds,
// and is only ever tightened, never loosened, by the outer budget. If this
// operation's budget expires while a page is in flight, that inner
// fetchJSONBody's own deadlineError sees a parent (this operation's dlCtx)
// whose Err() is already non-nil and passes its error through unchanged
// (idempotence rule 2); versionsPager.fetchPage's single
// cacheManager.MetadataDeadlineError call then normalizes it into exactly
// one sentinel, never two.
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

	pager := &versionsPager{
		parent:      ctx,
		dlCtx:       dlCtx,
		deps:        deps,
		versionsURL: versionsURL,
		policy:      policy,
		budget:      budget,
	}
	all, err := pager.collectAll()
	if err != nil {
		return nil, err
	}
	cacheVersionsList(deps.st, policy, versionsURL, all)
	return all, nil
}

// versionsPager carries one loadVersionsListCached call's shared state: the
// caller's own context (parent, consulted only to classify a failing page's
// error), the budget-bounded context every page request runs under (dlCtx),
// and the request parameters every page shares. See loadVersionsListCached's
// doc comment for the paging strategy these methods implement together.
type versionsPager struct {
	//nolint:containedctx // the caller's original context, consulted only
	// to classify a failing page's error as caller-canceled versus
	// budget-expired; it never starts work of its own.
	parent context.Context
	//nolint:containedctx // the budget-bounded context every page request
	// runs under; fetchPage funnels both walkPages and the prefetch
	// workers through it, so it lives on the shared state rather than in
	// each call's signature.
	dlCtx       context.Context
	deps        collectionDeps
	versionsURL string
	policy      cacheManager.Policy
	budget      time.Duration
}

// pageResult is one fetched page: its version strings, the total the server
// declared alongside them, and the fetch's error, already normalized by
// fetchPage.
type pageResult struct {
	err      error
	versions []string
	total    int
}

// fetchPage fetches the page at offset under the shared budget. It is the
// single funnel every page request and its error classification go through:
// a failure is normalized here, via cacheManager.MetadataDeadlineError, into
// at most one helpers.ErrMetadataFetchDeadline sentinel, whether the page
// was fetched by walkPages directly or by a prefetchScheduled worker.
func (p *versionsPager) fetchPage(offset int) pageResult {
	versions, total, err := fetchVersionsPage(p.dlCtx, p.deps, p.policy, p.versionsURL, versionLimit, offset)
	return pageResult{versions: versions, total: total, err: cacheManager.MetadataDeadlineError(p.parent, p.dlCtx, p.budget, err)}
}

// pagingExceeded builds the hard-failure verdict for a versions list that
// needs more than maxVersionPages requests. Cut like every other render of
// this value: versionsURL is the server's own versions_url when the root
// metadata declared one, and this verdict means the server kept declaring
// more pages, which is not the behavior to hand an uncut URL to a log for.
func (p *versionsPager) pagingExceeded() error {
	return fmt.Errorf("%w: %s", helpers.ErrVersionsPagingExceeded, helpers.WithoutCredentials(p.versionsURL))
}

// collectAll fetches page 0 and decides how the rest of the list is
// collected: a short page 0 is already the whole list; a positive total
// implying more pages than maxVersionPages fails hard before any further
// request; a positive total page 0 already covers needs nothing more; and
// otherwise the scheduled offsets are prefetched concurrently and consumed
// by the walk, with an empty schedule when the total is 0 or unreported.
//
// The pre-size below is the one thing the declared total is taken at its
// word for, and only after the pages-exceeded verdict has bounded it: a
// hostile or broken meta.count large enough to matter to an allocation has
// already failed the call above, never reaching make.
func (p *versionsPager) collectAll() ([]string, error) {
	first := p.fetchPage(0)
	if first.err != nil {
		return nil, first.err
	}
	if len(first.versions) < versionLimit || (first.total > 0 && versionLimit >= first.total) {
		return first.versions, nil
	}
	if first.total > maxVersionPages*versionLimit {
		return nil, p.pagingExceeded()
	}
	var scheduled []pageResult
	capacity := versionLimit
	if first.total > 0 {
		scheduled = p.prefetchScheduled(first.total)
		capacity = first.total
	}
	return p.walkPages(first.versions, scheduled, capacity)
}

// prefetchScheduled fetches, concurrently, every page offset the declared
// total schedules beyond page 0, bounded by cfg.DownloadWorkers - the same
// network-bound sizing the artifact prefetcher's own pool uses (see
// startPrefetchWorkers), and safe over the shared store for the same reason
// that pool already is: each page's fetchJSONWithCachePolicy writes a
// distinct APICache key behind the store's own mutex. Workers write disjoint
// slice elements, so no result mutex is needed; walkPages afterwards is what
// decides which of these results the list actually keeps, in offset order.
func (p *versionsPager) prefetchScheduled(total int) []pageResult {
	results := make([]pageResult, (total-1)/versionLimit)
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(p.deps.cfg.DownloadWorkers, 1))
	for i := range results {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = p.fetchPage((i + 1) * versionLimit)
		})
	}
	wg.Wait()
	return results
}

// walkPages assembles the final list. It is the paging walk itself, with the
// pages the schedule prefetched consumed in place of a fresh request:
// termination is judged page by page against what each page actually
// returned (its length and its own declared total), never against the
// schedule, so a scheduled page past the point the walk ends is discarded
// unread - content and error alike, exactly as an unfetched page would have
// been - and a page the schedule never covered is fetched on demand. The
// maxVersionPages ceiling binds the walk regardless of how a page was
// obtained.
func (p *versionsPager) walkPages(first []string, scheduled []pageResult, capacity int) ([]string, error) {
	all := make([]string, 0, capacity)
	all = append(all, first...)
	offset := versionLimit
	for page := 1; ; page++ {
		if page >= maxVersionPages {
			return nil, p.pagingExceeded()
		}
		result := p.pageAt(page, offset, scheduled)
		if result.err != nil {
			return nil, result.err
		}
		all = append(all, result.versions...)
		if len(result.versions) < versionLimit {
			return all, nil
		}
		offset += versionLimit
		if result.total > 0 && offset >= result.total {
			return all, nil
		}
	}
}

// pageAt serves the walk's page from the prefetched schedule when the
// schedule covers it, and fetches it fresh otherwise.
func (p *versionsPager) pageAt(page, offset int, scheduled []pageResult) pageResult {
	if page <= len(scheduled) {
		return scheduled[page-1]
	}
	return p.fetchPage(offset)
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
//
// The query of each source is also cut, via helpers.WithoutQuery - the same
// cut and the same reason it makes for GALAXY.yml: a signature source is
// repository content that may name a presigned download URL, whose query
// string is a time-limited capability, and the requirement spec this feeds is
// persisted both to the requirements Bolt bucket and to the S3 backend's
// snapshot object, shared across runners.
//
// The cut is free because of what this function's callers are, stated as a
// predicate rather than a list: every consumer of a normalized spec is a spec
// comparison or a spec hash, never a fetch - a run's own live sources are
// read from requirementSources(roots) instead, upstream of this function
// entirely and untouched by it. What it gives up: two sources differing only
// in their query now compare equal, so a query-only edit to a source no
// longer counts as a root change, and the prior resolve snapshot is replayed
// rather than re-resolved. That costs nothing, because a signature source
// plays no part in version resolution, and verification still reads the
// live, unstripped source.
//
// This cut has to run here, at the producer, even though a second one
// catches everything on the way out: store.snapshotData's own persist-side
// backstop (copyRequirementsCutQuery, internal/galaxy/store/snapshot.go)
// strips the identical query from every Requirements entry on every save.
// Dropping this cut and trusting that backstop alone would leave the live
// reqSpec this function feeds - and therefore reqHash, the hash
// requirementsSignatureFromSpec computes over it - carrying the query, while
// the persisted spec the backstop wrote is stripped. tryIncrementalResolve's
// own self-consistency check recomputes its hash from that persisted,
// stripped spec (via RequirementsSnapshot, never the live reqSpec) and
// compares it against the persisted hash, itself computed unstripped: the
// two would never agree again, for any requirement set naming a
// query-bearing signature source, on any run, for as long as the drop
// stood. Nothing fails outright - the whole-snapshot replay
// (loadResolvedFromSnapshot) is unaffected, since it compares the live hash
// against the persisted one directly rather than through the persisted
// spec, and a full resolve still succeeds whenever the incremental path
// declines - so the only casualty would be the incremental path itself,
// dying silently with no error to notice it by.
//
// No schema bump follows from this: the field's shape is unchanged. The
// stored hash and the stored spec are not always written together, though,
// which is what makes "one binary writes both" unsafe to assume:
// recordResolution is the only call that writes them in lockstep
// (SetMetaRequirements immediately followed by SetRequirements), while the
// persist-side backstop above rewrites the persisted spec on every dirty
// save regardless of whether recordResolution ran this particular run.
// install --frozen is the shape that reaches this today:
// resolveOrLoadLockfile's lockfile branch never calls recordResolution at
// all, yet a frozen install still dirties the store (SetInstalled) and
// still saves - so a spec an older, pre-cut binary once persisted
// unstripped is rewritten stripped by the backstop, while
// Meta.RequirementsHash, computed by that older binary over the unstripped
// value, survives the save completely untouched. Two binaries end up
// having written the two fields, not one, and the run that later reads
// them back hits the identical mismatch the upgrade direction below
// describes - not a third direction, just another way to arrive at the
// same one - and pays the identical one-time cost: a single full resolve,
// never a failure, after which recordResolutionIfNeeded rewrites both
// fields back into agreement. What a mixed-version cache does next -
// upgrade or downgrade - is where the two directions genuinely split, and
// they are not symmetric; stating them as one property, as an earlier
// version of this comment did, is exactly the mistake a reader must not
// repeat.
//
// Upgrade - an older binary with no cut persisted a spec with an unstripped
// query, and this binary reads it back. The stored hash was computed over
// the unstripped value; this binary's own normalizeSignatures strips it
// before recomputing, so the two disagree - over the whole requirement set
// rather than one root at a time, since a single sha256 sum covers every
// root's line - and both snapshotMatchesRequirements (loadResolvedFromSnapshot)
// and tryIncrementalResolve's own identical self-consistency check fail on
// that disagreement. A full resolve follows, once, and recordResolutionIfNeeded
// then rewrites both the spec and the hash in this binary's stripped form.
// Independently of whether that resolve even runs, the persist-side
// backstop strips the value on the way out regardless - the backstop for a
// spec this function's own cut never touched because nothing in this run
// rebuilt it; snapshotData's own doc comment
// (internal/galaxy/store/snapshot.go) states the one residual that
// survives it.
//
// Downgrade - a store this binary persisted stripped, later read by an older
// binary - is not the mirror of the above. The asymmetry is a property of
// the old binary rather than of any state: it applies no cut at all, so
// trimming and sorting an already-stripped value is a no-op, its recomputed
// hash equals the persisted one, and the self-check that catches the upgrade
// direction passes here instead. The root then reads as changed by
// splitRootsByChange - a hash match plays no part in what that function
// compares - and provided at least one other root in the run is still
// unchanged (tryIncrementalResolve's own precondition; with none, it falls
// back to a full resolve exactly like the upgrade direction), the old binary
// takes the targeted incremental path instead: it resolves only the changed
// root fresh over the network, and recordResolution's SetRequirements still
// rewrites the WHOLE persisted spec - every root, not only the changed one -
// in this old binary's own uncut form, restoring the query wherever
// requirements.yml still declares one. A mixed fleet whose requirements
// declare a query-bearing signature source therefore pays a resolve on
// every run that follows a run by the other binary, upgrade or downgrade
// alike, since each one's own write invalidates the hash the other
// computes. Nothing on this side of the cut can close the downgrade
// direction: the fix belongs to the binary that still lacks it.
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
		out = append(out, helpers.WithoutQuery(trimmed))
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
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
func requirementSpecEqual(a, b store.RequirementSpec) bool {
	if a.Constraint != b.Constraint || a.Source != b.Source || a.Type != b.Type {
		return false
	}
	return slices.Equal(normalizeSignatures(a.Signatures), normalizeSignatures(b.Signatures))
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
	currentSpec map[string]store.RequirementSpec,
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
	currentSpec map[string]store.RequirementSpec,
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

	resolvedNew, graphNew, err := resolveCollectionsInternal(ctx, deps, changedRoots, resolveNestedPartial)
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

func splitRootsByChange(roots []collection, currentSpec, prevSpec map[string]store.RequirementSpec) ([]collection, []collection) {
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
func buildRequirementsSpec(roots []collection) map[string]store.RequirementSpec {
	spec := make(map[string]store.RequirementSpec, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		constraint = normalizeRequirementConstraint(constraint)
		spec[fqdn] = store.RequirementSpec{
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
func requirementsSignatureFromSpec(spec map[string]store.RequirementSpec, noDeps bool, serversSig string) string {
	parts := make([]string, 0, len(spec))
	for fqdn, entry := range spec {
		constraint := entry.Constraint
		if constraint == "" {
			constraint = "*"
		}
		signatureKey := strings.Join(normalizeSignatures(entry.Signatures), ",")
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s", fqdn, constraint, entry.Source, entry.Type, signatureKey))
	}
	slices.Sort(parts)
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
	indegree := make(map[string]int, len(graph))
	reverse := make(map[string][]string, len(graph))
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

// topologicalLevels groups indegree's nodes into install levels: the first
// level is every node already at indegree 0, and each following level is
// whatever nodes reverse[node] reduces to indegree 0 once every node in the
// level before it is applied. remaining tracks how many nodes have not yet
// been placed in a level; if it is still positive once no node reaches
// indegree 0, those unplaced nodes form a cycle.
//
// Each level is sorted by key before it is appended, which makes
// runInstallLevel's dispatch order match the prefetch queue order
// sortTasksByLevel builds from the same (level, key) pair. That match is a
// latency optimization only, exactly as sortTasksByLevel's own doc comment
// states of its side: a worker still blocks on prefetch.Wait(key) for its own
// artifact regardless of fetch order.
//
// The loop reaches every reverse[node] edge exactly once - one decrement per
// edge, no rescanning of nodes already placed in an earlier level.
func topologicalLevels(indegree map[string]int, reverse map[string][]string) ([][]string, error) {
	current := make([]string, 0, len(indegree))
	for node, deg := range indegree {
		if deg == 0 {
			current = append(current, node)
		}
	}
	levels := make([][]string, 0, len(indegree))
	remaining := len(indegree)
	for len(current) > 0 {
		slices.Sort(current)
		levels = append(levels, current)
		remaining -= len(current)
		next := make([]string, 0, len(current))
		for _, node := range current {
			for _, child := range reverse[node] {
				indegree[child]--
				if indegree[child] == 0 {
					next = append(next, child)
				}
			}
		}
		current = next
	}
	if remaining > 0 {
		return nil, helpers.ErrDependencyGraphHasACycle
	}
	return levels, nil
}
