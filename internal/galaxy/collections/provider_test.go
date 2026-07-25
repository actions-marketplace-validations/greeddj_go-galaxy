package collections

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// testVersion100 is this file's dominant test fixture version literal,
// pulled out as a const purely to satisfy goconst - it carries no domain
// meaning beyond "a generic first version".
const testVersion100 = "1.0.0"

// mustSolverVersion parses testVersion100 as a solver.Version or fails the
// test. Every call site needs the same version, so this hardcodes it rather
// than threading an always-identical parameter.
func mustSolverVersion(t *testing.T) solver.Version {
	t.Helper()
	v, err := solver.NewVersion(testVersion100)
	if err != nil {
		t.Fatalf("solver.NewVersion(%q): %v", testVersion100, err)
	}
	return v
}

// countingProvider wraps a solver.Provider and counts calls to each method,
// so a test can assert a fast path really never reached a given method
// (rather than only inferring it from the fake server's own request count).
type countingProvider struct {
	solver.Provider

	universeCalls atomic.Int64
}

func (c *countingProvider) Universe(pkg string) ([]solver.Version, error) {
	c.universeCalls.Add(1)
	return c.Provider.Universe(pkg)
}

// TestProviderLazinessAvoidsVersionsList drives a full Solve against a
// MetadataProvider whose only root requirement is satisfied by the
// registry's own highest_version, and asserts Universe is never called and
// the fake server never receives a versions-list request: the highest_version
// probe alone must be enough to decide the package.
func TestProviderLazinessAvoidsVersionsList(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	p := &countingProvider{Provider: newTestMetadataProvider(t, srv)}
	reqs := []solver.Requirement{{Package: "acme.widgets", Constraint: "^1.0.0"}}

	result, err := solver.Solve(reqs, p)
	if err != nil {
		t.Fatalf("Solve: unexpected error: %v", err)
	}
	if result.Versions["acme.widgets"] != testVersion100 {
		t.Fatalf("Versions[acme.widgets] = %q, want 1.0.0", result.Versions["acme.widgets"])
	}
	if got := p.universeCalls.Load(); got != 0 {
		t.Fatalf("Universe was called %d times, want 0 (the highest_version probe should have sufficed)", got)
	}
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Fatalf("versions-list requests = %d, want 0", got)
	}
}

// TestProviderDependenciesWarmPinIsZeroNetwork warms the deps cache under
// the exact key resolveOne itself would use, then asserts Dependencies
// serves it without ever touching the (otherwise-empty, would-404) fake
// server.
func TestProviderDependenciesWarmPinIsZeroNetwork(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	st := store.New()
	st.SetDepsCache("acme.widgets@1.0.0", map[string]string{"acme.other": "^2.0.0"})

	p := NewMetadataProvider(context.Background(), testConfig(srv), testRuntime(srv), st, nil)
	deps, err := p.Dependencies("acme.widgets", mustSolverVersion(t))
	if err != nil {
		t.Fatalf("Dependencies: unexpected error: %v", err)
	}
	if len(deps) != 1 || deps["acme.other"] != "^2.0.0" {
		t.Fatalf("Dependencies = %v, want {acme.other: ^2.0.0}", deps)
	}
	if got := srv.Total(); got != 0 {
		t.Fatalf("server received %d requests, want 0 (warm pin must be zero-network)", got)
	}
}

// TestProviderOfflineMissReturnsErrOfflineMode drives Dependencies and
// Universe with an offline HTTP client and an empty cache, and asserts both
// surface errors.Is(err, helpers.ErrOfflineMode) rather than any other
// error shape.
func TestProviderOfflineMissReturnsErrOfflineMode(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "http://offline.example.invalid", Offline: true}
	runtime := infra.New(noopPrinter{}, fetch.NewOffline(0))
	p := NewMetadataProvider(context.Background(), cfg, runtime, store.New(), nil)

	if _, err := p.Universe("acme.widgets"); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Universe error = %v, want errors.Is(err, ErrOfflineMode)", err)
	}
	if _, err := p.Dependencies("acme.widgets", mustSolverVersion(t)); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Dependencies error = %v, want errors.Is(err, ErrOfflineMode)", err)
	}
}

