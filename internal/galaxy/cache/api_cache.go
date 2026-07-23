package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// apiCacheKey generates a stable cache key for a URL.
func apiCacheKey(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// FetchJSONWithCachePolicy fetches JSON with cache policy and unmarshals into out.
func FetchJSONWithCachePolicy(ctx context.Context, client *http.Client, url string, st *store.Store, out any, policy Policy) error {
	if st == nil || (!policy.Read && !policy.Write) {
		body, _, _, _, err := fetchJSONBody(ctx, client, url, nil)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)
	}

	key := apiCacheKey(url)
	if policy.Read {
		if ok, err := tryServeFromCache(ctx, client, url, st, key, out, policy); ok || err != nil {
			return err
		}
	}
	return fetchAndStore(ctx, client, url, st, key, out, policy)
}

// tryServeFromCache attempts to serve from cache and reports if handled.
// The cached body is decoded into out exactly once, right here: that single
// decode is also the corruption check. A body that fails to unmarshal -
// whether freshly written or long expired - is corrupt, and the false return
// sends the caller (FetchJSONWithCachePolicy) into fetchAndStore, which
// issues an unconditional fetch (nil validators) and overwrites the entry
// with fresh bytes. Routing a corrupt entry through revalidateCache's
// conditional GET instead would be wrong: the server would see the same
// ETag/Last-Modified it already served, reply 304, and hand the exact same
// unusable bytes straight back, so the entry would never heal.
func tryServeFromCache(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	key string,
	out any,
	policy Policy,
) (bool, error) {
	entry, ok := st.GetAPICache(key)
	if !isValidCacheEntry(ok, entry, url) {
		return false, nil
	}
	if err := json.Unmarshal(entry.Body, out); err != nil {
		return false, nil
	}
	if policy.TTL != 0 && time.Since(entry.FetchedAt) > policy.TTL {
		// Accepted micro-cost: on the rare TTL-expired-and-changed path, the
		// stale (but valid) body decoded above into out is simply overwritten
		// by revalidateCache's fresh decode below. This second decode is
		// bounded by a network round trip, so it is negligible next to it.
		return revalidateCache(ctx, client, url, st, key, entry, out, policy)
	}
	return true, nil
}

func isValidCacheEntry(ok bool, entry store.APICacheEntry, url string) bool {
	if !ok || entry.URL != url || len(entry.Body) == 0 {
		return false
	}
	return true
}

// revalidateCache issues a conditional GET for an entry that tryServeFromCache
// already decoded into out and found expired. On a 304, that decode is still
// valid - the server confirmed the bytes are unchanged - so the response is
// reported as handled without re-unmarshaling the same bytes a second time.
func revalidateCache(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	key string,
	entry store.APICacheEntry,
	out any,
	policy Policy,
) (bool, error) {
	body, etag, lastModified, notModified, err := fetchJSONBody(ctx, client, url, &entry)
	if err != nil {
		return false, err
	}
	if notModified {
		if policy.Write {
			st.SetAPICache(key, refreshAPICacheEntry(entry, etag, lastModified))
		}
		return true, nil
	}
	if policy.Write {
		st.SetAPICache(key, newAPICacheEntry(url, body, etag, lastModified, policy.TTL))
	}
	return true, json.Unmarshal(body, out)
}

// fetchAndStore downloads JSON and optionally stores it in the cache.
func fetchAndStore(ctx context.Context, client *http.Client, url string, st *store.Store, key string, out any, policy Policy) error {
	body, etag, lastModified, _, err := fetchJSONBody(ctx, client, url, nil)
	if err != nil {
		return err
	}
	if policy.Write {
		st.SetAPICache(key, newAPICacheEntry(url, body, etag, lastModified, policy.TTL))
	}
	return json.Unmarshal(body, out)
}

// newAPICacheEntry builds a cache entry from response data.
func newAPICacheEntry(url string, body []byte, etag, lastModified string, ttl time.Duration) store.APICacheEntry {
	return store.APICacheEntry{
		URL:          url,
		FetchedAt:    time.Now().UTC(),
		TTL:          ttl,
		Body:         body,
		ETag:         etag,
		LastModified: lastModified,
	}
}

// refreshAPICacheEntry updates timestamps and validators for a cached entry.
func refreshAPICacheEntry(entry store.APICacheEntry, etag, lastModified string) store.APICacheEntry {
	entry.FetchedAt = time.Now().UTC()
	if etag != "" {
		entry.ETag = etag
	}
	if lastModified != "" {
		entry.LastModified = lastModified
	}
	return entry
}

// fetchJSONBody fetches JSON bytes and validation headers for a URL,
// retrying a transient failure (a retryable HTTP status or a stalled body
// read, per fetchRetryable) up to helpers.FetchRetryPolicy's bound. Each
// attempt builds a fresh request and resends any conditional headers, so a
// retry is never served a stale If-None-Match/If-Modified-Since pair. A 304
// is treated as success and returned immediately, never retried.
func fetchJSONBody(ctx context.Context, client *http.Client, url string, entry *store.APICacheEntry) ([]byte, string, string, bool, error) {
	var (
		body               []byte
		etag, lastModified string
		notModified        bool
	)
	err := helpers.Retry(ctx, helpers.FetchRetryPolicy(), func() error {
		var attemptErr error
		body, etag, lastModified, notModified, attemptErr = fetchJSONBodyOnce(ctx, client, url, entry)
		return attemptErr
	}, fetchRetryable)
	if err != nil {
		return nil, "", "", false, err
	}
	return body, etag, lastModified, notModified, nil
}

// fetchJSONBodyOnce performs a single build+Do+status-classify+io.ReadAll
// cycle for url, the one attempt fetchJSONBody's retry loop repeats on a
// transient failure. The response body is always closed before returning,
// since every path here either reads it to completion or (on a 304) never
// needed it in the first place.
func fetchJSONBodyOnce(
	ctx context.Context,
	client *http.Client,
	url string,
	entry *store.APICacheEntry,
) ([]byte, string, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, "", "", false, err
	}
	if entry != nil {
		if entry.ETag != "" {
			req.Header.Set("If-None-Match", entry.ETag)
		}
		if entry.LastModified != "" {
			req.Header.Set("If-Modified-Since", entry.LastModified)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", false, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotModified {
		return nil, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", false, &HTTPStatusError{URL: url, Status: resp.Status, Code: resp.StatusCode}
	}

	body, err := io.ReadAll(resp.Body)
	return body, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, err
}

// HTTPStatusError describes a non-200 HTTP response.
type HTTPStatusError struct {
	URL    string
	Status string
	Code   int
}

// Error implements the error interface.
func (e *HTTPStatusError) Error() string {
	if e.URL != "" {
		return fmt.Sprintf("failed to fetch metadata: %s (%s)", e.Status, e.URL)
	}
	return "failed to fetch metadata: " + e.Status
}
