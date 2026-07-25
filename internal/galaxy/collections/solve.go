package collections

import (
	"context"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// solveCollections resolves roots via the PubGrub-style version solver
// instead of the greedy resolver, returning the same (resolved, graph) shape
// resolveCollectionsInternal's cold path does: resolved keyed by ns.name@version
// (collection.key()), graph mapping a parent key to its dependency keys. It
// is not wired into any production path yet - callers opt in explicitly.
func solveCollections(ctx context.Context, deps collectionDeps, roots []collection) (map[string]collection, map[string][]string, error) {
	var provider solver.Provider = NewMetadataProvider(ctx, deps.cfg, deps.runtime, deps.st)
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

// buildSolverRequirements builds one solver.Requirement per unique root
// fqdn from roots, mirroring resolverState.enqueueRoots/addRootConstraint's
// exact per-fqdn dedup: a root's effective constraint is its own Constraint,
// falling back to its Version when Constraint is empty; a constraint that
// normalizes to the empty string ("*", or already empty) never touches the
// stored value at all (mirroring addRootConstraint's own no-op on an empty
// constraint), so two roots naming the same fqdn - one bare, one
// constrained - never conflict; two roots naming the same fqdn with
// different NON-EMPTY normalized constraints do conflict, reported through
// the same helpers.ErrConflictingRootConstraints sentinel addRootConstraint
// uses.
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

// solverResultToResolvedGraph maps a solver.Result onto the same
// (resolved, graph) shape the greedy resolver's buildGraph produces: resolved
// keyed by fqdn (mirroring resolverState.resolved), graph keyed by
// collection.key() (mirroring buildGraphFromDeps' own output).
func solverResultToResolvedGraph(
	result *solver.Result,
	roots []collection,
	cfg *config.Config,
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
			Source:    sourceFor(fqdn, roots, cfg),
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

// sourceFor returns fqdn's install source: the matching root's own explicit
// Source when one is set, or cfg.Server otherwise. This is a harness-only
// mapping for the solver slot-in; a non-root dependency's own registry
// source is not modeled here.
func sourceFor(fqdn string, roots []collection, cfg *config.Config) string {
	for _, root := range roots {
		if fmt.Sprintf("%s.%s", root.Namespace, root.Name) == fqdn && root.Source != "" {
			return root.Source
		}
	}
	return cfg.Server
}
