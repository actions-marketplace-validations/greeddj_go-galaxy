package collections_test

// This file exercises `outdated` end to end against both a real fake Galaxy
// server (fakegalaxy) and a raw, hand-rolled HTTP/1.1 responder that answers
// a request with an attacker-controlled status-line reason phrase - the one
// shape fakegalaxy cannot produce, since http.ResponseWriter's own status
// line is never operator-influenced. Together with dry_run_e2e_test.go's
// TestOutdatedDryRunMutatesNothing, this is the suite proving outdated's
// report is sanitized on the same boundary as every other operator-facing
// line, honors --metrics-file (including under --dry-run, where the shared
// writeRunMetrics guard suppresses it), discloses every flag it cannot
// honor, and classifies its own failures onto the documented exit codes.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// rawStatusLineServer answers every request on a fresh connection with a
// fixed, verbatim HTTP/1.1 status line - including a reason phrase this test
// controls byte for byte - followed by an empty, non-cached JSON-shaped
// body. It exists because fakegalaxy always renders its status line through
// net/http's own http.ResponseWriter, which never lets a caller put an
// arbitrary byte sequence into the reason phrase; only a raw net.Listen
// responder can produce the exact wire bytes a hostile or badly configured
// real Galaxy deployment could.
type rawStatusLineServer struct {
	listener net.Listener
	url      string
}

// newRawStatusLineServer starts the responder and registers its shutdown via
// t.Cleanup. statusLine is written verbatim as the response's first line
// (e.g. "HTTP/1.1 404 <reason phrase>"), terminated with the server's own
// "\r\n" plus a Content-Length: 0 and Connection: close pair, so every
// accepted connection serves exactly one request before it is closed - a
// new connection is required for each subsequent one, which
// http.Transport's own dialer handles transparently for the client under
// test.
func newRawStatusLineServer(t *testing.T, statusLine string) *rawStatusLineServer {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := &rawStatusLineServer{listener: ln, url: "http://" + ln.Addr().String()}
	go srv.acceptLoop(statusLine)
	t.Cleanup(func() { _ = ln.Close() })
	return srv
}

// acceptLoop serves connections until the listener is closed by the test's
// t.Cleanup, at which point Accept returns an error and the loop exits.
func (s *rawStatusLineServer) acceptLoop(statusLine string) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go serveOneRawStatusLine(conn, statusLine)
	}
}

