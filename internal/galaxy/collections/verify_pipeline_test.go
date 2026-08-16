package collections

// This file pins how verification composes with the rest of the install
// pipeline: which failures may evict a cached artifact, what a cache hit costs
// a verifying run, where warm runs the check, and how a per-collection verdict
// reaches an exit code. The verification behavior itself - what verifies, what
// does not, and what is warned about - is verify_test.go's, whose fixtures this
// file reuses.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// signedCacheFixture is a cache already holding one signed artifact, wired so
// that a forced refetch serves the identical bytes: it is what an install worker
// sees on a cache hit, with every Delete counted.
type signedCacheFixture struct {
	deps         installDeps
	artifacts    *deleteCountingArtifacts
	meta         *types.GalaxyCollectionVersionInfo
	artifactPath string
	manifestJSON []byte
}

// newSignedCacheFixture seeds an artifact into a real local artifact store,
// alongside the sha sidecar that makes it a cache hit, and points its metadata
// at a server serving the same bytes back.
//
// The server matters only to the eviction rows: a refetch that failed to reach
// an origin would end the run with a download error and hide whether an
// eviction happened at all, which is the one thing those rows measure.
func newSignedCacheFixture(t *testing.T, breakChain bool, sources []string) *signedCacheFixture {
	t.Helper()
	tarPath, manifestJSON := buildSignedArtifact(t, breakChain)
	content := mustReadFile(t, tarPath)
	sha := sha256Hex(content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, cacheDir)

	col := testSignedCollection
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, content)
	mustWriteFile(t, artifactPath+helpers.ArtifactSHASidecarSuffix, []byte(sha))

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Signature: config.SignatureConfig{
			KeyringPath:   writeTestKeyring(t),
			RequiredCount: "1",
		},
	}
	runtime := infra.New(&capturingPrinter{}, http.DefaultClient)
	signedRoot := col
	signedRoot.Signatures = sources
	verify, err := newVerifyContext(cfg, runtime, []collection{signedRoot})
	if err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}

	artifacts := &deleteCountingArtifacts{Artifacts: local.NewArtifacts(cacheDir)}
	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = sha

	return &signedCacheFixture{
		deps: installDeps{
			collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
			artifacts:      artifacts,
			root:           newTestCollectionsRoot(t, downloadPath),
			verify:         verify,
		},
		artifacts:    artifacts,
		meta:         meta,
		artifactPath: artifactPath,
		manifestJSON: manifestJSON,
	}
}

// TestSignatureSourceFailureDoesNotEvictTheArtifact is the load-bearing proof
// for isSignatureSourceFailure: a signature source that cannot be read is not
// the cached artifact's fault, so the artifact must survive the failure
// untouched. Without that predicate, prepareWithRecovery's action arm would
// evict on this cause like on any other - a destructive Delete against shared
// cache state plus a full re-download, on every affected run, with the refetched
// bytes failing at the identical unreadable source.
//
// The second row is the positive control on the same harness: a manifest chain
// that does not match IS an artifact-side failure, so the identical fixture -
// same counting store, same seeded cache hit, same origin server - does evict
// exactly once. Without it, "zero evictions" would be indistinguishable from a
// harness whose Delete this path never reaches at all.
//
// KILLING MUTATION, run and reverted: the isSignatureSourceFailure conjunct
// deleted from prepareWithRecovery's action guard. Only the first row fails:
//
//	verify_pipeline_test.go:137: Delete calls = 1, want 0: a signature source
//	that could not be read is not the artifact's fault
func TestSignatureSourceFailureDoesNotEvictTheArtifact(t *testing.T) {
	t.Parallel()

	t.Run("an unreadable signature source evicts nothing", func(t *testing.T) {
		t.Parallel()
		absent := "file://" + filepath.Join(t.TempDir(), "absent.asc")
		fx := newSignedCacheFixture(t, false, []string{absent})

		err := installCollection(context.Background(), testSignedCollection, fx.deps, nil, fx.meta, downloadResult{})
		if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
			t.Fatalf("installCollection() = %v, want errors.Is helpers.ErrSignatureSourceUnavailable", err)
		}
		if got := fx.artifacts.deleteCalls.Load(); got != 0 {
			t.Fatalf("Delete calls = %d, want 0: a signature source that could not be read is not the artifact's fault", got)
		}
		assertExists(t, fx.artifactPath)
	})

	t.Run("a chain mismatch on the same harness evicts once", func(t *testing.T) {
		t.Parallel()
		fx := newSignedCacheFixture(t, true, nil)
		// Written after construction, which verifyContext's own contract
		// otherwise forbids: the signature has to be made over THIS fixture's
		// manifest, which does not exist until the artifact is built. It is
		// safe here and nowhere in production - this is one goroutine, before
		// any worker exists - and it is not license to write the map elsewhere.
		fx.deps.verify.sources[requirementKey(testSignedCollection)] =
			[]string{writeTestSignature(t, fx.manifestJSON)}

		err := installCollection(context.Background(), testSignedCollection, fx.deps, nil, fx.meta, downloadResult{})
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("installCollection() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
		}
		if got := fx.artifacts.deleteCalls.Load(); got != 1 {
			t.Fatalf("Delete calls = %d, want exactly 1 (one bounded evict-and-refetch)", got)
		}
	})
}

