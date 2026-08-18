package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// metadataFixturePassword is the credential every fixture in this file
// smuggles into a server-supplied metadata URL. It is deliberately
// distinctive, for the same reason collections/galaxy_info_test.go's
// urlPassword is: a value that cannot collide with any other byte sequence in
// a rendered message makes a substring search for it an answer rather than a
// coincidence.
const metadataFixturePassword = "pa55w0rd-must-not-be-rendered"

// metadataFixtureHostPath is the part of every fixture URL an operator reading
// a failure actually needs, and the part no rule here cuts.
const metadataFixtureHostPath = "hub.example/api/v3/collections/acme/widgets/versions/"

// metadataFixtureURL is that host and path with a scheme and nothing else: the
// form a cut message must still name, and the control fixture's whole URL.
const metadataFixtureURL = "https://" + metadataFixtureHostPath

// metadataFixtureUserinfoURL is the same value carrying a credential in its
// authority, which is what a message must not name.
const metadataFixtureUserinfoURL = "https://u:" + metadataFixturePassword + "@" + metadataFixtureHostPath

// metadataFixtureQuery is the capability half of the same fixture: a presigned
// query is what a message naming a URL must drop even where the URL itself is
// worth naming.
const metadataFixtureQuery = "?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900"

// metadataUnbuildableURL carries the same credential in its authority as
// metadataFixtureUserinfoURL does, alongside a port url.Parse refuses, so it
// is exactly the population helpers.ErrMetadataRequestBuildFailed exists for:
// a value carrying a credential that no request can be built from, which is
// also the one shape collections.checkMetadataURLUserinfo deliberately passes
// through unjudged.
const metadataUnbuildableURL = "https://u:" + metadataFixturePassword + "@hub.example:notaport/api/v3/collections/"

// notFoundClient returns a client answering every request with a bare 404, so
// a fixture reaches fetchJSONBodyOnce's status arm without a server: these
// fixtures name a host that resolves nowhere, on purpose.
func notFoundClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	})}
}

// okJSONClient returns a client answering every request with 200 and a minimal
// JSON document, the shape a request that actually reaches an endpoint gets
// back.
func okJSONClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
		}, nil
	})}
}

// TestHTTPStatusErrorNamesTheURLWithItsCredentialsCut pins the cut applied
// where fetchJSONBodyOnce builds an *HTTPStatusError, and pins it through the
// rendered message rather than through the URL field: the field is where the
// cut happens today, and asserting the text keeps this pin valid if the cut
// ever moves to Error() instead. It drives that arm through
// FetchJSONWithCachePolicy with no store, the shortest path a caller has to
// it.
//
// The three negative checks and the one positive check are independent
// t.Errorf calls rather than a t.Fatalf chain, because a message that carries
// a capability and a message that names nothing at all are different defects
// and a chain would only ever report the first.
//
// assertStatusErrorNamesTheCleanURL is the positive control, run on this same
// fixture with its query deleted: it proves the message does name the URL when
// there is nothing to cut, so "carries no query" here cannot be satisfied by a
// message that dropped the URL altogether.
//
// Killing mutation, run: deleting the cut at that construction site, so the
// field is assigned the raw url, fails all four checks - the three negative
// ones on the value it now renders, and the positive one because a URL
// carrying userinfo no longer contains the clean prefix that check looks for.
// The first of the four reads:
//
//	metadata_url_test.go:121: status error carries the presigned query: failed to fetch metadata:
//	404 Not Found (https://u:pa55w0rd-must-not-be-rendered@hub.example/api/v3/collections/acme/widgets/versions/
//	?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900)
//
// The other three label that same rendered value differently.
// assertStatusErrorNamesTheCleanURL stays green through the mutation, since a
// URL with nothing to cut renders identically either way - which is what makes
// it a control rather than a second pin of the same behavior.
func TestHTTPStatusErrorNamesTheURLWithItsCredentialsCut(t *testing.T) {
	t.Parallel()

	raw := metadataFixtureUserinfoURL + metadataFixtureQuery
	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), notFoundClient(), raw, nil, &out, Policy{}, 0)
	if err == nil {
		t.Fatal("a 404 returned no error, so this fixture never reached the status arm")
	}
	msg := err.Error()

	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("status error carries the presigned query: %s", msg)
	}
	if strings.Contains(msg, metadataFixturePassword) {
		t.Errorf("status error carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("status error carries the userinfo prefix %q: %s", "u:", msg)
	}
	if !strings.Contains(msg, metadataFixtureURL) || !strings.Contains(msg, "404") {
		t.Errorf("status error does not name the URL's scheme, host and path alongside its status: %s", msg)
	}

	assertStatusErrorNamesTheCleanURL(t)
}

