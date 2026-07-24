package collections

// This file proves runInstallLevel's fix for the ErrMissingCollection early
// return: that guard used to return from installLevels' per-level dispatch
// loop without joining workers already dispatched for earlier keys in the
// same level, leaking them (they still hold a sem slot, mutate the shared
// Store, and can outlive runInstall's backend-lock release) and racing the
// non-atomic failures read against a worker's atomic.AddInt32. Both tests
// drive installLevels directly, since the guard and the join it must now
// wait for are both internal to it.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// TestInstallLevelsMissingCollectionSurfaces proves installLevels still
// surfaces helpers.ErrMissingCollection after the runInstallLevel extraction:
// a single-key level whose key is absent from the collections map must still
// fail the run. The missing key is the level's only key, so no worker is ever
// dispatched - this pins the error-propagation path itself, independent of
// the join fix covered by TestInstallLevelsJoinsInFlightWorkerOnMissingCollection.
func TestInstallLevelsMissingCollectionSurfaces(t *testing.T) {
	t.Parallel()

	const missingKey = "acme.missing@1.0.0"
	cfg := &config.Config{Offline: true, DownloadPath: t.TempDir(), Workers: 1}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()
	// Wait(missingKey) is never reached (the guard trips before any dispatch),
	// so a bare prefetcher with only its done map allocated is enough.
	prefetch := &prefetcher{done: make(map[string]chan struct{})}

	levels := [][]string{{missingKey}}
	_, err := installLevels(
		context.Background(),
		cfg,
		runtime,
		st,
		nil,
		nil,
		map[string]collection{},
		map[string][]string{},
		levels,
		prefetch,
	)
	if !errors.Is(err, helpers.ErrMissingCollection) {
		t.Fatalf("err = %v, want errors.Is helpers.ErrMissingCollection", err)
	}
}

// TestInstallLevelsJoinsInFlightWorkerOnMissingCollection is the load-bearing
// proof for the leak/race fix. The level has two keys: key1 (present in
// collections, dispatched first) and a missing key (absent, trips the guard
// second). key1's worker is parked in prefetch.Wait(key1) on a hand-built
// prefetcher whose done channel for key1 is left open, so it cannot complete
// until the test releases it.
//
// Discrimination: the pre-fix installLevels returned from its dispatch loop
// the moment the guard tripped, without ever calling wg.Wait() for that
// level - so it would return almost immediately while key1's worker was still
// parked. The fixed runInstallLevel registers `defer wg.Wait()` before its
// dispatch loop, so the guard's return statement cannot actually hand control
// back to installLevels until every dispatched worker in the level -
// including the still-parked key1 - has finished. The first select below
// asserts exactly that: on fixed code it must time out (installLevels is
// still blocked inside the deferred wg.Wait()); on the pre-fix code it would
// instead receive from done and fail the test immediately.
func TestInstallLevelsJoinsInFlightWorkerOnMissingCollection(t *testing.T) {
	t.Parallel()

	col1 := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	key1 := col1.key()
	const missingKey = "acme.missing@1.0.0"

	collections := map[string]collection{key1: col1}
	graph := map[string][]string{key1: {}}
	levels := [][]string{{key1, missingKey}}

	// p is built by hand with key1 registered and its done channel left open:
	// prefetch.Wait(key1) blocks until p.finish(key1, ...) closes it below.
	p := &prefetcher{
		meta:       make(map[string]*types.GalaxyCollectionVersionInfo),
		errs:       make(map[string]error),
		prefetched: make(map[string]downloadResult),
		done:       map[string]chan struct{}{key1: make(chan struct{})},
	}

	cfg := &config.Config{Offline: true, DownloadPath: t.TempDir(), Workers: 2}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()

	done := make(chan error, 1)
	go func() {
		_, err := installLevels(
			context.Background(),
			cfg,
			runtime,
			st,
			nil,
			nil,
			collections,
			graph,
			levels,
			p,
		)
		done <- err
	}()

	// release unblocks key1's worker. Wrapped in sync.OnceFunc and registered
	// via t.Cleanup so it still fires (and the goroutine above still
	// terminates) even if an assertion below fails first. Once unblocked,
	// key1's installCollection reaches fetchArtifact with cacheHit false and
	// cfg.Offline true, so it fails fast on helpers.ErrOfflineMode without
	// touching the network - the worker completes quickly either way.
	release := sync.OnceFunc(func() {
		p.finish(key1, &types.GalaxyCollectionVersionInfo{}, downloadResult{}, nil)
	})
	t.Cleanup(release)

	select {
	case err := <-done:
		t.Fatalf("installLevels returned without joining its in-flight worker: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Expected: the fix keeps installLevels blocked in runInstallLevel's
		// deferred wg.Wait() until key1's worker is released below.
	}

	release()

	select {
	case err := <-done:
		if !errors.Is(err, helpers.ErrMissingCollection) {
			t.Fatalf("err = %v, want errors.Is helpers.ErrMissingCollection", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("installLevels did not return after its in-flight worker was released")
	}
}
