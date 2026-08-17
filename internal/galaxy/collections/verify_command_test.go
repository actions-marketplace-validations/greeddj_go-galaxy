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
// The refusing row uses a source that cannot verify rather than one that can, which is what keeps this
// file narrow: a failed verdict never reaches the manifest chain walk, so nothing here depends on
// fakegalaxy's artifacts carrying a FILES.json a chain walk could succeed against - that stays true of
// every row below, unchanged. A command-level SUCCESS path installing a validly signed collection has
// its own home instead: verify_signed_e2e_test.go proves that a real command install verifies and
// accepts an artifact walked all the way through MANIFEST.json, FILES.json, and the one file it lists,
// over the identical shared verifyCollectionSignatures this file's own refusing rows already reach - the
// proof this file cannot make on its own, since a refusing row's verdict is settled before the chain
// walk is ever reached. This file's job stays the wiring proof: that a real command reaches
// verifyCollectionSignatures at all, not what a successful verification over a real chain looks like.
//
// The same real-command-over-a-fake-server shape also carries this file's second
// concern: what a preview says about verification without exercising it
// (announceVerification's own tier split, and warnOfflineSignatureSources, the
// warning this file adds, which fires identically on a preview and a real run
// because the fact it states is equally true of both - a property shared by other
// warnings on the same funnel, not unique to this one), and the one asymmetry
// between install and warm a preview cannot see at all - warm's missing skip
// gate, TestWarmVerifiesACollectionInstallWouldSkip's own reachability proof.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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
// The returned cfg always carries a valid keyring path: what a caller does
// with it afterward is its own concern rather than this fixture's. Several
// tests below blank cfg.Signature.KeyringPath on some of their own rows, to
// drive the verification-off path through this identical fixture rather than
// building a second one for it.
func newVerifyCommandFixture(t *testing.T, sources []string) (*config.Config, *infra.Infra, string) {
	t.Helper()
	cfg, runtime := newVerifySetupFixture(t, writeTestKeyring(t), sources)

	return cfg, runtime, cfg.DownloadPath
}

// newVerifySetupFixture is newVerifyCommandFixture with the keyring path as a
// parameter rather than always a valid one, which is what
// TestDryRunRefusesEveryVerificationSetupFailureARealRunDoes needs to drive
// every one of newVerifyContext's own refusals - an absent path, a keybox, a
// declared source with none configured - through a real command rather than
// through newVerifyContext directly.
func newVerifySetupFixture(t *testing.T, keyringPath string, sources []string) (*config.Config, *infra.Infra) {
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
			KeyringPath:   keyringPath,
			RequiredCount: "1",
		},
	}

	return cfg, infra.New(&capturingPrinter{}, srv.Client())
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

