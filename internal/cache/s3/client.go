package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // used only for the S3-mandated Content-MD5 integrity header, not as a security primitive
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// signingKeyCache memoizes the SigV4 signing key for the current UTC date.
// Secret and region are immutable for the client's lifetime, so date is the
// only cache key; a plain mutex protects the string compare and the slice
// swap, both cheap enough that an RWMutex would add overhead without benefit.
type signingKeyCache struct {
	date string
	key  []byte
	mu   sync.Mutex
}

// Client implements minimal S3 operations with SigV4 signing.
type Client struct {
	client         *http.Client
	endpointHost   string
	endpointScheme string
	cfg            config.S3CacheConfig
	signing        signingKeyCache
}

// newClient constructs an S3 client from configuration.
func newClient(cfg config.S3CacheConfig, httpClient *http.Client) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, errS3BucketEmpty
	}
	if httpClient == nil {
		return nil, errS3HTTPClientNil
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", cfg.Region)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: %s", errS3InvalidEndpoint, endpoint)
	}
	// cfg.Endpoint is trimmed of a trailing "/" below, but that trim never
	// touches the host or scheme components, so parsed.Host/parsed.Scheme
	// here are exactly what requestURL would have derived from the trimmed
	// cfg.Endpoint on every request - safe to cache once and reuse.
	cfg.Endpoint = strings.TrimRight(endpoint, "/")

	// This client gets its own private copy of httpClient, with
	// CheckRedirect set to refuseRedirect (see that function's doc comment
	// for why every redirect is refused, not just a cross-host one), rather
	// than a mutation of httpClient in place. httpClient is the same
	// *http.Client internal/galaxy/fetch builds and internal/cache.New
	// threads through as runtime.HTTP for Galaxy metadata fetches and
	// artifact downloads too - and that client's default (nil) CheckRedirect
	// is load-bearing there: internal/galaxy/fetch/client_test.go's
	// TestNew_RedirectFromInsecureOriginToSecureOriginUsesSecureTransport and
	// auth_test.go's TestAuthTransport_RoundTrip_CrossOriginRedirectDropsToken
	// both drive a real redirect through that exact client and assert on the
	// outcome. Mutating httpClient.CheckRedirect here would silently
	// disable redirects for those callers as a side effect of constructing
	// an S3 client, which is not this function's business to decide. The
	// copy is a shallow struct copy, so Transport (and Jar, if any) stay
	// shared with httpClient - the connection pool is unaffected - only the
	// two http.Client values' CheckRedirect fields diverge.
	redirectless := *httpClient
	redirectless.CheckRedirect = refuseRedirect
	return &Client{cfg: cfg, client: &redirectless, endpointHost: parsed.Host, endpointScheme: parsed.Scheme}, nil
}

// refuseRedirect is the redirectless http.Client copy's CheckRedirect hook:
// it refuses every redirect the S3 endpoint answers with, unconditionally,
// rather than allowing same-origin or same-host-scheme-upgrade hops through.
// SigV4 signs the Host header and the canonical URI, so a redirect to a
// different path breaks the signature, and a redirect to a different host
// breaks it further by losing the Authorization header - which net/http
// strips only for a destination that is neither the same hostname nor a
// subdomain of it, comparing hostnames alone, so neither a different port
// nor a different scheme causes a strip. Such a hop cannot produce a
// verifiable request in any shape this client emits; following it only
// trades a legible "redirected to <origin>" for an opaque
// "403 SignatureDoesNotMatch".
//
// What following a redirect adds, beyond that, is exposure. net/http strips
// only Authorization, Www-Authenticate, Cookie, Cookie2, and the two Proxy-
// headers, so every X-Amz-* header this client sets - X-Amz-Security-Token,
// X-Amz-Content-Sha256, X-Amz-Date - travels to the target, which is the one
// party that did not already see the original request. On 307 and 308 the
// body is replayed too whenever GetBody is set, which it is for every
// bytes.Reader body here (the gzipped snapshot, the project registry, the
// lock object). And because the hostname comparison ignores the scheme, an
// https-to-http hop would carry the whole signed header set in cleartext,
// where it is replayable rather than merely observable.
//
// A same-host scheme upgrade is refused too, deliberately: it is the one hop
// that could in principle verify (SigV4 does not sign the scheme), so
// refusing it does cost a working configuration - an http:// endpoint whose
// front redirects to https:// on the same host and path. Papering over that
// endpoint is still worse than surfacing it, because the request that
// triggered the redirect already put this client's credentials on the wire
// in the clear before this hook ever runs: its SigV4 Authorization header
// always, and X-Amz-Security-Token whenever a session token is configured.
//
// Who can emit a redirect is a predicate, not a list: any party that already
// sees this request in the clear - the configured endpoint itself, or any
// intermediate the client accepts a certificate from, which on a plaintext
// endpoint includes an environment-configured HTTP proxy and any on-path
// attacker.
//
// req is the pending request for the redirect target, not the request that
// triggered it; req.Response is the response that caused this redirect and
// is populated only during a client redirect, so it is nil-checked before
// use. via (the redirect chain so far) is unused: refusing on the first hop
// makes a chain impossible.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	status := "redirect"
	if req.Response != nil {
		status = req.Response.Status
	}
	return fmt.Errorf("%w: %s redirected to %s; configure --s3-endpoint as that origin",
		errS3RedirectRefused, status, helpers.Origin(req.URL))
}

