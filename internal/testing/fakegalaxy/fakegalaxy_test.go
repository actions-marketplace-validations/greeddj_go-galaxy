package fakegalaxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/psvmcc/hub/pkg/types"
)

// doGet issues a GET against url with a background context, failing the
// test on any transport error. The caller owns the returned response and
// must close its body.
func doGet(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// getJSON performs a GET against url and decodes the JSON response body
// into target (skipped when target is nil or the status is not 200),
// returning the response's status code. The body is always drained and
// closed before returning.
func getJSON(t *testing.T, client *http.Client, url string, target any) int {
	t.Helper()
	resp := doGet(t, client, url)
	defer func() { _ = resp.Body.Close() }()
	if target != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			t.Fatalf("decode response from %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

// getConditional issues a GET against url carrying the given If-None-Match
// value, returning the response's status code and its (fully drained)
// body.
func getConditional(t *testing.T, client *http.Client, url, ifNoneMatch string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	req.Header.Set("If-None-Match", ifNoneMatch)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("conditional GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read conditional response from %s: %v", url, err)
	}
	return resp.StatusCode, body
}

// TestRootMetadataReflectsHighestVersion registers two versions and asserts
// root metadata reports the higher one as highest_version.
func TestRootMetadataReflectsHighestVersion(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.AddVersion("ns", "name", "2.5.0", nil)

	var root types.GalaxyCollection
	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name", &root)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	wantVersionsURL := s.URL() + "/api/v3/collections/ns/name/versions/"
	if root.VersionsURL != wantVersionsURL {
		t.Errorf("versions_url = %q, want %q", root.VersionsURL, wantVersionsURL)
	}
	if root.HighestVersion.Version != "2.5.0" {
		t.Errorf("highest_version.version = %q, want %q", root.HighestVersion.Version, "2.5.0")
	}
}

// TestV2AndBareAPIProbes404 asserts a real client's v2/bare-API probes 404
// rather than being served v3 metadata.
func TestV2AndBareAPIProbes404(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	for _, path := range []string{"/api/v2/collections/ns/name/", "/api/collections/ns/name/"} {
		if status := getJSON(t, s.Client(), s.URL()+path, nil); status != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want %d", path, status, http.StatusNotFound)
		}
	}
}

// TestVersionsListPagination registers three versions and asserts limit/offset
// paginate them, returning the whole set when neither is given.
func TestVersionsListPagination(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.AddVersion("ns", "name", "1.1.0", nil)
	s.AddVersion("ns", "name", "2.0.0", nil)

	base := s.URL() + "/api/v3/collections/ns/name/versions"
	cases := []struct {
		name      string
		query     string
		wantFirst string
		wantLen   int
	}{
		{name: "first page", query: "?limit=1&offset=0", wantLen: 1, wantFirst: "1.0.0"},
		{name: "third page", query: "?limit=1&offset=2", wantLen: 1, wantFirst: "2.0.0"},
		{name: "no params returns everything", query: "", wantLen: 3, wantFirst: "1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var versions types.GalaxyCollectionVersions
			status := getJSON(t, s.Client(), base+tc.query, &versions)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			if versions.Meta.Count != 3 {
				t.Errorf("meta.count = %d, want 3", versions.Meta.Count)
			}
			if len(versions.Data) != tc.wantLen {
				t.Fatalf("len(data) = %d, want %d", len(versions.Data), tc.wantLen)
			}
			if versions.Data[0].Version != tc.wantFirst {
				t.Errorf("data[0].version = %q, want %q", versions.Data[0].Version, tc.wantFirst)
			}
		})
	}
}

