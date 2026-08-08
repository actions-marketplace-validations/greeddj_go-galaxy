package collections

// This file pins the incremental snapshot-reuse path
// (tryIncrementalResolveWithSnapshot), since its recursive
// resolveCollectionsInternal call for the changed-root subset goes through
// the version solver: an unchanged root's subgraph must be served verbatim
// from the snapshot with zero network access, a newly added root's subgraph
// must be resolved fresh through the solver, the two must merge into one
// correct graph, and a genuine version clash between the preserved subset
// and the freshly-resolved subset must fall back to a full, combined-context
// solve rather than failing or silently keeping stale data.

import (
	"context"
	"net/http"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestIncrementalResolveMergesPreservedAndSolverResolvedSubsets drives two
// resolveCollectionsInternal calls sharing one store: the first resolves a
// single root (acme.app, depending on acme.lib) and records the snapshot;
// the second adds a brand new root (acme.tool) while keeping acme.app's own
// spec identical, which routes resolution through
// tryIncrementalResolveWithSnapshot. Every endpoint acme.app/acme.lib could
// touch is armed to fail hard before the second call, so if the incremental
// path re-resolved either of them instead of preserving them from the
// snapshot, the call would error out rather than silently passing.
func TestIncrementalResolveMergesPreservedAndSolverResolvedSubsets(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", testVersion100, map[string]string{"acme.lib": "^1.0.0"})
	srv.AddVersion("acme", "lib", testVersion100, nil)
	srv.AddVersion("acme", "tool", testVersion100, nil)

	cfg := &config.Config{Server: srv.URL(), Workers: 2}
	runtime := infra.New(noopPrinter{}, srv.Client())
	st := store.New()

	appRoot := collection{Namespace: "acme", Name: "app", Constraint: "^1.0.0", Source: srv.URL()}

	resolved1, graph1, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(cfg, runtime, st), []collection{appRoot}, true, true,
	)
	if err != nil {
		t.Fatalf("first resolveCollectionsInternal: %v", err)
	}
	appKey, libKey := resolved1["acme.app"].key(), resolved1["acme.lib"].key()
	assertAppLibResolved(t, "first", resolved1, graph1, appKey, libKey)

	armHardFailure(srv, "app")
	armHardFailure(srv, "lib")

	toolRoot := collection{Namespace: "acme", Name: "tool", Constraint: "^1.0.0", Source: srv.URL()}
	resolved2, graph2, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(cfg, runtime, st), []collection{appRoot, toolRoot}, true, true,
	)
	if err != nil {
		t.Fatalf("second resolveCollectionsInternal (acme.app/acme.lib must be preserved, not re-fetched): %v", err)
	}
	assertAppLibResolved(t, "second", resolved2, graph2, appKey, libKey)
	assertToolResolvedFresh(t, resolved2, graph2)
}

// assertAppLibResolved asserts acme.app and acme.lib are both at
// testVersion100 in resolved, and that graph carries the app -> lib edge -
// shared between the first (fresh) and second (preserved) resolve of this
// test, so both calls check the exact same invariant.
func assertAppLibResolved(t *testing.T, label string, resolved map[string]collection, graph map[string][]string, appKey, libKey string) {
	t.Helper()
	if resolved["acme.app"].Version != testVersion100 || resolved["acme.lib"].Version != testVersion100 {
		t.Fatalf("%s resolve = %v, want acme.app and acme.lib both at %s", label, resolved, testVersion100)
	}
	if edges := graph[appKey]; len(edges) != 1 || edges[0] != libKey {
		t.Fatalf("%s graph[%s] = %v, want exactly [%s]", label, appKey, edges, libKey)
	}
}

// assertToolResolvedFresh asserts the newly added acme.tool root resolved
// to testVersion100 with an empty (no-deps) graph node.
func assertToolResolvedFresh(t *testing.T, resolved map[string]collection, graph map[string][]string) {
	t.Helper()
	tool, ok := resolved["acme.tool"]
	if !ok || tool.Version != testVersion100 {
		t.Fatalf("resolved[acme.tool] = %+v, want version %s", tool, testVersion100)
	}
	toolKey := tool.key()
	if edges, ok := graph[toolKey]; !ok || len(edges) != 0 {
		t.Fatalf("graph[%s] = %v, ok=%v, want a present, empty (no-deps) node", toolKey, edges, ok)
	}
}

// armHardFailure arms an indefinite 500 on every endpoint a collection's
// resolution could touch, so any request to it - root metadata, versions
// list, or version detail - fails the call it is part of.
func armHardFailure(srv *fakegalaxy.Server, name string) {
	for _, ep := range []fakegalaxy.Endpoint{
		fakegalaxy.EndpointRootMetadata,
		fakegalaxy.EndpointVersionsList,
		fakegalaxy.EndpointVersionDetail,
	} {
		srv.Fail(ep, "acme", name, fakegalaxy.Fault{Status: http.StatusInternalServerError, Count: -1})
	}
}

