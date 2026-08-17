package collections

// This file is what verify_command_test.go could not be: proof that a VALID
// signature installs a collection through a real command, all the way down to
// the archive's own chain. verify_command_test.go's own refusing rows
// deliberately use a source that cannot verify, so a failed verdict never
// reaches the manifest chain walk - which was, until now, the only choice
// available to it: the artifacts fakegalaxy generated carried no FILES.json
// for a chain walk to succeed against. Now that fakegalaxy.Server.AddVersion
// bundles one, and a caller can sign fakegalaxy.Server.ManifestJSON's own
// return value and hand SignVersion that blob to attach, every row below
// drives a real collections.Start or collections.Warm run against a fake
// server, and every row whose own signature actually verifies walks the chain
// it names all the way to the one file it lists. The rest of this file's rows
// are the other half of the claim: a signature refused for a stated reason -
// signed over bytes the artifact does not carry, unverifiable outright, or
// never offered at all - still drives the identical real command against the
// identical fake server, so a refusal proven here is a refusal of what was
// actually checked, not of a fixture that could never have accepted anything.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// writeSignedRequirements writes a requirements.yml under dir naming exactly
// one collection, acme.app@*, with sources appended under its own
// signatures: block when non-empty, and returns its path.
func writeSignedRequirements(t *testing.T, dir string, sources []string) string {
	t.Helper()
	path := filepath.Join(dir, "requirements.yml")
	var body strings.Builder
	body.WriteString("collections:\n  - name: acme.app\n    version: \"*\"\n")
	if len(sources) > 0 {
		body.WriteString("    signatures:\n")
		for _, source := range sources {
			body.WriteString("      - " + source + "\n")
		}
	}
	mustWriteFile(t, path, []byte(body.String()))
	return path
}

// signedRequiredCount is the required-valid-signature-count spec every row
// in this file drives, the strict spelling of the default: a vacuous pass
// (nothing offered) fails closed rather than warning and proceeding, which
// is what makes N3's own killing mutation (see
// TestInstallVerifiesATransitiveServerSignedDependency) an observable
// failure rather than a silent one.
const signedRequiredCount = "+1"

// newSignedFixture builds a *config.Config and *infra.Infra wired to srv,
// through a fresh requirements.yml naming acme.app@* with sources under its
// own signatures: block, and a keyring configured under signedRequiredCount.
// It returns cfg, runtime, and cfg.DownloadPath, mirroring
// newVerifyCommandFixture's own shape (verify_command_test.go) closely
// enough to share a calling convention. It is a second builder rather than a
// parameter added to newVerifySetupFixture (verify_command_test.go) because
// that one already takes its own sources []string but constructs and hides
// its own server internally, returning only cfg and runtime - no caller can
// reach a *fakegalaxy.Server to call SignVersion on, which every row in this
// file needs. It also runs under signedRequiredCount ("+1") rather than that
// fixture's "1", the strict spelling every row here needs.
func newSignedFixture(
	t *testing.T, srv *fakegalaxy.Server, sources []string, workers int,
) (*config.Config, *infra.Infra, string) {
	t.Helper()
	root := t.TempDir()
	reqPath := writeSignedRequirements(t, root, sources)
	downloadPath := filepath.Join(root, "install")

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          workers,
		DownloadWorkers:  workers,
		Timeout:          verifyCommandTimeout,
		Signature: config.SignatureConfig{
			KeyringPath:   writeTestKeyring(t),
			RequiredCount: signedRequiredCount,
		},
	}
	return cfg, infra.New(&capturingPrinter{}, srv.Client()), downloadPath
}

