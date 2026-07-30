package cache

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestMetadataFetchRejectsOversizedResponse proves a Galaxy API metadata
// response that overruns helpers.MetadataMaxSize is rejected before it is
// decoded or cached, and that the rejection is treated as terminal rather
// than retried. The happy path - a small valid JSON body fetched and cached
// successfully through the same FetchJSONWithCachePolicy call - is already
// covered by TestFetchJSONWithCachePolicyCacheHit in api_cache_test.go, so it
// is not duplicated here.
func TestMetadataFetchRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32

	// A single small chunk reused for every write, so the server itself never
	// allocates anywhere near helpers.MetadataMaxSize; only the transferred
	// byte count grows across repeated writes of the same buffer.
	chunk := bytes.Repeat([]byte("a"), 64<<10)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		var written int64
		for written <= helpers.MetadataMaxSize {
			n, err := w.Write(chunk)
			written += int64(n)
			if err != nil {
				// The client aborts its read once the cumulative count
				// crosses the cap and closes the body, so a write past that
				// point is expected to fail; stop sending rather than spin.
				break
			}
		}
	}))
	defer srv.Close()

	st := store.New()
	var out map[string]any
	policy := Policy{Read: true, Write: true}

	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, st, &out, policy, 0)
	if !errors.Is(err, helpers.ErrArtifactTooLarge) {
		t.Fatalf("FetchJSONWithCachePolicy() error = %v, want ErrArtifactTooLarge", err)
	}

	key := apiCacheKey(srv.URL)
	if _, ok := st.GetAPICache(key); ok {
		t.Fatalf("expected no cache entry to be written after an over-cap fetch")
	}

	if got := requests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request (an over-cap response is terminal, not retried), got %d", got)
	}
}
