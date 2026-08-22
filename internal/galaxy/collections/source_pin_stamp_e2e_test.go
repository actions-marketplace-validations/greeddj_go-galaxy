package collections_test

// This file drives the install phase's own reading of a resolved Source end
// to end: a root pinned with source: must be fetched from that server by
// every phase, not only by the resolve that discovered it. Every entry it
// writes names an exact version, the constraint shape that makes the solver
// settle a root through its exact-pin fast path - through Dependencies
// alone, never Highest - which is the shape that exposes what these tests
// guard. See e2e_test.go for this package's own doc comment, and
// multi_server_e2e_test.go for the fixtures reused here.

import (
	"context"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// mirrorPath is the base path of the caching-proxy fixture's mirror
// endpoint - a second Galaxy endpoint under the configured server's own
// origin, the shape a proxy fronting several upstreams serves.
const mirrorPath = "/galaxy/mirror"

// pinnedReqSpec is one requirements.yml entry for buildPinnedRequirements: a
// collection at an exact version, optionally pinned to a source.
type pinnedReqSpec struct {
	name    string
	version string
	source  string
}

// buildPinnedRequirements renders entries into a requirements.yml body, each
// at its own exact version, with a source: line only where one is set.
func buildPinnedRequirements(entries []pinnedReqSpec) string {
	var b strings.Builder
	b.WriteString("collections:\n")
	for _, e := range entries {
		b.WriteString("  - name: " + e.name + "\n    version: \"" + e.version + "\"\n")
		if e.source != "" {
			b.WriteString("    source: " + e.source + "\n")
		}
	}
	return b.String()
}

// TestSourcePinnedExactRootInstallsFromItsOwnServer is the end-to-end guard
// for the whole path a source: has to survive: the resolve, the resolved
// snapshot, and the install phase's own second metadata read.
//
// The fixture is the caching-proxy shape that exposes it. One origin serves
// the mirror endpoint the two roots pin themselves to; the configured server
// is a DIFFERENT path under that same origin, which answers nothing - just
// as a proxy endpoint fronting an upstream that does not carry these
// collections would 404 them. Two roots are what makes it deterministic:
// two roots turn prewarmRootMetadata on, and it warms an exactly pinned
// root's dependency map on a provider whose bindings are discarded, so the
// solve's own Dependencies call returns from the warm cache without a fetch
// that could bind the root to its server.
//
// Reading the pinned source: back off the roots is therefore the only thing
// left that can keep the install phase pointed at the mirror. Stamping the
// configured server instead sends it to the endpoint that carries nothing,
// which fails the run.
func TestSourcePinnedExactRootInstallsFromItsOwnServer(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.NewAtBasePath(t, mirrorPath)
	one := srv.AddVersion("ns", "one", "1.0.0", nil)
	two := srv.AddVersion("ns", "two", "2.0.0", nil)

	mirror := srv.URL() + mirrorPath
	// The configured server shares the mirror's origin, so the pin matches it
	// by origin and no unmatched-source warning is in play, but it names a
	// path this fixture serves nothing under.
	servers := []config.Server{{ID: "proxy", URL: srv.URL() + "/galaxy/upstream"}}
	cfg := newMultiServerConfig(t, servers, buildPinnedRequirements([]pinnedReqSpec{
		{name: "ns.one", version: "1.0.0", source: mirror},
		{name: "ns.two", version: "2.0.0", source: mirror},
	}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "one")
	msAssertInstalled(t, cfg.DownloadPath, "two")

	// The artifact cache key folds the resolved Source in, so finding the
	// mirror's own bytes under the mirror's key is the on-disk proof that the
	// mirror, not the configured server, is what the run recorded.
	if got := msArtifactSHA256(t, cfg.CacheDir, mirror, "one", "1.0.0"); got != one.SHA256 {
		t.Fatalf("cached ns.one sha = %s, want the mirror's own %s", got, one.SHA256)
	}
	if got := msArtifactSHA256(t, cfg.CacheDir, mirror, "two", "2.0.0"); got != two.SHA256 {
		t.Fatalf("cached ns.two sha = %s, want the mirror's own %s", got, two.SHA256)
	}

	lf := msLockFile(t, cfg, runtime)
	for _, fqdn := range []string{"ns.one", "ns.two"} {
		if e := findLockEntry(t, lf, fqdn); e.Source != mirror {
			t.Fatalf("%s lockfile source = %q, want the pinned mirror %q", fqdn, e.Source, mirror)
		}
	}

	srv.ResetCounts()
	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := srv.Total(); got != 0 {
		t.Fatalf("srv.Total() on reinstall = %d, want 0 - the recorded source must replay, not re-resolve", got)
	}
}

// TestSourcePinnedExactRootUnderNoDepsInstallsFromItsOwnServer is the
// --no-deps half of the same guarantee, and the one case no binding can
// cover: NewNoDepsProvider answers an exactly pinned root's Dependencies
// without the real provider, so the solve reaches no server at all and the
// root's own source: is the only record of which one serves it. The pin is
// spelled as a server_list id here, which the resolved Source must record as
// that server's URL rather than as the id.
func TestSourcePinnedExactRootUnderNoDepsInstallsFromItsOwnServer(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	published := srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildPinnedRequirements([]pinnedReqSpec{
		{name: "ns.x", version: "1.0.0", source: "b"},
	}))
	cfg.NoDeps = true
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")
	if got := srvA.Total(); got != 0 {
		t.Fatalf("srvA.Total() = %d, want 0 - a pinned root never consults the first configured server", got)
	}
	if got := msArtifactSHA256(t, cfg.CacheDir, srvB.URL(), "x", "1.0.0"); got != published.SHA256 {
		t.Fatalf("cached ns.x sha = %s, want B's own %s", got, published.SHA256)
	}

	lf := msLockFile(t, cfg, runtime)
	if e := findLockEntry(t, lf, "ns.x"); e.Source != srvB.URL() {
		t.Fatalf("ns.x lockfile source = %q, want B's URL %q - an id must resolve to the server it names", e.Source, srvB.URL())
	}
}