// TestIncrementalMergeConflictFallsBackToFullSolve pins the recovery path:
// when the preserved (unchanged-root) subset and the freshly solver-resolved
// (changed-root) subset disagree on a shared transitive dependency's
// version, the incremental merge must not fail the whole resolve or keep
// either side's partial-context answer - it must fall back to a full,
// combined-context solve. The three constraint values are chosen so that
// the preserved answer (1.6.0), the changed-subset-alone answer (1.9.0),
// and the only value a full combined solve can produce (1.3.0) are all
// distinct: landing on 1.3.0 is only possible via genuine recomputation
// with both roots' constraints in view together.
func TestIncrementalMergeConflictFallsBackToFullSolve(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	for _, v := range []string{testVersion100, "1.3.0", "1.6.0", "1.9.0"} {
		srv.AddVersion("acme", "shared", v, nil)
	}
	srv.AddVersion("acme", "a", testVersion100, map[string]string{"acme.shared": "<1.9.0"})
	srv.AddVersion("acme", "b", testVersion100, map[string]string{"acme.shared": "!=1.6.0"})

	cfg := &config.Config{Server: srv.URL(), Workers: 2}
	runtime := infra.New(noopPrinter{}, srv.Client())
	st := store.New()

	rootA := collection{Namespace: "acme", Name: "a", Constraint: "^1.0.0", Source: srv.URL()}

	resolved1, _, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(cfg, runtime, st), []collection{rootA}, true, true,
	)
	if err != nil {
		t.Fatalf("first resolveCollectionsInternal: %v", err)
	}
	// acme.a alone resolves acme.shared against only its own "<1.9.0" bound:
	// the highest version below 1.9.0 is 1.6.0. This is what gets preserved.
	if got := resolved1["acme.shared"].Version; got != "1.6.0" {
		t.Fatalf("first resolve acme.shared = %q, want 1.6.0 (acme.a's own isolated bound)", got)
	}

	rootB := collection{Namespace: "acme", Name: "b", Constraint: "^1.0.0", Source: srv.URL()}
	resolved2, graph2, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(cfg, runtime, st), []collection{rootA, rootB}, true, true,
	)
	if err != nil {
		t.Fatalf("second resolveCollectionsInternal (must fall back to a full solve, not fail): %v", err)
	}

	// acme.b alone (the incremental path's changed-subset resolve) would
	// pick 1.9.0 for acme.shared ("!=1.6.0" alone is satisfied by the
	// highest overall version) - conflicting with the preserved 1.6.0. Only
	// a full solve combining "<1.9.0" (from a) with "!=1.6.0" (from b),
	// leaving {1.0.0, 1.3.0} as candidates, lands on 1.3.0.
	assertFullSolveRecovered(t, resolved2, graph2)
}

// assertFullSolveRecovered asserts the full-solve fallback's expected
// combined-context outcome: acme.shared at 1.3.0 (unreachable by either
// isolated resolution), both roots at testVersion100, and a graph edge from
// each root to the shared dependency.
func assertFullSolveRecovered(t *testing.T, resolved map[string]collection, graph map[string][]string) {
	t.Helper()
	if got := resolved["acme.shared"].Version; got != "1.3.0" {
		t.Fatalf("second resolve acme.shared = %q, want 1.3.0 (the full-context combined resolution)", got)
	}
	if resolved["acme.a"].Version != testVersion100 || resolved["acme.b"].Version != testVersion100 {
		t.Fatalf("second resolve = %v, want both acme.a and acme.b at %s", resolved, testVersion100)
	}

	sharedKey := resolved["acme.shared"].key()
	if edges := graph[resolved["acme.a"].key()]; len(edges) != 1 || edges[0] != sharedKey {
		t.Fatalf("graph[acme.a] = %v, want exactly [%s]", edges, sharedKey)
	}
	if edges := graph[resolved["acme.b"].key()]; len(edges) != 1 || edges[0] != sharedKey {
		t.Fatalf("graph[acme.b] = %v, want exactly [%s]", edges, sharedKey)
	}
}

// TestMergeResolvedGraphsDetectsVersionConflict is a fast, pure unit test of
// mergeResolvedGraphs' own conflict-detection rule, independent of any
// network simulation: two resolved sets sharing a fqdn at different
// versions must not merge, while two resolved sets that are either disjoint
// or agree on any shared fqdn must merge cleanly.
func TestMergeResolvedGraphsDetectsVersionConflict(t *testing.T) {
	t.Parallel()
	preservedResolved := map[string]collection{
		"acme.a":      {Namespace: "acme", Name: "a", Version: testVersion100},
		"acme.shared": {Namespace: "acme", Name: "shared", Version: "1.6.0"},
	}
	preservedGraph := map[string][]string{
		"acme.a@1.0.0":      {"acme.shared@1.6.0"},
		"acme.shared@1.6.0": {},
	}

	t.Run("conflicting version for the same fqdn", func(t *testing.T) {
		t.Parallel()
		resolvedNew := map[string]collection{
			"acme.b":      {Namespace: "acme", Name: "b", Version: testVersion100},
			"acme.shared": {Namespace: "acme", Name: "shared", Version: "1.9.0"},
		}
		graphNew := map[string][]string{
			"acme.b@1.0.0":      {"acme.shared@1.9.0"},
			"acme.shared@1.9.0": {},
		}
		if _, _, ok := mergeResolvedGraphs(preservedResolved, preservedGraph, resolvedNew, graphNew); ok {
			t.Fatalf("mergeResolvedGraphs succeeded, want a conflict (acme.shared has two different versions)")
		}
	})

	t.Run("disjoint new subset merges cleanly", func(t *testing.T) {
		t.Parallel()
		resolvedNew := map[string]collection{
			"acme.tool": {Namespace: "acme", Name: "tool", Version: testVersion100},
		}
		graphNew := map[string][]string{
			"acme.tool@1.0.0": {},
		}
		merged, mergedGraph, ok := mergeResolvedGraphs(preservedResolved, preservedGraph, resolvedNew, graphNew)
		if !ok {
			t.Fatalf("mergeResolvedGraphs failed unexpectedly for a disjoint new subset")
		}
		if len(merged) != 3 {
			t.Fatalf("merged = %v, want 3 entries (acme.a, acme.shared, acme.tool)", merged)
		}
		if len(mergedGraph) != 3 {
			t.Fatalf("mergedGraph = %v, want 3 entries", mergedGraph)
		}
	})
}
