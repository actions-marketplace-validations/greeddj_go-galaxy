// Package fakegit provides an in-process git remote test double: the smart
// HTTP upload-pack service (GET info/refs and POST git-upload-pack, protocol
// v0) over httptest, the same exchange over an ssh listener answering the
// "git-upload-pack '<path>'" exec a git client issues, an ssh agent on a unix
// socket, a known_hosts writer for the listener's host key, and an in-memory
// repository builder whose every object is stamped with one fixed time so a
// fixture's hashes are identical on every run. It exists so that nothing in
// this module's tests reaches a real git host, a real ssh-agent or the
// user's own key material.
//
// The double owns the server side of the exchange and nothing more: it does
// not validate tree entry names, symlink targets or collection metadata, so
// a repository built with RawTreeCommit or Symlink carries exactly the
// hostile shape a test put there and the code under test is what has to
// refuse it. Per repository, Capabilities decides whether shallow fetches and
// wants by reachable sha are advertised and honored - the zero value is the
// minimal server - and Fail scripts faults (a status, a hang before any byte,
// a stall after a prefix of the pack, a pack built from a different commit
// than the one wanted, a redirect of the advertisement) with the same Count
// grammar fakegalaxy uses. Requests are counted per Endpoint and the
// Authorization header (or, over ssh, the fingerprint of the key that
// authenticated) of each endpoint's latest request is captured, before any
// fault or auth check can answer it.
//
// Like fakegalaxy, the server and the repository builder can only be
// constructed from a test: New, NewRepo, StartAgent and GenerateKey take
// testing.TB, an interface implementable exclusively by the stdlib testing
// package, so nothing here can be reached from production code. Shutdown is
// registered via tb.Cleanup. A test that arms Hang or StallAfterBytes must
// abort the request it provokes - by a context deadline or by closing the
// client - since a blocked handler holds the server open until then.
package fakegit

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// Endpoint identifies one of the exchanges the fake answers, for fault
// injection (Fail), request counting (Count) and auth capture (SeenAuth).
type Endpoint int

// The three endpoints: the two halves of the smart-HTTP exchange and the
// single ssh exec that carries both halves over one channel.
const (
	EndpointInfoRefs Endpoint = iota
	EndpointUploadPack
	EndpointSSHExec
)

// endpointCount is the number of distinct Endpoint values, sizing Server's
// per-endpoint arrays.
const endpointCount = 3

// Path pieces of the smart-HTTP routes: a repository lives at
// "/<name>.git", its advertisement at "/<name>.git/info/refs" (with
// ?service=git-upload-pack) and its upload-pack at
// "/<name>.git/git-upload-pack".
const (
	repoSuffix     = ".git"
	infoRefsPath   = "/info/refs"
	uploadPackPath = "/" + uploadPackService
	serviceQuery   = "service"
)

// Content types the smart-HTTP protocol names for the two responses. go-git
// does not check either, but a real git client does.
const (
	advertisementContentType = "application/x-git-upload-pack-advertisement"
	resultContentType        = "application/x-git-upload-pack-result"
)

// Capabilities are the per-repository toggles of what the server advertises
// and honors. The zero value is the minimal server: agent, ofs-delta,
// no-progress and a symref for HEAD are always advertised, nothing else.
// Shallow adds the shallow capability and honors a deepen of exactly 1 for
// a single want; without it a deepen is answered with 400, so a client that
// asks for a depth the server never offered fails rather than being served
// a full pack. AllowReachableSHA1 adds allow-reachable-sha1-in-want and
// accepts a want that is reachable from an advertised tip; without it a want
// that is not a tip is refused with an ERR line.
type Capabilities struct {
	Shallow            bool
	AllowReachableSHA1 bool
}

