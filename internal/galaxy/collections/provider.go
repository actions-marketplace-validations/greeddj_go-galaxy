package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/Masterminds/semver/v3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// MetadataProvider adapts go-galaxy's real Galaxy metadata layer (root
// metadata, versions paging, per-version dependency fetches, and their
// shared cache/policy plumbing) to solver.Provider, so Solve can resolve
// against the live registry the same way the existing resolve.go pipeline
// does - same candidate URLs, same cache buckets, same offline behavior.
type MetadataProvider struct {
	deps collectionDeps
	// sources maps a root fqdn to its explicit install source (see
	// sourceOf): a root without one, and every transitive dependency, is
	// simply absent from this map, so sourceOf falls through to "" -
	// unpinned - letting serverCandidates walk the whole configured server
	// list for it.
	sources map[string]string
	// bindings maps every fqdn this provider has successfully fetched root
	// metadata for to the server base that answered it (see recordBinding).
	// It is a plain map with no mutex, deliberately: a single Solve call
	// drives every MetadataProvider method from one goroutine only, so an
	// unsynchronized map is sufficient here, unlike apiRootMemo (shared
	// across the install and prefetch worker pools, which is why that one
	// needs a mutex and this one does not). solveCollections reads it once
	// Solve returns, to stamp the same winning server onto each resolved
	// collection's Source.
	bindings map[string]string
}

// NewMetadataProvider builds a MetadataProvider sharing cfg/runtime/st with
// the rest of the install pipeline, so its cache reads and writes land in
// the same Store buckets loadCollectionMetadata uses -
// a warm entry written by either path satisfies the other. sources maps a
// root fqdn to its explicit install source (see sourceOf); passing nil (or
// an empty map) means every fqdn resolves against cfg.Server.
//
// It builds a fresh collectionDeps (a fresh apiRootMemo and
// unmatchedSourceMemo, scoped to this one provider) rather than reusing a
// caller's own: a caller that needs its provider to share those memos with
// the rest of its own pipeline - the solve phase's own provider, sharing
// deps.apiRoots with resolveCollectionsInternal's prewarm - uses
// newMetadataProviderWithDeps directly instead.
func NewMetadataProvider(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	sources map[string]string,
) *MetadataProvider {
	return newMetadataProviderWithDeps(newCollectionDeps(cfg, runtime, st), sources)
}

// newMetadataProviderWithDeps builds a MetadataProvider over an
// already-built collectionDeps, so its apiRootMemo and unmatchedSourceMemo -
// not just its Store - are shared with whatever else deps is threaded
// through, rather than each provider getting its own scoped-to-nothing-else
// copy the way NewMetadataProvider's own newCollectionDeps call produces.
func newMetadataProviderWithDeps(deps collectionDeps, sources map[string]string) *MetadataProvider {
	return &MetadataProvider{deps: deps, sources: sources, bindings: make(map[string]string)}
}

// Highest returns fqdn's registry-reported highest_version, with no
// constraint checking of its own - the core checks membership itself. An
// unknown package (translated from a 404/exhausted-candidates root-metadata
// fetch) or a package whose root metadata carries no highest_version
// reports ok=false, sending the core to Universe instead.
func (p *MetadataProvider) Highest(ctx context.Context, fqdn string) (solver.Version, bool, error) {
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return solver.Version{}, false, err
	}
	policy := cacheManager.PolicyForConstraint(p.deps.cfg, false)
	rootMeta, _, known, err := p.resolveRoot(ctx, fqdn, ns, name, policy)
	if err != nil {
		return solver.Version{}, false, err
	}
	if !known || rootMeta.HighestVersion.Version == "" {
		return solver.Version{}, false, nil
	}
	v, err := solver.NewVersion(rootMeta.HighestVersion.Version)
	if err != nil {
		return solver.Version{}, false, fmt.Errorf("parsing highest_version for %s: %w", fqdn, err)
	}
	return v, true, nil
}

