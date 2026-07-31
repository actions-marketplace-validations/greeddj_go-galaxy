package collections_test

// This file drives the ordered-server-fallback design end-to-end: two
// independent fakegalaxy servers, exercising first-match ownership,
// fail-closed classification (404 advances, 401/403/exhausted-5xx aborts),
// source: pinning by id/origin/anonymous-mismatch, transitive dependency
// server-list walking, a Galaxy NG / Automation Hub shaped server mixed into
// the list, per-origin credential isolation, and resolution determinism
// across differing worker counts. See e2e_test.go for this package's own
// doc comment.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// msReqSpec is one requirements.yml entry for buildMultiServerRequirements:
// a bare "name: *" entry, plus an optional source: line when source is set.
type msReqSpec struct {
	name   string
	source string
}

// buildMultiServerRequirements renders entries into a requirements.yml body,
// each at the wildcard version constraint (this file never cares about
// version selection, only which server answers).
func buildMultiServerRequirements(entries []msReqSpec) string {
	var b strings.Builder
	b.WriteString("collections:\n")
	for _, e := range entries {
		b.WriteString("  - name: " + e.name + "\n    version: \"*\"\n")
		if e.source != "" {
			b.WriteString("    source: " + e.source + "\n")
		}
	}
	return b.String()
}

// newMultiServerConfig builds a *config.Config wired with servers and a
// requirements.yml rendered from requirementsYAML, rooted under a fresh
// t.TempDir - so two calls with the same servers/requirements still get
// independent cache and download directories.
func newMultiServerConfig(t *testing.T, servers []config.Server, requirementsYAML string) *config.Config {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte(requirementsYAML), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
	server := ""
	if len(servers) > 0 {
		server = servers[0].URL
	}
	return &config.Config{
		Servers:          servers,
		Server:           server,
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}
}

// multiServerRuntime builds an *infra.Infra whose HTTP client is the real
// production fetch.New transport, wired from cfg.Servers exactly like
// cmd/go-galaxy/commands.newHTTPClient does - so per-origin token attachment
// and TLS dispatch are exercised honestly rather than through a single
// unconditional client.
func multiServerRuntime(cfg *config.Config) *infra.Infra {
	auths := make([]fetch.ServerAuth, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		u, err := url.Parse(s.URL)
		if err != nil {
			continue
		}
		auths = append(auths, fetch.ServerAuth{
			Origin:      helpers.Origin(u),
			Token:       s.Token.Reveal(),
			InsecureTLS: s.InsecureSkipTLSVerify,
		})
	}
	return infra.New(noopPrinter{}, fetch.New(cfg.Timeout, auths))
}

// msAssertInstalled fails the test unless ns.name's MANIFEST.json exists
// under cfg.DownloadPath. Every collection in this file lives under the
// fixed "ns" namespace, so it is hardcoded here rather than threaded
// through as a parameter every caller would pass the same value for.
func msAssertInstalled(t *testing.T, downloadPath, name string) {
	t.Helper()
	path := filepath.Join(downloadPath, "ansible_collections", "ns", name, "MANIFEST.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected ns.%s installed, MANIFEST.json missing at %s: %v", name, path, err)
	}
}