// TestVersionDetail asserts a registered version's detail body carries the
// download URL, the artifact sha256 matching the value AddVersion returned,
// and the dependencies round-tripped through metadata.dependencies; an
// unregistered version 404s.
func TestVersionDetail(t *testing.T) {
	t.Parallel()
	s := New(t)
	deps := map[string]string{"ns2.dep": ">=1.0.0"}
	v := s.AddVersion("ns", "name", "1.0.0", deps)

	var info types.GalaxyCollectionVersionInfo
	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/1.0.0/", &info)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	wantDownloadURL := s.URL() + "/download/ns-name-1.0.0.tar.gz"
	if info.DownloadURL != wantDownloadURL {
		t.Errorf("download_url = %q, want %q", info.DownloadURL, wantDownloadURL)
	}
	if info.Artifact.Sha256 != v.SHA256 {
		t.Errorf("artifact.sha256 = %q, want %q", info.Artifact.Sha256, v.SHA256)
	}
	if !maps.Equal(info.Metadata.Dependencies, deps) {
		t.Errorf("metadata.dependencies = %v, want %v", info.Metadata.Dependencies, deps)
	}

	status = getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/9.9.9/", nil)
	if status != http.StatusNotFound {
		t.Errorf("unknown version status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestArtifactChecksumAndExtract asserts the artifact download's bytes hash
// to the sha256 AddVersion reported, and that a real extractor can unpack the
// generated tarball.
func TestArtifactChecksumAndExtract(t *testing.T) {
	t.Parallel()
	s := New(t)
	v := s.AddVersion("ns", "name", "1.0.0", map[string]string{"ns2.dep": "*"})

	resp := doGet(t, s.Client(), s.URL()+"/download/ns-name-1.0.0.tar.gz")
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read artifact body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != v.SHA256 {
		t.Errorf("artifact sha256 = %q, want %q", got, v.SHA256)
	}

	dir := t.TempDir()
	if err := archive.ExtractTarGzStream(bytes.NewReader(body), dir); err != nil {
		t.Fatalf("ExtractTarGzStream: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "MANIFEST.json")); err != nil {
		t.Errorf("MANIFEST.json missing after extraction: %v", err)
	}
}

// TestETagConditionalGet asserts a JSON endpoint's ETag round-trips through
// If-None-Match, yielding 304 with an empty body on a match and 200 with the
// body on a mismatch.
func TestETagConditionalGet(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	url := s.URL() + "/api/v3/collections/ns/name"

	first := doGet(t, s.Client(), url)
	etag := first.Header.Get("ETag")
	_ = first.Body.Close()
	if etag == "" {
		t.Fatal("first response carries no ETag")
	}

	status, body := getConditional(t, s.Client(), url, etag)
	if status != http.StatusNotModified {
		t.Errorf("matching If-None-Match status = %d, want %d", status, http.StatusNotModified)
	}
	if len(body) != 0 {
		t.Errorf("matching If-None-Match body = %d bytes, want empty", len(body))
	}

	status, body = getConditional(t, s.Client(), url, `"0000000000000000"`)
	if status != http.StatusOK {
		t.Errorf("mismatching If-None-Match status = %d, want %d", status, http.StatusOK)
	}
	if len(body) == 0 {
		t.Error("mismatching If-None-Match body is empty, want the full JSON body")
	}
}

// TestFaultStatusOnce asserts a Fault with Count 1 affects exactly the next
// matching request, then stops.
func TestFaultStatusOnce(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "ns", "name", Fault{Status: http.StatusTooManyRequests, Count: 1})

	url := s.URL() + "/api/v3/collections/ns/name"
	if status := getJSON(t, s.Client(), url, nil); status != http.StatusTooManyRequests {
		t.Errorf("first GET status = %d, want %d", status, http.StatusTooManyRequests)
	}
	if status := getJSON(t, s.Client(), url, nil); status != http.StatusOK {
		t.Errorf("second GET status = %d, want %d", status, http.StatusOK)
	}
}

// TestFaultStatusIndefinite asserts a non-positive Count keeps failing every
// matching request.
func TestFaultStatusIndefinite(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "ns", "name", Fault{Status: http.StatusInternalServerError, Count: -1})

	url := s.URL() + "/api/v3/collections/ns/name"
	for i := range 2 {
		if status := getJSON(t, s.Client(), url, nil); status != http.StatusInternalServerError {
			t.Errorf("GET #%d status = %d, want %d", i+1, status, http.StatusInternalServerError)
		}
	}
}

// TestFaultHangUnblocksOnContextCancellation asserts a Fault with Hang set
// blocks the handler until the request's context ends, and unblocks (rather
// than hanging forever) once it does.
func TestFaultHangUnblocksOnContextCancellation(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "ns", "name", Fault{Hang: true, Count: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL()+"/api/v3/collections/ns/name", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := s.Client().Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the hung request to fail once its context ended, got nil error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected a context deadline exceeded error, got %v", err)
	}
}

// TestFaultStallAfterBytesDeliversRealPrefixThenBlocks asserts a
// StallAfterBytes artifact fault delivers exactly that many real artifact
// bytes, then blocks until the request context ends, and consumes its Count
// so a later request serves the full artifact.
func TestFaultStallAfterBytesDeliversRealPrefixThenBlocks(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)
	s.Fail(EndpointArtifact, "ns", "name", Fault{StallAfterBytes: 4, Count: 1})

	url := s.URL() + "/download/ns-name-1.0.0.tar.gz"

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The 4-byte prefix arrives on loopback in microseconds, well before the
	// 100ms deadline: this read must succeed.
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, prefix); err != nil {
		t.Fatalf("read stalled prefix: %v", err)
	}

	// A further read must block until the context's deadline ends the
	// request, surfacing as a non-nil error rather than more data.
	if n, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Fatalf("read past the stalled prefix: n=%d, err=nil, want a non-nil error", n)
	}

	// The fault's Count was consumed by the first request, so a second
	// request serves the full, unstalled artifact, whose first 4 bytes must
	// match the prefix already observed above.
	full := doGet(t, s.Client(), url)
	fullBody, err := io.ReadAll(full.Body)
	_ = full.Body.Close()
	if err != nil {
		t.Fatalf("read full artifact body: %v", err)
	}
	if full.StatusCode != http.StatusOK {
		t.Fatalf("second GET status = %d, want %d", full.StatusCode, http.StatusOK)
	}
	if !bytes.Equal(fullBody[:len(prefix)], prefix) {
		t.Errorf("fullBody[:4] = %x, want %x (the stalled prefix)", fullBody[:len(prefix)], prefix)
	}
}