// serveOneRawStatusLine reads exactly one HTTP request off conn - discarding
// it, since every test using this fixture cares only about the response -
// then writes the fixed status line and closes the connection. A malformed
// or absent request (e.g. the client gave up before sending headers) is
// answered with nothing further; the connection close alone is enough to
// unblock a caller waiting on it.
func serveOneRawStatusLine(conn net.Conn, statusLine string) {
	defer func() { _ = conn.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	_ = req.Body.Close()
	_, _ = conn.Write([]byte(statusLine + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// saveOutdatedLockfile writes a minimal lockfile at requirementsFile's
// default lockfile path, containing exactly the given entries, each sourced
// from server - the shape every outdated e2e fixture in this file needs
// before it can call collections.Outdated.
func saveOutdatedLockfile(t *testing.T, requirementsFile, server string, entries ...lockfile.Entry) string {
	t.Helper()
	lockPath := lockfile.ResolveDefaultPath(requirementsFile, "")
	for i := range entries {
		if entries[i].Source == "" {
			entries[i].Source = server
		}
	}
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        server,
		Collections:   entries,
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	return lockPath
}

// TestOutdatedSanitizesServerReasonPhrase proves a server's HTTP reason
// phrase - here carrying a screen-clear sequence, a cursor-home sequence, a
// color-set sequence, and a bell, exactly the payload a real terminal would
// act on - cannot reach the operator's terminal as raw control bytes. It
// lands on stderr specifically (a lookup failure is a diagnostic, kept off
// stdout), and the printable substring inside the payload still appears once
// sanitized - the positive control proving the message was printed and
// cleaned, not merely swallowed.
//
// The two sanitization assertions below are exact, not a sample of the
// injected bytes, and neither can be "every byte this test injects is
// individually absent": progress.Progress decorates every result-tier line
// with its own SGR escape codes unconditionally, not only on a terminal, so
// a blanket "stderr carries zero ESC bytes" assertion cannot hold there -
// see progress.go's ansiRed/ansiGreen/ansiReset. Instead:
//   - bytes.Count(stderr, U+FFFD) == 4 counts the replacement character
//     itself, one per injected C0 byte (the three ESC bytes opening the
//     screen-clear, cursor-home, and color-set sequences, plus the one BEL).
//     No prefix this program's printer emits ever contains U+FFFD, so this
//     single count catches every injected byte at once - including the
//     color-set sequence's own ESC, which a bytes.Contains check for it
//     could never assert on its own, since the printer's own ansiGreen
//     ("\x1b[1m\x1b[32m") contains that exact byte sequence as a legitimate
//     substring.
//   - stdout carries zero ESC bytes at all: in this fixture stdout holds
//     only the markerless PersistentPrintf summary line, which the printer
//     never colors, so this is both achievable and a real assertion.
//
// Deliberately not t.Parallel(): it drives a real progress.Progress through
// captureStdIO, which swaps the process-wide os.Stdout/os.Stderr for its
// duration - see captureStdIO's own doc comment in token_leak_e2e_test.go
// for why every test doing that in this package runs un-parallelized.
//
// Mutation: reverting reportOutdated's failure-line Errorf call to a bare
// fmt.Printf makes the replacement-count assertion fail with `stderr
// carries 0 U+FFFD replacement characters, want 4: ""` (every byte moved to
// stdout, so stderr is empty), the ESC-on-stdout assertion fail with
// `stdout contains a raw ESC byte at index 67: "Lookup failed:
// \"acme.widgets\"@1.0.0: failed to fetch metadata: 404 \x1b[2J\x1b[1;1H
// \x1b[32mEVERYTHING IS UP TO DATE\a (http://127.0.0.1:60058/api/collections/
// acme/widgets)\n.../requirements.lock.yml: 0 up to date, 0 outdated, 1
// failed\n"`, and the two stream-separation assertions plus the positive
// control fail as well ("expected the failure line to stay off stdout",
// "expected the failure line on stderr, got stderr=\"\"", and "expected the
// sanitized reason phrase text to still be printed, got stderr=\"\"") - run
// and confirmed.
func TestOutdatedSanitizesServerReasonPhrase(t *testing.T) {
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	raw := newRawStatusLineServer(t, "HTTP/1.1 404 \x1b[2J\x1b[1;1H\x1b[32mEVERYTHING IS UP TO DATE\a")
	saveOutdatedLockfile(t, reqPath, raw.url, lockfile.Entry{Name: "acme.widgets", Version: "1.0.0"})

	cfg := &config.Config{
		Server:           raw.url,
		RequirementsFile: reqPath,
		Workers:          1,
		Timeout:          e2eTimeout,
	}

	var outErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, fetch.New(cfg.Timeout, nil))
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})
	if outErr == nil {
		t.Fatal("expected an error from the failed lookup")
	}

	const wantReplacements = 4 // three ESC (screen-clear, cursor-home, color-set) plus one BEL
	if got := bytes.Count(stderr, []byte("�")); got != wantReplacements {
		t.Errorf("stderr carries %d U+FFFD replacement characters, want %d: %q", got, wantReplacements, stderr)
	}
	if idx := bytes.IndexByte(stdout, 0x1b); idx >= 0 {
		t.Errorf("stdout contains a raw ESC byte at index %d: %q", idx, stdout)
	}
	if bytes.Contains(stdout, []byte("Lookup failed")) {
		t.Errorf("expected the failure line to stay off stdout, got stdout=%q", stdout)
	}
	if !bytes.Contains(stderr, []byte("Lookup failed")) {
		t.Errorf("expected the failure line on stderr, got stderr=%q", stderr)
	}
	if !bytes.Contains(stderr, []byte("EVERYTHING IS UP TO DATE")) {
		t.Errorf("expected the sanitized reason phrase text to still be printed, got stderr=%q", stderr)
	}
}

// TestOutdatedSanitizesLockfileEntryName mirrors
// TestOutdatedSanitizesServerReasonPhrase for the report's other untrusted
// channel: a lockfile entry's own Name. helpers.SplitFQDN never validates
// name for character class - it checks only that splitting on "." yields
// exactly two non-empty halves - so a name carrying a raw control byte
// reaches reportOutdated unfiltered once lockfile.Load has parsed it off
// disk, through either of two different code paths that both end at the
// identical Errorf("Lookup failed: %q@%s: %s", ...) line: a name that does
// split into two parts and then fails its network lookup
// (testOutdatedSanitizesTwoPartHostileName), and a name that fails the
// split itself and never reaches the network at all
// (testOutdatedSanitizesMultiDotHostileName).
//
// The hostile byte in both subtests is a validly UTF-8-encoded C1 control
// character (U+009B, CSI), not a raw ESC or BEL, and deliberately so: the
// first subtest's name has to survive a real HTTP request, and net/url.Parse
// rejects any raw byte below 0x20 or equal to 0x7F before a request is even
// built ("net/url: invalid control character in URL", confirmed against the
// stdlib source) - so a name carrying either of those two would never reach
// the network, and that subtest would then be pinning a URL-construction
// failure instead of the sanitization property it exists to prove. A C1
// character encoded as valid UTF-8 has no byte below 0x80, so it survives
// request construction. The CSI rune is built from its numeric code point
// (rune(0x9b)) rather than placed directly in a string literal, so the
// source file carries no raw or escaped control character for a linter (or
// a human diffing this file) to trip over.
//
// The two subtests pin different defenses, not the same one twice: the
// two-part subtest's replacement count comes from r.Err and so is the only
// evidence in this file that safeout.Clean's own replacement actually fires
// for outdated's report, while the multi-dot subtest's zero-replacement
// count pins %q instead and would still pass even if Clean were removed
// from Errorf entirely. They must be read together for that reason: if a
// future edit made the two-part subtest's count coincidentally match the
// multi-dot subtest's, updating its "want" constant to keep both green
// would delete the only proof left that Clean's replacement fires here, and
// nothing in this file would flag that loss.
//
// Deliberately not t.Parallel(); see
// TestOutdatedSanitizesServerReasonPhrase's own doc comment for why.
//
// Mutation: reverting reportOutdated's failure-line Errorf call to a bare
// fmt.Printf kills both subtests. The first fails with `stderr carries 0
// U+FFFD replacement characters, want 1: ""`, `expected the failure line to
// stay off stdout, got stdout="Lookup failed: \"acme.\\u009bwidgets\"@1.0.0:
// failed to fetch metadata: 404 Not Found (http://127.0.0.1:.../api/
// collections/acme/\u009bwidgets)\n..."` - the double backslash before the
// first "u009b" is not a transcription error: reportOutdated's own %q on
// r.Name has already turned the raw CSI byte into the six literal characters
// \u009b before t.Errorf's own %q re-quotes the whole captured stdout for
// display, escaping that literal backslash a second time, while the second
// occurrence, reached through r.Err and never passed through %q by
// reportOutdated itself, is still a real control codepoint at that point and
// so is escaped only once - and the stream-separation and positive-control
// assertions fail alongside it. The second subtest fails with `expected the
// failure line to stay off stdout, got stdout="Lookup failed:
// \"acme.\\u009b.widgets\"@1.0.0: lockfile is invalid: invalid name
// \"acme.\\u009b.widgets\"\n..."` - both occurrences double-backslashed here,
// since lookupOutdated's own %q on e.Name already escaped the second one too
// before t.Errorf's own %q ever saw it - with the same stream-separation,
// lockfile-cause, and positive-control assertions failing alongside it - run
// and confirmed.
func TestOutdatedSanitizesLockfileEntryName(t *testing.T) {
	// t.Run, not t.Parallel(): both subtests drive captureStdIO, which swaps
	// the process-wide os.Stdout/os.Stderr, and Go only runs subtests
	// concurrently when they call t.Parallel() themselves - neither does, so
	// they already run one after the other under the parent's own serial
	// execution.
	t.Run("two-part name survives SplitFQDN and 404s", testOutdatedSanitizesTwoPartHostileName)
	t.Run("multi-dot name fails SplitFQDN before any network request", testOutdatedSanitizesMultiDotHostileName)
}

// testOutdatedSanitizesTwoPartHostileName is
// TestOutdatedSanitizesLockfileEntryName's first subtest, split into its own
// top-level function purely to stay under the cyclomatic-complexity budget
// alongside its sibling below.
//
// acme.<CSI>widgets is never registered, so its root-metadata lookup 404s:
// this exercises lookupOutdated's network-failure arm, not its SplitFQDN
// guard.
func testOutdatedSanitizesTwoPartHostileName(t *testing.T) {
	const csi = rune(0x9b)
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	hostileName := "acme." + string(csi) + "widgets"
	saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: hostileName, Version: "1.0.0"})

	cfg := &config.Config{
		Server:           s.URL(),
		RequirementsFile: reqPath,
		Workers:          1,
	}

	var outErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, s.Client())
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})
	if outErr == nil {
		t.Fatal("expected an error from the failed lookup")
	}

	// reportOutdated renders r.Name with %q, which is fmt's own escaping,
	// not safeout.Clean's: it turns the raw CSI byte into the printable
	// text "\u009b" before Clean ever sees that occurrence, so Clean finds
	// nothing left to replace there. The other occurrence - the same
	// split-name half echoed inside the 404's own *cacheManager.HTTPStatusError
	// message, reached through reportOutdated's %s on r.Err - is never
	// passed through %q at all, so it stays a raw byte until Clean
	// replaces it. One occurrence pre-neutralized by %q plus one replaced
	// by Clean is why the count below is 1, not 2.
	const wantReplacements = 1
	if got := bytes.Count(stderr, []byte("�")); got != wantReplacements {
		t.Errorf("stderr carries %d U+FFFD replacement characters, want %d: %q", got, wantReplacements, stderr)
	}
	if bytes.Contains(stdout, []byte("Lookup failed")) {
		t.Errorf("expected the failure line to stay off stdout, got stdout=%q", stdout)
	}
	if !bytes.Contains(stderr, []byte("Lookup failed")) {
		t.Errorf("expected the failure line on stderr, got stderr=%q", stderr)
	}
	// Positive control: the name's printable substrings survive
	// sanitization, proving the name was printed and cleaned rather than
	// dropped entirely.
	if !bytes.Contains(stderr, []byte("acme.")) || !bytes.Contains(stderr, []byte("widgets")) {
		t.Errorf("expected the sanitized name's printable substrings to still be printed, got stderr=%q", stderr)
	}
}

