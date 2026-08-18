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
	"sync/atomic"
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
// to cfg.Timeout - elapses, rather than hang indefinitely; the client
// carries no whole-response Timeout to fall back on, only this
// transport-level bound and watchdogTransport's own per-read guard.
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

// TestNewUnauthenticatedAttachesNoAuthorization pins the property the
// constructor exists for: a client built with no server configuration at all
// attaches no credential, whatever origin the request happens to name.
//
// The positive control on the same fixture is what makes that mean anything.
// The second half drives fetch.New against the very same server, configured
// with that server's own origin and a token, and requires the header to arrive
// - so "no Authorization was seen" is separated from "no request was seen",
// which is otherwise the identical observation.
func TestNewUnauthenticatedAttachesNoAuthorization(t *testing.T) {
	t.Parallel()

	// Buffered, and read after each round trip rather than shared as a slice:
	// the handler runs on the server's own goroutine, so a channel handoff is
	// what makes the read ordered with respect to it under -race.
	headers := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", srv.URL, err)
	}

	resp, err := fetch.NewUnauthenticated(waitBound, false).Do(mustGetRequest(t, srv.URL))
	if err != nil {
		t.Fatalf("Do error = %v, want nil", err)
	}
	_ = resp.Body.Close()
	if got := <-headers; got != "" {
		t.Fatalf("unauthenticated client sent Authorization = %q, want empty", got)
	}

	control := fetch.New(waitBound, []fetch.ServerAuth{{Origin: helpers.Origin(parsed), Token: "t"}})
	controlResp, err := control.Do(mustGetRequest(t, srv.URL))
	if err != nil {
		t.Fatalf("positive control: Do error = %v, want nil", err)
	}
	_ = controlResp.Body.Close()
	if got, want := <-headers, "Token t"; got != want {
		t.Fatalf("positive control: Authorization = %q, want %q; the fixture cannot show a header at all", got, want)
	}
}

// TestNewUnauthenticatedOfflineRefuses covers the offline parameter this
// constructor takes and NewOffline's own does not: it must select the same
// refuse-everything transport rather than quietly building a live client.
func TestNewUnauthenticatedOfflineRefuses(t *testing.T) {
	t.Parallel()

	resp, err := fetch.NewUnauthenticated(waitBound, true).Do(mustGetRequest(t, "https://galaxy.example.com/sig.asc"))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("Do returned a non-nil response, want nil: an offline client must never reach a transport")
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("Do error = %v, want errors.Is(err, helpers.ErrOfflineMode)", err)
	}
}

// redirectQuery is the capability a presigned URL carries. It is what
// net/http's own refererForURL would hand to a redirect target: that function
// strips a URL's userinfo and suppresses the header on an https->http
// downgrade, and keeps the query string through both.
const redirectQuery = "?X-Amz-Signature=deadbeef&X-Amz-Expires=900"

// redirectBody is what the redirect target answers with, so a test can tell
// "the hop arrived carrying no Referer" from "the hop never happened".
const redirectBody = "redirect-target-body"

