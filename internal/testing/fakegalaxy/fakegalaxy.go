// Package fakegalaxy provides an in-memory Ansible Galaxy v3 API test
// double, used by unit and integration tests that need to exercise
// HTTP-facing code paths (root metadata, versions list, version detail,
// artifact download) without a real network dependency. It answers the
// same routes and JSON shapes the real Galaxy API does, generates
// deterministic tar.gz artifacts with a matching sha256, and supports
// scripted fault injection (status codes, indefinite failures, hangs) and
// per-endpoint request counting.
//
// The server can only be constructed from a test: New requires a
// testing.TB, an interface implementable exclusively by the stdlib testing
// package, so this package - and the fake server it builds - can never be
// reached from production code. Shutdown is registered via tb.Cleanup.
package fakegalaxy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psvmcc/hub/pkg/types"
)

// Endpoint identifies one of the routes the fake server answers, for fault
// injection (Fail) and request counting (Count).
type Endpoint int

// The four routes the fake server answers.
const (
	EndpointRootMetadata Endpoint = iota
	EndpointVersionsList
	EndpointVersionDetail
	EndpointArtifact
)

// endpointCount is the number of distinct Endpoint values, sizing Server's
// per-endpoint counters array.
const endpointCount = 4

// Path segment names used by ServeHTTP's routing and the URLs this package
// builds. Named rather than repeated string literals, since "api" alone
// appears in three different route predicates.
const (
	apiSegment         = "api"
	v3Segment          = "v3"
	collectionsSegment = "collections"
	versionsSegment    = "versions"
	downloadSegment    = "download"
)

// decimalBase is the radix parseDigits parses in.
const decimalBase = 10

// tarFileMode is the fixed permission bits stamped on every generated tar
// entry, keeping the artifact's bytes independent of the environment it
// was built in.
const tarFileMode = 0o644

// These are fixed points in time, computed once rather than per response;
// they must be var since time.Date and time.Time.Format are not constant
// expressions. Never time.Now, so every byte this package produces - and
// its sha256 - is identical on every run and every machine.
//
//nolint:gochecknoglobals // see above: fixed, not runtime-mutable, state.
var (
	// fixedModTime is used for every generated tar entry's ModTime and for
	// the fixed Last-Modified header on JSON responses.
	fixedModTime = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	// fixedLastModified is fixedModTime pre-formatted the way net/http
	// expects it on a Last-Modified header.
	fixedLastModified = fixedModTime.UTC().Format(http.TimeFormat)
)

// Server is an in-memory Ansible Galaxy v3 API test double. It must only be
// constructed via New.
type Server struct {
	srv         *httptest.Server
	collections map[string]*fakeCollection
	artifacts   map[string]fakeArtifact
	baseURL     string
	faults      []faultRule
	mu          sync.Mutex
	counts      [endpointCount]int
}

// fakeCollection is the registry entry for one namespace/name pair: every
// registered version, plus the premarshaled root metadata body, which is
// the only piece that changes shape as versions are added (it tracks
// whichever version currently sorts highest).
type fakeCollection struct {
	namespace      string
	name           string
	versions       map[string]*fakeVersionEntry
	sortedVersions []string
	rootBody       []byte
}

// fakeVersionEntry is one registered collection version: its absolute
// version-detail URL (also reused by the versions-list Data entries) and
// its premarshaled version-detail body, which never changes once built
// since it does not depend on sibling versions.
type fakeVersionEntry struct {
	version    string
	href       string
	sha256     string
	detailBody []byte
}

// fakeArtifact is one registered download: its tarball bytes plus the
// namespace/name that produced it. The download route only carries a flat
// filename, so this lets Fail rules addressing EndpointArtifact by
// namespace/name still match.
type fakeArtifact struct {
	namespace string
	name      string
	data      []byte
}

// faultRule is one armed Fail call. It fires for requests to ep whose
// namespace/name match - an empty namespace or name field acts as a
// wildcard - decrementing fault.Count on each match until it is exhausted
// (a non-positive initial Count never decrements, so it matches
// indefinitely).
type faultRule struct {
	namespace string
	name      string
	ep        Endpoint
	fault     Fault
}

// Version describes one collection version registered with the fake
// server: the identity a caller used to register it, plus the sha256 of
// the artifact the server generated for it, so tests never need to
// hardcode a checksum.
type Version struct {
	Namespace string
	Name      string
	Version   string
	SHA256    string
}