// assertStatusErrorNamesTheCleanURL is the control described on
// TestHTTPStatusErrorNamesTheURLWithItsCredentialsCut: the same fixture with
// nothing to cut must render its URL intact.
func assertStatusErrorNamesTheCleanURL(t *testing.T) {
	t.Helper()

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), notFoundClient(), metadataFixtureURL, nil, &out, Policy{}, 0)
	if err == nil {
		t.Fatal("control: a 404 returned no error")
	}
	if !strings.Contains(err.Error(), metadataFixtureURL) {
		t.Errorf("control: a status error over a credential-free URL does not name it: %s", err.Error())
	}
}

// TestFetchJSONBodyRefusesAURLNoRequestCanBeBuiltFrom pins the other half of
// this file's rule, on the one arm where naming the value at all is the
// defect: net/http refuses to build a request from a URL url.Parse rejects,
// and the *url.Error it returns names the whole raw string with the password
// in cleartext. fetchJSONBodyOnce drops that error for
// helpers.ErrMetadataRequestBuildFailed, whose own doc comment holds why this
// is a replacement rather than a cut.
//
// The four negative checks, the classification check and the actionability
// check are independent t.Errorf calls: a message that leaks and a message
// that classifies wrong are different defects with different remedies, and a
// t.Fatalf chain would report only whichever came first.
//
// assertBuildableURLReachesTheClient is the positive control, on this same
// fixture with its port made real: it proves the refusal is this code's doing
// rather than a fixture that could never have been fetched anyway.
//
// Killing mutation, run: restoring the dropped error, so the arm returns what
// http.NewRequestWithContext handed it, fails all six checks. The first reads:
//
//	metadata_url_test.go:191: refusal message carries the password: parse
//	"https://u:pa55w0rd-must-not-be-rendered@hub.example:notaport/api/v3/collections/":
//	invalid port ":notaport" after host
//
// The other three negative checks label that same rendered value differently,
// while the classification and actionability checks fail on what the message
// no longer is. assertBuildableURLReachesTheClient stays green through it: the
// mutation changes only what a refused build returns.
func TestFetchJSONBodyRefusesAURLNoRequestCanBeBuiltFrom(t *testing.T) {
	t.Parallel()

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), okJSONClient(), metadataUnbuildableURL, nil, &out, Policy{}, 0)
	if err == nil {
		t.Fatal("a URL url.Parse refuses was accepted, want a refusal")
	}
	msg := err.Error()

	if strings.Contains(msg, metadataFixturePassword) {
		t.Errorf("refusal message carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("refusal message carries the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, metadataUnbuildableURL) {
		t.Errorf("refusal message carries the raw value: %s", msg)
	}
	if strings.Contains(msg, "hub.example") {
		t.Errorf("refusal message names part of the raw value: %s", msg)
	}
	if !errors.Is(err, helpers.ErrMetadataRequestBuildFailed) {
		t.Errorf("refusal does not classify as helpers.ErrMetadataRequestBuildFailed: %v", err)
	}
	if !strings.Contains(msg, "metadata url") {
		t.Errorf("refusal message names nothing actionable: %s", msg)
	}

	assertBuildableURLReachesTheClient(t)
}

// assertBuildableURLReachesTheClient is the control described on
// TestFetchJSONBodyRefusesAURLNoRequestCanBeBuiltFrom. It differs from that
// fixture in exactly one respect - a port url.Parse accepts - so a run
// reaching the client, credential and all, is what makes the refusal above the
// guard's doing rather than the fixture's.
func assertBuildableURLReachesTheClient(t *testing.T) {
	t.Helper()

	buildable := strings.Replace(metadataUnbuildableURL, ":notaport", ":8443", 1)
	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), okJSONClient(), buildable, nil, &out, Policy{}, 0)
	if err != nil {
		t.Fatalf("control: the same fixture with a real port err = %v, want nil", err)
	}
	if out["ok"] != true {
		t.Fatal("control: the response body did not decode, so the request never reached the client")
	}
}

// dialFailureClient returns a client whose transport always fails before any
// response, the shape a refused dial, a DNS failure and a TLS handshake error
// all take. net/http wraps whatever the transport returns in a *url.Error
// carrying the request URL, which is the value under test here.
func dialFailureClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errDialRefusedFixture
	})}
}

