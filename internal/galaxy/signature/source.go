package signature

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// fileScheme, httpScheme and httpsScheme are the whole allow-list
	// FetchRequirementSource dispatches on. url.Parse lowercases a scheme
	// before returning it, so these are compared against directly rather than
	// through a strings.ToLower nobody would ever see fire.
	fileScheme  = "file"
	httpScheme  = "http"
	httpsScheme = "https"

	// localhostAuthority is the one authority a file URL may name besides the
	// empty one. RFC 8089 gives "this host" both spellings, and a file URL
	// naming any other authority names a file on another machine.
	localhostAuthority = "localhost"
)

// Fetcher gathers signature blobs from the sources a requirements file names.
//
// One is built per run and shared across that run's workers: it owns a
// connection pool, so building one per collection would defeat connection reuse
// for the many small requests this phase makes. Its fields are written once at
// construction and only read afterwards, and *http.Client is itself safe for
// concurrent use, so the type is too.
//
// The client is built here rather than injected, and that is the point of the
// type rather than an implementation detail: a signature source is repository
// content, so no caller may be in a position to hand this a client carrying a
// Galaxy token or a relaxed TLS policy. See fetch.NewUnauthenticated for why
// that has to be closed by construction.
type Fetcher struct {
	client *http.Client
	// limit bounds one blob's bytes, on both the file and the http paths.
	limit int64
	// offline is kept alongside the client it was already used to build, so the
	// http path can refuse before composing a request rather than letting the
	// offline transport reject one it has already been handed. fetchHTTP holds
	// the argument for why that distinction is not cosmetic.
	offline bool
}

// NewFetcher returns a Fetcher whose blobs are capped at
// helpers.SignatureMaxSize and whose timeout is the no-progress budget
// fetch.NewUnauthenticated builds its client around - time-to-first-byte
// (ResponseHeaderTimeout) and the read watchdog's idle window - rather than a
// ceiling on a whole request, which that client deliberately does not set. A
// response that keeps dripping bytes is what helpers.SignatureFetchDeadline
// exists to bound instead. offline is passed straight through to the client, so
// an offline run's http sources are refused rather than attempted while its
// file sources still work.
func NewFetcher(timeout time.Duration, offline bool) *Fetcher {
	return newFetcher(timeout, offline, helpers.SignatureMaxSize)
}

// newFetcher is NewFetcher with the size ceiling as a parameter, so that
// crossing it can be exercised without a fixture the size of the real one. It
// mirrors loadKeyring below the same public entry point, and for the identical
// reason.
func newFetcher(timeout time.Duration, offline bool, limit int64) *Fetcher {
	return &Fetcher{client: fetch.NewUnauthenticated(timeout, offline), limit: limit, offline: offline}
}

