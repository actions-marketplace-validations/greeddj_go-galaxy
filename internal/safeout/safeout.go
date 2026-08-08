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
// structural, not informational: it puts sanitize-then-decorate into a
// function's signature rather than leaving it to be remembered at every
// call site.
//
// What it closes is the whole of the untrusted surface, and the reason is
// not a property of today's code but of what a constant is. A value of any
// other type is unassignable to Text - a string, a constant declared with
// an explicit string type, another defined string type, a type parameter
// constrained to ~string - so each has to pass an explicit conversion,
// either Clean or a Text(...) cast a reviewer can see. Untrusted input - a
// Galaxy server's error text, a manifest, a filesystem path, output a
// caller formatted elsewhere - arrives at run time, so it is never a
// constant, so it is always a typed value, so it is always refused.
//
// What it does not close is the complement of that argument: an untyped
// string constant, which is assignable to a defined string type. That is a
// bare literal, a named constant declared anywhere, or any constant
// expression over them, in this package or another - each compiles where
// Text is required with its control characters intact. The gap is
// therefore a question about what an author of this repository writes,
// which source review answers, rather than about untrusted input reaching
// a terminal, which is what this type is for. It is a structural check,
// not a proof.
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
// Replaced, not kept, despite superficially resembling the bidi/format
// group above: U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR. The
// justification one paragraph up does not reach them - they are category
// Zl/Zp, not Cf, and they are not directionality at all - because each one
// terminates a line for a Unicode-aware consumer the way \n does for this
// program's own terminal-facing output: Python's str.splitlines(), reached
// by CI tooling written in Python constantly, splits on both. A
// directionality mark can never do that; these two can.
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
// control range is [controlC1Min, controlC1Max]. lineSeparator and
// paragraphSeparator are the two line-terminating Unicode space characters
// (category Zl and Zp) that are not part of any control range above but
// are replaced anyway - see Clean's own doc comment for why they do not
// belong in the kept bidi/format group despite sitting nearby in the
// codepoint chart.
const (
	controlC0Max       = 0x20
	del                = 0x7f
	controlC1Min       = 0x80
	controlC1Max       = 0x9f
	lineSeparator      = '\u2028'
	paragraphSeparator = '\u2029'
)

// isControl reports whether r is one of the control ranges Clean replaces:
// C0 (below controlC0Max), DEL, or C1 ([controlC1Min, controlC1Max]). It is
// one of the two named halves of IsUnsafeRune's union (see isLineTerminator
// below for the other), kept separate rather than folded into one function
// so that neither name has to lie about the other's members: a caller
// asking "is this a control character" deserves the true answer, not one
// skewed by a rule about line terminators. Nothing outside this package
// consults isControl directly - a caller with that need goes through
// IsUnsafeRune, the exported union, or exports this half itself once it has
// an actual reason to.
func isControl(r rune) bool {
	return r < controlC0Max || r == del || (r >= controlC1Min && r <= controlC1Max)
}

// isLineTerminator reports whether r is one of the two Unicode space
// characters Clean also replaces despite neither being a control character:
// U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR. See Clean's own doc
// comment for why these two - and only these two, out of the wider
// bidi/format group that survives Clean untouched - are replaced anyway:
// each terminates a line for a Unicode-aware consumer (Python's
// str.splitlines(), notably) exactly as \n does for this program's own
// terminal-facing output. It is the other named half of IsUnsafeRune's
// union, kept separate from isControl for the identical reason that
// function's own doc states, and nothing outside this package consults it
// directly either.
func isLineTerminator(r rune) bool {
	return r == lineSeparator || r == paragraphSeparator
}

// IsUnsafeRune reports whether r is isControl or isLineTerminator - the full
// set Clean replaces once \n and \t are set aside (sanitizeRune below adds
// that exception on top). This union, not either predicate alone, is what a
// caller wanting Clean's whole "would this forge a line or drive a
// terminal" judgment should consult: helpers.IsPathElement is exactly such
// a caller, rejecting every rune this reports true without carrying the
// \n/\t exception a single path element has no use for.
//
// This is the one place a new codepoint class must be added for Clean to
// act on it: sanitizeRune consults only this union, never isControl or
// isLineTerminator directly, so a class added here reaches Clean, NewWriter,
// and helpers.IsPathElement together, with nothing else to remember to
// update. The reverse is not guaranteed and is not a defect: IsPathElement
// stopping short of a class Clean gains is not the drift this note
// forecloses, only a caller here forgetting to update it - Clean would
// still replace the character before it ever reached a terminal, so the
// printing invariant IsPathElement documents would survive unchanged
// either way.
func IsUnsafeRune(r rune) bool {
	return isControl(r) || isLineTerminator(r)
}

// sanitizeRune is strings.Map's per-rune callback for Clean.
func sanitizeRune(r rune) rune {
	switch {
	case r == '\n' || r == '\t':
		return r
	case IsUnsafeRune(r):
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