// Fault describes a scripted failure for one armed Fail rule. Status, if
// nonzero, makes a matching request receive that HTTP status with an empty
// body instead of the fake's normal response. Hang, instead, blocks a
// matching request until its context is done, then returns without writing
// anything (Status takes precedence if both are set). Count bounds how
// many matching requests are affected: a positive Count is decremented on
// each match until it reaches zero, at which point the rule stops matching;
// a negative Count matches indefinitely; a Count of zero (the zero value)
// never matches, so a Fault must set a positive or negative Count to fire.
//
// StallAfterBytes only applies to the artifact endpoint: on a request there,
// a matching Fault with StallAfterBytes > 0 writes exactly that many bytes of
// the real artifact, flushes them onto the wire, and then blocks until the
// request's context is done, writing nothing further - a mid-body stall,
// distinct from Hang's before-any-byte stall. On any other endpoint,
// StallAfterBytes is a no-op: the fault still consumes one Count, but
// enactFault then falls through to that endpoint's normal response. A
// well-formed Fault sets exactly one of Status, Hang, or StallAfterBytes; on
// the artifact endpoint, StallAfterBytes is checked before Status/Hang, so it
// takes precedence if more than one is set. StallAfterBytes composes with
// Count exactly like Status and Hang: Count 1 stalls only the next matching
// request, a negative Count stalls every one.
type Fault struct {
	Status          int
	Count           int
	StallAfterBytes int
	Hang            bool
}

// New starts an in-memory fake Galaxy v3 API server and registers its
// shutdown with tb.Cleanup. Taking testing.TB - rather than nothing, or a
// concrete *testing.T - is deliberate: it is the only interface
// implementable exclusively by the stdlib testing package, so this
// constructor, and therefore the server itself, can never be reached from
// production code.
func New(tb testing.TB) *Server {
	tb.Helper()

	s := &Server{
		collections: make(map[string]*fakeCollection),
		artifacts:   make(map[string]fakeArtifact),
	}
	// The server is started before baseURL is known so ServeHTTP can be
	// registered as its handler; no request can arrive before New returns
	// s to the caller, so assigning s.srv/s.baseURL afterward is safe.
	srv := httptest.NewServer(s)
	tb.Cleanup(srv.Close)
	s.srv = srv
	s.baseURL = srv.URL

	return s
}

// URL returns the fake server's base URL, e.g. "http://127.0.0.1:PORT".
// AddVersion uses it internally to build every absolute URL it returns or
// embeds in a response body.
func (s *Server) URL() string {
	return s.baseURL
}

// Client returns an *http.Client wired to talk to the fake server,
// following httptest.Server's own conventions for the underlying
// transport.
func (s *Server) Client() *http.Client {
	return s.srv.Client()
}

// AddVersion registers one version of a namespace/name collection, lazily
// creating the collection on first use, and returns the details a caller
// needs to assert against - in particular the generated artifact's sha256,
// which is always computed here rather than ever hardcoded by a caller.
// Registering a new version recomputes the collection's root metadata,
// since highest_version tracks whichever registered version currently
// sorts highest.
func (s *Server) AddVersion(namespace, name, version string, deps map[string]string) Version {
	data, sum := buildArtifact(namespace, name, version, deps)
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
	href := fmt.Sprintf("%s/api/v3/collections/%s/%s/versions/%s/", s.baseURL, namespace, name, version)
	downloadURL := fmt.Sprintf("%s/download/%s", s.baseURL, filename)
	detailBody := buildVersionDetailBody(namespace, name, version, href, downloadURL, filename, sum, len(data), deps)

	s.mu.Lock()
	defer s.mu.Unlock()

	key := collectionKey(namespace, name)
	col, ok := s.collections[key]
	if !ok {
		col = &fakeCollection{
			namespace: namespace,
			name:      name,
			versions:  make(map[string]*fakeVersionEntry),
		}
		s.collections[key] = col
	}
	col.versions[version] = &fakeVersionEntry{version: version, href: href, sha256: sum, detailBody: detailBody}
	col.sortedVersions = sortedVersionKeys(col.versions)
	recomputeRootBody(col, s.baseURL)

	s.artifacts[filename] = fakeArtifact{namespace: namespace, name: name, data: data}

	return Version{Namespace: namespace, Name: name, Version: version, SHA256: sum}
}

