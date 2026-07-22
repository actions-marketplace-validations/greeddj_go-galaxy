package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// fakeS3 is a minimal in-memory, path-style S3 fake used to exercise Backend
// and Client against real HTTP round trips without a network dependency or
// SigV4 verification (the client always signs requests, but this fake never
// checks the Authorization header).
type fakeS3 struct {
	objects map[string][]byte
	bucket  string
	mu      sync.Mutex
}

// newFakeS3 constructs an empty fake bucket store. One instance must be
// created per test so state never leaks between parallel tests.
func newFakeS3(bucket string) *fakeS3 {
	return &fakeS3{
		objects: make(map[string][]byte),
		bucket:  bucket,
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

// handleObject answers object-level requests: GET, PUT, and DELETE.
func (f *fakeS3) handleObject(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodGet:
		f.handleGet(w, key)
	case http.MethodPut:
		f.handlePut(w, r, key)
	case http.MethodDelete:
		f.handleDelete(w, key)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleGet returns the stored object bytes verbatim, or 404 if absent.
func (f *fakeS3) handleGet(w http.ResponseWriter, key string) {
	f.mu.Lock()
	body, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handlePut stores the request body under key, honoring the conditional
// If-None-Match: * header used by the distributed lock's "create if absent"
// semantics: it fails with 412 when the key already exists.
func (f *fakeS3) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("If-None-Match") == "*" {
		if _, exists := f.objects[key]; exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}
	f.objects[key] = body
	w.WriteHeader(http.StatusOK)
}

// handleDelete removes the object, always reporting success like S3 does
// even when the key is already absent.
func (f *fakeS3) handleDelete(w http.ResponseWriter, key string) {
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

	fake := newFakeS3("test")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := config.S3CacheConfig{
		Endpoint:  srv.URL,
		Bucket:    "test",
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
