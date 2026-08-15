package signature

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// sourceTestTimeout is what every Fetcher in this file is built with. It is
	// generous rather than tight on purpose: nothing here measures a timeout,
	// and a value small enough to race an in-process httptest round trip would
	// turn an unrelated regression into a flake.
	sourceTestTimeout = 5 * time.Second

	// sourceBlobBody is the payload every accepting case transfers. Its bytes
	// are never verified as a signature - this file covers the fetch and
	// nothing below it - so an armor envelope over a token body is enough to
	// tell "the blob arrived" from "something else did".
	sourceBlobBody = "-----BEGIN PGP SIGNATURE-----\n\naGVsbG8=\n-----END PGP SIGNATURE-----\n"

	// sigLeafName is the leaf every file fixture here is written to, and the
	// path every http fixture is requested under.
	sigLeafName = "sig.asc"

	// presignedQuery is the shape this project already treats as a bearer
	// capability elsewhere: a presigned object-storage URL's query string.
	presignedQuery = "?X-Amz-Signature=deadbeef"

	// serverBodyMarker is a token no error message may carry. It stands in for
	// whatever an error page, a proxy, or a login form puts in a non-200 body.
	serverBodyMarker = "body-marker-that-must-not-be-printed"

	// sourcePassword is the password every credentialed fixture in this file
	// carries, and the token their messages are searched for. It is spelled
	// once so a row cannot be written that greps for something it did not
	// embed.
	sourcePassword = "s3cr3t"

	// unavailablePrefix and unreadableSuffix are the file arm's one message,
	// hand-spelled rather than built from helpers.ErrSignatureSourceUnavailable
	// and the production format string: an expectation assembled from the
	// values it checks cannot be killed by mutating them.
	unavailablePrefix = `collection signature source unavailable: "`
	unreadableSuffix  = `" could not be read`

	// requestLogDepth is how many requests a fixture server records before a
	// send would block. Every test here makes one or two.
	requestLogDepth = 4

	// tinySourceLimit is the blob ceiling the size-cap tests run against, small
	// enough that both sides of it are spelled as literal bodies.
	tinySourceLimit = 64
)

// writeSourceFile writes body at dir/name and returns the absolute path.
func writeSourceFile(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), helpers.FileMod); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	return path
}

// newSourceServer answers every request with 200 and body, and hands back the
// RequestURI of each one it served so a test can assert what actually went out
// on the wire rather than what the caller passed in.
func newSourceServer(t *testing.T, body string) (*httptest.Server, chan string) {
	t.Helper()

	seen := make(chan string, requestLogDepth)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.RequestURI()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv, seen
}

// newFailingServer answers every request with status and a body carrying
// serverBodyMarker, so a test can prove that body never reaches a message.
func newFailingServer(t *testing.T, status int) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(serverBodyMarker))
	}))
	t.Cleanup(srv.Close)

	return srv
}

// newTestFetcher builds a Fetcher at the given ceiling. Tests that do not
// exercise the ceiling pass helpers.SignatureMaxSize, so the seam is only ever
// used to shrink the real value rather than to change what is under test.
func newTestFetcher(offline bool, limit int64) *Fetcher {
	return newFetcher(sourceTestTimeout, offline, limit)
}

// TestNewFetcherCarriesTheRealCeiling is what keeps every test below honest:
// they all run through the shrinking seam, so without this the public
// constructor could pass any ceiling at all - or none - and nothing would say
// so. The fetch on the end proves the client it built is usable, which the
// field check alone does not.
func TestNewFetcherCarriesTheRealCeiling(t *testing.T) {
	t.Parallel()

	f := NewFetcher(sourceTestTimeout, false)
	if f.limit != helpers.SignatureMaxSize {
		t.Fatalf("NewFetcher limit = %d, want helpers.SignatureMaxSize (%d)", f.limit, helpers.SignatureMaxSize)
	}
	if f.offline {
		t.Fatal("NewFetcher(offline=false) built an offline fetcher")
	}

	srv, _ := newSourceServer(t, sourceBlobBody)
	blob, err := f.FetchRequirementSource(t.Context(), srv.URL+"/"+sigLeafName)
	if err != nil {
		t.Fatalf("FetchRequirementSource error = %v, want nil", err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("data = %q, want the fixture blob", blob.Data)
	}
}

// TestFetchRequirementSourceAcceptsEveryFetchableSpelling is the positive
// control for every refusal in this file: the same function, the same fixture
// file and the same fixture server, accepted.
//
// The four file spellings are RFC 8089's. Three are here because url.Parse
// reports each differently: an empty authority, one naming localhost, and the
// single-slash form carrying no authority. The fourth spells that authority
// LocalHost and pins fetchFile's case-insensitive host compare instead. All
// four name one file, so accepting them is about spelling, not disk contents.
func TestFetchRequirementSourceAcceptsEveryFetchableSpelling(t *testing.T) {
	t.Parallel()

	path := writeSourceFile(t, t.TempDir(), sigLeafName, sourceBlobBody)
	srv, _ := newSourceServer(t, sourceBlobBody)

	cases := []struct {
		name   string
		source string
	}{
		{name: "file url with an empty authority", source: "file://" + path},
		{name: "file url naming localhost", source: "file://localhost" + path},
		{name: "file url naming LocalHost", source: "file://LocalHost" + path},
		{name: "file url with a single slash", source: "file:" + path},
		{name: "http url", source: srv.URL + "/" + sigLeafName},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), tc.source)
			if err != nil {
				t.Fatalf("FetchRequirementSource(%q) error = %v, want nil", tc.source, err)
			}
			if string(blob.Data) != sourceBlobBody {
				t.Fatalf("FetchRequirementSource(%q) data = %q, want the fixture blob", tc.source, blob.Data)
			}
			if blob.Origin != tc.source {
				t.Fatalf("FetchRequirementSource(%q) origin = %q, want the source itself", tc.source, blob.Origin)
			}
		})
	}
}

