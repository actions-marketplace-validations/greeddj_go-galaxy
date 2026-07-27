package collections

// This file closes the remaining half of a two-part contract on warmWithState's
// save/metrics tail. TestWarmPartialFailureKeepsSuccessfulCollectionCached
// (warm_e2e_test.go) already proves SaveStore itself runs despite a nonzero
// collection-failure count, and TestWarmMetricsWrittenForSuccessAndFailure's
// "failing warm" subtest (same file) already proves metrics are written on a
// failing warm; this file proves the other half - metrics are still written,
// and the save error takes precedence over any nonzero collection-failure
// count - when SaveStore itself fails. Reverting writeRunMetrics back behind
// the save-error return (undoing the fix in warmWithState) makes
// TestWarmWithStateSaveFailureWritesMetricsNoCollections fail with
// "read metrics file /...: open /...: no such file or directory", since
// readMetricsCommand's t.Fatalf on the missing file fires before the
// wrong-command t.Errorf below it is ever reached.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// errSaveFailSentinel stands in for a real backend's SaveStore failure (disk
// full, an S3 partition) that has nothing to do with whether any collection
// itself succeeded. It stays private to this file - production code never
// compares against it.
var errSaveFailSentinel = errors.New("simulated SaveStore failure")

// saveFailBackend wraps a real cacheManager.Backend - a real *local.Backend
// in every test below - and overrides only SaveStore to always fail, so
// Open/Lock/LoadStore/Artifacts/SweepTemp/RecordProject all behave exactly
// like the real backend they wrap. Same wrap-a-real-implementation shape as
// s3_cache_recovery_test.go's fetchOnceMismatchArtifacts.
type saveFailBackend struct {
	cacheManager.Backend
}

// SaveStore always fails, simulating a persistence failure at the very last
// step of a warm run.
func (saveFailBackend) SaveStore(context.Context, *store.Store) error {
	return errSaveFailSentinel
}

// readMetricsCommand reads the metrics file at path and returns its "command"
// field, failing the test on any I/O or decode error.
func readMetricsCommand(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixed cfg.MetricsFile, not user input.
	if err != nil {
		t.Fatalf("read metrics file %s: %v", path, err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("unmarshal metrics file %s: %v", path, err)
	}
	command, _ := written["command"].(string)
	return command
}

// TestWarmWithStateSaveFailureWritesMetricsNoCollections asserts that, with
// nothing to warm, a failing SaveStore still returns the save error and still
// leaves cfg.MetricsFile on disk with command == "warm": writeRunMetrics runs
// unconditionally, even on a save failure, and is not gated behind whether the
// save succeeded.
func TestWarmWithStateSaveFailureWritesMetricsNoCollections(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	metricsPath := filepath.Join(root, "metrics.json")

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		MetricsFile:      metricsPath,
		Workers:          1,
		Offline:          true,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	err := warmWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}

	if got := readMetricsCommand(t, metricsPath); got != "warm" {
		t.Errorf("metrics file not written despite the save failure: command = %q, want %q", got, "warm")
	}
}

// TestWarmWithStateSaveFailureWinsOverCollectionFailures asserts that when
// both a collection's artifact download fails and SaveStore itself fails,
// warmWithState's returned error is the save error, not ErrInstallationFailed -
// a save failure takes precedence over a nonzero collection-failure count,
// exactly like runInstall's finalizeInstall precedence.
func TestWarmWithStateSaveFailureWinsOverCollectionFailures(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)
	srv.Fail(fakegalaxy.EndpointArtifact, "acme", "widgets", fakegalaxy.Fault{Status: http.StatusServiceUnavailable, Count: -1})

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		Workers:          1,
	}
	state := newSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := warmWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, errSaveFailSentinel) {
		t.Fatalf("expected errors.Is errSaveFailSentinel, got %v", err)
	}
	if errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected the save error to win, but errors.Is ErrInstallationFailed matched too: %v", err)
	}
}

// newSaveFailState builds an installState wired to a saveFailBackend over a
// real *local.Backend rooted at cfg.CacheDir, opened and loaded exactly like
// initInstall would, but without ever taking the backend's lock: warmWithState
// never touches state.release or calls backend.Close itself, so this test
// helper does not need to provide either.
func newSaveFailState(t *testing.T, cfg *config.Config) *installState {
	t.Helper()
	backend := saveFailBackend{Backend: local.New(cfg.CacheDir)}
	ctx := context.Background()
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close(ctx) })
	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return &installState{
		backend:      backend,
		store:        st,
		extractStore: newExtractStore(cfg),
	}
}
