package collections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// TestLoadVersionsListCachedPagesThroughAllVersions registers a versions
// list large enough to span three offset pages (100 + 100 + 50, at
// versionLimit entries per page) and asserts loadVersionsListCached
// collects every version across all of them, issuing exactly one request
// per page.
func TestLoadVersionsListCachedPagesThroughAllVersions(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)

	const wantVersions = 250
	for i := range wantVersions {
		srv.AddVersion("acme", "widgets", fmt.Sprintf("1.0.%d", i), nil)
	}
	versionsURL := srv.URL() + "/api/v3/collections/acme/widgets/versions/"

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
	// 250 versions at versionLimit=100 per page means three requests:
	// offset 0 (100), offset 100 (100), offset 200 (50, short - stops the
	// loop without a fourth, empty request).
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 3 {
		t.Fatalf("Count(EndpointVersionsList) = %d, want 3", got)
	}
}

// TestLoadVersionsListCachedStopsAtMetaCount registers exactly two full
// pages' worth of versions and asserts loadVersionsListCached stops after
// the second request via the meta.count early-stop path (offset reaches the
// declared total), rather than issuing a third, empty request to discover
// there is nothing left.
func TestLoadVersionsListCachedStopsAtMetaCount(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)

	const wantVersions = 2 * versionLimit
	for i := range wantVersions {
		srv.AddVersion("acme", "gadgets", fmt.Sprintf("1.0.%d", i), nil)
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

// TestLoadVersionsListCachedExceedsPageCeilingFailsHard uses a bespoke
// server (not fakegalaxy, which honestly reflects its registered versions
// and could never produce this scenario) that always answers with a full
// page and a meta.count so large the offset never catches up to it. This
// simulates a server whose declared total keeps outrunning what it
// actually serves, and asserts loadVersionsListCached hard-fails with
// helpers.ErrVersionsPagingExceeded after exactly maxVersionPages requests,
// rather than looping forever or silently returning a truncated list.
func TestLoadVersionsListCachedExceedsPageCeilingFailsHard(t *testing.T) {
	t.Parallel()

	// A single full page body, reused for every request: versionLimit
	// distinct-looking version entries and a deliberately huge meta.count.
	// It is huge for two reasons: offset += versionLimit never reaches it
	// within maxVersionPages pages, so the ceiling fires; and an unclamped
	// page-0 pre-size of make([]string, 0, total) would panic with "cap out
	// of range", so this test also guards the clamp that bounds the
	// allocation to at most the ceiling's worth of entries. 1 << 50 is
	// float64-exact, so it survives the JSON meta.count parse unchanged.
	var page types.GalaxyCollectionVersions
	page.Meta.Count = 1 << 50
	page.Data = make([]types.GalaxyCollectionVersion, versionLimit)
	for i := range page.Data {
		page.Data[i] = types.GalaxyCollectionVersion{Version: fmt.Sprintf("1.0.%d", i)}
	}
	body, err := json.Marshal(&page)
	if err != nil {
		t.Fatalf("failed to marshal fixture page: %v", err)
	}

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Server: srv.URL}
	runtime := infra.New(noopPrinter{}, srv.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	versionsURL := srv.URL + "/versions/"
	_, err = loadVersionsListCached(context.Background(), deps, versionsURL, cacheManager.Policy{})
	if !errors.Is(err, helpers.ErrVersionsPagingExceeded) {
		t.Fatalf("expected errors.Is(err, helpers.ErrVersionsPagingExceeded), got %v", err)
	}
	if got := requests.Load(); got != int32(maxVersionPages) {
		t.Fatalf("requests = %d, want exactly maxVersionPages=%d", got, maxVersionPages)
	}
}
