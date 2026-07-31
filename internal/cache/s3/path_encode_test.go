package s3

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// TestRequestURLSignedPathMatchesSentPath is the load-bearing SigV4 proof
// that, for every key below, the canonical URI signed into the Authorization
// header is byte-identical to the path the stdlib actually places on the
// wire. encodePath (net/url's PathEscape-based encoder) and the raw
// objectPath fed straight into http.NewRequestWithContext must agree for a
// key containing a space, a literal '%', or a reserved character: routing
// objectPath through net/url.Parse instead would decode and re-escape %XX
// sequences differently than encodePath's own escaping, producing a
// mismatched signature (SignatureDoesNotMatch, surfaced as a 403) even
// though the request was otherwise well-formed. Both PathStyle modes are
// exercised because requestURL builds the sent URL differently in each
// branch.
func TestRequestURLSignedPathMatchesSentPath(t *testing.T) {
	t.Parallel()

	keys := []string{
		"space key",
		"café",
		":?#[]@!$&'()*+,;=",
		"a+b",
		"100%done",
		"acme-app-1.0.0.tar.gz",
	}

	for _, pathStyle := range []bool{true, false} {
		t.Run(fmt.Sprintf("pathStyle=%v", pathStyle), func(t *testing.T) {
			t.Parallel()
			c := newRequestURLTestClient(t, pathStyle)

			for _, key := range keys {
				t.Run(key, func(t *testing.T) {
					t.Parallel()

					reqURL, _, canonicalURI, _ := c.requestURL(key, nil)

					u, err := url.Parse(reqURL)
					if err != nil {
						t.Fatalf("url.Parse(%q): %v", reqURL, err)
					}
					if got := u.EscapedPath(); got != canonicalURI {
						t.Fatalf("sent path %q does not match signed canonical URI %q (reqURL=%q)",
							got, canonicalURI, reqURL)
					}
				})
			}
		})
	}
}

// newRequestURLTestClient builds a Client wired only for exercising
// requestURL - it never performs a real HTTP round trip, so a bare
// *http.Client and placeholder credentials are sufficient.
func newRequestURLTestClient(t *testing.T, pathStyle bool) *Client {
	t.Helper()
	cfg := config.S3CacheConfig{
		Endpoint:  "https://s3.example.com",
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: "secret",
		PathStyle: pathStyle,
	}
	c, err := newClient(cfg, &http.Client{})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

// cachedEndpointCase is one case for TestRequestURLUsesCachedEndpoint: an
// input endpoint/PathStyle pair plus the host, scheme, and requestURL output
// it must produce.
type cachedEndpointCase struct {
	name        string
	endpoint    string
	wantHost    string
	wantScheme  string
	wantReqURL  string
	wantURLHost string
	pathStyle   bool
}

// cachedEndpointTestCases returns cases covering both PathStyle modes, a
// custom non-TLS endpoint, and an endpoint given with and without a trailing
// "/". A function (rather than a package-level var) keeps the table a local,
// per-test value instead of shared global state.
func cachedEndpointTestCases() []cachedEndpointCase {
	return []cachedEndpointCase{
		{
			name:        "path-style https endpoint",
			endpoint:    "https://s3.example.com",
			pathStyle:   true,
			wantHost:    "s3.example.com",
			wantScheme:  "https",
			wantReqURL:  "https://s3.example.com/test-bucket/obj.txt",
			wantURLHost: "s3.example.com",
		},
		{
			name:        "virtual-host endpoint with trailing slash",
			endpoint:    "https://s3.example.com/",
			pathStyle:   false,
			wantHost:    "s3.example.com",
			wantScheme:  "https",
			wantReqURL:  "https://test-bucket.s3.example.com/obj.txt",
			wantURLHost: "test-bucket.s3.example.com",
		},
		{
			name:        "path-style custom non-TLS endpoint",
			endpoint:    "http://localhost:9000",
			pathStyle:   true,
			wantHost:    "localhost:9000",
			wantScheme:  "http",
			wantReqURL:  "http://localhost:9000/test-bucket/obj.txt",
			wantURLHost: "localhost:9000",
		},
		{
			name:        "virtual-host bare endpoint without scheme",
			endpoint:    "localhost:9000",
			pathStyle:   false,
			wantHost:    "localhost:9000",
			wantScheme:  "https",
			wantReqURL:  "https://test-bucket.localhost:9000/obj.txt",
			wantURLHost: "test-bucket.localhost:9000",
		},
	}
}

// assertCachedEndpointCase builds a client for tt.endpoint, checks the
// endpointHost/endpointScheme fields newClient cached, then calls
// requestURL and checks the reqURL/host it produces from those fields.
func assertCachedEndpointCase(t *testing.T, tt cachedEndpointCase) {
	t.Helper()
	cfg := config.S3CacheConfig{
		Endpoint:  tt.endpoint,
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: "secret",
		PathStyle: tt.pathStyle,
	}
	c, err := newClient(cfg, &http.Client{})
	if err != nil {
		t.Fatalf("newClient(%q): %v", tt.endpoint, err)
	}
	if c.endpointHost != tt.wantHost {
		t.Fatalf("endpointHost = %q, want %q", c.endpointHost, tt.wantHost)
	}
	if c.endpointScheme != tt.wantScheme {
		t.Fatalf("endpointScheme = %q, want %q", c.endpointScheme, tt.wantScheme)
	}

	reqURL, host, _, _ := c.requestURL("obj.txt", nil)
	if reqURL != tt.wantReqURL {
		t.Fatalf("requestURL reqURL = %q, want %q", reqURL, tt.wantReqURL)
	}
	if host != tt.wantURLHost {
		t.Fatalf("requestURL host = %q, want %q", host, tt.wantURLHost)
	}
}

// TestRequestURLUsesCachedEndpoint proves requestURL derives its host and
// scheme from the fields newClient parsed once at construction, rather than
// re-parsing cfg.Endpoint on every call. requestURL no longer calls
// url.Parse at all (a structural change verifiable by inspection), so
// identical output here for every case is what proves the removal did not
// shift any byte of the signed-and-sent URL.
func TestRequestURLUsesCachedEndpoint(t *testing.T) {
	t.Parallel()
	for _, tt := range cachedEndpointTestCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertCachedEndpointCase(t, tt)
		})
	}
}

