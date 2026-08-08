package collections

// This file pins the carrier half of internal/cache/s3's cache-backend
// classification at this package's own seam: a SaveStore failure that
// carries helpers.ErrCacheBackendUnavailable must survive
// installWithState's failure-classification logic exactly like any other
// SaveStore failure does - bare on a clean run, joined behind
// helpers.ErrInstallationFailed on a run that also recorded a per-collection
// failure - mirroring save_failure_test.go's own coverage of
// errSaveFailSentinel, but asserting on the real sentinel
// cmd/go-galaxy/exitcode branches on instead of an opaque stand-in.
//
// The LoadStore half of the same classification is deliberately NOT
// exercised end to end here: initInstall builds its own backend via
// internal/cache/cache.New and is not injectable, and internal/cache/s3's
// fakeS3 harness is unexported, unreachable from this package - the same
// limitation internal/cache/s3/state_deadline_test.go records for its own
// fixture. What IS pinned, and how each piece covers a distinct hop: the
// producer hop (internal/cache/s3/cache_backend_classification_test.go,
// which proves a dead bucket's GET/connection failure actually produces
// helpers.ErrCacheBackendUnavailable) and the classifier hop
// (cmd/go-galaxy/exitcode/exitcode_test.go's "cache backend unavailable"
// row, which proves that sentinel maps to ExitNetwork) are both pinned
// directly, and this file pins the carrier hop directly too, but only for
// SaveStore. For LoadStore, the carrier hop is not directly exercised; the
// inference that it behaves the same is sound rather than assumed, because
// initInstall returns backend.LoadStore's error bare - `st, err :=
// backend.LoadStore(lockCtx); if err != nil { return lockCtx, nil, err }`,
// its only LoadStore call - sharing no wrapping code with the SaveStore path
// this file does exercise. A bare return has no logic left to regress
// independently of what this file already pins for SaveStore's own
// bare-return arm.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// errCacheBackendUnavailableSaveFailure stands in for the shape a dead S3
// bucket's SaveStore failure takes: internal/cache/s3's errS3PutFailed (and
// every other status/transport failure in that package) wraps
// helpers.ErrCacheBackendUnavailable, so this local fixture reproduces that
// wrap directly rather than standing up a real S3 backend, which this
// package cannot reach (see this file's header comment).
var errCacheBackendUnavailableSaveFailure = fmt.Errorf("%w: simulated dead bucket", helpers.ErrCacheBackendUnavailable)

// cacheBackendUnavailableSaveFailBackend wraps a real cacheManager.Backend -
// a real *local.Backend, via newLocalState - and overrides only SaveStore to
// always fail with errCacheBackendUnavailableSaveFailure, the identical
// wrap-a-real-implementation shape save_failure_test.go's saveFailBackend
// uses.
type cacheBackendUnavailableSaveFailBackend struct {
	cacheManager.Backend
}

// SaveStore always fails with the cache-backend-unavailable shape,
// simulating a dead S3 bucket at the very last step of a run.
func (cacheBackendUnavailableSaveFailBackend) SaveStore(context.Context, *store.Store) error {
	return errCacheBackendUnavailableSaveFailure
}

// newCacheBackendUnavailableSaveFailState is newLocalState with its backend
// wrapped in cacheBackendUnavailableSaveFailBackend, so SaveStore always
// fails with the cache-backend-unavailable shape while everything else -
// Open, LoadStore, Artifacts, SweepTemp, RecordProject - behaves like the
// real local backend it wraps.
func newCacheBackendUnavailableSaveFailState(t *testing.T, cfg *config.Config) *installState {
	t.Helper()
	state := newLocalState(t, cfg)
	state.backend = cacheBackendUnavailableSaveFailBackend{Backend: state.backend}
	return state
}

// TestInstallWithStateCacheBackendUnavailableSaveFailureCarriesSentinelOnCleanRun
// asserts that on a run with zero collection failures, a SaveStore failure
// carrying helpers.ErrCacheBackendUnavailable reaches installWithState's
// caller with that sentinel still matchable via errors.Is - the tail
// SaveStore call in finalizeInstall returns the save error bare in this
// case, since annotateSaveFailure is only reached when the run also
// recorded at least one per-collection failure.
func TestInstallWithStateCacheBackendUnavailableSaveFailureCarriesSentinelOnCleanRun(t *testing.T) {
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
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
		Offline:          true,
	}
	state := newCacheBackendUnavailableSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	err := installWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is helpers.ErrCacheBackendUnavailable, got %v", err)
	}
	// Contrast with the joined-run test below: a clean run's tail save
	// failure is never folded behind helpers.ErrInstallationFailed, since
	// annotateSaveFailure only runs when summary.count > 0.
	if errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected a clean run's bare save failure NOT to carry helpers.ErrInstallationFailed, got %v", err)
	}
}

// TestInstallWithStateCacheBackendUnavailableSaveFailureJoinsBehindInstallationFailed
// asserts that on a run which also recorded a per-collection failure, the
// same SaveStore failure is folded in via annotateSaveFailure behind
// helpers.ErrInstallationFailed, while helpers.ErrCacheBackendUnavailable
// stays reachable through the same joined tree - the shape
// cmd/go-galaxy/exitcode's isInstallError/isFileIntegrityError checks depend
// on to still classify this run as ExitInstall rather than losing the
// classification to the save error's own (otherwise unclassified within
// isInstallError) network-class sentinel.
func TestInstallWithStateCacheBackendUnavailableSaveFailureJoinsBehindInstallationFailed(t *testing.T) {
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
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}
	state := newCacheBackendUnavailableSaveFailState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := installWithState(context.Background(), cfg, runtime, state, time.Now())
	if !errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is helpers.ErrInstallationFailed, got %v", err)
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is helpers.ErrCacheBackendUnavailable, got %v", err)
	}
}
