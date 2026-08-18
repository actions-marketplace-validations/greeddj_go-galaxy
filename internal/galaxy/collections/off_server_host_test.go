package collections

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// offHostWarnSubstring is the fixed fragment every off-server-host warning
// line contains, used by every test in this file to detect (or rule out) the
// warning without depending on its exact wording.
const offHostWarnSubstring = "differs from the configured server origin"

// offServerHostGuardCase is one table entry for
// TestWarnIfOffServerDownloadHostGuards.
type offServerHostGuardCase struct {
	name        string
	base        string
	downloadURL string
	wantWarn    bool
}

// offServerHostGuardCases builds the guard-branch table for
// warnIfOffServerDownloadHost, factored out of the test function itself so
// the test body stays short, and split in two halves by expected outcome:
// every origin mismatch that must warn, then every input that must stay
// silent - a real origin match under varying spelling, and each branch this
// function deliberately declines to judge (a blank base, an unparseable base
// or download URL, and a download URL with no host at all) so a false alarm
// never reaches CI output.
//
// Killing mutation, run: comparing lowercased Hostname() instead of
// helpers.Origin fails the row "scheme downgrade on the same host warns" (and
// the port row alongside it) with
//
//	downloadURL "http://galaxy.example.com/artifact.tar.gz" against base
//	"https://galaxy.example.com": warned=false, want true (warns=[])
//
// A second mutation, also run - dropping the dl.Hostname() == "" guard - fails
// the row "download URL without a host no warn" with
//
//	downloadURL "/local/artifact.tar.gz" against base
//	"https://galaxy.example.com": warned=true, want false (warns=[Downloading
//	/local/artifact.tar.gz from origin "://:", which differs from the
//	configured server origin "https://galaxy.example.com:443"])
//
// which is what that guard is for: a hostname-less URL yields a degenerate
// origin that matches nothing, so without it every relative download URL
// raises a false alarm.
func offServerHostGuardCases() []offServerHostGuardCase {
	return append(offServerOriginMismatchCases(), offServerOriginSilentCases()...)
}

// offServerOriginMismatchCases holds the rows that must warn: an origin
// differing in each of the three components Origin normalizes over.
func offServerOriginMismatchCases() []offServerHostGuardCase {
	return []offServerHostGuardCase{
		{
			name:        "differing host warns",
			base:        "https://galaxy.example.com",
			downloadURL: "https://cdn.other.example/artifact.tar.gz",
			wantWarn:    true,
		},
		{
			// The same host reached over a different scheme and port is a
			// different origin, and origin is what decides whether the request
			// carries the operator's token and TLS policy at all - so this
			// warns, where a hostname-only comparison stayed silent.
			name:        "same host different scheme and port warns",
			base:        "https://galaxy.example.com:443",
			downloadURL: "http://galaxy.example.com:8080/artifact.tar.gz",
			wantWarn:    true,
		},
		{
			// The narrow shape the row above generalizes, and the reason this
			// item exists: nothing about the host changes, the transport
			// silently stops being TLS, and no credential follows the request.
			name:        "scheme downgrade on the same host warns",
			base:        "https://galaxy.example.com",
			downloadURL: "http://galaxy.example.com/artifact.tar.gz",
			wantWarn:    true,
		},
	}
}

// offServerOriginSilentCases holds the rows that must stay silent: a genuine
// origin match under varying spelling, and every guard branch that declines to
// judge the comparison at all.
func offServerOriginSilentCases() []offServerHostGuardCase {
	return []offServerHostGuardCase{
		{
			name:        "same host no warn",
			base:        "https://galaxy.example.com",
			downloadURL: "https://galaxy.example.com/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			// Origin fills in the scheme's default port, so an explicit :443
			// against an implicit one is the same endpoint. This is also the
			// table's proof that it can still fall silent under the stricter
			// comparison, rather than warning about everything.
			name:        "same origin with implicit default port no warn",
			base:        "https://galaxy.example.com",
			downloadURL: "https://galaxy.example.com:443/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			name:        "origin comparison is case-insensitive",
			base:        "https://Galaxy.Example.COM",
			downloadURL: "https://galaxy.example.com/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			name:        "blank base no warn",
			base:        "   ",
			downloadURL: "https://cdn.other.example/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			// A raw control character makes url.Parse fail outright.
			name:        "unparseable base no warn",
			base:        "http://exa\x7fmple.com",
			downloadURL: "https://cdn.other.example/artifact.tar.gz",
			wantWarn:    false,
		},
		{
			name:        "unparseable download URL no warn",
			base:        "https://galaxy.example.com",
			downloadURL: "http://exa\x7fmple.com",
			wantWarn:    false,
		},
		{
			// A download URL with no host at all (e.g. a bare local path) has
			// an empty Hostname(), which is guarded against explicitly rather
			// than treated as a mismatch.
			name:        "download URL without a host no warn",
			base:        "https://galaxy.example.com",
			downloadURL: "/local/artifact.tar.gz",
			wantWarn:    false,
		},
	}
}