// FetchRequirementSource fetches the one signature source named by source and
// returns it as a Blob whose Origin names where it came from.
//
// The method name carries a constraint no type here enforces: source must be a
// value this program read out of a requirements file, and nothing else. A
// requirements file is repository content - authored by whoever can commit to
// the repository, not by the operator running the install - which is what
// fetchFile's own residual paragraph is written against, and what makes this a
// rule about provenance rather than about trust. The distinction it draws is
// where the value came FROM: a requirements file, and not a Galaxy server's
// version metadata or the persisted snapshot, both of which are documented
// trust boundaries of this project reachable by a principal who never had commit
// access at all. A later fetch of a server-supplied URI therefore needs a
// sibling of this method that refuses file outright, never a reuse of this one.
//
// It is a rule rather than a provenance-carrying type - the same idiom as
// config.Secret.Reveal, where the name is where a reader is expected to stop
// and check - because such a type would have exactly one possible value here:
// --signature is out of scope for this tool, and a server's own signatures
// arrive inline as bytes rather than as URIs to fetch.
//
// One attempt is made and nothing is retried, which is one of the two disjuncts
// helpers.SignatureFetchDeadline's own doc comment leaves open for whatever
// gathers these sources - a per-source sub-budget being the other, and the
// gatherer's to establish rather than this method's. What a single attempt buys
// is a bound per source that does not divide the phase budget in advance: a
// source that black-holes costs one dial timeout, and one that accepts a
// connection and never answers costs the operator's own --timeout
// (ResponseHeaderTimeout). Neither is free of the starvation the disjunction is
// about - at a raised --timeout, one such source can still consume the whole
// phase budget and leave every source after it unfetched - so this bounds an
// attempt, not a share.
//
// The credential is kept out of every message by the CONSTRUCTION of display
// rather than by the order of the checks - see parseRequirementSource, which
// owns both that construction and the whole grammar this method dispatches
// after. The userinfo refusal it makes still precedes the scheme dispatch here,
// for its own separate and still-true reason: no request is composed for a
// credentialed value at all, the same order requirements.validateRequirement
// puts checkSourceUserinfo in.
func (f *Fetcher) FetchRequirementSource(ctx context.Context, source string) (Blob, error) {
	parsed, display, err := parseRequirementSource(source)
	if err != nil {
		return Blob{}, err
	}

	// Only the three schemes parseRequirementSource accepts reach this point,
	// which is why there is no default arm: the grammar lives in that one
	// function so a caller validating a source ahead of time and this fetch can
	// never disagree about what is fetchable.
	if parsed.Scheme == fileScheme {
		return f.fetchFile(parsed, display)
	}

	return f.fetchHTTP(ctx, source, display)
}

// ValidateRequirementSource reports whether source is a value
// FetchRequirementSource could fetch, without fetching anything.
//
// It exists for the boundary a requirements file enters through
// (requirements.checkSignatureSources), so that a source this package would
// refuse is refused where it was written - as a configuration error naming the
// file - rather than inside an install worker, where it would arrive as one
// collection's failure among however many others. The two cannot drift, because
// this and the fetch answer through the same parseRequirementSource: adding a
// scheme, or a refusal, changes both at once.
//
// It returns exactly the errors the fetch's own refusals return, messages
// included, and touches nothing: no file is opened and no request is composed.
func ValidateRequirementSource(source string) error {
	_, _, err := parseRequirementSource(source)

	return err
}

