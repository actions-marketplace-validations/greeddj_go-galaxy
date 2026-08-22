package fetch

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// URLBinding is fetch's own view of one url-source credential binding:
// exactly enough for urlAuthTransport below to decide, per request, whether
// a Bearer token attaches - without fetch importing internal/galaxy/config,
// for the layering reason ServerAuth states. The command layer builds this
// slice from config.Config.URLCredentials immediately before calling
// NewURLDownload; that construction site is the only place
// config.Secret.Reveal() is called for a url token.
type URLBinding struct {
	// Origin is the binding's normalized origin, rendered exactly as
	// helpers.Origin renders a request URL's (urlsource.Prefix.Origin()
	// guarantees the compatibility, IPv6 brackets included), so the match
	// below is a byte comparison.
	Origin string
	// PathPrefix is the binding's escaped path prefix with no trailing
	// slash, "" when the binding covers the whole origin.
	PathPrefix string
	// Token is the revealed Bearer token.
	Token string
}

// NewURLDownload creates the HTTP client a url collection or role source is
// downloaded over. Like NewUnauthenticated it is handed no server
// configuration, so no Galaxy token and no relaxed certificate check exists
// for any origin by construction - a url requirement naming a configured
// server's own origin still gets neither. What it adds is the url-source
// credential layer: a Bearer token the operator bound to an origin and path
// prefix through the GO_GALAXY_URL_* environment, attached per request by
// urlAuthTransport below.
//
// Redirects are followed across origins, deliberately - the ordinary GitHub
// release shape 302s into presigned object storage - and the credential
// decision is made again on every hop, so a hop into an unbound origin
// carries no Authorization header at all (see urlAuthTransport.RoundTrip).
// The one hop refused outright is one that leaves https for plaintext http
// (see checkURLRedirect). Offline selects the same refuse-everything
// transport NewOffline builds, under the same redirect policy for the same
// one-symbol-per-client reason newClient states.
func NewURLDownload(timeout time.Duration, offline bool, bindings []URLBinding) *http.Client {
	client := newClient(timeout, offline, nil)
	client.CheckRedirect = checkURLRedirect
	if offline {
		return client
	}
	client.Transport = urlAuthTransport{base: client.Transport, bindings: bindings}
	return client
}

// urlAuthTransport attaches "Authorization: Bearer <token>" to a request
// whose URL matches one of the operator's url-source bindings, then forwards
// to base; every other request passes through unchanged. It is a distinct
// type from authTransport rather than a parameterization of it: the two
// differ in scheme ("Bearer" against "Token"), in match rule (origin plus
// longest path prefix against exact origin), and in what a mistake costs, so
// sharing code between them would couple two security decisions that must be
// reviewable one at a time.
type urlAuthTransport struct {
	base http.RoundTripper
	// bindings is built once at construction and never mutated afterward, so
	// concurrent RoundTrip calls need no lock to read it.
	bindings []URLBinding
}

// RoundTrip is the security-relevant half of this unit: it decides whether a
// request carries a url-source token at all.
//
// Matching is by normalized origin, byte for byte, plus the longest binding
// path prefix the request path equals or sits beneath on a "/" boundary. A
// tie is impossible: config refused two bindings on one canonical URL. The
// prefix comparison folds nothing - it reads the escaped path as written -
// so a request path carrying a dot or empty segment (which a server would
// resolve, letting a path spend a prefix-bound credential elsewhere on the
// host) attaches nothing; the one empty segment exempted is an embedded
// absolute URL's scheme separator, the caching-proxy shape
// pathHasUnsafeSegment describes. http.Client's own redirect handling makes such a
// path unreachable for a redirect hop (Location is resolved through
// ResolveReference, which cleans dot segments), but this transport must not
// depend on who built the request; failing safe is failing to a
// credential-less request.
//
// An Authorization header the caller already set is never overwritten, and
// req is cloned rather than mutated, both for the reasons
// authTransport.RoundTrip gives. The per-hop consequence is this design's
// point: the client builds each redirect hop's request fresh and calls
// RoundTrip again, so a hop into an unbound origin - GitHub's 302 into
// presigned object storage - carries no Authorization header at all. That is
// not merely the safe behavior but the working one: an S3-shaped endpoint
// refuses a request carrying both an Authorization header and query-string
// signing. This deliberately does not lean on net/http's own
// shouldCopyHeaderOnRedirect stripping, which forwards a pre-set
// Authorization header to every subdomain of the host that set it. The same
// re-evaluation also means a hop INTO a bound origin gains that binding's
// token: the credential is bound to the destination, however the request got
// there.
func (t urlAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return t.base.RoundTrip(req)
	}
	token, ok := matchURLBinding(t.bindings, req.URL)
	if !ok {
		return t.base.RoundTrip(req)
	}
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(cloned)
}