// Fail arms a fault rule: the next requests to ep matching namespace/name
// (each an exact match, or a wildcard when empty) are affected by f. Rules
// are scanned in the order Fail registered them, and each is independent -
// registering a second rule for the same endpoint does not replace the
// first.
func (s *Server) Fail(ep Endpoint, namespace, name string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, faultRule{namespace: namespace, name: name, ep: ep, fault: f})
}

// Count reports how many requests ep has received since the server started
// or since the last ResetCounts.
func (s *Server) Count(ep Endpoint) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[ep]
}

// Total reports how many requests every endpoint has received combined.
func (s *Server) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, c := range s.counts {
		total += c
	}
	return total
}

// ResetCounts zeroes every endpoint's request counter. Armed fault rules
// are unaffected; use a fresh Server to reset those too.
func (s *Server) ResetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = [endpointCount]int{}
}

// ServeHTTP hand-rolls routing for the four endpoints this fake answers,
// plus the two Galaxy v2/bare-API probe routes real clients use to detect
// server flavor - which must 404 rather than serve v3 metadata, since a
// client would otherwise mistake this fake for a different API version.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	switch {
	case isDownloadPath(segments):
		s.incr(EndpointArtifact)
		s.handleArtifact(w, r, segments[1])
	case isRootMetadataPath(segments):
		s.incr(EndpointRootMetadata)
		s.handleRootMetadata(w, r, segments[3], segments[4])
	case isVersionsListPath(segments):
		s.incr(EndpointVersionsList)
		s.handleVersionsList(w, r, segments[3], segments[4])
	case isVersionDetailPath(segments):
		s.incr(EndpointVersionDetail)
		s.handleVersionDetail(w, r, segments[3], segments[4], segments[6])
	default:
		// Covers the v2 ("api/v2/collections/ns/name") and bare-API
		// ("api/collections/ns/name") probe routes, and anything else: none
		// of them are served by this fake.
		http.NotFound(w, r)
	}
}

// isDownloadPath reports whether segments is "download/{file}".
func isDownloadPath(segments []string) bool {
	return len(segments) == 2 && segments[0] == downloadSegment
}

// isAPIv3CollectionPath reports whether segments starts with
// "api/v3/collections/{ns}/{name}", the common prefix of the three v3
// collection routes.
func isAPIv3CollectionPath(segments []string) bool {
	return len(segments) >= 5 &&
		segments[0] == apiSegment && segments[1] == v3Segment && segments[2] == collectionsSegment
}

// isRootMetadataPath reports whether segments is exactly
// "api/v3/collections/{ns}/{name}".
func isRootMetadataPath(segments []string) bool {
	return len(segments) == 5 && isAPIv3CollectionPath(segments)
}

// isVersionsListPath reports whether segments is
// "api/v3/collections/{ns}/{name}/versions".
func isVersionsListPath(segments []string) bool {
	return len(segments) == 6 && isAPIv3CollectionPath(segments) && segments[5] == versionsSegment
}

// isVersionDetailPath reports whether segments is
// "api/v3/collections/{ns}/{name}/versions/{version}".
func isVersionDetailPath(segments []string) bool {
	return len(segments) == 7 && isAPIv3CollectionPath(segments) && segments[5] == versionsSegment
}

// incr increments ep's request counter. It is called for every request
// this fake receives on a known route, before any fault handling, so
// Count/Total reflect requests regardless of whether a fault or a 404
// followed.
func (s *Server) incr(ep Endpoint) {
	s.mu.Lock()
	s.counts[ep]++
	s.mu.Unlock()
}

