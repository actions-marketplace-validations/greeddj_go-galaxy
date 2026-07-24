package s3

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// bucketListKey is the pseudo object key countRequest/requestCount use for a
// bucket-level ListObjectsV2 request, which - unlike GET/PUT/HEAD/DELETE -
// has no per-object key of its own. It can never collide with a real object
// key, since every real key under this fake's buckets is a non-empty
// slash-delimited path.
const bucketListKey = "list-objects-v2"

// bucketDeleteObjectsKey is bucketListKey's counterpart for a Multi-Object
// Delete (POST ?delete) request.
const bucketDeleteObjectsKey = "delete-objects"

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
	objects               map[string]fakeObject
	fails                 map[string]*forcedFailure
	requests              map[string]int
	failDeleteObjects     *deleteObjectsFailure
	lastDeleteHeaders     http.Header
	bucket                string
	lastDeleteReq         deleteRequest
	deleteDelay           time.Duration
	mu                    sync.Mutex
	ignoreIfNoneMatch     bool
	oversizedList         bool
	oversizedDeleteResult bool
}

// deleteObjectsFailure is a fault-injection rule for handleDeleteObjects: it
// reports key as a single <Error> entry in the DeleteResult (with the given
// code/message) instead of deleting it, while every other key in the same
// batch is still deleted normally - simulating a real per-key partial
// failure that DeleteObjects can report even under an overall 200 status.
type deleteObjectsFailure struct {
	key     string
	code    string
	message string
}