// TestCounters asserts per-endpoint and total request counts, and ResetCounts
// zeroing them.
func TestCounters(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name", nil)
	getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions", nil)
	getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/1.0.0/", nil)
	getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/name/versions/1.0.0/", nil)
	resp := doGet(t, s.Client(), s.URL()+"/download/ns-name-1.0.0.tar.gz")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if got := s.Count(EndpointRootMetadata); got != 1 {
		t.Errorf("Count(EndpointRootMetadata) = %d, want 1", got)
	}
	if got := s.Count(EndpointVersionsList); got != 1 {
		t.Errorf("Count(EndpointVersionsList) = %d, want 1", got)
	}
	if got := s.Count(EndpointVersionDetail); got != 2 {
		t.Errorf("Count(EndpointVersionDetail) = %d, want 2", got)
	}
	if got := s.Count(EndpointArtifact); got != 1 {
		t.Errorf("Count(EndpointArtifact) = %d, want 1", got)
	}
	if got := s.Total(); got != 5 {
		t.Errorf("Total() = %d, want 5", got)
	}

	s.ResetCounts()
	if got := s.Total(); got != 0 {
		t.Errorf("Total() after ResetCounts = %d, want 0", got)
	}
	for ep := EndpointRootMetadata; ep <= EndpointArtifact; ep++ {
		if got := s.Count(ep); got != 0 {
			t.Errorf("Count(%d) after ResetCounts = %d, want 0", ep, got)
		}
	}
}

