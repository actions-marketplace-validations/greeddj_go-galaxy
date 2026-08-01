package collections

// This file pins the !cfg.Offline term in refreshBypassesSnapshot against
// the specific state where it is actually load-bearing, not a merely
// theoretical one: Store.snapshotData age-evicts APICache, DepsCache, and
// Versions against helpers.CacheEntryMaxAge, but copies Resolved, Graph, and
// Requirements with no staleness filter at all, and Store.ClearCaches wipes
// exactly the same three metadata buckets. So "a resolve snapshot survives
// while the metadata caches a fallback solve would need are gone" is the
// normal shape of any cache older than the metadata TTL (or one explicitly
// cleared), not a corner case this term merely guards against in theory.

import (
	"context"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestRefreshOfflinePreservesResolveWithStaleMetadataCaches proves
// --refresh --offline still resolves successfully, purely from the
// persisted resolve snapshot, when the metadata caches (APICache, DepsCache,
// Versions) a fallback solve would otherwise need are empty - the shape
// Store.ClearCaches (and, identically, snapshotData's own age eviction)
// leaves behind. TestOfflineOutranksRefresh (e2e_test.go, in the external
// collections_test package) cannot pin this: its own fixture's API cache is
// still warm from the seeding install, so even a broken !cfg.Offline term
// would be served from that cache without ever reaching the network -
// proving the "no network touched" half of that test's claim by accident of
// fixture, not by the offline check actually firing.
//
// The store here is built directly (mirroring poisoned_version_test.go's
// seedResolvedSnapshot) rather than produced by a real install, so its
// metadata caches start empty by construction; st.ClearCaches() is called
// anyway, to state that emptiness as the point under test rather than an
// incidental property of a freshly built store. The runtime's HTTP client is
// fetch.NewOffline, matching how cmd/go-galaxy/commands/install.go actually
// enforces cfg.Offline in production (a real network-rejecting transport,
// not merely the cfg.Offline bool) - every other e2e fixture in this package
// wires a real fakegalaxy client regardless of cfg.Offline, which is exactly
// why none of them can pin this term either.
//
// resolveCollectionsInternal is called directly, not through Start or Lock:
// both of those would go on to need a version-specific metadata fetch (for
// the artifact sha or the download URL) that this fixture deliberately does
// not cache either, which would fail closed under the offline transport for
// a reason unrelated to the resolve-snapshot veto this test exists to pin.
//
// Mutation (dropping `&& !cfg.Offline` from refreshBypassesSnapshot, leaving
// `cfg != nil && cfg.Refresh`) confirmed to fail this test with:
//
//	refresh_offline_test.go:91: resolveCollectionsInternal (refresh +
//	offline, resolve snapshot only, no metadata cache): probing highest
//	version of acme.widgets: Get "http://127.0.0.1:PORT/api/v3/collections/
//	acme/widgets/": offline mode is enabled, network access is forbidden:
//	GET http://127.0.0.1:PORT/api/v3/collections/acme/widgets/
//	--- FAIL: TestRefreshOfflinePreservesResolveWithStaleMetadataCaches (0.00s)
//
// The identical mutation leaves TestOfflineOutranksRefresh (the external
// e2e test) passing, confirmed by running it against the same mutation -
// the concrete demonstration of why that test's own fixture cannot pin this
// term.
func TestRefreshOfflinePreservesResolveWithStaleMetadataCaches(t *testing.T) {
	t.Parallel()
	cfg, _ := poisonedVersionFixture(t)
	cfg.Refresh = true
	cfg.Offline = true
	runtime := infra.New(noopPrinter{}, fetch.NewOffline(0))

	prep, err := loadRoots(cfg, runtime)
	if err != nil {
		t.Fatalf("loadRoots: %v", err)
	}
	reqSpec := buildRequirementsSpec(prep.AllRoots)
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps, serversSignature(cfg))

	st := store.New()
	st.SetResolvedAll(map[string]store.ResolvedEntry{
		"acme.widgets": {Version: testVersion100, Source: cfg.Server},
	})
	st.SetGraphSnapshot(map[string][]string{"acme.widgets@" + testVersion100: {}})
	st.SetMetaRequirements(reqHash, cfg.Server)
	st.SetRequirements(reqSpec)
	st.ClearCaches()

	deps := newCollectionDeps(cfg, runtime, st)
	resolved, _, err := resolveCollectionsInternal(context.Background(), deps, prep.AllRoots, true, true)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal (refresh + offline, resolve snapshot only, no metadata cache): %v", err)
	}
	got, ok := resolved["acme.widgets"]
	if !ok || got.Version != testVersion100 {
		t.Fatalf(`resolved["acme.widgets"] = %+v, ok=%v, want Version=%s (served from the resolve snapshot)`,
			got, ok, testVersion100)
	}
}