// forcedFailure is a fault-injection rule matched by object key and HTTP
// method: while remaining is nonzero, a matching request receives status
// (and, if set, body) instead of the fake's normal response, and remaining
// is decremented (unless already negative, meaning "fail indefinitely"). It
// exists to force error branches - GET/HEAD/PUT/DELETE failures, optionally
// carrying an S3-style XML error document - that a conforming in-memory fake
// would otherwise never produce on its own.
type forcedFailure struct {
	method    string // http.MethodHead/Get/Put/Delete, or "" to match any method
	body      []byte // optional response body written alongside status, e.g. an S3 <Error> document
	status    int
	remaining int
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

// failNext arms a forced-failure rule for key: the next count requests to
// key matching method (or every method, if method == "") receive status
// instead of the fake's normal response. count == -1 fails every matching
// request indefinitely, until failNext is called again for the same key.
func (f *fakeS3) failNext(key, method string, status, count int) {
	f.failNextWithBody(key, method, status, count, nil)
}

// failNextWithBody behaves like failNext but additionally arms a response
// body to be written alongside the forced status code - for example an S3
// <Error> XML document - so a test can assert that callers surface its Code
// and Message.
func (f *fakeS3) failNextWithBody(key, method string, status, count int, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails == nil {
		f.fails = make(map[string]*forcedFailure)
	}
	f.fails[key] = &forcedFailure{method: method, status: status, remaining: count, body: body}
}

// countRequest records that key received one request via method, counting
// every request that reaches an object handler regardless of whether a
// forced-failure rule matches it. It exists purely as an observability hook
// so a test can assert the exact number of attempts a retrying call made
// (via requestCount) rather than inferring it indirectly from a
// forcedFailure's remaining count.
func (f *fakeS3) countRequest(key, method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests == nil {
		f.requests = make(map[string]int)
	}
	f.requests[method+" "+key]++
}

// requestCount reports how many requests key has received via method so
// far, as recorded by countRequest.
func (f *fakeS3) requestCount(key, method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[method+" "+key]
}

// shouldFail reports whether the request for key+method matches an armed
// forced-failure rule, consuming one use of it (unless the rule fails
// indefinitely). It returns the status and body to write; body is nil when
// none was armed. It locks mu itself; callers must not already hold mu.
func (f *fakeS3) shouldFail(key, method string) (int, []byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rule, ok := f.fails[key]
	if !ok || rule.remaining == 0 || (rule.method != "" && rule.method != method) {
		return 0, nil, false
	}
	if rule.remaining > 0 {
		rule.remaining--
	}
	return rule.status, rule.body, true
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
	case http.MethodPost:
		if r.URL.Query().Has("delete") {
			f.handleDeleteObjects(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleList renders a minimal ListObjectsV2 XML response for keys under
// the requested prefix. Pagination is not simulated: every matching key is
// returned in one page (IsTruncated is always false). When oversizedList is
// armed, the normal listing is skipped entirely in favor of
// writeOversizedList.
func (f *fakeS3) handleList(w http.ResponseWriter, r *http.Request) {
	f.countRequest(bucketListKey, http.MethodGet)

	f.mu.Lock()
	oversized := f.oversizedList
	f.mu.Unlock()
	if oversized {
		f.writeOversizedList(w)
		return
	}

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

// writeOversizedList streams a ListObjectsV2-shaped body well past
// helpers.S3ListMaxSize using a single reused chunk, so the fake itself
// never allocates anywhere near that size; only the cumulative transferred
// byte count grows across repeated writes of the same buffer. It stops as
// soon as a write fails, which is expected once the client's size-limited
// reader aborts the read and closes the response body after crossing the
// cap.
func (f *fakeS3) writeOversizedList(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	chunk := bytes.Repeat([]byte("a"), 64<<10)
	var written int64
	for written <= helpers.S3ListMaxSize {
		n, err := w.Write(chunk)
		written += int64(n)
		if err != nil {
			break
		}
	}
}

// handleDeleteObjects answers a Multi-Object Delete (POST ?delete) request:
// a forced failure armed via failNext(bucketDeleteObjectsKey, ...) takes
// precedence over everything below, exactly like the object-level handlers,
// so a test can drive deleteObjectsBatch's own transient-failure retry path.
// Absent that, it parses the <Delete> body via encoding/xml - never by
// string matching, so an object key carrying XML metacharacters round-trips
// as a single literal <Object> entry exactly like a conforming S3 endpoint
// would rather than being reinterpreted as extra markup - deletes every
// listed key that is not the armed failDeleteObjects key, and renders a
// Quiet-mode <DeleteResult> reporting only that one key (if any) as an
// <Error>. When oversizedDeleteResult is armed, the normal response is
// skipped entirely in favor of writeOversizedDeleteResult. The parsed
// request and its headers are captured for
// deleteObjectsRequest/deleteObjectsHeaders so a test can assert what
// deleteObjectsBatch actually sent.
func (f *fakeS3) handleDeleteObjects(w http.ResponseWriter, r *http.Request) {
	f.countRequest(bucketDeleteObjectsKey, http.MethodPost)
	if status, _, fail := f.shouldFail(bucketDeleteObjectsKey, http.MethodPost); fail {
		w.WriteHeader(status)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var req deleteRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.lastDeleteReq = req
	f.lastDeleteHeaders = r.Header.Clone()
	oversized := f.oversizedDeleteResult
	fail := f.failDeleteObjects
	f.mu.Unlock()
	if oversized {
		f.writeOversizedDeleteResult(w)
		return
	}

	var result deleteResult
	f.mu.Lock()
	for _, obj := range req.Objects {
		if fail != nil && obj.Key == fail.key {
			result.Errors = append(result.Errors, deleteError{Key: fail.key, Code: fail.code, Message: fail.message})
			continue
		}
		delete(f.objects, obj.Key)
	}
	f.mu.Unlock()

	payload, err := xml.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append([]byte(xml.Header), payload...))
}

// writeOversizedDeleteResult streams a DeleteResult-shaped body well past
// helpers.S3ListMaxSize using a single reused chunk, mirroring
// writeOversizedList, so a test can drive deleteObjectsBatch's size-limited
// response read into rejecting an oversized body.
func (f *fakeS3) writeOversizedDeleteResult(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	chunk := bytes.Repeat([]byte("a"), 64<<10)
	var written int64
	for written <= helpers.S3ListMaxSize {
		n, err := w.Write(chunk)
		written += int64(n)
		if err != nil {
			break
		}
	}
}

// failDeleteObjectsFor arms handleDeleteObjects to report key as a single
// <Error> entry (with the given code/message) instead of deleting it.
func (f *fakeS3) failDeleteObjectsFor(key, code, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failDeleteObjects = &deleteObjectsFailure{key: key, code: code, message: message}
}

// deleteObjectsRequest returns the most recently received Multi-Object
// Delete request body, so a test can assert exactly which <Object> entries
// deleteObjectsBatch sent.
func (f *fakeS3) deleteObjectsRequest() deleteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastDeleteReq
}

// deleteObjectsHeaders returns the most recently received Multi-Object
// Delete request's headers, so a test can assert deleteObjectsBatch signs
// and content-hashes the batch (Content-MD5, X-Amz-Content-Sha256,
// Content-Type) without re-deriving SigV4 verification in the fake.
func (f *fakeS3) deleteObjectsHeaders() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastDeleteHeaders
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
// (token/deadline verification never needs to transfer the body). A forced
// failure armed via failNext takes precedence over the normal response.
func (f *fakeS3) handleHead(w http.ResponseWriter, key string) {
	f.countRequest(key, http.MethodHead)
	// The body (if any) is deliberately not written here: real HTTP HEAD
	// responses carry no entity body, and net/http's server elides one even
	// if a handler attempts to write it, so arming a body on a HEAD rule
	// would never reach the client anyway.
	if status, _, fail := f.shouldFail(key, http.MethodHead); fail {
		w.WriteHeader(status)
		return
	}
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
// or 404 if absent. A forced failure armed via failNext/failNextWithBody
// takes precedence over the normal response.
func (f *fakeS3) handleGet(w http.ResponseWriter, key string) {
	f.countRequest(key, http.MethodGet)
	if status, body, fail := f.shouldFail(key, http.MethodGet); fail {
		w.WriteHeader(status)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
		return
	}
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
// backend that always overwrites regardless of the header. A forced failure
// armed via failNext takes precedence over both of those behaviors (e.g. to
// simulate another writer's create winning a race, regardless of what this
// fake's own object map currently holds for key).
func (f *fakeS3) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	f.countRequest(key, http.MethodPut)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status, failBody, fail := f.shouldFail(key, http.MethodPut); fail {
		w.WriteHeader(status)
		if len(failBody) > 0 {
			_, _ = w.Write(failBody)
		}
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
// context and observe the resulting timeout. A forced failure armed via
// failNext takes precedence over both of those behaviors.
func (f *fakeS3) handleDelete(w http.ResponseWriter, r *http.Request, key string) {
	f.countRequest(key, http.MethodDelete)
	if status, body, fail := f.shouldFail(key, http.MethodDelete); fail {
		w.WriteHeader(status)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
		return
	}
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

// newTestBackendAndFake behaves like newTestBackend but also returns the
// *fakeS3 backing it, so a test can arm fault injection via failNext or
// failNextWithBody against the same server the returned Backend talks to.
func newTestBackendAndFake(t *testing.T) (*Backend, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	return newTestBackendWithFake(t, fake), fake
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
