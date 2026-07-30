package collections_test

// This file covers helpers.MetadataFetchDeadline end to end through
// collections.Start: a byte-drip fault on a fake server's root-metadata
// endpoint (fakegalaxy.Fault.DripInterval) always makes genuine progress and
// so never trips the read-inactivity watchdog, and is caught by the
// whole-request metadata fetch deadline instead. Modeled on
// deadline_e2e_test.go's newDripFixture, but for the metadata surface
// instead of the artifact one: the watchdog's own idle window
// (dripWatchdogTimeout, shared with that file) is set deliberately far above
// the metadata deadline used here, so only the deadline can be the reason a
// drip-faulted run ends.
//
// Each test below was verified against a real revert of the production
// change it pins, and this comment quotes the actual observed output:
//
//   - TestMetadataByteDripFailsTheRunAtTheMetadataDeadline, reverting
//     fetchJSONBody to pass ctx straight through (dropping the
//     context.WithTimeout and both deadlineError calls, the same production
//     change internal/galaxy/cache's own TestMetadataByteDripFailsAtTheFetchDeadline
//     pins in isolation), hangs rather than fails - the run never returns,
//     confirmed by running with a bounded -timeout so the harness kills it
//     instead of blocking the suite forever:
//     "panic: test timed out after 5s
//     running tests:
//     TestMetadataByteDripFailsTheRunAtTheMetadataDeadline (5s)"
//     with the stuck goroutine's frame at
//     "github.com/greeddj/go-galaxy/internal/galaxy/cache.fetchJSONBodyOnce(...)"
//     reading the never-ending drip body.
//   - TestRootMetadataDeadlineAbortsTheServerWalk, changing
//     loadRootMetadataCached's non-404 arm from "return nil, "", err" to
//     "lastErr = err; continue" (advancing the walk instead of aborting it),
//     makes the run succeed instead of failing - server b answers and the
//     collection installs - observed as:
//     "expected an error, got nil"
//
// TestMetadataByteDripFailsTheRunAtTheMetadataDeadline's !errors.Is(err,
// context.Canceled) and !errors.Is(err, helpers.ErrReadStalled) assertions
// are DOCUMENTARY, not independently pinned, mirroring
// deadline_e2e_test.go's identical disclosure for the artifact-deadline
// case: this fixture fails resolution with a single cause (no per-collection
// join happens before resolution even starts), and that cause carries either
// the deadline signature or the stall signature, never both - a mutation
// that made the watchdog win instead of the deadline would already fail the
// errors.Is(err, helpers.ErrMetadataFetchDeadline) assertion above these two,
// so these are a tripwire on the fixture's own parameters (the watchdog
// window staying far above the budget), not on production.
//
// TestRootMetadataDeadlineAbortsTheServerWalk's srvB.Total() == 0 assertion
// is DOCUMENTARY too, for a different, structural reason: in this fixture
// (server b always has ns.x registered), no mutation of the walk can make
// that assertion the first one to fail. For it to fail first, the run would
// need to return a non-nil error carrying helpers.ErrMetadataFetchDeadline
// while server b was nonetheless consulted - and with the walk's non-404 arm
// changed to advance instead of abort, that combination is unreachable: the
// deadline error is not a *cacheManager.HTTPStatusError, so
// loadRootMetadataCached's lastErr (which only ever accumulates a 404) never
// holds it, and whichever of the two outcomes server b then produces wins
// outright - either it answers (a nil overall error, which fails the earlier
// err == nil check first, the actual mutation result observed and quoted
// above) or, in a fixture where it lacked the collection, it would 404 (a
// *cacheManager.HTTPStatusError, which fails the errors.Is
// helpers.ErrMetadataFetchDeadline check first instead). What the assertion
// does buy: a tripwire on the FIXTURE, not on production - it fires if a
// future edit registers ns.x on server a too, arms a second fault, or
// otherwise breaks the mutual exclusion this two-server setup relies on.
// TestRootMetadataServerWalkPositiveControl is what proves the fixture can
// reach server b at all, the same role a positive control plays throughout
// this file.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// metadataDripBudget is the deps.runtime.MetadataFetchDeadline override
// every fixture in this file uses.
const metadataDripBudget = 500 * time.Millisecond

// newMetadataDripFixture registers a single acme.solo collection (version
// "*", no dependencies) on a fresh fake server and returns a matching
// config/runtime pair, with the HTTP client's own watchdog window fixed at
// dripWatchdogTimeout (see deadline_e2e_test.go), deliberately far above
// metadataDripBudget, so only the metadata deadline can be the reason a
// drip-faulted run ends.
func newMetadataDripFixture(t *testing.T, deadline time.Duration) (*config.Config, *infra.Infra, *fakegalaxy.Server) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.solo")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "solo", "1.0.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          1,
		Timeout:          dripWatchdogTimeout,
	}
	runtime := infra.New(noopPrinter{}, fetch.New(cfg.Timeout, nil))
	runtime.MetadataFetchDeadline = deadline
	return cfg, runtime, s
}