// capturedOutput returns runtime's underlying *capturingPrinter, failing the
// test rather than panicking if runtime was ever built without one - every
// fixture in this file builds runtime through infra.New(&capturingPrinter{},
// ...) rather than any other Output implementation.
func capturedOutput(t *testing.T, runtime *infra.Infra) *capturingPrinter {
	t.Helper()
	printer, ok := runtime.Output.(*capturingPrinter)
	if !ok {
		t.Fatalf("runtime.Output is %T, want *capturingPrinter", runtime.Output)
	}

	return printer
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
//	verify_command_test.go:169: Start() = <nil>, want errors.Is helpers.ErrSignatureVerificationFailed
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
//	verify_command_test.go:213: Warm() = <nil>, want errors.Is helpers.ErrSignatureVerificationFailed
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

// TestDryRunDisclosesThatVerificationWasNotExercised pins
// announceVerification's own split: which tier a run's verification state
// goes out on, and what it says there, moves with cfg.DryRun rather than
// staying fixed.
//
// Three rows, one fixture (newVerifyCommandFixture with no signatures:
// declared, so every row installs cleanly and only the tiers differ): (a) a
// keyring configured under --dry-run puts the disclosure on the warn tier and
// leaves the persist tier silent about verification; (b) the same keyring on
// a real run puts the opposite line on the persist tier and leaves the warn
// tier silent about the preview caveat - the control that the split did not
// move the real-run line install already had before this file existed; (c)
// no keyring configured under --dry-run carries neither line on either tier,
// since a run that verifies nothing has no verification state to disclose.
//
// KILLING MUTATION, run and reverted: the return statement removed from
// announceVerification's cfg.DryRun branch, so a dry run's Warnf call falls
// through into the real-run PersistentPrintf call right below it instead of
// returning first. Row (a)'s warn-tier check still passes - the Warnf call
// still ran - so it is the persist-tier check right after it that fails:
//
//	verify_command_test.go:276: the persist tier carries a verification line on a dry run
//
// KILLING MUTATION, run and reverted: the "so the would-fail count covers no
// signature verdict" clause deleted from announceVerification's dry-run
// message. Row (a)'s warn-tier check fails instead:
//
//	verify_command_test.go:272: no dry-run verification disclosure on the warn tier
func TestDryRunDisclosesThatVerificationWasNotExercised(t *testing.T) {
	t.Parallel()

	t.Run("keyring configured, dry run: the warn tier discloses and the persist tier is silent", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)
		cfg.DryRun = true

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		printer := capturedOutput(t, runtime)
		if !printer.hasWarnContaining("so the would-fail count covers no signature verdict") {
			t.Logf("warns = %v", printer.warns)
			t.Fatalf("no dry-run verification disclosure on the warn tier")
		}
		if printer.hasPersistentPrintContaining("Signature verification") {
			t.Logf("persists = %v", printer.persists)
			t.Fatalf("the persist tier carries a verification line on a dry run")
		}
	})

	t.Run("keyring configured, real run: the persist tier carries the real-run line, the warn tier is silent about it", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		printer := capturedOutput(t, runtime)
		want := fmt.Sprintf("🔏 Signature verification on: keyring %s, required count 1", cfg.Signature.KeyringPath)
		if !slices.Contains(printer.persists, want) {
			t.Fatalf("persists = %v, want it to contain %q", printer.persists, want)
		}
		if printer.hasWarnContaining("this preview validates the setup and verifies no collection") {
			t.Fatalf("the warn tier carries a dry-run disclosure on a real run: %v", printer.warns)
		}
	})

	t.Run("no keyring, dry run: neither tier carries a verification line", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)
		cfg.DryRun = true
		cfg.Signature.KeyringPath = ""

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		printer := capturedOutput(t, runtime)
		if printer.hasPersistentPrintContaining("Signature verification") ||
			printer.hasWarnContaining("signature verification is configured") {
			t.Fatalf("a verification line leaked with no keyring configured; persists=%v warns=%v", printer.persists, printer.warns)
		}
	})
}

