package fetch_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// tinyTimeout is small enough that a hung response fails the
// ResponseHeaderTimeout test in well under a second, but large enough that a
// normal in-process fakegalaxy round trip never trips it by accident.
const tinyTimeout = 50 * time.Millisecond

// waitBound bounds how long this file's tests wait for an HTTP round trip
// to return, so a regression that reintroduces an unbounded hang fails the
// test instead of hanging the suite.
const waitBound = 5 * time.Second

// TestNew_ResponseHeaderTimeout_FiresOnHang exercises the transport-level
// half of the no-progress timeout: a server that never writes a status line
// or headers must make the request fail once ResponseHeaderTimeout - wired
// to cfg.Timeout - elapses, rather than hang indefinitely (the whole
// response Timeout that used to bound this was removed).
func TestNew_ResponseHeaderTimeout_FiresOnHang(t *testing.T) {
	t.Parallel()

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "hangs", "1.0.0", nil)
	// Count: 1 is required here, not cosmetic: a zero-value Fault.Count
	// never matches ruleMatches (its exhausted-rule check treats Count == 0
	// as "already used up"), so an unbounded Hang needs an explicit
	// positive Count the same way every other fakegalaxy fault test does.
	s.Fail(fakegalaxy.EndpointRootMetadata, "acme", "hangs", fakegalaxy.Fault{Hang: true, Count: 1})

	client := fetch.New(tinyTimeout, nil)
	url := fmt.Sprintf("%s/api/v3/collections/%s/%s/", s.URL(), "acme", "hangs")

	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}

	done := make(chan error, 1)
	go func() {
		resp, doErr := client.Do(req)
		if doErr == nil {
			// Not expected on this path, but close defensively so a
			// surprising success never leaks a connection.
			_ = resp.Body.Close()
		}
		done <- doErr
	}()

	select {
	case doErr := <-done:
		if doErr == nil {
			t.Fatal("Do error = nil, want a ResponseHeaderTimeout failure since the fake server hung before writing headers")
		}
	case <-time.After(waitBound):
		t.Fatal("Do did not return within the outer bound; ResponseHeaderTimeout failed to fire")
	}
}

// TestNew_SuccessfulRoundTrip_ReadsBodyAndCloses is a happy-path companion
// to the hang test above: it exercises the same client's watchdog-wrapped
// transport end to end against a server that answers normally, confirming
// the wrapping added around the response body does not disturb an ordinary
// request.
func TestNew_SuccessfulRoundTrip_ReadsBodyAndCloses(t *testing.T) {
	t.Parallel()

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "widgets", "1.0.0", nil)

	client := fetch.New(time.Second, nil)
	url := fmt.Sprintf("%s/api/v3/collections/%s/%s/", s.URL(), "acme", "widgets")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do error = %v, want nil", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("Body.Close error = %v, want nil", cerr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error = %v, want nil", err)
	}
	if len(body) == 0 {
		t.Fatal("ReadAll returned an empty body, want the root metadata JSON payload")
	}
	if got := s.Count(fakegalaxy.EndpointRootMetadata); got != 1 {
		t.Fatalf("EndpointRootMetadata count = %d, want 1", got)
	}
}

// mustGetRequest builds a GET *http.Request bound to t's test context,
// failing the test immediately on a parse error. It exists so this file's
// TLS-dispatch tests can drive requests through http.Client.Do (satisfying
// the noctx linter) without repeating the same three lines everywhere.
func mustGetRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext(%q) error = %v, want nil", target, err)
	}
	return req
}

// TestNewOffline_AttachesNothing_ReachesNoTransport pins the contract that
// NewOffline's client never reaches the auth/TLS-dispatch layering this
// unit adds: it rejects the request outright, before either layer could run,
// so a future refactor that accidentally routed the offline client through
// them would be caught here rather than surfacing as a token leak later.
func TestNewOffline_AttachesNothing_ReachesNoTransport(t *testing.T) {
	t.Parallel()

	client := fetch.NewOffline(time.Second)
	req := mustGetRequest(t, "https://galaxy.example.com/api/v3/")

	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("Do returned a non-nil response, want nil: an offline client must never reach a transport")
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Do error = %v, want errors.Is(err, helpers.ErrOfflineMode)", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization header = %q, want empty: nothing must be attached before the offline rejection", got)
	}
}

