package helpers

import (
	"fmt"
	"io"
)

// sizeLimitedReader wraps an io.Reader so that reading past a cumulative
// byte ceiling fails instead of silently truncating. Unlike io.LimitReader -
// which stops returning bytes at the limit and reports a clean io.EOF,
// indistinguishable from the underlying stream genuinely ending there - this
// type treats crossing the ceiling as an error, since a truncated artifact
// download would otherwise be mistaken for a short-but-complete one and fail
// a later checksum check with a misleading cause.
type sizeLimitedReader struct {
	r   io.Reader
	err error
	max int64
	n   int64
}

// NewSizeLimitedReader returns an io.Reader over r that passes bytes through
// unchanged while the cumulative count read so far is at or under limit, and
// fails with helpers.ErrResponseTooLarge as soon as it exceeds limit.
//
// The crossing call returns zero bytes and drops what it just read, which
// io.Reader permits. Returning them instead is what makes the refusal
// disappear, and the predicate is general rather than a property of any
// particular caller: any caller that treats a fully-satisfied request as
// success discards a non-nil error returned alongside it - io.ReadAtLeast's
// `if n >= min { err = nil }` and io.CopyN's `if written == n { return n, nil }`
// are both in the standard library, and both are reachable through a
// bufio.Reader or a gzip.Reader layered over this one.
func NewSizeLimitedReader(r io.Reader, limit int64) io.Reader {
	return &sizeLimitedReader{r: r, max: limit}
}

// Read reads from the wrapped reader and tracks the cumulative byte count.
// Once that count exceeds max, it returns ErrResponseTooLarge instead of the
// underlying reader's own result, since a stream that overruns its ceiling
// must never be reported as a clean read regardless of what the underlying
// reader itself returned (including its own io.EOF).
//
// The refusal is sticky: once it fires, every later call returns it without
// touching the underlying reader, so a caller that keeps reading cannot drain
// further bytes past the ceiling.
//
// Clamping p bounds the overrun to a single byte rather than to a whole read
// buffer, which is also what makes the byte count the error reports exact. It
// is not what produces the refusal.
//
// This is the same shape internal/galaxy/archive's decompressedLimitReader
// uses, deliberately: two size caps in one program that disagree about what a
// crossing read returns is a trap for whoever reads only one of them.
func (r *sizeLimitedReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	// Keeps p[:remaining+1] in range: a max below zero drives remaining
	// negative, and at -2 or lower that reslice panics.
	remaining := max(r.max-r.n, 0)
	// Taking this branch means remaining < len(p), hence remaining+1 <= len(p),
	// so the reslice is always within p.
	if remaining < int64(len(p)) {
		p = p[:remaining+1]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.max {
		r.err = fmt.Errorf("%w: read %d bytes, limit is %d bytes", ErrResponseTooLarge, r.n, r.max)
		return 0, r.err
	}
	return n, err
}