// TestDryRunFetchesNoSignatureSource proves classifyDryRun's own probes never
// reach the network for a collection's declared signature source, which is
// what makes announceVerification's dry-run disclosure true rather than
// aspirational: a preview that quietly fetched the source anyway would still
// claim it validates the setup and verifies no collection.
//
// The fixture primes the artifact cache with verification switched off, so
// priming itself never touches the counting server: what the dry-run
// assertions below need in hand is a cached artifact, not a verified one, and
// counting the priming run's own fetch would hide a preview's fetch behind
// it. Both install --dry-run and warm --dry-run then run against that primed,
// signature-declaring fixture and must leave the counter at 0. The positive
// control is the same fixture and the same counting server: a real run, with
// verification back on, must reach it exactly once - proving the counter
// itself is wired to something a real gather touches, so the dry runs' 0 is a
// property of the preview rather than of a server nothing ever calls.
//
// Its subtests share one cfg, one runtime, and one hit counter, and run in
// declaration order rather than in parallel: the priming step above them has
// to land before any of the three read the counter, and the counter itself
// would race under t.Parallel.
//
// KILLING MUTATION, run and reverted: a fetch of the first root's first source,
// vc.fetcher.FetchRequirementSource, added to newVerifyContext under a cfg.DryRun
// guard just before it returns the context - a plausible "at least check
// reachability at plan time" edit. The install --dry-run subtest then fails:
//
//	verify_command_test.go:380: install --dry-run hit the signature source 1 times, want 0
func TestDryRunFetchesNoSignatureSource(t *testing.T) {
	// hits is shared, unreset state across every subtest below, which run in
	// declaration order rather than in parallel (see above). A mutation that
	// makes an earlier subtest hit the source therefore also inflates every
	// later subtest's own count, since nothing zeroes it between them: the
	// killing-mutation quote above is pinned only against the first subtest
	// it reaches, and says nothing about whether the later two failed on
	// their own or merely inherited an already-wrong count.
	var hits atomic.Int32
	sigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("these bytes are not an OpenPGP signature\n"))
	}))
	t.Cleanup(sigSrv.Close)

	cfg, runtime, _ := newVerifyCommandFixture(t, []string{sigSrv.URL + "/sig.asc"})

	// Prime the artifact cache with verification switched off, through warm:
	// priming itself never reaches sigSrv, and warm never records an install,
	// so the positive control below cannot take install's own canSkipInstall
	// shortcut and skip verification (and the fetch it needs) entirely.
	cfg.Signature.DisableGPGVerify = true
	if err := Warm(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("priming Warm() = %v, want nil", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("priming hit the signature source %d times, want 0 (verification was off)", got)
	}
	cfg.Signature.DisableGPGVerify = false

	t.Run("install --dry-run fetches nothing", func(t *testing.T) {
		cfg.DryRun = true
		defer func() { cfg.DryRun = false }()

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() (dry run) = %v, want nil", err)
		}
		if got := hits.Load(); got != 0 {
			t.Fatalf("install --dry-run hit the signature source %d times, want 0", got)
		}
	})

	t.Run("warm --dry-run fetches nothing", func(t *testing.T) {
		cfg.DryRun = true
		defer func() { cfg.DryRun = false }()

		if err := Warm(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Warm() (dry run) = %v, want nil", err)
		}
		if got := hits.Load(); got != 0 {
			t.Fatalf("warm --dry-run hit the signature source %d times, want 0", got)
		}
	})

	t.Run("positive control: a real run hits the same source exactly once", func(t *testing.T) {
		err := Start(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := hits.Load(); got != 1 {
			t.Fatalf("a real run hit the signature source %d times, want exactly 1", got)
		}
	})
}

// verifySetupFailureRow is one row of
// TestDryRunRefusesEveryVerificationSetupFailureARealRunDoes's table: a
// keyring path and a declared source set that newVerifyContext - or, for a
// signature source this tool cannot fetch, loadRoots ahead of it - refuses
// with wantErr.
type verifySetupFailureRow struct {
	wantErr     error
	name        string
	keyringPath string
	sources     []string
}

// verifySetupFailureRows builds
// TestDryRunRefusesEveryVerificationSetupFailureARealRunDoes's table,
// factored out to keep that test under the funlen budget.
func verifySetupFailureRows(t *testing.T) []verifySetupFailureRow {
	t.Helper()
	// A GnuPG keybox: the "KBXf" magic looksLikeKeybox reads sits at bytes
	// 8..11, and nothing else about the file's shape matters to that check.
	keybox := make([]byte, 12)
	copy(keybox[8:], "KBXf")
	badKeyboxPath := filepath.Join(t.TempDir(), "keybox.gpg")
	mustWriteFile(t, badKeyboxPath, keybox)

	return []verifySetupFailureRow{
		{
			name:        "absent keyring path",
			keyringPath: filepath.Join(t.TempDir(), "does-not-exist.asc"),
			wantErr:     helpers.ErrKeyringUnreadable,
		},
		{
			name:        "keyring is a GnuPG keybox",
			keyringPath: badKeyboxPath,
			wantErr:     helpers.ErrKeyringIsKeybox,
		},
		{
			name:    "signatures declared with no keyring configured",
			sources: []string{"https://example.invalid/acme-app.asc"},
			wantErr: helpers.ErrKeyringRequired,
		},
		{
			name:        "a signature source this tool does not fetch",
			keyringPath: writeTestKeyring(t),
			sources:     []string{"ftp://example.invalid/acme-app.asc"},
			wantErr:     helpers.ErrUnsupportedSignatureSource,
		},
	}
}

