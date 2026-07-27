package fetch

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// stubTransport is a minimal http.RoundTripper double that never touches the
// network: it records every request it receives (letting a test inspect
// exactly what a wrapping transport did to it) and returns a fixed 200 OK.
type stubTransport struct {
	reqs []*http.Request
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.reqs = append(s.reqs, req)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

// TestAuthTransport_RoundTrip_OriginExactMatch drives authTransport
// directly against a bare stub, with no TLS or real network involved at
// all, proving the header is attached if and only if the request's
// normalized origin exactly matches a configured one: a different host, a
// different port, and an http-vs-https variant of the same host must all be
// treated as a different origin, since a poisoned download_url need only
// differ in one of those to be a different endpoint entirely.
func TestAuthTransport_RoundTrip_OriginExactMatch(t *testing.T) {
	t.Parallel()

	tokens := map[string]string{"https://galaxy.example.com:443": "secret-a"}

	cases := []struct {
		name    string
		url     string
		wantHdr string
	}{
		{"configured origin, implicit default port", "https://galaxy.example.com/api/v3/", "Token secret-a"},
		{"configured origin, explicit default port", "https://galaxy.example.com:443/api/v3/", "Token secret-a"},
		{"different host", "https://other.example.com/api/v3/", ""},
		{"different port", "https://galaxy.example.com:8443/api/v3/", ""},
		{"http instead of https, same host", "http://galaxy.example.com/api/v3/", ""},
		{"off-server download host", "https://cdn.example.org/artifact.tar.gz", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base := &stubTransport{}
			at := authTransport{base: base, tokens: tokens}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext error = %v, want nil", err)
			}
			resp, err := at.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip error = %v, want nil", err)
			}
			_ = resp.Body.Close()

			if len(base.reqs) != 1 {
				t.Fatalf("base received %d requests, want 1", len(base.reqs))
			}
			if got := base.reqs[0].Header.Get("Authorization"); got != tc.wantHdr {
				t.Fatalf("Authorization header = %q, want %q", got, tc.wantHdr)
			}
		})
	}
}

// TestAuthTransport_RoundTrip_DoesNotOverwriteExistingAuthorization checks
// that a caller-supplied Authorization header always wins, regardless of
// whether the request's origin also has a configured token.
func TestAuthTransport_RoundTrip_DoesNotOverwriteExistingAuthorization(t *testing.T) {
	t.Parallel()

	base := &stubTransport{}
	at := authTransport{base: base, tokens: map[string]string{"https://galaxy.example.com:443": "secret-a"}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://galaxy.example.com/api/v3/", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}
	req.Header.Set("Authorization", "Bearer caller-supplied")

	resp, err := at.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip error = %v, want nil", err)
	}
	_ = resp.Body.Close()
	if len(base.reqs) != 1 {
		t.Fatalf("base received %d requests, want 1", len(base.reqs))
	}
	if got := base.reqs[0].Header.Get("Authorization"); got != "Bearer caller-supplied" {
		t.Fatalf("Authorization header = %q, want the caller's own %q untouched", got, "Bearer caller-supplied")
	}
}

// TestAuthTransport_RoundTrip_DoesNotMutateCallerRequest asserts the
// caller's original *http.Request is left untouched, per
// http.RoundTripper's contract: a caller (in particular http.Client across
// a redirect) may still hold and reuse it after RoundTrip returns.
func TestAuthTransport_RoundTrip_DoesNotMutateCallerRequest(t *testing.T) {
	t.Parallel()

	base := &stubTransport{}
	at := authTransport{base: base, tokens: map[string]string{"https://galaxy.example.com:443": "secret-a"}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://galaxy.example.com/api/v3/", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}

	resp, err := at.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip error = %v, want nil", err)
	}
	_ = resp.Body.Close()

	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("caller's original request Authorization = %q, want empty: RoundTrip must not mutate its input", got)
	}
	if len(base.reqs) != 1 {
		t.Fatalf("base received %d requests, want 1", len(base.reqs))
	}
	if base.reqs[0] == req {
		t.Fatal("base received the exact same *http.Request pointer the caller passed in, want a clone")
	}
}

// TestAuthTransport_RoundTrip_MultipleServersDistinctTokens proves the
// origin -> token map dispatches each configured server's own token, never
// mixing them up.
func TestAuthTransport_RoundTrip_MultipleServersDistinctTokens(t *testing.T) {
	t.Parallel()

	tokens := map[string]string{
		"https://a.example.com:443": "token-a",
		"https://b.example.com:443": "token-b",
	}
	base := &stubTransport{}
	at := authTransport{base: base, tokens: tokens}

	for _, host := range []string{"a.example.com", "b.example.com"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/api/v3/", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext error = %v, want nil", err)
		}
		resp, err := at.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip error = %v, want nil", err)
		}
		_ = resp.Body.Close()
	}

	want := []string{"Token token-a", "Token token-b"}
	for i, w := range want {
		if got := base.reqs[i].Header.Get("Authorization"); got != w {
			t.Fatalf("request %d Authorization = %q, want %q", i, got, w)
		}
	}
}

// TestAuthTransport_RoundTrip_CrossOriginRedirectDropsToken drives a real
// redirect through http.Client rather than asserting the property in a
// comment: http.Client builds a fresh request per hop and calls RoundTrip
// again for it, so authTransport re-evaluates the origin match on every
// hop independently instead of inheriting whatever it decided for the
// first one.
func TestAuthTransport_RoundTrip_CrossOriginRedirectDropsToken(t *testing.T) {
	t.Parallel()

	var bHeader string
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()

	var aHeader string
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHeader = r.Header.Get("Authorization")
		http.Redirect(w, r, serverB.URL, http.StatusFound)
	}))
	defer serverA.Close()

	parsedA, err := url.Parse(serverA.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", serverA.URL, err)
	}

	tokens := map[string]string{helpers.Origin(parsedA): "secret-a"}
	client := &http.Client{Transport: authTransport{base: http.DefaultTransport, tokens: tokens}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, serverA.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do error = %v, want nil", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if aHeader != "Token secret-a" {
		t.Fatalf("server A received Authorization = %q, want %q", aHeader, "Token secret-a")
	}
	if bHeader != "" {
		t.Fatalf("server B received Authorization = %q, want empty: the redirect crossed origins and must drop the token", bHeader)
	}
}

