// Package fetch builds the *http.Client every outbound request in this program
// shares, the S3 cache backend's included. New stacks three transports over
// the pooled ones: a watchdog that fails a body read which has stopped making
// progress, token attachment, and TLS-policy dispatch. The client itself
// carries no overall Timeout, deliberately - the watchdog bounds inactivity
// instead, so a slow but steadily streaming artifact download is not truncated
// for being large.
//
// Both credential-bearing layers key on the request URL's normalized origin
// (helpers.Origin) and never on a server id, a configured URL, or a prefix
// match, so a URL that merely resembles a configured server receives neither
// its token nor its relaxed certificate checking. NewOffline builds the same
// client shape around a transport that refuses every request instead.
package fetch

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ServerAuth is fetch's own view of one configured Galaxy server's
// transport-relevant fields: exactly enough for the auth and TLS-dispatch
// layers below to build their origin-keyed maps, without fetch importing
// internal/galaxy/config - config is a layer above fetch, and depending on
// it here would invert that boundary. The command layer builds this slice
// from config.Config.Servers immediately before calling New; that
// construction site is the only place config.Secret.Reveal() is called for
// a Galaxy token, since only the command layer can see the Secret-typed
// field this is derived from. The rule Reveal is governed by is a predicate
// rather than a count - the plaintext may be taken only where the value goes
// onto the wire in that same statement - and the S3 cache client satisfies it
// at two sites of its own for its own credentials.
type ServerAuth struct {
	// Origin is helpers.Origin(parsed server URL), already normalized, so
	// New never has to reparse or renormalize a server's configured URL.
	Origin string
	// Token is the server's revealed API token, or "" if it carries none.
	Token string
	// InsecureTLS mirrors the server's validate_certs=false setting.
	InsecureTLS bool
}

// New creates a configured HTTP client with reasonable defaults. servers
// describes every configured Galaxy server's origin, token, and TLS policy;
// a request whose URL origin matches one of them is routed to that server's
// transport and, if the server carries a token, has it attached. Any other
// request - an off-server download host, or a wholly unrelated origin such
// as the S3 cache backend's own requests, since this same *http.Client is
// shared with it - gets the plain, fully-verified secure transport with no
// credential attached.
func New(timeout time.Duration, servers []ServerAuth) *http.Client {
	return newClient(timeout, false, servers)
}

// NewOffline creates an HTTP client whose transport rejects every request
// with helpers.ErrOfflineMode. Cache reads that don't go through the client
// continue to work; any code path that actually needs the network fails fast.
// It takes no server configuration: offlineTransport rejects every request
// before auth attachment or TLS dispatch would ever run.
func NewOffline(timeout time.Duration) *http.Client {
	return newClient(timeout, true, nil)
}

func newClient(timeout time.Duration, offline bool, servers []ServerAuth) *http.Client {
	if offline {
		// The offline transport rejects every request before it ever reaches
		// the network, so it never returns a body for the watchdog to guard;
		// it is deliberately left unwrapped.
		return &http.Client{Timeout: timeout, Transport: offlineTransport{}}
	}

	dispatch := tlsDispatchTransport{secure: newTransport(timeout, nil), insecureOrigins: insecureOriginSet(servers)}
	if len(dispatch.insecureOrigins) > 0 {
		dispatch.insecure = newTransport(timeout, &tls.Config{
			// InsecureSkipVerify is set here only because at least one
			// configured server's validate_certs=false selected it, and
			// tlsDispatchTransport (see below) routes only that server's own
			// origin to this transport - every other origin, including this
			// same run's other, fully-verified servers, never reaches it.
			InsecureSkipVerify: true, // #nosec G402 -- opt-in per-origin via validate_certs=false, never a blanket default
		})
	}

	auth := authTransport{base: dispatch, tokens: tokensByOrigin(servers)}
	// The client itself carries no Timeout, which would bound the entire
	// request (headers plus however large the artifact body is) as a single
	// cap. watchdogTransport instead guards every body read individually, so
	// a stalled connection is still caught without capping total transfer
	// time.
	return &http.Client{Transport: watchdogTransport{base: auth, idle: timeout}}
}