// TestDryRunRefusesEveryVerificationSetupFailureARealRunDoes proves
// newVerifyContext's own refusals are dry-run-independent: every one of them
// is reached, and reached identically, before installWithState's and
// warmWithState's own cfg.DryRun branch is ever consulted - either inside
// newVerifyContext itself, or (for a signature source this tool cannot fetch)
// inside loadRoots, which both commands call before newVerifyContext even
// runs. Each row therefore runs twice, once per DryRun spelling, and both
// must classify identically. This table drives only Start;
// TestOfflineWarnsAboutUnfetchableSignatureSources drives the identical
// newVerifyContext funnel through both Start and Warm on both dry-run
// spellings, which is what closes warmWithState's own half of this claim.
//
// Two refusals this surface owns are deliberately not rows here:
// helpers.ErrInvalidSignatureCount and helpers.ErrUnknownSignatureStatusCode
// live in config.applySignatureConfig, which runs inside BuildCollectionConfig
// before any command's Start or Warm - dry-run-independent by construction,
// since no command has even begun by the time that check runs. They are
// covered in internal/galaxy/config/signature_test.go's
// TestApplySignatureConfigValidation instead.
//
// The positive control closes the table: a good keyring with no signatures:
// block installs cleanly under both spellings, which is what shows every row
// above is a refusal of that specific setup rather than of the fixture in
// general.
func TestDryRunRefusesEveryVerificationSetupFailureARealRunDoes(t *testing.T) {
	t.Parallel()

	for _, row := range verifySetupFailureRows(t) {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			for _, dryRun := range []bool{true, false} {
				t.Run(fmt.Sprintf("dry run = %v", dryRun), func(t *testing.T) {
					t.Parallel()
					cfg, runtime := newVerifySetupFixture(t, row.keyringPath, row.sources)
					cfg.DryRun = dryRun

					err := Start(context.Background(), cfg, runtime)
					if !errors.Is(err, row.wantErr) {
						t.Fatalf("Start() = %v, want errors.Is %v", err, row.wantErr)
					}
					if got := exitcode.FromError(err); got != exitcode.ExitUsage {
						t.Fatalf("exit code = %d, want %d (ExitUsage)", got, exitcode.ExitUsage)
					}
				})
			}
		})
	}

	t.Run("positive control: a good keyring with no signatures succeeds under both spellings", func(t *testing.T) {
		t.Parallel()
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("dry run = %v", dryRun), func(t *testing.T) {
				t.Parallel()
				cfg, runtime, _ := newVerifyCommandFixture(t, nil)
				cfg.DryRun = dryRun

				if err := Start(context.Background(), cfg, runtime); err != nil {
					t.Fatalf("Start() = %v, want nil", err)
				}
			})
		}
	})
}

// offlineWarnRow is one row of TestOfflineWarnsAboutUnfetchableSignatureSources's
// table: a declared source set, an --offline, keyring and --disable-gpg-verify
// spelling, and whether warnOfflineSignatureSources must fire.
type offlineWarnRow struct {
	name             string
	sources          []string
	offline          bool
	keyring          bool
	disableGPGVerify bool
	wantWarn         bool
}

// offlineWarnRows is TestOfflineWarnsAboutUnfetchableSignatureSources's own
// table, factored out to keep that test under the funlen budget.
//
// The second row is the inner-loop proof: a file source that never needs the
// network is listed first, ahead of the https source that does, over one
// collection's own declared list - the exact shape warnOfflineSignatureSources'
// own doc comment cites, a file:// source verifying on its own hiding a later
// http(s) source in the same list.
//
// KILLING MUTATION, run and reverted: warnOfflineSignatureSources' inner loop
// narrowed to look only at root.Signatures[0], applied through go test
// -overlay so no production file is edited. The second row's warning-must-fire
// leaves then fail, since the https source sitting at index 1 is never looked
// at:
//
//	verify_command_test.go:725: offline-signature-source warning present = false, want true
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var offlineWarnRows = []offlineWarnRow{
	{
		name: "offline with an https source warns", offline: true, keyring: true,
		sources: []string{"https://example.invalid/acme-app.asc"}, wantWarn: true,
	},
	{
		name: "offline with a file source listed first still reaches the https source after it", offline: true, keyring: true,
		sources: []string{"file:///nonexistent/sig.asc", "https://example.invalid/acme-app.asc"}, wantWarn: true,
	},
	{
		name: "online is silent", offline: false, keyring: true,
		sources: []string{"https://example.invalid/acme-app.asc"}, wantWarn: false,
	},
	{
		name: "offline with only a file source is silent", offline: true, keyring: true,
		sources: []string{"file:///nonexistent/sig.asc"}, wantWarn: false,
	},
	{
		name: "offline with no keyring is silent", offline: true, keyring: false,
		sources: []string{"https://example.invalid/acme-app.asc"}, wantWarn: false,
	},
	{
		name: "offline with --disable-gpg-verify is silent", offline: true, keyring: true, disableGPGVerify: true,
		sources: []string{"https://example.invalid/acme-app.asc"}, wantWarn: false,
	},
	{
		name: "offline with no declared sources at all is silent", offline: true, keyring: true,
		wantWarn: false,
	},
}