// do delegates to c.client.Do and normalizes a transport-level failure into
// helpers.ErrCacheBackendUnavailable, so a dead bucket is distinguishable
// from the caller's own configuration mistakes and from a request that
// merely returned a non-2xx status (each idempotent verb's own status
// handling, e.g. s3StatusError, already carries the class that applies to
// its status).
//
// The exclusion below asks exactly one question - did whoever asked for this
// work stop wanting it - by checking req.Context().Err(), not the shape of
// err. req.Context() is the caller's own context: newRequest builds req with
// it (http.NewRequestWithContext below) and nothing between there and here
// re-points it - watchdogTransport wraps every request in its own derived
// context but calls its base RoundTripper with req.Clone(wctx), leaving the
// req object held in this function untouched, and authTransport /
// tlsDispatchTransport dispatch purely by request origin without touching
// the context at all. This is the same discriminator
// internal/galaxy/cache's deadlineError (its parent.Err() != nil check) and
// internal/galaxy/collections' artifactDeadlineError already use for the
// identical question on their own surfaces, not a new pattern introduced
// here.
//
// It deliberately does not test the shape of err instead (e.g.
// errors.Is(err, context.DeadlineExceeded)). Measured on go1.26.5 with this
// project's transport settings, both a net.Dialer timeout
// (helpers.FetchDialContextTimeout) and http.Transport's own
// ResponseHeaderTimeout satisfy errors.Is(err, context.DeadlineExceeded)
// while the caller's own context is still live, so an error-shape test would
// silently exclude the two commonest outage shapes from ever reaching the
// sentinel: a black-holed endpoint (the dial never completes) and one that
// accepts a connection and then never answers (headers never arrive). A
// future reader must be able to re-derive that defect from this comment
// without re-measuring it, which is why both shapes are named here. Do not
// reintroduce errors.Is(err, context.Canceled) or
// errors.Is(err, context.DeadlineExceeded) as a second, "for symmetry" or
// belt-and-suspenders check: such an error already classifies ExitInterrupt
// in exitcode.FromError regardless of what this function returns, since that
// check runs first, so the check buys nothing - and leaving any error-shape
// test in place is exactly what would silently restore this exclusion.
//
// When a state-object or artifact budget is in force, the context that
// budget's own context.WithTimeout built IS req.Context() by the time this
// method runs - every call site in this file threads its ctx parameter
// straight into newRequest, and callers higher up construct that ctx from
// the relevant budget (see internal/galaxy/cache's stateDeadlineBackend and
// internal/galaxy/collections' downloadCollectionToCache/fetchArtifact). So a
// budget expiry still returns the raw context-carrying error here, unwrapped,
// exactly as it did before this predicate existed, and
// internal/galaxy/cache's deadlineError / internal/galaxy/collections'
// artifactDeadlineError normalize it into their own sentinel by testing
// errors.Is against that same raw error - true by construction now, not by
// coincidence of how the standard library happens to shape a timeout error.
//
// helpers.ErrCacheBackendUnavailable means the backend did not answer for a
// reason that is not this program's own doing; no consumer of that sentinel
// has to reason about an error tree that also carries a context signal.
//
// This changes no exit code today: cmd/go-galaxy/exitcode's isTransportError
// already matches both context.DeadlineExceeded and
// helpers.ErrCacheBackendUnavailable into the same ExitNetwork class, so a
// dial timeout or a ResponseHeaderTimeout classifies the same way on either
// side of this predicate. What this buys is message accuracy - the sentinel
// now means what it says for every transport failure with a live caller
// context, not only a connect refusal - and defense in depth for a future
// consumer that comes to distinguish the two classes without knowing this
// history.
//
// The funnel's boundary: only a failure c.client.Do itself returns is
// normalized here. A mid-stream body-read failure - Artifacts'
// downloadToFile, or readObject's io.ReadAll after a 200 response - happens
// after do has already returned successfully and is never labeled by this
// method.
//
// One race is resolved deliberately, not by accident: if the caller cancels
// in the same instant a genuine transport error arrives, req's own context
// already carries that cancellation by the time this check runs, so the raw
// error returns unlabeled rather than as helpers.ErrCacheBackendUnavailable -
// the caller's own decision to stop wins over labeling the failure as the
// backend's fault.
//
// A redirect refusal (errS3RedirectRefused, raised by c.client's
// CheckRedirect hook - see refuseRedirect) gets an arm of its own, and what
// matters about its position is only that it precedes the wrap below; its
// order relative to the context exclusion above is not observable, since
// both arms return the same error unwrapped. Without the arm the refusal
// would fall through to the wrap and pick up
// helpers.ErrCacheBackendUnavailable on top of the
// helpers.ErrCacheBackendUnusable errS3RedirectRefused already carries - two
// classes on one error tree, which this package's own partition rule (see
// variables.go) forbids - and s3Retryable would then retry it as an ordinary
// transport failure, spending the whole retry budget re-triggering the same
// refusal instead of returning it once.
//
// c.client.Do returns a non-nil *http.Response alongside a CheckRedirect
// error and this arm discards it, which leaks nothing: net/http has already
// closed that body before returning, as http.Client.CheckRedirect's own
// godoc states.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	// #nosec G704 -- req's own Host header is always this Client's configured
	// S3 endpoint: requestURL builds it from c.cfg.Endpoint/c.cfg.Bucket in
	// path-style mode, or from c.endpointHost/c.endpointScheme in virtual-host
	// mode - never from a remote value. Only the object key path segment
	// varies, which is not an SSRF vector since req's own destination host is
	// fixed by configuration; c.client's CheckRedirect (refuseRedirect, set
	// once in newClient on this client's own private copy) refuses every
	// redirect the endpoint answers with, so a later hop can never move this
	// request's destination host away from what this guard bounds either.
	resp, err := c.client.Do(req)
	if err == nil {
		return resp, nil
	}
	if errors.Is(err, errS3RedirectRefused) {
		return nil, err
	}
	if req.Context().Err() != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %w", errS3TransportFailed, err)
}

