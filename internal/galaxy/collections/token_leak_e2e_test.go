package collections_test

// This file pins the token-secrecy invariant end-to-end: a Galaxy API token
// must never reach a human (stdout/stderr, at any verbosity) or a file (the
// local Bolt snapshot, the S3 snapshot payload, the lockfile, the metrics
// file, GALAXY.yml). See config.Secret's own doc comment for the design and
// internal/galaxy/config/servers_test.go for the unit-level redaction table;
// this file is the integration-level counterpart, run against a real fake
// Galaxy server, a real fetch.New transport, and the real progress.Printer.

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// leakToken is deliberately distinctive - it cannot collide with any
// generated artifact byte, filename, or protocol keyword the pipeline emits
// on its own, so any match found by the tests below is unambiguously the
// token itself, not incidental text.
const leakToken = "tok3n-must-not-appear-anywhere"

// tokenLeakConfig builds a *config.Config for a single fakegalaxy server
// requiring leakToken, rooted under a fresh t.TempDir() so cache, install,
// lockfile, and metrics paths are all isolated per test.
func tokenLeakConfig(t *testing.T, srv *fakegalaxy.Server) *config.Config {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - name: ns.a\n    version: \"*\"\n"), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
	return &config.Config{
		Servers:          []config.Server{{URL: srv.URL(), Token: config.NewSecret(leakToken)}},
		Server:           srv.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		MetricsFile:      filepath.Join(root, "metrics.json"),
		Workers:          4,
		Timeout:          e2eTimeout,
		Verbose:          true,
	}
}

// tokenLeakRuntime builds an *infra.Infra wired exactly like the CLI's own
// newHTTPClient (see cmd/go-galaxy/commands/install.go's serverAuths): the
// real fetch.New transport, so per-origin token attachment is exercised
// honestly, paired with the printer captured by the caller.
func tokenLeakRuntime(cfg *config.Config, printer *progress.Progress) *infra.Infra {
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
	return infra.New(printer, fetch.New(cfg.Timeout, auths))
}

// captureStdIO redirects the process-wide os.Stdout/os.Stderr to pipes for
// the duration of fn, draining both concurrently (so a chatty fn can never
// deadlock on a full pipe buffer), and returns everything written to each.
// This is the only way to observe progress.New's real output: Progress
// reads the os.Stdout/os.Stderr package vars at construction time, and
// exports no other way to redirect them.
func captureStdIO(t *testing.T, fn func()) ([]byte, []byte) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdout): %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stderr): %v", err)
	}
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
	}()

	var outBuf, errBuf bytes.Buffer
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&outBuf, outR)
		close(outDone)
	}()
	go func() {
		_, _ = io.Copy(&errBuf, errR)
		close(errDone)
	}()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	<-outDone
	<-errDone
	_ = outR.Close()
	_ = errR.Close()
	return outBuf.Bytes(), errBuf.Bytes()
}

