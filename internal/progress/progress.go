package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

const (
	ansiRed     = "\x1b[1m\x1b[31m"
	ansiGreen   = "\x1b[1m\x1b[32m"
	ansiYellow  = "\x1b[1m\x1b[33m"
	ansiReset   = "\x1b[1m\x1b[0m"
	okGlyph     = "✔"
	failGlyph   = "✗"
	warnGlyph   = "!"
	debugPrefix = "🚧 Debug: "
)

// Environment variables that override the terminal check, in the precedence
// this package applies: envNoColor first, then either force variable.
const (
	// envNoColor disables color whatever the destination is, per no-color.org:
	// present and non-empty is enough, whatever the value.
	envNoColor = "NO_COLOR"
	// envClicolorForce and envForceColor each enable color even when the
	// destination is not a terminal, which is what makes color usable in a CI
	// whose log viewer renders it. A literal "0" is not a request to force -
	// it is the conventional way to say "do not force" - so it falls through
	// to the terminal check rather than disabling color outright.
	envClicolorForce = "CLICOLOR_FORCE"
	envForceColor    = "FORCE_COLOR"
)

// stream is one destination together with whether lines written to it may
// carry color. The two are bound together because the answer differs per
// destination: `go-galaxy install > install.log` leaves stderr a terminal
// while stdout is a file, and a single process-wide decision would get one of
// the two wrong.
type stream struct {
	w     io.Writer
	color bool
}

// ok, fail and warn return the marker prefix for this stream, colored only
// when this destination accepts color.
func (s stream) ok() string   { return marker(okGlyph, ansiGreen, s.color) }
func (s stream) fail() string { return marker(failGlyph, ansiRed, s.color) }
func (s stream) warn() string { return marker(warnGlyph, ansiYellow, s.color) }

// marker builds one status prefix: the glyph, wrapped in seq and a full reset
// when colored, and the single trailing space every marker carries either way.
// The reset always precedes the payload, so no line can leave color state
// applied to text that follows it.
func marker(glyph, seq string, colored bool) string {
	if !colored {
		return glyph + " "
	}
	return seq + glyph + ansiReset + " "
}

// colorEnabled reports whether lines written to f may carry color.
//
// NO_COLOR wins over the force variables when both are set. That resolution is
// a choice, and the reason is that the two are not symmetric: NO_COLOR is an
// opt-out a user sets once in their environment, and an opt-out another
// variable can override is not one. The force variables exist for the opposite
// situation - a CI that is not a terminal but does render escapes - and such a
// CI has no reason to also set NO_COLOR.
func colorEnabled(f *os.File) bool {
	if os.Getenv(envNoColor) != "" {
		return false
	}
	if forcesColor(os.Getenv(envClicolorForce)) || forcesColor(os.Getenv(envForceColor)) {
		return true
	}
	return isTerminal(f)
}

// forcesColor reports whether an environment value is a request to force color
// on. Empty means unset; "0" is the conventional "do not force" and neither
// forces nor disables, so it falls through to the terminal check.
func forcesColor(value string) bool {
	return value != "" && value != "0"
}

// Progress renders CLI progress output with optional spinner. Regular output
// goes to out; error and failure lines go to errOut so diagnostics do not
// contaminate stdout consumers.
type Progress struct {
	s      *spinner
	out    stream
	errOut stream
	mu     sync.Mutex
	v      bool
	q      bool
}

// newProgress builds a Progress writing regular output to out and error output
// to errOut, creating and starting a spinner only when output is neither quiet
// nor verbose and the target is a terminal - otherwise raw ANSI escapes or
// interleaved spinner frames would clutter logs (typical in CI, where stdout is
// not a TTY).
func newProgress(verbose, quiet, terminal bool, out, errOut io.Writer) *Progress {
	return newStreamProgress(verbose, quiet, terminal,
		stream{w: out, color: terminal}, stream{w: errOut, color: terminal})
}

// newStreamProgress is newProgress with each destination's color decided
// independently of the other, and independently of whether the spinner runs.
// Those are two questions, not one: the spinner is drawn on stdout and a
// spinner rendered into a file is the noise the terminal check has always
// existed to avoid, while NO_COLOR asks for plain text on a terminal that is
// still perfectly able to redraw a line. Conflating them would silently take
// the spinner away from anyone who set NO_COLOR.
func newStreamProgress(verbose, quiet, terminal bool, out, errOut stream) *Progress {
	if quiet || verbose || !terminal {
		return &Progress{
			v:      verbose,
			q:      quiet,
			out:    out,
			errOut: errOut,
		}
	}

	// Whether frames are drawn is decided by os.Stdout itself, not by the
	// terminal parameter: the parameter is what selects between this
	// package's output states, and a test constructing state A passes it
	// true while running with a pipe on stdout. Measured, not assumed - a
	// test binary's stdout is a pipe under `go test`, under `go test -v`,
	// and under `go test -v` with a real pty as the go tool's own stdout.
	spin := newSpinner(os.Stdout, isTerminal(os.Stdout), out.color)

	p := &Progress{
		v:      verbose,
		q:      quiet,
		s:      spin,
		out:    out,
		errOut: errOut,
	}
	p.s.start()
	return p
}

// New creates a Progress printer configured for verbose/quiet output.
func New(verbose, quiet bool) *Progress {
	return newStreamProgress(verbose, quiet, isTerminal(os.Stdout),
		stream{w: os.Stdout, color: colorEnabled(os.Stdout)},
		stream{w: os.Stderr, color: colorEnabled(os.Stderr)})
}