// TestWarnIfOffServerDownloadHostGuards drives warnIfOffServerDownloadHost
// directly for every guard branch listed in offServerHostGuardCases.
func TestWarnIfOffServerDownloadHostGuards(t *testing.T) {
	t.Parallel()

	for _, tt := range offServerHostGuardCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			printer := &capturingPrinter{}
			runtime := infra.New(printer, http.DefaultClient)

			warnIfOffServerDownloadHost(runtime, tt.base, tt.downloadURL)

			// The warning goes out through Warnf, not Printf, so it survives
			// --quiet; assert against that channel specifically.
			if got := printer.hasWarnContaining(offHostWarnSubstring); got != tt.wantWarn {
				t.Fatalf("downloadURL %q against base %q: warned=%v, want %v (warns=%v)",
					tt.downloadURL, tt.base, got, tt.wantWarn, printer.warns)
			}
		})
	}
}

// TestOffServerDownloadHostWarningCutsPresignedQuery proves the warning names
// where the artifact is coming from without handing that capability to
// everyone who can read the build log: a presigned download URL's query string
// is a bearer token for the artifact, and this line goes to stderr in every
// mode, quiet included.
//
// The path assertion is the positive half and is what keeps the cut honest: a
// warning that had dropped the URL altogether would satisfy the query check
// and would no longer say which download was off-server.
//
// Killing mutation, run: restoring the bare downloadURL in
// warnIfOffServerDownloadHost's Warnf fails this with
//
//	off_server_host_test.go:213: warning line carries the presigned query:
//	[Downloading https://cdn.other.example/artifact.tar.gz
//	?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeefcafe&X-Amz-Expires=600
//	from origin "https://cdn.other.example:443", which differs from the
//	configured server origin "https://galaxy.example.com:443"]
func TestOffServerDownloadHostWarningCutsPresignedQuery(t *testing.T) {
	t.Parallel()

	const base = "https://galaxy.example.com"
	const artifact = "https://cdn.other.example/artifact.tar.gz"
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	warnIfOffServerDownloadHost(runtime, base, artifact+presignedQuery)

	if !printer.hasWarnContaining(offHostWarnSubstring) {
		t.Fatalf("expected an off-server-host warning, got %v", printer.warns)
	}
	if printer.hasWarnContaining("X-Amz-Signature") {
		t.Errorf("warning line carries the presigned query: %v", printer.warns)
	}
	if !printer.hasWarnContaining(artifact) {
		t.Errorf("warning line does not name the download URL it is about: %v", printer.warns)
	}
}

