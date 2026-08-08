package progress

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

// The spinner's whole vocabulary: how often a frame is drawn, the glyphs it
// cycles through, and the escape sequences that surround one frame.
//
// spinnerFrames is a string rather than a slice because this package keeps no
// package-level variables and a []string cannot be a constant; newSpinner
// turns it into runes once per spinner.
//
// The two wrap sequences bracket one frame rather than one run: they are
// written by writeFrameLocked, not by spinnerEnterSeq and spinnerLeaveSeq,
// so autowrap is off only for the bytes of the frame that needs it off.
const (
	spinnerDelay      = 100 * time.Millisecond
	spinnerFrames     = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
	spinnerColorSeq   = "\x1b[32m"
	spinnerResetSeq   = "\x1b[0m"
	spinnerEraseSeq   = "\r\x1b[K"
	spinnerWrapOffSeq = "\x1b[?7l"
	spinnerWrapOnSeq  = "\x1b[?7h"
	spinnerEnterSeq   = "\x1b[?25l"
	spinnerLeaveSeq   = "\x1b[?25h" + spinnerEraseSeq
	frameBufSize      = 128
)

// The terminal type that renders escape sequences literally instead of acting
// on them, so a frame drawn there must carry no color.
const (
	envTerm      = "TERM"
	dumbTerminal = "dumb"
)

// spinner draws a one-line activity indicator on w, redrawing it every delay
// until it is stopped.
//
// The lifecycle has exactly one invariant, and every guard in this file is a
// spelling of it: stopCh is non-nil exactly while a render goroutine is the
// current one. A render goroutine is handed the channel it was started with,
// so one that wins the mutex after a stop already ran sees a field that is no
// longer its own channel and writes nothing - which is what keeps a stale
// frame from landing on top of the sequence that restored the terminal.
//
// Two decisions about how a frame occupies the screen are accepted here
// rather than solved. A frame is one physical line by construction: autowrap
// is off for exactly the bytes of one frame, and the suffix is cut at its
// first newline, which together are what make a one-line erase enough to
// clear the whole frame - \n is the only line-affecting character that
// survives safeout.Clean, since \r and ESC are both C0 and become U+FFFD.
// What that gives up is a suffix wider than the terminal, which is truncated
// at the right margin instead of wrapping, and a multi-line suffix, which
// shows its first line only. And autowrap-off degrades rather than breaks: a
// terminal that ignores DECAWM wraps as it always did and a one-line erase
// leaves a tail behind there, which is the naive behavior and never worse
// than it.
//
// Holding that mode for one write rather than for the run is what keeps an
// abnormal exit harmless. A run killed between frames leaves the terminal as
// the cursor-hide sequence alone would, because the last frame restored
// autowrap in the same write that turned it off. That matters for the exit
// this program deliberately does not catch: an unhandled SIGQUIT runs no
// deferred Close, and the goroutine dump it exists to produce is printed to
// the very terminal a run-scoped mode would have left unable to wrap it.
type spinner struct {
	w       io.Writer
	stopCh  chan struct{}
	suffix  safeout.Text
	frames  []rune
	delay   time.Duration
	next    int
	mu      sync.Mutex
	render  bool
	colored bool
}

// newSpinner builds a spinner drawing on w. render decides whether it draws
// at all and colorAllowed whether its frames carry color, which a TERM of
// "dumb" takes away even from a destination that otherwise accepts color.
//
// render is decided by the caller, and the check available to it is a
// character-device test rather than a real terminal probe. One configuration
// gets a spinner it would not have had otherwise: a run whose stdout is
// redirected to the null device draws frames into it, invisibly, at the cost
// of one write every delay. Closing that would mean a real tty check, which is
// a dependency this package does not carry; the same imprecision already
// governs colorEnabled, so such a run already emits colored markers there.
func newSpinner(w io.Writer, render, colorAllowed bool) *spinner {
	return &spinner{
		w:       w,
		frames:  []rune(spinnerFrames),
		delay:   spinnerDelay,
		render:  render,
		colored: colorAllowed && os.Getenv(envTerm) != dumbTerminal,
	}
}