// TestFailWildcardAndNamespaceMatching asserts ruleMatches' namespace/name
// wildcard semantics: an empty namespace/name on a Fail call matches any
// collection, while a rule pinned to one namespace must not trip for a
// request against a different one.
func TestFailWildcardAndNamespaceMatching(t *testing.T) {
	t.Parallel()

	t.Run("wildcard fault fires for a collection never named in Fail", func(t *testing.T) {
		t.Parallel()
		s := New(t)
		s.Fail(EndpointRootMetadata, "", "", Fault{Status: http.StatusTooManyRequests, Count: 1})
		s.AddVersion("other", "thing", "1.0.0", nil)

		url := s.URL() + "/api/v3/collections/other/thing"
		if status := getJSON(t, s.Client(), url, nil); status != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d", status, http.StatusTooManyRequests)
		}
	})

	t.Run("namespace-pinned fault does not fire for a different namespace", func(t *testing.T) {
		t.Parallel()
		s := New(t)
		s.Fail(EndpointRootMetadata, "ns1", "x", Fault{Status: http.StatusTooManyRequests, Count: 1})
		s.AddVersion("ns2", "x", "1.0.0", nil)

		url := s.URL() + "/api/v3/collections/ns2/x"
		if status := getJSON(t, s.Client(), url, nil); status != http.StatusOK {
			t.Errorf("status = %d, want %d", status, http.StatusOK)
		}
	})
}