// s3ErrorResponse captures the fields S3 puts in the XML <Error> document
// that many non-2xx responses carry in their body, alongside the HTTP status
// line.
type s3ErrorResponse struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// s3StatusError builds an error wrapping sentinel that folds in the response
// body's S3 error code and message when one is present, falling back to the
// bare HTTP status line when the body is empty, is not XML, or lacks a Code
// element. Reading or parsing the body never fails this call outright - a
// malformed or truncated error body must not hide the original failure - and
// the caller remains responsible for closing resp.Body afterward.
func s3StatusError(sentinel error, resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, s3ErrorBodyLimit))
	if err != nil {
		return fmt.Errorf("%w: %s", sentinel, resp.Status)
	}
	var parsed s3ErrorResponse
	if err := xml.Unmarshal(data, &parsed); err != nil || parsed.Code == "" {
		return fmt.Errorf("%w: %s", sentinel, resp.Status)
	}
	return fmt.Errorf("%w: %s (%s: %s)", sentinel, resp.Status, parsed.Code, parsed.Message)
}

// getObject performs a GET request for the object key, retrying a
// transient failure (a retryable HTTP status or a stalled body read) up to
// s3RetryPolicy's bound. Each attempt builds a fresh request via newRequest
// so its X-Amz-Date and signature are never stale by the time a retry
// fires; a failed attempt's response body is closed before the next one, and
// only the eventual successful response is returned open for the caller to
// read and close.
func (c *Client) getObject(ctx context.Context, key string) (*http.Response, error) {
	var success *http.Response
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodGet, key, nil, nil, emptySHA256, nil, putCondition{})
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return errS3NotFound
		}
		if resp.StatusCode != http.StatusOK {
			statusErr := wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3GetFailed, resp))
			_ = resp.Body.Close()
			return statusErr
		}
		success = resp
		return nil
	}, s3RetryableFor(ctx))
	if err != nil {
		return nil, err
	}
	return success, nil
}

// headObject performs a HEAD request for the object key, retrying a
// transient failure like getObject. HEAD responses never carry a body
// (net/http elides it even if a handler writes one), so the failure here
// stays status-only rather than going through s3StatusError.
func (c *Client) headObject(ctx context.Context, key string) (http.Header, error) {
	var headers http.Header
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodHead, key, nil, nil, emptySHA256, nil, putCondition{})
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return errS3NotFound
		}
		if resp.StatusCode != http.StatusOK {
			return wrapRetryableStatus(resp.StatusCode, fmt.Errorf("%w: %s", errS3HeadFailed, resp.Status))
		}
		headers = resp.Header.Clone()
		return nil
	}, s3RetryableFor(ctx))
	if err != nil {
		return nil, err
	}
	return headers, nil
}