// TestProviderMalformedDependencyKeyAborts registers a version whose
// dependency map carries a key that is not a valid "ns.name" fqdn, and
// asserts Dependencies reports helpers.ErrInvalidDependencyKey directly, and
// that a full Solve depending on it aborts with that same error rather than
// producing a *solver.ConflictError.
func TestProviderMalformedDependencyKeyAborts(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, map[string]string{"not-a-fqdn": "^1.0.0"})

	p := newTestMetadataProvider(t, srv)
	_, err := p.Dependencies("acme.widgets", mustSolverVersion(t))
	if !errors.Is(err, helpers.ErrInvalidDependencyKey) {
		t.Fatalf("Dependencies error = %v, want errors.Is(err, ErrInvalidDependencyKey)", err)
	}

	reqs := []solver.Requirement{{Package: "acme.widgets", Constraint: "^1.0.0"}}
	_, solveErr := solver.Solve(reqs, p)
	if !errors.Is(solveErr, helpers.ErrInvalidDependencyKey) {
		t.Fatalf("Solve error = %v, want errors.Is(err, ErrInvalidDependencyKey)", solveErr)
	}
	var conflictErr *solver.ConflictError
	if errors.As(solveErr, &conflictErr) {
		t.Fatalf("Solve error is a *solver.ConflictError, want a plain aborting error: %v", solveErr)
	}
}