// TestInstallVerifiesAServerSignedCollectionEndToEnd is N1: a real
// collections.Start run installs a collection whose only signature is the
// one a Galaxy server carries in its own version metadata, verified under
// the strict required-count spelling and walked all the way through the
// archive's chain - MANIFEST.json, the FILES.json it names, and the one
// file that listing vouches for.
//
// The second row is the control on the identical fixture: signing bytes the
// artifact does not carry must still fail closed, with no install directory
// left behind, which is what shows the first row's success is the signature
// actually verifying rather than this fixture installing regardless of what
// SignVersion is handed.
func TestInstallVerifiesAServerSignedCollectionEndToEnd(t *testing.T) {
	t.Parallel()

	t.Run("a server-carried signature that verifies installs cleanly", func(t *testing.T) {
		t.Parallel()
		srv := fakegalaxy.New(t)
		srv.AddVersion("acme", "app", "1.0.0", nil)
		srv.SignVersion(t, "acme", "app", "1.0.0", signTestBytes(t, srv.ManifestJSON("acme", "app", "1.0.0")))

		cfg, runtime, downloadPath := newSignedFixture(t, srv, nil, 2)

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		installDir := filepath.Join(downloadPath, "ansible_collections", "acme", "app")
		assertExists(t, filepath.Join(installDir, "MANIFEST.json"))
		assertExists(t, filepath.Join(installDir, "FILES.json"))

		printer := capturedOutput(t, runtime)
		if printer.hasWarnContaining("Nothing verified") {
			t.Fatalf("a verified collection must not warn about a vacuous pass: %v", printer.warns)
		}
	})

	t.Run("control: a signature over bytes the artifact does not carry fails closed", func(t *testing.T) {
		t.Parallel()
		srv := fakegalaxy.New(t)
		srv.AddVersion("acme", "app", "1.0.0", nil)
		srv.SignVersion(t, "acme", "app", "1.0.0", signTestBytes(t, []byte("a document this artifact does not carry")))

		cfg, runtime, downloadPath := newSignedFixture(t, srv, nil, 2)

		err := Start(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
		}
		if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "acme", "app")); statErr == nil {
			t.Fatal("the refused collection's install directory exists")
		}
	})
}

// TestWarmVerifiesAServerSignedCollectionAcrossCachedRuns is N2: a second
// collections.Warm run, against a cache the first run already populated,
// still verifies the collection's server-carried signature - and does so
// without a single request to the version-detail endpoint, since the
// document that signature rides on is served from the persisted API cache
// this run reloads from disk rather than from the fake server.
//
// The claim this pins is that the server's own signature survives two
// separate round trips: the persisted API cache entry (an exact-version
// document, cached with no TTL once fetched), and the snapshot's own
// bucket-mapped Bolt round trip - this fixture never sets cfg.S3Cache, so
// it exercises the local backend, whose Store methods write straight into
// Bolt buckets rather than the S3 backend's gzipped-JSON blob. Warm opens a
// fresh backend and loads a fresh Store from disk on every call, so calling
// it twice with the same cfg is what exercises both round trips, rather
// than replaying anything held in memory.
//
// KILLING MUTATION, run and reverted via go test -overlay against
// install.go's servableFromCacheAlone: the `&& !deps.verify.enabled()`
// conjunct deleted from its return statement, so a cache hit is served with
// no metadata round trip even while this run verifies - a plausible "the
// fast path already checks enough" edit. Measured: the first Warm() stays
// green and the version-detail count stays 0 through it, so every
// assertion above the one this pins is satisfied by the mutated state; the
// second Warm() call itself is what fails:
//
//	verify_signed_e2e_test.go:224: Warm() (second run) = installation failed: warm failed for 1 collections, want nil
//
// with the per-collection cause folded under that headline and recorded by
// the run's own Output.Errorf - this fixture's capturingPrinter, not this
// process's stderr:
//
//	Failed: acme.app error: acme.app@1.0.0: collection signature verification failed: no valid signature
//
// exit code 10 (exitcode.ExitSignature). Skipping the metadata fetch skips
// the one document the server's own signature rides on, so a Warm run that
// never re-fetches it also never re-gathers it - the failure this row
// exists to catch. A broader mutation - rewriting gatherOne to always
// return sources[i] regardless of i - never kills this test: taken
// literally it panics on an out-of-range index and fails one other
// test, chosen nondeterministically between runs; taken index-safe it
// is inert here outright, since this row's own sources list is empty.
//
// KILLING MUTATION, run and reverted via go test -overlay against
// internal/galaxy/cache/api_cache.go's isValidCacheEntry: its body replaced
// with an unconditional `return false`, forcing every cache read this run
// makes to look like a miss and therefore forcing a revalidating fetch. The
// second Warm() call still succeeds - a cache miss just refetches - so it
// is the request-count assertion right after it that fails instead, with
// nothing above it disturbed:
//
//	verify_signed_e2e_test.go:227: second Warm() reached the version-detail endpoint 1 times, want 0: the server's own
//	signature must be served from the persisted cache, not fetched again
func TestWarmVerifiesAServerSignedCollectionAcrossCachedRuns(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	srv.SignVersion(t, "acme", "app", "1.0.0", signTestBytes(t, srv.ManifestJSON("acme", "app", "1.0.0")))

	cfg, runtime, _ := newSignedFixture(t, srv, nil, 2)

	if err := Warm(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Warm() (first run) = %v, want nil", err)
	}

	srv.ResetCounts()
	runtime.Output = &capturingPrinter{}
	if err := Warm(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Warm() (second run) = %v, want nil", err)
	}
	if got := srv.Count(fakegalaxy.EndpointVersionDetail); got != 0 {
		t.Fatalf("second Warm() reached the version-detail endpoint %d times, want 0: the server's own "+
			"signature must be served from the persisted cache, not fetched again", got)
	}
	printer := capturedOutput(t, runtime)
	if printer.hasWarnContaining("Nothing verified") {
		t.Fatalf("a verified collection must not warn about a vacuous pass: %v", printer.warns)
	}
}