// TestMetadataTransportErrorNamesTheURLWithItsCredentialsCut pins
// helpers.CutTransportURL as this path reaches it - the one wrapper the
// artifact path shares. It is the arm reached without any server having
// answered, so it is the shape a CI meets first when an endpoint is wrong or
// unreachable - and net/http's own redaction does not cover it: that redaction
// masks a password and leaves the query, which on a server-chosen value is the
// capability rather than a detail of one.
//
// The four checks are independent t.Errorf calls rather than a t.Fatalf chain,
// for the reason the status-arm test above states: a message that carries a
// capability and a message that names nothing are different defects.
//
// assertTransportErrorStaysClassifiable is the second half, and it is what
// makes the wrapper safe rather than merely quiet: every classifier on this
// path reads through errors.Is and errors.As, so the pin is that the original
// *url.Error is still reachable underneath.
//
// Killing mutation, run: returning client.Do's error unchanged fails two of
// the four checks, the query one and the userinfo one. The password check
// stays green under it, and that is the measurement this whole arm rests on
// rather than an oversight: net/http masks a password to "***" while
// composing this error and leaves everything else, so the credential half is
// already covered and the query half is not. The positive check stays green
// too, since the raw value contains the clean prefix as well - which is what
// makes it a control on the cut rather than a second pin of it. The first
// failure reads:
//
//	metadata_url_test.go:283: transport error carries the presigned query:
//	Get "https://u:***@hub.example/api/v3/collections/acme/widgets/versions/
//	?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900": connect: connection refused
func TestMetadataTransportErrorNamesTheURLWithItsCredentialsCut(t *testing.T) {
	t.Parallel()

	raw := metadataFixtureUserinfoURL + metadataFixtureQuery
	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), dialFailureClient(), raw, nil, &out, Policy{}, 0)
	if err == nil {
		t.Fatal("a failing transport returned no error, so this fixture never reached the transport arm")
	}
	msg := err.Error()

	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("transport error carries the presigned query: %s", msg)
	}
	if strings.Contains(msg, metadataFixturePassword) {
		t.Errorf("transport error carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("transport error carries the userinfo prefix %q: %s", "u:", msg)
	}
	if !strings.Contains(msg, metadataFixtureHostPath) {
		t.Errorf("transport error does not name the host and path it failed to reach: %s", msg)
	}

	assertTransportErrorStaysClassifiable(t, err)
}

// assertTransportErrorStaysClassifiable is the half of
// TestMetadataTransportErrorNamesTheURLWithItsCredentialsCut that holds the
// rendering change to being a rendering change: fetchRetryable and
// deadlineError both read this error's shape, and both do it through errors.As
// and errors.Is, which traverse Unwrap. A wrapper that dropped the original
// would silently move every one of those verdicts.
//
// The errors.As call is what actually pins that. KILLING MUTATION, run:
// deleting helpers.TransportURLError's Unwrap makes this t.Fatalf fire, with
// its rendering left correct - reported against the call site above rather
// than against the t.Fatalf itself, since this helper calls t.Helper():
//
//	metadata_url_test.go:295: errors.As(err, &*url.Error) failed on Get
//	"https://hub.example/api/v3/collections/acme/widgets/versions/": connect: connection refused, want success
//
// The equality below it is a regression guard rather than a second pin, and
// cannot be one - it is only ever evaluated on a tree that errors.As already
// walked, and on such a tree both sides are false for any realistic mutation
// (measured: a transport failure with no status carries none of the sentinels
// fetchRetryable reads). It is kept for the one shape it would still catch: a
// future wrapper that unwrapped to something other than the original.
func assertTransportErrorStaysClassifiable(t *testing.T, err error) {
	t.Helper()

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("errors.As(err, &*url.Error) failed on %v, want success", err)
	}
	// Equality with the unwrapped verdict, not a fixed verdict: what this
	// wrapper must not do is CHANGE a classification, and asserting a
	// particular answer would pin fetchRetryable's own policy here instead.
	if got, want := fetchRetryable(err), fetchRetryable(urlErr); got != want {
		t.Errorf("fetchRetryable(wrapped) = %v, fetchRetryable(unwrapped) = %v, want them equal", got, want)
	}
}

// errDialRefusedFixture is the transport failure dialFailureClient returns. It
// is declared here, at the end of the file, rather than beside that helper:
// this file carries mutation citations by line number, and a declaration
// placed above them would move every one of them for no reason a reader
// benefits from.
var errDialRefusedFixture = errors.New("connect: connection refused")