// parseRequirementSource applies the whole grammar a signature source has to
// satisfy and returns the parsed URL alongside the display form every message
// about it uses.
//
// The display form is computed before anything has parsed the value. Three
// cuts. The query string is where a presigned capability travels, and a
// signature source can carry one just as a download URL can. The userinfo is a
// credential this grammar refuses outright and must not print while refusing.
// The fragment is the part that never travels at all: net/http composes a
// request from the path and the query alone, and url.Parse splits a fragment
// off before the Path fetchFile opens, so a message keeping one would name a
// location no request reached and no file was read from -
// "file:///tmp/a#b.asc" opens /tmp/a. Their order is immaterial:
// WithoutUserinfo's authority scan ends at "?" and at "#", and the other two
// keep everything before their own delimiter, so no cut can reach across
// another's boundary. helpers.WithoutUserinfo's own doc comment holds what a
// delimiter sitting INSIDE a userinfo costs, the one residual all three share.
func parseRequirementSource(source string) (*url.URL, string, error) {
	display := helpers.TruncateForMessage(helpers.WithoutUserinfo(helpers.WithoutFragment(helpers.WithoutQuery(source))))

	parsed, err := url.Parse(source)
	if err != nil {
		// The parse error is deliberately not wrapped in: url.Error renders the
		// whole value it failed on, query string and userinfo included.
		return nil, display, fmt.Errorf("%w: %q could not be parsed as a URL", helpers.ErrUnsupportedSignatureSource, display)
	}
	if parsed.User != nil {
		return nil, display, fmt.Errorf("%w: %q", helpers.ErrSignatureSourceUserinfo, display)
	}
	// Two shapes are refused here rather than left to the scheme switch, which
	// would pass "http" and "https" straight through to a request that can never
	// be composed: an opaque URL - "http:host/sig.asc", a scheme followed by
	// something that does not begin with "/" - and an http or https URL naming
	// no authority at all - "https:///sig.asc", "https://". http.Client answers
	// either with "no Host in request URL", which this package would report as a
	// source that was unreachable. That is a value no host was ever contacted
	// for, classified as a transport failure - the one class a CI is most likely
	// to retry forever.
	//
	// One message covers both because it is exact for both: neither names a host
	// to fetch from, and neither names an absolute path this tool could read
	// locally - the "/sig.asc" of "https:///sig.asc" is an absolute path, but a
	// local one only the file scheme ever reaches fetchFile with.
	//
	// For http and https the two halves overlap rather than dividing the space,
	// and the overlap runs one way: url.Parse sets Opaque only when no "//"
	// follows the scheme, and parses a Host only out of an authority that "//"
	// introduces, so an opaque http value already has an empty Host and
	// hostlessHTTP alone would refuse it. The Opaque half earns its place on
	// every other scheme instead - "data:...", "file:relative/x" - where it
	// renders this exact message rather than leaving the value to the switch
	// below: the default arm for one, fetchFile's own opaque check for the other.
	//
	// fetchFile still makes its own opaque check, because there the same
	// condition is one half of a wider question, "does this name an absolute
	// local path".
	if parsed.Opaque != "" || hostlessHTTP(parsed) {
		return nil, display, fmt.Errorf("%w: %q names no host and no absolute path",
			helpers.ErrUnsupportedSignatureSource, display)
	}
	switch parsed.Scheme {
	case fileScheme:
		return parsed, display, checkFileSource(parsed, display)
	case httpScheme, httpsScheme:
		return parsed, display, nil
	default:
		// Everything else lands here, the empty scheme included - so a bare
		// server_list id, which url.Parse accepts while yielding no scheme, is
		// refused. That is the deliberate opposite of how the sibling check on a
		// collection's source: value treats one - there such an id is left alone
		// because it names a configured server rather than a location, while
		// here nothing resolves an id, so a value naming no location names
		// nothing at all.
		return nil, display, fmt.Errorf("%w: %q", helpers.ErrUnsupportedSignatureSource, display)
	}
}

// checkFileSource applies the file scheme's own sub-grammar: which authority a
// file URL may name, and that it names an absolute path.
//
// It lives here, in the grammar every caller shares, rather than only in
// fetchFile where it used to. Measured before the move, five values were
// accepted by ValidateRequirementSource and then refused by the fetch:
// "file://otherhost/abs/sig.asc", "file://evil.example/abs/sig.asc", "file:",
// "file://" and "file://localhost". A value accepted at load and refused in an
// install worker arrives joined behind helpers.ErrInstallationFailed and exits
// as an install failure rather than as the usage error it is - the exact
// exit-class defect the load-time gate was added to close.
//
// fetchFile keeps its own copies of both checks, which its doc comment already
// calls a backstop: this function guards the boundary, that one guards the
// operation.
func checkFileSource(u *url.URL, display string) error {
	if u.Host != "" && !strings.EqualFold(u.Host, localhostAuthority) {
		return fmt.Errorf("%w: %q names a host this tool cannot read a file from",
			helpers.ErrUnsupportedSignatureSource, display)
	}
	if u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return fmt.Errorf("%w: %q does not name an absolute local path",
			helpers.ErrUnsupportedSignatureSource, display)
	}

	return nil
}

// hostlessHTTP reports whether u is an http or https URL naming no authority at
// all: "https:///sig.asc", "https://", "http://". url.Parse accepts every one of
// them, reporting an empty Host alongside an empty Opaque, so without this
// neither the opaque refusal nor the scheme switch finds anything wrong with a
// value no request can be composed from.
//
// The scheme half is not redundant with the host half: a file URL names no
// authority by design - "file:///abs" is RFC 8089's own spelling of "this
// host" - so an empty authority condemns a value only when its scheme is one
// this tool would have had to contact a host for.
func hostlessHTTP(u *url.URL) bool {
	return u.Host == "" && (u.Scheme == httpScheme || u.Scheme == httpsScheme)
}