// TestFetchRequirementSourceRefusesEveryUnfetchableShape covers the whole
// unsupported-source class in one table: a scheme outside the three, no scheme
// at all, a file URL naming another host, one carrying a relative path, one
// naming no path at all, and a value url.Parse itself refuses.
//
// The failure message names the row and logs the error separately rather than
// rendering it inline, for the reason the mutation quotes below depend on:
// under a mutation these rows fail carrying a rendered URL, which a quoted
// line should not have to reproduce byte for byte.
func TestFetchRequirementSourceRefusesEveryUnfetchableShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		source string
	}{
		{name: "ftp scheme", source: "ftp://h/x"},
		{name: "git+ssh scheme", source: "git+ssh://h/x"},
		{name: "data scheme", source: "data:text/plain;base64,aGk="},
		{name: "bare absolute path", source: "/bare/abs/path"},
		{name: "relative path", source: "relative/path"},
		{name: "empty", source: ""},
		{name: "scheme-relative url", source: "//host/path"},
		{name: "file url naming a foreign host", source: "file://evil.example/x"},
		{name: "file url with a relative path", source: "file:relative/x"},
		{name: "file url naming localhost with no path", source: "file://localhost"},
		{name: "unparseable url", source: "http://%zz/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Three mutations, one per refusal this table stands for, each
			// applied through go test -overlay so no production file is edited.
			//
			// Deleting the scheme switch's default refusal - its return
			// replaced by `return Blob{}, nil` - fails every row that reaches
			// that arm, the ftp one among them:
			//
			//	source_test.go:255: ftp scheme was not refused as unfetchable
			//
			// Accepting any file authority - the u.Host condition in fetchFile
			// replaced by a bare false - fails the foreign-host row, since an
			// absent /x is then a source that is unavailable rather than one
			// naming nothing fetchable:
			//
			//	source_test.go:255: file url naming a foreign host was not refused as unfetchable
			//
			// Dropping the absolute-path half of fetchFile's second condition -
			// !strings.HasPrefix(u.Path, "/") replaced by a bare false - fails
			// the no-path row, on the empty path the open is then handed:
			//
			//	source_test.go:255: file url naming localhost with no path was not refused as unfetchable
			//
			// Two rows are deliberately not among the three, and both are
			// documentary rather than pinned: each is refused twice over, so no
			// single deletion moves it. The relative row is caught by
			// FetchRequirementSource's opaque check and again by the Opaque half
			// of that same condition in fetchFile. The data row is caught by
			// that same opaque check - url.Parse reports it as an opaque URL -
			// and again by the default arm above, which has no "data" case to
			// reach, so it survives either deletion on its own. The opaque
			// check's own killing mutation is quoted on the credential test
			// below, which reaches it through a value fetchFile would have
			// accepted.
			_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), tc.source)
			if !errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
				t.Logf("error = %v", err)
				t.Fatalf("%s was not refused as unfetchable", tc.name)
			}
		})
	}
}

// TestFetchFileRefusalsAreAboutTheSpellingNotTheFile is the exact positive
// control for the two file-shaped rows above, which name paths nothing wrote.
//
// One real file, three URLs for it: the localhost spelling reads it, while the
// foreign-authority and relative spellings of that same path are refused. So
// the refusals are shown to be about how the URL names the file rather than
// about the file being missing, which a table of synthetic paths cannot say.
func TestFetchFileRefusalsAreAboutTheSpellingNotTheFile(t *testing.T) {
	t.Parallel()

	path := writeSourceFile(t, t.TempDir(), sigLeafName, sourceBlobBody)
	fetcher := newTestFetcher(false, helpers.SignatureMaxSize)

	blob, err := fetcher.FetchRequirementSource(t.Context(), "file://localhost"+path)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(a localhost file url) error = %v, want nil", err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}

	for _, source := range []string{"file://evil.example" + path, "file:" + strings.TrimPrefix(path, "/")} {
		if _, err := fetcher.FetchRequirementSource(t.Context(), source); !errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
			t.Fatalf("FetchRequirementSource(%q) error = %v, want the unsupported-source sentinel", source, err)
		}
	}
}