// newTransport builds one *http.Transport with the dial, pooling, and HTTP/2
// settings every fetch transport shares, differing only in tlsConfig (nil
// selects Go's default: full certificate verification). Routing both the
// secure and insecure transports through this single builder is what keeps
// their dialer, ResponseHeaderTimeout, idle settings, and
// ForceAttemptHTTP2 from drifting apart if one of the two is ever tuned
// without the other.
func newTransport(timeout time.Duration, tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   helpers.FetchDialContextTimeout,
			KeepAlive: helpers.FetchDialContextKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     helpers.FetchForceAttemptHTTP2,
		MaxIdleConns:          helpers.FetchMaxIdleConns,
		MaxIdleConnsPerHost:   helpers.FetchMaxIdleConnsPerHost,
		IdleConnTimeout:       helpers.FetchIdleConnTimeout,
		TLSHandshakeTimeout:   helpers.FetchTLSHandshakeTimeout,
		ExpectContinueTimeout: helpers.FetchExpectContinueTimeout,
		// ResponseHeaderTimeout bounds time-to-first-byte: timeout is now a
		// no-progress budget rather than a whole-response cap, so a slow but
		// steadily streaming artifact download is not truncated by it.
		ResponseHeaderTimeout: timeout,
		TLSClientConfig:       tlsConfig,
	}
}

// insecureOriginSet collects the normalized origin of every server in
// servers whose InsecureTLS is set, pre-sized to len(servers) (a tight
// upper bound). An empty result is what tells newClient to leave the
// insecure transport nil rather than build a connection pool nothing will
// ever be dispatched to.
func insecureOriginSet(servers []ServerAuth) map[string]struct{} {
	origins := make(map[string]struct{}, len(servers))
	for _, s := range servers {
		if s.InsecureTLS {
			origins[s.Origin] = struct{}{}
		}
	}
	return origins
}

// tokensByOrigin collects the normalized origin -> token map for every
// server in servers that carries a token, pre-sized to len(servers).
func tokensByOrigin(servers []ServerAuth) map[string]string {
	tokens := make(map[string]string, len(servers))
	for _, s := range servers {
		if s.Token != "" {
			tokens[s.Origin] = s.Token
		}
	}
	return tokens
}

// tlsDispatchTransport routes each request to one of two independently
// pooled *http.Transports purely by the request URL's normalized origin,
// using the insecure pool if and only if that exact origin was configured
// with validate_certs=false. Everything else - including a redirect target,
// a download host, or an origin that merely resembles a configured one -
// goes to secure.
//
// This is deliberately two pools dispatched at request time rather than one
// transport with a per-address DialTLSContext, for two independent reasons:
//  1. http.Transport keys idle connections by {proxy, scheme, addr,
//     onlyH1} - tls.Config is not part of that key - so a single pool
//     serving both TLS policies would be correctness-by-remembering-to-
//     route-right rather than correctness-by-construction.
//  2. Setting DialTLSContext disables the transport's own ALPN-based HTTP/2
//     negotiation, silently defeating helpers.FetchForceAttemptHTTP2 for
//     every origin dialed through it, secure ones included.
type tlsDispatchTransport struct {
	secure   *http.Transport
	insecure *http.Transport // nil unless at least one configured server sets validate_certs=false
	// insecureOrigins is built once at construction (see insecureOriginSet)
	// and never mutated afterward, so concurrent RoundTrip calls need no
	// lock to read it.
	insecureOrigins map[string]struct{}
}

// RoundTrip dispatches req to the insecure transport only when one was
// built at all (so the common all-secure case never even indexes the map)
// and req's normalized origin is exactly one of insecureOrigins; every
// other request goes to secure.
func (t tlsDispatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.insecure != nil {
		if _, ok := t.insecureOrigins[helpers.Origin(req.URL)]; ok {
			return t.insecure.RoundTrip(req)
		}
	}
	return t.secure.RoundTrip(req)
}

type offlineTransport struct{}

// RoundTrip rejects every request with ErrOfflineMode.
func (offlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: %s %s", helpers.ErrOfflineMode, req.Method, req.URL)
}