// testOutdatedSanitizesMultiDotHostileName is
// TestOutdatedSanitizesLockfileEntryName's second subtest: three dot-
// separated parts, not two, so helpers.SplitFQDN rejects this name outright
// and lookupOutdated returns its own fmt.Errorf("%w: invalid name %q",
// helpers.ErrLockfileInvalid, e.Name) without ever building a URL or
// reaching the fake server.
func testOutdatedSanitizesMultiDotHostileName(t *testing.T) {
	const csi = rune(0x9b)
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	hostileName := "acme." + string(csi) + ".widgets"
	saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: hostileName, Version: "1.0.0"})

	cfg := &config.Config{
		Server:           s.URL(),
		RequirementsFile: reqPath,
		Workers:          1,
	}

	var outErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, s.Client())
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})
	if outErr == nil {
		t.Fatal("expected an error from the invalid lockfile entry name")
	}

	if got := s.Total(); got != 0 {
		t.Errorf("fake server saw %d requests, want 0: SplitFQDN must reject this name before any network call", got)
	}
	// Both occurrences of the hostile byte on this line go through %q
	// before Clean ever runs: reportOutdated's own %q on r.Name, and
	// lookupOutdated's own %q on e.Name when it built r.Err in the first
	// place. Fmt's escaping already turned the raw byte into printable
	// text at both sites, so Clean has nothing left to replace - the
	// count is 0, not a lower positive number. A 0-vs-N assertion is
	// still a real sanitization proof: it demonstrates the byte never
	// reaches Clean in raw form on this path at all, which is a stronger
	// property than "Clean replaced it", not a weaker one.
	const wantReplacements = 0
	if got := bytes.Count(stderr, []byte("�")); got != wantReplacements {
		t.Errorf("stderr carries %d U+FFFD replacement characters, want %d: %q", got, wantReplacements, stderr)
	}
	if bytes.Contains(stdout, []byte("Lookup failed")) {
		t.Errorf("expected the failure line to stay off stdout, got stdout=%q", stdout)
	}
	if !bytes.Contains(stderr, []byte("Lookup failed")) {
		t.Errorf("expected the failure line on stderr, got stderr=%q", stderr)
	}
	if !bytes.Contains(stderr, []byte(helpers.ErrLockfileInvalid.Error())) {
		t.Errorf("expected the lockfile-invalid cause on stderr, got stderr=%q", stderr)
	}
	// Positive control: the name's printable substrings survive
	// sanitization (here, fmt's own %q escaping) rather than being
	// dropped entirely.
	if !bytes.Contains(stderr, []byte("acme.")) || !bytes.Contains(stderr, []byte("widgets")) {
		t.Errorf("expected the sanitized name's printable substrings to still be printed, got stderr=%q", stderr)
	}
}

