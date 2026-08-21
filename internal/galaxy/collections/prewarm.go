package collections

import (
	"context"
	"sync"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// prewarmRootMetadata is a best-effort, cfg.Workers-bounded warm of metadata
// documents the sequential solve resolveCollectionsInternal is about to run
// (solveCollections, via solver.Solve) would otherwise request one root at a
// time: it fans every root out onto its own goroutine, ahead of the solve,
// so the round trips those roots would otherwise pay serially overlap
// instead. It warms nothing the solve has no use for, but the solve can
// still end early - on a conflict, or on a failure - without ever reaching a
// root this function already paid for.
//
// Concurrency is safe here for three separate reasons. First, prewarmOne
// builds its own MetadataProvider per call via newMetadataProviderWithDeps,
// so MetadataProvider.bindings - the one field that method documents as
// unsynchronized, safe only because a single Solve call drives it from one
// goroutine - is never shared between two goroutines: each provider's
// bindings map is written and discarded within that one prewarmOne call,
// never read back by this function or by any other goroutine. Second, the
// shared state the fan-out mutates - deps.st (Store, whose own doc comment
// requires every access to go through its RWMutex-protected methods),
// deps.apiRoots, and deps.unmatchedSources - is already concurrency-safe on
// its own terms, independent of anything this function does; everything else
// a goroutine touches is read-only for the whole fan-out: deps.cfg,
// deps.runtime (whose Output is itself goroutine-safe), and the sources map
// built once above the loop and never written after.
//
// Third, wg.Wait below returns, and therefore this function returns,
// strictly before resolveCollectionsInternal reaches solveCollections, so
// the solve's own single-goroutine MetadataProvider never runs concurrently
// with a prewarm goroutine still in flight.
//
// It calls the same MetadataProvider methods the solve itself calls -
// Highest, Dependencies - rather than reimplementing their fetch-and-cache
// logic (resolveRootMetadata, fetchVersionMetadataCached) a second time,
// because that is what makes "a document this function warmed is a cache
// hit during solve" a property of both call sites agreeing to call the same
// method, not of two independent implementations agreeing about URLs, cache
// keys, and policy by construction.
//
// What gets warmed, per root, depends on how the solve would resolve it. An
// unpinned root's root-metadata document is requested by both Highest and
// Universe - they share one cacheManager.PolicyForConstraint(deps.cfg,
// false) call and one resolveRoot - so warming it via Highest alone covers
// whichever of the two the solver ends up calling, without needing to know
// which. An exactly pinned root whose dependencies the solve will follow is
// warmed via Dependencies, the method that fetches and caches its
// version-detail document. An exactly pinned root under cfg.NoDeps warms
// nothing: the solve
// wraps its provider in NewNoDepsProvider, whose Dependencies never reaches
// the wrapped provider at all, so a prewarmed document for it would be the
// only request made for that root all run.
//
// The versions list a Universe call would page through is deliberately never
// warmed. The solver has cheaper paths that settle a package without it - an
// exact pin it can verify, or a Highest answer that satisfies the
// constraints accumulated for that package so far - so warming the versions
// list unconditionally for every root would add a request the solve
// frequently never needs, on top of the request this function already made
// to learn the same root's highest_version.
//
// A root is skipped - contributing no request at all - when
// prewarmPolicyUsable(cacheManager.PolicyForConstraint(deps.cfg, exact)) is
// false: this function pays for a request only where the solve itself would
// later read what that request wrote, and PolicyForConstraint (internal/galaxy/
// cache/policy.go) is the single function deciding both sides of that
// question, since it is also what the solve's own MetadataProvider calls to
// decide whether to read or write the cache.
//
// A prewarm goroutine's own error - a network failure, an auth failure, a
// malformed response - is never returned to resolveCollectionsInternal and
// never logged above Debugf: the classified failure the operator sees comes
// from the real, sequential solve afterward, exactly once, not from a
// best-effort warm that ran ahead of it. The first such error does cancel
// warmCtx, stopping every root not yet dispatched and unblocking the solve
// sooner rather than later: with N roots and a cfg.Workers-sized pool there
// are ceil(N/cfg.Workers) waves, and a stalled server makes each of this
// function's own requests burn its own helpers.MetadataFetchDeadline (two
// minutes) - silently finishing every wave regardless of an early failure
// would turn one such stall into ceil(N/cfg.Workers) times that budget,
// entirely inside the window this run already holds the backend's
// whole-run exclusive lock (see loadVersionsListCached's own doc comment,
// internal/galaxy/collections/resolve.go, for the fuller argument on why
// that hold is accepted rather than capped elsewhere in this package).
//
// Every request this function issues still pays deps.runtime.MetadataDeadline
// (Infra.MetadataFetchDeadline) exactly like any other metadata fetch this
// package makes - nothing here grants it a separate or larger budget - and
// none of it is reachable from runtime.Metrics: that counter is scoped to
// ArtifactStore cache hits/misses and bytes downloaded, and a root-metadata
// or version-detail fetch never touches an ArtifactStore at all.
//
// Two residuals are accepted rather than closed. First, only the root layer
// is parallelized: a transitive dependency's identity is not known until the
// solve itself decides which package versions are actually in play, so
// nothing below the roots this function was handed can be warmed ahead of
// time. Second, only a successful (HTTP 200) response is ever cached, so a
// losing candidate probe - an apiRoot variant that 404s before the winning
// one answers, or a configured server that does not carry the collection at
// all - is warmed by nothing this function does and is paid again by
// whichever walk runs next. deps.apiRoots (see newMetadataProviderWithDeps,
// and solveCollections' own use of it) bounds the apiRoot half of that, and
// only for a walk that has not started yet: once any one collection against
// a given server base has recorded that base's winning apiRoot, every walk
// beginning afterward - this function's or the solve's - tries only that
// apiRoot's own two URL variants. A walk already in flight keeps the full
// candidate list it read, which under this function's own fan-out means at
// least the whole first cfg.Workers-sized wave pays the losing candidates
// against a base whose winner is not the first candidate, rather than only
// the first root paying them. Nothing bounds the server-list half: a 404 is
// a fact about one collection on one server and is recorded nowhere, so an
// unpinned root the first configured server does not carry pays its losing
// server probes twice, once here and once in the solve. Both are request
// count traded for wall-clock overlap, and both are disclosed rather than
// closed.
//
// A version conflict that forces the solver to materialize an exactly
// pinned package's version detail after this function already warmed its
// root metadata still reuses that warmed root-metadata document - the
// candidate walk and its winning apiRoot are unaffected by which version the
// solver ultimately needs - but the version-detail document itself is warmed
// only when this function's own exact-pin arm ran for that root; a
// transitively reached or newly conflicting exact pin still pays for its own
// version-detail fetch during the solve.
func prewarmRootMetadata(ctx context.Context, deps collectionDeps, roots []collection) {
	if !prewarmEnabled(deps, roots) {
		return
	}

	// Local to this function, deliberately never hoisted into
	// resolveCollectionsInternal: solveCollections must run on the caller's
	// own, untouched ctx, so a prewarm failure can only ever speed up the
	// solve's own failure (by ending the warm early) and never itself become
	// the reason the solve's context is already canceled.
	warmCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sources := rootSourceMap(roots)
	// max(deps.cfg.Workers, 1): see warmCollections/runInstallLevel's
	// identical guard - a zero Workers would make sem unbuffered and deadlock
	// the first send, since no worker has started to drain it yet.
	sem := make(chan struct{}, max(deps.cfg.Workers, 1))
	var wg sync.WaitGroup
	for _, root := range roots {
		// Once warmCtx is canceled (by an earlier root's own error), no
		// further root is dispatched: the loop stops here, on this
		// iteration. A goroutine already started runs on that same warmCtx,
		// so its own in-flight request is canceled with it and it returns
		// that context error into the swallow-and-Debugf arm below; wg.Wait
		// still waits for every one of them, since the solve must not start
		// while any prewarm goroutine is still live.
		if warmCtx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := prewarmOne(warmCtx, deps, sources, root); err != nil {
				deps.runtime.Output.Debugf("prewarm %s.%s: %v", root.Namespace, root.Name, err)
				cancel()
			}
		})
	}
	// wg.Wait is mandatory, not merely tidy, ahead of this function
	// returning: MetadataProvider.bindings is unsynchronized precisely
	// because a single Solve call is documented to drive every
	// MetadataProvider method from one goroutine, and the caller reaches
	// solveCollections immediately after this call returns.
	wg.Wait()
}