// putCondition is the conditional header a write carries, and the two forms
// are mutually exclusive in practice rather than by construction: a
// create-if-absent write (ifNoneMatch) asserts the object is not there, while
// a compare-and-swap write (ifMatch, an ETag) asserts it is there and is
// still exactly the version the caller last read. The zero value is an
// unconditional overwrite.
//
// The type exists rather than a second bool parameter because the retry rule
// below turns on "is this write conditional at all", which a caller must not
// be able to get wrong by passing the two flags independently.
type putCondition struct {
	ifMatch     string
	ifNoneMatch bool
}

// isConditional reports whether this write carries any precondition, which is
// what decides whether it may be retried.
func (c putCondition) isConditional() bool {
	return c.ifNoneMatch || c.ifMatch != ""
}

// putObject uploads an object with optional metadata. A conditional PUT -
// create-if-absent or compare-and-swap alike - is single-shot and never
// retried, deliberately including a transport failure that never produced a
// response: such a failure is indistinguishable from a lost success (the PUT
// may already have landed on the remote before the response was lost), so
// retrying it would observe 412 (the object it just wrote now exists, or no
// longer carries the ETag it swapped against) and misreport its own success
// as contention, which the distributed lock's acquireLock loop cannot
// distinguish from a live holder - see reclaimIfExpired/tryAcquireOnce, whose
// own loop is the sole retrier of conditional PUTs, transport failures
// included. That ambiguity is unchanged by compare-and-swap: a CAS write
// narrows who may win, not whether a lost response can be read two ways. An
// unconditional overwrite is safe to retry: each attempt reseeks body to its
// start and rebuilds the request (fresh signature) before resending.
func (c *Client) putObject(
	ctx context.Context,
	key string,
	body io.ReadSeeker,
	size int64,
	contentType, contentEncoding string,
	meta map[string]string,
	cond putCondition,
	payloadHash string,
) error {
	payloadHash, err := resolvePayloadHash(body, payloadHash)
	if err != nil {
		return err
	}
	attempt := func() error {
		req, err := c.newRequest(ctx, http.MethodPut, key, nil, body, payloadHash, meta, cond)
		if err != nil {
			return err
		}
		req.ContentLength = size
		applyContentHeaders(req, contentType, contentEncoding)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		return handlePutResponse(resp, cond)
	}
	if cond.isConditional() {
		return attempt()
	}
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return attempt()
	}, s3RetryableFor(ctx))
}

// deleteObject deletes an object by key, retrying a transient failure like
// getObject. A missing object (404) is treated as already deleted, matching
// S3's own idempotent DELETE semantics.
func (c *Client) deleteObject(ctx context.Context, key string) error {
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodDelete, key, nil, nil, emptySHA256, nil, putCondition{})
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3DeleteFailed, resp))
		}
		return nil
	}, s3RetryableFor(ctx))
}

// deleteObjectsMaxKeys is the maximum number of keys S3's Multi-Object
// Delete (DeleteObjects) accepts in a single request.
const deleteObjectsMaxKeys = 1000