// stubDialTransport is a real *http.Transport whose DialContext never
// actually reaches a peer: it only records that this pool was the one
// chosen for a request, then hands back one end of an already-closed
// net.Pipe so the RoundTrip that follows fails harmlessly instead of
// hanging. This is the "distinguishable stub inner transport" used to prove
// tlsDispatchTransport's routing decision without any real network or TLS
// handshake, and without needing tlsDispatchTransport's fields to be an
// interface (they are concrete *http.Transport, matching production).
type stubDialTransport struct {
	*http.Transport

	hits atomic.Int32
}

func newStubDialTransport() *stubDialTransport {
	s := &stubDialTransport{}
	s.Transport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			s.hits.Add(1)
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
	}
	return s
}

// TestTLSDispatchTransport_RoutesByExactOrigin proves dispatch selection is
// driven purely by helpers.Origin(req.URL): a configured insecure origin
// (and only that exact origin) reaches the insecure pool, while a different
// host, a different port, an http-vs-https variant of the same host, and an
// off-server (e.g. S3-shaped) download host all reach the secure pool.
func TestTLSDispatchTransport_RoutesByExactOrigin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		url       string
		wantInsec bool
	}{
		{"configured insecure origin", "http://insecure.example.com/artifact.tar.gz", true},
		{"different host", "http://other.example.com/artifact.tar.gz", false},
		{"different port", "http://insecure.example.com:8080/artifact.tar.gz", false},
		{"https variant of the same host", "https://insecure.example.com/artifact.tar.gz", false},
		{"off-server / S3-shaped download host", "http://s3.example.com/bucket/key", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			secure := newStubDialTransport()
			insecure := newStubDialTransport()
			dispatch := tlsDispatchTransport{
				secure:          secure.Transport,
				insecure:        insecure.Transport,
				insecureOrigins: map[string]struct{}{"http://insecure.example.com:80": {}},
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext error = %v, want nil", err)
			}
			// The stub dial never yields a usable connection, so RoundTrip
			// always errors and resp is always nil; only which pool it
			// dialed into matters here.
			resp, _ := dispatch.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			gotSecure, gotInsecure := secure.hits.Load(), insecure.hits.Load()
			wantSecure, wantInsecure := int32(1), int32(0)
			if tc.wantInsec {
				wantSecure, wantInsecure = 0, 1
			}
			if gotSecure != wantSecure || gotInsecure != wantInsecure {
				t.Fatalf("dial hits secure=%d insecure=%d, want secure=%d insecure=%d", gotSecure, gotInsecure, wantSecure, wantInsecure)
			}
		})
	}
}

// unwrapDispatch peels client's transport chain down to the
// tlsDispatchTransport at its core, failing the test with a precise "what I
// found instead" message if the chain does not have the expected shape.
func unwrapDispatch(t *testing.T, client *http.Client) tlsDispatchTransport {
	t.Helper()
	wd, ok := client.Transport.(watchdogTransport)
	if !ok {
		t.Fatalf("client.Transport type = %T, want watchdogTransport", client.Transport)
	}
	at, ok := wd.base.(authTransport)
	if !ok {
		t.Fatalf("watchdogTransport.base type = %T, want authTransport", wd.base)
	}
	dispatch, ok := at.base.(tlsDispatchTransport)
	if !ok {
		t.Fatalf("authTransport.base type = %T, want tlsDispatchTransport", at.base)
	}
	return dispatch
}

// TestNewClient_NoInsecureServer_InsecureTransportIsNil is the default-path
// regression this whole feature must never disturb: with every configured
// server fully verified, the insecure pool is never even constructed, not
// merely unused.
func TestNewClient_NoInsecureServer_InsecureTransportIsNil(t *testing.T) {
	t.Parallel()

	client := New(time.Second, []ServerAuth{{Origin: "https://galaxy.example.com:443", Token: "t"}})
	if dispatch := unwrapDispatch(t, client); dispatch.insecure != nil {
		t.Fatal("insecure transport is non-nil with no insecure server configured, want nil")
	}
}

// TestNewClient_InsecureServer_BuildsInsecureTransportForItsOriginOnly
// checks the companion case: one insecure server builds the insecure pool
// and scopes insecureOrigins to exactly that server's origin, not to every
// server in the list.
func TestNewClient_InsecureServer_BuildsInsecureTransportForItsOriginOnly(t *testing.T) {
	t.Parallel()

	client := New(time.Second, []ServerAuth{
		{Origin: "https://secure.example.com:443", Token: "t1"},
		{Origin: "https://insecure.example.com:443", Token: "t2", InsecureTLS: true},
	})

	dispatch := unwrapDispatch(t, client)
	if dispatch.insecure == nil {
		t.Fatal("insecure transport is nil with one insecure server configured, want non-nil")
	}
	if len(dispatch.insecureOrigins) != 1 {
		t.Fatalf("insecureOrigins = %v, want exactly one entry", dispatch.insecureOrigins)
	}
	if _, ok := dispatch.insecureOrigins["https://insecure.example.com:443"]; !ok {
		t.Fatalf("insecureOrigins = %v, want it to contain the insecure server's own origin", dispatch.insecureOrigins)
	}
}
