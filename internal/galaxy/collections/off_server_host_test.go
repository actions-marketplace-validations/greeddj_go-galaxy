package collections

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
