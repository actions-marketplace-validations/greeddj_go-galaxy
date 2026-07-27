package collections

import (
	"context"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// testVersion200 is this file's second-most-common fixture version literal
// (testVersion100, this package's first, already covers "1.0.0"), pulled
// out as a const purely to satisfy goconst.
const testVersion200 = "2.0.0"

// TestSolveCollectionsMultiSourceRoots drives two roots, each pinned to its
// own explicit Source, against two independent fake servers - each server
// registers a distinguishable version of its own collection only - and
// asserts solveCollections fetches each root from its own server (not the
// other) and records that same server as the resolved collection's Source.
func TestSolveCollectionsMultiSourceRoots(t *testing.T) {
	t.Parallel()
	srvX := fakegalaxy.New(t)
	srvY := fakegalaxy.New(t)
	srvX.AddVersion("acme", "a", testVersion100, nil)
	srvY.AddVersion("acme", "b", testVersion200, nil)

	runtime := infra.New(noopPrinter{}, srvX.Client())
	cfg := &config.Config{Server: srvX.URL()}
	roots := []collection{
		{Namespace: "acme", Name: "a", Source: srvX.URL(), Constraint: "^1.0.0"},
		{Namespace: "acme", Name: "b", Source: srvY.URL(), Constraint: "^2.0.0"},
	}

	resolved, _, err := solveCollections(context.Background(), newCollectionDeps(cfg, runtime, store.New()), roots)
	if err != nil {
		t.Fatalf("solveCollections: unexpected error: %v", err)
	}

	a, ok := resolved["acme.a"]
	if !ok || a.Version != testVersion100 || a.Source != srvX.URL() {
		t.Fatalf("resolved[acme.a] = %+v, want version 1.0.0 from %s", a, srvX.URL())
	}
	b, ok := resolved["acme.b"]
	if !ok || b.Version != testVersion200 || b.Source != srvY.URL() {
		t.Fatalf("resolved[acme.b] = %+v, want version 2.0.0 from %s", b, srvY.URL())
	}
}

// TestSolveCollectionsTransitiveDepUsesDefaultServer pins the reference
// behavior a naive parent-source-inheritance reading would get wrong: a
// transitive dependency always resolves against cfg.Server, never against
// the source of whichever parent required it. The root lives on its own
// explicit-source server and depends on a package published only on the
// default server; the dependency must still resolve, with cfg.Server (not
// the root's server) recorded as its Source.
func TestSolveCollectionsTransitiveDepUsesDefaultServer(t *testing.T) {
	t.Parallel()
	srvRoot := fakegalaxy.New(t)
	srvDefault := fakegalaxy.New(t)
	srvRoot.AddVersion("acme", "a", testVersion100, map[string]string{"acme.dep": "^1.0.0"})
	srvDefault.AddVersion("acme", "dep", testVersion100, nil)

	runtime := infra.New(noopPrinter{}, srvRoot.Client())
	cfg := &config.Config{Server: srvDefault.URL()}
	roots := []collection{
		{Namespace: "acme", Name: "a", Source: srvRoot.URL(), Constraint: "^1.0.0"},
	}

	resolved, graph, err := solveCollections(context.Background(), newCollectionDeps(cfg, runtime, store.New()), roots)
	if err != nil {
		t.Fatalf("solveCollections: unexpected error: %v", err)
	}

	dep, ok := resolved["acme.dep"]
	if !ok {
		t.Fatalf("resolved is missing acme.dep: %v", resolved)
	}
	if dep.Version != testVersion100 {
		t.Fatalf("resolved[acme.dep].Version = %q, want 1.0.0", dep.Version)
	}
	if dep.Source != srvDefault.URL() {
		t.Fatalf("resolved[acme.dep].Source = %q, want cfg.Server (%s), not the root's own source (%s)",
			dep.Source, srvDefault.URL(), srvRoot.URL())
	}

	rootKey := resolved["acme.a"].key()
	depKey := dep.key()
	edges := graph[rootKey]
	if len(edges) != 1 || edges[0] != depKey {
		t.Fatalf("graph[%s] = %v, want exactly [%s]", rootKey, edges, depKey)
	}
}

// TestSolveCollectionsSharedTransitiveDepUsesDefaultServer covers the
// unambiguous case: two roots on two DIFFERENT explicit-source servers both
// depend on the same package, published only on the default server. Neither
// root's source ever touches the shared dependency; it resolves once,
// against cfg.Server.
func TestSolveCollectionsSharedTransitiveDepUsesDefaultServer(t *testing.T) {
	t.Parallel()
	srvX := fakegalaxy.New(t)
	srvY := fakegalaxy.New(t)
	srvDefault := fakegalaxy.New(t)
	srvX.AddVersion("acme", "a", testVersion100, map[string]string{"acme.shared": "^1.0.0"})
	srvY.AddVersion("acme", "b", testVersion100, map[string]string{"acme.shared": "^1.0.0"})
	srvDefault.AddVersion("acme", "shared", testVersion100, nil)

	runtime := infra.New(noopPrinter{}, srvX.Client())
	cfg := &config.Config{Server: srvDefault.URL()}
	roots := []collection{
		{Namespace: "acme", Name: "a", Source: srvX.URL(), Constraint: "^1.0.0"},
		{Namespace: "acme", Name: "b", Source: srvY.URL(), Constraint: "^1.0.0"},
	}

	resolved, _, err := solveCollections(context.Background(), newCollectionDeps(cfg, runtime, store.New()), roots)
	if err != nil {
		t.Fatalf("solveCollections: unexpected error: %v", err)
	}

	shared, ok := resolved["acme.shared"]
	if !ok {
		t.Fatalf("resolved is missing acme.shared: %v", resolved)
	}
	if shared.Source != srvDefault.URL() {
		t.Fatalf("resolved[acme.shared].Source = %q, want cfg.Server (%s)", shared.Source, srvDefault.URL())
	}
}

// TestRootSourceMap pins rootSourceMap's per-root rule directly: a root
// with an explicit Source maps to that source, an unpinned root maps to ""
// (a distinct, stable value - not cfg.Server), and a transitive
// dependency's fqdn is simply never a key.
func TestRootSourceMap(t *testing.T) {
	t.Parallel()
	roots := []collection{
		{Namespace: "acme", Name: "a", Source: "https://explicit.example"},
		{Namespace: "acme", Name: "b"},
	}

	sources := rootSourceMap(roots)
	if sources["acme.a"] != "https://explicit.example" {
		t.Fatalf("sources[acme.a] = %q, want the explicit source", sources["acme.a"])
	}
	if v, ok := sources["acme.b"]; !ok || v != "" {
		t.Fatalf("sources[acme.b] = (%q, %v), want (\"\", true) - unpinned", v, ok)
	}
	if _, ok := sources["acme.dep"]; ok {
		t.Fatalf("sources unexpectedly has an entry for a non-root fqdn: %v", sources)
	}
}

// TestMetadataProviderSourceOf pins MetadataProvider.sourceOf's own
// fallback directly: a fqdn recorded in sources returns that source, and
// any other fqdn (a transitive dependency, or a root recorded with an empty
// source, never distinguished here) falls back to "" - unpinned - so
// serverCandidates walks the whole configured server list for it; there is
// no inheritance from whichever parent required it.
func TestMetadataProviderSourceOf(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "https://default.example"}
	sources := map[string]string{"acme.a": "https://explicit.example"}
	p := NewMetadataProvider(context.Background(), cfg, infra.New(noopPrinter{}, nil), store.New(), sources)

	if got := p.sourceOf("acme.a"); got != "https://explicit.example" {
		t.Fatalf("sourceOf(acme.a) = %q, want the explicit source", got)
	}
	if got := p.sourceOf("acme.dep"); got != "" {
		t.Fatalf("sourceOf(acme.dep) = %q, want \"\" (unpinned)", got)
	}
}