// offlineWarnCommand is one command TestOfflineWarnsAboutUnfetchableSignatureSources
// drives both offlineWarnRows and its own dry-run spellings through.
type offlineWarnCommand struct {
	run  func(context.Context, *config.Config, *infra.Infra) error
	name string
}

// offlineWarnCommands is install and warm, the two commands that reach
// newVerifyContext and therefore warnOfflineSignatureSources.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var offlineWarnCommands = []offlineWarnCommand{
	{name: "install", run: Start},
	{name: "warm", run: Warm},
}

// newVerifyTwoRootFixture builds a requirements.yml naming two collections -
// acme.app and acme.other - with a keyring configured, so a test can prove a
// property that holds across every root the file declares rather than only
// the first one. Only acme.other carries secondSources, when given.
//
// It exists for the outer-loop proof below: warnOfflineSignatureSources runs
// on prep.AllRoots, which loadRoots builds straight from parsing this file,
// before resolveOrLoadLockfile ever contacts a server - so the fake server
// here never needs to know about either collection for the warning itself to
// fire, and acme.other is never registered on it at all.
func newVerifyTwoRootFixture(t *testing.T, secondSources []string) (*config.Config, *infra.Infra) {
	t.Helper()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	var body strings.Builder
	body.WriteString("collections:\n")
	body.WriteString("  - name: acme.app\n    version: \"*\"\n")
	body.WriteString("  - name: acme.other\n    version: \"*\"\n")
	if len(secondSources) > 0 {
		body.WriteString("    signatures:\n")
		for _, source := range secondSources {
			body.WriteString("      - " + source + "\n")
		}
	}
	mustWriteFile(t, reqPath, []byte(body.String()))

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          2,
		DownloadWorkers:  2,
		Timeout:          verifyCommandTimeout,
		Signature: config.SignatureConfig{
			KeyringPath:   writeTestKeyring(t),
			RequiredCount: "1",
		},
	}

	return cfg, infra.New(&capturingPrinter{}, srv.Client())
}

// assertOuterLoopReachesSecondRoot drives the outer-loop half of the proof
// TestOfflineWarnsAboutUnfetchableSignatureSources' own rows cannot make,
// factored into its own function - rather than an inline t.Run closure - to
// keep that test under the cyclop budget.
func assertOuterLoopReachesSecondRoot(t *testing.T, offlineWarnSubstr string) {
	t.Helper()
	cfg, runtime := newVerifyTwoRootFixture(t, []string{"https://example.invalid/acme-app.asc"})
	cfg.Offline = true

	_ = Start(context.Background(), cfg, runtime)
	printer := capturedOutput(t, runtime)
	if !printer.hasWarnContaining(offlineWarnSubstr) {
		t.Logf("warns = %v", printer.warns)
		t.Fatalf("the outer loop stopped at the first root and never reached the second one's own source")
	}
}

