package fetch_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// tinyTimeout is small enough that a hung response fails the
// ResponseHeaderTimeout test in well under a second, but large enough that a
// normal in-process fakegalaxy round trip never trips it by accident.
const tinyTimeout = 50 * time.Millisecond

// waitBound bounds how long this file's tests wait for an HTTP round trip
// to return, so a regression that reintroduces an unbounded hang fails the
// test instead of hanging the suite.
const waitBound = 5 * time.Second

// TestNew_ResponseHeaderTimeout_FiresOnHang exercises the transport-level
// half of the no-progress timeout: a server that never writes a status line
// or headers must make the request fail once ResponseHeaderTimeout - wired
// to cfg.Timeout - elapses, rather than hang indefinitely (the whole
// response Timeout that used to bound this was removed).
func TestNew_ResponseHeaderTimeout_FiresOnHang(t *testing.T) {
	t.Parallel()

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "hangs", "1.0.0", nil)
	// Count: 1 is required here, not cosmetic: a zero-value Fault.Count
	// never matches ruleMatches (its exhausted-rule check treats Count == 0
	// as "already used up"), so an unbounded Hang needs an explicit
	// positive Count the same way every other fakegalaxy fault test does.
	s.Fail(fakegalaxy.EndpointRootMetadata, "acme", "hangs", fakegalaxy.Fault{Hang: true, Count: 1})

	client := fetch.New(tinyTimeout)
	url := fmt.Sprintf("%s/api/v3/collections/%s/%s/", s.URL(), "acme", "hangs")

	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}

	done := make(chan error, 1)
	go func() {
		resp, doErr := client.Do(req)
		if doErr == nil {
			// Not expected on this path, but close defensively so a
			// surprising success never leaks a connection.
			_ = resp.Body.Close()
		}
		done <- doErr
	}()

	select {
	case doErr := <-done:
		if doErr == nil {
			t.Fatal("Do error = nil, want a ResponseHeaderTimeout failure since the fake server hung before writing headers")
		}
	case <-time.After(waitBound):
		t.Fatal("Do did not return within the outer bound; ResponseHeaderTimeout failed to fire")
	}
}

// TestNew_SuccessfulRoundTrip_ReadsBodyAndCloses is a happy-path companion
// to the hang test above: it exercises the same client's watchdog-wrapped
// transport end to end against a server that answers normally, confirming
// the wrapping added around the response body does not disturb an ordinary
// request.
func TestNew_SuccessfulRoundTrip_ReadsBodyAndCloses(t *testing.T) {
	t.Parallel()

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "widgets", "1.0.0", nil)

	client := fetch.New(time.Second)
	url := fmt.Sprintf("%s/api/v3/collections/%s/%s/", s.URL(), "acme", "widgets")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext error = %v, want nil", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do error = %v, want nil", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("Body.Close error = %v, want nil", cerr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error = %v, want nil", err)
	}
	if len(body) == 0 {
		t.Fatal("ReadAll returned an empty body, want the root metadata JSON payload")
	}
	if got := s.Count(fakegalaxy.EndpointRootMetadata); got != 1 {
		t.Fatalf("EndpointRootMetadata count = %d, want 1", got)
	}
}
