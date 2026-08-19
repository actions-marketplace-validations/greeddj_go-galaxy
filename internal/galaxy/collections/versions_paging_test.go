package collections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// pagedVersions returns n version strings in the ascending order fakegalaxy
// and newDeclaredCountServer both page them out in, so a test can compare a
// collected list against the exact sequence an offset-ordered walk yields.
func pagedVersions(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("1.0.%d", i)
	}
	return out
}

// TestLoadVersionsListCachedPagesThroughAllVersions registers a versions
// list large enough to span three offset pages (100 + 100 + 50, at
// versionLimit entries per page) and asserts loadVersionsListCached
// collects every version across all of them, in exact offset order, issuing
// exactly one request per page. DownloadWorkers is set above 1 so the pages
// after page 0 are genuinely fetched concurrently, which is what makes the
// order assertion meaningful: however the concurrent fetches land, the
// assembled list must be the offset-ordered one.
func TestLoadVersionsListCachedPagesThroughAllVersions(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)

	const wantVersions = 250
	want := pagedVersions(wantVersions)
	for _, v := range want {
		srv.AddVersion("acme", "widgets", v, nil)
	}
	versionsURL := srv.URL() + "/api/v3/collections/acme/widgets/versions/"

	cfg := &config.Config{Server: srv.URL(), DownloadWorkers: 4}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(versions, want) {
		t.Fatalf("versions not in offset order: got %d entries, first %q, last %q; want %d entries %q..%q",
			len(versions), versions[0], versions[len(versions)-1], len(want), want[0], want[len(want)-1])
	}
	// 250 versions at versionLimit=100 per page means three requests: offset
	// 0 (100), then the declared total schedules exactly offsets 100 (100)
	// and 200 (50) - never a fourth, empty request.
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 3 {
		t.Fatalf("Count(EndpointVersionsList) = %d, want 3", got)
	}
}

// TestLoadVersionsListCachedStopsAtMetaCount registers exactly two full
// pages' worth of versions and asserts loadVersionsListCached stops after
// the second request via the meta.count early-stop path (the next offset
// would reach the declared total), rather than issuing a third, empty
// request to discover there is nothing left.
func TestLoadVersionsListCachedStopsAtMetaCount(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)

	const wantVersions = 2 * versionLimit
	for _, v := range pagedVersions(wantVersions) {
		srv.AddVersion("acme", "gadgets", v, nil)
	}
	versionsURL := srv.URL() + "/api/v3/collections/acme/gadgets/versions/"

	cfg := &config.Config{Server: srv.URL()}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(versions) != wantVersions {
		t.Fatalf("len(versions) = %d, want %d", len(versions), wantVersions)
	}
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 2 {
		t.Fatalf("Count(EndpointVersionsList) = %d, want 2", got)
	}
}

// newDeclaredCountServer starts an httptest server that pages actual out
// through ?limit=&offset= slices while declaring exactly count in every
// response's meta.count, however far that diverges from what it actually
// serves - the mismatch the production walk must never trust the total
// over. The handler only reads shared state, so concurrent page fetches are
// safe against it; served counts every request received.
func newDeclaredCountServer(t *testing.T, actual []string, count int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		start := min(max(offset, 0), len(actual))
		end := min(start+max(limit, 0), len(actual))

		var page types.GalaxyCollectionVersions
		page.Meta.Count = count
		page.Data = make([]types.GalaxyCollectionVersion, 0, end-start)
		for _, v := range actual[start:end] {
			page.Data = append(page.Data, types.GalaxyCollectionVersion{Version: v})
		}
		body, err := json.Marshal(&page)
		if err != nil {
			// page is built entirely from strings and an int; marshaling it
			// cannot fail, so a non-nil error is a structural bug in this
			// fixture rather than a scenario to answer.
			panic(fmt.Sprintf("marshal fixture page: %v", err))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &served
}

// TestLoadVersionsListCachedExceedsPageCeilingFailsHard drives a server that
// answers every request with a full page and no meta.count at all (0), so
// nothing ever schedules or stops the walk except the ceiling: without a
// declared total the sequential fallback pages on demand, and this asserts
// it hard-fails with helpers.ErrVersionsPagingExceeded after exactly
// maxVersionPages requests, rather than paging forever or silently
// returning a truncated list. fakegalaxy is not usable here: it honestly
// reflects its registered versions and could never keep a walk unsatisfied.
func TestLoadVersionsListCachedExceedsPageCeilingFailsHard(t *testing.T) {
	t.Parallel()
	srv, served := newDeclaredCountServer(t, pagedVersions(maxVersionPages*versionLimit+versionLimit), 0)

	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	_, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrVersionsPagingExceeded) {
		t.Fatalf("expected errors.Is(err, helpers.ErrVersionsPagingExceeded), got %v", err)
	}
	if got := served.Load(); got != int32(maxVersionPages) {
		t.Fatalf("requests = %d, want exactly maxVersionPages=%d", got, maxVersionPages)
	}
}