// prewarmPolicyUsable reports whether a prewarm request under policy is
// worth issuing at all: only when the solve could actually read back
// whatever this function's own request would write. A write-only policy
// (--refresh's non-exact shape) writes a document the solve then declines to
// read, so the warm saves the solve nothing and the run pays for both
// requests. A policy with writes disabled (--offline's shape, or --no-cache,
// which disables both) stores nothing for the solve to read at all, for the
// same doubling. Both halves must hold, so this is a plain logical AND
// rather than a threshold or a priority between the two.
func prewarmPolicyUsable(policy cacheManager.Policy) bool {
	return policy.Read && policy.Write
}

// prewarmEnabled reports whether prewarmRootMetadata should run at all for
// roots under deps. Three cases make it pointless before any policy is
// consulted: a nil cfg; fewer than two roots, since with at most one root
// there is nothing to overlap and overlapping round trips is this function's
// whole point; and a nil store, which is prewarmPolicyUsable's own rule
// applied one level up - nothing this function fetched would be stored, so
// the solve would repeat every request rather than read any of them back.
// The two-root check is an optimization only: removing it costs one
// goroutine and changes no request the run makes, which is why nothing pins
// it. The store check is not - removing it doubles a nil-store run's
// metadata requests, which TestPrewarmSkippedWithoutStore pins.
//
// Otherwise it checks whether either shape of policy PolicyForConstraint can
// produce - the non-exact policy an unpinned root uses, or the exact policy a
// pinned root uses - is one prewarmPolicyUsable accepts. Both are checked
// because --refresh makes exactly one of them unusable: it leaves the
// non-exact shape write-only while the exact shape stays readable and
// writable, so a run mixing pinned and unpinned roots still has real work to
// do for the pinned half. --offline and --no-cache are not that case - each
// makes both shapes unusable, and this returns false under either one
// whatever the roots look like. See PolicyForConstraint's own four-way split
// in internal/galaxy/cache/policy.go.
func prewarmEnabled(deps collectionDeps, roots []collection) bool {
	if deps.cfg == nil || deps.st == nil || len(roots) < 2 {
		return false
	}
	return prewarmPolicyUsable(cacheManager.PolicyForConstraint(deps.cfg, false)) ||
		prewarmPolicyUsable(cacheManager.PolicyForConstraint(deps.cfg, true))
}