// msArtifactSHA256 hashes the cached tarball for ns.name@version under
// cacheDir, scoped to source (the server that actually resolved it - see
// helpers.ArtifactKey), the same on-disk location the local artifact backend
// commits a cached tarball to.
func msArtifactSHA256(t *testing.T, cacheDir, source, ns, name, version string) string {
	t.Helper()
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", ns, name, version)
	key := helpers.ArtifactKey(source, filename)
	data, err := os.ReadFile(filepath.Join(cacheDir, key)) //nolint:gosec // path built from this test's own temp dir and fixture names.
	if err != nil {
		t.Fatalf("read cached artifact %s (key %s): %v", filename, key, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// msLockFile runs collections.Lock against cfg/runtime and loads the
// resulting lockfile, so a test can inspect each entry's recorded Source and
// SHA256 - the winner, flowed all the way from MetadataProvider.recordBinding
// through solverResultToResolvedGraph to buildLockfile.
func msLockFile(t *testing.T, cfg *config.Config, runtime *infra.Infra) *lockfile.File {
	t.Helper()
	if err := collections.Lock(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	return lf
}

// TestMultiServerFirstMatchOwnership drives ns.a (server A only), ns.b
// (server B only), and ns.both (both servers, with different artifact
// bytes) through a single unpinned install, and asserts each resolves from
// the server the "first match wins" rule predicts: ns.both's installed
// bytes are A's, never B's, even though B also has a version satisfying the
// same constraint.
func TestMultiServerFirstMatchOwnership(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "a", "1.0.0", nil)
	srvB.AddVersion("ns", "b", "1.0.0", nil)
	bothA := srvA.AddVersion("ns", "both", "1.0.0", nil)
	// A non-nil-but-empty deps map renders as "{}" rather than "null" in the
	// generated MANIFEST.json, giving B's copy of ns.both different artifact
	// bytes (and therefore a different sha256) than A's, without changing
	// either copy's resolved dependency set (both are still "no deps").
	srvB.AddVersion("ns", "both", "1.0.0", map[string]string{})

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{
		{name: "ns.a"}, {name: "ns.b"}, {name: "ns.both"},
	}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "a")
	msAssertInstalled(t, cfg.DownloadPath, "b")
	msAssertInstalled(t, cfg.DownloadPath, "both")

	if got := msArtifactSHA256(t, cfg.CacheDir, srvA.URL(), "ns", "both", "1.0.0"); got != bothA.SHA256 {
		t.Fatalf("installed ns.both sha = %s, want A's own sha %s (first-match ownership)", got, bothA.SHA256)
	}

	lf := msLockFile(t, cfg, runtime)
	if e := findLockEntry(t, lf, "ns.a"); e.Source != srvA.URL() {
		t.Fatalf("ns.a lockfile source = %q, want %q", e.Source, srvA.URL())
	}
	if e := findLockEntry(t, lf, "ns.b"); e.Source != srvB.URL() {
		t.Fatalf("ns.b lockfile source = %q, want %q", e.Source, srvB.URL())
	}
	entryBoth := findLockEntry(t, lf, "ns.both")
	if entryBoth.Source != srvA.URL() {
		t.Fatalf("ns.both lockfile source = %q, want %q (first match, A owns it)", entryBoth.Source, srvA.URL())
	}
	if entryBoth.SHA256 != bothA.SHA256 {
		t.Fatalf("ns.both lockfile sha = %q, want A's own sha %q", entryBoth.SHA256, bothA.SHA256)
	}
}

// TestMultiServerAdvancesOnPlain404 asserts that ns.b, absent from A but
// present on B, resolves from B: a 404 across every apiRoot candidate of A
// advances the walk to B rather than failing the run.
func TestMultiServerAdvancesOnPlain404(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "b", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.b"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "b")

	lf := msLockFile(t, cfg, runtime)
	if e := findLockEntry(t, lf, "ns.b"); e.Source != srvB.URL() {
		t.Fatalf("ns.b lockfile source = %q, want %q", e.Source, srvB.URL())
	}
}

// TestMultiServerAuthFailureAbortsClosed asserts a 401 from A - the wrong
// token configured for a server that requires one - aborts the whole run
// with helpers.ErrGalaxyAuthFailed naming A, and never falls through to B:
// srvB.Total() stays 0.
func TestMultiServerAuthFailureAbortsClosed(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.RequireAuth("Token good")
	srvA.AddVersion("ns", "x", "1.0.0", nil)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL(), Token: config.NewSecret("wrong")},
		{ID: "b", URL: srvB.URL()},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrGalaxyAuthFailed) {
		t.Fatalf("expected errors.Is ErrGalaxyAuthFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "server a:") {
		t.Fatalf("expected the error to name server %q, got %v", "a", err)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (fail closed, never falls through on auth failure)", got)
	}
}

// TestMultiServerForbiddenAbortsClosed is TestMultiServerAuthFailureAbortsClosed's
// 403 sibling: AuthFailStatus(403) must classify the same as a 401.
func TestMultiServerForbiddenAbortsClosed(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.RequireAuth("Token good")
	srvA.AuthFailStatus(http.StatusForbidden)
	srvA.AddVersion("ns", "x", "1.0.0", nil)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL(), Token: config.NewSecret("wrong")},
		{ID: "b", URL: srvB.URL()},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrGalaxyAuthFailed) {
		t.Fatalf("expected errors.Is ErrGalaxyAuthFailed, got %v", err)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (fail closed, never falls through on a 403)", got)
	}
}