// fetchFile reads a signature blob out of a local file named by a file URL.
//
// Three spellings are accepted, and the set is a decision rather than whatever
// url.Parse happened to produce. An empty authority ("file:///abs") and one
// naming localhost ("file://localhost/abs") are RFC 8089's two ways of writing
// "this host"; the single-slash form ("file:/abs") is a third, which url.Parse
// reports as an empty authority with an absolute path and which is therefore
// accepted without a branch of its own. Any other authority names a file on
// another machine, which this tool cannot read and must not silently
// reinterpret as a local path. The relative form ("file:relative/x") is refused
// too: url.Parse reports it as an Opaque with no Path, and a signature source
// has no base for it to be relative to.
//
// What O_NONBLOCK below buys is bounded, and the bound is worth stating: it
// keeps the OPEN from blocking on the shape a local writer can plant, and it
// does nothing for the READ that follows. A regular file on a hung network or
// FUSE mount blocks in the read itself, which no budget this package or its
// callers hold can end - a context cannot interrupt a blocking read on a file
// descriptor - so such a source stalls the phase it sits in for as long as the
// mount does. That is the residual, and it is accepted: the alternative is
// reading local files on a goroutine nothing can join, which trades a stalled
// phase for a leaked one.
//
// The open carries syscall.O_NONBLOCK and the regular-file check is made
// against the descriptor it returned, not against a path stat preceding it.
// Both halves matter and neither substitutes for the other. O_NONBLOCK is what
// makes the open itself safe on the shape a local writer can plant:
// open(O_RDONLY) on a FIFO blocks until a writer appears, which is past every
// deadline this phase carries, since a process blocked in open is not making a
// request any budget is watching - POSIX has O_NONBLOCK|O_RDONLY on a FIFO
// return at once instead. Checking the mode of the opened descriptor is what
// removes the window between a decision and the file it was made about: a stat
// naming a path answers about whatever sat there at that instant, while
// File.Stat answers about the exact object this process is about to read.
// cleanup's own manifest scan states the blocking-open property for the same
// reason, one layer away.
//
// Every failure below renders one message, which is a decision about what a CI
// log may be used for. This arm reads a local path selected by repository
// content, so each outcome an operator can tell apart is an oracle a repository
// can query about the machine running the install: four distinguishable
// answers (absent, unreadable, not a regular file, too large) are a filesystem
// probe. One bit is intrinsic to the message and is the accepted floor: reading
// the file is the whole operation, so a repository can always learn whether a
// path was readable as a signature. The message still names the path, so an
// operator reproduces the distinction with one ls -l; nothing else about the
// path survives into the message. The http arm deliberately keeps its own
// vocabulary - a status code, an offline refusal, a certificate failure, a
// cancellation - none of which is an oracle about this machine's filesystem.
//
// The message is where that floor holds; how long the call takes is not. The
// conditions collapsed here were measured apart on wall clock - an absent path
// at 2.7us, a permission-denied one at 10.7us, a directory at 12.1us, a 64 MiB
// file at 195us - so an observer able to time this one call recovers more than
// the one bit. The gap is accepted: none of it reaches the message a CI log
// keeps, and it does not survive the phase this call sits in, where up to
// helpers.MaxSignaturesPerCollection sources are gathered under a
// network-latency budget no microsecond-scale difference is separable from.
// Padding the arm to a fixed duration would tax every ordinary run to deny a
// channel that narrow.
//
// The residual this accepts, stated rather than closed: repository content
// selects an absolute local path and this process reads it. The observable is
// the one bit above, the message names the path an operator can check, and it
// is strictly weaker than a repository-content-driven WRITE this project
// already accepts - ansible.cfg discovery lets the same repository redirect
// [defaults] collections_path and [galaxy] cache_dir, which CLAUDE.md rules is
// containment relative to a configured path rather than a vulnerability.
//
// A file source works under --offline. It is local state, exactly like the
// cache: nothing about reading it needs the network to be reachable.
func (f *Fetcher) fetchFile(u *url.URL, display string) (Blob, error) {
	if u.Host != "" && !strings.EqualFold(u.Host, localhostAuthority) {
		return Blob{}, fmt.Errorf("%w: %q names a host this tool cannot read a file from",
			helpers.ErrUnsupportedSignatureSource, display)
	}
	// The Opaque half is kept even though FetchRequirementSource now refuses
	// every opaque value ahead of this call: the condition as a whole asks
	// whether the URL names an absolute local path, which is this function's own
	// question and has a second way of answering no.
	if u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return Blob{}, fmt.Errorf("%w: %q does not name an absolute local path",
			helpers.ErrUnsupportedSignatureSource, display)
	}

	// #nosec G304,G703 -- u.Path comes from a requirements file, which is
	// repository content; reading the local path it names is the entire
	// operation, and the residual that accepts is stated above.
	file, err := os.OpenFile(u.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Blob{}, unreadableFileSource(display)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Blob{}, unreadableFileSource(display)
	}

	data, err := f.readFile(file, info.Size())
	if err != nil || int64(len(data)) > f.limit {
		return Blob{}, unreadableFileSource(display)
	}

	return Blob{Origin: display, Data: data}, nil
}

