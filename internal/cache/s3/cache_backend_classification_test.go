package s3

// This file pins the cache-backend exit-code classification variables.go's
// partition doc describes: a status-level failure exhausting its retry
// budget and a transport-level failure that never reached an HTTP response
// both carry helpers.ErrCacheBackendUnavailable, a probe failure discovered
// once at Open carries helpers.ErrCacheBackendUnusable instead, and neither
// ever carries the other - nor does either ever carry a context signal a
// caller-driven cancellation should keep for itself.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestLoadStoreClassifiesExhaustedGetRetriesAsCacheBackendUnavailable forces
// the snapshot object's GET to fail with a retryable 500 on every one of
// s3RetryMaxAttempts attempts: LoadStore's final error must still match both
// errS3GetFailed (the specific verb that failed) and
// helpers.ErrCacheBackendUnavailable (the class exitcode.FromError branches
// on), since errS3GetFailed's own declaration in variables.go now wraps the
// latter.
func TestLoadStoreClassifiesExhaustedGetRetriesAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	putStoreObject(ctx, t, b, helpers.StoreSnapshotSchemaVersion, nil)
	key := b.key(statePrefix, storeObject)
	fake.failNext(key, http.MethodGet, http.StatusInternalServerError, s3RetryMaxAttempts)

	_, err := b.LoadStore(ctx)
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
}

// TestSaveStoreClassifiesExhaustedPutRetriesAsCacheBackendUnavailable is
// TestLoadStoreClassifiesExhaustedGetRetriesAsCacheBackendUnavailable's PUT
// counterpart: SaveStore's final error over an unconditional PUT that fails
// with a retryable 500 on every attempt matches both errS3PutFailed and
// helpers.ErrCacheBackendUnavailable.
func TestSaveStoreClassifiesExhaustedPutRetriesAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	key := b.key(statePrefix, storeObject)
	fake.failNext(key, http.MethodPut, http.StatusInternalServerError, s3RetryMaxAttempts)

	err := b.SaveStore(ctx, store.New())
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if !errors.Is(err, errS3PutFailed) {
		t.Fatalf("expected errors.Is(err, errS3PutFailed), got %v", err)
	}
}

// TestClientDoClassifiesConnectionFailureAsCacheBackendUnavailable is the
// headline case this whole item exists for: a bucket that is DOWN (nobody
// listening) rather than merely slow. It points a Backend at a closed
// httptest listener - a connect refusal, not a status response - and
// confirms (a) Client.do's normalization reaches LoadStore's returned error
// as helpers.ErrCacheBackendUnavailable, and, separately, (b) that error
// satisfies NEITHER context.Canceled NOR context.DeadlineExceeded. Without
// (b) this test would prove nothing about the funnel's context exclusion:
// TestClientDoExcludesCallerCancellationFromCacheBackendUnavailable below
// names this test as its positive control precisely because (b) confirms
// helpers.ErrCacheBackendUnavailable is reachable through errors.Is here,
// which is what makes that test's own negative assertion meaningful rather
// than vacuous.
func TestClientDoClassifiesConnectionFailureAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()

	// A server that is started and immediately closed leaves its port
	// refusing connections, unlike an unroutable address (which would hang on
	// dial instead of failing fast) or a running server answering a non-2xx
	// status (a different failure shape, already covered by the two tests
	// above).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	cfg := config.S3CacheConfig{
		Endpoint:  closedURL,
		Bucket:    "test",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
	backend, err := New(cfg, http.DefaultClient, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = backend.LoadStore(context.Background())

	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("expected NOT errors.Is(err, context.Canceled), got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected NOT errors.Is(err, context.DeadlineExceeded), got %v", err)
	}
}