// TestMultiServerUnavailableAbortsAfterRetryBudget asserts an indefinitely
// retryable 503 from A aborts with helpers.ErrGalaxyServerUnavailable naming
// A, never falls through to B, and spends exactly the retry budget (proving
// the budget is not multiplied by the server-list walk).
func TestMultiServerUnavailableAbortsAfterRetryBudget(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.Fail(fakegalaxy.EndpointRootMetadata, "", "", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrGalaxyServerUnavailable) {
		t.Fatalf("expected errors.Is ErrGalaxyServerUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "server a:") {
		t.Fatalf("expected the error to name server %q, got %v", "a", err)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (fail closed, never falls through on exhausted retries)", got)
	}
	if got := srvA.Count(fakegalaxy.EndpointRootMetadata); got != helpers.FetchRetryMaxAttempts {
		t.Fatalf("srvA root-metadata count = %d, want %d (the retry budget, not multiplied by the candidate walk)",
			got, helpers.FetchRetryMaxAttempts)
	}
}

// TestMultiServerAll404YieldsUnknownPackage asserts that when neither
// configured server has a collection at all, the run fails as an ordinary
// unknown-package resolution conflict (not a network abort), classified by
// exitcode as ExitResolution.
func TestMultiServerAll404YieldsUnknownPackage(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.ghost"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var conflictErr *solver.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected a *solver.ConflictError, got %T: %v", err, err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitResolution {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitResolution (%d)", got, exitcode.ExitResolution)
	}
}

// TestMultiServerPinnedByID asserts a source: value matching a configured
// server_list id installs from exactly that server, never consulting A.
func TestMultiServerPinnedByID(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: "b"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")
	if got := srvA.Total(); got != 0 {
		t.Fatalf("srvA.Total() = %d, want 0 (pinned by id, A is never consulted)", got)
	}
}

// TestMultiServerPinnedByOriginDifferentPath asserts a source: value naming
// a different path under a configured server's origin still adopts that
// server's id (and therefore its credential): B is a Galaxy NG / Automation
// Hub shaped server mounted at hubPath, configured by its bare origin only,
// while the pinned source names hubPath explicitly - a different path,
// same origin. B's token must still be attached, since fetch dispatches
// credentials by origin, not by the exact configured URL string.
func TestMultiServerPinnedByOriginDifferentPath(t *testing.T) {
	t.Parallel()
	const hubPath = "/content/published"
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.NewAtBasePath(t, hubPath)
	srvB.RequireAuth("Token btok")
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL()},
		{ID: "b", URL: srvB.URL(), Token: config.NewSecret("btok")},
	}
	source := srvB.URL() + hubPath
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: source}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")

	value, present := srvB.SeenAuth(fakegalaxy.EndpointRootMetadata)
	if !present || value != "Token btok" {
		t.Fatalf("SeenAuth(EndpointRootMetadata) = (%q, %v), want (\"Token btok\", true) - the origin match must still attach B's token",
			value, present)
	}
	if got := srvA.Total(); got != 0 {
		t.Fatalf("srvA.Total() = %d, want 0", got)
	}
}