// deleteAllUnderPrefix lists and deletes every object under prefix, one
// DeleteObjects batch per list page, so memory stays bounded to a single page
// (<=1000 keys) rather than the whole key set. A page is chunked to
// deleteObjectsMaxKeys defensively, in case a non-standard endpoint returns a
// larger page than S3's 1000 default.
func (c *Client) deleteAllUnderPrefix(ctx context.Context, prefix string) error {
	var token string
	for {
		page, err := c.listObjectsPage(ctx, prefix, token)
		if err != nil {
			return err
		}
		if err := c.deleteObjects(ctx, keysFrom(page.Contents), deleteObjectsMaxKeys); err != nil {
			return err
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	return nil
}

// deleteObjects deletes keys via one or more DeleteObjects batches of at most
// maxKeys each, so a page larger than S3's own 1000-key ceiling still gets
// chunked correctly. maxKeys is an injectable test seam; production always
// passes deleteObjectsMaxKeys.
func (c *Client) deleteObjects(ctx context.Context, keys []string, maxKeys int) error {
	for start := 0; start < len(keys); start += maxKeys {
		end := min(start+maxKeys, len(keys))
		if err := c.deleteObjectsBatch(ctx, keys[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// deleteRequest is the S3 Multi-Object Delete request body. Quiet suppresses
// per-key <Deleted> entries in the response, so only <Error> entries come
// back - all deleteObjectsBatch needs to detect a partial failure.
type deleteRequest struct {
	XMLName xml.Name            `xml:"Delete"`
	Objects []deleteObjectEntry `xml:"Object"`
	Quiet   bool                `xml:"Quiet"`
}

// deleteObjectEntry names one object key in a deleteRequest. It is named
// deleteObjectEntry, rather than deleteObject, to avoid clashing with the
// Client's existing single-object deleteObject method.
type deleteObjectEntry struct {
	Key string `xml:"Key"`
}

// deleteResult is the S3 Multi-Object Delete response body in Quiet mode:
// only failed keys are reported, each as an <Error> entry.
type deleteResult struct {
	XMLName xml.Name      `xml:"DeleteResult"`
	Errors  []deleteError `xml:"Error"`
}

// deleteError is one per-key failure reported by a DeleteObjects call.
type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// deleteObjectsBatch issues one DeleteObjects (POST ?delete) request for up
// to deleteObjectsMaxKeys keys, retrying a transient failure like the other
// idempotent verbs. The request body is built with encoding/xml rather than
// string templating: an object key is bucket-derived (a trust boundary - see
// the package's trust-model notes), so templating it into the XML body would
// be an XML-injection vector. A 200 status is never itself treated as
// success: S3 reports a per-key failure as an <Error> element inside a 200
// body, so the body is always parsed and any <Error> is surfaced as a
// terminal failure via errS3DeleteFailed (s3Retryable's default-deny
// classifies a plain error as non-retryable, so a per-key failure fails this
// call closed rather than being silently retried).
func (c *Client) deleteObjectsBatch(ctx context.Context, keys []string) error {
	objects := make([]deleteObjectEntry, len(keys))
	for i, key := range keys {
		objects[i] = deleteObjectEntry{Key: key}
	}
	body, err := xml.Marshal(deleteRequest{Quiet: true, Objects: objects})
	if err != nil {
		return err
	}
	payload := append([]byte(xml.Header), body...)

	hash := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(hash[:])
	// Content-MD5 is the S3-mandated protocol integrity header for
	// DeleteObjects (S3 rejects a request missing it), not a security
	// primitive - the payload is already authenticated via the SigV4
	// signature over its sha256 hash.
	sum := md5.Sum(payload) //nolint:gosec // see the Content-MD5 comment above
	contentMD5 := base64.StdEncoding.EncodeToString(sum[:])

	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodPost, "", url.Values{"delete": {""}},
			bytes.NewReader(payload), payloadHash, nil, putCondition{})
		if err != nil {
			return err
		}
		req.ContentLength = int64(len(payload))
		req.Header.Set("Content-Type", "application/xml")
		req.Header.Set("Content-MD5", contentMD5)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode != http.StatusOK {
			return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3DeleteFailed, resp))
		}
		data, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, helpers.S3ListMaxSize))
		if err != nil {
			// NewSizeLimitedReader's helpers.ErrResponseTooLarge is a bare size
			// ceiling shared with the artifact-download and metadata-fetch
			// surfaces, so it is wrapped here naming this one - an S3
			// batch-delete response - to keep errors.Is matching intact while
			// telling an operator which response actually overran.
			return fmt.Errorf("s3 batch-delete response: %w", err)
		}
		var result deleteResult
		if err := xml.Unmarshal(data, &result); err != nil {
			return err
		}
		if len(result.Errors) > 0 {
			e := result.Errors[0]
			return fmt.Errorf("%w: %d of %d keys failed (first: %q %s: %s)",
				errS3DeleteFailed, len(result.Errors), len(keys), e.Key, e.Code, e.Message)
		}
		return nil
	}, s3RetryableFor(ctx))
}

// listObjects returns object keys under the given prefix.
func (c *Client) listObjects(ctx context.Context, prefix string) ([]string, error) {
	keys := []string{}
	var token string
	for {
		result, err := c.listObjectsPage(ctx, prefix, token)
		if err != nil {
			return nil, err
		}
		keys = appendKeys(keys, result.Contents)
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}
	return keys, nil
}

func resolvePayloadHash(body io.ReadSeeker, payloadHash string) (string, error) {
	if payloadHash != "" {
		return payloadHash, nil
	}
	hash, err := hashReader(body)
	if err != nil {
		return "", err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hash, nil
}

func applyContentHeaders(req *http.Request, contentType, contentEncoding string) {
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
}

// handlePutResponse turns one PUT response into this package's vocabulary.
// Every status but 404 means the same thing whatever the write carried; 404 is
// the one whose meaning is decided by the precondition, which is why cond is a
// parameter rather than something the caller compensates for afterwards.
//
// A 404 answering an unconditional or create-if-absent PUT names the bucket:
// the write asserted nothing about an existing object, so the only thing S3
// can report missing is the container. A 404 answering a compare-and-swap
// names the object instead - S3 answers one when the key no longer exists,
// which happens exactly when a delete lands between the caller's read of the
// ETag and this write. Reporting that as errS3BucketNotFound would be wrong
// twice over: it accuses a bucket that is demonstrably there, and it hands a
// caller that could have retried immediately an unavailable-backend error
// instead (see reclaimIfExpired, whose lock object is deleted by any holder
// releasing it).
func handlePutResponse(resp *http.Response, cond putCondition) error {
	switch resp.StatusCode {
	case http.StatusPreconditionFailed:
		return errS3PreconditionFailed
	case http.StatusNotFound:
		if cond.ifMatch != "" {
			return errS3NotFound
		}
		return errS3BucketNotFound
	case http.StatusOK, http.StatusNoContent:
		return nil
	default:
		return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3PutFailed, resp))
	}
}

