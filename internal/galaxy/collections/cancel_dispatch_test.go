package collections

// This file proves that runInstallLevel and warmCollections both stop
// dispatching further collections onto their worker pool once the run's
// context is canceled - a collection not yet started never begins, while
// every worker already started still runs to completion and is still
// joined (runInstallLevel via its deferred wg.Wait, warmCollections via its
// inline one) - and that neither loop turns cancellation into an error:
// installLevels/warmWithState both still reach their own tail
// (finalizeInstall, or the snapshot save and metrics report) for an
// interrupted run exactly as they do for one that finished on its own.
//
// Each "canceled" subtest below is paired with a "live" positive control on
// the identical fixture, run with context.Background() instead of a
// pre-canceled context: without it, a canceled subtest that recorded zero
// failures would be indistinguishable from a fixture that could never
// record a failure in the first place.

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

// newCancelDispatchInstallFixture builds an installDeps and a three-key
// level for TestRunInstallLevelStopsDispatchingAfterCancel, mirroring
// TestRunInstallLevelZeroWorkersDoesNotDeadlock's own fixture: root is nil,
// so installCollection fails fast through newInstallTarget's own nil-root
// guard instead of reaching the network, which is what makes every
// collection's failure deterministic and offline.
func newCancelDispatchInstallFixture(
	t *testing.T,
) (installDeps, map[string]collection, map[string][]string, []string, *capturingPrinter) {
	t.Helper()
	cfg := &config.Config{Workers: 1, Offline: true}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	deps := newInstallDeps(cfg, runtime, store.New(), nil, nil, nil, nil)
	collections := map[string]collection{
		"acme.alpha@1.0.0": {Namespace: "acme", Name: "alpha", Version: "1.0.0"},
		"acme.beta@1.0.0":  {Namespace: "acme", Name: "beta", Version: "1.0.0"},
		"acme.gamma@1.0.0": {Namespace: "acme", Name: "gamma", Version: "1.0.0"},
	}
	graph := map[string][]string{}
	level := []string{"acme.alpha@1.0.0", "acme.beta@1.0.0", "acme.gamma@1.0.0"}
	return deps, collections, graph, level, printer
}

// waitRunInstallLevel blocks until done delivers runInstallLevel's result or
// zeroWorkersDeadlockTimeout elapses, failing the test on either a timeout or
// a non-nil result. Factored out of the two subtests below purely to keep
// each of their own cyclomatic complexity low; the wait itself is identical
// in both.
func waitRunInstallLevel(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runInstallLevel returned %v, want nil", err)
		}
	case <-time.After(zeroWorkersDeadlockTimeout):
		t.Fatal("runInstallLevel did not return before the timeout")
	}
}

// TestRunInstallLevelStopsDispatchingAfterCancel proves runInstallLevel's
// dispatch loop, over a three-key level, stops handing out further keys once
// the run's context is canceled.
func TestRunInstallLevelStopsDispatchingAfterCancel(t *testing.T) {
	t.Parallel()

	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		deps, collections, graph, level, printer := newCancelDispatchInstallFixture(t)
		var failures failureRecorder
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan error, 1)
		go func() {
			done <- runInstallLevel(ctx, deps, collections, graph, level, &prefetcher{}, &failures)
		}()
		waitRunInstallLevel(t, done)

		// Killing mutation: removing the "if ctx.Err() != nil { break }" check
		// (start.go, top of runInstallLevel's dispatch loop) makes `go test
		// -run TestRunInstallLevelStopsDispatchingAfterCancel/canceled -v`
		// fail with the real observed output:
		//   failures.count() = 3, want 0: a canceled run must dispatch no collection
		// (every collection in the level is now dispatched despite the
		// canceled context, and each fails through the fixture's nil-root
		// guard, recording three failures instead of zero)
		if got := failures.count(); got != 0 {
			t.Fatalf("failures.count() = %d, want 0: a canceled run must dispatch no collection", got)
		}
		if printer.hasErrContaining("Failed: ") {
			t.Fatalf("printer recorded a \"Failed: \" line after cancellation: %v", printer.errs)
		}
	})

	// live is the mandatory positive control on the identical fixture, run
	// with context.Background(): it proves the fixture can produce three
	// failures at all, which is what makes the canceled subtest's zero above
	// mean "dispatch stopped" rather than "this fixture never fails".
	t.Run("live", func(t *testing.T) {
		t.Parallel()
		deps, collections, graph, level, printer := newCancelDispatchInstallFixture(t)
		var failures failureRecorder

		done := make(chan error, 1)
		go func() {
			done <- runInstallLevel(context.Background(), deps, collections, graph, level, &prefetcher{}, &failures)
		}()
		waitRunInstallLevel(t, done)

		if got := failures.count(); got != 3 {
			t.Fatalf(
				"failures.count() = %d, want 3: every collection in the level must fail through the fixture's nil-root guard",
				got,
			)
		}
		if !printer.hasErrContaining("Failed: ") {
			t.Fatalf("expected a \"Failed: \" line from the live control, got %v", printer.errs)
		}
	})
}

