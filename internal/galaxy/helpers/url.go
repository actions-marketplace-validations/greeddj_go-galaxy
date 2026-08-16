package helpers

import (
	"fmt"
	"strings"
)

// WithoutQuery returns raw up to, but not including, its first "?".
//
// A URL's query string is where a bearer capability travels, which is why this
// is a cut rather than a formatting choice. A Galaxy NG or Automation Hub
// deployment backs its artifacts with object storage and answers with a
// presigned download URL, whose query string is a time-limited capability:
// anyone holding that exact URL can fetch the artifact without credentials. So
// every sink that outlives the request itself takes the stripped form -
// GALAXY.yml is the caller this argument was first written for, since it is
// written into the collections tree, which routinely outlives the run and gets
// uploaded wholesale as a CI artifact, handing that capability to everyone who
// can read the build's output. The scheme, host and path are kept, since they
// are the informational part - where this thing actually came from - and carry
// no capability on their own.
//
// The cut is textual rather than a url.Parse round trip precisely because it
// cannot fail: the first literal "?" always begins the query, so a URL this
// tool could not parse still gets stripped rather than silently written
// through intact.
//
// It removes a query and nothing else - a URL's userinfo, the other place a
// credential travels, survives it untouched - so a value that may carry one
// goes through WithoutUserinfo below as well, in either order. WithoutFragment
// is the third cut alongside these two, removing the one part of a URL neither
// of them touches.
func WithoutQuery(raw string) string {
	before, _, _ := strings.Cut(raw, "?")
	return before
}

// WithoutFragment returns raw up to, but not including, its first "#".
//
// A fragment is the one part of a URL that never goes on the wire: net/http
// composes a request from the escaped path and the query alone, and url.Parse
// splits a fragment off before the Path a file:// source is opened by, so it
// decides neither where a request goes nor which file is read. A value rendered
// with its fragment intact therefore names something other than what happened -
// "file:///tmp/a#b.asc" opens /tmp/a - which is what makes this a cut rather
// than a formatting choice, since a message an operator is expected to
// reproduce with one ls -l has to name the path that was actually opened.
//
// It is also the third place bytes this program did not author can sit in a
// URL, after the query and the userinfo, and the only one of the three that no
// endpoint and no filesystem ever sees. Cutting it therefore costs no
// information about where a value came from.
//
// Like WithoutQuery, the cut is textual rather than a url.Parse round trip
// precisely because it cannot fail: the first literal "#" always begins the
// fragment, so a URL this tool could not parse is stripped exactly like one it
// could.
func WithoutFragment(raw string) string {
	before, _, _ := strings.Cut(raw, "#")
	return before
}