// TestInstallVerifiesATransitiveServerSignedDependency is N3: a real
// collections.Start run resolves acme.app@1.0.0's dependency on
// acme.lib@1.0.0, and verifies BOTH collections' server-carried signatures -
// the dependency's included, even though it is never named in
// requirements.yml and is installed only because acme.app declares it.
//
// KILLING MUTATION, run and reverted via go test -overlay against
// install.go's prepareInstall: its
//
//	return payloadFromPrefetched(col, metaOverride, prefetched)
//
// line changed to pass nil in place of metaOverride. Both collections are
// prefetched ahead of their install levels here, so both lose their version
// metadata - and with it, the only signature either one has (a server's own,
// carried on that document) - under the mutation. acme.lib is a dependency of
// acme.app, so it installs at the earlier level; installLevels breaks before
// starting the next level on any failure within one, so acme.app's own
// install is never attempted and never separately recorded as failed. The
// run fails with:
//
//	Start() = installation failed for 1 collections, want nil
//
// exit code 10, with the per-collection cause the summary's own one-line
// headline omits (see summaryError's own doc comment for why) recorded by
// the run's own Output.Errorf - this fixture's capturingPrinter, not this
// process's stderr:
//
//	Failed: acme.lib error: acme.lib@1.0.0: collection signature verification failed: no valid signature
//
// "+1" rather than the default "1" is what makes this mutation observable at
// all: under "1", the same mutation's zero-signature outcome is a vacuous
// pass - installed successfully, with a warning nothing in this row asserts
// on - which is a SILENT vacuous pass as far as this test's own assertions
// are concerned, not a caught regression.
func TestInstallVerifiesATransitiveServerSignedDependency(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "lib", "1.0.0", nil)
	srv.SignVersion(t, "acme", "lib", "1.0.0", signTestBytes(t, srv.ManifestJSON("acme", "lib", "1.0.0")))
	srv.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": "*"})
	srv.SignVersion(t, "acme", "app", "1.0.0", signTestBytes(t, srv.ManifestJSON("acme", "app", "1.0.0")))

	cfg, runtime, downloadPath := newSignedFixture(t, srv, nil, 4)

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	assertExists(t, filepath.Join(downloadPath, "ansible_collections", "acme", "app", "MANIFEST.json"))
	assertExists(t, filepath.Join(downloadPath, "ansible_collections", "acme", "lib", "MANIFEST.json"))
}

