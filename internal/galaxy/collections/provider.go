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
	// ctx is the sole seam through which cancellation reaches the HTTP/cache
	// layer beneath solver.Provider's methods, which take no ctx of their
	// own; a single Solve call drives every method from one goroutine only.
	//
	//nolint:containedctx // required: solver.Provider has no ctx parameter of its own to thread through instead.
	ctx  context.Context
	deps collectionDeps
}

// NewMetadataProvider builds a MetadataProvider sharing cfg/runtime/st with
// the rest of the install pipeline, so its cache reads and writes land in
// the exact same Store buckets resolveOne and loadCollectionMetadata use -
// a warm entry written by either path satisfies the other.
func NewMetadataProvider(ctx context.Context, cfg *config.Config, runtime *infra.Infra, st *store.Store) *MetadataProvider {
	return &MetadataProvider{ctx: ctx, deps: newCollectionDeps(cfg, runtime, st)}
}

// Highest returns fqdn's registry-reported highest_version, with no
// constraint checking of its own - the core checks membership itself. An
// unknown package (translated from a 404/exhausted-candidates root-metadata
// fetch) or a package whose root metadata carries no highest_version
// reports ok=false, sending the core to Universe instead.
func (p *MetadataProvider) Highest(fqdn string) (solver.Version, bool, error) {
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return solver.Version{}, false, err
	}
	policy := cachePolicyForConstraint(p.deps.cfg, false)
	rootMeta, _, known, err := p.resolveRoot(fqdn, ns, name, policy)
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
func (p *MetadataProvider) Universe(fqdn string) ([]solver.Version, error) {
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return nil, err
	}
	policy := cachePolicyForConstraint(p.deps.cfg, false)
	_, versionsURL, known, err := p.resolveRoot(fqdn, ns, name, policy)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, nil
	}
	raw, err := loadVersionsListCached(p.ctx, p.deps, versionsURL, policy)
	if err != nil {
		return nil, err
	}
	return buildSolverUniverse(raw), nil
}

// Dependencies returns the validated dependency map of fqdn@v: dependency
// fqdn mapped to its canonical Constraint. A warm deps-cache entry (written
// under the same "ns.name@version" key resolveOne uses) is served without
// any network access; a miss fetches the version's metadata, validates and
// caches its dependency map, and returns it. A malformed dependency key
// aborts with helpers.ErrInvalidDependencyKey; an unparseable constraint
// aborts wrapped, both as a provider contract violation the core never
// tries to guess around.
func (p *MetadataProvider) Dependencies(fqdn string, v solver.Version) (map[string]solver.Constraint, error) {
	ns, name, err := splitFQDN(fqdn)
	if err != nil {
		return nil, err
	}
	policy := cachePolicyForConstraint(p.deps.cfg, true)
	cacheKey := fmt.Sprintf("%s.%s@%s", ns, name, v.Original())

	if raw, ok := cachedDeps(p.deps.st, policy, cacheKey); ok {
		return canonicalizeDependencies(fqdn, raw)
	}

	col := collection{Namespace: ns, Name: name}
	_, versionsURL, err := resolveRootMetadata(p.ctx, p.deps, col, policy, fqdn)
	if err != nil {
		return nil, err
	}
	info, err := fetchVersionMetadataCached(p.ctx, p.deps, col.Source, versionsURL, v.Original(), policy)
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

// resolveRoot loads fqdn's root metadata (built from ns/name, with no
// explicit source), translating a 404 or an exhausted-candidate-list
// failure into known=false rather than an error, so Highest/Universe can
// both report "unknown package" instead of aborting the solve. Any other
// failure (a genuine network/offline error, or a non-404 HTTP status)
// propagates unchanged.
func (p *MetadataProvider) resolveRoot(
	fqdn, ns, name string,
	policy cacheManager.Policy,
) (*types.GalaxyCollection, string, bool, error) {
	col := collection{Namespace: ns, Name: name}
	rootMeta, versionsURL, err := resolveRootMetadata(p.ctx, p.deps, col, policy, fqdn)
	if err != nil {
		if isUnknownPackageError(err) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	return rootMeta, versionsURL, true, nil
}

// isUnknownPackageError reports whether err represents "this package does
// not exist in the registry" rather than a genuine failure: either
// loadRootMetadataCached exhausted every candidate (helpers.
// ErrLoadMetadataFailed, the normal outcome for our always-empty-Source
// collection), or a raw 404 surfaced directly (defensive: not reachable
// today given an empty Source, but cheap to also recognize).
func isUnknownPackageError(err error) bool {
	if errors.Is(err, helpers.ErrLoadMetadataFailed) {
		return true
	}
	var statusErr *cacheManager.HTTPStatusError
	return errors.As(err, &statusErr) && statusErr.Code == http.StatusNotFound
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
func (noDepsProvider) Dependencies(string, solver.Version) (map[string]solver.Constraint, error) {
	return map[string]solver.Constraint{}, nil
}