// TestOutdatedQuietStillReportsAndSplitsStreams proves every one of
// reportOutdated's four lines is result tier: --quiet suppresses none of
// them, and a failed lookup still lands on stderr while the other three
// stay on stdout.
//
// Deliberately not t.Parallel(); see
// TestOutdatedSanitizesServerReasonPhrase's own doc comment for why.
func TestOutdatedQuietStillReportsAndSplitsStreams(t *testing.T) {
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "current", "1.0.0", nil)
	s.AddVersion("acme", "stale", "1.0.0", nil)
	s.AddVersion("acme", "stale", "2.0.0", nil)
	// acme.missing is never registered, so its root-metadata lookup 404s.

	lockPath := saveOutdatedLockfile(t, reqPath, s.URL(),
		lockfile.Entry{Name: "acme.current", Version: "1.0.0"},
		lockfile.Entry{Name: "acme.stale", Version: "1.0.0"},
		lockfile.Entry{Name: "acme.missing", Version: "1.0.0"},
	)

	cfg := &config.Config{
		Server:           s.URL(),
		RequirementsFile: reqPath,
		Workers:          2,
		Quiet:            true,
	}

	var outErr error
	stdout, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, s.Client())
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})
	if outErr == nil {
		t.Fatal("expected an error from the failed acme.missing lookup")
	}

	if !bytes.Contains(stdout, []byte("Up to date: acme.current@1.0.0")) {
		t.Errorf("expected the up-to-date line on stdout despite --quiet, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte("Outdated: acme.stale 1.0.0 -> 2.0.0")) {
		t.Errorf("expected the outdated line on stdout despite --quiet, got stdout=%q", stdout)
	}
	if !bytes.Contains(stdout, []byte(lockPath)) {
		t.Errorf("expected the summary line on stdout despite --quiet, got stdout=%q", stdout)
	}
	if bytes.Contains(stdout, []byte("Lookup failed")) {
		t.Errorf("expected the failure line to stay off stdout even under --quiet, got stdout=%q", stdout)
	}
	if !bytes.Contains(stderr, []byte(`Lookup failed: "acme.missing"@1.0.0`)) {
		t.Errorf("expected the failure line on stderr, got stderr=%q", stderr)
	}
}