// Universe returns every published version of fqdn, deduplicated by
// original string and sorted into the solver's own descending total order
// (semver precedence, then original string, both descending) - the sort
// order Universe's own contract only requires as a defense-in-depth
// convention, never load-bearing for correctness, but computed here anyway
// since the core would otherwise re-sort with the exact same comparator. An
// unknown package returns (nil, nil), matching solver.Provider's contract.
func (p *MetadataProvider) Universe(ctx context.Context, fqdn string) ([]solver.Version, error) {
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(p.deps.cfg, false)
	_, versionsURL, known, err := p.resolveRoot(ctx, fqdn, ns, name, policy)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, nil
	}
	raw, err := loadVersionsListCached(ctx, p.deps, versionsURL, policy)
	if err != nil {
		return nil, err
	}
	return buildSolverUniverse(raw), nil
}

// Dependencies returns the validated dependency map of fqdn@v: dependency
// fqdn mapped to its canonical Constraint. A warm deps-cache entry (written
// under the helpers.ScopedDepsCacheKey it computes, scoped to whichever
// server fqdn is bound to - see boundBaseFor) is served without any network
// access when that server is known with no ambiguity; a miss fetches the
// version's metadata, validates and caches its dependency map, and returns
// it. A malformed dependency key aborts with helpers.ErrInvalidDependencyKey;
// an unparseable constraint aborts wrapped, both as a provider contract
// violation the core never tries to guess around.
func (p *MetadataProvider) Dependencies(ctx context.Context, fqdn string, v solver.Version) (map[string]solver.Constraint, error) {
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(p.deps.cfg, true)
	col := collection{Namespace: ns, Name: name, Source: p.sourceOf(fqdn)}

	base, err := p.boundBaseFor(ctx, col, fqdn, policy)
	if err != nil {
		return nil, err
	}
	cacheKey := helpers.ScopedDepsCacheKey(base, fmt.Sprintf("%s.%s@%s", ns, name, v.Original()))

	if raw, ok := cachedDeps(p.deps.st, policy, cacheKey); ok {
		return canonicalizeDependencies(fqdn, raw)
	}

	root, err := resolveRootMetadata(ctx, p.deps, col, policy, fqdn)
	if err != nil {
		return nil, err
	}
	p.recordBinding(fqdn, root.base)
	info, err := fetchVersionMetadataCached(ctx, p.deps, root.base, root.versionsURL, v.Original(), policy)
	if err != nil {
		return nil, err
	}
	raw, err := parseDependencies(extractDependencies(info))
	if err != nil {
		return nil, err
	}
	cacheDeps(p.deps.st, policy, cacheKey, raw)
	return canonicalizeDependencies(fqdn, raw)
}

// boundBaseFor returns the server base fqdn's deps-cache key must be scoped
// to (see ScopedDepsCacheKey), resolving it over the network only when
// genuinely ambiguous. When col has only one possible server candidate - a
// pinned col.Source, or a single configured server, both cases
// serverCandidates already resolves with no network access - that candidate
// IS the server fqdn will end up bound to, so it is returned directly. This
// is also what keeps a warm single-server deps-cache hit zero-network: the
// overwhelming majority of deployments configure exactly one server, so this
// branch covers them without ever touching resolveRootMetadata.
//
// Otherwise (more than one configured server, so which one actually serves
// fqdn is genuinely undetermined without asking) it prefers whatever server
// has already answered a prior Highest/Universe/Dependencies call for fqdn
// this Solve (p.bindings). Failing that - reached only when the solver
// decides fqdn's version via its exact-pin fast path before ever probing it
// with Highest or Universe (see the solver's packageIsExactPin), which
// requirements pinning an exact version trigger routinely - it resolves root
// metadata now to settle which server actually serves it, recording the
// binding via recordBinding so every later call for fqdn reuses it.
func (p *MetadataProvider) boundBaseFor(ctx context.Context, col collection, fqdn string, policy cacheManager.Policy) (string, error) {
	if candidates := serverCandidates(p.deps, col); len(candidates) == 1 {
		return candidates[0].base, nil
	}
	if base, ok := p.bindings[fqdn]; ok {
		return base, nil
	}
	root, err := resolveRootMetadata(ctx, p.deps, col, policy, fqdn)
	if err != nil {
		return "", err
	}
	p.recordBinding(fqdn, root.base)
	return root.base, nil
}