// Fault describes a scripted failure for one armed Fail rule. Count bounds
// how many matching requests it affects, exactly as in fakegalaxy: a
// positive Count is decremented on each match until it reaches zero, a
// negative Count matches indefinitely, and a zero Count (the zero value)
// never matches, so a Fault must set a positive or negative Count to fire.
//
// Status, if nonzero, answers a matching HTTP request with that status and
// an empty body. Redirect, if non-empty, answers a matching info/refs request
// with 302 and that absolute URL as Location, the shape a host uses to move
// a repository - and the shape under which a client must not forward its
// Authorization header to the new host. Hang blocks a matching request until
// its context is done (over ssh, until the connection or the server closes)
// and writes nothing. StallAfterBytes, on upload-pack only, writes the
// shallow update and NAK, then exactly that many bytes of the packfile,
// flushes them onto the wire and blocks like Hang - a mid-pack stall, distinct
// from Hang's before-any-byte stall. ServeCommit, on upload-pack only, serves
// a pack built from that commit instead of the wanted one, so the client
// receives a well-formed pack that does not contain what it asked for.
//
// Precedence when several are set on one Fault: Redirect, then Status, then
// Hang, then StallAfterBytes; ServeCommit composes with the others only in
// that it changes what a served pack holds. Over ssh only Hang, StallAfterBytes
// and ServeCommit apply; there is no status line or Location to carry the
// other two, so a Fault arming only those is a no-op there beyond consuming
// one Count.
type Fault struct {
	Redirect        string
	Status          int
	Count           int
	StallAfterBytes int
	ServeCommit     plumbing.Hash
	Hang            bool
}

// Server is the in-process git remote. It must only be constructed via New.
type Server struct {
	srv            *httptest.Server
	repos          map[string]*Repo
	caps           map[string]Capabilities
	ssh            *SSHServer
	tb             testing.TB
	baseURL        string
	requiredAuth   string
	faults         []faultRule
	authSeen       [endpointCount]capturedAuth
	mu             sync.Mutex
	counts         [endpointCount]int
	authFailStatus int
}

// capturedAuth is the Authorization header state one endpoint's most recent
// request carried: present distinguishes a request that carried no header
// at all from one that carried an empty value.
type capturedAuth struct {
	value   string
	present bool
}

// faultRule is one armed Fail call: it fires for requests to ep whose
// repository name matches (an empty name is a wildcard), consuming one Count
// per match.
type faultRule struct {
	name  string
	fault Fault
	ep    Endpoint
}

// New starts the HTTP half of the remote and registers its shutdown with
// tb.Cleanup; the ssh half starts lazily on the first SSH call. See the
// package comment for why the constructor takes testing.TB.
func New(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{
		tb:    tb,
		repos: make(map[string]*Repo),
		caps:  make(map[string]Capabilities),
	}
	// The server is started before baseURL is known so ServeHTTP can be its
	// handler; no request can arrive before New returns s to the caller.
	srv := httptest.NewServer(s)
	tb.Cleanup(s.Close)
	s.srv = srv
	s.baseURL = srv.URL
	return s
}

// URL returns the HTTP base URL, e.g. "http://127.0.0.1:PORT".
func (s *Server) URL() string {
	return s.baseURL
}

// RepoURL returns the HTTP clone URL of the repository registered as name:
// URL()+"/"+name+".git".
func (s *Server) RepoURL(name string) string {
	return s.baseURL + "/" + name + repoSuffix
}

// Add registers r at "/<name>.git" on both transports, replacing any
// repository of the same name. Capabilities stay whatever SetCapabilities
// last set for name, the zero value otherwise.
func (s *Server) Add(name string, r *Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[name] = r
}

// SetCapabilities sets what the repository registered as name advertises
// and honors. It may be called before or after Add.
func (s *Server) SetCapabilities(name string, c Capabilities) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps[name] = c
}

// Fail arms a fault rule: the next requests to ep for the repository name
// (an exact match, or every repository when name is empty) are affected by
// f. Rules are scanned in the order Fail registered them and each is
// independent.
func (s *Server) Fail(ep Endpoint, name string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, faultRule{name: name, ep: ep, fault: f})
}