// outdatedMetricsFixture builds the three-entry lockfile (one up to date,
// one outdated, one that 404s) shared by TestOutdatedWritesMetricsReport and
// its dry-run counterpart, returning the lockfile path and a *config.Config
// with RequirementsFile and Server already set.
func outdatedMetricsFixture(t *testing.T) (*fakegalaxy.Server, *config.Config, string) {
	t.Helper()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "current", "1.0.0", nil)
	s.AddVersion("acme", "stale", "1.0.0", nil)
	s.AddVersion("acme", "stale", "2.0.0", nil)

	lockPath := saveOutdatedLockfile(t, reqPath, s.URL(),
		lockfile.Entry{Name: "acme.current", Version: "1.0.0"},
		lockfile.Entry{Name: "acme.stale", Version: "1.0.0"},
		lockfile.Entry{Name: "acme.missing", Version: "1.0.0"},
	)

	cfg := &config.Config{
		Server:           s.URL(),
		RequirementsFile: reqPath,
		Workers:          2,
	}
	return s, cfg, lockPath
}

// TestOutdatedWritesMetricsReport proves outdated honors --metrics-file: the
// report's collections/failures fields describe the run's own work (three
// lookups, one failure), cache_hits/cache_misses/bytes_downloaded are a
// truthful 0/0/0 since outdated never touches an ArtifactStore, lockfile and
// lockfile_hash are populated, and frozen is absent even though cfg.Frozen
// is set - proving writeRunMetrics is called with a literal false rather
// than cfg.Frozen.
//
// Mutation: passing cfg.Frozen instead of a literal false to writeRunMetrics
// makes the frozen-absence assertion below fail with `expected "frozen" to
// be absent from the report despite cfg.Frozen, got true` - run and
// confirmed.
func TestOutdatedWritesMetricsReport(t *testing.T) {
	t.Parallel()
	s, cfg, lockPath := outdatedMetricsFixture(t)
	metricsPath := filepath.Join(t.TempDir(), "metrics.json")
	cfg.MetricsFile = metricsPath
	// Set despite outdated never honoring it, specifically to prove the
	// report's own "frozen" field stays absent regardless.
	cfg.Frozen = true

	runtime := infra.New(noopPrinter{}, s.Client())
	if err := collections.Outdated(context.Background(), cfg, runtime); err == nil {
		t.Fatal("expected an error from the failed acme.missing lookup")
	}

	report := readMetricsReport(t, metricsPath)
	assertOutdatedMetricsReport(t, report, lockPath)
}