// handleRootMetadata answers "api/v3/collections/{ns}/{name}".
func (s *Server) handleRootMetadata(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if s.applyFault(w, r, EndpointRootMetadata, namespace, name) {
		return
	}
	s.mu.Lock()
	col, ok := s.collections[collectionKey(namespace, name)]
	var body []byte
	if ok {
		body = col.rootBody
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSONWithETag(w, r, body)
}

// handleVersionsList answers "api/v3/collections/{ns}/{name}/versions",
// honoring ?limit=&offset= pagination over the registered versions sorted
// ascending.
func (s *Server) handleVersionsList(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if s.applyFault(w, r, EndpointVersionsList, namespace, name) {
		return
	}
	s.mu.Lock()
	col, ok := s.collections[collectionKey(namespace, name)]
	var sortedVersions []string
	var entries map[string]*fakeVersionEntry
	if ok {
		sortedVersions = col.sortedVersions
		entries = col.versions
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	total := len(sortedVersions)
	limit := parseQueryInt(r, "limit", total)
	offset := parseQueryInt(r, "offset", 0)
	page := paginate(sortedVersions, offset, limit)

	var resp types.GalaxyCollectionVersions
	resp.Meta.Count = total
	resp.Data = make([]types.GalaxyCollectionVersion, len(page))
	for i, v := range page {
		entry := entries[v]
		resp.Data[i] = types.GalaxyCollectionVersion{Version: entry.version, Href: entry.href}
	}

	body, err := json.Marshal(&resp)
	if err != nil {
		// resp is built entirely from strings and an int; marshaling it
		// cannot fail. A non-nil error here would mean a structural bug in
		// this harness, not a caller mistake, so it is asserted away.
		panic(fmt.Sprintf("fakegalaxy: marshal versions list: %v", err))
	}
	writeJSONWithETag(w, r, body)
}

// handleVersionDetail answers
// "api/v3/collections/{ns}/{name}/versions/{version}".
func (s *Server) handleVersionDetail(w http.ResponseWriter, r *http.Request, namespace, name, version string) {
	if s.applyFault(w, r, EndpointVersionDetail, namespace, name) {
		return
	}
	s.mu.Lock()
	col, ok := s.collections[collectionKey(namespace, name)]
	var body []byte
	if ok {
		var entry *fakeVersionEntry
		entry, ok = col.versions[version]
		if ok {
			body = entry.detailBody
		}
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSONWithETag(w, r, body)
}

// handleArtifact answers "download/{file}" with the raw tarball bytes and
// no cache validators, unlike the JSON endpoints. A StallAfterBytes fault is
// handled here rather than in applyFault/enactFault, since it needs the
// artifact's own bytes (art.data) to serve a real prefix before blocking,
// which those two shared helpers do not have access to.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request, filename string) {
	s.mu.Lock()
	art, ok := s.artifacts[filename]
	s.mu.Unlock()

	// A fault targeting a namespace/name is matched against the artifact's
	// registered identity, since the download URL itself carries only a
	// flat filename, not namespace/name path segments.
	if fault, matched := s.consumeFault(EndpointArtifact, art.namespace, art.name); matched {
		if fault.StallAfterBytes > 0 && ok {
			serveArtifactStall(w, r, art.data, fault.StallAfterBytes)
			return
		}
		if enactFault(w, r, fault) {
			return
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(art.data)
}

// serveArtifactStall writes the first StallAfterBytes bytes of a real artifact,
// flushes them onto the wire (forcing chunked encoding so the client cannot
// infer completion from a Content-Length), then blocks until the request
// context is canceled. It relies only on the client aborting the request - no
// timer, no wall clock - so it stays deterministic under -race.
func serveArtifactStall(w http.ResponseWriter, r *http.Request, data []byte, after int) {
	n := min(after, len(data))
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data[:n])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()
}

// applyFault consumes the first armed fault rule matching ep/namespace/name,
// if any, and enacts it against w/r. It reports whether it did (in which
// case the caller must not write any further response).
func (s *Server) applyFault(w http.ResponseWriter, r *http.Request, ep Endpoint, namespace, name string) bool {
	fault, matched := s.consumeFault(ep, namespace, name)
	if !matched {
		return false
	}
	return enactFault(w, r, fault)
}

// consumeFault scans armed fault rules, in the order Fail registered them,
// for the first one matching ep/namespace/name whose Count has not been
// exhausted, and consumes one use of it (unless it fails indefinitely,
// Count < 0). It reports the matched Fault and true, or a zero Fault and
// false when nothing matches.
func (s *Server) consumeFault(ep Endpoint, namespace, name string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.faults {
		rule := &s.faults[i]
		if !ruleMatches(rule, ep, namespace, name) {
			continue
		}
		if rule.fault.Count > 0 {
			rule.fault.Count--
		}
		return rule.fault, true
	}
	return Fault{}, false
}

// ruleMatches reports whether rule fires for a request to ep/namespace/name:
// its endpoint must match, its Count must be nonzero (a zero Count is the
// disabled zero value and never fires; a positive Count fires until it is
// decremented to zero, a negative Count fires indefinitely), and its
// namespace/name must either be a wildcard (empty) or match exactly.
func ruleMatches(rule *faultRule, ep Endpoint, namespace, name string) bool {
	if rule.ep != ep || rule.fault.Count == 0 {
		return false
	}
	if rule.namespace != "" && rule.namespace != namespace {
		return false
	}
	return rule.name == "" || rule.name == name
}

// enactFault carries out a matched Fault against w/r: a nonzero Status
// writes that status with an empty body; otherwise Hang blocks until r's
// context is done and writes nothing. It reports whether it wrote (or
// waited for) a response; a Fault with neither set is a no-op that returns
// false, letting the caller fall through to its normal response.
func enactFault(w http.ResponseWriter, r *http.Request, fault Fault) bool {
	if fault.Status != 0 {
		w.WriteHeader(fault.Status)
		return true
	}
	if fault.Hang {
		<-r.Context().Done()
		return true
	}
	return false
}

// writeJSONWithETag serves body as a JSON response, honoring conditional
// GET via If-None-Match against an ETag computed over body's exact bytes
// (If-Modified-Since is not honored). The artifact endpoint never calls
// this: binary downloads carry no validators in this fake.
func writeJSONWithETag(w http.ResponseWriter, r *http.Request, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:])[:16] + `"`

	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("ETag", etag)
	header.Set("Last-Modified", fixedLastModified)

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// collectionKey builds the registry key for a namespace/name pair. Test
// collection names never contain a slash, so this cannot collide.
func collectionKey(namespace, name string) string {
	return namespace + "/" + name
}

// parseQueryInt reads the named query parameter as a non-negative integer,
// returning def when it is absent or malformed.
func parseQueryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, ok := parseDigits(raw)
	if !ok {
		return def
	}
	return n
}

// paginate returns the slice of versions starting at offset and containing
// up to limit elements, clamped to versions' bounds. A negative offset is
// treated as zero; an offset beyond the end, or a non-positive limit after
// clamping, yields an empty (non-nil) slice.
func paginate(versions []string, offset, limit int) []string {
	if offset < 0 {
		offset = 0
	}
	if offset > len(versions) {
		offset = len(versions)
	}
	end := offset + limit
	if limit < 0 || end > len(versions) {
		end = len(versions)
	}
	if end < offset {
		end = offset
	}
	return versions[offset:end]
}

// sortedVersionKeys returns versions' keys sorted ascending by
// compareDottedVersions. The slice is pre-sized to len(versions) since its
// final length is already known.
func sortedVersionKeys(versions map[string]*fakeVersionEntry) []string {
	keys := make([]string, 0, len(versions))
	for v := range versions {
		keys = append(keys, v)
	}
	sort.Slice(keys, func(i, j int) bool {
		return compareDottedVersions(keys[i], keys[j]) < 0
	})
	return keys
}

// compareDottedVersions compares two dot-separated version strings
// component by component, treating each component as a base-10 integer
// when it parses as one and falling back to a lexical comparison
// otherwise. It returns a negative number, zero, or a positive number as a
// sorts before, equal to, or after b - enough to order the plain semver
// strings this harness registers, though it is not a full semver
// implementation (no pre-release or build-metadata handling).
func compareDottedVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if c := compareDottedComponent(aParts[i], bParts[i]); c != 0 {
			return c
		}
	}
	return len(aParts) - len(bParts)
}

