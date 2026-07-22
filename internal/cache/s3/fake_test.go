package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// fakeObject is one stored object: its body, the X-Amz-Meta-* headers it was
// last written with (captured verbatim, keyed by canonical header name), and
// the time it was last written, which stands in for S3's Last-Modified.
type fakeObject struct {
	modified time.Time
	meta     map[string]string
	data     []byte
}

// fakeS3 is a minimal in-memory, path-style S3 fake used to exercise Backend
// and Client against real HTTP round trips without a network dependency or
// SigV4 verification (the client always signs requests, but this fake never
// checks the Authorization header).
type fakeS3 struct {
	objects map[string]fakeObject
	bucket  string
	mu      sync.Mutex
	// deleteDelay, when non-zero, makes handleDelete wait that long (or
	// until the request's context is done, whichever comes first) before
	// acting. It lets a test simulate a slow/hanging DELETE so a caller's
	// bounded context can be observed timing it out. Zero (the default)
	// preserves the original immediate-delete behavior for every other
	// test. It must be set before the fake starts handling requests that
	// exercise it: this test double does not synchronize concurrent
	// writes to it against concurrent reads from handler goroutines.
	deleteDelay time.Duration
	// ignoreIfNoneMatch, when true, makes handlePut always overwrite and
	// report success regardless of the If-None-Match header, simulating a
	// non-conforming backend that does not enforce conditional PUT. False
	// (the default) preserves the original 412-on-existing behavior every
	// other test relies on. Like deleteDelay, it must be set before the
	// fake starts handling requests that exercise it.
	ignoreIfNoneMatch bool
}

// newFakeS3 constructs an empty fake bucket store named "test". One
// instance must be created per test so state never leaks between parallel
// tests.
func newFakeS3() *fakeS3 {
	return &fakeS3{
		objects: make(map[string]fakeObject),
		bucket:  "test",
	}
}

// ServeHTTP routes requests to the bucket root or an object path.
func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	root := "/" + f.bucket
	switch {
	case r.URL.Path == root:
		f.handleBucket(w, r)
	case strings.HasPrefix(r.URL.Path, root+"/"):
		key := strings.TrimPrefix(r.URL.Path, root+"/")
		f.handleObject(w, r, key)
	default:
		http.NotFound(w, r)
	}
}

// handleBucket answers bucket-level requests: existence probes (HEAD) and
// listing (GET with list-type=2).
func (f *fakeS3) handleBucket(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodHead:
		// The bucket always exists in this fake, so ensureBucket's HEAD
		// probe succeeds and it never attempts to create the bucket.
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			f.handleList(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleList renders a minimal ListObjectsV2 XML response for keys under
// the requested prefix. Pagination is not simulated: every matching key is
// returned in one page (IsTruncated is always false).
func (f *fakeS3) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")

	f.mu.Lock()
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	f.mu.Unlock()
	sort.Strings(keys)

	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	body.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, key := range keys {
		body.WriteString("<Contents><Key>")
		body.WriteString(key)
		body.WriteString("</Key></Contents>")
	}
	body.WriteString("<IsTruncated>false</IsTruncated>")
	body.WriteString("</ListBucketResult>")

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body.String())
}

// handleObject answers object-level requests: HEAD, GET, PUT, and DELETE.
func (f *fakeS3) handleObject(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodHead:
		f.handleHead(w, key)
	case http.MethodGet:
		f.handleGet(w, key)
	case http.MethodPut:
		f.handlePut(w, r, key)
	case http.MethodDelete:
		f.handleDelete(w, r, key)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleHead reports the stored object's metadata headers and Last-Modified
// verbatim, or 404 if absent. This backs the lock protocol's HEAD-only reads
// (token/deadline verification never needs to transfer the body).
func (f *fakeS3) handleHead(w http.ResponseWriter, key string) {
	f.mu.Lock()
	obj, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeObjectHeaders(w.Header(), obj)
	w.WriteHeader(http.StatusOK)
}

// handleGet returns the stored object's metadata headers and body verbatim,
// or 404 if absent.
func (f *fakeS3) handleGet(w http.ResponseWriter, key string) {
	f.mu.Lock()
	obj, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeObjectHeaders(w.Header(), obj)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.data)
}

// handlePut stores the request body and its X-Amz-Meta-* headers under key,
// honoring the conditional If-None-Match: * header used by the distributed
// lock's "create if absent" semantics: it fails with 412 when the key
// already exists - unless ignoreIfNoneMatch simulates a non-conforming
// backend that always overwrites regardless of the header.
func (f *fakeS3) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ignoreIfNoneMatch && r.Header.Get("If-None-Match") == "*" {
		if _, exists := f.objects[key]; exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}
	f.objects[key] = fakeObject{
		data:     body,
		meta:     captureMeta(r.Header),
		modified: time.Now(),
	}
	w.WriteHeader(http.StatusOK)
}

// captureMeta extracts the X-Amz-Meta-* headers from an incoming request,
// keyed by their canonical header name, so handleHead/handleGet can echo
// them back verbatim on later reads.
func captureMeta(header http.Header) map[string]string {
	meta := make(map[string]string, len(header))
	for name, values := range header {
		if len(values) == 0 || !strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
			continue
		}
		meta[name] = values[0]
	}
	return meta
}

// writeObjectHeaders sets obj's captured metadata headers plus a
// Last-Modified timestamp on the response, mirroring what a real S3 HEAD or
// GET response carries.
func writeObjectHeaders(header http.Header, obj fakeObject) {
	for name, value := range obj.meta {
		header.Set(name, value)
	}
	header.Set("Last-Modified", obj.modified.UTC().Format(http.TimeFormat))
}

// handleDelete removes the object, always reporting success like S3 does
// even when the key is already absent. If deleteDelay is set, it first
// waits that long - or until the request's context is done, whichever
// comes first - so a test can make DELETE outlast a caller's bounded
// context and observe the resulting timeout.
func (f *fakeS3) handleDelete(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	delay := f.deleteDelay
	f.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			http.Error(w, r.Context().Err().Error(), http.StatusGatewayTimeout)
			return
		case <-timer.C:
		}
	}

	f.mu.Lock()
	delete(f.objects, key)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// newTestBackend starts a fake S3 server and returns a Backend configured
// to talk to it in path-style mode. SigV4 signatures are computed by the
// real client but never verified by the fake.
func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	return newTestBackendWithFake(t, newFakeS3())
}

// newNonConformingTestBackend starts a fake S3 server with
// ignoreIfNoneMatch set, simulating a backend that does not enforce
// conditional PUT, and returns a Backend pointed at it.
func newNonConformingTestBackend(t *testing.T) *Backend {
	t.Helper()
	fake := newFakeS3()
	fake.ignoreIfNoneMatch = true
	return newTestBackendWithFake(t, fake)
}

// newTestBackendWithFake starts fake as an httptest server and returns a
// Backend configured to talk to it in path-style mode. SigV4 signatures are
// computed by the real client but never verified by the fake.
func newTestBackendWithFake(t *testing.T, fake *fakeS3) *Backend {
	t.Helper()

	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := config.S3CacheConfig{
		Endpoint:  srv.URL,
		Bucket:    fake.bucket,
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: "y",
		PathStyle: true,
		Enabled:   true,
	}
	backend, err := New(cfg, srv.Client(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return backend
}