// WithoutUserinfo returns raw with the userinfo of its authority removed, or
// raw itself when it carries none.
//
// A URL's userinfo is a credential, and one that acts as well as renders:
// net/http sets Basic auth from it before any transport runs, and
// url.URL.String() prints the password back out in plain text at every sink the
// value reaches. So a value this program did not author is cut here before it
// is rendered anywhere - a message, a log line, or persisted state.
//
// The cut is textual for a reason stronger than WithoutQuery's own. The values
// that most need it are exactly the ones url.Parse refuses: an authority
// spelling an invalid port or a malformed IP literal, a path carrying a bad
// percent escape, a control character anywhere in the value. Each of those
// still carries a password, and a caller that renders the raw string once the
// parse has failed prints it verbatim - which is the whole reason this is a
// total function over a string rather than a method on a *url.URL nobody has in
// hand on that path.
//
// The scan is RFC 3986's authority, walked directly: an optional scheme up to
// its ":", an optional "//", then the authority up to the first "/", "?" or
// "#". Everything through the last "@" of that authority is dropped - the last
// rather than the first, matching what net/url's own authority parse does.
// Terminating the scan at "?" and "#" is what lets this compose with
// WithoutQuery and WithoutFragment in any order, since no cut can then reach
// across another's boundary.
//
// The residual is a property of that delimiter set rather than of any one
// character, and it is a disclosure to a message rather than to the wire: a
// value whose intended userinfo CONTAINS one of "/", "?" or "#" leaves no "@"
// inside the authority this scan reads, so this function returns it unchanged
// and what survives into a message is decided by the cuts composed alongside
// it. Two outcomes follow, and the second is the weaker one. "?" and "#" have a
// cut of their own, so "https://user:pa?55w0rd@h/x" renders as
// "https://user:pa" - a truncated password rather than none at all - while "/"
// has no cut anywhere, so "https://user:pa/55w0rd@h/x" renders whole. RFC 3986
// permits none of the three inside userinfo, and url.Parse never reads a value
// spelled that way as carrying one: it either refuses the value outright, as it
// does all three above, since "user:pa" is an invalid port, or takes what
// precedes the delimiter for a host. Either way the credential authenticates
// nothing, which is what keeps this a disclosure to a message.
//
// Cutting on "/" as well is not available, which is why that outcome is
// disclosed rather than closed: "https://h/x@y" is a perfectly ordinary URL
// with an "@" in its path and is textually indistinguishable from the shape
// above, so a cut there would truncate every value carrying a path.
//
// An opaque URL's body is read as an authority too, which is what makes
// "http:u:p@h/x" lose its credential. The one legitimate value that costs
// anything is "mailto:u@e.com", rewritten to "mailto:e.com": no caller here
// fetches a mailto, and every one of them refuses it as an unsupported source
// regardless, so the cut errs toward removing too much rather than too little.
func WithoutUserinfo(raw string) string {
	// authorityPrefix is what introduces an authority once a scheme, if any,
	// has been consumed. Naming it keeps the two places its length is used from
	// being spelled as a bare number.
	const authorityPrefix = "//"

	// A ":" reached before any of "/?#" ends a scheme; one reached after it
	// does not, so the whole delimiter set is scanned once rather than
	// searching for a ":" that may belong to a port or to userinfo.
	rest, off := raw, 0
	if i := strings.IndexAny(rest, ":/?#"); i >= 0 && rest[i] == ':' {
		rest, off = rest[i+1:], off+i+1
	}
	if strings.HasPrefix(rest, authorityPrefix) {
		rest, off = rest[len(authorityPrefix):], off+len(authorityPrefix)
	}

	authority := rest
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}

	// No userinfo is the overwhelmingly common case, and returning raw itself
	// keeps it allocation-free: only a value that actually carries a credential
	// pays for the one concatenation below.
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}

	return raw[:off] + raw[off+at+1:]
}

// MessageValueMaxLen bounds one attacker-influenced string before an
// operator-facing message renders it. Three values are subject to it today: a
// Galaxy version-metadata href, which becomes a signature blob's origin; the
// display form of a signature source out of a requirements file; and the
// identity a collection artifact's own MANIFEST.json declares.
//
// None of them is bounded by what such a value can legitimately be - the first
// only by MetadataMaxSize (16 MiB), the third only by ManifestScanMaxBytes
// (64 MiB) - so the ceiling is. 512 bytes is roughly three times the longest
// real value measured: 119 bytes for a galaxy.ansible.com v3 version-detail
// href, 172 for a Red Hat Automation Hub one carrying a synclist-scoped base
// path and a long namespace, 66 for this project's own test double. A
// collection's namespace, name and version are each an order of magnitude
// shorter again.
//
// What the cap removes is the multiplication rather than one long line: an
// origin is copied onto every blob gathered for a collection and rendered once
// per non-ignored failure, so an 8 MiB href across a full
// MaxSignaturesPerCollection set produced a 512.0 MiB error string, measured,
// rendered at least twice and written to stderr both times, per collection,
// times the worker count.
const MessageValueMaxLen = 512

// TruncateForMessage bounds value at MessageValueMaxLen before a message
// renders it, cutting visibly rather than silently: a value past the ceiling
// keeps its first MessageValueMaxLen bytes and then says how long it really
// was, so an operator can tell a long value from a truncated one and a reader
// of the code cannot mistake this for a silent shortening.
//
// The cut is on a byte boundary, which can split a multi-byte rune. That costs
// nothing where it is used: every caller renders the result with %q, which
// escapes an invalid byte rather than emitting it, and internal/safeout
// replaces one with U+FFFD at the printer boundary regardless.
func TruncateForMessage(value string) string {
	if len(value) <= MessageValueMaxLen {
		return value
	}

	return value[:MessageValueMaxLen] + fmt.Sprintf("... (%d bytes)", len(value))
}