// compareDottedComponent compares one dot-separated component of two
// versions, numerically if both sides parse as plain digit strings, or
// lexically otherwise.
func compareDottedComponent(a, b string) int {
	an, aOK := parseDigits(a)
	bn, bOK := parseDigits(b)
	if aOK && bOK {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

// parseDigits parses s as a base-10 non-negative integer, succeeding only
// if every rune is an ASCII digit. Unlike fmt.Sscanf's "%d", it never
// accepts a numeric prefix of a longer string (e.g. "12abc"): it reports
// false for anything that is not purely digits.
func parseDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*decimalBase + int(r-'0')
	}
	return n, true
}

// recomputeRootBody rebuilds col's premarshaled root metadata body after a
// version has been added, since highest_version tracks whichever version
// now sorts highest. It must be called with s.mu held, and requires
// col.sortedVersions to already reflect the newly added version.
func recomputeRootBody(col *fakeCollection, baseURL string) {
	highest := col.sortedVersions[len(col.sortedVersions)-1]

	var root types.GalaxyCollection
	root.Href = fmt.Sprintf("%s/api/v3/collections/%s/%s/", baseURL, col.namespace, col.name)
	root.Namespace = col.namespace
	root.Name = col.name
	root.VersionsURL = fmt.Sprintf("%s/api/v3/collections/%s/%s/versions/", baseURL, col.namespace, col.name)
	root.HighestVersion.Href = col.versions[highest].href
	root.HighestVersion.Version = highest

	body, err := json.Marshal(&root)
	if err != nil {
		// root is built entirely from strings; marshaling it cannot fail.
		panic(fmt.Sprintf("fakegalaxy: marshal root metadata: %v", err))
	}
	col.rootBody = body
}