// TestClientDoClassifiesLiveTimeoutShapesAsCacheBackendUnavailable is the
// test the closed-listener headline case above cannot be: that fixture
// produces a connect refusal, the one shape an error-shape predicate would
// exclude from context.DeadlineExceeded anyway, so it is structurally
// incapable of killing a regression from Client.do's req.Context().Err()
// test back to that shape-based predicate. Both
// rows below instead reproduce the two outage shapes that DO satisfy
// errors.Is(err, context.DeadlineExceeded) while the caller's own context
// stays live - a black-holed endpoint (dial times out) and one that accepts a
// connection and then never answers (headers never arrive) - fully local and
// deterministic, with no external network dependency and no black-holed
// public address.
//
// Each row runs its precondition as a control FIRST: it calls c.client.Do(req)
// directly (bypassing Client.do entirely) and asserts the raw error satisfies
// errors.Is(rawErr, context.DeadlineExceeded) while req.Context().Err() is
// still nil. This is what stops the row passing vacuously if a future Go
// toolchain changes how a dial or response-header timeout is mapped to an
// error: the row fails loudly at the precondition instead of silently
// proving nothing about Client.do. Only once that precondition holds does the
// row call c.do(req) and assert the sentinel.
//
// KILLING MUTATION, run and reverted: restoring the old error-shape predicate
// (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
// in place of req.Context().Err() != nil makes both rows fail, quoting the
// real transport error each shape produces on this toolchain (reported at the
// caller's line, since the helper calls t.Helper()):
//
//	cache_backend_classification_test.go:202: expected errors.Is(err, helpers.ErrCacheBackendUnavailable),
//	got Get "http://127.0.0.1:<port>/test/some-key": dial tcp 127.0.0.1:<port>: i/o timeout
//
//	cache_backend_classification_test.go:202: expected errors.Is(err, helpers.ErrCacheBackendUnavailable),
//	got Get "http://127.0.0.1:<port>/test/some-key": net/http: timeout awaiting response headers
//
// while TestClientDoClassifiesConnectionFailureAsCacheBackendUnavailable and
// TestClientDoExcludesCallerCancellationFromCacheBackendUnavailable both still
// PASS under the identical mutation - confirming neither of those tests can
// catch this regression, only these two rows can.
func TestClientDoClassifiesLiveTimeoutShapesAsCacheBackendUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		newTransport func(t *testing.T, addr string) http.RoundTripper
		name         string
	}{
		{
			// A 1ns dial timeout expires before even a local TCP handshake can
			// complete, so pointing it at a live listener (rather than an address
			// nobody listens on) reliably reproduces a genuine dial timeout - not
			// the connect-refusal shape the headline case above already covers.
			name: "dial timeout",
			newTransport: func(t *testing.T, _ string) http.RoundTripper {
				t.Helper()
				return &http.Transport{
					DialContext: (&net.Dialer{Timeout: time.Nanosecond}).DialContext,
				}
			},
		},
		{
			// ResponseHeaderTimeout fires once the connection is established (the
			// dial itself succeeds against a real, accepting listener) but no
			// response headers ever arrive, reproducing an endpoint that accepts a
			// connection and then never answers.
			name: "response header timeout",
			newTransport: func(t *testing.T, _ string) http.RoundTripper {
				t.Helper()
				return &http.Transport{ResponseHeaderTimeout: 150 * time.Millisecond}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertLiveTimeoutClassifiesAsCacheBackendUnavailable(t, tt.newTransport)
		})
	}
}

// assertLiveTimeoutClassifiesAsCacheBackendUnavailable runs one row's body:
// build a Client against a live listener using newTransport, run the
// precondition control against the raw *http.Client, then assert Client.do's
// own normalization. Split out of
// TestClientDoClassifiesLiveTimeoutShapesAsCacheBackendUnavailable purely to
// stay under the funlen budget; the two together still cover exactly the
// same two rows.
func assertLiveTimeoutClassifiesAsCacheBackendUnavailable(
	t *testing.T,
	newTransport func(t *testing.T, addr string) http.RoundTripper,
) {
	t.Helper()

	ln, _ := newAcceptingNeverRespondingListener(t)
	transport := newTransport(t, ln.Addr().String())
	httpClient := &http.Client{Transport: transport}

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
	req, err := client.newRequest(context.Background(), http.MethodGet, "some-key", nil, nil, emptySHA256, nil, putCondition{})
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}

	// Precondition control: the raw transport failure, bypassing Client.do
	// entirely, must already satisfy errors.Is(rawErr, context.DeadlineExceeded)
	// with a live caller context - otherwise this row proves nothing.
	rawResp, rawErr := client.client.Do(req)
	if rawResp != nil {
		_ = rawResp.Body.Close()
	}
	if !errors.Is(rawErr, context.DeadlineExceeded) {
		t.Fatalf("precondition: expected errors.Is(rawErr, context.DeadlineExceeded), got %v", rawErr)
	}
	if req.Context().Err() != nil {
		t.Fatalf("precondition: expected req.Context().Err() == nil, got %v", req.Context().Err())
	}

	resp, err := client.do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
}