// listObjectsPage fetches one ListObjectsV2 page, retrying the whole
// request-read-parse cycle on a transient failure. bucketRequest is
// single-shot, so this outer retry is the sole layer: it recovers both a
// transient status (surfaced by bucketRequest as a retryable-classified
// error) and a body-read stall (helpers.ErrReadStalled) during the io.ReadAll
// here, which happens after bucketRequest has already returned a 200, within
// one bounded attempt budget.
func (c *Client) listObjectsPage(ctx context.Context, prefix, token string) (listBucketResult, error) {
	query := url.Values{}
	query.Set("list-type", "2")
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if token != "" {
		query.Set("continuation-token", token)
	}
	var result listBucketResult
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		resp, err := c.bucketRequest(ctx, http.MethodGet, query)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, helpers.S3ListMaxSize))
		_ = resp.Body.Close()
		if err != nil {
			// NewSizeLimitedReader's helpers.ErrResponseTooLarge is a bare size
			// ceiling shared with the artifact-download and metadata-fetch
			// surfaces, so it is wrapped here naming this one - an S3 listing
			// response - to keep errors.Is matching intact while telling an
			// operator which response actually overran.
			return fmt.Errorf("s3 listing response: %w", err)
		}
		var parsed listBucketResult
		if err := xml.Unmarshal(data, &parsed); err != nil {
			return err
		}
		result = parsed
		return nil
	}, s3RetryableFor(ctx))
	if err != nil {
		return listBucketResult{}, err
	}
	return result, nil
}

func appendKeys(dst []string, contents []listBucketContent) []string {
	for _, item := range contents {
		if item.Key != "" {
			dst = append(dst, item.Key)
		}
	}
	return dst
}

// keysFrom extracts each entry's key from a single ListObjectsV2 page's
// Contents, skipping any empty key (mirrors appendKeys's same defensive
// skip). Unlike appendKeys, which accumulates across every page of a full
// listing, this is scoped to one page's worth of keys - deleteAllUnderPrefix
// deletes a page at a time precisely so it never needs the accumulated form.
func keysFrom(contents []listBucketContent) []string {
	keys := make([]string, 0, len(contents))
	for _, item := range contents {
		if item.Key != "" {
			keys = append(keys, item.Key)
		}
	}
	return keys
}

// ensureBucket creates the bucket when it does not exist.
func (c *Client) ensureBucket(ctx context.Context) error {
	if err := c.headBucket(ctx); err != nil {
		if errors.Is(err, errS3BucketNotFound) {
			return c.createBucket(ctx)
		}
		return err
	}
	return nil
}

// headBucket checks whether the configured bucket exists. Like headObject,
// this is a HEAD request with no response body, so its failure stays
// status-only rather than going through s3StatusError. It retries a
// transient failure like the other idempotent verbs.
func (c *Client) headBucket(ctx context.Context) error {
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodHead, "", nil, nil, emptySHA256, nil, putCondition{})
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return errS3BucketNotFound
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return wrapRetryableStatus(resp.StatusCode, fmt.Errorf("%w: %s", errS3BucketHeadFailed, resp.Status))
		}
		return nil
	}, s3RetryableFor(ctx))
}

// createBucket sends a CreateBucket request with region configuration.
func (c *Client) createBucket(ctx context.Context) error {
	var (
		body        io.ReadSeeker
		contentType string
		contentSize int64
		payloadHash = emptySHA256
	)
	if c.cfg.Region != "" && c.cfg.Region != "us-east-1" {
		payload := fmt.Appendf(nil,
			"<CreateBucketConfiguration xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\">"+
				"<LocationConstraint>%s</LocationConstraint>"+
				"</CreateBucketConfiguration>",
			c.cfg.Region,
		)
		hash := sha256.Sum256(payload)
		payloadHash = hex.EncodeToString(hash[:])
		body = bytes.NewReader(payload)
		contentType = "application/xml"
		contentSize = int64(len(payload))
	}
	req, err := c.newRequest(ctx, http.MethodPut, "", nil, body, payloadHash, nil, putCondition{})
	if err != nil {
		return err
	}
	req.ContentLength = contentSize
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusConflict {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return s3StatusError(errS3CreateBucketFailed, resp)
	}
	return nil
}