// buildVersionDetailBody premarshals the version-detail JSON body once at
// registration time: unlike the root metadata's highest_version, one
// version's detail never depends on its siblings, so it is built exactly
// once and served verbatim afterward. Dependencies are set on both
// metadata.dependencies - the field the loader's extractDependencies reads
// first - and manifest.collection_info.dependencies, mirroring the
// MANIFEST.json bundled in the artifact itself.
func buildVersionDetailBody(
	namespace, name, version, href, downloadURL, filename, sha string,
	size int,
	deps map[string]string,
) []byte {
	var info types.GalaxyCollectionVersionInfo
	info.Version = version
	info.Href = href
	info.Name = name
	info.Namespace.Name = namespace
	info.DownloadURL = downloadURL
	info.Artifact.Filename = filename
	info.Artifact.Sha256 = sha
	info.Artifact.Size = int64(size)
	info.Metadata.Dependencies = deps
	info.Manifest.CollectionInfo.Namespace = namespace
	info.Manifest.CollectionInfo.Name = name
	info.Manifest.CollectionInfo.Version = version
	info.Manifest.CollectionInfo.Dependencies = deps

	body, err := json.Marshal(&info)
	if err != nil {
		// info is built entirely from strings, an int64, and a
		// map[string]string; marshaling it cannot fail.
		panic(fmt.Sprintf("fakegalaxy: marshal version detail: %v", err))
	}
	return body
}

// buildArtifact produces a deterministic tar.gz artifact for one
// collection version: a MANIFEST.json describing the collection - the
// same shape a real ansible-galaxy build produces, and the shape
// archive.ExtractTarGzStream expects - plus one small regular file, so a
// real extractor can unpack it like any other Galaxy artifact. It returns
// the gzip bytes and their sha256 (hex), which callers use as the
// artifact's content hash rather than ever hardcoding one.
func buildArtifact(namespace, name, version string, deps map[string]string) ([]byte, string) {
	manifest := buildManifestJSON(namespace, name, version, deps)
	readme := []byte("# " + namespace + "." + name + "\n")

	// strings.Builder is used as the in-memory sink for gzip/tar output:
	// its Write accepts arbitrary bytes (Go strings are not required to be
	// valid UTF-8), so it needs no extra buffer type for this one-shot,
	// non-hot-path build.
	var sink strings.Builder
	gz := gzip.NewWriter(&sink)
	tw := tar.NewWriter(gz)

	writeTarFile(tw, "MANIFEST.json", manifest)
	writeTarFile(tw, "README.md", readme)

	// Both writers target an in-memory strings.Builder sink that never
	// fails, and every header's Size matches the bytes written to it;
	// Close can only error on a short write or a flush failure, neither of
	// which this deterministic pairing can produce.
	_ = tw.Close()
	_ = gz.Close()

	built := []byte(sink.String())
	sum := sha256.Sum256(built)
	return built, hex.EncodeToString(sum[:])
}

// writeTarFile appends one regular file entry to tw with a fixed mode,
// owner, and mod time, so the resulting tar.gz - and therefore its sha256
// - never depends on the environment it was built in.
func writeTarFile(tw *tar.Writer, name string, content []byte) {
	header := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(content)),
		Mode:     tarFileMode,
		ModTime:  fixedModTime,
	}
	// See buildArtifact: this in-memory pairing cannot fail.
	_ = tw.WriteHeader(header)
	_, _ = tw.Write(content)
}

// buildManifestJSON renders the MANIFEST.json bundled inside a generated
// artifact, reusing the vendored manifest type so its wire shape matches
// exactly what archive/collections code parses from a real one.
func buildManifestJSON(namespace, name, version string, deps map[string]string) []byte {
	var manifest types.GalaxyCollectionVersionInfoManifest
	manifest.Format = 1
	manifest.CollectionInfo.Namespace = namespace
	manifest.CollectionInfo.Name = name
	manifest.CollectionInfo.Version = version
	manifest.CollectionInfo.Dependencies = deps

	body, err := json.Marshal(&manifest)
	if err != nil {
		// manifest is built entirely from strings, an int, and a
		// map[string]string; marshaling it cannot fail.
		panic(fmt.Sprintf("fakegalaxy: marshal MANIFEST.json: %v", err))
	}
	return body
}