// sourceOf returns fqdn's install source: its own entry in sources when one
// was recorded (a root with an explicit source), or "" otherwise - a
// transitive dependency is never recorded in sources, and a root without an
// explicit source is recorded with "" too, so both cases fall through to
// unpinned here. An unpinned fqdn's serverCandidates then walks the whole
// configured server list rather than being nailed to a single one; there is
// no source inheritance from a requiring parent to its dependencies.
func (p *MetadataProvider) sourceOf(fqdn string) string {
	return p.sources[fqdn]
}

// recordBinding remembers base as the server whose root-metadata fetch for
// fqdn just succeeded, so solveCollections can later stamp that same server
// onto fqdn's resolved collection (see sourceFor). It is called on every
// fetch success, never conditioned on what the caller does with the result
// afterward: Highest reports known=false when the fetch succeeded but
// HighestVersion.Version is empty, and a package later decided via Universe
// must not be left unbound just because that earlier Highest call rejected
// it for an unrelated reason. base == "" (never expected once a fetch has
// actually succeeded) is a no-op rather than poisoning the map with an
// empty winner.
func (p *MetadataProvider) recordBinding(fqdn, base string) {
	if base == "" {
		return
	}
	p.bindings[fqdn] = base
}

// resolveRoot loads fqdn's root metadata (built from ns/name, with fqdn's
// own source per sourceOf), translating a 404 or an exhausted-candidate-list
// failure into known=false rather than an error, so Highest/Universe can
// both report "unknown package" instead of aborting the solve. Any other
// failure (a genuine network/offline error, a non-404 HTTP status, or a
// classified auth/availability abort) propagates unchanged. A successful
// fetch is recorded via recordBinding before it is ever inspected further.
func (p *MetadataProvider) resolveRoot(
	ctx context.Context,
	fqdn, ns, name string,
	policy cacheManager.Policy,
) (*types.GalaxyCollection, string, bool, error) {
	col := collection{Namespace: ns, Name: name, Source: p.sourceOf(fqdn)}
	root, err := resolveRootMetadata(ctx, p.deps, col, policy, fqdn)
	if err != nil {
		if isUnknownPackageError(err) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	p.recordBinding(fqdn, root.base)
	return root.meta, root.versionsURL, true, nil
}

// isUnknownPackageError reports whether err represents "this package does
// not exist in the registry" rather than a genuine failure. Every fqdn -
// root or transitive dependency, with or without an explicit source - now
// falls through loadRootMetadataCached's full v3/v2/bare-API candidate list
// on a 404 instead of failing on the first one, so the common outcome for an
// unknown package is that every candidate 404s and loadRootMetadataCached
// returns the last of those 404s as-is (a raw *cacheManager.HTTPStatusError).
// helpers.ErrLoadMetadataFailed is only reached in the narrower case of an
// empty candidate list (no server configured and no explicit source), so
// both forms must be recognized here.
func isUnknownPackageError(err error) bool {
	if errors.Is(err, helpers.ErrLoadMetadataFailed) {
		return true
	}
	statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err)
	return ok && statusErr.Code == http.StatusNotFound
}

// splitFQDN validates fqdn as a "namespace.name" fully qualified collection
// name, wrapping helpers.ErrInvalidDependencyKey - the same sentinel
// parseDependencies uses - since a malformed key reaching this provider,
// whether from a root requirement or a dependency map entry, is the same
// class of provider-contract violation.
func splitFQDN(fqdn string) (string, string, error) {
	ns, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return "", "", fmt.Errorf("%w: %q", helpers.ErrInvalidDependencyKey, fqdn)
	}
	return ns, name, nil
}

