package fetch

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// NewGit creates the HTTP client a git collection source is fetched over. It
// is NewUnauthenticated's transport - no Galaxy token for any origin, no
// relaxed certificate check for any origin, the stall watchdog, the proxy
// from the environment - under a stricter redirect policy, and the policy is
// the reason this is a separate constructor rather than a call to that one.
//
// NewUnauthenticated follows a redirect into another origin because nothing
// rides along on its requests. On this client something does: go-git sets a
// Basic Authorization header on every request of an authenticated session,
// and net/http's own redirect handling forwards that header to any subdomain
// of the host that set it, on any port, and across an https-to-http
// downgrade. go-git strips the credential from the session's NEXT request
// when a redirect changes the host or port, but the redirected request
// itself has already carried it. So a redirect that changes the scheme, the
// host or the port is refused here outright, before net/http issues it, and
// the operator is told to write the address the remote actually serves. A
// same-origin redirect (the common /repo to /repo.git/info/refs shape) is
// followed, under the same Referer deletion and hop ceiling every client in
// this package applies.
//
// A run that needs a git remote under --offline never reaches this client:
// every caller refuses a git acquisition before opening a session, so there
// is no offline variant to build.
func NewGit(timeout time.Duration) *http.Client {
	client := newClient(timeout, false, nil)
	client.CheckRedirect = checkGitRedirect
	client.Transport = errorBodyCapTransport{base: client.Transport, max: helpers.GitErrorBodyMaxSize}
	return client
}

// errorBodyCapTransport bounds the body of a non-2xx response. go-git reads
// such a body whole to compose its error, and nothing else on this client
// bounds it: the watchdog fires on inactivity, not on volume, and the pack
// cap counts only bytes written to the object store. It wraps the watchdog
// rather than sitting beneath it, so a stalled error body is still cut off by
// inactivity and a flooding one by size.
type errorBodyCapTransport struct {
	base http.RoundTripper
	max  int64
}

func (t errorBodyCapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || (resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices) {
		return resp, err
	}
	resp.Body = cappedBody{Reader: io.LimitReader(resp.Body, t.max), Closer: resp.Body}
	return resp, nil
}

// cappedBody is a response body truncated at the cap; Close still closes the
// underlying body so the connection is released.
type cappedBody struct {
	io.Reader
	io.Closer
}

// checkGitRedirect is checkRedirect plus the same-origin rule NewGit
// describes. via[0] is the request the session started with; a hop whose
// scheme, host or port differs from it is refused with the git transport
// sentinel so the failure classifies as the wire failure it is.
func checkGitRedirect(req *http.Request, via []*http.Request) error {
	if err := checkRedirect(req, via); err != nil {
		return err
	}
	if len(via) == 0 {
		return nil
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return fmt.Errorf("%w: redirect from %s to %s leaves the origin; write the address the remote serves",
			helpers.ErrGitTransportFailed, helpers.URLForMessage(first.String()), helpers.URLForMessage(req.URL.String()))
	}
	return nil
}