// TestFetchRequirementSourceRefusesUserinfo pins both halves of the userinfo
// rule: the value is refused, and the refusal does not print the credential it
// was refused for.
//
// The positive control is the same server and the same path with the userinfo
// deleted, which must be fetched normally - so the refusal is shown to be about
// the credential rather than about anything else in the URL.
func TestFetchRequirementSourceRefusesUserinfo(t *testing.T) {
	t.Parallel()

	srv, _ := newSourceServer(t, sourceBlobBody)
	clean := srv.URL + "/" + sigLeafName
	credentialed := strings.Replace(clean, "http://", "http://user:pass@", 1) + presignedQuery

	// Deleting the userinfo branch in FetchRequirementSource, applied through
	// go test -overlay so no production file is edited, fails here: net/http
	// sets Basic auth from the URL and the fixture server answers 200, so the
	// fetch succeeds and no sentinel is returned.
	//
	//	source_test.go:311: a source carrying userinfo: error = <nil>, want the userinfo sentinel
	_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), credentialed)
	if !errors.Is(err, helpers.ErrSignatureSourceUserinfo) {
		t.Fatalf("a source carrying userinfo: error = %v, want the userinfo sentinel", err)
	}

	for _, secret := range []string{"pass", "user:pass", "user@", "X-Amz-Signature"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("refusal message %q carries %q, which it exists to keep out of every sink", err, secret)
		}
	}

	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), clean)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(%q) error = %v, want nil", clean, err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}
}

// TestFetchRequirementSourceNeverPrintsACredential covers the credentialed
// spellings the userinfo refusal above never sees, because url.Parse refuses
// four of them and reports the fifth as an opaque URL with no User to find.
//
// Every row is a real credential leak if the message is built from the raw
// value: the password sits in the authority, and the reason the value was
// refused is somewhere else entirely - an invalid port, a bad percent escape,
// a malformed IP literal, a control character, or a shape no request can be
// composed from. What makes the rows a class rather than five cases is that
// display is computed before the parse, so none of them has an arm of its own.
//
// Both halves are asserted, because either alone is satisfiable by a broken
// implementation: a refusal that printed nothing at all would keep the password
// out while leaving an operator no way to find the offending entry, so the host
// must still be named.
//
// The positive control is the fixture server's own clean URL, fetched by the
// same constructor before the table runs. It is the only control these rows can
// have: the other four values are refused for a defect in the value itself, so
// no fixture can serve them however the fetch is built.
func TestFetchRequirementSourceNeverPrintsACredential(t *testing.T) {
	t.Parallel()

	srv, _ := newSourceServer(t, sourceBlobBody)
	clean := srv.URL + "/" + sigLeafName
	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), clean)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(%q) error = %v, want nil", clean, err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}

	cases := []struct {
		name     string
		source   string
		wantHost string
	}{
		{name: "invalid port", source: "https://user:" + sourcePassword + "@host:notaport/sig.asc", wantHost: "host:notaport"},
		{name: "bad percent escape", source: "https://user:" + sourcePassword + "@host/%zz/sig.asc", wantHost: "host"},
		{name: "invalid ip literal", source: "https://user:" + sourcePassword + "@[::1x]/sig.asc", wantHost: "[::1x]"},
		{name: "control character", source: "https://user:" + sourcePassword + "@host/sig\x00.asc", wantHost: "host"},
		// The opaque row is the one that was misclassified rather than merely
		// leaked: before FetchRequirementSource refused Opaque, this value
		// passed the scheme switch, reached http.Client, and came back as
		// "no Host in request URL" wrapped in ErrSignatureSourceUnavailable -
		// a value no host was ever contacted for, reported as a transport
		// failure, which is the one class a CI retries forever.
		{name: "opaque url", source: "http:user:" + sourcePassword + "@host/sig.asc", wantHost: "host"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Two mutations, applied through go test -overlay so no production
			// file is edited.
			//
			// Computing display from helpers.WithoutQuery alone, with the
			// userinfo cut no longer composed with it, fails every row on the
			// second assertion. The invalid-port row, quoted as it came:
			//
			//	source_test.go:415: refusal message carries "s3cr3t"
			//
			// Deleting FetchRequirementSource's refusal of a value that names no
			// host fails this table's opaque row alone, on the first assertion
			// (the hostless-URL test below fails too): the value is fetched, and
			// http.Client's own answer arrives under the other sentinel:
			//
			//	source_test.go:412: opaque url was not refused as an unsupported source
			//
			// That deletion has to take both halves of the one condition, and
			// the row is over-determined between them: an opaque http URL is
			// reported by url.Parse with an empty Host as well as a non-empty
			// Opaque, so hostlessHTTP catches exactly what the Opaque test would
			// have, and deleting either half on its own fails nothing here.
			//
			// The rendered error goes to the log rather than into either
			// message, since one of these rows carries a NUL byte and another
			// an OS-chosen rendering of a malformed address.
			_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), tc.source)
			if !errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
				t.Logf("error = %v", err)
				t.Fatalf("%s was not refused as an unsupported source", tc.name)
			}
			if strings.Contains(err.Error(), sourcePassword) {
				t.Fatalf("refusal message carries %q", sourcePassword)
			}
			if !strings.Contains(err.Error(), tc.wantHost) {
				t.Logf("message = %q", err)
				t.Fatalf("%s: refusal message does not name the host, so it names nothing an operator can act on", tc.name)
			}
		})
	}
}