// TestLoadVersionsListCachedExcessiveTotalFailsUpFront asserts a page-0
// meta.count implying more pages than maxVersionPages fails hard with
// helpers.ErrVersionsPagingExceeded after that single request - no page the
// verdict already condemns is ever fetched, and the list is never truncated
// to what those requests would have carried. The count is huge for a second
// reason too: an up-front verdict is also what keeps a hostile meta.count
// away from the pre-size allocation (an unbounded make cap would panic with
// "cap out of range" long before any page ceiling fired). 1 << 50 is
// float64-exact, so it survives the JSON meta.count parse unchanged.
func TestLoadVersionsListCachedExcessiveTotalFailsUpFront(t *testing.T) {
	t.Parallel()
	srv, served := newDeclaredCountServer(t, pagedVersions(versionLimit), 1<<50)

	cfg := &config.Config{Server: srv.URL, DownloadWorkers: 4}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	_, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrVersionsPagingExceeded) {
		t.Fatalf("expected errors.Is(err, helpers.ErrVersionsPagingExceeded), got %v", err)
	}
	if got := served.Load(); got != 1 {
		t.Fatalf("requests = %d, want exactly 1 (the verdict must precede any scheduled page)", got)
	}
}

// TestLoadVersionsListCachedToleratesLyingTotal covers the walk against a
// server whose declared total promises five pages it does not have: the
// concurrently prefetched schedule covers all five offsets, and the walk
// must still end the list at the first short or empty page - discarding
// whatever the over-scheduled offsets returned - collecting exactly the
// offset-ordered sequence a strictly sequential walk of the same responses
// yields, with no error.
func TestLoadVersionsListCachedToleratesLyingTotal(t *testing.T) {
	t.Parallel()
	const declared = 5 * versionLimit
	cases := []struct {
		name   string
		actual int
	}{
		{name: "a short page ends the walk", actual: 2*versionLimit + 30},
		{name: "an empty page ends the walk", actual: 2 * versionLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := pagedVersions(tc.actual)
			srv, _ := newDeclaredCountServer(t, want, declared)

			cfg := &config.Config{Server: srv.URL, DownloadWorkers: 4}
			runtime := infra.New(noopPrinter{}, srv.Client())
			deps := newCollectionDeps(cfg, runtime, store.New())

			versionsURL := srv.URL + "/versions/"
			versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(versions, want) {
				t.Fatalf("len(versions) = %d, want the %d actually-served entries in offset order", len(versions), tc.actual)
			}
		})
	}
}

// TestLoadVersionsListCachedZeroTotalFallsBackSequential asserts a server
// that never reports a total (meta.count 0) is paged sequentially, on
// demand: nothing is scheduled ahead, each page is requested only because
// the one before it was full, and the walk still ends on the short page
// with the complete list - three requests for 250 entries, never a fourth.
func TestLoadVersionsListCachedZeroTotalFallsBackSequential(t *testing.T) {
	t.Parallel()
	const actual = 2*versionLimit + 50
	want := pagedVersions(actual)
	srv, served := newDeclaredCountServer(t, want, 0)

	cfg := &config.Config{Server: srv.URL, DownloadWorkers: 4}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	versions, err := loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(versions, want) {
		t.Fatalf("len(versions) = %d, want all %d entries in offset order", len(versions), actual)
	}
	if got := served.Load(); got != 3 {
		t.Fatalf("requests = %d, want exactly 3 (page 0 plus the two pages a full predecessor demanded)", got)
	}
}
