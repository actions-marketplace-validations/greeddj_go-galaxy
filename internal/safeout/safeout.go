// Package safeout sanitizes text before it reaches an operator's terminal
// or a log processor.
//
// Text this program prints often originates outside this program: an S3
// error's XML <Message>, an HTTP reason phrase, Galaxy metadata, a
// requirements file, a manifest, or a filesystem path. Any of those can
// carry terminal control sequences - ANSI escapes, cursor moves, a title
// change - that a naive fmt.Fprintf passes straight to the terminal or a
// log scraper. Clean and NewWriter replace those with U+FFFD rather than
// deleting them, so hostile input becomes visible garbage instead of an
// invisible command. See Clean for the exact rule.
package safeout

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Text is a string that has passed through Clean. Its purpose is
// structural, not informational: a function requiring Text as a parameter
// cannot compile against a plain string, so sanitize-then-decorate is
// enforced by the type checker rather than left to be remembered at every
// call site.
type Text string

// Clean replaces with U+FFFD the control characters a terminal or log
// processor could act on as a command. See below for the exact rule,
// including the two characters it deliberately keeps.
//
// The rule is closed and enumerable by codepoint rather than by intent: a
// character is replaced exactly when it is in C0 (U+0000-U+001F), is DEL
// (U+007F), or is in C1 (U+0080-U+009F), and is not \n or \t. This holds
// however the input encoded it - a legitimately UTF-8-encoded C1
// character and a raw 0x80-0x9F byte both become U+FFFD. Separately, and
// whatever its value, every byte that is not part of a valid UTF-8
// encoding is replaced too: a lone 0xFF, or a truncated multi-byte
// sequence at the end of the input. That is why the result is always
// valid UTF-8.
//
// Kept: \n and \t survive deliberately. \n survives because errors.Join
// renders joined causes separated by \n, and this program prints such
// errors as-is, so stripping it would turn every aggregated failure
// report into one run-on line; a hostile message can still claim extra
// plain-text lines this way, but can never overwrite a line already
// emitted, since \r does not survive. \t survives because it only ever
// advances the cursor forward, never back. Bidi and format control
// characters (U+202A-U+202E, U+2066-U+2069) also survive, deliberately:
// they are content directionality, not a command a terminal acts on -
// they cannot move the cursor, clear a line, set a window title, or emit
// a hyperlink - and a real defense against visual reordering would also
// have to cover homoglyphs and combining marks, which is not achievable;
// a partial defense would invite belief in a guarantee this function does
// not provide.
//
// Nothing is ever deleted outright: a removed character becomes U+FFFD
// rather than vanishing, because silent deletion can itself mislead -
// "good\rbad" deleting down to "goodbad" reads as a sensible word, which
// is not what was actually sent.
func Clean(s string) Text {
	return Text(strings.Map(sanitizeRune, s))
}

// Boundaries of the character ranges sanitizeRune replaces: the C0 control
// range is everything below controlC0Max, del is DEL (U+007F), and the C1
// control range is [controlC1Min, controlC1Max].
const (
	controlC0Max = 0x20
	del          = 0x7f
	controlC1Min = 0x80
	controlC1Max = 0x9f
)

// sanitizeRune is strings.Map's per-rune callback for Clean.
func sanitizeRune(r rune) rune {
	switch {
	case r == '\n' || r == '\t':
		return r
	case r < controlC0Max, r == del, r >= controlC1Min && r <= controlC1Max:
		return utf8.RuneError
	default:
		return r
	}
}

// NewWriter wraps w so every Write is sanitized through Clean before
// reaching w.
//
// Each Write is sanitized independently of any other, and no split can
// let a character Clean removes reach the underlying writer. The removed
// characters come in two shapes and a split defeats neither. A one-byte
// one - any C0 character, DEL, or a raw 0x80-0x9F byte - stays whole
// inside whichever Write receives it, since there is nothing to split.
// A two-byte C1 encoding split between its bytes leaves an incomplete
// encoding on each side, so each side decodes to utf8.RuneError and each
// is replaced.
//
// What a split does cost is legitimate text: a multi-byte rune divided
// between two Writes renders as one U+FFFD per byte rather than as the
// character, so a 3-byte box-drawing rune becomes three of them. That is
// not hypothetical for this writer - printTree emits box-drawing runes on
// every line of its output - but it is bounded by how a caller chunks its
// writes, and the callers here write whole formatted lines at a time.
//
// This writer is not a hot path - it exists for CLI report output, not
// per-tick logging - so the extra allocation a Write costs when it has
// anything to replace is not worth avoiding.
func NewWriter(w io.Writer) io.Writer {
	return &writer{w: w}
}

// writer is the io.Writer NewWriter returns.
type writer struct {
	w io.Writer
}

// Write sanitizes p through Clean and writes the result to the
// underlying writer. It returns 0, err on the underlying writer's error,
// and len(p), nil otherwise: the pre-sanitization length, since the
// io.Writer contract requires a non-nil error whenever n < len(p), and
// the sanitized form can differ in length from p.
func (s *writer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, string(Clean(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}