// TestFetchStripsQueryFromOriginAndErrors covers the asymmetry the query cut is
// built around: the request goes out with the query, because the query may be
// the capability that makes the source fetchable at all, while everything
// reported back is cut off at it.
//
// Both directions are asserted against one shape, since either alone would be
// satisfied by a broken implementation: stripping the query from the request
// too would pass the reporting half while breaking every presigned source.
func TestFetchStripsQueryFromOriginAndErrors(t *testing.T) {
	t.Parallel()

	srv, seen := newSourceServer(t, sourceBlobBody)
	clean := srv.URL + "/" + sigLeafName

	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), clean+presignedQuery)
	if err != nil {
		t.Fatalf("FetchRequirementSource error = %v, want nil", err)
	}
	if got := <-seen; !strings.Contains(got, strings.TrimPrefix(presignedQuery, "?")) {
		t.Fatalf("server saw RequestURI %q, want the query carried through to the wire", got)
	}

	// Filling Blob.Origin from source instead of display, applied through go
	// test -overlay so no production file is edited, fails here. The rendered
	// values go to the log rather than into the message, since both carry the
	// fixture server's own ephemeral port and no two runs would agree:
	//
	//	source_test.go:455: Blob.Origin kept the capability query
	if blob.Origin != clean {
		t.Logf("origin = %q, want %q", blob.Origin, clean)
		t.Fatalf("Blob.Origin kept the capability query")
	}

	failing := newFailingServer(t, http.StatusNotFound)
	_, err = newTestFetcher(false, helpers.SignatureMaxSize).
		FetchRequirementSource(t.Context(), failing.URL+"/"+sigLeafName+presignedQuery)
	if err == nil {
		t.Fatal("FetchRequirementSource error = nil, want a refusal from the 404 fixture")
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Fatalf("error message %q carries the capability query", err)
	}
}

// TestFetchNon200NamesOnlyTheStatus pins what a refusal from an answering
// server is allowed to say. The body is bytes chosen by whoever answered, so it
// is neither read nor rendered; the status code is what an operator can act on.
func TestFetchNon200NamesOnlyTheStatus(t *testing.T) {
	t.Parallel()

	srv := newFailingServer(t, http.StatusForbidden)

	_, err := newTestFetcher(false, helpers.SignatureMaxSize).
		FetchRequirementSource(t.Context(), srv.URL+"/"+sigLeafName)
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("FetchRequirementSource error = %v, want the unavailable sentinel", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error message %q does not name the status code", err)
	}
	if strings.Contains(err.Error(), serverBodyMarker) {
		t.Fatalf("error message %q carries the response body, which is never read", err)
	}
}

// TestFetchFileSizeCeiling covers the ceiling on the file path, on both sides
// of it. A file of exactly the ceiling is accepted and one byte more is
// refused, which is what makes the read one byte longer than the ceiling
// load-bearing rather than decorative.
func TestFetchFileSizeCeiling(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	exact := writeSourceFile(t, dir, "exact.asc", strings.Repeat("a", tinySourceLimit))
	over := writeSourceFile(t, dir, "over.asc", strings.Repeat("a", tinySourceLimit+1))

	blob, err := newTestFetcher(false, tinySourceLimit).FetchRequirementSource(t.Context(), "file://"+exact)
	if err != nil {
		t.Fatalf("a file of exactly the ceiling: error = %v, want nil", err)
	}
	if len(blob.Data) != tinySourceLimit {
		t.Fatalf("a file of exactly the ceiling: read %d bytes, want %d", len(blob.Data), tinySourceLimit)
	}

	// Reading only f.limit bytes instead of f.limit+1, applied through go test
	// -overlay so no production file is edited, fails here: the read stops at
	// the ceiling and reports a clean EOF, so the file is indistinguishable
	// from one that ends exactly there and is accepted.
	//
	//	source_test.go:517: a file one byte over the ceiling: error = <nil>, want the unavailable sentinel
	_, err = newTestFetcher(false, tinySourceLimit).FetchRequirementSource(t.Context(), "file://"+over)
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("a file one byte over the ceiling: error = %v, want the unavailable sentinel", err)
	}
}