// TestMultiServerPinnedMatchingNothingSendsNoAuth asserts a source: value
// matching neither configured server's id nor origin is treated as an
// anonymous, unmatched base: no Authorization header is attached, and if
// that server requires one, the run fails closed with
// helpers.ErrGalaxyAuthFailed naming the base URL - never A or B, which are
// never even consulted.
func TestMultiServerPinnedMatchingNothingSendsNoAuth(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvC := fakegalaxy.New(t) // never part of cfg.Servers
	srvC.RequireAuth("Token good")
	srvC.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL(), Token: config.NewSecret("atok")},
		{ID: "b", URL: srvB.URL(), Token: config.NewSecret("btok")},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: srvC.URL()}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrGalaxyAuthFailed) {
		t.Fatalf("expected errors.Is ErrGalaxyAuthFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), srvC.URL()) {
		t.Fatalf("expected the error to name the base URL %s, got %v", srvC.URL(), err)
	}
	if value, present := srvC.SeenAuth(fakegalaxy.EndpointRootMetadata); present || value != "" {
		t.Fatalf("SeenAuth = (%q, %v), want no Authorization header sent to an unmatched server", value, present)
	}
	if got := srvA.Total(); got != 0 {
		t.Fatalf("srvA.Total() = %d, want 0", got)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0", got)
	}
}

// TestMultiServerPinnedNeverFallsThrough asserts a collection pinned to A
// that only exists on B never falls through to B: it fails as an unknown
// package, and B is never consulted.
func TestMultiServerPinnedNeverFallsThrough(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: "a"}}))
	runtime := multiServerRuntime(cfg)

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var conflictErr *solver.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected a *solver.ConflictError (unknown package), got %T: %v", err, err)
	}
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (a pinned collection never falls through)", got)
	}
}

// TestMultiServerTransitiveDepWalksList asserts a transitive dependency of a
// pinned root is not itself pinned: ns.root is pinned to A, but its
// dependency ns.dep - absent from A, present on B - resolves from B, proving
// there is no source inheritance from parent to dependency.
func TestMultiServerTransitiveDepWalksList(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "root", "1.0.0", map[string]string{"ns.dep": ">=1.0.0"})
	srvB.AddVersion("ns", "dep", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.root", source: "a"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "root")
	msAssertInstalled(t, cfg.DownloadPath, "dep")

	lf := msLockFile(t, cfg, runtime)
	if e := findLockEntry(t, lf, "ns.dep"); e.Source != srvB.URL() {
		t.Fatalf("ns.dep lockfile source = %q, want %q (no source inheritance from the pinned root)", e.Source, srvB.URL())
	}
}