// TestCacheHitForcesMetadataWhenVerifying pins the conjunct prepareInstall's
// fast path grew: a verifying run does not serve a cache hit without version
// metadata, because a server's own signatures ride on that document and
// skipping it would silently reduce the gathered set to whatever the
// requirements file named.
//
// The second row is the positive control on the identical cache hit and the
// identical fake server: with verification off, the fast path is taken and the
// server sees no request at all, so the first row's requests are the conjunct's
// doing rather than something every cache hit pays.
//
// KILLING MUTATION, run and reverted: the `&& !deps.verify.enabled()` conjunct
// deleted from prepareInstall's fast path. Only the first row fails:
//
//	verify_pipeline_test.go:190: fake server request count = 0, want at least
//	one version-metadata request while verifying
func TestCacheHitForcesMetadataWhenVerifying(t *testing.T) {
	t.Parallel()

	t.Run("verifying forces the metadata fetch", func(t *testing.T) {
		t.Parallel()
		srv, deps, col := newCacheHitMetadataFixture(t, true)
		if _, _, err := prepareInstall(
			context.Background(), deps, col, nil, downloadResult{}, "acme-app-1.0.0.tar.gz", false); err != nil {
			t.Fatalf("prepareInstall() error = %v, want nil", err)
		}
		if got := srv.Total(); got == 0 {
			t.Fatalf("fake server request count = %d, want at least one version-metadata request while verifying", got)
		}
	})

	t.Run("not verifying serves the same hit with no request", func(t *testing.T) {
		t.Parallel()
		srv, deps, col := newCacheHitMetadataFixture(t, false)
		if _, _, err := prepareInstall(
			context.Background(), deps, col, nil, downloadResult{}, "acme-app-1.0.0.tar.gz", false); err != nil {
			t.Fatalf("prepareInstall() error = %v, want nil", err)
		}
		if got := srv.Total(); got != 0 {
			t.Fatalf("fake server request count = %d, want 0 (the fast path serves a cache hit unasked)", got)
		}
	})
}

// newCacheHitMetadataFixture seeds a cache hit for a collection the fake Galaxy
// server also publishes, and returns the server alongside deps that either
// verify or do not. The artifact's bytes are the fake server's own, so the
// verifying row's metadata fetch describes the artifact it finds cached.
func newCacheHitMetadataFixture(t *testing.T, verifying bool) (*fakegalaxy.Server, installDeps, collection) {
	t.Helper()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	mustMkdirAll(t, cacheDir)
	col := testSignedCollection
	col.Source = srv.URL()

	tarPath, _ := buildSignedArtifact(t, false)
	mustWriteFile(t, filepath.Join(cacheDir, artifactKey(col)), mustReadFile(t, tarPath))

	cfg := &config.Config{
		Server:       srv.URL(),
		Servers:      []config.Server{{URL: srv.URL()}},
		CacheDir:     cacheDir,
		DownloadPath: filepath.Join(root, "install"),
		Workers:      1,
		NoDeps:       true,
	}
	runtime := infra.New(&capturingPrinter{}, srv.Client())

	var verify *verifyContext
	if verifying {
		cfg.Signature = config.SignatureConfig{KeyringPath: writeTestKeyring(t), RequiredCount: "1"}
		built, err := newVerifyContext(cfg, runtime, []collection{col})
		if err != nil {
			t.Fatalf("newVerifyContext() error = %v, want nil", err)
		}
		verify = built
	}

	return srv, installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      local.NewArtifacts(cacheDir),
		verify:         verify,
	}, col
}

