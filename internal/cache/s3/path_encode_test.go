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

// TestRequestURLSignedPathMatchesSentPath is the load-bearing SigV4 proof for
// this fix: for every key below, the canonical URI signed into the
// Authorization header must be byte-identical to the path the stdlib
// actually places on the wire. Before this fix, encodePath (net/url's
// PathEscape-based encoder) and the raw objectPath fed straight into
// http.NewRequestWithContext diverged for a key containing a space, a
// literal '%', or a reserved character - net/url.Parse decodes and
// re-escapes %XX sequences differently than encodePath's own escaping,
// producing a mismatched signature (SignatureDoesNotMatch, surfaced as a
// 403) even though the request was otherwise well-formed. Both PathStyle
// modes are exercised because requestURL builds the sent URL differently in
// each branch.
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

// TestAwsURIEncodeMatchesS3 proves awsURIEncode reproduces AWS SigV4's exact
// UriEncode transform byte-for-byte: only A-Za-z0-9 and -._~ are left
// literal, every other byte becomes an uppercase-hex %XX escape (including a
// literal '%' itself), and "/" is preserved or escaped depending on
// encodeSlash. This is stricter than url.PathEscape, which leaves several
// sub-delimiters (+$&,;=:@) literal - exactly the divergence that caused the
// signing bug this change fixes.
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
