package helpers

import (
	"errors"
	"fmt"
	"net/url"
)

// TransportURLError renders a failed HTTP request without the capability its
// URL carries, keeping the transport failure itself reachable through Unwrap.
// CutTransportURL below is its only producer and holds why both halves are
// needed.
type TransportURLError struct {
	err     *url.Error
	display string
}

// CutTransportURL re-renders the *url.Error http.Client.Do returns so the
// message names rawURL cut, and returns err untouched for anything that is not
// one.
//
// net/http masks a URL's password before composing that error and leaves
// everything else, so what survives its own redaction is the whole query
// string - and on a URL a server chose, that query is a capability rather than
// a detail of one. This arm is also the one reached without any server having
// answered: a refused dial, a DNS failure and a TLS handshake error all land
// here, which is a wider set of occasions than a non-200 response has.
//
// The original error stays reachable through Unwrap, which is what keeps this
// a rendering change and nothing else: every classifier over one of these
// trees reads it through errors.Is or errors.As, and both traverse Unwrap. A
// classifier that type-asserts instead would be the one thing that breaks
// here, and that is a rule about classifiers rather than about this wrapper.
//
// One type serves every caller because the whole of what a caller contributes
// is rawURL: the shape of the message, the cut applied to it and the
// classification left underneath are the same question wherever an
// http.Client.Do failure is re-rendered. What differs between callers is why
// that value is worth cutting, which is what each call site states for itself.
func CutTransportURL(rawURL string, err error) error {
	urlErr, ok := errors.AsType[*url.Error](err)
	if !ok {
		return err
	}

	return &TransportURLError{err: urlErr, display: WithoutCredentials(rawURL)}
}

// Error renders the *url.Error's own shape - operation, URL, cause - over the
// cut URL.
//
// What an operator sees is not net/http's own message minus its
// credential-bearing parts, and the difference is a redirect. Measured, a
// request for "http://u:s3cr3t@origin/start?orig=1" answered with a 302 to a
// second host that then refused the connection renders, from net/http:
//
//	Get "http://127.0.0.1:55811/final?X-Amz-Signature=deadbeef": dial tcp ...: connect: connection refused
//
// That is the FINAL hop with its query whole, since url.Error.URL is built
// from the request that actually failed while its Op comes from the first one.
// This type names the URL the caller asked for instead, so the same failure
// names the original host and path under both cuts and the presigned query of
// a hop this program never chose does not appear at all. That is the larger
// win rather than a smaller one: a deployment answering a metadata or artifact
// request with a redirect into presigned object storage is the ordinary shape,
// and it is exactly the shape whose uncut rendering hands out a live
// capability.
//
// It costs a redirected failure its hop. The message no longer says which URL
// in the chain could not be reached, only which one was asked for - accepted
// rather than unnoticed, since the cause printed beside it still names what
// went wrong, and the address of a hop the operator configured nowhere is not
// what they act on.
//
// The cause is the *url.Error's inner error rather than the whole of it, which
// is what keeps the cut meaningful: rendering e.err would put net/http's own
// uncut URL straight back. That leaves the cause as the one part of this line
// no cut here reaches, which is why no RoundTripper this program installs may
// render its own request URL uncut - internal/galaxy/fetch's offlineTransport
// is the one that renders a URL at all, and it cuts.
func (e *TransportURLError) Error() string {
	return fmt.Sprintf("%s %q: %s", e.err.Op, e.display, e.err.Err)
}

// Unwrap exposes the original *url.Error, so every errors.Is and errors.As
// caller downstream classifies exactly what it classified before.
func (e *TransportURLError) Unwrap() error {
	return e.err
}
