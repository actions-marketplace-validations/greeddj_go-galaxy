// Package gzipstream is the one place this module opens a gzip reader over
// bytes it did not produce. Every such stream is either a collection artifact
// or this program's own persisted cache state, both of which arrive across a
// documented trust boundary, so the reader they are read through has to hold
// two properties klauspost/pgzip does not offer on its own: a member that
// produces nothing must end the read rather than cost a stack frame, and the
// caller's context must be observed on the COMPRESSED side, where one member
// can consume an arbitrary number of wire bytes without ever handing back one
// decompressed byte for a decompressed-side check to fire on.
//
// One gzip reader in this process sits outside that class by construction,
// and is named here so the sentence above reads as the boundary it is rather
// than as an absolute: net/http advertises Accept-Encoding: gzip on every
// request this program's fetch clients issue - newTransport
// (internal/galaxy/fetch/client.go) sets no DisableCompression - and
// inflates a Content-Encoding: gzip response through compress/gzip, over
// bytes a Galaxy server chose. What keeps it out is the first of the two
// properties above: the standard library walks members in a for loop rather
// than by recursion, so no member costs a stack frame there (measured on
// go1.26.6, darwin/arm64: about 3 million members a second) and a flood of
// them costs time alone, under whatever wall-clock budget the request it
// arrives on already carries (helpers.MetadataFetchDeadline,
// helpers.ArtifactDownloadDeadline).
//
// The first property is why this package exists at all. pgzip's own Read ends
// a finished member with `return z.Read(p)`, and Go eliminates no tail call,
// so a stream of members producing no output recurses once per member until
// the goroutine stack is gone - an unrecoverable "fatal error: stack overflow"
// chosen by the archive's own bytes. Reader.Read below is a loop over the same
// two pgzip primitives that recursion uses (Multistream(false), then Reset),
// so a member costs no stack, and helpers.ErrEmptyGzipMember refuses the first
// member that produces nothing rather than counting how many follow it. The
// refusal is therefore O(1) in members: the flood is answered at its first
// member, whatever number the archive put behind it.
//
// A gzip member producing no bytes cannot be part of anything this program
// reads - a tar stream is at least the 1024-byte end marker its two zero
// blocks make, and store.Store.MarshalSnapshot never produces zero bytes - so
// the rule is a property of the input rather than a threshold somebody picked.
// helpers.ErrEmptyGzipMember's own doc comment holds that argument and the
// reason the sentinel joins no exit-code predicate.
//
// Concatenation itself stays supported: Multistream(false) plus an explicit
// Reset per member is exactly what pgzip's own multistream loop does, so a
// stream of members that carry bytes reads back as their concatenation, and
// trailing garbage still ends the read with pgzip's own "gzip: invalid header"
// rather than with anything this package invented.
//
// The O(1) above is the refusal and not the walk. A stream this reader ACCEPTS
// pays a member boundary per member, and what one costs is pgzip's rather
// than this loop's: crossing it kills and restarts a readahead goroutine and
// refills a block pool, whether pgzip's own multistream loop crosses it or
// this one does. Measured on go1.26.6, darwin/arm64 (Apple M3 Pro) over
// 200,000 accepted 25-byte members: 654 ms, 14 allocations and 35.9 KiB of
// allocation per member here, against 621 ms and 13 allocations at the same
// 35.9 KiB through pgzip's own loop, where compress/gzip reads the identical
// stream in 40 ms and 17 allocations altogether. Nothing in this package
// bounds that, and what a reader can be handed is its own ceiling over the
// twenty-odd bytes a member costs its author: about 12 million members inside
// helpers.StateObjectMaxCompressedSize, about 200 million inside
// helpers.ArtifactMaxDownloadSize, each read under whatever wall-clock budget
// it already carries (helpers.StateObjectDeadline,
// helpers.ArtifactDownloadDeadline).
package gzipstream

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/klauspost/pgzip"
)