// TestWarmVerifiesSignatures pins warm's own insertion point: signatures: on a
// requirements entry is checked by warm, not only by install, and it is checked
// where warmVerifyAndEnsure runs it - after the lockfile pin and before the
// artifact is materialized in the extracted store every later install
// hardlinks from.
//
// The three rows are one fixture with one thing changed each time: a signature
// that does not verify is refused, the same artifact with a signature that does
// verify is accepted (the positive control, without which the refusal could be
// a fixture nothing accepts), and a wrong pin alongside the bad signature
// reports the pin - which is what pins the order of the two checks.
func TestWarmVerifiesSignatures(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, false)
	sha := sha256Hex(mustReadFile(t, tarPath))
	good := serverSignatureMeta(signTestBytes(t, manifestJSON))
	bad := serverSignatureMeta(signTestBytes(t, []byte("a document this artifact does not carry")))

	t.Run("a signature that does not verify refuses the warm", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		payload := installPayload{meta: bad, artifact: artifactData{Path: tarPath}, artifactSHA: sha}
		err := warmVerifyAndEnsure(context.Background(), fx.deps, testSignedCollection, payload)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("warmVerifyAndEnsure() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
	})

	t.Run("a signature that verifies accepts the same artifact", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		payload := installPayload{meta: good, artifact: artifactData{Path: tarPath}, artifactSHA: sha}
		if err := warmVerifyAndEnsure(context.Background(), fx.deps, testSignedCollection, payload); err != nil {
			t.Fatalf("warmVerifyAndEnsure() = %v, want nil", err)
		}
	})

	t.Run("the pin is checked before the signature", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		pinned := testSignedCollection
		pinned.SHA256 = testOtherDigest
		payload := installPayload{meta: bad, artifact: artifactData{Path: tarPath}, artifactSHA: sha}
		err := warmVerifyAndEnsure(context.Background(), fx.deps, pinned, payload)
		if !errors.Is(err, helpers.ErrSHA256Mismatch) {
			t.Fatalf("warmVerifyAndEnsure() = %v, want errors.Is helpers.ErrSHA256Mismatch", err)
		}
	})
}

// TestSignatureVerdictAggregatesToTheSignatureExitClass carries a REAL verdict
// - the error verifyCollectionSignatures itself builds, collection key and all
// - through the aggregation a per-collection worker performs, and asserts the
// exit code an operator's CI branches on.
//
// cmd/go-galaxy/exitcode's own signatureExitCases already pins the ordering
// with a bare sentinel; what this adds is that the shape this package actually
// produces survives it, so a future wrap here that hid the sentinel behind
// something errors.Is cannot walk would fail where the synthetic row could not.
func TestSignatureVerdictAggregatesToTheSignatureExitClass(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	meta := serverSignatureMeta(signTestBytes(t, []byte("a document this artifact does not carry")))
	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if err == nil {
		t.Fatal("verifyCollectionSignatures() = nil, want a verification verdict")
	}

	var failures failureRecorder
	failures.record(err)
	if got := exitcode.FromError(failures.summary().installError()); got != exitcode.ExitSignature {
		t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
	}
}

// TestSignatureFetchDeadlineFiresOnAStalledSource proves the budget is really
// wired around the gather, not merely declared: a source that accepts the
// connection and never answers is ended by this collection's own signature
// budget, and the failure is reported as that budget rather than as a caller's
// cancellation.
//
// The budget is shrunk through Infra's test-only override, which is the only
// thing that field exists for. The stall is the server refusing to answer until
// the request's context is done, so nothing here waits on a wall clock beyond
// the override itself.
func TestSignatureFetchDeadlineFiresOnAStalledSource(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)

	stalled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer stalled.Close()

	fx := newVerifyFixture(t, writeTestKeyring(t), "1", []string{stalled.URL + "/acme-app.asc"})
	fx.deps.runtime.SignatureFetchDeadline = 50 * time.Millisecond

	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath))
	if !errors.Is(err, helpers.ErrSignatureFetchDeadline) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureFetchDeadline", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("verifyCollectionSignatures() = %v, must not leave a context sentinel reachable", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
		t.Fatalf("exit code = %d, want %d (a spent budget is a wire failure, never an interrupt)", got, exitcode.ExitNetwork)
	}
}