// TestClientsSendNoRefererAcrossARedirect pins the third property
// NewUnauthenticated's contract rests on, and pins it where it actually lives:
// on newClient, so every client this package builds has it. The two
// constructors are run against one fixture pair for exactly that reason - a
// property proven on one of them says nothing about the other.
//
// The positive control is in the same assertion block rather than a separate
// test: the redirect must be followed and the target's own body must arrive,
// since "the target saw no Referer" is otherwise indistinguishable from "the
// target was never reached at all".
func TestClientsSendNoRefererAcrossARedirect(t *testing.T) {
	t.Parallel()

	// Buffered and read after each round trip: the handler runs on the
	// server's own goroutine, so the channel handoff is what orders the read
	// with respect to it under -race.
	referers := make(chan string, 2)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referers <- r.Header.Get("Referer")
		_, _ = w.Write([]byte(redirectBody))
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/dest", http.StatusFound)
	}))
	defer source.Close()

	sourceURL, err := url.Parse(source.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v, want nil", source.URL, err)
	}

	clients := []struct {
		client *http.Client
		name   string
	}{
		// The shared client is built with the redirecting origin configured as
		// a Galaxy server, which is the shape a presigned artifact download
		// actually takes: a configured host answering with a hop elsewhere.
		{name: "New", client: fetch.New(waitBound, []fetch.ServerAuth{{Origin: helpers.Origin(sourceURL), Token: "t"}})},
		{name: "NewUnauthenticated", client: fetch.NewUnauthenticated(waitBound, false)},
	}

	for _, tc := range clients {
		// Deleting req.Header.Del("Referer") from checkRedirect, applied
		// through go test -overlay so no production file is edited, fails on
		// the last assertion - at the New row, since t.Fatalf stops the test
		// before the second client runs. The header goes to the log, since it
		// carries the fixture's own ephemeral port and no two runs would agree:
		//
		//	client_test.go:299: New: the redirect target received a Referer
		resp, err := tc.client.Do(mustGetRequest(t, source.URL+"/sig.asc"+redirectQuery))
		if err != nil {
			t.Fatalf("%s: Do error = %v, want nil", tc.name, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: ReadAll error = %v, want nil", tc.name, err)
		}
		if string(body) != redirectBody {
			t.Fatalf("%s: body = %q, want the redirect target's own body; the hop did not arrive", tc.name, body)
		}
		if got := <-referers; got != "" {
			t.Logf("Referer = %q", got)
			t.Fatalf("%s: the redirect target received a Referer", tc.name)
		}
	}
}