// TestOfflineWarnsAboutUnfetchableSignatureSources pins
// warnOfflineSignatureSources' own predicate - --offline, plus a declared
// source signature.SourceRequiresNetwork says needs the network - against the
// five ways it must stay silent: !cfg.Offline, no keyring configured,
// --disable-gpg-verify (the other half of signature.VerificationEnabled - a
// future edit hoisting this warning above the enabled check would otherwise
// go uncaught), a file-only source list, and no declared sources at all. Both
// commands and both dry-run spellings run every row, since the warning fires
// from newVerifyContext before either command's own dry-run branch is
// reached.
//
// The final subtest, "the fact behind the warning...", is what the warning
// exists to give an operator advance notice of: primed with verification off
// (through warm, which never records an install, so the second run below
// cannot take install's own canSkipInstall shortcut and skip verification
// entirely), a real install --offline run whose gather reaches the declared
// https source fails closed rather than installing unverified. The subtest
// right before it is the outer-loop proof this table's own rows cannot make,
// since every row declares sources on one collection: over a fixture naming
// two, with only the second carrying a source, the warning must still fire.
//
// KILLING MUTATION, run and reverted: the `if !cfg.Offline { return }` guard
// deleted from warnOfflineSignatureSources. The "online is silent" control
// then fails, since the warning now fires whether or not --offline is set:
//
//	verify_command_test.go:725: offline-signature-source warning present = true, want false
//
// KILLING MUTATION, run and reverted: SourceRequiresNetwork's body replaced
// with `return false`, applied through go test -overlay against
// internal/galaxy/signature/source.go so no collections-package file is
// edited. Every row above whose want is true then fails, one of them with:
//
//	verify_command_test.go:725: offline-signature-source warning present = false, want true
//
// KILLING MUTATION, run and reverted: warnOfflineSignatureSources' outer loop
// narrowed to look only at roots[0], applied through go test -overlay so no
// production file is edited. The outer-loop subtest below then fails: acme.app
// (the first root) declares no source at all, so acme.other's own source is
// never reached:
//
//	verify_command_test.go:734: the outer loop stopped at the first root and never reached the second one's own source
func TestOfflineWarnsAboutUnfetchableSignatureSources(t *testing.T) {
	t.Parallel()

	const offlineWarnSubstr = "declares a signature source that must be fetched over the network"

	for _, row := range offlineWarnRows {
		for _, command := range offlineWarnCommands {
			for _, dryRun := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/dry run = %v", row.name, command.name, dryRun), func(t *testing.T) {
					t.Parallel()
					cfg, runtime, _ := newVerifyCommandFixture(t, row.sources)
					cfg.Offline = row.offline
					cfg.DryRun = dryRun
					cfg.Signature.DisableGPGVerify = row.disableGPGVerify
					if !row.keyring {
						cfg.Signature.KeyringPath = ""
					}

					_ = command.run(context.Background(), cfg, runtime)
					printer := capturedOutput(t, runtime)
					got := printer.hasWarnContaining(offlineWarnSubstr)
					if got != row.wantWarn {
						t.Logf("warns = %v", printer.warns)
						t.Fatalf("offline-signature-source warning present = %v, want %v", got, row.wantWarn)
					}
				})
			}
		}
	}

	t.Run("the outer loop does not stop at the first root: only the second collection declares a source", func(t *testing.T) {
		t.Parallel()
		assertOuterLoopReachesSecondRoot(t, offlineWarnSubstr)
	})

	t.Run("the fact behind the warning: a real offline gather fails rather than installing unverified", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, []string{"https://example.invalid/acme-app.asc"})

		cfg.Signature.DisableGPGVerify = true
		if err := Warm(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("priming Warm() = %v, want nil", err)
		}

		cfg.Signature.DisableGPGVerify = false
		cfg.Offline = true
		err := Start(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureSourceUnavailable", err)
		}
		if !errors.Is(err, helpers.ErrOfflineMode) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrOfflineMode", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitInstall {
			t.Fatalf("exit code = %d, want %d (ExitInstall)", got, exitcode.ExitInstall)
		}
	})
}

