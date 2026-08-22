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
	return solverResultToResolvedGraph(result, deps.cfg, mp.bindings, sources, mp.pins)
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
// solve (see MetadataProvider.recordBinding), and sources is the same
// roots' explicit-source map (rootSourceMap) that provider resolved
// against; together with cfg they decide every resolved entry's Source, in
// the order sourceFor documents. A binding outranks a root's own Source
// field there because it names the server that actually served the
// collection, which for an unpinned root the requirement alone cannot say.
func solverResultToResolvedGraph(
	result *solver.Result,
	cfg *config.Config,
	bindings, sources map[string]string,
	pins map[string]exactPin,
) (map[string]collection, map[string][]string, error) {
	resolved := make(map[string]collection, len(result.Versions))
	for fqdn, version := range result.Versions {
		ns, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, nil, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, fqdn)
		}
		col := collection{
			Namespace: ns,
			Name:      name,
			Version:   version,
		}
		stampResolvedSource(&col, fqdn, pins, bindings, sources, cfg)
		resolved[fqdn] = col
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

// stampResolvedSource stamps col's Source (and pin-carried fields) from its
// discovery pin when one exists, else from sourceFor. A git or url pin's
// source is its locator, taken from the pin itself: the solver decides an
// exact pin through Dependencies alone, and MetadataProvider answers a
// pinned fqdn from the pin without asking any server, so no binding is ever
// recorded for it and sourceFor would otherwise reach for a Galaxy server
// that holds no such collection. A url pin additionally stamps its sha256
// onto the collection, which is what makes verifyPinnedSHA enforce the
// origin digest on every install and warm, not only under --frozen.
//
// That same "an exact pin is decided through Dependencies alone" property is
// why sourceFor consults sources at all: a Galaxy root pinned to an exact
// version also leaves the solve unbound whenever its Dependencies call
// answered without a fetch, which is what cfg.NoDeps does to every such root.
func stampResolvedSource(
	col *collection,
	fqdn string,
	pins map[string]exactPin,
	bindings, sources map[string]string,
	cfg *config.Config,
) {
	switch pin, pinned := pins[fqdn]; {
	case pinned && pin.typ == typeGit:
		col.Source, col.Type, col.Ref = pin.locator, typeGit, pin.ref
	case pinned:
		col.Source, col.Type, col.SHA256 = pin.locator, typeURL, pin.sha256
	default:
		col.Source = sourceFor(fqdn, bindings, sources, cfg)
	}
}

// sourceFor returns fqdn's install source, decided in three tiers.
//
// A binding wins: the server base fqdn was bound to during the solve (see
// MetadataProvider.recordBinding, which records a root and a transitive
// dependency alike), so the resolved collection's Source always agrees with
// whichever server served it under this run's first-match ownership rule.
//
// A root carrying an explicit source: that got no binding falls back to that
// source:. Such a root has exactly one server candidate by construction - a
// source: nails its fqdn to one server for the whole run - so which server
// serves it needs no network evidence at all, and pinnedServerCandidate is
// what turns the requirement's spelling into that candidate. Its base is
// what gets stamped, not the raw spelling, so this tier and the binding tier
// record the same value for the same root: a source: naming a server_list id
// records that server's URL rather than the id, and every consumer keying on
// Source - the artifact cache key, the lockfile entry, the installed record -
// sees one spelling whichever tier answered. The case that reaches this tier
// is cfg.NoDeps: an exactly pinned root is settled there through a
// Dependencies call NewNoDepsProvider answers itself, so neither the real
// provider nor any server ever sees the root, and boundBaseFor - which binds
// a single-candidate root without fetching anything - is never reached for it
// either.
//
// firstServerURL is the last tier, and the only one left for a collection
// with no source: of its own to fall back to: an unpinned root, recorded in
// sources as "", or a transitive dependency, never recorded there at all.
func sourceFor(fqdn string, bindings, sources map[string]string, cfg *config.Config) string {
	if base, ok := bindings[fqdn]; ok {
		return base
	}
	// cfg == nil never happens in production (see firstServerURL) but would
	// panic in pinnedServerCandidate's own server-list walk, so the pinned
	// tier is skipped for it rather than guarded one level down.
	if source := sources[fqdn]; source != "" && cfg != nil {
		candidate, _ := pinnedServerCandidate(cfg, source)
		return candidate.base
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