// Reader reads a gzip stream, member by member, under a context.
//
// src is held because a member boundary needs it: pgzip's Reset takes the
// source reader again, and it has to be the very same *bufio.Reader the
// constructor handed the decompressor. Two independent, measured reasons stop
// that field from being narrowed to a plain io.Reader (go1.26.6,
// darwin/arm64). pgzip's makeReader returns a source that already satisfies
// flate.Reader unchanged and wraps anything else in a bufio.Reader of its own,
// so handing Reset a source that is not one - contextReader below is exactly
// that - gives every member a FRESH buffer and discards whatever the previous
// member's buffer had already read past the boundary: a three-member stream
// then reads back as its first member alone, ending "AAAA stop: EOF" where the
// whole stream is "AAAA BBBB CCCC".
// And klauspost/flate type-asserts *bufio.Reader for its fast decode path
// (inflate_gen.go's `case *bufio.Reader`), so a bespoke io.ByteReader of the
// same shape would silently drop every read to the generic Huffman path.
type Reader struct {
	z   *pgzip.Reader
	src *bufio.Reader
	// err is the terminal verdict, remembered so every later Read repeats it
	// rather than touching the decompressor again. Read's own doc comment
	// holds why this type has to own that stickiness.
	err error
	// n counts the decompressed bytes the CURRENT member has produced, and is
	// reset at every member boundary. Zero at a member's end is the refusal.
	n int64
}

// NewReader opens r as a gzip stream under ctx, with pgzip's default block
// sizing - the sizing an unpack of a whole artifact wants.
//
// The wrapping order is load-bearing in both directions: the context reader
// sits under the buffer so a stalled or canceled source is observed on the
// bytes actually taken off it, and the buffer sits directly under pgzip so the
// decompressor and every Reset below share one buffered view of the stream.
func NewReader(ctx context.Context, r io.Reader) (*Reader, error) {
	src := bufio.NewReader(&contextReader{ctx: ctx, r: r})
	z, err := pgzip.NewReader(src)
	if err != nil {
		return nil, err
	}
	// Multistream(false) is what turns pgzip's own member loop off, so the one
	// below replaces it rather than running alongside it.
	z.Multistream(false)
	return &Reader{z: z, src: src}, nil
}

// NewReaderN opens r as a gzip stream under ctx with the caller's own block
// sizing, for a caller reading the front of an archive rather than unpacking
// one. It wraps r exactly as NewReader does, for the same reasons.
func NewReaderN(ctx context.Context, r io.Reader, blockSize, blocks int) (*Reader, error) {
	src := bufio.NewReader(&contextReader{ctx: ctx, r: r})
	z, err := pgzip.NewReaderN(src, blockSize, blocks)
	if err != nil {
		return nil, err
	}
	z.Multistream(false)
	return &Reader{z: z, src: src}, nil
}

