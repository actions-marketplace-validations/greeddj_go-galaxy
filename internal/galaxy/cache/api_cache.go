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

// FetchJSONWithCachePolicy fetches JSON with cache policy and unmarshals into
// out. budget bounds the underlying fetchJSONBody call end to end (see its
// own doc comment); non-positive means helpers.MetadataFetchDeadline, so a
// bad value degrades structurally rather than by caller convention.
func FetchJSONWithCachePolicy(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	out any,
	policy Policy,
	budget time.Duration,
) error {
	if st == nil || (!policy.Read && !policy.Write) {
		body, _, _, _, err := fetchJSONBody(ctx, client, url, nil, budget)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)
	}

	key := apiCacheKey(url)
	if policy.Read {
		if ok, err := tryServeFromCache(ctx, client, url, st, key, out, policy, budget); ok || err != nil {
			return err
		}
	}
	return fetchAndStore(ctx, client, url, st, key, out, policy, budget)
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
	budget time.Duration,
) (bool, error) {
	entry, ok := st.GetAPICache(key)
	if !isValidCacheEntry(ok, entry, url) {
		return false, nil
	}
	if err := json.Unmarshal(entry.Body, out); err != nil {
		return false, nil
	}
	// A FetchedAt in the future cannot come from this program - every writer
	// stamps time.Now().UTC() - so it is a corrupt or clock-skewed entry, not a
	// fresh one. time.Since is negative for it, which would otherwise pass the
	// TTL test forever and pin the entry as permanently fresh; treat it as
	// expired and revalidate. Both sides are wall-clock only (.UTC() strips the
	// monotonic reading, and a decoded stamp never had one).
	age := time.Since(entry.FetchedAt)
	if policy.TTL != 0 && (age > policy.TTL || age < 0) {
		// Accepted micro-cost: on the rare TTL-expired-and-changed path, the
		// stale (but valid) body decoded above into out is simply overwritten
		// by revalidateCache's fresh decode below. This second decode is
		// bounded by a network round trip, so it is negligible next to it.
		return revalidateCache(ctx, client, url, st, key, entry, out, policy, budget)
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
	budget time.Duration,
) (bool, error) {
	body, etag, lastModified, notModified, err := fetchJSONBody(ctx, client, url, &entry, budget)
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
func fetchAndStore(
	ctx context.Context,
	client *http.Client,
	url string,
	st *store.Store,
	key string,
	out any,
	policy Policy,
	budget time.Duration,
) error {
	body, etag, lastModified, _, err := fetchJSONBody(ctx, client, url, nil, budget)
	if err != nil {
		return err
	}
	if policy.Write {
		st.SetAPICache(key, newAPICacheEntry(url, body, etag, lastModified, policy.TTL))
	}
	return json.Unmarshal(body, out)
}

// newAPICacheEntry builds a cache entry from response data. Body is stored
// verbatim - byte for byte what the server sent, with no cut applied
// anywhere in this package.
//
// For a version-detail document, that body includes download_url, which on
// an object-storage-backed Galaxy NG or Automation Hub deployment is a
// presigned URL whose query string is a time-limited bearer capability (see
// helpers.WithoutQuery's own doc comment for what that means). It is stored
// uncut here, unlike GALAXY.yml's own copy of the same value
// (buildGalaxyYAML, internal/galaxy/collections/galaxy_info.go): that
// sidecar is written for an operator to read, while this entry is read back
// and its URL is fetched by this program itself - cutting it here would turn
// every cache-served download into a 403 against a presign that no longer
// names anything.
//
// The exposure window this leaves is bounded by the presign's own expiry,
// not by anything this program controls: neither the entry's own TTL nor
// helpers.CacheEntryMaxAge, both of which govern how long the entry survives
// in the persisted snapshot rather than how long the URL inside it stays
// live. A principal who can read the shared cache - the trust boundary
// Backend.LoadStore/LoadProjectRegistry document - gets a working capability
// only while the presign it names is still live, and gets a fresh one every
// time this entry is refreshed, exactly the capability a legitimate fetch
// through this cache would use regardless.
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
//
// budget bounds the whole call - build+Do+status-classify+io.ReadAll, and
// every retry attempt and backoff sleep in the helpers.Retry loop below - as
// one shared budget established ONCE here, around the outer loop, rather
// than re-derived per attempt: a non-positive value (in particular the zero
// value every existing caller in this package's own test suite passes)
// structurally falls back to helpers.MetadataFetchDeadline via
// metadataBudget. This is deliberately not established one layer up, in
// internal/galaxy/collections/compat.go's thin wrapper: the precedent this
// follows (helpers.ArtifactDownloadDeadline, established in
// downloadCollectionToCache) is "the budget is established by the code that
// owns the unit of work, and never behind the Backend seam". This function
// owns one metadata request - build, Do, status-classify, ReadAll, retry -
// exactly the unit the budget bounds; internal/galaxy/cache is on the
// business-logic side of the Backend seam (it is not internal/cache/*), so
// establishing the budget here does not cross it. Establishing it one layer
// up in collections/compat.go instead would put the budget in a pass-through
// wrapper that owns nothing, would leave fetchRetryable unable to see the
// sentinel it must classify terminal (see fetchRetryable's own doc comment),
// and would let a future direct caller of the exported
// FetchJSONWithCachePolicy escape the budget entirely.
//
// A pure cache hit (tryServeFromCache returning true without revalidating)
// never reaches this function at all, so it never pays for a timer. The
// budget also deliberately does not cover the caller's json.Unmarshal: that
// is local CPU work on bytes already fully read, not a network operation a
// hostile or degraded endpoint can stall.
//
// One benign race worth naming here, since it is easy to mistake for a bug
// later: a retryable 5xx whose backoff sleep is cut short by this budget
// surfaces to the caller as helpers.ErrMetadataFetchDeadline, not as the
// *HTTPStatusError that triggered the retry in the first place. This is a
// property of helpers.Retry itself - its backoff wait races ctx.Done()
// against the jittered timer and, on losing, returns a bare ctx.Err()
// straight from that select, discarding the status error the closure last
// produced - not of anything specific to this call site. On the root-metadata
// path this means a retryable-status server that keeps failing until the
// budget runs out aborts with helpers.ErrMetadataFetchDeadline instead of
// tryServerRootMetadata's usual helpers.ErrGalaxyServerUnavailable wrap; both
// still abort the whole server-list walk, and both still classify
// ExitNetwork, so only the operator-facing message differs.
func fetchJSONBody(
	ctx context.Context,
	client *http.Client,
	url string,
	entry *store.APICacheEntry,
	budget time.Duration,
) ([]byte, string, string, bool, error) {
	budget = metadataBudget(budget)
	dlCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var (
		body               []byte
		etag, lastModified string
		notModified        bool
	)
	err := helpers.Retry(dlCtx, helpers.FetchRetryPolicy(), func() error {
		var attemptErr error
		body, etag, lastModified, notModified, attemptErr = fetchJSONBodyOnce(dlCtx, client, url, entry)
		return deadlineError(ctx, dlCtx, budget, helpers.ErrMetadataFetchDeadline, attemptErr)
	}, fetchRetryable)
	if err != nil {
		return nil, "", "", false, deadlineError(ctx, dlCtx, budget, helpers.ErrMetadataFetchDeadline, err)
	}
	return body, etag, lastModified, notModified, nil
}

// metadataBudget returns budget when it is a positive duration, or
// helpers.MetadataFetchDeadline otherwise, so a non-positive value (the zero
// value included) degrades structurally to the real constant rather than by
// caller convention.
func metadataBudget(budget time.Duration) time.Duration {
	if budget <= 0 {
		return helpers.MetadataFetchDeadline
	}
	return budget
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

	body, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, helpers.MetadataMaxSize))
	if err != nil {
		// NewSizeLimitedReader's helpers.ErrResponseTooLarge is a bare size
		// ceiling shared with two other surfaces (an artifact download, an S3
		// listing or batch-delete response), so it is wrapped here naming this
		// one - a Galaxy metadata document - to keep errors.Is matching intact
		// while telling an operator which response actually overran.
		return nil, "", "", false, fmt.Errorf("galaxy metadata document: %w", err)
	}
	return body, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
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
