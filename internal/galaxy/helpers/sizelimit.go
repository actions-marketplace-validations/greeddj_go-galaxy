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
	max int64
	n   int64
}

// NewSizeLimitedReader returns an io.Reader over r that passes bytes through
// unchanged while the cumulative count read so far is at or under limit, and
// fails with helpers.ErrResponseTooLarge as soon as it exceeds limit. The
// Read call that crosses the ceiling still returns the bytes it read in that
// call, so a caller that checks n before err (as io.Reader documents) sees no
// data loss up to the ceiling; only the error signals that the stream must
// not be trusted or used past that point.
func NewSizeLimitedReader(r io.Reader, limit int64) io.Reader {
	return &sizeLimitedReader{r: r, max: limit}
}

// Read reads from the wrapped reader and tracks the cumulative byte count.
// Once that count exceeds max, it returns ErrResponseTooLarge instead of the
// underlying reader's own result, since a stream that overruns its ceiling
// must never be reported as a clean read regardless of what the underlying
// reader itself returned (including its own io.EOF).
func (r *sizeLimitedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.max {
		return n, fmt.Errorf("%w: read %d bytes, limit is %d bytes", ErrResponseTooLarge, r.n, r.max)
	}
	return n, err
}