// Read fills p from the current member and, at a member boundary, advances to
// the next one - iteratively, which is the whole point of this type.
//
// The three non-boundary outcomes are handed straight back. Bytes are returned
// alone, without the error pgzip paired with them, because pgzip pairs none:
// its readahead goroutine truncates a short final block and delivers io.EOF in
// a block of its own carrying no bytes (gunzip.go's doReadAhead), so the
// "n > 0" arm never drops an ending - and if it ever did, this package's
// multi-member test would lose each member's tail. A (0, nil) return is
// pgzip's own way of reporting that a hard error is now pending - it stashes
// the error and returns nothing on that one call - so passing it through is
// what lets the next call surface the error itself.
//
// The stickiness is this type's own, and it has to be: the hang it prevents
// is pgzip's own in the Multistream(false) mode this loop must run it in.
// pgzip's Read returns the final zero-length block to the pool on its
// lastBlock arm (gunzip.go's `z.blockPool <- z.current`) immediately before
// answering (0, io.EOF), so a member ends with the pool full, z.current nil and
// z.lastBlock set; a later Read then skips its channel receive, copies
// nothing, and sends into that full z.blockPool - a send with no select and
// no context, which no deadline and no cancellation can break. A Reset that
// FAILS - the only kind a terminal return here ever follows - repairs none of
// that: readHeader returns before the doReadAhead that clears z.lastBlock, and
// Reset records its own failure nowhere, since it sets z.err to nil on the way
// in and readHeader's error return never touches it. So the second Read finds
// neither an error to stop it nor any state saying the stream ended. A Reset
// that succeeds does reach doReadAhead and does clear z.lastBlock, which is
// what lets this loop cross an ordinary member boundary at all. Measured on
// go1.26.6, darwin/arm64 (Apple M3 Pro): raw pgzip under Multistream(false),
// drained of one 25-byte member and read once more, blocked past a
// two-second ceiling with no Reset having run at all - as did the same
// reader over one 20-byte empty member, and the same reader after a Reset
// over the exhausted source - while raw pgzip with multistream left ON
// answered that first fixture (0, io.EOF) at once and repeated it. A second
// pgzip Read past the ending blocked past a three-second ceiling against a
// single small member, one 8 MiB member and three members alike.
// Remembering the verdict is therefore not a convenience for a caller that
// reads past an error - it is what keeps this loop from inheriting a hang
// the mode it must use carries.
//
// Three returns below are terminal and all three set the field. Two of them
// need it measurably: the boundary Reset, where an ordinary ending and a
// trailing-garbage ErrHeader both arrive, and the member refusal, since pgzip
// leaves z.err nil at a member's own io.EOF and a second Read parks on the
// same full pool with no Reset having run at all. The third, a hard error
// pgzip reports from inside a member, is already sticky in pgzip's own z.err
// (gunzip.go stores it before returning and answers the next Read from it),
// and sets the field anyway so that the verdict is this type's own rather than
// a property of which arm produced it.
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for {
		n, err := r.z.Read(p)
		r.n += int64(n)
		switch {
		case n > 0:
			return n, nil
		case err == nil:
			return 0, nil
		case !errors.Is(err, io.EOF):
			r.err = err
			return 0, err
		}

		// The member ended. A member that produced nothing is the refusal this
		// package exists for, and it is checked before the Reset that would
		// otherwise move on to the next one.
		if r.n == 0 {
			r.err = helpers.ErrEmptyGzipMember
			return 0, r.err
		}
		if err := r.z.Reset(r.src); err != nil {
			// io.EOF here means the stream really ended, which is the answer
			// this Read owes its caller. Anything else is pgzip's own verdict
			// on what follows the member - "gzip: invalid header" for trailing
			// garbage - and is returned unchanged rather than renamed.
			r.err = err
			return 0, err
		}
		// Reset re-enables multistream (gunzip.go sets z.multistream = true
		// unconditionally), so the loop has to disarm it again or pgzip
		// resumes the recursion this type replaced.
		r.z.Multistream(false)
		r.n = 0
	}
}

// Close releases the decompressor's readahead goroutine and block pool. It
// does not close the source reader, which the caller owns.
func (r *Reader) Close() error {
	return r.z.Close()
}

// contextReader fails a read once ctx is done, so a decompression stops at the
// next read taken off the compressed source instead of running the stream out.
// It reports ctx.Err() unwrapped, keeping context.Canceled and
// context.DeadlineExceeded reachable through errors.Is for
// cmd/go-galaxy/exitcode to classify - the same shape and the same reason as
// the decompressed-side readers in internal/galaxy/archive and
// internal/galaxy/manifest, which this one does not replace: a member whose
// deflate stream is nothing but zero-length stored blocks produces no
// decompressed byte for those to fire on, and a member that produces bytes
// steadily is stopped by whichever of the two observes the cancellation first.
//
// It holds the context in a field rather than taking one per call, which is
// the only shape io.Reader allows: Read's signature is fixed, so a reader that
// must observe cancellation has nowhere else to keep it.
//
//nolint:containedctx // io.Reader cannot take a context per call
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
