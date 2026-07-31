package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/briandowns/spinner"
	"github.com/greeddj/go-galaxy/internal/safeout"
)

const (
	spinnerDelay   = 100 * time.Millisecond
	spinnerCharSet = 14
	spinnerColor   = "green"
	ansiRed        = "\x1b[1m\x1b[31m"
	ansiGreen      = "\x1b[1m\x1b[32m"
	ansiYellow     = "\x1b[1m\x1b[33m"
	ansiReset      = "\x1b[1m\x1b[0m"
	ok             = ansiGreen + "✔" + ansiReset
	fail           = ansiRed + "✗" + ansiReset
	warn           = ansiYellow + "!" + ansiReset
	okPrefix       = ok + " "
	failPrefix     = fail + " "
	warnPrefix     = warn + " "
	debugPrefix    = "🚧 Debug: "
)

// Progress renders CLI progress output with optional spinner. Regular output
// goes to out; error and failure lines go to errOut so diagnostics do not
// contaminate stdout consumers.
type Progress struct {
	s      *spinner.Spinner
	out    io.Writer
	errOut io.Writer
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
	if quiet || verbose || !terminal {
		return &Progress{
			v:      verbose,
			q:      quiet,
			out:    out,
			errOut: errOut,
		}
	}

	spin := spinner.New(spinner.CharSets[spinnerCharSet], spinnerDelay)
	_ = spin.Color(spinnerColor)

	p := &Progress{
		v:      verbose,
		q:      quiet,
		s:      spin,
		out:    out,
		errOut: errOut,
	}
	p.s.Start()
	return p
}

// New creates a Progress printer configured for verbose/quiet output.
func New(verbose, quiet bool) *Progress {
	return newProgress(verbose, quiet, isStdoutTerminal(), os.Stdout, os.Stderr)
}

// isStdoutTerminal reports whether stdout is connected to a terminal.
func isStdoutTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Okf prints a success message with a colored marker. For standalone use.
func Okf(format string, args ...any) {
	writeLine(os.Stdout, okPrefix, safeout.Clean(fmt.Sprintf(format, args...)))
}

// Errorf prints an error message with a colored marker to stderr. For
// standalone use.
func Errorf(format string, args ...any) {
	writeLine(os.Stderr, failPrefix, safeout.Clean(fmt.Sprintf(format, args...)))
}

// Printf updates the spinner suffix when a spinner is active, otherwise
// prints a log line unless quiet mode is enabled.
func (p *Progress) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s != nil {
		// The spinner render goroutine reads Suffix under the spinner's own
		// lock, so the update must take that same lock to avoid a data race.
		p.s.Lock()
		p.s.Suffix = " " + string(safeout.Clean(fmt.Sprintf(format, args...)))
		p.s.Unlock()
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
	p.emit(p.out, okPrefix, safeout.Clean(fmt.Sprintf(format, args...)))
}

// Errorf prints an error message with a colored marker to stderr, so failures
// do not contaminate stdout consumers.
func (p *Progress) Errorf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.errOut, failPrefix, safeout.Clean(fmt.Sprintf(format, args...)))
}

// Warnf prints a warning message with a colored marker to stderr. Like
// Errorf, it always emits regardless of verbose/quiet mode and never
// touches stdout, consistent with the stdout-purity rule.
func (p *Progress) Warnf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(p.errOut, warnPrefix, safeout.Clean(fmt.Sprintf(format, args...)))
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

// Close stops the spinner if it is running.
func (p *Progress) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s != nil {
		p.s.Stop()
	}
}

// emit writes prefix and msg to w as a single decorated line, stopping and
// restarting the spinner around the write when one is active so the line
// survives it. p.mu must be held by the caller.
//
// It is the funnel for every line a *Progress writes; the package-level
// helpers, which own no spinner, reach writeLine directly instead.
func (p *Progress) emit(w io.Writer, prefix string, msg safeout.Text) {
	if p.s != nil {
		p.s.Stop()
		writeLine(w, prefix, msg)
		p.s.Restart()
		return
	}
	writeLine(w, prefix, msg)
}

// writeLine is the one place this package turns a decorated line into bytes,
// and it enforces the package's governing rule: sanitize the payload,
// decorate afterwards, never sanitize a line that has already been
// decorated. Every caller passes msg as a safeout.Text - built by applying
// safeout.Clean to exactly what entered through a parameter (a caller's
// format/args, or Write's payload) - so a prefix this file declares (a
// colored marker, a debug tag, a timing string) is never itself subject to
// Clean, and an already-sanitized payload is never re-sanitized. The
// safeout.Text parameter type makes this a compile-time property: writeLine
// cannot be called with a plain string where Text is required, so
// sanitize-then-decorate is type-checked rather than a convention callers
// must remember.
func writeLine(w io.Writer, prefix string, msg safeout.Text) {
	_, _ = fmt.Fprintf(w, "%s%s\n", prefix, msg)
}
