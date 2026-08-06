package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// slowDownErrorBody is a representative S3 XML <Error> document: a 503
// response body carrying a throttling error code and a human-readable
// message, exactly the shape s3StatusError is meant to surface.
const slowDownErrorBody = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message></Error>`

// TestGetObjectSurfacesXMLErrorDetails proves that a non-2xx GET response
// carrying an S3 XML error body has its Code and Message folded into the
// returned error, while the error stays matchable against errS3GetFailed via
// errors.Is. 503 is a retryable status, so the failure is armed
// indefinitely: getObject now retries it internally, and this asserts the
// enrichment still holds on the terminal error once retries are exhausted.
func TestGetObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-error-object"
	fake.failNextWithBody(key, http.MethodGet, http.StatusServiceUnavailable, -1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
	if got := fake.requestCount(key, http.MethodGet); got != s3RetryMaxAttempts {
		t.Fatalf("expected exactly s3RetryMaxAttempts=%d GET attempts against a persistently failing key, got %d",
			s3RetryMaxAttempts, got)
	}
}

// TestDeleteObjectSurfacesXMLErrorDetails proves the same enrichment applies
// to deleteObject, exercising a second call site beyond getObject. The
// failure is armed indefinitely since deleteObject now retries a 503
// internally; a bounded count would be consumed by the retries and the
// final attempt would then observe a plain (never-created) key as already
// deleted, masking the failure this test means to exercise.
func TestDeleteObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "delete-error-object"
	fake.failNextWithBody(key, http.MethodDelete, http.StatusServiceUnavailable, -1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := b.client.deleteObject(ctx, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3DeleteFailed) {
		t.Fatalf("expected errors.Is(err, errS3DeleteFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
}

// TestPutObjectSurfacesXMLErrorDetails proves handlePutResponse's default
// (non-2xx, non-precondition, non-not-found) branch enriches its error the
// same way, covering an overwrite PUT that the backend rejects with a
// throttling response. This is an unconditional PUT, which now retries a
// 503 internally, so the failure is armed indefinitely: a bounded count
// would let a later retry attempt succeed in writing the object instead of
// exercising the failure this test means to cover.
func TestPutObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-error-object"
	fake.failNextWithBody(key, http.MethodPut, http.StatusServiceUnavailable, -1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	payload := []byte("payload")
	err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)), "", "", nil, putCondition{}, "")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3PutFailed) {
		t.Fatalf("expected errors.Is(err, errS3PutFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
}

// TestGetObjectFallsBackToStatusOnlyOnMalformedBody proves that a non-2xx
// body that is not a valid S3 XML error document yields the original
// status-only error form, without ever propagating the underlying
// XML-parse failure, and without disturbing errors.Is matching. 500 is a
// retryable status, so the failure is armed indefinitely to survive
// getObject's internal retries.
func TestGetObjectFallsBackToStatusOnlyOnMalformedBody(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-malformed-body"
	fake.failNextWithBody(key, http.MethodGet, http.StatusInternalServerError, -1, []byte("not xml at all"))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for a malformed body, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), http.StatusText(http.StatusInternalServerError)) {
		t.Fatalf("expected the status text in the fallback error, got %q", err.Error())
	}
}

// TestGetObjectFallsBackToStatusOnlyOnEmptyBody proves the same fallback for
// a response with no body at all (the common case for many real S3 error
// responses returned without a body, e.g. from a misconfigured proxy). 502
// is a retryable status, so the failure is armed indefinitely to survive
// getObject's internal retries.
func TestGetObjectFallsBackToStatusOnlyOnEmptyBody(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-empty-body"
	fake.failNext(key, http.MethodGet, http.StatusBadGateway, -1)

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for an empty body, got %q", err.Error())
	}
}

// TestHeadObjectStaysStatusOnly proves headObject never appends a code or
// message even when a body is armed on the forced failure: HEAD responses
// carry no entity body (net/http elides it), so headObject's error is the
// status-only form by construction, not merely by the fake choosing not to
// send one. The failure is armed indefinitely since headObject now retries
// a 503 internally.
func TestHeadObjectStaysStatusOnly(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "head-error-object"
	fake.failNextWithBody(key, http.MethodHead, http.StatusServiceUnavailable, -1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err := b.client.headObject(ctx, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3HeadFailed) {
		t.Fatalf("expected errors.Is(err, errS3HeadFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for a HEAD response, got %q", err.Error())
	}
}

// TestGetObjectRetriesTransientFailureThenSucceeds proves getObject
// transparently recovers from a bounded run of transient 503s: it seeds a
// real object, arms exactly two forced failures on it, and confirms the
// call still succeeds - reading back the object's real body - after
// exactly three GET attempts (two failures plus the succeeding one), rather
// than surfacing the second failure to the caller.
func TestGetObjectRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-retry-then-succeed"
	payload := []byte("recovered payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		"text/plain", "", nil, putCondition{}, ""); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	fake.failNext(key, http.MethodGet, http.StatusServiceUnavailable, 2)

	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		t.Fatalf("expected getObject to recover after two transient failures, got %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read recovered body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected the recovered body %q, got %q", payload, body)
	}
	if got := fake.requestCount(key, http.MethodGet); got != 3 {
		t.Fatalf("expected exactly 3 GET attempts (2 failures + 1 success), got %d", got)
	}
}

// TestHeadObjectRetriesTransientFailureThenSucceeds mirrors
// TestGetObjectRetriesTransientFailureThenSucceeds for headObject.
func TestHeadObjectRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "head-retry-then-succeed"
	payload := []byte("payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		"text/plain", "", nil, putCondition{}, ""); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	fake.failNext(key, http.MethodHead, http.StatusServiceUnavailable, 2)

	if _, err := b.client.headObject(ctx, key); err != nil {
		t.Fatalf("expected headObject to recover after two transient failures, got %v", err)
	}
	if got := fake.requestCount(key, http.MethodHead); got != 3 {
		t.Fatalf("expected exactly 3 HEAD attempts (2 failures + 1 success), got %d", got)
	}
}

// TestDeleteObjectRetriesTransientFailureThenSucceeds mirrors
// TestGetObjectRetriesTransientFailureThenSucceeds for deleteObject.
func TestDeleteObjectRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "delete-retry-then-succeed"
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fake.failNext(key, http.MethodDelete, http.StatusServiceUnavailable, 2)

	if err := b.client.deleteObject(ctx, key); err != nil {
		t.Fatalf("expected deleteObject to recover after two transient failures, got %v", err)
	}
	if got := fake.requestCount(key, http.MethodDelete); got != 3 {
		t.Fatalf("expected exactly 3 DELETE attempts (2 failures + 1 success), got %d", got)
	}
}

// TestPutObjectUnconditionalRetriesTransientFailureThenSucceeds proves an
// unconditional (overwrite) putObject recovers from a bounded run of
// transient 503s, re-seeking and resending its body on each retry, and
// confirms the object actually landed once the call succeeds.
func TestPutObjectUnconditionalRetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-retry-then-succeed"
	payload := []byte("recovered payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fake.failNext(key, http.MethodPut, http.StatusServiceUnavailable, 2)

	if err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		"text/plain", "", nil, putCondition{}, ""); err != nil {
		t.Fatalf("expected putObject to recover after two transient failures, got %v", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 3 {
		t.Fatalf("expected exactly 3 PUT attempts (2 failures + 1 success), got %d", got)
	}

	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		t.Fatalf("getObject after recovery: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read written body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected the written body %q, got %q", payload, body)
	}
}

// TestPutObjectConditionalDoesNotRetryOnTransientFailure is the core
// lock-deadlock guard: a conditional create-if-absent PUT (ifNoneMatch)
// must never be retried, even against a persistently failing transient
// status. A lost-success retry would observe 412 (the object it just
// created now exists) and misreport its own success as contention, which
// the distributed lock's acquireLock loop cannot distinguish from a live
// holder. This asserts the call is issued exactly once and returns the
// failure as-is.
func TestPutObjectConditionalDoesNotRetryOnTransientFailure(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-conditional-no-retry"
	payload := []byte("payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fake.failNext(key, http.MethodPut, http.StatusServiceUnavailable, -1)

	err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		"text/plain", "", nil, putCondition{ifNoneMatch: true}, "")
	if !errors.Is(err, errS3PutFailed) {
		t.Fatalf("expected errors.Is(err, errS3PutFailed), got %v", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 1 {
		t.Fatalf("expected a conditional PUT to be issued exactly once despite a retryable status, got %d attempts", got)
	}
}

// TestGetObjectNotFoundIsNotRetried confirms a plain 404 (no forced failure
// needed - the key simply was never written) is reported as errS3NotFound
// after exactly one attempt, never retried: errS3NotFound is a terminal,
// non-transient outcome.
func TestGetObjectNotFoundIsNotRetried(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-never-written"
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	err := getObjectError(ctx, t, b, key)
	if !errors.Is(err, errS3NotFound) {
		t.Fatalf("expected errors.Is(err, errS3NotFound), got %v", err)
	}
	if got := fake.requestCount(key, http.MethodGet); got != 1 {
		t.Fatalf("expected exactly 1 GET attempt for a not-found key, got %d", got)
	}
}

// TestPutObjectPreconditionFailedIsNotRetried confirms a 412 on a
// conditional PUT is reported as errS3PreconditionFailed after exactly one
// attempt: it is both a non-retryable status and, independently, a
// conditional PUT is never retried regardless of status.
func TestPutObjectPreconditionFailedIsNotRetried(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-precondition-no-retry"
	payload := []byte("payload")
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fake.failNext(key, http.MethodPut, http.StatusPreconditionFailed, 1)

	err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		"text/plain", "", nil, putCondition{ifNoneMatch: true}, "")
	if !errors.Is(err, errS3PreconditionFailed) {
		t.Fatalf("expected errors.Is(err, errS3PreconditionFailed), got %v", err)
	}
	if got := fake.requestCount(key, http.MethodPut); got != 1 {
		t.Fatalf("expected exactly 1 PUT attempt for a precondition failure, got %d", got)
	}
}

// newRedirectRefusalFixture starts two httptest servers - a front server the
// returned *Client is configured to talk to, and a target server it must
// never reach - and builds the *Client from a fresh *http.Client dedicated
// to this fixture. When redirect is true, the front server answers every
// request with a 302 to the target; when false, it answers 200 directly, so
// the identical fixture also serves as this behavior's own positive control.
// The returned counters record how many requests each server received.
func newRedirectRefusalFixture(t *testing.T, redirect bool) (*Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	var target atomic.Int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(targetSrv.Close)

	var front atomic.Int32
	frontSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		front.Add(1)
		if redirect {
			http.Redirect(w, r, targetSrv.URL+r.URL.Path, http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(frontSrv.Close)

	cfg := config.S3CacheConfig{
		Endpoint:  frontSrv.URL,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	c, err := newClient(cfg, frontSrv.Client())
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c, &front, &target
}

// TestClientRefusesEndpointRedirect proves that a redirect answered by the
// configured S3 endpoint is refused rather than followed: getObject fails
// with helpers.ErrCacheBackendUnusable and NOT ALSO
// helpers.ErrCacheBackendUnavailable - the exclusivity variables.go's own
// partition doc requires of every sentinel that can escape this package,
// checked here against the actual error tree do() produces for this
// scenario rather than only against the bare errS3RedirectRefused variable
// (sentinel_class_test.go's own coverage) - the redirect target never
// receives a request, and the front endpoint is asked exactly once, proving
// the refusal is not itself retried as an ordinary transport failure.
func TestClientRefusesEndpointRedirect(t *testing.T) {
	t.Parallel()
	client, frontRequests, targetRequests := newRedirectRefusalFixture(t, true)

	resp, err := client.getObject(context.Background(), "redirect-object")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnusable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnusable), got %v", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected err to NOT also carry helpers.ErrCacheBackendUnavailable "+
			"(exclusive with helpers.ErrCacheBackendUnusable per variables.go's partition), got %v", err)
	}
	// Documentary, not pinned: this cannot be the first failing line, because
	// any state that lets a request reach the target also makes getObject
	// succeed (the target answers 200), which the non-nil check above catches
	// first. It is kept as a direct statement of the containment property the
	// refusal exists to provide. Reordering the chain to "fix" that would
	// only make the non-nil check documentary instead.
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("expected zero requests reaching the redirect target, got %d", got)
	}
	// Pinned, and not by the same state as the exclusivity check above: a
	// classifier that retried the refusal would leave the error tree clean
	// and drive this count to s3RetryMaxAttempts, failing here alone.
	if got := frontRequests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request to the configured endpoint (not retried), got %d", got)
	}
}

// TestClientRefusesEndpointRedirectPositiveControl is
// TestClientRefusesEndpointRedirect's positive control on the identical
// fixture: with the front server answering 200 instead of redirecting,
// getObject succeeds. This proves the refusal above exercises the redirect
// path itself, rather than some other property of the fixture (a bad
// signature, an unreachable endpoint) that would fail getObject regardless
// of whether a redirect was ever involved.
func TestClientRefusesEndpointRedirectPositiveControl(t *testing.T) {
	t.Parallel()
	client, frontRequests, targetRequests := newRedirectRefusalFixture(t, false)

	resp, err := client.getObject(context.Background(), "redirect-object")
	if err != nil {
		t.Fatalf("expected getObject to succeed against a direct 200 response, got %v", err)
	}
	_ = resp.Body.Close()
	if got := frontRequests.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request to the configured endpoint, got %d", got)
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("expected zero requests to the unused redirect target, got %d", got)
	}
}

// TestS3RedirectPolicyDoesNotAffectTheSharedClient is the load-bearing test
// for newClient's copy-not-mutate decision: it builds a single *http.Client,
// passes it to newClient, and then drives a real redirect through that
// SAME, original *http.Client - never through the *Client newClient
// returned. The redirect is still followed and reaches the target exactly
// once, proving newClient's CheckRedirect assignment landed on a private
// copy rather than on the caller's own client, which internal/galaxy/fetch
// shares for Galaxy metadata and artifact downloads and whose own tests
// depend on the default (redirect-following) behavior.
func TestS3RedirectPolicyDoesNotAffectTheSharedClient(t *testing.T) {
	t.Parallel()

	var targetRequests atomic.Int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(targetSrv.Close)

	frontSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(frontSrv.Close)

	shared := frontSrv.Client()

	cfg := config.S3CacheConfig{
		Endpoint:  frontSrv.URL,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	if _, err := newClient(cfg, shared); err != nil {
		t.Fatalf("newClient: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, frontSrv.URL+"/probe", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext: %v", err)
	}
	resp, err := shared.Do(req)
	if err != nil {
		t.Fatalf("expected the original, shared client to still follow a redirect, got %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected a 200 from the followed redirect, got %d", resp.StatusCode)
	}
	if got := targetRequests.Load(); got != 1 {
		t.Fatalf("expected the redirect to be followed and reach the target exactly once, got %d", got)
	}
}

// assertContainsCodeAndMessage fails the test unless err's message contains
// both code and message, which s3StatusError appends in parentheses after
// the HTTP status line.
func assertContainsCodeAndMessage(t *testing.T, err error, code, message string) {
	t.Helper()
	got := err.Error()
	if !strings.Contains(got, code) {
		t.Fatalf("expected error to contain code %q, got %q", code, got)
	}
	if !strings.Contains(got, message) {
		t.Fatalf("expected error to contain message %q, got %q", message, got)
	}
}

// getObjectError calls getObject and returns the resulting error. getObject
// already closes the response body and returns a nil *http.Response on every
// error path, so there is nothing to close here in practice; the nil check
// simply keeps this helper correct even if that contract ever changes.
func getObjectError(ctx context.Context, t *testing.T, b *Backend, key string) error {
	t.Helper()
	resp, err := b.client.getObject(ctx, key)
	if resp != nil {
		defer func() {
			_ = resp.Body.Close()
		}()
	}
	return err
}

// getObjectRetryResponseHeaderTimeout is the ResponseHeaderTimeout every row
// below arms: short enough to keep the test fast, long enough that the local
// loopback dial itself never races it.
const getObjectRetryResponseHeaderTimeout = 150 * time.Millisecond

// TestGetObjectRetriesAResponseHeaderTimeout is the headline case this
// classifier exists for: an S3-compatible endpoint that accepts a connection
// and then never answers, the exact shape a black-holed or overloaded object
// store produces. This failure satisfies errors.Is(err, context.DeadlineExceeded)
// while the caller's own context is still live, so a classifier keying on the
// error's shape rather than on the caller's context would refuse it and
// getObject would spend only the first of its s3RetryMaxAttempts attempts -
// which is exactly what this test counts. It asserts that by counting accepted
// TCP connections directly, rather than trusting a response, since the server
// here never produces one.
func TestGetObjectRetriesAResponseHeaderTimeout(t *testing.T) {
	t.Parallel()

	ln, accepted := newAcceptingNeverRespondingListener(t)
	httpClient := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: getObjectRetryResponseHeaderTimeout}}

	cfg := config.S3CacheConfig{
		Endpoint:  "http://" + ln.Addr().String(),
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	client, err := newClient(cfg, httpClient)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	resp, err := client.getObject(context.Background(), "some-key")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected an error against an endpoint that never answers, got nil")
	}
	if !errors.Is(err, errS3TransportFailed) {
		t.Fatalf("expected errors.Is(err, errS3TransportFailed), got %v", err)
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if got := accepted.Load(); got != s3RetryMaxAttempts {
		t.Fatalf("expected exactly s3RetryMaxAttempts=%d accepted connections, got %d", s3RetryMaxAttempts, got)
	}
}

// TestGetObjectRecoversFromATransientTransportFailure is
// TestGetObjectRetriesAResponseHeaderTimeout's positive control: it proves
// the errS3TransportFailed retry arm actually RECOVERS a request rather than
// merely counting attempts before giving up. The server hijacks the raw
// connection and closes it without writing a status line for the first two
// requests - a genuine transport failure (the client observes a closed
// connection, never a response) distinct from a slow response - and answers
// normally on the third.
func TestGetObjectRecoversFromATransientTransportFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	const key = "transient-transport-object"
	payload := []byte("recovered after a transport failure")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 2 {
			// t.Errorf, not t.Fatal: this runs on the server's own handler
			// goroutine, where a Fatal would kill that goroutine and leave
			// the client hanging on a request nobody answers, turning a
			// named failure into a timeout.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("ResponseWriter does not support hijacking")
				return
			}
			conn, _, hijackErr := hijacker.Hijack()
			if hijackErr != nil {
				t.Errorf("Hijack: %v", hijackErr)
				return
			}
			_ = conn.Close() // close without writing anything: a genuine transport failure, not a status.
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	cfg := config.S3CacheConfig{
		Endpoint:  srv.URL,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	client, err := newClient(cfg, srv.Client())
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	resp, err := client.getObject(context.Background(), key)
	if err != nil {
		t.Fatalf("expected getObject to recover from a transient transport failure, got %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read recovered body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected the recovered body %q, got %q", payload, body)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("expected exactly 3 requests (2 transport failures + 1 success), got %d", got)
	}
}

// sessionTokenHeaderCase is one row for TestSessionTokenHeader.
type sessionTokenHeaderCase struct {
	name  string
	token config.Secret
	want  string
}

// TestSessionTokenHeader pins the branch that decides whether an S3 request
// carries X-Amz-Security-Token, and what it carries. The zero-value row is
// what pins IsSet as the predicate: a Secret that was never configured must
// leave the header off entirely rather than send an empty one, which is a
// header a strict endpoint rejects rather than ignores.
func TestSessionTokenHeader(t *testing.T) {
	t.Parallel()

	cases := []sessionTokenHeaderCase{
		{name: "configured token is sent", token: config.NewSecret("st"), want: "st"},
		{name: "unset token sends no header", token: config.Secret{}, want: ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got string
			var seen atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Amz-Security-Token")
				seen.Store(true)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			cfg := config.S3CacheConfig{
				Endpoint:     srv.URL,
				Bucket:       "test",
				Region:       "us-east-1",
				AccessKey:    "x",
				SecretKey:    config.NewSecret("y"),
				SessionToken: tt.token,
				PathStyle:    true,
				Enabled:      true,
			}
			c, err := newClient(cfg, srv.Client())
			if err != nil {
				t.Fatalf("newClient: %v", err)
			}
			resp, err := c.getObject(context.Background(), "any-object")
			if err != nil {
				t.Fatalf("getObject: %v", err)
			}
			_ = resp.Body.Close()

			if !seen.Load() {
				t.Fatalf("the endpoint was never reached, so the header assertion below proves nothing")
			}
			if got != tt.want {
				t.Fatalf("X-Amz-Security-Token = %q, want %q", got, tt.want)
			}
		})
	}
}