// TestRootMetadataUnregistered404 asserts root metadata 404s for a
// namespace/name that was never registered, even while a different
// collection is.
func TestRootMetadataUnregistered404(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/unknown", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestVersionsListUnregistered404 asserts the versions-list route 404s for
// a namespace/name that was never registered.
func TestVersionsListUnregistered404(t *testing.T) {
	t.Parallel()
	s := New(t)

	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/ns/unknown/versions", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestArtifactUnknownFilename404 asserts the download route 404s for a
// filename that was never registered, even while a different artifact is.
func TestArtifactUnknownFilename404(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("ns", "name", "1.0.0", nil)

	resp := doGet(t, s.Client(), s.URL()+"/download/does-not-exist.tar.gz")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestFaultFiresOnEachEndpoint asserts each of the four routes passes its
// own Endpoint constant into applyFault: arming a one-shot fault against
// one endpoint trips exactly the first request to it, while the second
// request gets the fake's normal response.
func TestFaultFiresOnEachEndpoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		path       string
		endpoint   Endpoint
		wantStatus int
	}{
		{
			name:       "root metadata",
			path:       "/api/v3/collections/acme/name",
			endpoint:   EndpointRootMetadata,
			wantStatus: http.StatusOK,
		},
		{
			name:       "versions list",
			path:       "/api/v3/collections/acme/name/versions",
			endpoint:   EndpointVersionsList,
			wantStatus: http.StatusOK,
		},
		{
			name:       "version detail",
			path:       "/api/v3/collections/acme/name/versions/1.0.0/",
			endpoint:   EndpointVersionDetail,
			wantStatus: http.StatusOK,
		},
		{
			name:       "artifact",
			path:       "/download/acme-name-1.0.0.tar.gz",
			endpoint:   EndpointArtifact,
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New(t)
			s.AddVersion("acme", "name", "1.0.0", nil)
			s.Fail(tc.endpoint, "acme", "name", Fault{Status: http.StatusTooManyRequests, Count: 1})

			url := s.URL() + tc.path
			if status := getJSON(t, s.Client(), url, nil); status != http.StatusTooManyRequests {
				t.Errorf("first request status = %d, want %d", status, http.StatusTooManyRequests)
			}
			if status := getJSON(t, s.Client(), url, nil); status != tc.wantStatus {
				t.Errorf("second request status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

// TestFaultNoopFallsThroughToNormalResponse asserts a Fault with neither
// Status nor Hang set is still consumed by consumeFault (its Count is
// decremented) but enactFault treats it as a no-op, letting the request
// fall through to the fake's normal response body.
func TestFaultNoopFallsThroughToNormalResponse(t *testing.T) {
	t.Parallel()
	s := New(t)
	s.AddVersion("acme", "name", "1.0.0", nil)
	s.Fail(EndpointRootMetadata, "acme", "name", Fault{Count: 1})

	var root types.GalaxyCollection
	status := getJSON(t, s.Client(), s.URL()+"/api/v3/collections/acme/name", &root)
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if root.HighestVersion.Version != "1.0.0" {
		t.Errorf("highest_version.version = %q, want %q", root.HighestVersion.Version, "1.0.0")
	}
}

// TestCompareDottedVersionsAndComponents directly exercises
// compareDottedComponent and compareDottedVersions: numeric components
// compare by magnitude rather than lexically, non-numeric components fall
// back to a lexical comparison, and a shorter version sorts before a longer
// one that only adds trailing zero components.
func TestCompareDottedVersionsAndComponents(t *testing.T) {
	t.Parallel()

	if got := compareDottedComponent("2", "10"); got != -1 {
		t.Errorf(`compareDottedComponent("2", "10") = %d, want -1 (numeric, not lexical)`, got)
	}
	if got := compareDottedComponent("beta", "alpha"); got <= 0 {
		t.Errorf(`compareDottedComponent("beta", "alpha") = %d, want > 0 (lexical fallback)`, got)
	}
	if got := compareDottedVersions("1.2", "1.2.0"); got >= 0 {
		t.Errorf(`compareDottedVersions("1.2", "1.2.0") = %d, want < 0 (shorter arity sorts first)`, got)
	}
}

// TestPaginateClamps directly exercises paginate's boundary clamps: a
// negative offset, an offset past the slice's end, a negative limit, and an
// offset/limit sum that overflows int and wraps to a value below offset -
// the scenario paginate's final clamp guards against, since parseQueryInt
// places no upper bound on a caller-supplied limit.
func TestPaginateClamps(t *testing.T) {
	t.Parallel()
	versions := []string{"1.0.0", "1.1.0", "2.0.0"}

	cases := []struct {
		name   string
		want   []string
		offset int
		limit  int
	}{
		{name: "negative offset clamped to zero", want: []string{"1.0.0", "1.1.0"}, offset: -5, limit: 2},
		{name: "offset past end yields empty slice", want: []string{}, offset: 10, limit: 2},
		{name: "negative limit returns through end of slice", want: []string{"1.1.0", "2.0.0"}, offset: 1, limit: -1},
		{name: "overflowing sum wraps below offset", want: []string{}, offset: 1, limit: math.MaxInt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := paginate(versions, tc.offset, tc.limit)
			if got == nil {
				t.Fatal("paginate returned a nil slice, want non-nil")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("paginate(versions, %d, %d) = %v, want %v", tc.offset, tc.limit, got, tc.want)
			}
		})
	}
}

// TestParseDigitsAndQueryInt directly exercises parseDigits' rejection of
// empty and non-purely-numeric input (with a plain digit string as a
// positive control), and parseQueryInt's fallback to its default on a
// malformed or altogether absent query parameter.
func TestParseDigitsAndQueryInt(t *testing.T) {
	t.Parallel()

	digitCases := []struct {
		name   string
		input  string
		wantN  int
		wantOK bool
	}{
		{name: "empty string", input: "", wantN: 0, wantOK: false},
		{name: "trailing non-digit", input: "12a", wantN: 0, wantOK: false},
		{name: "plain digits", input: "42", wantN: 42, wantOK: true},
	}
	for _, tc := range digitCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, ok := parseDigits(tc.input)
			if n != tc.wantN || ok != tc.wantOK {
				t.Errorf("parseDigits(%q) = (%d, %v), want (%d, %v)", tc.input, n, ok, tc.wantN, tc.wantOK)
			}
		})
	}

	t.Run("malformed query value falls back to default", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/?limit=abc", nil)
		if got := parseQueryInt(req, "limit", 5); got != 5 {
			t.Errorf("parseQueryInt = %d, want 5", got)
		}
	})
	t.Run("absent query value falls back to default", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		if got := parseQueryInt(req, "limit", 7); got != 7 {
			t.Errorf("parseQueryInt = %d, want 7", got)
		}
	})
}