// bucketRequest issues a single request against the bucket root. Its sole
// caller, listObjectsPage, owns the retry, so a transient status (returned
// here as a retryable-classified error via wrapRetryableStatus) and a
// body-read stall during the listing share one bounded attempt budget rather
// than nesting two. On a non-2xx it closes the response body and returns; on
// 200 it returns the response with its body still open for the caller to read.
func (c *Client) bucketRequest(ctx context.Context, method string, query url.Values) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, "", query, nil, emptySHA256, nil, putCondition{})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, errS3BucketNotFound
	}
	if resp.StatusCode != http.StatusOK {
		statusErr := wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3BucketRequestFailed, resp))
		_ = resp.Body.Close()
		return nil, statusErr
	}
	return resp, nil
}

// listBucketResult represents the S3 ListBucket XML response.
type listBucketResult struct {
	NextContinuationToken  string              `xml:"NextContinuationToken"`
	ContinuationToken      string              `xml:"ContinuationToken"`
	Prefix                 string              `xml:"Prefix"`
	Delimiter              string              `xml:"Delimiter"`
	StartAfter             string              `xml:"StartAfter"`
	ContinuationTokenStart string              `xml:"ContinuationTokenStart"`
	Contents               []listBucketContent `xml:"Contents"`
	CommonPrefixes         []listBucketPrefix  `xml:"CommonPrefixes"`
	KeyCount               int                 `xml:"KeyCount"`
	MaxKeys                int                 `xml:"MaxKeys"`
	IsTruncated            bool                `xml:"IsTruncated"`
}

// listBucketContent represents an object entry in a ListBucket response.
type listBucketContent struct {
	Key string `xml:"Key"`
}

// listBucketPrefix represents a common prefix entry in a ListBucket response.
type listBucketPrefix struct {
	Prefix string `xml:"Prefix"`
}

// newRequest builds and signs a request for the given object key.
func (c *Client) newRequest(
	ctx context.Context,
	method, key string,
	query url.Values,
	body io.ReadSeeker,
	payloadHash string,
	meta map[string]string,
	cond putCondition,
) (*http.Request, error) {
	reqURL, host, canonicalURI, canonicalQuery := c.requestURL(key, query)
	if payloadHash == "" {
		payloadHash = emptySHA256
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	if c.cfg.SessionToken.IsSet() {
		// Reveal here is the value going onto the wire: this header is where
		// a session token is transmitted, so there is nothing further to
		// protect it from.
		req.Header.Set("X-Amz-Security-Token", c.cfg.SessionToken.Reveal())
	}
	for key, value := range meta {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		name := "X-Amz-Meta-" + helpers.UpperFirstRune(strings.TrimSpace(key))
		req.Header.Set(name, trimmed)
	}
	if cond.ifNoneMatch {
		req.Header.Set("If-None-Match", "*")
	}
	if cond.ifMatch != "" {
		req.Header.Set("If-Match", cond.ifMatch)
	}
	canonicalHeaders, signedHeaders := canonicalizeHeaders(host, req.Header)
	req.Header.Set("Authorization", c.signRequest(method, canonicalURI, canonicalQuery, amzDate, payloadHash, canonicalHeaders, signedHeaders))
	return req, nil
}

// requestURL builds the request URL and canonical components.
func (c *Client) requestURL(key string, query url.Values) (string, string, string, string) {
	endpoint := c.cfg.Endpoint
	host := c.endpointHost
	key = strings.TrimLeft(key, "/")

	var objectPath string
	if c.cfg.PathStyle {
		if key == "" {
			objectPath = "/" + c.cfg.Bucket
		} else {
			objectPath = "/" + c.cfg.Bucket + "/" + key
		}
	} else {
		host = c.cfg.Bucket + "." + host
		objectPath = "/" + key
	}

	// escapedPath is used for BOTH the signed canonical URI and the sent
	// wire path below, so they are byte-identical: signing and sending
	// through two different encoders (net/url's escaping vs. this one) is
	// exactly what let a reserved character in a key produce a
	// SignatureDoesNotMatch (403) - see awsURIEncode's doc comment.
	escapedPath := encodePath(objectPath)
	canonicalURI := escapedPath
	canonicalQuery := canonicalizeQuery(query)
	reqURL := endpoint + escapedPath
	if !c.cfg.PathStyle && c.endpointScheme != "" {
		reqURL = c.endpointScheme + "://" + host + escapedPath
	}
	if canonicalQuery != "" {
		reqURL += "?" + canonicalQuery
	}
	return reqURL, host, canonicalURI, canonicalQuery
}

// signingKeyForDate returns the SigV4 signing key for date, deriving it once
// per UTC date and reusing it for every request on the same date. Secret and
// region are immutable for the client's lifetime, so date is the only cache
// key; the key is recomputed when the date rolls over at UTC midnight, since a
// key for the wrong date signs the wrong scope and the request would 403. The
// cached slice is never mutated after derivation, so returning it directly
// (no copy) is race-free once the field access is guarded.
func (c *Client) signingKeyForDate(date string) []byte {
	c.signing.mu.Lock()
	defer c.signing.mu.Unlock()
	if c.signing.key == nil || c.signing.date != date {
		// Reveal here feeds the SigV4 HMAC chain, the one operation that needs
		// the plaintext secret. deriveSigningKey keeps a plain-string
		// signature deliberately: it is cryptography over key material and has
		// no business knowing this program's configuration types.
		c.signing.key = deriveSigningKey(c.cfg.SecretKey.Reveal(), date, c.cfg.Region)
		c.signing.date = date
	}
	return c.signing.key
}

// signRequest builds the AWS SigV4 Authorization header value.
func (c *Client) signRequest(
	method string,
	canonicalURI string,
	canonicalQuery string,
	amzDate string,
	payloadHash string,
	canonicalHeaders string,
	signedHeaders string,
) string {
	date := amzDate[:8]
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", date, c.cfg.Region)
	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(hash[:]),
	}, "\n")

	signingKey := c.signingKeyForDate(date)
	signature := hmacSHA256Hex(signingKey, stringToSign)
	return fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey,
		scope,
		signedHeaders,
		signature,
	)
}