// start hides the cursor and begins redrawing frames. It is a no-op for a
// spinner that does not render or that is already running.
//
// The first frame is written synchronously, before the render goroutine
// exists, so a caller observes a drawn spinner as soon as start returns
// instead of one tick later.
func (s *spinner) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.render || s.stopCh != nil {
		return
	}
	s.stopCh = make(chan struct{})
	_, _ = io.WriteString(s.w, spinnerEnterSeq)
	s.writeFrameLocked()
	go s.run(s.stopCh, s.delay)
}

// stop erases the current frame and restores the cursor. It is a no-op when
// no render goroutine is current, which makes it idempotent and safe on a
// spinner that never started. Autowrap needs nothing here, since every frame
// already restored it before this one was drawn.
//
// It does not wait for the render goroutine to exit, and must not: it holds
// the mutex that goroutine takes to draw a frame, so waiting here would
// deadlock. Clearing stopCh is what makes the wait unnecessary - the goroutine
// still in flight can no longer write anything.
func (s *spinner) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCh == nil {
		return
	}
	close(s.stopCh)
	s.stopCh = nil
	_, _ = io.WriteString(s.w, spinnerLeaveSeq)
}

// restart stops the spinner and starts it again, which is how a caller prints
// a line of its own without a frame landing in the middle of it.
func (s *spinner) restart() {
	s.stop()
	s.start()
}

// run redraws a frame on every tick until stop is closed. Both the channel and
// the period are parameters rather than fields so this goroutine reads neither
// through the mutex it must take anyway, and so the identity check below
// compares against the channel this goroutine was started with.
func (s *spinner) run(stop chan struct{}, delay time.Duration) {
	ticker := time.NewTicker(delay)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.stopCh == stop {
				s.writeFrameLocked()
			}
			s.mu.Unlock()
		}
	}
}

// setSuffix replaces the text drawn to the right of the frame glyph.
//
// The safeout.Text parameter is a structural check rather than a proof. A
// string-typed value cannot be passed without an explicit conversion, so
// anything reaching here from outside this file has to have been cleaned or
// deliberately cast; an untyped string constant written at this call site
// would still be assignable, which is the gap the type does not close.
func (s *spinner) setSuffix(text safeout.Text) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suffix = text
}

// suffixText returns the current suffix.
func (s *spinner) suffixText() safeout.Text {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suffix
}

// writeFrameLocked draws one frame - autowrap off, erase, glyph, suffix,
// autowrap on - as a single write, and advances to the next glyph. s.mu must
// be held by the caller.
//
// The single write is a property callers depend on and not an incidental
// consequence of using a builder: the escape sequences here are only ever
// handed to the writer whole, so no reader of that writer can observe a
// half-written one. The two wrap sequences are the reason the window is this
// narrow - opening and closing it around these bytes alone leaves the mode
// untouched for everything else that reaches the terminal, including whatever
// is printed after this process is gone.
//
// The frame is composed in a local builder rather than in a buffer reused
// across ticks: at ten frames a second the one allocation that costs is
// noise, and handing a Write call a buffer this struct keeps aliasing would
// be a contract worth more than it saves.
func (s *spinner) writeFrameLocked() {
	var frame strings.Builder
	frame.Grow(frameBufSize)
	frame.WriteString(spinnerWrapOffSeq)
	frame.WriteString(spinnerEraseSeq)
	if s.colored {
		frame.WriteString(spinnerColorSeq)
		frame.WriteRune(s.frames[s.next])
		frame.WriteString(spinnerResetSeq)
	} else {
		frame.WriteRune(s.frames[s.next])
	}
	frame.WriteString(string(firstLine(s.suffix)))
	frame.WriteString(spinnerWrapOnSeq)
	s.next = (s.next + 1) % len(s.frames)
	_, _ = io.WriteString(s.w, frame.String())
}

// firstLine returns text up to its first newline. Slicing preserves the
// safeout.Text type, so a frame is assembled without ever converting a plain
// string back into sanitized text.
func firstLine(text safeout.Text) safeout.Text {
	if i := strings.IndexByte(string(text), '\n'); i >= 0 {
		return text[:i]
	}
	return text
}