// String renders urlAuthTransport without any token material, for the reason
// authTransport.String states: only the count of bindings is shown, never
// which origin or what a token is.
func (t urlAuthTransport) String() string {
	return fmt.Sprintf("fetch.urlAuthTransport{bindings: %d}", len(t.bindings))
}

// matchURLBinding returns the token of the binding that covers u, choosing
// the longest matching path prefix among bindings on u's exact origin, and
// false when none covers it or the path carries a segment the doc comment on
// RoundTrip refuses to match on.
func matchURLBinding(bindings []URLBinding, u *url.URL) (string, bool) {
	origin := helpers.Origin(u)
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	if pathHasUnsafeSegment(path) {
		return "", false
	}
	best, bestLen, found := "", -1, false
	for _, b := range bindings {
		if b.Origin != origin {
			continue
		}
		prefix := strings.TrimPrefix(b.PathPrefix, "/")
		if prefix != "" && path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		if len(prefix) > bestLen {
			best, bestLen, found = b.Token, len(prefix), true
		}
	}
	return best, found
}

// pathHasUnsafeSegment reports whether any "/"-separated segment of path is
// empty or folds to "." or ".." once lower-cased with "%2e" read as the dot
// it decodes to - the same fold urlsource applies at the requirements
// boundary, repeated here because this transport must hold for any request
// it is handed, not only one that passed that boundary. The one empty
// segment tolerated is the "//" separator of an embedded absolute http(s)
// URL - the caching-proxy path shape urlsource admits - recognized by the
// canonical lower-case "http:"/"https:" segment immediately before it, read
// as written with no folding: a request spelling that separator any other
// way fails safe to a credential-less request.
func pathHasUnsafeSegment(path string) bool {
	if path == "" {
		return false
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		folded := strings.ReplaceAll(strings.ToLower(segment), "%2e", ".")
		if folded == "." || folded == ".." {
			return true
		}
		if folded != "" {
			continue
		}
		if i == 0 || (segments[i-1] != "http:" && segments[i-1] != "https:") {
			return true
		}
	}
	return false
}

// checkURLRedirect is checkRedirect plus the one refusal NewURLDownload
// states: a hop that leaves https for plaintext http. The rule is
// unconditional rather than credential-conditioned - a credential could
// never attach to the http hop anyway, since the scheme is part of the
// origin and config refuses a non-loopback http binding - because what it
// protects is the artifact bytes themselves: a first, unpinned fetch of a
// url source is what every later sha256 check descends from, and an https
// requirement whose bytes detour over plaintext would undermine that pin at
// its root. The via[0] comparison makes the requirement URL's own scheme the
// floor: a chain that started on https may never drop below it, while one
// the operator already spelled as http gave up nothing this rule could keep.
// The refusal wraps helpers.ErrDownloadFailed so it classifies as the
// download failure it is; the retry loop will re-refuse it identically,
// which is wasted but bounded by the existing retry budget.
func checkURLRedirect(req *http.Request, via []*http.Request) error {
	if err := checkRedirect(req, via); err != nil {
		return err
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("%w: redirect from %s to %s leaves https for plaintext http; refusing to follow",
			helpers.ErrDownloadFailed, helpers.URLForMessage(via[0].URL.String()), helpers.URLForMessage(req.URL.String()))
	}
	return nil
}
