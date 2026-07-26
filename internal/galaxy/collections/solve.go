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
	sources := rootSourceMap(roots, deps.cfg)
	var provider solver.Provider = NewMetadataProvider(ctx, deps.cfg, deps.runtime, deps.st, sources)
	if deps.cfg.NoDeps {
		provider = NewNoDepsProvider(provider)
	}

	reqs, err := buildSolverRequirements(roots)
	if err != nil {
		return nil, nil, err
	}

	result, err := solver.Solve(reqs, provider)
	if err != nil {
		return nil, nil, err
	}
	return solverResultToResolvedGraph(result, roots, deps.cfg)
}

// rootSourceMap builds a root fqdn -> source map from roots: a root's own
// explicit Source when non-empty, else cfg.Server. It is passed straight
// through to NewMetadataProvider so the version solver fetches every root
// from its own configured server, and reused by solverResultToResolvedGraph
// (via sourceFor) so the resolved collection's own recorded Source always
// agrees with whichever server actually served it. A transitive dependency
// is deliberately never a key in the returned map - see sourceFor and
// MetadataProvider.sourceOf, both of which fall back to cfg.Server for
// anything absent here: there is no source inheritance from a parent.
func rootSourceMap(roots []collection, cfg *config.Config) map[string]string {
	sources := make(map[string]string, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		source := root.Source
		if source == "" {
			source = cfg.Server
		}
		sources[fqdn] = source
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
// collection.key() (buildGraphFromDeps' output).
func solverResultToResolvedGraph(
	result *solver.Result,
	roots []collection,
	cfg *config.Config,
) (map[string]collection, map[string][]string, error) {
	sources := rootSourceMap(roots, cfg)
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
			Source:    sourceFor(fqdn, sources, cfg),
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

// sourceFor returns fqdn's install source given the precomputed root source
// map (rootSourceMap): that map's own entry when fqdn is a root, or
// cfg.Server otherwise - the same reference mapping MetadataProvider's
// sourceOf uses, so the server that actually served fqdn and the Source
// recorded on its resolved collection always agree. A transitive dependency
// (any fqdn absent from sources) always resolves to cfg.Server; there is no
// source inheritance from whichever parent(s) require it.
func sourceFor(fqdn string, sources map[string]string, cfg *config.Config) string {
	if source, ok := sources[fqdn]; ok {
		return source
	}
	return cfg.Server
}
