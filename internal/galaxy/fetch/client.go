package fetch

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// New creates a configured HTTP client with reasonable defaults.
func New(timeout time.Duration) *http.Client {
	return newClient(timeout, false)
}

// NewOffline creates an HTTP client whose transport rejects every request
// with helpers.ErrOfflineMode. Cache reads that don't go through the client
// continue to work; any code path that actually needs the network fails fast.
func NewOffline(timeout time.Duration) *http.Client {
	return newClient(timeout, true)
}

func newClient(timeout time.Duration, offline bool) *http.Client {
	if offline {
		// The offline transport rejects every request before it ever reaches
		// the network, so it never returns a body for the watchdog to guard;
		// it is deliberately left unwrapped.
		return &http.Client{Timeout: timeout, Transport: offlineTransport{}}
	}
	transport := &http.Transport{
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
	}
	// The client itself carries no Timeout: bounding the entire request
	// (headers plus however large the artifact body is) is exactly what
	// this change replaces. watchdogTransport instead guards every body read
	// individually, so a stalled connection is still caught without capping
	// total transfer time.
	return &http.Client{Transport: watchdogTransport{base: transport, idle: timeout}}
}

type offlineTransport struct{}

// RoundTrip rejects every request with ErrOfflineMode.
func (offlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: %s %s", helpers.ErrOfflineMode, req.Method, req.URL)
}