// TestDownloadCollectionPrintsNoCapability drives the two halves of the rule
// in one fixture, because they are only meaningful together: the request must
// carry the presigned query whole - it is what the object store authenticates
// the GET by - while the line announcing that request must not.
//
// The recorded query is the positive control, and a strong one: it fails if a
// future cut is applied to the URL the request is built from rather than to
// the one that is printed, which is exactly the shape that would silently
// break every presigned download while leaving this test's other half green.
//
// Two killing mutations, both run. Restoring the bare collectionURL in
// downloadCollection's Printf fails
//
//	off_server_host_test.go:272: printed line carries the presigned query:
//	[🌐 Downloading http://127.0.0.1:57653/artifact.tar.gz
//	?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeefcafe&X-Amz-Expires=600]
//
// - the port is the httptest server's own and differs on every run - and
// cutting the URL handed to http.NewRequestWithContext instead fails
//
//	off_server_host_test.go:269: server saw query "", want the whole presigned
//	query "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeefcafe&X-Amz-Expires=600"
func TestDownloadCollectionPrintsNoCapability(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenQuery = r.URL.RawQuery
		mu.Unlock()
		_, _ = w.Write([]byte("artifact bytes"))
	}))
	t.Cleanup(srv.Close)

	printer := &capturingPrinter{}
	runtime := infra.New(printer, srv.Client())
	artifact := srv.URL + "/artifact.tar.gz"

	resp, err := downloadCollection(context.Background(), runtime, artifact+presignedQuery)
	if err != nil {
		t.Fatalf("downloadCollection: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	mu.Lock()
	got := seenQuery
	mu.Unlock()
	if want := strings.TrimPrefix(presignedQuery, "?"); got != want {
		t.Errorf("server saw query %q, want the whole presigned query %q", got, want)
	}
	if printer.hasPrintContaining("X-Amz-Signature") {
		t.Errorf("printed line carries the presigned query: %v", printer.prints)
	}
	if !printer.hasPrintContaining(artifact) {
		t.Errorf("printed line does not name the artifact being downloaded: %v", printer.prints)
	}
}

// TestDownloadCollectionErrorNamesNoCapability covers the third site in this
// function that renders a server-supplied URL, alongside the announcement
// TestDownloadCollectionPrintsNoCapability covers: the error a non-200
// answer produces. It is the render most likely to be pasted somewhere
// public, since it is the one an operator sees when a download fails.
//
// The positive half is asserted on the same fixture rather than trusted: the
// error must still name the host and path, so "carries no capability" is not
// satisfied by an error that names nothing. The status is asserted too,
// since it is the other half of what makes the message actionable.
//
// Killing mutation, run: restoring the bare collectionURL in that fmt.Errorf
// fails at off_server_host_test.go:320 with the query reproduced whole - the
// assertion pins precisely "carries the presigned query", so the query is the
// one part of this quote that must not be elided:
//
//	error text carries the presigned query: download failed:
//	http://127.0.0.1:52265/artifact.tar.gz?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeefcafe&X-Amz-Expires=600
//	(403 Forbidden)
//
// The port is the httptest server's own and differs on every run.
func TestDownloadCollectionErrorNamesNoCapability(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	runtime := infra.New(&capturingPrinter{}, srv.Client())
	artifact := srv.URL + "/artifact.tar.gz"

	resp, err := downloadCollection(context.Background(), runtime, artifact+presignedQuery)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("downloadCollection() error = nil, want a non-200 failure")
	}
	msg := err.Error()
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("error text carries the presigned query: %s", msg)
	}
	if !strings.Contains(msg, artifact) {
		t.Errorf("error text does not name the artifact: %s", msg)
	}
	if !strings.Contains(msg, "403") {
		t.Errorf("error text does not name the status: %s", msg)
	}
}

// unreachableArtifactHost is a loopback address with a port nothing can be
// listening on: binding port 1 needs privileges no test run has, so the dial
// is refused immediately and deterministically, with no ephemeral-port race
// against another process and no packet leaving the host.
const unreachableArtifactHost = "https://127.0.0.1:1"

// TestDownloadCollectionTransportErrorNamesNoCapability covers the remaining
// render in downloadCollection, alongside the announcement and the non-200
// error the two tests above cover: the error a request that never reached a
// server produces. A refused dial, a DNS failure and a TLS handshake error all
// land there, none of them needing a server to answer anything - a wider set
// of occasions than the non-200 arm just below it in that function has.
//
// There is no password assertion, deliberately. net/http composes the
// *url.Error through its own stripPassword, so the password is already "***"
// before this code sees it and an assertion on it would pass with the cut
// removed - documentary, not pinned. What net/http leaves whole is everything
// else: the username, and the entire presigned query, which on an artifact URL
// IS the capability. Those two are what the negative checks below pin.
//
// The positive checks are what keep the negative ones honest, and each is
// reachable on its own. The message must still name the host and path, and
// must still carry the transport cause - compared against the unwrapped
// *url.Error's own inner error rather than a hardcoded "connection refused",
// so it asserts the cause survived rather than restating one platform's
// wording. errors.As must still reach that *url.Error, and downloadRetryable
// must still answer true, which is the classification this must not move.
//
// Two killing mutations, both run. Restoring the bare err in
// downloadCollection's transport arm fails the two negative checks and leaves
// every positive one green; the first failure reads:
//
//	off_server_host_test.go:398: transport error text carries the presigned query:
//	Get "https://u:***@127.0.0.1:1/artifact.tar.gz?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeefcafe&X-Amz-Expires=600":
//	dial tcp 127.0.0.1:1: connect: connection refused
//
// The second labels that same value as carrying the userinfo prefix. Deleting
// helpers.TransportURLError's Unwrap method fails the reachability check
// instead, with the rendering left correct:
//
//	off_server_host_test.go:392: the transport failure is no longer reachable as
//	*url.Error, which is what downloadRetryable and exitcode classify through:
//	Get "https://127.0.0.1:1/artifact.tar.gz": dial tcp 127.0.0.1:1: connect: connection refused
func TestDownloadCollectionTransportErrorNamesNoCapability(t *testing.T) {
	t.Parallel()

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	artifact := unreachableArtifactHost + "/artifact.tar.gz"
	// #nosec G101 -- test fixture literal, not a real credential
	const userinfo = "u:s3cr3t@"
	requested := strings.Replace(artifact, "https://", "https://"+userinfo, 1) + presignedQuery

	resp, err := downloadCollection(context.Background(), runtime, requested)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("downloadCollection(%q) error = nil, want a transport failure", requested)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("the transport failure is no longer reachable as *url.Error, which is what "+
			"downloadRetryable and exitcode classify through: %v", err)
	}

	msg := err.Error()
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("transport error text carries the presigned query: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("transport error text carries the userinfo prefix %q: %s", "u:", msg)
	}
	// Host and path without the scheme, so this stays green under the mutation
	// below - a real control rather than a fourth assertion the same mutation
	// happens to kill. The uncut message names them too, just behind "u:***@".
	if !strings.Contains(msg, "127.0.0.1:1/artifact.tar.gz") {
		t.Errorf("transport error text does not name the artifact: %s", msg)
	}
	if !strings.Contains(msg, urlErr.Err.Error()) {
		t.Errorf("transport error text does not carry the transport cause %q: %s", urlErr.Err, msg)
	}
	if !downloadRetryable(err) {
		t.Errorf("downloadRetryable(transport failure) = false, want the transport arm unchanged: %v", err)
	}
}

