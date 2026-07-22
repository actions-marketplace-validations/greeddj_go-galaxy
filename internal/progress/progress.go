package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/briandowns/spinner"
)

const (
	spinnerDelay   = 100 * time.Millisecond
	spinnerCharSet = 14
	spinnerColor   = "green"
	ansiRed        = "\x1b[1m\x1b[31m"
	ansiGreen      = "\x1b[1m\x1b[32m"
	ansiReset      = "\x1b[1m\x1b[0m"
	ok             = ansiGreen + "✔" + ansiReset
	fail           = ansiRed + "✗" + ansiReset
)

// Progress renders CLI progress output with optional spinner.
type Progress struct {
	s   *spinner.Spinner
	out io.Writer
	v   bool
	q   bool
}

// newProgress builds a Progress writing to out, creating and starting a
// spinner only when output is neither quiet nor verbose and the target is a
// terminal - otherwise raw ANSI escapes or interleaved spinner frames would
// clutter logs (typical in CI, where stdout is not a TTY).
func newProgress(verbose, quiet, terminal bool, out io.Writer) *Progress {
	if quiet || verbose || !terminal {
		return &Progress{
			v:   verbose,
			q:   quiet,
			out: out,
		}
	}

	spin := spinner.New(spinner.CharSets[spinnerCharSet], spinnerDelay)
	_ = spin.Color(spinnerColor)

	p := &Progress{
		v:   verbose,
		q:   quiet,
		s:   spin,
		out: out,
	}
	p.s.Start()
	return p
}

// New creates a Progress printer configured for verbose/quiet output.
func New(verbose, quiet bool) *Progress {
	return newProgress(verbose, quiet, isStdoutTerminal(), os.Stdout)
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
	_, _ = fmt.Fprintf(os.Stdout, ok+" "+format+"\n", args...)
}

// Errorf prints an error message with a colored marker. For standalone use.
func Errorf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stdout, fail+" "+format+"\n", args...)
}

// Printf updates the spinner suffix when a spinner is active, otherwise
// prints a log line unless quiet mode is enabled.
func (p *Progress) Printf(format string, args ...any) {
	if p.s != nil {
		// Plain assignment: the spinner goroutine reads Suffix concurrently,
		// but closing that race is deferred to a later commit.
		p.s.Suffix = fmt.Sprintf(" "+format, args...)
		return
	}
	if p.q {
		return
	}
	_, _ = fmt.Fprintf(p.out, format+"\n", args...)
}

// PersistentPrintf prints a persistent line that survives spinner updates.
// Unlike Printf, this always emits regardless of verbose/quiet mode - result
// lines (success/failure) must never be swallowed.
func (p *Progress) PersistentPrintf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if p.s != nil {
		p.s.Stop()
		_, _ = fmt.Fprintf(p.out, "%s\n", msg)
		p.s.Restart()
		return
	}
	_, _ = fmt.Fprintf(p.out, "%s\n", msg)
}

// Okf prints a success message with a colored marker.
func (p *Progress) Okf(format string, args ...any) {
	p.PersistentPrintf(ok+" "+format, args...)
}

// Errorf prints an error message with a colored marker.
func (p *Progress) Errorf(format string, args ...any) {
	p.PersistentPrintf(fail+" "+format, args...)
}

// Debugf prints a debug message when verbose mode is enabled.
func (p *Progress) Debugf(format string, args ...any) {
	if p.v {
		_, _ = fmt.Fprintf(p.out, "🚧 Debug: "+format+"\n", args...)
	}
}

// DebugSincef prints a debug message with timing info.
func (p *Progress) DebugSincef(start time.Time, format string, args ...any) {
	if p.v {
		_, _ = fmt.Fprintf(p.out, "⏱️ Debug Timing ("+time.Since(start).Round(time.Millisecond).String()+"): "+format+"\n", args...)
	}
}

// Write implements io.Writer for log output integration.
func (p *Progress) Write(payload []byte) (int, error) {
	message := strings.TrimRight(string(payload), "\n")
	if message == "" {
		return len(payload), nil
	}
	if p.s != nil {
		p.s.Stop()
		_, _ = fmt.Fprintln(p.out, message)
		p.s.Restart()
		return len(payload), nil
	}
	if p.q {
		return len(payload), nil
	}
	_, _ = fmt.Fprintln(p.out, message)
	return len(payload), nil
}

// Close stops the spinner if it is running.
func (p *Progress) Close() {
	if p.s != nil {
		p.s.Stop()
	}
}