// canonicalizeDependencies normalizes and validates every constraint in raw,
// returning a fresh map keyed identically. A constraint that normalizes to
// the empty string is left unconstrained; any other normalized form must
// parse via Masterminds/semver, or the whole call fails - a malformed
// constraint is a provider contract violation surfaced from here, not
// guessed at by the core.
func canonicalizeDependencies(fqdn string, raw map[string]string) (map[string]solver.Constraint, error) {
	out := make(map[string]solver.Constraint, len(raw))
	for dep, rawConstraint := range raw {
		normalized := helpers.NormalizeConstraint(rawConstraint)
		if normalized != "" {
			if _, err := semver.NewConstraint(normalized); err != nil {
				return nil, fmt.Errorf("invalid dependency constraint %q for %s -> %s: %w", rawConstraint, fqdn, dep, err)
			}
		}
		out[dep] = normalized
	}
	return out, nil
}

// rankedVersion pairs a solver.Version with the *semver.Version parsed from
// the same original string, so buildSolverUniverse can sort by precedence
// without solver.Version exposing its own parsed form outside its package.
type rankedVersion struct {
	sv       *semver.Version
	original solver.Version
}

// buildSolverUniverse parses raw into solver.Versions, dropping any string
// that fails to parse, deduplicating by original string, and sorting the
// result descending by the solver's own total order: semver precedence
// descending, tied-broken by the original string descending (byte-wise).
// This total order matters only as a defense-in-depth convention (the core
// re-sorts regardless), but is cheap to get right here directly rather than
// reusing buildCandidates' single-level GreaterThan, which is not a total
// order for equal-precedence strings (e.g. "1.0.0" vs "1.0.0+build") and
// would make this provider's own output non-deterministic.
func buildSolverUniverse(raw []string) []solver.Version {
	seen := make(map[string]bool, len(raw))
	ranked := make([]rankedVersion, 0, len(raw))
	for _, r := range raw {
		if seen[r] {
			continue
		}
		v, err := solver.NewVersion(r)
		if err != nil {
			continue
		}
		seen[r] = true
		sv, err := semver.NewVersion(r)
		if err != nil {
			// Unreachable: solver.NewVersion(r) above already parsed r
			// successfully via the exact same semver.NewVersion call.
			continue
		}
		ranked = append(ranked, rankedVersion{sv: sv, original: v})
	}
	slices.SortFunc(ranked, compareRankedVersionsDescending)

	out := make([]solver.Version, len(ranked))
	for i, r := range ranked {
		out[i] = r.original
	}
	return out
}

// compareRankedVersionsDescending implements buildSolverUniverse's total
// order: semver precedence descending, then original string descending.
func compareRankedVersionsDescending(a, b rankedVersion) int {
	if c := b.sv.Compare(a.sv); c != 0 {
		return c
	}
	ao, bo := a.original.Original(), b.original.Original()
	switch {
	case ao > bo:
		return -1
	case ao < bo:
		return 1
	default:
		return 0
	}
}

// noDepsProvider wraps a solver.Provider so every package resolves as if it
// declared no dependencies, for a --no-deps run: Highest and Universe
// delegate unchanged (a package's own version candidates are unaffected),
// but Dependencies always reports an empty map, so the solve never adds a
// single dependency edge.
type noDepsProvider struct {
	solver.Provider
}

// NewNoDepsProvider wraps p so its Dependencies never contributes an edge,
// leaving Highest/Universe delegated to p unchanged.
func NewNoDepsProvider(p solver.Provider) solver.Provider {
	return noDepsProvider{Provider: p}
}

// Dependencies always reports no dependencies, without ever calling the
// wrapped provider.
func (noDepsProvider) Dependencies(context.Context, string, solver.Version) (map[string]solver.Constraint, error) {
	return map[string]solver.Constraint{}, nil
}