// TestMultiServerHubShapeInList mixes a Galaxy NG / Automation Hub shaped
// server (mounted at hubPath, v3 directly under it) into a two-server list
// alongside a plain galaxy.ansible.com shaped one, and asserts both
// collections hosted on the hub resolve successfully. fakegalaxy only counts
// a request that actually matches its own configured route shape (see
// fakegalaxy.NewAtBasePath), so an unmatched galaxy.ansible.com-shaped probe
// against the hub is invisible to Count even without the apiRootMemo
// optimization; what Count does prove is that each of the two collections
// costs exactly one COUNTED hit - no redundant successful re-fetch of
// either's root metadata.
func TestMultiServerHubShapeInList(t *testing.T) {
	t.Parallel()
	const hubPath = "/api/automation-hub"
	srvHub := fakegalaxy.NewAtBasePath(t, hubPath)
	srvB := fakegalaxy.New(t)
	srvHub.AddVersion("ns", "one", "1.0.0", nil)
	srvHub.AddVersion("ns", "two", "1.0.0", nil)

	servers := []config.Server{
		{ID: "hub", URL: srvHub.URL() + hubPath},
		{ID: "b", URL: srvB.URL()},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.one"}, {name: "ns.two"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "one")
	msAssertInstalled(t, cfg.DownloadPath, "two")

	if got := srvHub.Count(fakegalaxy.EndpointRootMetadata); got != 2 {
		t.Fatalf("srvHub root-metadata count = %d, want 2 (one successful hit per collection)", got)
	}
}

// TestMultiServerAuthReachesOwnServerOnly asserts that when A and B both
// require auth with different expected headers, each one's SeenAuth matches
// its own configured token and never the other's.
func TestMultiServerAuthReachesOwnServerOnly(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.RequireAuth("Token atok")
	srvB.RequireAuth("Token btok")
	srvA.AddVersion("ns", "a", "1.0.0", nil)
	srvB.AddVersion("ns", "b", "1.0.0", nil)

	servers := []config.Server{
		{ID: "a", URL: srvA.URL(), Token: config.NewSecret("atok")},
		{ID: "b", URL: srvB.URL(), Token: config.NewSecret("btok")},
	}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{
		{name: "ns.a", source: "a"}, {name: "ns.b", source: "b"},
	}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if v, ok := srvA.SeenAuth(fakegalaxy.EndpointRootMetadata); !ok || v != "Token atok" {
		t.Fatalf("srvA SeenAuth(EndpointRootMetadata) = (%q, %v), want (\"Token atok\", true)", v, ok)
	}
	if v, ok := srvB.SeenAuth(fakegalaxy.EndpointRootMetadata); !ok || v != "Token btok" {
		t.Fatalf("srvB SeenAuth(EndpointRootMetadata) = (%q, %v), want (\"Token btok\", true)", v, ok)
	}
}

// TestMultiServerResolutionDeterministicAcrossWorkerCounts resolves and
// installs ~12 collections split across two servers (some on A only, some on
// B only, several on both with different artifact bytes) twice - with
// cfg.Workers 1 and 8, against fresh cache/download directories but the same
// two servers and requirements - and asserts the resulting fqdn -> Source
// map and fqdn -> SHA256 map are byte-identical between the two runs: the
// server-list walk and first-match ownership are resolve-time decisions,
// unaffected by how many install workers happen to run concurrently.
func TestMultiServerResolutionDeterministicAcrossWorkerCounts(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	names := []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8", "c9", "c10", "c11", "c12"}
	for i, name := range names {
		switch {
		case i < 4: // A only
			srvA.AddVersion("ns", name, "1.0.0", nil)
		case i < 8: // B only
			srvB.AddVersion("ns", name, "1.0.0", nil)
		default: // both, with different artifact bytes (see TestMultiServerFirstMatchOwnership)
			srvA.AddVersion("ns", name, "1.0.0", nil)
			srvB.AddVersion("ns", name, "1.0.0", map[string]string{})
		}
	}
	entries := make([]msReqSpec, len(names))
	for i, name := range names {
		entries[i] = msReqSpec{name: "ns." + name}
	}
	requirementsYAML := buildMultiServerRequirements(entries)
	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}

	runOnce := func(t *testing.T, workers int) (map[string]string, map[string]string) {
		t.Helper()
		cfg := newMultiServerConfig(t, servers, requirementsYAML)
		cfg.Workers = workers
		runtime := multiServerRuntime(cfg)
		if err := collections.Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start (workers=%d): %v", workers, err)
		}
		lf := msLockFile(t, cfg, runtime)
		sources := make(map[string]string, len(lf.Collections))
		shas := make(map[string]string, len(lf.Collections))
		for _, e := range lf.Collections {
			sources[e.Name] = e.Source
			shas[e.Name] = e.SHA256
		}
		return sources, shas
	}

	sources1, shas1 := runOnce(t, 1)
	sources8, shas8 := runOnce(t, 8)

	if !reflect.DeepEqual(sources1, sources8) {
		t.Fatalf("Source map differs between workers=1 and workers=8:\n1: %v\n8: %v", sources1, sources8)
	}
	if !reflect.DeepEqual(shas1, shas8) {
		t.Fatalf("SHA256 map differs between workers=1 and workers=8:\n1: %v\n8: %v", shas1, shas8)
	}
}