// newCancelDispatchWarmFixture builds a cfg/runtime/state and a three-key
// collections map for TestWarmCollectionsStopsDispatchingAfterCancel,
// mirroring TestWarmCollectionsZeroWorkersDoesNotDeadlock's own fixture: no
// server is configured and Offline is set, so warmOne's metadata resolve
// fails deterministically through loadRootMetadataCached's empty
// server-candidate list, with no network I/O.
func newCancelDispatchWarmFixture(
	t *testing.T,
) (*config.Config, *infra.Infra, *installState, map[string]collection, *capturingPrinter) {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	cfg := &config.Config{CacheDir: cacheDir, Workers: 1, Offline: true}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	state := &installState{backend: local.New(cacheDir), store: store.New()}
	collections := map[string]collection{
		"acme.alpha@1.0.0": {Namespace: "acme", Name: "alpha", Version: "1.0.0"},
		"acme.beta@1.0.0":  {Namespace: "acme", Name: "beta", Version: "1.0.0"},
		"acme.gamma@1.0.0": {Namespace: "acme", Name: "gamma", Version: "1.0.0"},
	}
	return cfg, runtime, state, collections, printer
}

// waitWarmCollections blocks until done delivers warmCollections's result or
// zeroWorkersDeadlockTimeout elapses, failing the test on a timeout.
// warmCollections returns no error - only a failureSummary - so unlike
// waitRunInstallLevel there is nothing to check beyond arrival; the summary
// itself is returned for the caller's own assertions.
func waitWarmCollections(t *testing.T, done <-chan failureSummary) failureSummary {
	t.Helper()
	select {
	case summary := <-done:
		return summary
	case <-time.After(zeroWorkersDeadlockTimeout):
		t.Fatal("warmCollections did not return before the timeout")
		return failureSummary{}
	}
}

// TestWarmCollectionsStopsDispatchingAfterCancel is warm's mirror of
// TestRunInstallLevelStopsDispatchingAfterCancel: warmCollections must stop
// dispatching once ctx is canceled, and still return a failureSummary
// through its own inline wg.Wait rather than an error derived from
// ctx.Err().
func TestWarmCollectionsStopsDispatchingAfterCancel(t *testing.T) {
	t.Parallel()

	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, state, collections, printer := newCancelDispatchWarmFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan failureSummary, 1)
		go func() {
			done <- warmCollections(ctx, cfg, runtime, state, collections)
		}()
		summary := waitWarmCollections(t, done)

		// Killing mutation: removing the "if ctx.Err() != nil { break }" check
		// (start.go, top of warmCollections's dispatch loop) makes `go test
		// -run TestWarmCollectionsStopsDispatchingAfterCancel/canceled -v`
		// fail with the real observed output:
		//   summary.count = 3, want 0: a canceled run must dispatch no collection
		// (every collection is now dispatched despite the canceled context,
		// and each fails through the fixture's empty server-candidate list,
		// recording three failures instead of zero)
		if summary.count != 0 {
			t.Fatalf("summary.count = %d, want 0: a canceled run must dispatch no collection", summary.count)
		}
		if printer.hasErrContaining("Failed: ") {
			t.Fatalf("printer recorded a \"Failed: \" line after cancellation: %v", printer.errs)
		}
	})

	// live is the mandatory positive control on the identical fixture, run
	// with context.Background(): it proves the fixture can produce three
	// failures at all, which is what makes the canceled subtest's zero above
	// mean "dispatch stopped" rather than "this fixture never fails".
	t.Run("live", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, state, collections, printer := newCancelDispatchWarmFixture(t)

		done := make(chan failureSummary, 1)
		go func() {
			done <- warmCollections(context.Background(), cfg, runtime, state, collections)
		}()
		summary := waitWarmCollections(t, done)

		if summary.count != 3 {
			t.Fatalf(
				"summary.count = %d, want 3: every collection must fail through the fixture's empty server-candidate list",
				summary.count,
			)
		}
		if !printer.hasErrContaining("Failed: ") {
			t.Fatalf("expected a \"Failed: \" line from the live control, got %v", printer.errs)
		}
	})
}