// TestAwsURIEncodeMatchesS3 proves awsURIEncode reproduces AWS SigV4's exact
// UriEncode transform byte-for-byte: only A-Za-z0-9 and -._~ are left
// literal, every other byte becomes an uppercase-hex %XX escape (including a
// literal '%' itself), and "/" is preserved or escaped depending on
// encodeSlash. This is stricter than url.PathEscape, which leaves several
// sub-delimiters (+$&,;=:@) literal - exactly the divergence that would break
// SigV4 signing if awsURIEncode's escaping matched url.PathEscape instead.
func TestAwsURIEncodeMatchesS3(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		want        string
		encodeSlash bool
	}{
		{name: "space becomes percent-20", input: "my key", want: "my%20key"},
		{name: "plus is not unreserved and gets escaped", input: "a+b", want: "a%2Bb"},
		{name: "dollar is not unreserved and gets escaped", input: "a$b", want: "a%24b"},
		{name: "slash stays literal when encodeSlash is false", input: "a/b", want: "a/b"},
		{name: "unreserved tilde stays literal", input: "a~b", want: "a~b"},
		{name: "literal percent gets escaped to percent-25", input: "100%done", want: "100%25done"},
		{name: "multibyte utf-8 rune becomes its raw bytes in uppercase hex", input: "café", want: "caf%C3%A9"},
		{name: "slash gets escaped to percent-2F when encodeSlash is true", input: "a/b", want: "a%2Fb", encodeSlash: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := awsURIEncode(tt.input, tt.encodeSlash); got != tt.want {
				t.Fatalf("awsURIEncode(%q, %v) = %q, want %q", tt.input, tt.encodeSlash, got, tt.want)
			}
		})
	}
}

// TestS3RoundTripReservedCharKey proves that an object key containing
// reserved characters (a space, a '+', and a '!') PUTs and GETs/HEADs
// successfully through the fake S3 server and comes back byte-identical.
// fakeS3 never verifies the Authorization header it receives (see its own
// doc comment), so this test validates that PUT and GET/HEAD agree on how
// the reserved-character key is escaped on the wire; it does not itself
// validate the signature against a conforming S3 endpoint - that proof is
// TestRequestURLSignedPathMatchesSentPath above.
func TestS3RoundTripReservedCharKey(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const key = "dir/weird +key!.dat"
	payload := []byte("small body")

	if err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)),
		"application/octet-stream", "", nil, false, ""); err != nil {
		t.Fatalf("putObject(%q): %v", key, err)
	}

	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		t.Fatalf("getObject(%q): %v", key, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read getObject(%q) body: %v", key, err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("getObject(%q) body = %q, want %q", key, got, payload)
	}

	if _, err := b.client.headObject(ctx, key); err != nil {
		t.Fatalf("headObject(%q): %v", key, err)
	}
}