// msReusedConfig clones cfg with the same cache, install and requirements
// paths but a different server list, so a second run reuses the first run's
// persisted snapshot if and only if the signature says it may.
func msReusedConfig(cfg *config.Config, servers []config.Server) *config.Config {
	clone := *cfg
	clone.Servers = servers
	if len(servers) > 0 {
		clone.Server = servers[0].URL
	}
	return &clone
}

// TestMultiServerSnapshotReuseIsPartitionedByServerList asserts the
// resolved-snapshot reuse signature accounts for the effective server list:
// re-running against the identical list reuses the persisted resolution and
// touches no server, while re-running against the same two servers in the
// other order re-resolves, because under first-match ownership that order
// decides which server owns each collection.
func TestMultiServerSnapshotReuseIsPartitionedByServerList(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "shared", "1.0.0", nil)
	srvB.AddVersion("ns", "shared", "1.0.0", nil)

	forward := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, forward, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("first install: %v", err)
	}

	srvA.ResetCounts()
	srvB.ResetCounts()
	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("second install with an identical server list: %v", err)
	}
	if got := srvA.Count(fakegalaxy.EndpointRootMetadata); got != 0 {
		t.Fatalf("srvA root metadata requests on reuse = %d, want 0 (the snapshot should have been reused)", got)
	}

	reversed := []config.Server{{ID: "b", URL: srvB.URL()}, {ID: "a", URL: srvA.URL()}}
	reorderedCfg := msReusedConfig(cfg, reversed)
	reorderedRuntime := multiServerRuntime(reorderedCfg)

	srvA.ResetCounts()
	srvB.ResetCounts()
	if err := collections.Start(context.Background(), reorderedCfg, reorderedRuntime); err != nil {
		t.Fatalf("third install with a reordered server list: %v", err)
	}
	if srvA.Count(fakegalaxy.EndpointRootMetadata)+srvB.Count(fakegalaxy.EndpointRootMetadata) == 0 {
		t.Fatal("expected a reordered server list to re-resolve, but no server was contacted")
	}
}

// TestMultiServerArtifactCacheKeyIsScopedPerServer is the regression guard
// for keeping the artifact-cache key scoped per server: two independent
// projects, sharing one cache dir, each pin ns.shared@1.0.0 to a different
// server carrying different artifact bytes for that same name and version.
// Because helpers.ArtifactKey folds the server into the key rather than
// using a flat, percent-encoded-filename-only key, each server's bytes land
// under its own key, so both projects install their own server's bytes and
// both cache entries coexist on disk, instead of the second project
// silently reusing the first project's cached tarball - the wrong bytes,
// with no metadata round trip able to catch the mismatch.
func TestMultiServerArtifactCacheKeyIsScopedPerServer(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	verA := srvA.AddVersion("ns", "shared", "1.0.0", nil)
	// A non-nil-but-empty deps map renders differently in the generated
	// MANIFEST.json than a nil one, giving B's copy different artifact bytes
	// (and therefore a different sha256) than A's, without changing either
	// copy's dependency set.
	verB := srvB.AddVersion("ns", "shared", "1.0.0", map[string]string{})
	if verA.SHA256 == verB.SHA256 {
		t.Fatalf("fixture bug: A and B must have distinct artifact bytes to prove no collision, both hash to %s", verA.SHA256)
	}

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfgA := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared", source: "a"}}))
	cfgB := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared", source: "b"}}))
	cfgB.CacheDir = cfgA.CacheDir // force both projects onto one shared cache dir

	runtimeA := multiServerRuntime(cfgA)
	runtimeB := multiServerRuntime(cfgB)

	if err := collections.Start(context.Background(), cfgA, runtimeA); err != nil {
		t.Fatalf("install pinned to A: %v", err)
	}
	if err := collections.Start(context.Background(), cfgB, runtimeB); err != nil {
		t.Fatalf("install pinned to B: %v", err)
	}
	msAssertInstalled(t, cfgA.DownloadPath, "shared")
	msAssertInstalled(t, cfgB.DownloadPath, "shared")

	if got := msArtifactSHA256(t, cfgA.CacheDir, srvA.URL(), "ns", "shared", "1.0.0"); got != verA.SHA256 {
		t.Fatalf("A's cached artifact sha = %s, want A's own sha %s", got, verA.SHA256)
	}
	if got := msArtifactSHA256(t, cfgB.CacheDir, srvB.URL(), "ns", "shared", "1.0.0"); got != verB.SHA256 {
		t.Fatalf("B's cached artifact sha = %s, want B's own sha %s", got, verB.SHA256)
	}
}