// isTerminal reports whether f is connected to a terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Okf prints a success message with a marker to stdout. For standalone use.
//
// This helper and Errorf below own no Progress, so they resolve their
// destination on every call rather than once at construction. They are the
// path cmd/go-galaxy/main.go prints a run's final line through, which is
// exactly why they cannot skip the check: before this they printed color
// unconditionally, so `go-galaxy install > install.log 2>&1` put escape bytes
// in front of the one line an operator greps for.
func Okf(format string, args ...any) {
	s := stream{w: os.Stdout, color: colorEnabled(os.Stdout)}
	writeLine(s.w, s.ok(), safeout.Clean(fmt.Sprintf(format, args...)))
}

// Errorf prints an error message with a marker to stderr. For standalone use;
// see Okf for why the destination is resolved per call.
func Errorf(format string, args ...any) {
	s := stream{w: os.Stderr, color: colorEnabled(os.Stderr)}
	writeLine(s.w, s.fail(), safeout.Clean(fmt.Sprintf(format, args...)))
}

// Printf updates the spinner suffix when a spinner is active, otherwise
// prints a log line unless quiet mode is enabled.
func (p *Progress) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s != nil {
		// The leading space separates the suffix from the frame glyph and is
		// sanitized along with the message rather than prepended after it:
		// Clean maps rune by rune, so cleaning the joined string yields the
		// same value as joining the cleaned one, and doing it this way keeps
		// a plain string from ever reaching safeout.Text uncleaned here.
		p.s.setSuffix(safeout.Clean(" " + fmt.Sprintf(format, args...)))
		return
	}
	if p.q {
		return
	}
	p.emit(p.out, "", safeout.Clean(fmt.Sprintf(format, args...)))
}

// PersistentPrintf prints a persistent line to stdout that survives spinner
// updates.
func (p *Progress) PersistentPrintf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.out, "", safeout.Clean(fmt.Sprintf(format, args...)))
}

// Okf prints a success message with a colored marker to stdout.
func (p *Progress) Okf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.out, p.out.ok(), safeout.Clean(fmt.Sprintf(format, args...)))
}

// Errorf prints an error message with a colored marker to stderr, so failures
// do not contaminate stdout consumers.
func (p *Progress) Errorf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.errOut, p.errOut.fail(), safeout.Clean(fmt.Sprintf(format, args...)))
}

// Warnf prints a warning message with a colored marker to stderr. Like
// Errorf, it always emits regardless of verbose/quiet mode and never
// touches stdout, consistent with the stdout-purity rule.
func (p *Progress) Warnf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.errOut, p.errOut.warn(), safeout.Clean(fmt.Sprintf(format, args...)))
}

// Debugf prints a debug message when verbose mode is enabled.
func (p *Progress) Debugf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.v {
		p.emit(p.out, debugPrefix, safeout.Clean(fmt.Sprintf(format, args...)))
	}
}

// DebugSincef prints a debug message with timing info.
func (p *Progress) DebugSincef(start time.Time, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.v {
		prefix := "⏱️ Debug Timing (" + time.Since(start).Round(time.Millisecond).String() + "): "
		p.emit(p.out, prefix, safeout.Clean(fmt.Sprintf(format, args...)))
	}
}

// Write implements io.Writer for log output integration. This is the path
// log.SetOutput(p) sends the stdlib logger's output through under -v (see
// cmd/go-galaxy/commands/runner.go), so it sanitizes precisely because
// nothing in this program controls what a dependency writes to that logger.
func (p *Progress) Write(payload []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	message := strings.TrimRight(string(payload), "\n")
	if message == "" {
		return len(payload), nil
	}
	if p.q {
		return len(payload), nil
	}
	p.emit(p.out, "", safeout.Clean(message))
	return len(payload), nil
}

// Close stops the spinner if it is running and drops it.
//
// Dropping it is what makes the restore final. emit restarts the spinner
// around every line it writes, so a Progress that kept a stopped spinner
// would hide the cursor and spawn a fresh render goroutine on the next Okf or
// Warnf - after the run's only Close, with nothing left to stop it. Nothing
// prints after Close today, but that is the order the callers happen to have
// rather than something the type enforces, and this restore is this package's
// own to keep.
func (p *Progress) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s != nil {
		p.s.stop()
		p.s = nil
	}
}

// emit writes prefix and msg to w as a single decorated line, stopping and
// restarting the spinner around the write when one is active so the line
// survives it. p.mu must be held by the caller.
//
// It is the funnel for every line a *Progress writes; the package-level
// helpers, which own no spinner, reach writeLine directly instead.
func (p *Progress) emit(dst stream, prefix string, msg safeout.Text) {
	if p.s != nil {
		p.s.stop()
		writeLine(dst.w, prefix, msg)
		p.s.restart()
		return
	}
	writeLine(dst.w, prefix, msg)
}

// writeLine is the one place this package turns a decorated line into bytes,
// and it enforces the package's governing rule: sanitize the payload,
// decorate afterwards, never sanitize a line that has already been
// decorated. Every caller passes msg as a safeout.Text - built by applying
// safeout.Clean to exactly what entered through a parameter (a caller's
// format/args, or Write's payload) - so a prefix this file declares (a
// colored marker, a debug tag, a timing string) is never itself subject to
// Clean, and an already-sanitized payload is never re-sanitized. The
// safeout.Text parameter puts that rule into the signature: a value of any
// other type cannot reach msg without an explicit conversion, so a payload
// arriving here from anywhere else has been cleaned or deliberately cast. It
// is a structural check rather than a proof - safeout.Text's own doc comment
// (internal/safeout/safeout.go) holds what the type does not close.
func writeLine(w io.Writer, prefix string, msg safeout.Text) {
	_, _ = fmt.Fprintf(w, "%s%s\n", prefix, msg)
}
