package collections

// This file drives resolveWithoutDeps directly - the --no-deps fast path -
// against a real fakegalaxy stand-in, proving the fix at its source: an
// unpinned root resolves to a concrete version rather than keeping the
// literal "*" constraint, and an exactly pinned root short-circuits inside
// resolveTaskVersion before any metadata HTTP request.

import (
	"context"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestResolveWithoutDepsUnpinnedRootResolvesConcreteVersion asserts that an
// unpinned (version "*") root resolved through resolveWithoutDeps lands on a
// concrete version - the highest registered - instead of keeping "*" as its
// Version, which would otherwise flow verbatim into the artifact cache key
// and lockfile entry.
func TestResolveWithoutDepsUnpinnedRootResolvesConcreteVersion(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "solo", "1.0.0", nil)
	srv.AddVersion("acme", "solo", "2.0.0", nil)

	cfg := &config.Config{Server: srv.URL(), Workers: 2, NoDeps: true}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root := collection{Namespace: "acme", Name: "solo", Version: "*", Constraint: "*", Source: srv.URL()}
	resolved, graph, err := resolveWithoutDeps(context.Background(), deps, []collection{root}, false)
	if err != nil {
		t.Fatalf("resolveWithoutDeps: %v", err)
	}

	got, ok := resolved["acme.solo"]
	if !ok {
		t.Fatalf("expected a resolved entry for acme.solo, got %#v", resolved)
	}
	if got.Version != "2.0.0" {
		t.Fatalf("resolved version = %q, want %q (the highest registered version)", got.Version, "2.0.0")
	}
	if _, ok := graph[got.key()]; !ok {
		t.Fatalf("expected graph to contain a node for %s, got %#v", got.key(), graph)
	}
}

// TestResolveWithoutDepsPinnedRootSkipsMetadataFetch asserts that a root
// pinned to an exact version resolves with zero HTTP requests: the common
// --no-deps case (a pinned requirements.yml entry) must stay network-free,
// short-circuiting inside resolveTaskVersion before resolveRootMetadata is
// ever called.
func TestResolveWithoutDepsPinnedRootSkipsMetadataFetch(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "pinned", "1.0.0", nil)

	cfg := &config.Config{Server: srv.URL(), Workers: 2, NoDeps: true}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	root := collection{Namespace: "acme", Name: "pinned", Version: "1.0.0", Constraint: "1.0.0", Source: srv.URL()}
	resolved, _, err := resolveWithoutDeps(context.Background(), deps, []collection{root}, false)
	if err != nil {
		t.Fatalf("resolveWithoutDeps: %v", err)
	}

	got, ok := resolved["acme.pinned"]
	if !ok {
		t.Fatalf("expected a resolved entry for acme.pinned, got %#v", resolved)
	}
	if got.Version != "1.0.0" {
		t.Fatalf("resolved version = %q, want %q", got.Version, "1.0.0")
	}
	if total := srv.Total(); total != 0 {
		t.Fatalf("expected 0 HTTP requests for a pinned root, got %d (request counts: root=%d versions=%d detail=%d artifact=%d)",
			total,
			srv.Count(fakegalaxy.EndpointRootMetadata),
			srv.Count(fakegalaxy.EndpointVersionsList),
			srv.Count(fakegalaxy.EndpointVersionDetail),
			srv.Count(fakegalaxy.EndpointArtifact),
		)
	}
}