// TestMultiServerDepsCacheKeyIsScopedPerServer is the deps-cache sibling of
// TestMultiServerArtifactCacheKeyIsScopedPerServer: two projects sharing one
// cache dir each pin ns.shared@1.0.0 to a different server, and each
// server's copy of ns.shared declares a different dependency. Before the
// fix, both projects' resolves shared one deps-cache entry keyed only by
// "ns.shared@1.0.0" with no server component, so the second project's
// resolve would silently reuse the first project's cached dependency map
// instead of ever asking its own server. With the fix, each server's
// dependency map lands under its own scoped key, so both projects end up
// with their own server's transitive dependency installed.
func TestMultiServerDepsCacheKeyIsScopedPerServer(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "shared", "1.0.0", map[string]string{"ns.depa": "*"})
	srvA.AddVersion("ns", "depa", "1.0.0", nil)
	srvB.AddVersion("ns", "shared", "1.0.0", map[string]string{"ns.depb": "*"})
	srvB.AddVersion("ns", "depb", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfgA := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared", source: "a"}}))
	cfgB := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.shared", source: "b"}}))
	cfgB.CacheDir = cfgA.CacheDir // force both projects onto one shared cache dir (and deps cache)

	runtimeA := multiServerRuntime(cfgA)
	runtimeB := multiServerRuntime(cfgB)

	if err := collections.Start(context.Background(), cfgA, runtimeA); err != nil {
		t.Fatalf("install pinned to A: %v", err)
	}
	if err := collections.Start(context.Background(), cfgB, runtimeB); err != nil {
		t.Fatalf("install pinned to B: %v", err)
	}

	msAssertInstalled(t, cfgA.DownloadPath, "shared")
	msAssertInstalled(t, cfgA.DownloadPath, "depa")
	msAssertInstalled(t, cfgB.DownloadPath, "shared")
	msAssertInstalled(t, cfgB.DownloadPath, "depb")
}

// TestMultiServerSourceSwitchForcesReinstall proves installEntryMatches'
// server-source check end to end: a collection first installed pinned to A
// is re-run against the same cache dir and download path but pinned to B
// instead. canSkipInstall compares the recorded install's server against the
// newly resolved one, so a source change forces a reinstall against B rather
// than silently keeping A's install untouched with B never even contacted.
func TestMultiServerSourceSwitchForcesReinstall(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvA.AddVersion("ns", "x", "1.0.0", nil)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: "a"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("first install (pinned to A): %v", err)
	}
	msAssertInstalled(t, cfg.DownloadPath, "x")
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 before switching source", got)
	}

	switchedCfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x", source: "b"}}))
	switchedCfg.CacheDir = cfg.CacheDir         // reuse the snapshot recording A's install
	switchedCfg.DownloadPath = cfg.DownloadPath // reuse the on-disk install tree
	switchedRuntime := multiServerRuntime(switchedCfg)

	if err := collections.Start(context.Background(), switchedCfg, switchedRuntime); err != nil {
		t.Fatalf("second install (pinned to B): %v", err)
	}
	if got := srvB.Total(); got == 0 {
		t.Fatal("srvB.Total() = 0, want > 0 (a source switch must force a real reinstall from B, not a silent skip)")
	}
}