// TestInstallVerifiesAFileSignatureSourceOffline is N4: a real
// collections.Start run, under cfg.Offline, verifies a requirements-declared
// file:// signature source and installs the collection.
//
// What cfg.Offline actually exercises here is narrower than its name
// suggests, and that is stated rather than hidden: this package's fixtures
// inject srv.Client() directly, not the production fetch.NewOffline
// transport that refuses every request outright, so cfg.Offline is read by
// ordinary production code here exactly as it would be on any other offline
// row in this package - nothing about that reach is unique to this test.
// The cache is primed below first because fetchArtifact's own cache-miss
// guard refuses under cfg.Offline before verification is ever reached. What
// this row actually proves is that a file:// source still verifies under
// that construction, because Fetcher.fetchFile reads local disk regardless
// of cfg.Offline by design - not that the network transport itself was
// blocked.
func TestInstallVerifiesAFileSignatureSourceOffline(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	sigPath := filepath.Join(t.TempDir(), "collection.asc")
	mustWriteFile(t, sigPath, signTestBytes(t, srv.ManifestJSON("acme", "app", "1.0.0")))

	cfg, runtime, downloadPath := newSignedFixture(t, srv, []string{"file://" + sigPath}, 2)

	// Prime the cache online, with verification off: fetchArtifact refuses a
	// cache miss outright under cfg.Offline, before verification is ever
	// reached, so the offline run below needs both the version metadata and
	// the artifact already cached.
	cfg.Signature.DisableGPGVerify = true
	if err := Warm(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("priming Warm() = %v, want nil", err)
	}
	cfg.Signature.DisableGPGVerify = false
	cfg.Offline = true

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	assertExists(t, filepath.Join(downloadPath, "ansible_collections", "acme", "app", "MANIFEST.json"))
}

// TestInstallRefusesWithNoSignatureAnywhere is N5: a real collections.Start
// run, with a keyring configured and the strict required-count spelling, but
// no signature offered from either a requirement source or the server's own
// metadata, refuses the collection and installs nothing.
func TestInstallRefusesWithNoSignatureAnywhere(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg, runtime, downloadPath := newSignedFixture(t, srv, nil, 2)

	err := Start(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitSignature {
		t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
	}
	if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "acme", "app")); statErr == nil {
		t.Fatal("the refused collection's install directory exists")
	}
}

// TestWarmLeavesTheExtractedTreeAfterARefusedSignature is N6: a real
// collections.Warm run refuses a collection whose declared signature source
// cannot be verified, and exits through the signature exit class - but the
// content-addressable extracted tree for that artifact's sha is still
// present on disk afterward.
//
// This is the residual verifyCollectionSignatures' own doc comment
// discloses by name: on a fresh download with an extracted store configured
// (which warm always carries), streamDownloadAndExtract promotes the
// content-addressable tree during the download itself, before
// warmVerifyAndEnsure ever calls verifyCollectionSignatures - so a refused
// collection still leaves that tree behind. This row exists so a future
// change that closes the residual is noticed rather than silently assumed;
// it pins today's actual behavior, not a claim that leaving the tree behind
// is the right outcome. The sha is read from AddVersion's own return value
// rather than hardcoded, since it names bytes this test built and could
// change if the fixture ever does.
func TestWarmLeavesTheExtractedTreeAfterARefusedSignature(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	v := srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg, runtime, _ := newSignedFixture(t, srv, []string{writeUnverifiableSignature(t)}, 2)

	err := Warm(context.Background(), cfg, runtime)
	if got := exitcode.FromError(err); got != exitcode.ExitSignature {
		t.Fatalf("exit code = %d, want %d (err = %v)", got, exitcode.ExitSignature, err)
	}

	extractedStore := extracted.NewStore(cfg.CacheDir)
	if !extractedStore.Ready(v.SHA256) {
		t.Fatalf("extracted tree for sha %s is not present, want it left behind by the download that ran "+
			"ahead of the refused verification", v.SHA256)
	}
}