// TestFetchHTTPSizeCeiling is the file ceiling's counterpart on the other path,
// and it is a separate test rather than a row because the two caps are
// implemented differently: helpers.NewSizeLimitedReader fails on the first byte
// past the ceiling, so nothing here reads one byte more the way the file path
// has to.
func TestFetchHTTPSizeCeiling(t *testing.T) {
	t.Parallel()

	exactSrv, _ := newSourceServer(t, strings.Repeat("a", tinySourceLimit))
	blob, err := newTestFetcher(false, tinySourceLimit).
		FetchRequirementSource(t.Context(), exactSrv.URL+"/"+sigLeafName)
	if err != nil {
		t.Fatalf("a body of exactly the ceiling: error = %v, want nil", err)
	}
	if len(blob.Data) != tinySourceLimit {
		t.Fatalf("a body of exactly the ceiling: read %d bytes, want %d", len(blob.Data), tinySourceLimit)
	}

	overSrv, _ := newSourceServer(t, strings.Repeat("a", tinySourceLimit+1))
	_, err = newTestFetcher(false, tinySourceLimit).
		FetchRequirementSource(t.Context(), overSrv.URL+"/"+sigLeafName)
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("a body one byte over the ceiling: error = %v, want the unavailable sentinel", err)
	}
}

// TestFetchFileRefusesEveryNonRegularShape covers the mode check made against
// the opened descriptor, with the positive control in the same table.
//
// The named-pipe row is the one that matters, and it is the only row that can
// fail this test in two entirely different ways, because two independent
// properties of one open are what make it pass. Both are quoted below, since
// each is a separate way to break the same line.
//
// The other two refusal rows are documentary rather than pinned, and the mode
// check's own deletion is what shows it: run with that check gone, only the
// named-pipe row fails. A directory descriptor cannot be read, and an absent
// path never opens at all, so each of those two reaches the same sentinel by a
// route the mode check plays no part in. They stay because the shapes are worth
// naming - and the directory row is the sole pin for the mode arm's MESSAGE in
// the test below - not because a mutation moves them here.
func TestFetchFileRefusesEveryNonRegularShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		build  func(t *testing.T, dir string) string
		name   string
		wantOK bool
	}{
		{name: "named pipe", build: buildNamedPipeSource},
		{name: "directory", build: buildDirectorySource},
		{name: "absent path", build: buildAbsentSource},
		{name: "positive control: regular file", build: buildRegularSource, wantOK: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source := "file://" + tc.build(t, t.TempDir())

			// Two mutations, applied through go test -overlay so no production
			// file is edited, both against fetchFile's open and both quoted as
			// they came.
			//
			// Dropping syscall.O_NONBLOCK from the open flags does not fail the
			// named-pipe row - it hangs it, in the blocking open that flag
			// exists to avoid, since open(O_RDONLY) on a FIFO waits for a
			// writer that never comes. The artifact is therefore a timeout
			// under an explicit -timeout rather than an assertion:
			//
			//	panic: test timed out after 20s
			//		running tests:
			//			TestFetchFileRefusesEveryNonRegularShape/named_pipe (20s)
			//
			// Deleting the mode check - the !info.Mode().IsRegular() half of
			// the condition below the open - fails that same row with an
			// assertion instead: a FIFO opened non-blocking with no writer
			// reads as a clean EOF, so the source is accepted as an empty
			// signature blob:
			//
			//	source_test.go:612: named pipe: error = <nil>, want the unavailable sentinel
			_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), source)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("positive control: FetchRequirementSource(%q) error = %v, want nil", source, err)
				}

				return
			}
			if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
				t.Fatalf("%s: error = %v, want the unavailable sentinel", tc.name, err)
			}
		})
	}
}

// TestFetchFileFailuresRenderOneMessage pins the file arm's collapsed
// vocabulary: four failures an attacker can select between - a path that is
// absent, one that cannot be opened, one that is not a regular file, and one
// whose bytes run past the ceiling - must be one observation, differing only in
// the path they name.
//
// A signature source is repository content, so a message that discriminated
// between these would be a filesystem oracle a repository could query about the
// machine running the install. One bit is intrinsic to the message and stays:
// whether the file was readable as a signature at all.
//
// The rows deliberately run at two different ceilings, since the over-ceiling
// row needs a small one, and the message must not vary with it either.
func TestFetchFileFailuresRenderOneMessage(t *testing.T) {
	t.Parallel()

	// The control runs at the smaller of the two ceilings, on a body that
	// exactly fills it: the harness is shown capable of acceptance, and the
	// ceiling is shown not to be what refuses the rows below.
	control := writeSourceFile(t, t.TempDir(), sigLeafName, strings.Repeat("a", tinySourceLimit))
	if _, err := newTestFetcher(false, tinySourceLimit).
		FetchRequirementSource(t.Context(), "file://"+control); err != nil {
		t.Fatalf("positive control: FetchRequirementSource(a regular file at the ceiling) error = %v, want nil", err)
	}

	cases := []struct {
		build func(t *testing.T, dir string) string
		name  string
		limit int64
	}{
		{name: "absent path", build: buildAbsentSource, limit: helpers.SignatureMaxSize},
		{name: "unreadable regular file", build: buildUnreadableSource, limit: helpers.SignatureMaxSize},
		{name: "directory", build: buildDirectorySource, limit: helpers.SignatureMaxSize},
		{name: "over the ceiling", build: buildOversizeSource, limit: tinySourceLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := tc.build(t, t.TempDir())

			// Restoring one distinguishable message - the open-failure arm
			// wrapping its own OS error again, as `fmt.Errorf("%w: %q: %w",
			// helpers.ErrSignatureSourceUnavailable, display, err)` - applied
			// through go test -overlay so no production file is edited, fails
			// the absent row and the unreadable row alike. The absent one,
			// quoted as it came:
			//
			//	source_test.go:686: absent path renders a message of its own
			//
			// Both messages go to the log rather than into the assertion: they
			// carry an OS-chosen temporary path no two runs agree on.
			//
			// What those two rows pin is that arm's MESSAGE rather than its
			// existence, and the difference is recorded once, here: deleting the
			// arm outright fails nothing in this package at all. os.OpenFile
			// hands back a nil *os.File alongside its error, File.Stat on a nil
			// receiver answers fs.ErrInvalid, and the arm below renders the
			// identical message from it. The rows claim one message for four
			// failures and that claim stays true either way, so this is a note
			// rather than a defect.
			_, err := newTestFetcher(false, tc.limit).FetchRequirementSource(t.Context(), "file://"+path)
			if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
				t.Fatalf("FetchRequirementSource(%q) error = %v, want the unavailable sentinel", path, err)
			}
			if got, want := err.Error(), unavailablePrefix+"file://"+path+unreadableSuffix; got != want {
				t.Logf("message = %q, want %q", got, want)
				t.Fatalf("%s renders a message of its own", tc.name)
			}
		})
	}
}