// RequireAuth demands an exact Authorization header value on every HTTP
// request, e.g. "Basic " followed by the base64 of "user:pass". The empty
// string, the default, means anonymous. A request whose header is missing or
// differs is answered with http.StatusUnauthorized (or what AuthFailStatus
// set) and an empty body - after its header was captured for SeenAuth and
// after any armed fault had its chance, so a redirect or a hang is observed
// even on an unauthenticated request. The ssh half authenticates by key and
// ignores this setting.
func (s *Server) RequireAuth(expected string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requiredAuth = expected
}

// AuthFailStatus overrides the status an HTTP auth failure answers with,
// e.g. http.StatusForbidden in place of the default http.StatusUnauthorized.
func (s *Server) AuthFailStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authFailStatus = status
}

// SeenAuth reports what ep's most recent request authenticated with and
// whether it authenticated at all: over HTTP the Authorization header value
// and its presence, over ssh the SHA256 fingerprint of the public key the
// session was accepted with. It reports ("", false) for an endpoint that has
// not received any request.
func (s *Server) SeenAuth(ep Endpoint) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.authSeen[ep]
	return seen.value, seen.present
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
// and captured auth are unaffected.
func (s *Server) ResetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = [endpointCount]int{}
}

// Close stops both transports. It is registered with tb.Cleanup by New and
// is safe to call again.
func (s *Server) Close() {
	s.mu.Lock()
	sshSrv := s.ssh
	s.mu.Unlock()
	if sshSrv != nil {
		sshSrv.close()
	}
	s.srv.Close()
}

