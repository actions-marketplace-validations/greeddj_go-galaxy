// Package fetch builds this program's *http.Clients. New builds the one every
// credential-bearing outbound request shares, the S3 cache backend's included,
// stacking three transports over the pooled ones: a watchdog that fails a body
// read which has stopped making progress, token attachment, and TLS-policy
// dispatch. The client itself carries no overall Timeout, deliberately - the
// watchdog bounds inactivity instead, so a slow but steadily streaming artifact
// download is not truncated for being large.
//
// Both credential-bearing layers key on the request URL's normalized origin
// (helpers.Origin) and never on a server id, a configured URL, or a prefix
// match, so a URL that merely resembles a configured server receives neither
// its token nor its relaxed certificate checking.
//
// Two constructors deliberately do not hand back that shared client, and each
// closes something off rather than merely configuring it differently.
// NewOffline builds the same client shape around a transport that refuses every
// request instead. NewUnauthenticated builds one that is handed no server
// configuration at all, so neither credential-bearing layer has anything it
// could match - which is what a request driven by repository content needs.
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

// NewUnauthenticated creates an HTTP client that can attach no credential to
// any request and can relax no certificate check for any origin, offline
// selecting the same refuse-everything transport NewOffline builds.
//
// It takes no server configuration at all, which is what makes those two
// properties belong to the signature rather than to whatever a caller passed:
// tokensByOrigin(nil) is an empty map, so authTransport can never match an
// origin, and insecureOriginSet(nil) is empty too, so dispatch.insecure stays
// nil and every request goes to the fully-verified transport. Both paths are
// closed by construction, which is strictly stronger than a caller remembering
// to pass an empty slice.
//
// A request whose URL is repository content needs exactly that. The values in a
// requirements file's signatures: block are authored by whoever can commit to
// the repository, not by the operator running the install, while both
// credential-bearing layers dispatch on a bare helpers.Origin match - so on the
// shared client a hostile repository naming a configured hub's origin would get
// a token-bearing request to a path of its own choosing, or a silently
// unverified certificate on an origin an operator had relaxed for their own
// server. ansible draws the same line for signature retrieval: it attaches no
// credential there and forces validate_certs=True whatever the server's own
// setting says.
//
// Redirects are followed exactly as the shared client follows them, and that is
// safe here precisely because nothing rides along. Two of the three things that
// make that true are properties of this constructor, argued above: a hop into
// another origin carries no token to leak and inherits no relaxed TLS policy,
// since this client holds neither for any origin. The third is not this
// constructor's - net/http would otherwise put the previous URL, query string
// and all, into the redirect target's Referer header - and it is closed for
// every client this package builds by checkRedirect below.
func NewUnauthenticated(timeout time.Duration, offline bool) *http.Client {
	return newClient(timeout, offline, nil)
}

// checkRedirect is assigned to every client this package returns, and it does
// two things a client that assigned nothing would not.
//
// It deletes the Referer header net/http composes for a redirect hop.
// refererForURL strips a URL's userinfo and suppresses the header entirely on
// an https->http downgrade, but it KEEPS the query string, so a presigned
// source - the exact shape helpers.WithoutQuery exists to keep out of every
// sink that outlives a request - is handed verbatim to whatever origin a
// redirect points at. Observed on the wire, and observable again by deleting
// the Del below: a request for a URL carrying "?X-Amz-Signature=..." answered
// with a 302 to another origin delivers that query to the redirect target in
// Referer. CheckRedirect runs after that header has been set on the outgoing
// request, which is what makes deleting it here the whole fix rather than half.
//
// It also re-imposes the redirect ceiling, and that half is not optional:
// setting CheckRedirect REPLACES http.Client's own defaultCheckRedirect, so a
// hook returning nil unconditionally would turn a hostile redirect loop into an
// unbounded one. The error mirrors Go's own shape rather than minting a
// sentinel of this project's, so an exhausted redirect budget keeps classifying
// exactly as it did before this hook existed.
//
// It belongs to newClient rather than to one constructor because the shape it
// protects against is not specific to any of them: an artifact download URL is
// routinely a presigned object-storage URL, so the shared credential-bearing
// client carries the same capability in its query string that a repository's
// own signature source can.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= helpers.FetchMaxRedirects {
		// A rendered message rather than a sentinel, deliberately: this
		// reproduces net/http's own defaultCheckRedirect wording, so an
		// exhausted redirect budget keeps classifying exactly as it did when
		// net/http raised it, and nothing gains a new value to match on.
		//nolint:err113 // mirrors net/http's own defaultCheckRedirect error, never a sentinel to match
		return fmt.Errorf("stopped after %d redirects", helpers.FetchMaxRedirects)
	}
	req.Header.Del("Referer")

	return nil
}

func newClient(timeout time.Duration, offline bool, servers []ServerAuth) *http.Client {
	if offline {
		// The offline transport rejects every request before it ever reaches
		// the network, so it never returns a body for the watchdog to guard;
		// it is deliberately left unwrapped. CheckRedirect is still assigned:
		// one symbol on every client this package builds leaves no question
		// about which of them can be redirected, and this one cannot.
		return &http.Client{Timeout: timeout, Transport: offlineTransport{}, CheckRedirect: checkRedirect}
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
	return &http.Client{Transport: watchdogTransport{base: auth, idle: timeout}, CheckRedirect: checkRedirect}
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
		// ResponseHeaderTimeout bounds time-to-first-byte: timeout is a
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

// RoundTrip rejects every request with ErrOfflineMode, naming the method and
// the request URL under helpers.WithoutCredentials.
//
// The cut belongs here rather than at whatever renders this error, and that is
// a rule binding every RoundTripper this program installs rather than a
// property of this one. A transport error is reproduced verbatim by the
// wrappers that cut a URL further up - helpers.TransportURLError renders its
// own cut display and then prints the cause beside it - so a transport naming
// its own req.URL puts back exactly what those wrappers removed. net/http's
// own masking is no help either: it rewrites the URL of the *url.Error its
// client composes, never an error a RoundTripper built underneath it, so
// req.URL.String() here renders userinfo with the password in cleartext and
// the whole query alongside it.
//
// What the cut costs is nothing this message is for: the method, scheme, host
// and path all survive, which is the whole of what an operator needs to see
// which request --offline refused.
func (offlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: %s %s", helpers.ErrOfflineMode, req.Method, helpers.WithoutCredentials(req.URL.String()))
}
