package collections

// This file proves buildLockfile's own guard against a non-canonical
// meta.Artifact.Sha256: lock is the command that manufactures a pin, so
// rejecting a non-canonical value here means a poisoned or lying server can
// never get its bad digest committed to a lockfile in the first place.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// testWidgetsFQDN is the fqdn key buildLockfile's resolved map uses for the
// acme.widgets fixture every test in this file shares.
const testWidgetsFQDN = "acme.widgets"

// sha256RewritingTransport wraps a real http.RoundTripper (fakegalaxy's own)
// and rewrites the "artifact.sha256" field of every version-detail response
// (matched by URL path, since fakegalaxy's version-detail route is the only
// one shaped ".../versions/<version>/") to replacement, before the response
// body ever reaches loadCollectionMetadata. This is the only way to drive a
// non-canonical meta.Artifact.Sha256 through buildLockfile without teaching
// the shared fakegalaxy test double a body-tampering hook it has no other
// use for.
type sha256RewritingTransport struct {
	base        http.RoundTripper
	pathMarker  string
	replacement string
}

func (t *sha256RewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || !strings.Contains(req.URL.Path, t.pathMarker) {
		return resp, err
	}
	var body map[string]any
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		return nil, decErr
	}
	_ = resp.Body.Close()
	if artifact, ok := body["artifact"].(map[string]any); ok {
		artifact["sha256"] = t.replacement
	}
	rewritten, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return nil, marshalErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return resp, nil
}

// newBuildLockfileFixture registers acme.widgets@1.0.0 on a fresh fakegalaxy
// server and returns a collectionDeps whose HTTP client rewrites that
// version's served artifact.sha256 to replacement, plus the resolved/graph
// maps buildLockfile needs.
func newBuildLockfileFixture(t *testing.T, replacement string) (collectionDeps, map[string]collection, map[string][]string) {
	t.Helper()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	// fakegalaxy's own Client() carries a nil Transport for a plain (non-TLS)
	// server - httptest.Server falls back to http.DefaultTransport
	// internally, so this wrapper does the same explicitly, since it needs a
	// concrete, non-nil RoundTripper to delegate to.
	client := &http.Client{
		Transport: &sha256RewritingTransport{
			base:        http.DefaultTransport,
			pathMarker:  "/versions/" + testVersion100 + "/",
			replacement: replacement,
		},
	}

	cfg := &config.Config{Server: srv.URL(), Workers: 1}
	runtime := infra.New(noopPrinter{}, client)
	st := store.New()
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	resolved := map[string]collection{testWidgetsFQDN: col}
	graph := map[string][]string{col.key(): {}}
	return newCollectionDeps(cfg, runtime, st), resolved, graph
}

// TestBuildLockfileRejectsNonCanonicalDigest proves a non-canonical,
// non-empty meta.Artifact.Sha256 (here, uppercase hex - a shape a real
// server has actually served, per the S3 x-amz-meta-sha256 finding) fails
// buildLockfile with helpers.ErrMalformedArtifactSHA256 and produces no
// lockfile at all, rather than silently committing the bad digest.
func TestBuildLockfileRejectsNonCanonicalDigest(t *testing.T) {
	t.Parallel()
	const nonCanonical = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	deps, resolved, graph := newBuildLockfileFixture(t, nonCanonical)

	lf, err := buildLockfile(context.Background(), deps, resolved, graph)
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Fatalf("buildLockfile error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	if lf != nil {
		t.Fatalf("buildLockfile lockfile = %+v, want nil: no lockfile must be written on rejection", lf)
	}
}

// TestBuildLockfileAcceptsEmptyDigest proves an empty meta.Artifact.Sha256 -
// a server that simply does not publish digests - still produces a lockfile
// entry with an empty pin and no error: verifyPinnedSHA treats an empty pin
// as no pin at all, and that contract must survive this guard.
func TestBuildLockfileAcceptsEmptyDigest(t *testing.T) {
	t.Parallel()
	deps, resolved, graph := newBuildLockfileFixture(t, "")

	lf, err := buildLockfile(context.Background(), deps, resolved, graph)
	if err != nil {
		t.Fatalf("buildLockfile error = %v, want nil for an empty (unpublished) digest", err)
	}
	if len(lf.Collections) != 1 {
		t.Fatalf("unexpected lockfile collections: %+v, want exactly 1 entry", lf.Collections)
	}
	entry := lf.Collections[0]
	if entry.Name != testWidgetsFQDN || entry.Version != testVersion100 {
		t.Fatalf("unexpected lockfile entry: %+v, want %s@%s", entry, testWidgetsFQDN, testVersion100)
	}
	if entry.SHA256 != "" {
		t.Fatalf("lockfile entry SHA256 = %q, want empty", entry.SHA256)
	}
}
