package collections

// TestVersionsPagingSharesOneMetadataBudget covers loadVersionsListCached's
// loop-level budget: the one place a per-request metadata budget alone is
// not enough, since the number of pages is chosen by the server (it
// declares meta.count), not by the operator or a fixed program constant.
//
// This test was verified against a real revert of the production change it
// pins, and this comment quotes the actual observed output:
//
//   - Deleting the loop-level budget in loadVersionsListCached (passing ctx
//     straight through to fetchVersionsPage instead of a dlCtx bounded by
//     deps.runtime.MetadataDeadline(), and calling fetchVersionsPage's error
//     straight through instead of through cacheManager.MetadataDeadlineError)
//     makes every one of the 8 pages complete (each pays only its own
//     roughly 150ms per-request budget, well under the 2-minute default, and
//     nothing else bounds the loop as a whole), so loadVersionsListCached
//     SUCCEEDS instead of failing, observed as:
//     "unexpected error: <nil>, want errors.Is ErrMetadataFetchDeadline"
//     Margin: at 150ms per page against the 500ms loop budget used here,
//     each page has roughly 3.2x headroom before the loop budget would
//     itself become the reason a healthy server's pages start failing - a
//     future edit narrowing that margin should re-check this test still
//     reliably ends before all 8 pages complete.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// versionsPagingDeadlineTotalPages is the number of full pages
// newVersionsPagingDelayServer's meta.count forces the loop to want, sized
// well above what the loop-level budget below can complete.
const versionsPagingDeadlineTotalPages = 8

// versionsPagingDeadlineBudget is the loop-level
// deps.runtime.MetadataDeadline() budget every test in this file uses.
const versionsPagingDeadlineBudget = 500 * time.Millisecond

// newVersionsPagingDelayServer starts an httptest server that sleeps delay
// before answering every request with one full page of versionLimit
// version entries and a meta.count forcing
// versionsPagingDeadlineTotalPages total pages. served counts every request
// the handler receives, incremented before the delay so it reflects an
// in-flight request even if the client aborts before the response arrives.
func newVersionsPagingDelayServer(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var served atomic.Int32

	var page types.GalaxyCollectionVersions
	page.Meta.Count = versionsPagingDeadlineTotalPages * versionLimit
	page.Data = make([]types.GalaxyCollectionVersion, versionLimit)
	for i := range page.Data {
		page.Data[i] = types.GalaxyCollectionVersion{Version: fmt.Sprintf("1.0.%d", i)}
	}
	body, err := json.Marshal(&page)
	if err != nil {
		t.Fatalf("marshal fixture page: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &served
}

// TestVersionsPagingSharesOneMetadataBudget asserts a server that keeps
// answering every page (never a short page, never reaching meta.count) but
// takes 150ms per response is caught by loadVersionsListCached's own
// loop-level budget: the call fails with helpers.ErrMetadataFetchDeadline
// having served fewer than versionsPagingDeadlineTotalPages pages, rather
// than completing all of them under a per-request-only budget.
func TestVersionsPagingSharesOneMetadataBudget(t *testing.T) {
	t.Parallel()
	const pageDelay = 150 * time.Millisecond

	srv, served := newVersionsPagingDelayServer(t, pageDelay)
	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	runtime.MetadataFetchDeadline = versionsPagingDeadlineBudget
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	_, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("unexpected error: %v, want errors.Is ErrMetadataFetchDeadline", err)
	}
	if got := served.Load(); got >= versionsPagingDeadlineTotalPages {
		t.Fatalf("served = %d pages, want fewer than %d (the loop-level budget must end the run before all pages complete)",
			got, versionsPagingDeadlineTotalPages)
	}
}

// TestVersionsPagingDeadlineFixturePositiveControl is the positive control
// for TestVersionsPagingSharesOneMetadataBudget: the identical fixture with
// no per-page delay completes every page under the same loop-level budget,
// proving that budget is not itself what would fail an ordinary paging run
// against this fixture.
func TestVersionsPagingDeadlineFixturePositiveControl(t *testing.T) {
	t.Parallel()
	srv, served := newVersionsPagingDelayServer(t, 0)
	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	runtime.MetadataFetchDeadline = versionsPagingDeadlineBudget
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("loadVersionsListCached: %v", err)
	}
	if want := versionsPagingDeadlineTotalPages * versionLimit; len(versions) != want {
		t.Fatalf("len(versions) = %d, want %d", len(versions), want)
	}
	if got := served.Load(); got != versionsPagingDeadlineTotalPages {
		t.Fatalf("served = %d pages, want exactly %d", got, versionsPagingDeadlineTotalPages)
	}
}