// buildNamedPipeSource makes dir/sig.asc a FIFO, skipping where the platform
// has none. It mirrors cleanup's own named-pipe fixture, which states the same
// blocking-open property this row exists for.
func buildNamedPipeSource(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, sigLeafName)
	if err := syscall.Mkfifo(path, helpers.FileMod); err != nil {
		t.Skipf("named pipes unavailable on this platform: %v", err)
	}

	return path
}

// buildDirectorySource makes dir/sig.asc a directory.
func buildDirectorySource(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, sigLeafName)
	if err := os.Mkdir(path, helpers.DirMod); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}

	return path
}

// buildAbsentSource names a leaf dir deliberately does not hold.
func buildAbsentSource(_ *testing.T, dir string) string {
	return filepath.Join(dir, sigLeafName)
}

// buildUnreadableSource writes a genuine signature file and takes every
// permission off it, which is the one shape that reaches the open failing on a
// path that is otherwise perfectly ordinary.
//
// It skips rather than fails where the mode does not bite - any run as a
// principal that bypasses it (root, or a platform whose permissions do not work
// this way) - since there the fixture cannot produce the condition at all.
func buildUnreadableSource(t *testing.T, dir string) string {
	t.Helper()

	path := writeSourceFile(t, dir, sigLeafName, sourceBlobBody)
	if err := os.Chmod(path, 0); err != nil {
		t.Skipf("chmod unavailable on this platform: %v", err)
	}
	if probe, err := os.Open(path); err == nil { //nolint:gosec // G304: the path is this test's own fixture
		_ = probe.Close()
		t.Skip("this principal reads a mode-0 file, so the fixture cannot produce an unreadable regular file")
	}

	return path
}

// buildOversizeSource writes one byte more than tinySourceLimit, the ceiling
// its row runs the fetcher at.
func buildOversizeSource(t *testing.T, dir string) string {
	t.Helper()

	return writeSourceFile(t, dir, sigLeafName, strings.Repeat("a", tinySourceLimit+1))
}

// buildRegularSource writes a genuine signature file, the shape every row above
// is contrasted against.
func buildRegularSource(t *testing.T, dir string) string {
	t.Helper()

	return writeSourceFile(t, dir, sigLeafName, sourceBlobBody)
}

// TestFetchFileURLUnderOffline and TestFetchHTTPURLUnderOfflineIsRefused are
// one pair: offline is about the network, so a local file must still be read
// and an http source must not be attempted. Either test alone would be
// satisfied by an implementation that got the other half backwards.
func TestFetchFileURLUnderOffline(t *testing.T) {
	t.Parallel()

	path := writeSourceFile(t, t.TempDir(), sigLeafName, sourceBlobBody)

	blob, err := newTestFetcher(true, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), "file://"+path)
	if err != nil {
		t.Fatalf("FetchRequirementSource(a file url, offline) error = %v, want nil", err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("data = %q, want the fixture blob", blob.Data)
	}
}

