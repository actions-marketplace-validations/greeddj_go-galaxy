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

// SourceRequiresNetwork reports whether FetchRequirementSource would dispatch
// source to fetchHTTP - the one arm that refuses outright under an offline
// Fetcher, rather than to fetchFile, which reads local state and works under
// --offline regardless.
//
// It is written in terms of parseRequirementSource, exactly as
// ValidateRequirementSource is just above, so the grammar keeps one home: a
// scheme parseRequirementSource learns to accept, or to refuse, changes what
// both functions answer at once, never one without the other.
//
// A source parseRequirementSource refuses answers false here as well as
// there: a value nothing here would ever fetch cannot be said to require the
// network to fetch it. Every source parseRequirementSource accepts answers
// true here except the file scheme, mirroring FetchRequirementSource's own
// dispatch above rather than restating its scheme list: fetchFile is the one
// arm that never dials anything, so it is the one exclusion, and a scheme
// parseRequirementSource later learns to accept falls to fetchHTTP - and
// therefore answers true here - without a second edit.
func SourceRequiresNetwork(source string) bool {
	parsed, _, err := parseRequirementSource(source)
	if err != nil {
		return false
	}

	return parsed.Scheme != fileScheme
}

// parseRequirementSource applies the whole grammar a signature source has to
// satisfy and returns the parsed URL alongside the display form every message
// about it uses.
//
// The display form is helpers.URLForMessage, which holds what each of its cuts
// removes and why they compose in any order. Two of the three earn their place
// here for reasons specific to a signature source: the query string is where a
// presigned capability travels, and a repository-authored source can carry one
// just as a download URL can, while the userinfo is a credential this grammar
// refuses outright and must not print while refusing. The third matters here
// more than at any other caller, because this is the one boundary whose values
// are not all requested: a fragment splits off before the Path fetchFile opens,
// so "file:///tmp/a#b.asc" opens /tmp/a, and a message keeping the fragment
// would name a file nothing read.
//
// It is computed before anything has parsed the value, which is what lets the
// refusal below name a value url.Parse itself rejected.
func parseRequirementSource(source string) (*url.URL, string, error) {
	display := helpers.URLForMessage(source)

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
// Every failure of the READ below - the open, the stat, or the size check -
// renders exactly one message, which is a decision about what a CI log may be
// used for: absent, unreadable, not a regular file, and too large are one
// observation rather than four. The message still names the path, so an
// operator reproduces the distinction with one ls -l. That collapse bounds
// only this function's own failure arm; it says nothing about what a read
// that SUCCEEDS goes on to make this program say, which the next two
// paragraphs state in full. The http arm deliberately keeps its own
// vocabulary - a status code, an offline refusal, a certificate failure, a
// cancellation - none of which is an oracle about this machine's filesystem.
//
// A read that succeeds is not where a repository-selected path stops being
// able to make this program talk about the machine it ran on. The bytes go
// on to signature.Verify, whose packet-framing walk in framing.go renders a
// verdict that is a function of the file's own content rather than merely of
// whether it opened: judgeHeader's unreadable-header arm renders the exact
// remaining byte count, which for a file that is not a packet stream is the
// whole file size; and the same walk's other arms render a first-byte tag
// class (judgeHeader's own tag arm), a declared-versus-present body length
// pair (judgePacket), and a parser's own unread-byte count (driveParser) -
// each a fact about this specific file's bytes, not a number this package
// invented. maxRenderedFailures bounds how many such verdicts one
// collection's own verification message renders to 8, per collection, per
// run; nothing bounds what a repository can still learn across collections
// or runs by naming a different path each time.
//
// That framing detail is disclosed rather than bounded, on three grounds.
// First, a bound would have to cover the whole framing vocabulary rather than
// one site - judgeHeader's unreadable-header size, its secret-key arm, its
// tag arm, judgePacket's declared-versus-present pair, and driveParser's
// unread-versus-total pair are each their own distinguishable answer about a
// local file - and a partial bound would leave the claim this comment makes
// both false and harder to state precisely than leaving every arm alone.
// Second, no two-way split covers every mechanism for closing it. One
// teaches framing.go where its bytes came from - collapsing the layer split
// this package is built on, framing.go judging bytes and source.go fetching
// them without either knowing the other's business. Another drops the cause
// from the error tree at verificationError, which is precisely the coupling
// verdictError's own doc comment forbids in the other direction. A further
// mechanism satisfies neither: redacting a failure's rendered message inside
// verificationError, keyed on the failure's own Origin scheme, leaves
// framing.go untouched and keeps the cause wrapped with %w, so errors.Is and
// exitcode.FromError stay unaffected - what it costs instead is the verdict
// renderer itself learning about source provenance, and the operator losing
// the diagnostic the Third ground below argues is worth keeping.
// Third, closing it would only narrow the oracle rather than close it: the
// status class a failure classifies as would still survive any such bound,
// so the floor still would not be one bit - the identical conclusion
// fetchHTTP's own doc comment reaches about refusing a loopback or
// link-local destination - and it would cost the operator the one
// diagnostic a server-carried or http-fetched blob's own failure still gets
// to keep.
//
// fetchHTTP's own remedy - a destination allow-list enforced at the dial,
// argued on that method's doc comment - has no purchase on this arm: a
// file:// source dials nothing, so there is no connection for an allow-list
// to gate.
//
// The message is where the read-failure collapse above holds; how long the
// call takes is not. The four conditions it collapses were measured apart on
// wall clock - an absent path at 2.7us, a permission-denied one at 10.7us, a
// directory at 12.1us, a 64 MiB file at 195us - so an observer able to time
// this one call recovers a distinction the failure message alone does not
// make. The gap is accepted: none of it reaches the message a CI log keeps,
// and it does not survive the phase this call sits in, where up to
// helpers.MaxSignaturesPerCollection sources are gathered under a
// network-latency budget no microsecond-scale difference is separable from.
// Padding the arm to a fixed duration would tax every ordinary run to deny a
// channel that narrow.
//
// The residual this accepts, stated rather than closed: repository content
// selects an absolute local path and this process reads it, and what a
// repository can learn from that spans both the collapsed read-failure
// message above and the content-dependent verification verdict beside it.
// Both are strictly weaker than a repository-content-driven WRITE this
// project already accepts - ansible.cfg discovery lets the same repository
// redirect [defaults] collections_path and [galaxy] cache_dir, which
// CLAUDE.md rules as containment relative to a configured path rather than a
// vulnerability.
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
// value with the query string that display exists to cut off.
//
// A non-200 is named by its status code and by nothing else. The body of such a
// response is bytes chosen by whoever answered, so it is never read and never
// rendered - a signature source is repository content, and an operator's stderr
// is not a place to print what it managed to make a server return.
//
// The disclosure fetchFile's own oracle paragraph does not make, because it
// reasons about the filesystem alone: this arm is a network-reachability
// oracle for whoever wrote the requirements file. A signatures: entry is an
// outbound GET from wherever the install runs, and what one outcome discloses
// is a predicate rather than a fixed list of shapes: an outcome carries
// whatever net/http put inside the *url.Error a failed request returns,
// because transportCause (below) hands back that inner error verbatim rather
// than the *url.Error's own rendering. What that measurably includes: the
// resolved address, its family, the port, and the connect errno - "dial tcp
// 127.0.0.1:54825: connect: connection refused", and a name that resolves to
// an IPv6 address rendering as "dial tcp [::1]:...: ..." - so a hostname in a
// signatures: entry reads back as this runner's own DNS resolver answered
// it, not merely as reachable or not; a TLS hostname mismatch as its own
// shape, rendering as "tls: failed to verify certificate: x509: certificate
// is valid for <the certificate's own DNS names>, not <the name requested>" -
// measured through a real client round trip, which is what puts the
// certificate's own SAN list, not merely the fact that verification failed,
// into an operator-facing message; "net/http: timeout awaiting response
// headers" as its own distinguishable outcome, separating a destination that
// accepted the connection and then stalled from one that refused it
// outright; and an over-ceiling body as a third, later outcome of its own,
// through helpers.ErrResponseTooLarge, once a response has actually been
// read rather than merely dialed. No address class is refused: a loopback,
// link-local, or unique-local destination is fetched exactly like any other.
//
// Redirects are FOLLOWED, not refused: fetch.checkRedirect bounds the hop
// count and deletes the Referer header net/http would otherwise compose for
// the hop, but it does not refuse the hop itself. So a failing hop's own dial
// text names the redirect TARGET's resolved address, while display - and
// every message this method builds around it - still names only the
// originally declared URL. That is why the remedy sentence below reads
// "enforced at the dial" rather than "enforced on the declared value": an
// allow-list checked only against the URL a signatures: entry names is
// walked past by a single redirect to an address that value never mentioned.
//
// That is a decision, not an omission, and it rests on three grounds. First,
// refusing one would not close the oracle above, only move it: a signature
// source is not the only destination repository content selects, and it is
// not the only one left unrefused - a discovered ./ansible.cfg is repository
// content too, and every destination it can configure ([galaxy] server,
// server_list, and a [galaxy_server.<id>]'s own url) is requested without a
// refusal of its own, and the last of those can carry a token in the file
// itself where a signature source never can. A collection's source: is a
// second example: it is requested even when it matches no configured Galaxy
// server, which warnUnmatchedSource
// (internal/galaxy/collections/server_candidates.go) warns about and proceeds
// with, recording there that a refusing mode is a flag surface nothing has
// asked for. Second, no predicate separates the two cases a refusal would have
// to tell apart: an operator's own hub on a private range is a legitimate
// signature host, and nothing distinguishes it from a probe, since both are a
// URL in a file the repository authors. A loopback/link-local refusal alone
// would still close something real - a service bound to loopback is reachable
// from nowhere else, and link-local covers 169.254.169.254, the address a
// cloud runner's metadata service answers on - but it narrows the oracle
// rather than closing it: the identical probe still runs against any RFC 1918
// address, and the same refusal's false positive lands on a legitimate
// private-range hub exactly as a broader one's would. Third, the check could
// only ever be enforced where the address is known, which is the dial and not
// the parse - a name resolving to a private address, and a name that
// re-resolves between the check and the connection, are both invisible to
// url.Parse - which puts it inside the shared client builder in
// internal/galaxy/fetch, where a control leaking onto the credential-bearing
// client built there would refuse every --server naming a private host: a
// worse failure than the one it closes.
//
// What would change the answer is a property, not a narrower version of this
// check: a refusal binding every repository-authored destination this program
// requests - a source:, a signature source, and whatever is added next -
// expressed as an operator-configured allow-list of origins rather than a
// blocklist of address ranges, and enforced at the dial. Until such a thing
// exists the residual stays exactly what it already was above: the
// reachability oracle, kept rather than closed.
//
// fetch.TestNewUnauthenticatedAttachesNoAuthorization
// (internal/galaxy/fetch/client_test.go) pins exactly what it can prove: the
// DESTINATION receives no credential of this program's own, wherever a
// signatures: entry points it. That is narrower than "no credential ever
// travels on this client's requests" and deliberately so: fetch.newClient's
// own newTransport sets Proxy: http.ProxyFromEnvironment, so an operator
// whose ambient HTTP_PROXY or HTTPS_PROXY carries userinfo has net/http
// attach a Proxy-Authorization header of net/http's own composing - measured,
// a proxy URL carrying userinfo makes a real *http.Transport send exactly
// that header, Basic-encoded, to the configured proxy. That credential
// travels to the PROXY the operator configured, never to the destination a
// signature source names, and it is not a credential this program ever read
// or chose. The TLS half is closed by construction rather than pinned by a
// test that never checks it - fetch.NewUnauthenticated's own doc comment
// holds that argument: insecureOriginSet(nil) is empty, so dispatch.insecure
// stays nil and every request goes to the fully-verified transport
// regardless of origin.
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