// TestBuildSolverUniverseIsDeterministic feeds buildSolverUniverse the same
// version set in several different (shuffled) input orders, including an
// equal-precedence tie (1.0.0 vs 1.0.0+build), and asserts every call
// produces the exact same descending order regardless of input order - the
// raw versions list can arrive in any order over the wire.
func TestBuildSolverUniverseIsDeterministic(t *testing.T) {
	t.Parallel()
	orderings := [][]string{
		{testVersion100, "1.0.0+build", "2.0.0", "1.5.0"},
		{"2.0.0", "1.0.0+build", "1.5.0", testVersion100},
		{"1.5.0", "2.0.0", testVersion100, "1.0.0+build"},
		{"1.0.0+build", testVersion100, "1.5.0", "2.0.0"},
	}
	// 1.0.0+build and 1.0.0 have equal semver precedence (build metadata is
	// not significant to Compare), so the tie-break is the original string
	// descending: "1.0.0+build" > "1.0.0" byte-wise, so it sorts first.
	want := []string{"2.0.0", "1.5.0", "1.0.0+build", testVersion100}

	var wantGot []string
	for i, raw := range orderings {
		got := buildSolverUniverse(raw)
		strs := make([]string, len(got))
		for j, v := range got {
			strs[j] = v.Original()
		}
		if i == 0 {
			wantGot = strs
			if !equalStrings(strs, want) {
				t.Fatalf("orderings[0] = %v, want %v", strs, want)
			}
			continue
		}
		if !equalStrings(strs, wantGot) {
			t.Fatalf("orderings[%d] = %v, want %v (must match every other input order)", i, strs, wantGot)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestProviderUnknownPackageIsNotAnError registers no version at all for
// "acme.ghost" and asserts Highest/Universe both report the solver.Provider
// "unknown package" contract (ok=false / nil slice, both with a nil error),
// and that a full Solve requiring it produces a *solver.ConflictError (not
// an abort) whose proof mentions the package has no published versions.
func TestProviderUnknownPackageIsNotAnError(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	p := newTestMetadataProvider(t, srv)

	v, ok, err := p.Highest("acme.ghost")
	if err != nil || ok {
		t.Fatalf("Highest = (%v, %v, %v), want (_, false, nil)", v, ok, err)
	}
	versions, err := p.Universe("acme.ghost")
	if err != nil || versions != nil {
		t.Fatalf("Universe = (%v, %v), want (nil, nil)", versions, err)
	}

	reqs := []solver.Requirement{{Package: "acme.ghost", Constraint: "^1.0.0"}}
	_, solveErr := solver.Solve(reqs, p)
	var conflictErr *solver.ConflictError
	if !errors.As(solveErr, &conflictErr) {
		t.Fatalf("Solve error is not a *solver.ConflictError: %v", solveErr)
	}
	if !errors.Is(solveErr, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(err, ErrNoVersionSatisfiesConstraints) = false")
	}
	proof := conflictErr.Error()
	if !strings.Contains(proof, "acme.ghost") || !strings.Contains(proof, "no published versions") {
		t.Fatalf("proof %q does not mention acme.ghost has no published versions", proof)
	}
}

// TestNoDepsProviderReturnsEmptyDependencies pins noDepsProvider's contract
// in isolation (Dependencies always empty, never delegating), and then
// end-to-end: a full Solve wrapped in NewNoDepsProvider against a package
// that really does declare a dependency resolves only the root and never
// adds the dependency edge.
func TestNoDepsProviderReturnsEmptyDependencies(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, map[string]string{"acme.other": "^1.0.0"})
	srv.AddVersion("acme", "other", testVersion100, nil)

	inner := &countingProvider{Provider: newTestMetadataProvider(t, srv)}
	wrapped := NewNoDepsProvider(inner)

	deps, err := wrapped.Dependencies("acme.widgets", mustSolverVersion(t))
	if err != nil {
		t.Fatalf("Dependencies: unexpected error: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("Dependencies = %v, want an empty map", deps)
	}

	reqs := []solver.Requirement{{Package: "acme.widgets", Constraint: "^1.0.0"}}
	result, err := solver.Solve(reqs, wrapped)
	if err != nil {
		t.Fatalf("Solve: unexpected error: %v", err)
	}
	if len(result.Versions) != 1 || result.Versions["acme.widgets"] != testVersion100 {
		t.Fatalf("Versions = %v, want exactly {acme.widgets: 1.0.0} (no-deps must not pull in acme.other)", result.Versions)
	}
	if edges := result.Graph["acme.widgets"]; len(edges) != 0 {
		t.Fatalf("Graph[acme.widgets] = %v, want no edges under --no-deps", edges)
	}
}

// TestProviderHighestEmptyFallsBackToUniverse serves a root metadata
// document whose highest_version is present but empty (a shape fakegalaxy
// itself never produces, since it always fills highest_version once a
// version exists), and asserts Highest reports ok=false while Universe
// still succeeds against the same collection's versions list - the
// documented fallback path.
func TestProviderHighestEmptyFallsBackToUniverse(t *testing.T) {
	t.Parallel()
	srv := newEmptyHighestVersionServer(t)

	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	p := NewMetadataProvider(context.Background(), cfg, runtime, store.New(), nil)

	_, ok, err := p.Highest("acme.widgets")
	if err != nil || ok {
		t.Fatalf("Highest = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	versions, err := p.Universe("acme.widgets")
	if err != nil {
		t.Fatalf("Universe: unexpected error: %v", err)
	}
	if len(versions) != 1 || versions[0].Original() != testVersion100 {
		t.Fatalf("Universe = %v, want exactly [1.0.0]", versions)
	}
}

// newEmptyHighestVersionServer starts a minimal httptest.Server answering
// the v3 root-metadata, versions-list, and version-detail routes for
// acme/widgets by hand, with highest_version deliberately left empty - the
// one shape fakegalaxy's own harness cannot produce.
func newEmptyHighestVersionServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/collections/acme/widgets/":
			root := map[string]any{
				"versions_url":    srv.URL + "/api/v3/collections/acme/widgets/versions/",
				"highest_version": map[string]any{"href": "", "version": ""},
			}
			_ = json.NewEncoder(w).Encode(root)
		case "/api/v3/collections/acme/widgets/versions/":
			payload := map[string]any{
				"meta": map[string]any{"count": 1},
				"data": []map[string]any{{"version": testVersion100, "href": srv.URL + "/api/v3/collections/acme/widgets/versions/1.0.0/"}},
			}
			_ = json.NewEncoder(w).Encode(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestMetadataProvider builds a MetadataProvider pointed at srv.
func newTestMetadataProvider(t *testing.T, srv *fakegalaxy.Server) *MetadataProvider {
	t.Helper()
	return NewMetadataProvider(context.Background(), testConfig(srv), testRuntime(srv), store.New(), nil)
}

func testConfig(srv *fakegalaxy.Server) *config.Config {
	return &config.Config{Server: srv.URL()}
}

func testRuntime(srv *fakegalaxy.Server) *infra.Infra {
	return infra.New(noopPrinter{}, srv.Client())
}
