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
		return &http.Client{Timeout: timeout, Transport: offlineTransport{}}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
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
		},
	}
}

type offlineTransport struct{}

// RoundTrip rejects every request with ErrOfflineMode.
func (offlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: %s %s", helpers.ErrOfflineMode, req.Method, req.URL)
}