// assertSameDryRunReport fails the test unless off and on agree on the ok,
// error and persist tiers, byte for byte. Factored out of
// TestDryRunReportIsUnchangedByVerification to keep that test under the
// cyclop budget; the warn tier is deliberately not compared here, since the
// one caller below needs the opposite verdict on it.
//
// This helper has exactly one caller, over a single-collection fixture. On
// that fixture, oks and persists are each pinned by their own killing
// mutation below; only errs is documentary.
//
// KILLING MUTATION, run and reverted: reportDryRunResults' default-case
// action verb given a cfg.Signature.KeyringPath-conditioned override,
// applied through go test -overlay against dryrun.go so no production file
// was left edited. The comparison fails:
//
//	verify_command_test.go:857: oks differ: off=[Would install: acme.app@1.0.0 ...] on=[Would verify-install: acme.app@1.0.0 ...]
//
// KILLING MUTATION, run and reverted: classifyDryRun's own summary
// PersistentPrintf given an analogous cfg.Signature.KeyringPath-conditioned
// prefix, applied the same way. The comparison fails:
//
//	verify_command_test.go:857: persists differ: off=[Dry run: 1 would install, ...] on=[Signed dry run: 1 would install, ...]
//
// errs is documentary rather than pinned, for a reason specific to this
// fixture: its one collection lands on the "off" run's ok tier (measured -
// off=[Would install: acme.app@1.0.0 (would download)]), and
// reportDryRunResults never calls both Errorf and Okf for one collection.
// A difference confined to the "off" run's own errs therefore makes that
// run's Start() return non-nil, which trips the "Start() (verification
// off) = %v, want nil" check that runs above this helper's own call; a
// difference confined to the "on" run's errs instead moves the one
// collection off the ok tier, so oks differs first. Either way, the errs
// check below can never be the first of the three to fail on this fixture,
// and it is kept anyway rather than deleted, since a caller whose "off" run
// does not land on the ok tier could make it reachable on its own.
func assertSameDryRunReport(t *testing.T, off, on *capturingPrinter) {
	t.Helper()
	if !slices.Equal(off.oks, on.oks) {
		t.Fatalf("oks differ: off=%v on=%v", off.oks, on.oks)
	}
	if !slices.Equal(off.errs, on.errs) {
		t.Fatalf("errs differ: off=%v on=%v", off.errs, on.errs)
	}
	if !slices.Equal(off.persists, on.persists) {
		t.Fatalf("persists differ: off=%v on=%v", off.persists, on.persists)
	}
}

// TestDryRunReportIsUnchangedByVerification pins the no-per-collection-verdict
// decision: classifyDryRun's own report - what it prints on the ok, error and
// persist tiers - never depends on whether this run verifies signatures at
// all. The warn tier is the one place verification is allowed to show up
// (announceVerification's own disclosure), so it is the one tier this test
// requires to differ rather than match.
//
// The same fixture runs twice, differing only in Signature.KeyringPath, with
// the printer swapped between runs so each run's own lines can be compared
// rather than merged.
//
// The positive control proves the byte-identity assertion is capable of
// catching a real difference: the identical fixture run online and then
// offline-and-uncached produces reports that visibly differ (a would-download
// line against a would-fail one), which is what shows the "off" and "on"
// reports above were found identical because they ARE, not because this
// comparison cannot tell two reports apart.
//
// KILLING MUTATION, run and reverted: `if cfg.Signature.KeyringPath != "" {
// return fmt.Errorf(...) }` added at the top of dryRunPinVerdict, which
// already takes cfg. The byte-identity assertion then fails:
//
//	verify_command_test.go:857: oks differ: off=[Would install: acme.app@1.0.0 (would download)] on=[]
func TestDryRunReportIsUnchangedByVerification(t *testing.T) {
	t.Parallel()

	t.Run("verification off and on report identically apart from the warn tier", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)
		cfg.DryRun = true
		cfg.Signature.KeyringPath = ""

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() (verification off) = %v, want nil", err)
		}
		off := capturedOutput(t, runtime)

		// onErr is checked below the comparisons deliberately: what this
		// subtest is about is the report the two runs printed, not whether
		// either one happened to return nil, so a mutation that fails the
		// "on" run over its own verification state is caught by the
		// byte-identity comparison rather than hiding behind an error check
		// placed ahead of it.
		runtime.Output = &capturingPrinter{}
		cfg.Signature.KeyringPath = writeTestKeyring(t)
		onErr := Start(context.Background(), cfg, runtime)
		on := capturedOutput(t, runtime)

		assertSameDryRunReport(t, off, on)
		if slices.Equal(off.warns, on.warns) {
			t.Fatal("expected the warn tier to differ between verification off and on, and it did not")
		}
		if onErr != nil {
			t.Fatalf("Start() (verification on) = %v, want nil", onErr)
		}
	})

	t.Run("positive control: differing --offline reports are caught as different", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)
		cfg.DryRun = true

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() (online) = %v, want nil", err)
		}
		online := capturedOutput(t, runtime)

		runtime.Output = &capturingPrinter{}
		cfg.Offline = true
		if err := Start(context.Background(), cfg, runtime); err == nil {
			t.Fatal("Start() (offline, uncached) = nil, want a would-fail error")
		}
		offline := capturedOutput(t, runtime)

		if slices.Equal(online.oks, offline.oks) &&
			slices.Equal(online.errs, offline.errs) &&
			slices.Equal(online.persists, offline.persists) {
			t.Fatal("expected the online and offline dry-run reports to differ, and they did not")
		}
	})
}