// ServeHTTP routes the two smart-HTTP endpoints. The path must be
// "/<name>.git/info/refs" with ?service=git-upload-pack, or
// "/<name>.git/git-upload-pack"; anything else, including a receive-pack
// service, is 404. Each matched route is counted first, then its
// Authorization header is captured, then armed faults run, then the auth
// check, then the exchange itself - so Count and SeenAuth reflect every
// request whatever answered it, and a fault is observable on a request that
// would have failed auth.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, infoRefsPath):
		name, ok := repoName(strings.TrimSuffix(r.URL.Path, infoRefsPath))
		if !ok || r.URL.Query().Get(serviceQuery) != uploadPackService {
			http.NotFound(w, r)
			return
		}
		s.handleInfoRefs(w, r, name)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, uploadPackPath):
		name, ok := repoName(strings.TrimSuffix(r.URL.Path, uploadPackPath))
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.handleUploadPack(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

// handleInfoRefs answers the advertisement for name.
func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, name string) {
	s.incr(EndpointInfoRefs)
	s.captureHeader(r, EndpointInfoRefs)
	fault, matched := s.consumeFault(EndpointInfoRefs, name)
	if matched && s.enactHTTPFault(w, r, fault) {
		return
	}
	if s.rejectAuth(w, r) {
		return
	}
	repo, caps, ok := s.lookup(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", advertisementContentType)
	w.WriteHeader(http.StatusOK)
	if err := advertise(w, repo, caps, true); err != nil {
		s.tb.Errorf("fakegit: advertise %s: %v", name, err)
	}
}

// handleUploadPack answers one upload-pack request for name. The request
// body is read in full before anything else happens: net/http only watches a
// connection for the client going away once the body has been consumed, so
// a Hang that blocked with the body unread would never see the request
// context end when the client gives up, and would hold the server open.
func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, name string) {
	s.incr(EndpointUploadPack)
	s.captureHeader(r, EndpointUploadPack)
	body, readErr := io.ReadAll(r.Body)
	fault, matched := s.consumeFault(EndpointUploadPack, name)
	if matched && s.enactHTTPFault(w, r, fault) {
		return
	}
	if s.rejectAuth(w, r) {
		return
	}
	repo, caps, ok := s.lookup(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if readErr != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	req, closed, err := readUploadRequest(bytes.NewReader(body))
	if err != nil || closed {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	reply, err := buildReply(repo, caps, req, fault)
	if err != nil {
		s.tb.Errorf("fakegit: build reply for %s: %v", name, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if reply.badRequest != "" {
		http.Error(w, reply.badRequest, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", resultContentType)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeReply(w, flusher, r.Context().Done(), reply, fault.StallAfterBytes)
}

// lookup returns the repository registered as name and its capabilities.
func (s *Server) lookup(name string) (*Repo, Capabilities, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.repos[name]
	return repo, s.caps[name], ok
}

// incr counts one request to ep.
func (s *Server) incr(ep Endpoint) {
	s.mu.Lock()
	s.counts[ep]++
	s.mu.Unlock()
}

// captureHeader records the Authorization header r carried for ep.
func (s *Server) captureHeader(r *http.Request, ep Endpoint) {
	value, present := "", false
	if values, ok := r.Header["Authorization"]; ok {
		present = true
		if len(values) > 0 {
			value = values[0]
		}
	}
	s.recordAuth(ep, value, present)
}

// recordAuth stores what ep's latest request authenticated with.
func (s *Server) recordAuth(ep Endpoint, value string, present bool) {
	s.mu.Lock()
	s.authSeen[ep] = capturedAuth{value: value, present: present}
	s.mu.Unlock()
}

// rejectAuth enforces RequireAuth on r: it writes the failure status and
// reports true when a required header is missing or differs, and reports
// false, writing nothing, when no auth is required or it matches.
func (s *Server) rejectAuth(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	required := s.requiredAuth
	failStatus := s.authFailStatus
	s.mu.Unlock()
	if required == "" {
		return false
	}
	if values, ok := r.Header["Authorization"]; ok && len(values) > 0 && values[0] == required {
		return false
	}
	if failStatus == 0 {
		failStatus = http.StatusUnauthorized
	}
	w.WriteHeader(failStatus)
	return true
}

// consumeFault scans armed rules in registration order for the first one
// matching ep/name whose Count is not exhausted, consumes one use of it
// (unless Count is negative) and returns it.
func (s *Server) consumeFault(ep Endpoint, name string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.faults {
		rule := &s.faults[i]
		if rule.ep != ep || rule.fault.Count == 0 || (rule.name != "" && rule.name != name) {
			continue
		}
		if rule.fault.Count > 0 {
			rule.fault.Count--
		}
		return rule.fault, true
	}
	return Fault{}, false
}

// enactHTTPFault carries out the before-any-byte part of fault against w/r:
// Redirect, Status and Hang, in that precedence. It reports whether it
// answered the request; StallAfterBytes and ServeCommit are left to the
// upload-pack handler, which needs the pack to enact them.
func (s *Server) enactHTTPFault(w http.ResponseWriter, r *http.Request, fault Fault) bool {
	switch {
	case fault.Redirect != "":
		w.Header().Set("Location", fault.Redirect)
		w.WriteHeader(http.StatusFound)
		return true
	case fault.Status != 0:
		w.WriteHeader(fault.Status)
		return true
	case fault.Hang:
		<-r.Context().Done()
		return true
	}
	return false
}

// writeReply writes reply to w: the header, then - when stallAfter is
// positive - only that many pack bytes followed by a flush and a block until
// done is closed, otherwise the whole pack. flusher may be nil.
func writeReply(w io.Writer, flusher http.Flusher, done <-chan struct{}, reply uploadPackReply, stallAfter int) {
	if _, err := w.Write(reply.header); err != nil {
		return
	}
	if stallAfter > 0 {
		n := min(stallAfter, len(reply.pack))
		if _, err := w.Write(reply.pack[:n]); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		<-done
		return
	}
	_, _ = w.Write(reply.pack)
}

// repoName extracts name from a path of the form "/<name>.git". A name with
// a further slash or an empty name is refused, so a repository can only be
// addressed at the top level it was registered at.
func repoName(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, repoSuffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(p, "/"), repoSuffix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}