// prewarmOne makes at most one provider call for root, never both, and none
// at all in three cases: when root's constraint is malformed (a real error
// here is not this function's to report - see prewarmRootMetadata's own doc
// comment - so it is swallowed as a skip, not surfaced), when root is
// exactly pinned under cfg.NoDeps (the solve itself makes no request for
// such a root), or when the policy the solve would use is not
// prewarmPolicyUsable. Otherwise it warms root's highest_version via Highest
// (an unpinned or range-constrained root) or its dependency map via
// Dependencies (an exactly pinned root the solve will follow dependencies
// for). One provider call is not one request: Highest pays whatever apiRoot
// and server-candidate walk loadRootMetadataCached needs, and Dependencies
// pays that walk plus a version-detail fetch on top of it.
func prewarmOne(ctx context.Context, deps collectionDeps, sources map[string]string, root collection) error {
	// A git root has no Galaxy root metadata to warm: its answers come from
	// the discovery memo, and asking a server for it would only mint a 404.
	if root.isGit() {
		return nil
	}
	constraint := root.Constraint
	if constraint == "" {
		constraint = root.Version
	}
	version, exact, err := exactVersionFromConstraints([]string{constraint})
	if err != nil {
		// A malformed constraint is buildSolverRequirements' failure to
		// report, not this best-effort warm's: the solve will hit the
		// identical parse and fail loudly and exactly once.
		return nil
	}
	if exact && deps.cfg.NoDeps {
		return nil
	}
	if !prewarmPolicyUsable(cacheManager.PolicyForConstraint(deps.cfg, exact)) {
		return nil
	}

	fqdn := root.Namespace + "." + root.Name
	// A fresh provider per call, deliberately: see prewarmRootMetadata's own
	// doc comment for why this is what keeps MetadataProvider.bindings safe
	// under concurrent prewarm goroutines while still sharing deps' own
	// apiRootMemo, unmatchedSourceMemo, and Store with every other provider
	// built over the same deps - including solveCollections' own.
	mp := newMetadataProviderWithDeps(deps, sources)
	if !exact {
		_, _, err := mp.Highest(ctx, fqdn)
		return err
	}

	v, err := solver.NewVersion(version)
	if err != nil {
		// exactVersionFromConstraints already proved version parses as a
		// semver.Version; solver.NewVersion wraps the identical parse, so
		// this is unreachable in practice - handled defensively as a skip
		// rather than asserted away, matching this function's own
		// swallow-and-let-the-solve-report-it contract.
		return nil
	}
	_, err = mp.Dependencies(ctx, fqdn, v)
	return err
}
