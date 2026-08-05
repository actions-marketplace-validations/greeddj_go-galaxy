package cleanup

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// TestCleanupStopsRemovingWhenTheContextEnds pins the guarantee runCleanup's
// own doc comment states: once this run stops owning the cache, the removal
// loop stops at the next collection rather than deleting the rest of them.
// The context it stops on is the holder context - the same one an S3
// heartbeat cancels after another acquirer takes the lock away - which the
// local backend hands back unchanged, so a caller-canceled context reaches
// removeUnused by exactly the path a stolen lock would.
//
// Two collections are seeded, both unreferenced and therefore both removal
// candidates, and the sorted iteration order makes the first of them the one
// the loop would reach first: a check placed after the reachable skip instead
// of before it would still stop here, but a check placed after the removal
// would not, and the surviving first collection is what separates them.
//
// The live-context row is the mandatory positive control on the identical
// fixture: it must remove both, which is what makes "nothing was removed"
// mean the loop stopped rather than that the fixture never had anything to
// remove.
func TestCleanupStopsRemovingWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		canceled bool
	}{
		{name: "a canceled run removes nothing", canceled: true},
		{name: "a live run removes both", canceled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cacheDir := t.TempDir()
			downloadPath := t.TempDir()
			seedManifestAt(t, downloadPath, "ns", "first", "1.0.0")
			seedManifestAt(t, downloadPath, "ns", "second", "1.0.0")
			registerCleanupProject(t, cacheDir, downloadPath)

			cfg := &config.Config{CacheDir: cacheDir}
			runtime := infra.New(&recordingPrinter{}, http.DefaultClient)
			err := Start(cleanupTestContext(t, tc.canceled), cfg, runtime)

			assertCleanupRemoval(t, err, downloadPath, tc.canceled)
		})
	}
}

// cleanupTestContext returns either an already-canceled context or the test's
// own live one.
func cleanupTestContext(t *testing.T, canceled bool) context.Context {
	t.Helper()
	if !canceled {
		return t.Context()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// assertCleanupRemoval checks the outcome of one row of
// TestCleanupStopsRemovingWhenTheContextEnds: a canceled run must report the
// cancellation, classify as an interrupt rather than a lock-loss, and leave
// both trees alone; a live run must report nothing and remove both.
func assertCleanupRemoval(t *testing.T, err error, downloadPath string, canceled bool) {
	t.Helper()
	first := filepath.Join(downloadPath, "ansible_collections", "ns", "first", "MANIFEST.json")
	second := filepath.Join(downloadPath, "ansible_collections", "ns", "second", "MANIFEST.json")

	if !canceled {
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		for _, path := range []string{first, second} {
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("expected %s to be removed by a live run, stat error: %v", path, statErr)
			}
		}
		return
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want errors.Is context.Canceled", err)
	}
	// The cancellation is propagated with %w rather than flattened, and this
	// is the assertion that keeps it that way: an operator's Ctrl-C during
	// cleanup must stay an interrupt. Only a genuine lock-loss cause turns
	// into exit 8, and this run's context carries no such cause.
	if got := exitcode.FromError(err); got != exitcode.ExitInterrupt {
		t.Fatalf("exitcode.FromError = %d, want ExitInterrupt (%d)", got, exitcode.ExitInterrupt)
	}
	if errors.Is(err, helpers.ErrCacheLockLost) {
		t.Fatalf("Start = %v, a plain cancellation must not read as a lost lock", err)
	}
	for _, path := range []string{first, second} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("expected %s to survive a canceled run, stat error: %v", path, statErr)
		}
	}
}

// TestSweepLegacyArtifactsStopsWhenTheContextEnds covers the second of the
// three per-iteration stop points runCleanup's doc states, and covers it
// directly, because no end-to-end run can reach it with a context that ends
// in between: removeUnused returns first on a context canceled before the
// run, and a context that ends mid-run needs a real S3 heartbeat. Calling the
// pass itself is what makes the granularity
// ("the next legacy artifact key") testable at all.
//
// The live-context row is the positive control: the same seeded artifact must
// actually be swept, so "it survived" means the pass stopped rather than that
// it never had a candidate.
func TestSweepLegacyArtifactsStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		canceled bool
	}{
		{name: "a canceled sweep purges nothing", canceled: true},
		{name: "a live sweep purges the legacy key", canceled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cacheDir := t.TempDir()
			cfg := &config.Config{CacheDir: cacheDir}
			runtime := newTestRuntime()
			backend := openTestBackend(t, cfg, runtime)

			legacyPath := filepath.Join(cacheDir, legacyArtifactKey("ns", "name", "1.0.0"))
			if err := os.WriteFile(legacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
				t.Fatalf("seed legacy artifact: %v", err)
			}
			installedByKey := map[string][]installedCollection{
				"ns.name@1.0.0": {{Namespace: "ns", Name: "name", Version: "1.0.0"}},
			}

			sweepLegacyArtifacts(cleanupTestContext(t, tc.canceled), cfg, runtime, backend, installedByKey)

			_, statErr := os.Stat(legacyPath)
			if tc.canceled && statErr != nil {
				t.Fatalf("expected the legacy artifact to survive a canceled sweep, stat error: %v", statErr)
			}
			if !tc.canceled && !os.IsNotExist(statErr) {
				t.Fatalf("expected the legacy artifact to be swept by a live sweep, stat error: %v", statErr)
			}
		})
	}
}

// openTestBackend builds and opens a real local backend for cfg, closing it
// when the test ends. Callers need one only for its ArtifactStore.
func openTestBackend(t *testing.T, cfg *config.Config, runtime *infra.Infra) cacheManager.Backend {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("build backend: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("open backend: %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(context.Background()); err != nil {
			t.Errorf("close backend: %v", err)
		}
	})
	return backend
}
