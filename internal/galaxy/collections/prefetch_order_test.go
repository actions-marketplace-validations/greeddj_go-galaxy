package collections

// This file proves the prefetch download queue is ordered by install level:
// sortTasksByLevel/buildLevelIndex in isolation (Test 1), and the wiring
// through startPrefetcher end to end, where a single worker's FIFO drain of
// the task channel makes the resulting Commit order directly observable
// (Test 2).

import (
	"context"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestSortTasksByLevelOrdersByLevelThenKey proves sortTasksByLevel orders a
// scrambled task list by ascending install level, breaking ties within a
// level by collection key, and that a key absent from levelIndex (which
// cannot happen in practice, since collections and levels always derive from
// the same graph) defaults to level 0 rather than sorting arbitrarily.
func TestSortTasksByLevelOrdersByLevelThenKey(t *testing.T) {
	t.Parallel()
	base := collection{Namespace: "acme", Name: "base", Version: "1.0.0"}
	lib1 := collection{Namespace: "acme", Name: "lib1", Version: "1.0.0"}
	lib2 := collection{Namespace: "acme", Name: "lib2", Version: "1.0.0"}
	app := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

	// lib2 is listed before lib1 within level 1 on purpose, to prove the key
	// tie-break re-sorts within a level rather than preserving input order.
	levels := [][]string{{base.key()}, {lib2.key(), lib1.key()}, {app.key()}}
	levelIndex := buildLevelIndex(levels)

	tasks := []collection{app, lib2, base, lib1}
	sortTasksByLevel(tasks, levelIndex)

	want := []string{base.key(), lib1.key(), lib2.key(), app.key()}
	got := make([]string, len(tasks))
	for i, col := range tasks {
		got[i] = col.key()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sortTasksByLevel order = %v, want %v", got, want)
	}

	for i := 0; i+1 < len(tasks); i++ {
		li, lj := levelIndex[tasks[i].key()], levelIndex[tasks[i+1].key()]
		if li > lj {
			t.Fatalf("level not ascending at index %d: %d > %d", i, li, lj)
		}
	}

	// A key absent from levelIndex must default to level 0 and sort
	// deterministically alongside the explicit level-0 entries, tie-broken by
	// key: "acme.base@1.0.0" < "acme.missing@1.0.0" < the level-2 app.
	missing := collection{Namespace: "acme", Name: "missing", Version: "1.0.0"}
	tasks2 := []collection{app, missing, base}
	sortTasksByLevel(tasks2, levelIndex)

	want2 := []string{base.key(), missing.key(), app.key()}
	got2 := make([]string, len(tasks2))
	for i, col := range tasks2 {
		got2[i] = col.key()
	}
	if !slices.Equal(got2, want2) {
		t.Fatalf("sortTasksByLevel with absent key order = %v, want %v", got2, want2)
	}
}

// waitAll blocks until every key in keys has a completed prefetch result,
// mirroring how installLevels drains the prefetcher via Wait(col.key()) for
// each collection it installs. Waiting here (rather than calling Close
// immediately) is required for a deterministic read of the commit order:
// Close cancels the prefetcher's context before joining its workers, so
// calling it before every task has actually finished races the in-flight
// downloads and can abort them with a canceled-context error instead of
// letting them complete.
func waitAll(t *testing.T, prefetch *prefetcher, keys []string) {
	t.Helper()
	for _, key := range keys {
		if _, _, ok, err := prefetch.Wait(key); !ok {
			t.Fatalf("Wait(%s): key was never registered with the prefetcher", key)
		} else if err != nil {
			t.Fatalf("Wait(%s): unexpected prefetch error: %v", key, err)
		}
	}
}

// TestPrefetchQueueOrderedByLevel proves the wiring end to end: with a single
// prefetch worker (Workers: 1), the task channel is drained strictly FIFO, so
// the order artifacts are committed to the cache reveals the exact order the
// tasks were enqueued in. A three-level dependency chain (app -> lib -> base)
// must therefore commit leaf-first: base, then lib, then app - proving the
// prefetch queue tracks the level-ordered install consumer rather than racing
// in map order.
func TestPrefetchQueueOrderedByLevel(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "base", "1.0.0", nil)
	srv.AddVersion("acme", "lib", "1.0.0", map[string]string{"acme.base": ">=1.0.0"})
	srv.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": ">=1.0.0"})

	base := collection{Namespace: "acme", Name: "base", Version: "1.0.0"}
	lib := collection{Namespace: "acme", Name: "lib", Version: "1.0.0"}
	app := collection{Namespace: "acme", Name: "app", Version: "1.0.0"}
	collections := map[string]collection{base.key(): base, lib.key(): lib, app.key(): app}
	graph := map[string][]string{
		app.key():  {lib.key()},
		lib.key():  {base.key()},
		base.key(): {},
	}

	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels: %v", err)
	}
	if len(levels) != 3 || len(levels[0]) != 1 || len(levels[1]) != 1 || len(levels[2]) != 1 ||
		levels[0][0] != base.key() || levels[1][0] != lib.key() || levels[2][0] != app.key() {
		t.Fatalf("unexpected levels, want [[base],[lib],[app]]: %#v", levels)
	}

	fx := newPrefetchHandoffFixture(t, srv, 1)
	prefetch := startPrefetcher(
		context.Background(),
		newPrefetchDeps(fx.cfg, fx.runtime, fx.st, fx.artifacts),
		collections,
		levels,
	)
	// Wait for every task to actually finish before Close: Close cancels the
	// prefetcher's context, and canceling before completion would race the
	// still-downloading tasks instead of deterministically observing them all
	// committed.
	waitAll(t, prefetch, []string{base.key(), lib.key(), app.key()})
	prefetch.Close()

	want := []string{artifactKey(base), artifactKey(lib), artifactKey(app)}
	got := fx.artifacts.commitOrderSnapshot()
	if !slices.Equal(got, want) {
		t.Fatalf("commit order = %v, want %v (leaf-first enqueue matching level order)", got, want)
	}
}