// TestFetchHTTPURLUnderOfflineIsRefused pins the refusal and where it is
// raised. The message assertion is the last of the three: the offline transport
// formats the request URL into its own message, so a refusal raised after the
// request had been composed would print the capability query this value carries.
//
// The positive control is the same URL, served by the same fixture, fetched by
// an online fetcher first - so the three refusals below are shown to be about
// offline mode rather than about a value that could never have been fetched at
// all. A control on a different fixture cannot say that, and neither can the
// current call graph: two producers of helpers.ErrOfflineMode are reachable
// from this call path, so "the check was never reached" is not reachable right
// now - a property of who raises the sentinel rather than one of this test.
func TestFetchHTTPURLUnderOfflineIsRefused(t *testing.T) {
	t.Parallel()

	srv, _ := newSourceServer(t, sourceBlobBody)
	source := srv.URL + "/" + sigLeafName + presignedQuery

	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), source)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(the same url, online) error = %v, want nil", err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}

	// Deleting the offline early return in fetchHTTP - so the already-offline
	// client's own transport produces the refusal instead - applied through go
	// test -overlay so no production file is edited, fails on the third
	// assertion below. The two above it still pass under that mutation, which
	// is what makes the third one pinnable rather than documentary: the
	// transport raises the same two sentinels, and only the message differs.
	//
	//	source_test.go:822: the offline refusal carries the capability query
	_, err = newTestFetcher(true, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), source)
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("FetchRequirementSource(an http url, offline) error = %v, want the offline sentinel", err)
	}
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("FetchRequirementSource(an http url, offline) error = %v, want the unavailable sentinel too", err)
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Logf("message = %q", err)
		t.Fatalf("the offline refusal carries the capability query")
	}
}

// TestFetchHTTPSSurfacesCertificateVerification covers the https arm of the
// scheme switch and, in the same assertion, the property the dedicated client
// exists for: httptest's own self-signed certificate is refused, because this
// client holds no relaxed TLS policy for any origin and cannot be handed one.
//
// The first assertion is what separates the two possible reasons for a failure:
// an unsupported-source sentinel would mean https never reached a request at
// all, so this row would prove nothing about the transport.
func TestFetchHTTPSSurfacesCertificateVerification(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sourceBlobBody))
	}))
	t.Cleanup(srv.Close)

	_, err := newTestFetcher(false, helpers.SignatureMaxSize).
		FetchRequirementSource(t.Context(), srv.URL+"/"+sigLeafName)
	if errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
		t.Fatalf("https url refused as unfetchable: %v", err)
	}
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("FetchRequirementSource(an https url with a self-signed certificate) error = %v, want the unavailable sentinel", err)
	}
	if !strings.Contains(err.Error(), "x509") {
		t.Fatalf("error = %v, want an x509 verification failure: this client must trust no self-signed certificate", err)
	}
}

// TestFetchHTTPSurfacesContextCancellation pins that a caller's own
// cancellation stays reachable through errors.Is. cmd/go-galaxy/exitcode checks
// context.Canceled ahead of every other class, so an implementation that
// rendered the cause instead of wrapping it would report an operator's Ctrl-C
// as a network failure.
func TestFetchHTTPSurfacesContextCancellation(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
	}))
	// LIFO: the handler is released before Close waits for it, so the fixture
	// cannot deadlock its own teardown.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()

	_, err := newTestFetcher(false, helpers.SignatureMaxSize).
		FetchRequirementSource(ctx, srv.URL+"/"+sigLeafName)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchRequirementSource(canceled mid-request) error = %v, want context.Canceled reachable", err)
	}
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("FetchRequirementSource(canceled mid-request) error = %v, want the unavailable sentinel too", err)
	}
}

// TestTransportCause covers the unwrap directly, because two of its three
// shapes are ones http.Client does not produce: it always wraps a transport
// failure in a *url.Error carrying a non-nil Err, so the pass-through arm is
// only reachable from here.
func TestTransportCause(t *testing.T) {
	t.Parallel()

	// A real sentinel rather than a fresh errors.New: what the caller wraps has
	// to stay matchable through errors.Is, and helpers.ErrOfflineMode is one of
	// the values cmd/go-galaxy/exitcode actually classifies on.
	inner := helpers.ErrOfflineMode
	cases := []struct {
		err  error
		want error
		name string
	}{
		{name: "not a url.Error", err: inner, want: inner},
		{name: "url.Error carrying no cause", err: &url.Error{Op: "Get", URL: "https://h/x?t=1"}, want: nil},
		{name: "url.Error carrying a cause", err: &url.Error{Op: "Get", URL: "https://h/x?t=1", Err: inner}, want: inner},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := transportCause(tc.err)
			want := tc.want
			if want == nil {
				want = tc.err
			}
			if !errors.Is(got, want) {
				t.Fatalf("transportCause(%v) = %v, want %v", tc.err, got, want)
			}
		})
	}
}