// readMetricsReport reads and decodes the JSON metrics report at path into a
// generic map, so a test can assert individual fields (including a field's
// deliberate absence, which a typed metrics.Report struct would hide behind
// its own zero value) without depending on the full Report shape.
func readMetricsReport(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own temp dir.
	if err != nil {
		t.Fatalf("read metrics file: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal metrics report: %v", err)
	}
	return report
}

// assertOutdatedMetricsReport checks every field TestOutdatedWritesMetricsReport
// cares about, split out of that test purely to stay under the
// cyclomatic-complexity budget: the two together still cover the same set of
// assertions.
func assertOutdatedMetricsReport(t *testing.T, report map[string]any, lockPath string) {
	t.Helper()
	if got := report["command"]; got != "outdated" {
		t.Errorf("command = %v, want %q", got, "outdated")
	}
	if got := report["collections"]; got != float64(3) {
		t.Errorf("collections = %v, want 3", got)
	}
	if got := report["failures"]; got != float64(1) {
		t.Errorf("failures = %v, want 1", got)
	}
	if got, ok := report["frozen"]; ok {
		t.Errorf("expected \"frozen\" to be absent from the report despite cfg.Frozen, got %v", got)
	}
	if got := report["cache_hits"]; got != float64(0) {
		t.Errorf("cache_hits = %v, want 0", got)
	}
	if got := report["cache_misses"]; got != float64(0) {
		t.Errorf("cache_misses = %v, want 0", got)
	}
	if got := report["bytes_downloaded"]; got != float64(0) {
		t.Errorf("bytes_downloaded = %v, want 0", got)
	}
	if got, _ := report["lockfile"].(string); got != lockPath {
		t.Errorf("lockfile = %q, want %q", got, lockPath)
	}
	if got, _ := report["lockfile_hash"].(string); got == "" {
		t.Error("expected a non-empty lockfile_hash")
	}
}

// TestOutdatedDryRunSuppressesMetricsReport proves --dry-run's only real
// effect on outdated: writeRunMetrics' own shared cfg.DryRun guard suppresses
// the report and warns naming the skipped path, exactly as it does for
// install/warm/lock - outdated grows no cfg.DryRun branch of its own.
//
// Deliberately not t.Parallel(); see
// TestOutdatedSanitizesServerReasonPhrase's own doc comment for why.
func TestOutdatedDryRunSuppressesMetricsReport(t *testing.T) {
	s, cfg, _ := outdatedMetricsFixture(t)
	metricsPath := filepath.Join(t.TempDir(), "metrics.json")
	cfg.MetricsFile = metricsPath
	cfg.DryRun = true

	var outErr error
	_, stderr := captureStdIO(t, func() {
		printer := progress.New(cfg.Verbose, cfg.Quiet)
		defer printer.Close()
		runtime := infra.New(printer, s.Client())
		outErr = collections.Outdated(context.Background(), cfg, runtime)
	})
	if outErr == nil {
		t.Fatal("expected an error from the failed acme.missing lookup")
	}

	if _, statErr := os.Stat(metricsPath); !os.IsNotExist(statErr) {
		t.Errorf("expected no metrics file written under --dry-run, stat error = %v", statErr)
	}
	if !bytes.Contains(stderr, []byte(metricsPath)) {
		t.Errorf("expected a stderr warning naming the skipped metrics path, got stderr=%q", stderr)
	}
}