// newOffHostTestServer starts an httptest server that always serves content
// (a minimal valid tar.gz built by buildMinimalTarGz), and returns it
// alongside content's sha256, ready to be wired as an artifact's DownloadURL.
func newOffHostTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	content := buildMinimalTarGz(t)
	sha := sha256Hex(content)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)
	return server, sha
}

// runOffHostInstall installs col against server's artifact using a fresh
// capturingPrinter-backed runtime and cfg.Server as given, returning the
// printer (to inspect for a warning) and the resulting install path.
func runOffHostInstall(t *testing.T, cfgServer string, server *httptest.Server, sha string) (*capturingPrinter, string) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	// Source is set explicitly to cfgServer here, mirroring what
	// solverResultToResolvedGraph always stamps onto a real resolved
	// collection: warnIfOffServerDownloadHost now compares against the
	// collection's own bound server, not a package-wide cfg.Server.
	col := collection{Namespace: "acme", Name: "offhost", Version: "1.0.0", Source: cfgServer}
	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = sha

	cfg := &config.Config{
		Server:       cfgServer,
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		// NoDeps set purely for symmetry with the other fixtures in this
		// package that reuse buildMinimalTarGz, whose artifact carries no
		// dependencies.
		NoDeps: true,
	}

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      local.NewArtifacts(cfg.CacheDir),
		root:           newTestCollectionsRoot(t, downloadPath),
	}

	if err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{}); err != nil {
		t.Fatalf("installCollection: %v", err)
	}
	return printer, filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
}

// TestOffServerDownloadHostWarns proves the warning fires, and the install
// still succeeds, when an artifact's download URL resolves to a different
// host than the configured Galaxy server - the shape a poisoned or
// off-server-redirected cached download_url would take. cfg.Server is never
// dialed here: meta is passed in directly as an override, exactly like a
// value served from a cached snapshot, so this proves the warning covers
// that path and not just a freshly fetched one. The assertion targets the
// Warnf channel specifically: this is a security/integrity signal that must
// survive --quiet, unlike the transient Printf tier.
func TestOffServerDownloadHostWarns(t *testing.T) {
	t.Parallel()
	server, sha := newOffHostTestServer(t)

	printer, installPath := runOffHostInstall(t, "https://galaxy.example.invalid", server, sha)

	if !printer.hasWarnContaining(offHostWarnSubstring) {
		t.Fatalf("expected an off-server-host warning to be recorded via Warnf, got %v", printer.warns)
	}
	// Warnf prefixes its own marker at render time, so the format string
	// itself must not also carry the emoji - that would double it.
	if printer.hasWarnContaining("⚠️") {
		t.Fatalf("expected no emoji marker in the Warnf-recorded line, got %v", printer.warns)
	}
	if _, statErr := os.Stat(installPath); statErr != nil {
		t.Fatalf("expected the collection to be installed despite the host-mismatch warning, stat error: %v", statErr)
	}
}

// TestSameHostDownloadNoWarn proves the ordinary case - the download URL's
// host matches the configured server - never emits the off-host warning,
// alongside a successful install.
func TestSameHostDownloadNoWarn(t *testing.T) {
	t.Parallel()
	server, sha := newOffHostTestServer(t)

	printer, installPath := runOffHostInstall(t, server.URL, server, sha)

	if printer.hasWarnContaining(offHostWarnSubstring) {
		t.Fatalf("expected no off-server-host warning for a same-host download, got %v", printer.warns)
	}
	if _, statErr := os.Stat(installPath); statErr != nil {
		t.Fatalf("expected install path %s to exist, stat error: %v", installPath, statErr)
	}
}
