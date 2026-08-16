package collections

// This file is the reachability proof for signature verification: it drives the
// two commands that verify - collections.Start and collections.Warm - end to
// end against a fake Galaxy server, with a keyring configured the way an
// operator configures one and a signatures: block in the requirements file.
//
// Everything else in this package's signature tests enters below a seam
// (installCollection, prepareInstall, warmVerifyAndEnsure, verifyCollectionSignatures),
// so each of them stays green if the verify context never reaches the workers
// at all. These are the tests that fail when the wiring is cut.
//
// The refusing row uses a source that cannot verify rather than one that can,
// which is what keeps the fake server usable here: a failed verdict never
// reaches the manifest chain walk, and the artifacts fakegalaxy generates carry
// no FILES.json for a chain walk to succeed against. A chain walk on a
// command-level SUCCESS path is therefore left uncovered, deliberately and
// narrowly: it is the same shared verifyCollectionSignatures the install-side
// tests already walk a real chain through (TestVerifyFailsOnChainMismatch and
// TestWarmVerifiesSignatures' own positive control), over an artifact this
// package builds itself. What that leaves untested is fakegalaxy's archive
// shape, not this project's verification path.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// verifyCommandTimeout is the HTTP client timeout these command-level fixtures
// use: long enough never to be the reason a row fails, short enough that a real
// hang still fails fast.
const verifyCommandTimeout = 30 * time.Second

// newVerifyCommandFixture builds everything a real collections.Start or
// collections.Warm run needs - a fake Galaxy server publishing acme.app@1.0.0,
// a cold cache, an empty collections tree, and a requirements.yml - with a
// keyring configured and, optionally, a signatures: block naming sources.
//
// The keyring is always configured, so both rows of every test below run with
// verification ON: what differs between them is only whether the requirements
// file names a source that cannot verify. That is what makes the passing row a
// control over the fixture rather than over the feature.
func newVerifyCommandFixture(t *testing.T, sources []string) (*config.Config, *infra.Infra, string) {
	t.Helper()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")

	var body strings.Builder
	body.WriteString("collections:\n  - name: acme.app\n    version: \"*\"\n")
	if len(sources) > 0 {
		body.WriteString("    signatures:\n")
		for _, source := range sources {
			body.WriteString("      - " + source + "\n")
		}
	}
	mustWriteFile(t, reqPath, []byte(body.String()))

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          2,
		DownloadWorkers:  2,
		Timeout:          verifyCommandTimeout,
		Signature: config.SignatureConfig{
			KeyringPath:   writeTestKeyring(t),
			RequiredCount: "1",
		},
	}

	return cfg, infra.New(&capturingPrinter{}, srv.Client()), downloadPath
}

// writeUnverifiableSignature writes bytes that are not OpenPGP data at all and
// returns a file:// URL naming them. Reading it succeeds, so the source is in
// hand; checking it fails, so the required count of one goes unsatisfied - the
// verdict this file needs, reached without any signature having verified and
// therefore without a chain walk.
func writeUnverifiableSignature(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unverifiable.asc")
	mustWriteFile(t, path, []byte("these bytes are not an OpenPGP signature\n"))
	return "file://" + path
}

// TestInstallCommandVerifiesSignatures is the reachability proof for install:
// a real collections.Start run, with a keyring configured and a signatures:
// source that cannot verify, must fail the run with the signature verdict and
// exit 10.
//
// The second row is the positive control on the same fixture and the same
// verification state: drop the signatures: block and the identical requirements
// install cleanly against the identical server. Without it, the refusal could
// be a fixture that installs nothing whatever the signatures say.
//
// KILLING MUTATION, run and reverted: plan.verify replaced by nil at
// installLevels' call site in installWithState (install_command.go), which cuts
// the verify context off from every install worker while leaving the whole
// package compiling. The first row fails:
//
//	verify_command_test.go:127: Start() = <nil>, want errors.Is helpers.ErrSignatureVerificationFailed
func TestInstallCommandVerifiesSignatures(t *testing.T) {
	t.Parallel()

	t.Run("a signature that cannot verify fails the install", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, downloadPath := newVerifyCommandFixture(t, []string{writeUnverifiableSignature(t)})

		err := Start(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
		}
		if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "acme", "app", "MANIFEST.json")); statErr == nil {
			t.Fatal("the refused collection was installed anyway")
		}
	})

	t.Run("the same requirements without signatures install", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, downloadPath := newVerifyCommandFixture(t, nil)

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		assertExists(t, filepath.Join(downloadPath, "ansible_collections", "acme", "app", "MANIFEST.json"))
	})
}

// TestWarmCommandVerifiesSignatures is the same reachability proof for warm,
// which mounts the flags and must therefore honor them: signatures: on a
// requirements entry is checked by a real collections.Warm run, not only by the
// install pipeline.
//
// The second row is the positive control on the same fixture, exactly as
// install's is: the same requirements without a signatures: block warm cleanly.
//
// KILLING MUTATION, run and reverted: the verify argument replaced by nil in
// warmCollections' newInstallDeps call (warm_command.go), which compiles even
// with the parameter left in place, since an unused parameter is legal. The
// first row fails:
//
//	verify_command_test.go:171: Warm() = <nil>, want errors.Is helpers.ErrSignatureVerificationFailed
func TestWarmCommandVerifiesSignatures(t *testing.T) {
	t.Parallel()

	t.Run("a signature that cannot verify fails the warm", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, []string{writeUnverifiableSignature(t)})

		err := Warm(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Warm() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
		}
	})

	t.Run("the same requirements without signatures warm", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)

		if err := Warm(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Warm() = %v, want nil", err)
		}
	})
}