// assertNoTokenInTree walks every file under root and fails the test if any
// one of them contains token, reporting the offending path. Walking rather
// than hardcoding a fixed set of filenames means a future file added under
// root - a new sidecar, a new bucket - is covered automatically.
func assertNoTokenInTree(t *testing.T, root, token string) {
	t.Helper()
	if _, err := os.Stat(root); err != nil {
		return // nothing was written under root at all; nothing to check.
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // path comes from this test's own WalkDir over its own temp tree.
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(token)) {
			t.Errorf("token leaked into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// assertNoTokenInFile fails the test if the file at path contains token. A
// missing file is not itself a failure here - some of this test's callers
// probe a file that only exists when a particular feature (metrics,
// lockfile) is configured on, and other tests in this package already cover
// the "file gets written" behavior on its own.
func assertNoTokenInFile(t *testing.T, path, token string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixed cfg field, not user input.
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read %s: %v", path, err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Errorf("token leaked into %s", path)
	}
}

// TestTokenNeverLeaksDuringVerboseInstall runs a full verbose install
// against a fake Galaxy server whose token is the distinctive leakToken,
// then asserts the token appears in none of: stdout, stderr (both captured
// through the real progress.Printer, the loudest configuration available),
// the local Bolt snapshot, the metrics file, the lockfile, every GALAXY.yml
// under the install path, or the store payload the S3 backend would have
// marshaled for the same run.
//
// This test and TestTokenNeverLeaksOnAuthFailure both swap the process-wide
// os.Stdout/os.Stderr (see captureStdIO) and so deliberately do not run in
// parallel - with each other or, since no other test in this package
// constructs a real progress.Progress, with anything else.
func TestTokenNeverLeaksDuringVerboseInstall(t *testing.T) {
	srv := fakegalaxy.New(t)
	srv.RequireAuth("Token " + leakToken)
	srv.AddVersion("ns", "a", "1.0.0", nil)

	cfg := tokenLeakConfig(t, srv)
	ctx := context.Background()

	var installErr, lockErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := tokenLeakRuntime(cfg, printer)
		runtime.DebugAnsibleConfig(cfg)
		runtime.WarnConfig(cfg)

		installErr = collections.Start(ctx, cfg, runtime)
		// Lock reuses the same cache dir and requirements, exercising the
		// lockfile-writing path (Start alone never writes one) against the
		// same server and token.
		lockErr = collections.Lock(ctx, cfg, runtime)
	})
	if installErr != nil {
		t.Fatalf("Start: %v", installErr)
	}
	if lockErr != nil {
		t.Fatalf("Lock: %v", lockErr)
	}

	if bytes.Contains(stdout, []byte(leakToken)) {
		t.Errorf("token leaked into stdout: %q", stdout)
	}
	if bytes.Contains(stderr, []byte(leakToken)) {
		t.Errorf("token leaked into stderr: %q", stderr)
	}

	assertNoTokenInTree(t, cfg.CacheDir, leakToken)
	assertNoTokenInTree(t, cfg.DownloadPath, leakToken)
	assertNoTokenInFile(t, cfg.MetricsFile, leakToken)

	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	assertNoTokenInFile(t, lockPath, leakToken)

	// The S3 backend's SaveStore marshals via the same *store.Store.
	// MarshalSnapshot this local run just persisted (see
	// internal/cache/s3/backend.go's SaveStore): loading the store this
	// install actually wrote and re-running that same marshal path asserts
	// the S3 wire payload is equally token-free, without standing up a real
	// S3 backend in this test.
	backend, err := cacheBackend.New(cfg, tokenLeakRuntime(cfg, progress.New(false, true)))
	if err != nil {
		t.Fatalf("cacheBackend.New: %v", err)
	}
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() {
		_ = backend.Close(ctx)
	}()
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	payload, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot: %v", err)
	}
	if bytes.Contains(payload, []byte(leakToken)) {
		t.Errorf("token leaked into the marshaled snapshot payload: %s", payload)
	}
}

// TestTokenNeverLeaksOnAuthFailure is TestTokenNeverLeaksDuringVerboseInstall's
// failure-path counterpart: a wrong token still must never appear in the
// error output, even though the whole point of the run is to report that
// authentication failed. Deliberately not t.Parallel(); see the doc comment
// on TestTokenNeverLeaksDuringVerboseInstall.
func TestTokenNeverLeaksOnAuthFailure(t *testing.T) {
	srv := fakegalaxy.New(t)
	srv.RequireAuth("Token correct-token-value")
	srv.AddVersion("ns", "a", "1.0.0", nil)

	cfg := tokenLeakConfig(t, srv)
	cfg.Servers[0].Token = config.NewSecret(leakToken) // deliberately wrong

	var installErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := tokenLeakRuntime(cfg, printer)
		runtime.DebugAnsibleConfig(cfg)
		runtime.WarnConfig(cfg)
		installErr = collections.Start(context.Background(), cfg, runtime)
	})
	if installErr == nil {
		t.Fatalf("expected an auth failure, got nil")
	}
	if bytes.Contains(stdout, []byte(leakToken)) {
		t.Errorf("token leaked into stdout on auth failure: %q", stdout)
	}
	if bytes.Contains(stderr, []byte(leakToken)) {
		t.Errorf("token leaked into stderr on auth failure: %q", stderr)
	}
}