// TestWarmVerifiesACollectionInstallWouldSkip is the reachability proof for
// the asymmetry announceVerification's own doc comment cites: warm carries no
// skip gate over an already-cached collection, so it re-verifies one install
// would have skipped.
//
// Row 1 primes with verification off over a requirements file carrying an
// unverifiable file:// source, then runs Warm with verification on: the
// artifact is already cached, and warmOne re-verifies it anyway, so the run
// fails. Row 2 draws the identical shape for Start: install's own
// canSkipInstall gate means the second run finds a matching installed record
// and extract marker and skips the collection before
// verifyCollectionSignatures is ever reached, so it succeeds - and says so,
// on the persist tier, through reportSkippedUnverified.
//
// KILLING MUTATION, run and reverted: warmVerifyAndEnsure changed to return
// nil before ever calling verifyCollectionSignatures whenever
// deps.extractStore.Ready(payload.artifactSHA) already reports true - a
// plausible "warm has its own skip gate too" edit. Row 1 then fails:
//
//	verify_command_test.go:926: Warm() (verification on, cache primed) = <nil>, want errors.Is helpers.ErrSignatureVerificationFailed
func TestWarmVerifiesACollectionInstallWouldSkip(t *testing.T) {
	t.Parallel()

	t.Run("warm has no skip gate: it re-verifies a cached collection and fails", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, []string{writeUnverifiableSignature(t)})
		cfg.Signature.DisableGPGVerify = true

		if err := Warm(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("priming Warm() (verification off) = %v, want nil", err)
		}

		cfg.Signature.DisableGPGVerify = false
		err := Warm(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Warm() (verification on, cache primed) = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
	})

	t.Run("install's skip gate makes its verdict faithful: an already-installed collection is never re-verified", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, []string{writeUnverifiableSignature(t)})
		cfg.Signature.DisableGPGVerify = true

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("priming Start() (verification off) = %v, want nil", err)
		}

		cfg.Signature.DisableGPGVerify = false
		runtime.Output = &capturingPrinter{}
		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() (verification on, already installed) = %v, want nil", err)
		}
		printer := capturedOutput(t, runtime)
		if !printer.hasPersistentPrintContaining("already-installed collection(s) were skipped and therefore not verified") {
			t.Fatalf("persists = %v, want the skipped-unverified line", printer.persists)
		}
	})
}

// TestQueryBearingSignatureSourceIsFetchedWithItsQueryIntact is the fetch
// control for the persisted-spec query cut normalizeSignatures makes
// (internal/galaxy/collections/resolve.go): a run's live signature source -
// read from requirementSources(roots), never from the persisted spec - keeps
// its query when it is actually fetched. Without this,
// signature_query_persistence_test.go's tests would be indistinguishable
// from a change that broke fetching outright: a query stripped everywhere,
// not just at the point of persistence, would make those tests pass too.
//
// Reuses TestDryRunFetchesNoSignatureSource's counting-server shape, above,
// inspecting the request it actually receives rather than merely counting
// it.
func TestQueryBearingSignatureSourceIsFetchedWithItsQueryIntact(t *testing.T) {
	t.Parallel()
	const capabilityQuery = "X-Amz-Signature=deadbeefcapability&X-Amz-Expires=3600"

	var hits atomic.Int32
	queries := make(chan string, 4)
	sigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		queries <- r.URL.RawQuery
		_, _ = w.Write([]byte("these bytes are not an OpenPGP signature\n"))
	}))
	t.Cleanup(sigSrv.Close)

	cfg, runtime, _ := newVerifyCommandFixture(t, []string{sigSrv.URL + "/sig.asc?" + capabilityQuery})

	err := Start(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("signature source fetched %d times, want exactly 1", got)
	}

	got := <-queries
	if got != capabilityQuery {
		t.Fatalf("request query = %q, want %q (the live source keeps its query; only the persisted spec cuts it)", got, capabilityQuery)
	}
}