// TestOutdatedWarnsAboutUnhonoredFlags proves warnUnhonoredFlags fires
// exactly once, naming every configured flag outdated cannot honor, and
// changes nothing about the run itself: the same server sees the identical
// number of requests whether or not the flags are set. The "none set" run
// is this test's positive control, proving the warning is conditional
// rather than unconditionally printed.
//
// Deliberately not t.Parallel(); see
// TestOutdatedSanitizesServerReasonPhrase's own doc comment for why.
func TestOutdatedWarnsAboutUnhonoredFlags(t *testing.T) {
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "widgets", "1.0.0", nil)
	saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: "acme.widgets", Version: "1.0.0"})

	newCfg := func() *config.Config {
		return &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}
	}
	run := func(cfg *config.Config) ([]byte, []byte) {
		var err error
		stdout, stderr := captureStdIO(t, func() {
			printer := progress.New(cfg.Verbose, cfg.Quiet)
			defer printer.Close()
			runtime := infra.New(printer, s.Client())
			err = collections.Outdated(context.Background(), cfg, runtime)
		})
		if err != nil {
			t.Fatalf("Outdated: %v", err)
		}
		return stdout, stderr
	}

	_, cleanStderr := run(newCfg())
	cleanTotal := s.Total()
	if bytes.Contains(cleanStderr, []byte("does not honor")) {
		t.Errorf("expected no unhonored-flags warning for a clean config, got stderr=%q", cleanStderr)
	}

	s.ResetCounts()
	flaggedCfg := newCfg()
	flaggedCfg.Refresh = true
	flaggedCfg.NoCache = true
	_, flaggedStderr := run(flaggedCfg)
	flaggedTotal := s.Total()

	warningLines := 0
	for line := range bytes.SplitSeq(flaggedStderr, []byte("\n")) {
		if bytes.Contains(line, []byte("does not honor")) {
			warningLines++
		}
	}
	if warningLines != 1 {
		t.Errorf("expected exactly one unhonored-flags warning line, got %d in stderr=%q", warningLines, flaggedStderr)
	}
	if !bytes.Contains(flaggedStderr, []byte("--refresh")) {
		t.Errorf("expected the warning to name --refresh, got stderr=%q", flaggedStderr)
	}
	if !bytes.Contains(flaggedStderr, []byte("--no-cache")) {
		t.Errorf("expected the warning to name --no-cache, got stderr=%q", flaggedStderr)
	}
	if flaggedTotal != cleanTotal {
		t.Errorf("server saw %d requests with --refresh --no-cache set, %d with neither; expected them equal", flaggedTotal, cleanTotal)
	}
}

// TestOutdatedFailedLookupExitCode pins outdated's exit-code classification
// for its two distinct lookup-failure shapes: a plain 404 (the network
// class) and a lockfile entry whose name is not a "namespace.name" FQDN
// (the lockfile class, which must win even though both failures are joined
// behind the identical helpers.ErrLatestVersionLookupFailed headline).
func TestOutdatedFailedLookupExitCode(t *testing.T) {
	t.Parallel()

	t.Run("404 lookup classifies ExitNetwork", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		reqPath := filepath.Join(root, "requirements.yml")
		s := fakegalaxy.New(t)
		saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: "acme.missing", Version: "1.0.0"})

		cfg := &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}
		runtime := infra.New(noopPrinter{}, s.Client())
		err := collections.Outdated(context.Background(), cfg, runtime)
		if err == nil {
			t.Fatal("expected an error from the 404 lookup")
		}
		if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
			t.Errorf("exitcode.FromError(err) = %d, want ExitNetwork (%d); err=%v", got, exitcode.ExitNetwork, err)
		}
	})

	t.Run("invalid lockfile entry name classifies ExitLock", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		reqPath := filepath.Join(root, "requirements.yml")
		s := fakegalaxy.New(t)
		saveOutdatedLockfile(t, reqPath, s.URL(), lockfile.Entry{Name: "nodothere", Version: "1.0.0"})

		cfg := &config.Config{Server: s.URL(), RequirementsFile: reqPath, Workers: 1}
		runtime := infra.New(noopPrinter{}, s.Client())
		err := collections.Outdated(context.Background(), cfg, runtime)
		if err == nil {
			t.Fatal("expected an error from the invalid lockfile entry name")
		}
		if got := exitcode.FromError(err); got != exitcode.ExitLock {
			t.Errorf("exitcode.FromError(err) = %d, want ExitLock (%d); err=%v", got, exitcode.ExitLock, err)
		}
	})
}