// TestNew_InsecureIsNeverGlobal is the most important test in this unit:
// two independent httptest.NewTLSServer instances, each with its own
// self-signed certificate, in one run. Only one of them is configured
// validate_certs=false; the other must still fail with a certificate error,
// proving the insecure policy is scoped to that one origin rather than
// disabling verification for the whole client.
func TestNew_InsecureIsNeverGlobal(t *testing.T) {
	t.Parallel()

	insecureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer insecureSrv.Close()

	secureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer secureSrv.Close()

	insecureURL, err := url.Parse(insecureSrv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", insecureSrv.URL, err)
	}

	client := fetch.New(5*time.Second, []fetch.ServerAuth{
		{Origin: helpers.Origin(insecureURL), InsecureTLS: true},
	})

	respInsecure, err := client.Do(mustGetRequest(t, insecureSrv.URL))
	if err != nil {
		t.Fatalf("Do(insecureSrv.URL) error = %v, want nil: this origin is configured validate_certs=false", err)
	}
	_ = respInsecure.Body.Close()

	respSecure, err := client.Do(mustGetRequest(t, secureSrv.URL))
	if respSecure != nil {
		_ = respSecure.Body.Close()
	}
	if err == nil {
		t.Fatal("Do(secureSrv.URL) error = nil, want a certificate error: this origin was never configured insecure")
	}
	if !strings.Contains(err.Error(), "x509") {
		t.Fatalf("Do(secureSrv.URL) error = %v, want an x509 certificate verification failure", err)
	}
}

// TestNew_RedirectFromInsecureOriginToSecureOriginUsesSecureTransport
// drives a real redirect, through http.Client, from the one configured
// insecure origin to an unconfigured one: the redirect hop must fail with a
// certificate error, proving it was routed through the fully-verifying
// secure transport rather than inheriting the first hop's insecure policy.
// The companion property - that the token is also dropped across the
// redirect - is proven directly at the auth layer by
// TestAuthTransport_RoundTrip_CrossOriginRedirectDropsToken; this test's
// job is the TLS-dispatch half specifically.
func TestNew_RedirectFromInsecureOriginToSecureOriginUsesSecureTransport(t *testing.T) {
	t.Parallel()

	secureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer secureSrv.Close()

	var insecureHitToken string
	insecureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		insecureHitToken = r.Header.Get("Authorization")
		http.Redirect(w, r, secureSrv.URL, http.StatusFound)
	}))
	defer insecureSrv.Close()

	insecureURL, err := url.Parse(insecureSrv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", insecureSrv.URL, err)
	}

	client := fetch.New(5*time.Second, []fetch.ServerAuth{
		{Origin: helpers.Origin(insecureURL), InsecureTLS: true, Token: "secret-a"},
	})

	resp, err := client.Do(mustGetRequest(t, insecureSrv.URL))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Do error = nil, want a certificate error once the redirect crosses into secureSrv's unconfigured origin")
	}
	if !strings.Contains(err.Error(), "x509") {
		t.Fatalf("Do error = %v, want an x509 certificate verification failure on the redirect hop", err)
	}
	const wantToken = "Token secret-a"
	if insecureHitToken != wantToken {
		t.Fatalf("insecureSrv received Authorization = %q, want %q (its own origin is configured with a token)", insecureHitToken, wantToken)
	}
}

// TestNew_S3ShapedOriginIsIsolatedFromGalaxyServerConfig covers the
// documented sharing of this *http.Client with the S3 cache backend
// (internal/cache/cache.go passes runtime.HTTP into s3.New): a request to
// an origin that is not any configured Galaxy server must never carry a
// Galaxy server's token, even when another configured server in the same
// client is insecure and does carry one.
func TestNew_S3ShapedOriginIsIsolatedFromGalaxyServerConfig(t *testing.T) {
	t.Parallel()

	var s3Header string
	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s3Header = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer s3Srv.Close()

	insecureSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer insecureSrv.Close()

	insecureURL, err := url.Parse(insecureSrv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", insecureSrv.URL, err)
	}

	client := fetch.New(5*time.Second, []fetch.ServerAuth{
		{Origin: helpers.Origin(insecureURL), InsecureTLS: true, Token: "galaxy-secret"},
	})

	resp, err := client.Do(mustGetRequest(t, s3Srv.URL))
	if err != nil {
		t.Fatalf("Do(s3Srv.URL) error = %v, want nil: an unconfigured plain-HTTP origin must still work through secure", err)
	}
	_ = resp.Body.Close()

	if s3Header != "" {
		t.Fatalf("s3-shaped origin received Authorization = %q, want empty (never configured with a token)", s3Header)
	}
}