// unreadableFileSource is the one message every failure on the file path
// renders, for the reason fetchFile's own doc comment gives: a message that
// discriminates between the ways a local read can fail is a filesystem oracle
// for whoever wrote the source. It names display and nothing else - no wrapped
// OS error, no size, no shape - so the four outcomes it stands for are one
// observation.
//
// That binds a caller as well as this function: nothing may branch on
// fs.ErrNotExist for a signature source, because no OS cause is reachable
// through this error at all. An absent path and an unreadable one are one
// answer here by design, and a caller that needs to tell them apart is asking
// for the oracle this collapse exists to withhold.
func unreadableFileSource(display string) error {
	return fmt.Errorf("%w: %q could not be read", helpers.ErrSignatureSourceUnavailable, display)
}

// readFile reads at most limit+1 bytes of file, so that its caller can tell a
// file of exactly limit bytes from a longer one truncated there.
//
// That +1 belongs to this path alone and must not be copied to the http one:
// helpers.NewSizeLimitedReader already fails on the first byte past its
// ceiling, so handing it limit+1 would raise the ceiling by a byte, while an
// io.LimitedReader merely stops and reports a clean EOF, which is
// indistinguishable from the file genuinely ending there. Two size caps, two
// behaviors on the crossing read, and the arithmetic differs accordingly.
//
// helpers.NewSizeLimitedReader is deliberately not used here either, for the
// reason keyringMaxSize's own doc comment records: it raises
// helpers.ErrResponseTooLarge, which names a response, and this is a local file
// no response was ever involved in reading.
//
// size is the caller's own File.Stat of this same descriptor, so the reservation
// describes the object being read rather than whatever a path resolved to
// earlier. It is still clamped from both sides: at the ceiling, since the read
// stops there whatever the file claims, and at zero, since a negative capacity
// panics make and a size is not this function's to vouch for.
func (f *Fetcher) readFile(file *os.File, size int64) ([]byte, error) {
	// The extra bytes.MinRead is the headroom ReadFrom wants available before
	// it stops, so an ordinary signature is read into one allocation.
	buf := bytes.NewBuffer(make([]byte, 0, min(max(size, 0), f.limit)+bytes.MinRead))
	if _, err := buf.ReadFrom(&io.LimitedReader{R: file, N: f.limit + 1}); err != nil {
		// Unreachable through fetchFile, which refuses every non-regular shape
		// before this call, and reachable here: the descriptor is a parameter, so
		// TestReadFileReportsAReadFailure hands this a directory and gets the
		// read failure a vanished mount or a medium error would produce. What the
		// arm is worth is that such a failure is reported as an unavailable
		// source rather than as a short blob, which would fail the signature
		// check on truncated bytes instead.
		return nil, err
	}

	return buf.Bytes(), nil
}

