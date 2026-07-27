package fetch

import (
	"fmt"
	"net/http"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// authTransport attaches "Authorization: Token <token>" to a request whose
// URL origin matches a configured server carrying a token, then forwards to
// base; every other request passes through unchanged.
//
// authTransport is a distinct type from tlsDispatchTransport - never merged
// into one - so that a revealed token and InsecureSkipVerify never live in
// the same editable struct, and so this type stays testable against a bare
// stub http.RoundTripper with no TLS setup involved at all.
type authTransport struct {
	base http.RoundTripper
	// tokens maps a normalized origin (helpers.Origin) to that server's
	// token. It is built once at construction (see tokensByOrigin) and
	// never mutated afterward, so concurrent RoundTrip calls need no lock
	// to read it.
	tokens map[string]string
}

// RoundTrip is the security-relevant half of this unit: it decides whether
// a request carries a Galaxy token at all.
//
// Matching is by ORIGIN (helpers.Origin), never a URL or host prefix or
// substring. An artifact's download_url can come straight out of a poisoned
// cache snapshot - a documented trust boundary (see CLAUDE.md) - so
// origin-exact matching is what keeps a poisoned URL from exfiltrating a
// token to a host that merely shares a scheme, a hostname, or a port with a
// configured server: only a byte-for-byte normalized origin match attaches
// the header at all.
//
// An Authorization header the caller already set is never overwritten - a
// caller-supplied credential, whatever its source, always takes precedence
// over one this transport would attach.
//
// req is never mutated: it is cloned via req.Clone before the header is
// set, per http.RoundTripper's contract that a RoundTripper must not modify
// the request it is given. This matters in particular across an
// http.Client-driven redirect: the client builds each hop's request fresh
// from its own state and calls RoundTrip again, so a cross-origin redirect
// re-evaluates this same origin check on the new request rather than
// inheriting whatever this transport did to the previous one.
func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return t.base.RoundTrip(req)
	}
	token, ok := t.tokens[helpers.Origin(req.URL)]
	if !ok {
		return t.base.RoundTrip(req)
	}

	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Token "+token)
	return t.base.RoundTrip(cloned)
}

// String renders authTransport without any token material, so a %v of the
// client's transport chain - a debug dump, a panic value, a failed test's
// diff - can never leak a configured token: only the count of
// token-bearing origins is shown, never which origin or what the token is.
func (t authTransport) String() string {
	return fmt.Sprintf("fetch.authTransport{origins: %d}", len(t.tokens))
}