// TestFetchRequirementSourceRefusesAHostlessHTTPURL covers the three spellings
// url.Parse accepts while reporting neither an authority nor an opaque body, so
// that nothing but this refusal stands between them and a request naming no
// host to send it to.
//
// The exit class is asserted alongside the sentinel, because the class is the
// whole reason the refusal is raised here rather than left to the transport:
// http.Client answers such a URL with "no Host in request URL", which arrives
// under the unavailable sentinel and exits 4 - the network class, which a CI
// retries. A value no host was ever contacted for has to exit 2 instead, the
// class an operator clears by editing something.
//
// The positive control is the same fetcher against the fixture server: an http
// URL differing from every row below only in naming a host.
func TestFetchRequirementSourceRefusesAHostlessHTTPURL(t *testing.T) {
	t.Parallel()

	srv, _ := newSourceServer(t, sourceBlobBody)
	control := srv.URL + "/" + sigLeafName
	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), control)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(%q) error = %v, want nil", control, err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}

	cases := []struct {
		name   string
		source string
	}{
		{name: "https with an empty authority and a path", source: "https:///" + sigLeafName},
		{name: "https with nothing after the slashes", source: "https://"},
		{name: "http with nothing after the slashes", source: "http://"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Two mutations, applied through go test -overlay so no production
			// file is edited, one per assertion below.
			//
			// Narrowing the refusal back to opaque values alone - the
			// hostlessHTTP half of FetchRequirementSource's condition deleted -
			// fails every row on the first assertion, which is the defect this
			// test exists for: the value reaches http.Client, and its answer
			// arrives under the wrong sentinel and the wrong class:
			//
			//	source_test.go:988: error = collection signature source unavailable: "https:///sig.asc": http: no Host in request URL
			//	source_test.go:989: "https:///sig.asc" was not refused as an unsupported source
			//
			// Dropping helpers.ErrUnsupportedSignatureSource from
			// exitcode.isSignatureConfigError fails the second assertion
			// instead, with the first still passing - which is what makes the
			// class pinnable here rather than merely implied by the sentinel:
			//
			//	source_test.go:992: "https:///sig.asc" classified as exit 1, want ExitUsage (2)
			_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), tc.source)
			if !errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
				t.Logf("error = %v", err)
				t.Fatalf("%q was not refused as an unsupported source", tc.source)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitUsage {
				t.Fatalf("%q classified as exit %d, want ExitUsage (%d)", tc.source, got, exitcode.ExitUsage)
			}
		})
	}
}

// TestFetchFileNamesThePathItOpened covers the fragment cut on the one arm
// where it decides more than what a message reads like. url.Parse splits a
// fragment off before the Path fetchFile opens, so "file://<dir>/a#b.asc" opens
// <dir>/a - and a display that kept the fragment would name a file this run
// never touched, in Blob.Origin and in the file arm's one failure message
// alike. The second is what the collapsed vocabulary's own justification rests
// on: an operator is expected to reproduce the distinction with one ls -l, and
// cannot if the path named is not the path opened.
//
// The first subtest puts a different body at the leaf the source spells out in
// full, so which file was read is visible in the data rather than only in the
// message. The two subtests are separate chains rather than one, because each
// asserts the same property through a different sink and neither should be able
// to hide the other's failure behind an earlier t.Fatalf.
func TestFetchFileNamesThePathItOpened(t *testing.T) {
	t.Parallel()

	// The leaf a fragment-carrying source spells out in full, and the part of
	// it url.Parse keeps as the Path: everything from the "#" on is a fragment
	// that never reaches the open.
	const (
		baseLeaf = "a"
		fullLeaf = baseLeaf + "#b.asc"
		// decoyBody sits at fullLeaf, so a read that reached that file rather
		// than baseLeaf fails on the bytes before it fails on the name.
		decoyBody = "decoy-body-that-must-not-be-read"
	)

	t.Run("the file that is opened", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		opened := writeSourceFile(t, dir, baseLeaf, sourceBlobBody)
		writeSourceFile(t, dir, fullLeaf, decoyBody)

		blob, err := newTestFetcher(false, helpers.SignatureMaxSize).
			FetchRequirementSource(t.Context(), "file://"+filepath.Join(dir, fullLeaf))
		if err != nil {
			t.Fatalf("FetchRequirementSource(a file url carrying a fragment) error = %v, want nil", err)
		}
		if string(blob.Data) != sourceBlobBody {
			t.Fatalf("data = %q, want the bytes of the file named before the fragment", blob.Data)
		}

		// Dropping helpers.WithoutFragment from the display composition in
		// FetchRequirementSource, applied through go test -overlay so no
		// production file is edited, fails this subtest here and the one below
		// on its own message assertion. The rendered values go to the log
		// rather than into either message, since each carries an OS-chosen
		// temporary path no two runs agree on, so only the assertion line is
		// quotable:
		//
		//	source_test.go:1053: Blob.Origin names a path other than the one that was opened
		if blob.Origin != "file://"+opened {
			t.Logf("origin = %q, want %q", blob.Origin, "file://"+opened)
			t.Fatalf("Blob.Origin names a path other than the one that was opened")
		}
	})

	t.Run("the message that names it", func(t *testing.T) {
		t.Parallel()

		absent := filepath.Join(t.TempDir(), baseLeaf)

		// The same mutation quoted above fails this subtest too, on the message
		// rather than on the origin, and the same OS-chosen path keeps its own
		// rendering out of that quote:
		//
		//	source_test.go:1074: the message names a path other than the one that was opened
		_, err := newTestFetcher(false, helpers.SignatureMaxSize).
			FetchRequirementSource(t.Context(), "file://"+absent+"#b.asc")
		if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
			t.Fatalf("FetchRequirementSource(an absent path carrying a fragment) error = %v, want the unavailable sentinel", err)
		}
		if got, want := err.Error(), unavailablePrefix+"file://"+absent+unreadableSuffix; got != want {
			t.Logf("message = %q, want %q", got, want)
			t.Fatalf("the message names a path other than the one that was opened")
		}
	})
}