// TestVerifyContextIsSafeForConcurrentUse exercises what verifyContext,
// signature.Verify and signature.Keyring all claim in prose: one keyring, one
// policy and one fetcher, read by every worker of a run without
// synchronization.
//
// It is named from signature.Verify's own doc comment, which until this test
// existed declined to claim the property had been observed rather than only
// argued. Under -race, which is how CI runs this package, a write anywhere on
// that shared state fails here.
//
// The collections differ per goroutine while the context is shared, which is
// the real shape: workers verify different artifacts through one context. Half
// the goroutines take the requirement-source path and half the server-blob
// path, so both gathers run concurrently against the same fetcher.
func TestVerifyContextIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	const workers = 32

	tarPath, manifestJSON := buildSignedArtifact(t, false)
	source := writeTestSignature(t, manifestJSON)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", []string{source})
	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			payload := verifyPayload(nil, tarPath)
			if i%2 == 1 {
				payload = verifyPayload(meta, tarPath)
			}
			errs <- verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, payload)
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil from every worker", err)
		}
	}
}

// TestSignatureVerdictDoesNotEvictTheArtifact pins the exclusion of
// helpers.ErrSignatureVerificationFailed from prepareWithRecovery's
// evict-and-refetch.
//
// A server's own signatures come from the version metadata, and the retry
// re-reads the same cached metadata, so one junk entry there would otherwise
// cost a deleteObject against shared cache state plus a full re-download on
// every run, reaching the identical verdict - a destructive write primitive a
// hostile or broken server triggers at will.
//
// The second row is the positive control on the same harness, and it is the
// same one the source-failure test uses for the same reason: a chain mismatch
// IS about the artifact's own content, so it still evicts exactly once. The
// pair is what makes the first row a statement about which verdicts evict
// rather than about a harness whose Delete is never reached.
//
// KILLING MUTATION, run and reverted: isBlobSetVerdict dropped from
// unrepairableByRefetch's disjunction. The first row fails:
//
//	verify_pipeline_test.go:440: Delete calls = 1, want 0: a verdict over the gathered blobs is not the artifact's fault
func TestSignatureVerdictDoesNotEvictTheArtifact(t *testing.T) {
	t.Parallel()

	t.Run("a failing signature evicts nothing", func(t *testing.T) {
		t.Parallel()
		fx := newSignedCacheFixture(t, false, nil)
		fx.meta.Signatures = []any{map[string]any{"signature": "these bytes are not an OpenPGP signature\n"}}

		err := installCollection(context.Background(), testSignedCollection, fx.deps, nil, fx.meta, downloadResult{})
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("installCollection() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := fx.artifacts.deleteCalls.Load(); got != 0 {
			t.Fatalf("Delete calls = %d, want 0: a verdict over the gathered blobs is not the artifact's fault", got)
		}
		assertExists(t, fx.artifactPath)
	})

	t.Run("a chain mismatch on the same harness still evicts once", func(t *testing.T) {
		t.Parallel()
		fx := newSignedCacheFixture(t, true, nil)
		fx.deps.verify.sources[requirementKey(testSignedCollection)] =
			[]string{writeTestSignature(t, fx.manifestJSON)}

		err := installCollection(context.Background(), testSignedCollection, fx.deps, nil, fx.meta, downloadResult{})
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("installCollection() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
		}
		if got := fx.artifacts.deleteCalls.Load(); got != 1 {
			t.Fatalf("Delete calls = %d, want exactly 1", got)
		}
	})
}

// TestSkippedCollectionsAreReportedWhenVerifying pins the line an operator gets
// on the run that starts verifying an existing workspace: every collection is
// already installed, the gate skips them all, nothing is verified, and the
// per-collection skip lines sit on the transient tier that --quiet and a
// non-TTY CI both drop.
//
// The second row is the control: with verification off there is nothing to
// report and the line must not appear, so it is not one more line on every
// ordinary re-run.
func TestSkippedCollectionsAreReportedWhenVerifying(t *testing.T) {
	t.Parallel()

	t.Run("verifying reports the skipped collections", func(t *testing.T) {
		t.Parallel()
		printer := &capturingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		fx.deps.verify.recordSkippedUnverified()
		fx.deps.verify.recordSkippedUnverified()

		fx.deps.verify.reportSkippedUnverified(runtime)
		if !printer.hasPersistentPrintContaining("not verified on this run") {
			t.Fatalf("no result-tier line about skipped collections; persists=%v", printer.persists)
		}
		if !printer.hasPersistentPrintContaining("2 ") {
			t.Fatalf("the line does not carry the count; persists=%v", printer.persists)
		}
	})

	t.Run("not verifying reports nothing", func(t *testing.T) {
		t.Parallel()
		printer := &capturingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)
		var off *verifyContext

		off.recordSkippedUnverified()
		off.reportSkippedUnverified(runtime)
		if len(printer.persists) != 0 {
			t.Fatalf("persists = %v, want nothing when verification is off", printer.persists)
		}
	})
}