// canonicalizeHeaders returns canonical and signed header strings.
func canonicalizeHeaders(host string, headers http.Header) (string, string) {
	entries := map[string]string{
		"host": normalizeHeaderValue([]string{host}),
	}
	for name, values := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		entries[lower] = normalizeHeaderValue(values)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name)
		canonical.WriteString(":")
		canonical.WriteString(entries[name])
		canonical.WriteString("\n")
	}

	return canonical.String(), strings.Join(names, ";")
}

// normalizeHeaderValue trims and collapses whitespace in header values.
func normalizeHeaderValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(normalized, ",")
}

// canonicalizeQuery returns the canonical query string.
func canonicalizeQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(values))
	for _, key := range keys {
		vals := values[key]
		sort.Strings(vals)
		for _, value := range vals {
			pairs = append(pairs, awsEncode(key)+"="+awsEncode(value))
		}
	}
	return strings.Join(pairs, "&")
}

// awsEncode encodes a query value according to AWS canonical rules.
func awsEncode(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	escaped = strings.ReplaceAll(escaped, "%7E", "~")
	return escaped
}

// encodePath encodes a path for signature calculations and, since requestURL
// now builds the sent URL from this same result, for the wire request too.
// It defers to awsURIEncode with slashes left literal, so "/" segment
// boundaries survive untouched while every other byte gets S3's exact
// percent-encoding.
func encodePath(value string) string {
	if value == "" {
		return "/"
	}
	return awsURIEncode(value, false)
}

// upperHex is the hex digit alphabet AWS SigV4 requires for percent-encoding:
// uppercase, unlike net/url's lowercase output.
const upperHex = "0123456789ABCDEF"

// awsURIEncode percent-encodes s per AWS SigV4 UriEncode: A-Za-z0-9 and -._~
// stay literal; "/" stays literal when encodeSlash is false (path) and becomes
// %2F otherwise (query); every other byte is percent-encoded with UPPERCASE
// hex. It iterates raw UTF-8 bytes, so a multibyte rune becomes its individual
// %XX bytes. Stricter than url.PathEscape (which leaves +$&,;=:@ literal);
// matching S3's exact encoding on both the signed canonical URI and the wire
// path is what stops a reserved character producing a SignatureDoesNotMatch.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	// Range over the integer length, not the string itself: ranging over a
	// string decodes UTF-8 and would skip the byte indices of a multibyte
	// rune's continuation bytes, which is exactly what this loop must not
	// do - it needs every raw byte, since a multibyte rune must become its
	// individual %XX escapes.
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

// deriveSigningKey derives the signing key for the given date and region.
func deriveSigningKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	return hmacSHA256(kService, "aws4_request")
}

// hmacSHA256 returns the HMAC-SHA256 of data using key.
func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

// hmacSHA256Hex returns the hex-encoded HMAC-SHA256 of data.
func hmacSHA256Hex(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, data))
}

// hashReader returns the SHA256 hash of the reader's contents.
func hashReader(r io.Reader) (string, error) {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
