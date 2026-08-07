package collections

import (
	"context"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// solveCollections resolves roots via the PubGrub-style version solver,
// returning (resolved, graph): resolved keyed by ns.name@version
// (collection.key()), graph mapping a parent key to its dependency keys. It
// is the production cold-resolve path.
func solveCollections(ctx context.Context, deps collectionDeps, roots []collection) (map[string]collection, map[string][]string, error) {
	sources := rootSourceMap(roots)
	// newMetadataProviderWithDeps, not NewMetadataProvider: this provider must
	// share deps.apiRoots (and deps.unmatchedSources) with the rest of this
	// resolve phase, in particular with prewarmRootMetadata's own per-root
	// providers (see its doc comment) - a root-metadata document either of
	// them already fetched is then served to the other from deps.st with no
	// further network request, and the winning apiRoot either of them already
	// discovered for a server base is not re-probed by the other.
	mp := newMetadataProviderWithDeps(deps, sources)
	var provider solver.Provider = mp
	if deps.cfg.NoDeps {
		provider = NewNoDepsProvider(provider)
	}

	reqs, err := buildSolverRequirements(roots)
	if err != nil {
		return nil, nil, err
	}

	result, err := solver.Solve(ctx, reqs, provider)
	if err != nil {
		return nil, nil, err
	}
	// mp.bindings is read only now that Solve has returned: the solver core
	// drives every MetadataProvider method from this one goroutine, so there
	// is no concurrent writer left to race with this read.
	return solverResultToResolvedGraph(result, deps.cfg, mp.bindings)
}

// rootSourceMap builds a root fqdn -> explicit-source map from roots: a
// root's own Source, which may be "" (unpinned - the root walks the
// configured server list instead of being nailed to one). It is passed
// straight through to NewMetadataProvider so MetadataProvider.sourceOf can
// tell a pinned root from an unpinned one. A transitive dependency is
// deliberately never a key in the returned map - see sourceOf, which falls
// back to "" for anything absent here: there is no source inheritance from
// a parent to its dependencies.
func rootSourceMap(roots []collection) map[string]string {
	sources := make(map[string]string, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		sources[fqdn] = root.Source
	}
	return sources
}

// buildSolverRequirements builds one solver.Requirement per unique root
// fqdn from roots, deduplicating per fqdn: a root's effective constraint is
// its own Constraint, falling back to its Version when Constraint is empty; a
// constraint that normalizes to the empty string ("*", or already empty)
// never touches the stored value, so two roots naming the same fqdn - one
// bare, one constrained - never conflict; two roots naming the same fqdn with
// different NON-EMPTY normalized constraints do conflict, reported through the
// helpers.ErrConflictingRootConstraints sentinel.
func buildSolverRequirements(roots []collection) ([]solver.Requirement, error) {
	order := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	constraints := make(map[string]string, len(roots))

	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		if !seen[fqdn] {
			seen[fqdn] = true
			order = append(order, fqdn)
		}

		raw := root.Constraint
		if raw == "" {
			raw = root.Version
		}
		normalized := helpers.NormalizeConstraint(raw)
		if normalized == "" {
			continue
		}
		if existing, ok := constraints[fqdn]; ok && existing != normalized {
			return nil, fmt.Errorf("%w for %s: %q vs %q", helpers.ErrConflictingRootConstraints, fqdn, existing, normalized)
		}
		constraints[fqdn] = normalized
	}

	reqs := make([]solver.Requirement, 0, len(order))
	for _, fqdn := range order {
		reqs = append(reqs, solver.Requirement{Package: fqdn, Constraint: constraints[fqdn]})
	}
	return reqs, nil
}

// solverResultToResolvedGraph maps a solver.Result onto the (resolved, graph)
// shape the install pipeline consumes: resolved keyed by fqdn, graph keyed by
// collection.key() (buildGraphFromDeps' output). bindings is
// MetadataProvider's own fqdn -> winning-server-base map, built during the
// solve (see MetadataProvider.recordBinding); it - not a root's own,
// possibly-unpinned Source field - is what stamps every resolved entry's
// Source, so a root or a transitive dependency alike always records
// whichever server actually served it.
func solverResultToResolvedGraph(
	result *solver.Result,
	cfg *config.Config,
	bindings map[string]string,
) (map[string]collection, map[string][]string, error) {
	resolved := make(map[string]collection, len(result.Versions))
	for fqdn, version := range result.Versions {
		ns, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, nil, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, fqdn)
		}
		resolved[fqdn] = collection{
			Namespace: ns,
			Name:      name,
			Version:   version,
			Source:    sourceFor(fqdn, bindings, cfg),
		}
	}

	depsByParent := make(map[string]map[string]string, len(result.Graph))
	for parentFQDN, depFQDNs := range result.Graph {
		if len(depFQDNs) == 0 {
			continue
		}
		deps := make(map[string]string, len(depFQDNs))
		for _, depFQDN := range depFQDNs {
			// buildGraphFromDeps only ever reads the key set of this map (the
			// dependency fqdn), never its value, so an empty placeholder is
			// enough here.
			deps[depFQDN] = ""
		}
		depsByParent[parentFQDN] = deps
	}

	graph, err := buildGraphFromDeps(resolved, depsByParent)
	if err != nil {
		return nil, nil, err
	}
	ensureGraphNodes(resolved, graph)
	return resolved, graph, nil
}

// sourceFor returns fqdn's install source: the server base that actually
// answered its root-metadata fetch during the solve (bindings, recorded by
// MetadataProvider.recordBinding on every fetch success - root or
// transitive dependency alike), so the resolved collection's Source always
// agrees with whichever server served it under this run's first-match
// ownership rule. The firstServerURL fallback is defensive only: every
// package solver.Solve actually decides is bound before Solve returns (via
// Highest/Universe's shared resolveRoot, or via Dependencies), so bindings
// is never actually missing an entry for a decided fqdn in practice.
func sourceFor(fqdn string, bindings map[string]string, cfg *config.Config) string {
	if source, ok := bindings[fqdn]; ok {
		return source
	}
	return firstServerURL(cfg)
}

// firstServerURL returns cfg's first configured server: Servers[0].URL when
// Servers is populated, else cfg.Server - the shape every hand-built
// *config.Config in this package's test suite (no Servers slice) still has,
// and the shape resolveServers guarantees for a real one (cfg.Server always
// equals Servers[0].URL once it has run).
func firstServerURL(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if len(cfg.Servers) > 0 {
		return cfg.Servers[0].URL
	}
	return cfg.Server
}