// TestClientDoExcludesCallerCancellationFromCacheBackendUnavailable is the
// refusal companion to TestClientDoClassifiesConnectionFailureAsCacheBackendUnavailable,
// named above as this test's positive control: that test already proves
// helpers.ErrCacheBackendUnavailable is reachable through errors.Is out of
// Client.do's normalization when it should be, so this test's own negative
// assertion below actually discriminates rather than passing vacuously
// against a classifier that never produces the sentinel at all.
//
// It points Client.do at a handler that never writes a response - so the
// round trip is still in flight inside c.client.Do itself, not in a later
// body read - then cancels the caller's own context while that call is
// blocked, and asserts context.Canceled reaches the caller FIRST, then that
// helpers.ErrCacheBackendUnavailable does NOT.
//
// KILLING MUTATION, run and reverted: deleting the
// "if req.Context().Err() != nil { return nil, err }" branch from Client.do
// makes this test fail with:
//
//	cache_backend_classification_test.go:348: expected NOT errors.Is(err, helpers.ErrCacheBackendUnavailable),
//	got cache backend unavailable: s3 request failed: Get "http://127.0.0.1:<port>/test/some-key": context canceled
//
// while TestClientDoClassifiesConnectionFailureAsCacheBackendUnavailable
// still passes, confirming that test alone cannot catch this regression.
func TestClientDoExcludesCallerCancellationFromCacheBackendUnavailable(t *testing.T) {
	t.Parallel()

	var reachedHandler atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reachedHandler.Store(true)
		<-r.Context().Done() // never respond; the server's own context ends once the client tears the connection down.
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

	ctx, cancel := context.WithCancel(context.Background())
	// Idempotent backstop for the early-exit paths below: httptest.Server.Close
	// blocks until every outstanding request completes, and the handler only
	// returns once this context ends, so a t.Fatal between here and the explicit
	// cancel() would leave the cleanup deadlocked on a request nobody cancels.
	defer cancel()
	req, err := client.newRequest(ctx, http.MethodGet, "some-key", nil, nil, emptySHA256, nil, putCondition{})
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		resp, doErr := client.do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		errCh <- doErr
	}()

	// Wait for the handler to actually observe the request before canceling,
	// so the cancellation races an in-flight round trip rather than a request
	// that has not even been dispatched yet.
	waitForLockEvent(t, "the handler to receive the in-flight request", reachedHandler.Load)
	cancel()

	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Client.do did not return after caller cancellation")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected NOT errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
}

// TestOpenProbeFailureClassifiesAsCacheBackendUnusableNotUnavailable is the
// refusal companion to TestOpenProbeFailsOnNonConformingBackend (probe_test.go),
// naming TestClientDoClassifiesConnectionFailureAsCacheBackendUnavailable as
// its positive control for the identical reason described on
// TestClientDoExcludesCallerCancellationFromCacheBackendUnavailable above.
// Assertion order is deliberate: errS3ConditionalPutUnsupported must still
// classify as helpers.ErrCacheBackendUnusable (checked FIRST, and unaffected
// by the mutation below), which is what makes the second assertion
// pinnable rather than moot.
//
// KILLING MUTATION, run and reverted: adding helpers.ErrCacheBackendUnavailable
// to errS3ConditionalPutUnsupported's wrap (`fmt.Errorf("%w: %w: ...",
// helpers.ErrCacheBackendUnusable, helpers.ErrCacheBackendUnavailable)`)
// makes this test fail with:
//
//	cache_backend_classification_test.go:384: expected NOT errors.Is(err, helpers.ErrCacheBackendUnavailable),
//	got cache backend cannot be used as configured: cache backend unavailable: s3 backend does not enforce
//	conditional PUT (If-None-Match); distributed locking cannot guarantee mutual exclusion
//
// while its first assertion (helpers.ErrCacheBackendUnusable) still passes,
// exactly as the task instructions describe.
func TestOpenProbeFailureClassifiesAsCacheBackendUnusableNotUnavailable(t *testing.T) {
	t.Parallel()
	b := newNonConformingTestBackend(t)
	ctx := context.Background()

	err := b.Open(ctx)

	if !errors.Is(err, helpers.ErrCacheBackendUnusable) {
		t.Fatalf("expected errors.Is(err, helpers.ErrCacheBackendUnusable), got %v", err)
	}
	if errors.Is(err, helpers.ErrCacheBackendUnavailable) {
		t.Fatalf("expected NOT errors.Is(err, helpers.ErrCacheBackendUnavailable), got %v", err)
	}
}