// TestClientsStillRefuseAnEndlessRedirectChain is the other half of assigning
// CheckRedirect at all: the field REPLACES http.Client's own default check, so
// a hook that only stripped a header would turn this fixture from a bounded
// failure into an unbounded loop.
//
// The hop count is asserted as well as the error, because the error alone would
// also be produced by a hook that refused the first redirect outright - which
// would break every legitimate presigned download in the process.
func TestClientsStillRefuseAnEndlessRedirectChain(t *testing.T) {
	t.Parallel()

	var hops atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer srv.Close()

	// Two mutations, applied through go test -overlay so no production file is
	// edited, one per half of what this hook owes the fixture.
	//
	// Returning nil unconditionally - the shape a hook that only stripped the
	// Referer would have, and exactly what assigning CheckRedirect costs, since
	// the field REPLACES http.Client's own default check - does not fail this
	// test, it hangs it, following the fixture's redirects forever. The
	// artifact is a timeout under an explicit -timeout rather than an
	// assertion, recorded as it came:
	//
	//	panic: test timed out after 20s
	//		running tests:
	//			TestClientsStillRefuseAnEndlessRedirectChain (20s)
	//
	// Refusing at the first hop instead - len(via) >= 1 - passes both
	// assertions above it and fails the hop count, which is what makes that
	// one pinned rather than documentary:
	//
	//	client_test.go:352: server served 1 requests, want 10: the ceiling must bite where net/http's own default did
	resp, err := fetch.New(waitBound, nil).Do(mustGetRequest(t, srv.URL+"/sig.asc"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Do error = nil, want a refusal: a self-redirecting server must not be followed forever")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("Do error = %v, want net/http's own redirect-ceiling wording", err)
	}
	if got := hops.Load(); got != 10 {
		t.Fatalf("server served %d requests, want 10: the ceiling must bite where net/http's own default did", got)
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

// offlineFixturePassword is the credential this file's offline fixture
// smuggles into the URL it refuses. Distinctive on purpose: a substring search
// for a value that can collide with nothing else in a rendered message is an
// answer rather than a coincidence.
const offlineFixturePassword = "pa55w0rd-must-not-be-rendered"

// offlineFixtureHostPath is the part of that fixture an operator reading the
// refusal actually needs, and the part no cut here removes.
const offlineFixtureHostPath = "hub.example/api/v3/collections/acme/widgets/"

// offlineFixtureQuery is the capability half of the same fixture: a presigned
// query is what a message naming a URL must drop even where the URL itself is
// worth naming.
const offlineFixtureQuery = "?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900"

// offlineFixtureURL carries both credential-bearing parts of a URL at once, so
// one fixture covers both halves of the cut under test.
const offlineFixtureURL = "https://u:" + offlineFixturePassword + "@" + offlineFixtureHostPath + offlineFixtureQuery

// TestNewOffline_RefusalNamesTheURLWithItsCredentialsCut pins the cut
// offlineTransport.RoundTrip applies to the URL it names.
//
// The assertion is on the transport's own error rather than on the *url.Error
// http.Client wraps it in, because that inner value is what an operator
// actually reads. Every re-render on the paths reaching this transport drops
// net/http's outer message and prints the cause beside a display of its own -
// helpers.CutTransportURL does that for a metadata or artifact request,
// signature.transportCause for a signature source - so whatever this transport
// renders survives every cut applied above it. Asserting on the outer render
// instead would assert nothing about this code: net/http's own masking
// rewrites that URL to "u:***@..." with the query left intact, which carries
// both the userinfo prefix and the presigned query no matter what happens
// here.
//
// The five checks are independent t.Errorf calls rather than a t.Fatalf chain:
// a message that carries a capability, one that names nothing at all, and one
// that stopped classifying are three different defects with three different
// remedies, and a chain would only ever report the first.
//
// assertOfflineRefusalNamesACleanURL is the positive control, on this same
// transport with a URL that has nothing to cut: it proves a refusal does name
// the URL it refused, so "carries no query" here cannot be satisfied by a
// message that dropped the URL altogether.
//
// KILLING MUTATION, run: rendering req.URL in place of the cut form in
// offlineTransport.RoundTrip fails three of the five checks - the password,
// the userinfo prefix and the presigned query. The host-and-path check stays
// green under it, since the uncut value contains that substring too, which is
// what makes it a control on the cut rather than a second pin of it, and so
// does the classification check, since the mutation moves no sentinel. The
// first failure reads:
//
//	client_test.go:565: offline refusal carries the password: offline mode is
//	enabled, network access is forbidden: GET https://u:pa55w0rd-must-not-be-rendered@hub.example/api/v3/collections/acme/widgets/
//	?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900
//
// assertOfflineRefusalNamesACleanURL stays green through it as well: a URL
// with nothing to cut renders identically either way.
func TestNewOffline_RefusalNamesTheURLWithItsCredentialsCut(t *testing.T) {
	t.Parallel()

	resp, err := fetch.NewOffline(0).Do(mustGetRequest(t, offlineFixtureURL))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("Do returned a non-nil response, want nil: an offline client must never reach a transport")
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("errors.As(err, &*url.Error) failed on %v, so this fixture never reached the transport", err)
	}
	msg := urlErr.Err.Error()

	if strings.Contains(msg, offlineFixturePassword) {
		t.Errorf("offline refusal carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("offline refusal carries the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("offline refusal carries the presigned query: %s", msg)
	}
	if !strings.Contains(msg, offlineFixtureHostPath) {
		t.Errorf("offline refusal does not name the host and path it refused: %s", msg)
	}
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Errorf("offline refusal does not classify as helpers.ErrOfflineMode: %v", err)
	}

	assertOfflineRefusalNamesACleanURL(t)
}

// assertOfflineRefusalNamesACleanURL is the control described on
// TestNewOffline_RefusalNamesTheURLWithItsCredentialsCut: the same transport,
// handed a URL with nothing to cut, must still name that URL whole.
func assertOfflineRefusalNamesACleanURL(t *testing.T) {
	t.Helper()

	clean := "https://" + offlineFixtureHostPath
	resp, err := fetch.NewOffline(0).Do(mustGetRequest(t, clean))
	if resp != nil {
		_ = resp.Body.Close()
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("control: errors.As(err, &*url.Error) failed on %v", err)
	}
	if got := urlErr.Err.Error(); !strings.Contains(got, clean) {
		t.Errorf("control: a refusal over a credential-free URL does not name it: %s", got)
	}
}
