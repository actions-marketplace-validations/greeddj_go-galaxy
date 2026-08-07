package collections

// This file proves cfg.Workers == 0 cannot deadlock either per-collection
// dispatch loop that bounds its worker pool with a buffered-channel
// semaphore sized from cfg.Workers: warmCollections and runInstallLevel. Both
// guard with max(_, 1) (see start.go); without that guard, a zero Workers
// makes the semaphore channel unbuffered, and the loop's first send blocks
// forever because no worker has started yet to drain it - nothing reachable
// from the CLI can set Workers to 0 (newConfigFromCLI clamps it to NumCPU),
// but a config.Config built programmatically can.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// zeroWorkersDeadlockTimeout bounds how long these tests wait for a
// dispatch loop to return before concluding it has deadlocked. Generous
// enough that a slow CI runner never trips it on a working implementation,
// short enough that a real deadlock still fails the test in seconds rather
// than hanging the whole suite until the package-level test timeout.
const zeroWorkersDeadlockTimeout = 5 * time.Second

// TestWarmCollectionsZeroWorkersDoesNotDeadlock proves warmCollections
// completes (rather than hanging forever on its semaphore's first send) when
// cfg.Workers is 0.
func TestWarmCollectionsZeroWorkersDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	cfg := &config.Config{CacheDir: cacheDir, Workers: 0, Offline: true}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	state := &installState{
		backend: local.New(cacheDir),
		store:   store.New(),
	}
	collections := map[string]collection{
		"acme.widgets@1.0.0": {Namespace: "acme", Name: "widgets", Version: "1.0.0"},
	}

	done := make(chan failureSummary, 1)
	go func() {
		done <- warmCollections(context.Background(), cfg, runtime, state, collections)
	}()

	select {
	case <-done:
		// Returned - Workers == 0 was treated as at least one worker.
	case <-time.After(zeroWorkersDeadlockTimeout):
		t.Fatal("warmCollections(Workers: 0) did not return: the semaphore channel was never buffered, " +
			"so its first send blocked forever with no worker started yet to drain it")
	}
}

// TestRunInstallLevelZeroWorkersDoesNotDeadlock proves runInstallLevel
// completes when cfg.Workers is 0, mirroring
// TestWarmCollectionsZeroWorkersDoesNotDeadlock for install's own identical
// hazard.
func TestRunInstallLevelZeroWorkersDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workers: 0, Offline: true}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	// root is nil: this test only proves the semaphore does not deadlock, not
	// that the (uncreated) collections tree is written to - a nil root makes
	// installCollection fail fast via newInstallTarget's own guard instead,
	// which is still a prompt return, not a deadlock.
	deps := newInstallDeps(cfg, runtime, store.New(), nil, nil, nil, nil)
	collections := map[string]collection{
		"acme.widgets@1.0.0": {Namespace: "acme", Name: "widgets", Version: "1.0.0"},
	}
	graph := map[string][]string{}
	level := []string{"acme.widgets@1.0.0"}
	var failures failureRecorder

	done := make(chan error, 1)
	go func() {
		done <- runInstallLevel(context.Background(), deps, collections, graph, level, &prefetcher{}, &failures)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runInstallLevel: %v", err)
		}
	case <-time.After(zeroWorkersDeadlockTimeout):
		t.Fatal("runInstallLevel(Workers: 0) did not return: the semaphore channel was never buffered, " +
			"so its first send blocked forever with no worker started yet to drain it")
	}
}