// fetchHTTP fetches a signature blob over http or https, in exactly one
// attempt.
//
// The offline refusal is raised here, before a request is composed, rather than
// left to the offline transport the client was already built around. Two
// reasons, and the second is the one that would not be rediscovered: composing
// a request that cannot go anywhere is pointless work, and offlineTransport's
// own message formats req.URL, so letting the request reach it would print the
// value with the query string display exists to cut off.
//
// A non-200 is named by its status code and by nothing else. The body of such a
// response is bytes chosen by whoever answered, so it is never read and never
// rendered - a signature source is repository content, and an operator's stderr
// is not a place to print what it managed to make a server return.
//
// The disclosure fetchFile's own oracle paragraph does not make, because it
// reasons about the filesystem alone: this arm is a network-reachability oracle
// for whoever wrote the requirements file. A signatures: entry is an outbound
// GET from wherever the install runs, and its outcomes are distinguishable in
// the operator-facing message - a transport failure, a TLS failure, a status
// code, and a 200 whose bytes are not a signature all read differently - so a
// repository can map what its CI runner can reach, internal addresses included.
// Nothing here narrows the destination: whether this tool should refuse
// loopback and link-local targets is a decision of its own, and this paragraph
// is the disclosure it would be made against rather than a claim that the
// question is settled.
func (f *Fetcher) fetchHTTP(ctx context.Context, source, display string) (Blob, error) {
	if f.offline {
		return Blob{}, fmt.Errorf("%w: %q: %w", helpers.ErrSignatureSourceUnavailable, display, helpers.ErrOfflineMode)
	}

	// The request carries the full source, query string included: the query may
	// be the capability that makes the source fetchable at all. Only what is
	// reported back is stripped.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		// Documented-uncovered, and unreachable rather than merely awkward to
		// reach: NewRequestWithContext fails on a nil context, an invalid
		// method, or a URL url.Parse refuses, and here the context is the
		// caller's, the method is a constant, and the URL already parsed above
		// through that same url.Parse. The arm stays because the alternative is
		// dereferencing a request this call may not have built, and what is
		// lost by never covering it is a case this program cannot produce.
		//
		// transportCause, not err: the one failure shape this call has that
		// carries a cause is url.Parse's own *url.Error, which renders the whole
		// raw value - query string and userinfo included - and would put back
		// exactly what display was computed to keep out.
		return Blob{}, fmt.Errorf("%w: %q: %w", helpers.ErrSignatureSourceUnavailable, display, transportCause(err))
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return Blob{}, fmt.Errorf("%w: %q: %w", helpers.ErrSignatureSourceUnavailable, display, transportCause(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Blob{}, fmt.Errorf("%w: %q: HTTP %d", helpers.ErrSignatureSourceUnavailable, display, resp.StatusCode)
	}

	// The read is not pre-sized from Content-Length, unlike the file path's
	// read from the stat size: that header is chosen by whoever answered, so
	// sizing an allocation from it hands a hostile source a way to spend this
	// process's memory without spending its own bytes.
	data, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, f.limit))
	if err != nil {
		return Blob{}, fmt.Errorf("%w: %q: %w", helpers.ErrSignatureSourceUnavailable, display, err)
	}

	return Blob{Origin: display, Data: data}, nil
}

// transportCause returns the failure inside a *url.Error, or err unchanged when
// it is not one.
//
// http.Client wraps every transport failure in a *url.Error, whose Error()
// renders the whole request URL - query string included - so wrapping one as it
// came would put back exactly what display was computed to keep out. Only the
// rendering is dropped: the inner error is returned for the caller to wrap with
// %w, so helpers.ErrOfflineMode, helpers.ErrReadStalled, context.Canceled and
// context.DeadlineExceeded all stay reachable through errors.Is, which is what
// cmd/go-galaxy/exitcode classifies on.
func transportCause(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || urlErr.Err == nil {
		return err
	}

	return urlErr.Err
}