// TestMetadataByteDripFailsTheRunAtTheMetadataDeadline asserts a byte-drip
// fault on the root-metadata endpoint fails the whole run with
// helpers.ErrMetadataFetchDeadline, never joined behind
// helpers.ErrInstallationFailed (this is a resolve-time failure, before any
// collection-level install work starts), and installs nothing.
func TestMetadataByteDripFailsTheRunAtTheMetadataDeadline(t *testing.T) {
	t.Parallel()
	cfg, runtime, s := newMetadataDripFixture(t, metadataDripBudget)
	s.Fail(fakegalaxy.EndpointRootMetadata, "acme", "solo", fakegalaxy.Fault{DripInterval: 5 * time.Millisecond, Count: -1})

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error from a byte-dripped metadata response, got nil")
	}
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("expected errors.Is ErrMetadataFetchDeadline, got %v", err)
	}
	if errors.Is(err, helpers.ErrInstallationFailed) {
		t.Fatalf("a resolve-time metadata deadline must not classify as a per-collection install failure: %v", err)
	}
	// DOCUMENTARY, not independently falsifiable in this fixture - see the
	// file header for why.
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
	if errors.Is(err, helpers.ErrReadStalled) {
		t.Fatalf("expected the drip to defeat the watchdog, not trip it: %v", err)
	}

	assertPathAbsent(t, installPathFor(cfg.DownloadPath, "solo"))
}

// TestMetadataDripFixtureInstallsWithoutTheFault is the positive control for
// TestMetadataByteDripFailsTheRunAtTheMetadataDeadline: the identical
// fixture with no fault armed installs successfully under the same budget.
func TestMetadataDripFixtureInstallsWithoutTheFault(t *testing.T) {
	t.Parallel()
	cfg, runtime, _ := newMetadataDripFixture(t, metadataDripBudget)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, cfg.DownloadPath, "solo")
}

// TestRootMetadataDeadlineAbortsTheServerWalk pins that the API-root/server-
// walk multiplier for a metadata deadline is 1, not the candidate count: two
// servers are configured in order, the collection exists only on the
// second, and a drip is armed on the first server's root metadata. The run
// must fail with helpers.ErrMetadataFetchDeadline - a deadline is not a
// *cacheManager.HTTPStatusError, so loadRootMetadataCached's non-404 arm
// aborts the whole walk instead of advancing it. The srvB.Total() == 0
// assertion below is this test's fixture-level corroboration of that same
// claim, not an independent pin in its own right - see the file header for
// why it is documentary.
func TestRootMetadataDeadlineAbortsTheServerWalk(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)
	srvA.Fail(fakegalaxy.EndpointRootMetadata, "", "", fakegalaxy.Fault{DripInterval: 5 * time.Millisecond, Count: -1})

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)
	runtime.MetadataFetchDeadline = metadataDripBudget

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("expected errors.Is ErrMetadataFetchDeadline, got %v", err)
	}
	// DOCUMENTARY, not independently falsifiable in this fixture - see the
	// file header for why.
	if got := srvB.Total(); got != 0 {
		t.Fatalf("srvB.Total() = %d, want 0 (the deadline must abort the walk, not advance it)", got)
	}
}

// TestRootMetadataServerWalkPositiveControl is
// TestRootMetadataDeadlineAbortsTheServerWalk's mandatory positive control:
// the identical two-server setup with no fault armed, where server a 404s
// (it never registered ns.x) and server b serves the collection, asserting
// server b's count is non-zero. Without this, "srvB.Total() == 0" above
// could pass trivially against a fixture that can never reach server b at
// all.
func TestRootMetadataServerWalkPositiveControl(t *testing.T) {
	t.Parallel()
	srvA := fakegalaxy.New(t)
	srvB := fakegalaxy.New(t)
	srvB.AddVersion("ns", "x", "1.0.0", nil)

	servers := []config.Server{{ID: "a", URL: srvA.URL()}, {ID: "b", URL: srvB.URL()}}
	cfg := newMultiServerConfig(t, servers, buildMultiServerRequirements([]msReqSpec{{name: "ns.x"}}))
	runtime := multiServerRuntime(cfg)

	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := srvB.Total(); got == 0 {
		t.Fatalf("srvB.Total() = %d, want > 0 (without the fault, the walk must advance to server b)", got)
	}
}
