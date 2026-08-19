package gzipstream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// gzipMemberHeader is the ten bytes every member below opens with: the gzip
// magic, deflate as the method, no flags, no mtime, no extra flags, and 0xff
// for "unknown" as the operating system.
const gzipMemberHeader = "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff"

// emptyMember is the smallest gzip member there is: the ten-byte header, one
// fixed-Huffman final block carrying nothing (0x03 0x00), and an eight-byte
// trailer over zero bytes. It is hand-spelled rather than produced by a writer
// because its length is the point - twenty bytes is what one member of a flood
// costs its author on the wire, and a fixture built by compress/gzip would
// quietly pay 23 for the same nothing.
//
// Measured on go1.26.6, darwin/arm64 (Apple M3 Pro): klauspost/pgzip accepts
// this member and reads zero bytes out of it with a nil error, and 3,200,000
// of them concatenated - 64,000,000 bytes on the wire - kill a process reading
// them through pgzip's own Reader with "fatal error: stack overflow" against a
// 512 MiB goroutine stack, while 3,000,000 of them (60,000,000 bytes) survive
// and take 21.8 s. That is the defect this package exists to answer; the same
// 3,200,000-member stream is refused here in well under a millisecond
// (measured three times at 450 us, 511 us and 538 us, each including the
// reader's own construction), because the refusal lands on the first member
// and never reaches the second.
func emptyMember() []byte {
	member := make([]byte, 0, 20)
	member = append(member, gzipMemberHeader...)
	member = append(member, 0x03, 0x00)
	return append(member, 0, 0, 0, 0, 0, 0, 0, 0)
}

// contentMember renders one ordinary gzip member carrying payload, through
// compress/gzip rather than through the library under test, so a fixture this
// package accepts is one an independent writer produced.
func contentMember(t *testing.T, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("writing a member payload: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing a member: %v", err)
	}
	return buf.Bytes()
}

// storedBlockMember renders one gzip member whose deflate stream is padBlocks
// zero-length stored blocks followed by a final stored block carrying payload.
// A zero-length stored block costs five bytes on the wire and produces nothing,
// which is the shape that lets a member consume an arbitrary number of wire
// bytes without handing a single decompressed byte to a reader downstream of
// it - the reason this package's context check sits on the compressed side.
//
// A nil payload therefore renders a member that produces nothing at all,
// however large padBlocks makes it.
func storedBlockMember(padBlocks int, payload []byte) []byte {
	member := make([]byte, 0, len(gzipMemberHeader)+padBlocks*5+5+len(payload)+8)
	member = append(member, gzipMemberHeader...)
	for range padBlocks {
		// BFINAL=0, BTYPE=00 (stored), then LEN=0 and its one's complement.
		member = append(member, 0x00, 0x00, 0x00, 0xff, 0xff)
	}
	size := len(payload)
	lo, hi := byte(size&0xff), byte(size>>8&0xff)
	member = append(member, 0x01, lo, hi, ^lo, ^hi)
	member = append(member, payload...)

	sum := crc32.ChecksumIEEE(payload)
	//nolint:gosec // the same fixture length, rendered little-endian.
	return append(member,
		byte(sum), byte(sum>>8), byte(sum>>16), byte(sum>>24),
		byte(size), byte(size>>8), byte(size>>16), byte(size>>24),
	)
}

// buildStream concatenates members and appends trailing verbatim, which is the
// one fixture builder every row of the table below shares: a row that must be
// accepted and a row that must be refused differ only in which members they
// hand this function, never in how the bytes were assembled.
func buildStream(members [][]byte, trailing []byte) []byte {
	size := len(trailing)
	for _, m := range members {
		size += len(m)
	}
	out := make([]byte, 0, size)
	for _, m := range members {
		out = append(out, m...)
	}
	return append(out, trailing...)
}

// emptyTarStream is a well-formed tar carrying no entries: the two 512-byte
// zero blocks that end every tar stream, and nothing else. It is the shape
// archive.ProbeTarGz documents itself as accepting, so the member rule must
// not refuse it - a member producing 1024 bytes produces bytes.
func emptyTarStream() []byte {
	return make([]byte, 1024)
}

// TestReaderMemberRules drives one stream shape per row through Reader, all
// built by buildStream out of the member constructors above.
//
// The accepted rows are the refusals' positive control and are not decoration:
// "content then an empty member" is refused while "two members carrying
// content read back concatenated" builds that same two-member shape out of the
// same carrier and is accepted - so the refusal names the second member's
// emptiness, not a reader unable to walk past a boundary at all.
//
// Killing mutations, all three run, and all three land on the one assertion
// this table shares - what distinguishes them is which row reaches it, which
// is why the rows below are not three spellings of one fixture. Deleting
// z.Multistream(false) from NewReader leaves pgzip's own member loop in charge
// and accepts both two-or-more-member refusals, the first of them:
//
//	gzipstream_test.go:160: content then an empty member: error = <nil>, want gzip stream carries a member that produces no bytes
//
// Deleting the zero-byte member refusal - the `if r.n == 0` arm in Read - and
// letting the loop Reset straight past such a member accepts every refusal row
// there is, the single-member one included:
//
//	gzipstream_test.go:160: a single empty member: error = <nil>, want gzip stream carries a member that produces no bytes
//
// Dropping only the Multistream(false) re-arm after Reset leaves the first
// boundary correct and every later one not, so exactly one row fails - the
// three-member one, which is what that row is in the table for:
//
//	gzipstream_test.go:160: content twice, then an empty member: error = <nil>, want gzip stream carries a member that produces no bytes
func TestReaderMemberRules(t *testing.T) {
	t.Parallel()

	for _, tt := range memberCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewReader(t.Context(), bytes.NewReader(tt.stream))
			if err != nil {
				t.Fatalf("%s: NewReader = %v, want a reader", tt.name, err)
			}
			defer func() {
				_ = r.Close()
			}()

			got, err := io.ReadAll(r)
			assertMemberOutcome(t, tt, got, err)
		})
	}
}

// memberCases builds the table TestReaderMemberRules drives, every row of it
// out of buildStream and the member constructors above. It is a function of
// its own only so the test that runs it stays inside the length budget the
// linter enforces.
func memberCases(t *testing.T) []memberCase {
	t.Helper()

	content := []byte("collection artifact bytes")
	// One rendered member, reused wherever a row wants a second copy of it, so
	// a refusal row and its positive control differ only in what follows.
	carrier := contentMember(t, content)

	return []memberCase{
		{
			name:   "one member carrying content",
			stream: buildStream([][]byte{carrier}, nil),
			want:   content,
		},
		{
			name:   "two members carrying content read back concatenated",
			stream: buildStream([][]byte{carrier, carrier}, nil),
			want:   append(append([]byte{}, content...), content...),
		},
		{
			name:   "a well-formed empty tar",
			stream: buildStream([][]byte{contentMember(t, emptyTarStream())}, nil),
			want:   emptyTarStream(),
		},
		{
			name:    "a single empty member",
			stream:  buildStream([][]byte{emptyMember()}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:    "content then an empty member",
			stream:  buildStream([][]byte{carrier, emptyMember()}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:    "content twice, then an empty member",
			stream:  buildStream([][]byte{carrier, carrier, emptyMember()}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:    "a member of zero-length stored blocks producing nothing",
			stream:  buildStream([][]byte{storedBlockMember(4096, nil)}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:     "content then trailing garbage",
			stream:   buildStream([][]byte{carrier}, []byte("no member starts here")),
			wantText: "gzip: invalid header",
		},
	}
}

// memberCase is one row of the table above: a stream, and the one outcome it
// must produce - a sentinel, a verbatim error message, or bytes.
type memberCase struct {
	wantErr  error
	name     string
	wantText string
	stream   []byte
	want     []byte
}

// assertMemberOutcome checks one row's outcome, split out of the loop so that
// the loop stays within the cyclomatic budget the linter enforces. It carries
// no logic beyond picking which of the three expectations the row declared,
// and calls t.Helper(), so a failure is reported on the call to it.
func assertMemberOutcome(t *testing.T, c memberCase, got []byte, err error) {
	t.Helper()

	switch {
	case c.wantErr != nil:
		if !errors.Is(err, c.wantErr) {
			t.Fatalf("%s: error = %v, want %v", c.name, err, c.wantErr)
		}
	case c.wantText != "":
		if err == nil || err.Error() != c.wantText {
			t.Fatalf("%s: error = %v, want %q", c.name, err, c.wantText)
		}
	default:
		if err != nil {
			t.Fatalf("%s: error = %v, want nil", c.name, err)
		}
		if !bytes.Equal(got, c.want) {
			t.Fatalf("%s: read %q, want %q", c.name, got, c.want)
		}
	}
}

// tarAcrossMembers renders one continuous tar stream of entryCount entries and
// then chops those bytes into memberCount pieces, each gzipped as a member of
// its own. Nothing about the split respects tar's own framing, so an entry's
// header and its body routinely land in different members - which is what
// makes reading it back a claim about the boundary rather than about gzip.
func tarAcrossMembers(t *testing.T, entryCount, memberCount int) ([]byte, []string) {
	t.Helper()

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	names := make([]string, 0, entryCount)
	for i := range entryCount {
		name := fmt.Sprintf("file-%02d.txt", i)
		body := bytes.Repeat([]byte{byte('a' + i)}, 700)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatalf("writing a tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("writing a tar body: %v", err)
		}
		names = append(names, name)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar writer: %v", err)
	}

	tarBytes := raw.Bytes()
	chunk := len(tarBytes) / memberCount
	members := make([][]byte, 0, memberCount)
	for i := range memberCount {
		start := i * chunk
		end := start + chunk
		if i == memberCount-1 {
			end = len(tarBytes)
		}
		members = append(members, contentMember(t, tarBytes[start:end]))
	}
	return buildStream(members, nil), names
}

// TestReaderReadsOneTarSplitAcrossManyMembers reads a single tar stream whose
// bytes were split across eight gzip members, which is the property the loop
// in Read exists to keep: pgzip's own multistream handling would deliver the
// same bytes by recursing once per member, and this one must deliver them
// without a boundary being observable to the tar walk at all.
//
// It is the test that proves the *bufio.Reader is owned by this package rather
// than minted by pgzip per member. Reset over a source pgzip has to wrap itself
// hands each member a fresh buffer and drops whatever the previous member's
// buffer had read past the boundary, which no single-member fixture can see.
//
// Run under -race it is also the only place two goroutines touch this type's
// state at a member boundary: pgzip's readahead goroutine is killed and
// restarted by every Reset, seven times here.
//
// Killing mutation, run: handing pgzip the contextReader directly instead of
// the *bufio.Reader wrapping it - which is what makes pgzip's own makeReader
// mint a buffer per member, discarding what the previous one had read past the
// boundary - fails this test partway through the first member's share of the
// entries, the rest of the tar having gone missing with that buffer:
//
//	gzipstream_test.go:344: tar.Next after 3 entries = unexpected EOF, want the next entry
func TestReaderReadsOneTarSplitAcrossManyMembers(t *testing.T) {
	t.Parallel()

	const (
		entryCount  = 24
		memberCount = 8
	)
	stream, names := tarAcrossMembers(t, entryCount, memberCount)

	r, err := NewReader(t.Context(), bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader = %v, want a reader", err)
	}
	defer func() {
		_ = r.Close()
	}()

	var got []string
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next after %d entries = %v, want the next entry", len(got), err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading the body of %s: %v", header.Name, err)
		}
		if len(body) != 700 {
			t.Fatalf("%s carries %d bytes, want 700", header.Name, len(body))
		}
		got = append(got, header.Name)
	}

	if len(got) != entryCount {
		t.Fatalf("read %d entries, want %d", len(got), entryCount)
	}
	for i, name := range names {
		if got[i] != name {
			t.Fatalf("entry %d is %s, want %s", i, got[i], name)
		}
	}
}

// cancelOnRead cancels ctx as it serves read number after, so a test can stop
// a decompression at a chosen point on the COMPRESSED side deterministically
// instead of racing a timer against it.
type cancelOnRead struct {
	r      io.Reader
	cancel context.CancelFunc
	after  int
	reads  int
}

func (c *cancelOnRead) Read(p []byte) (int, error) {
	c.reads++
	if c.reads == c.after {
		c.cancel()
	}
	return c.r.Read(p)
}

// TestReaderObservesCancellationOnTheCompressedSide drives the shape a
// decompressed-side check cannot see: a member of zero-length stored blocks,
// 64 KiB of wire bytes that produce nothing, with a payload at the end so the
// same fixture is capable of being accepted.
//
// TestReaderAcceptsTheSameStoredBlockMemberUncanceled is that positive
// control, on this very fixture: without it, "it stopped" would be
// indistinguishable from a member no reader could have walked in the first
// place.
//
// The cancellation lands on the source's second read rather than its first, so
// the reader is already inside the member when it fires: bufio refills 4,096
// bytes at a time against a member of more than 64 KiB, so a dozen more reads
// were still to come.
//
// Killing mutation, run: deleting the contextReader from both constructors and
// handing pgzip the bufio.Reader over the caller's own source fails this test
// with
//
//	gzipstream_test.go:421: reading a canceled stored-block member = <nil>, want errors.Is context.Canceled
func TestReaderObservesCancellationOnTheCompressedSide(t *testing.T) {
	t.Parallel()

	stream := buildStream([][]byte{storedBlockMember(13107, []byte("payload"))}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &cancelOnRead{r: bytes.NewReader(stream), cancel: cancel, after: 2}

	r, err := NewReader(ctx, src)
	if err != nil {
		t.Fatalf("NewReader = %v, want a reader", err)
	}
	defer func() {
		_ = r.Close()
	}()

	if _, err := io.ReadAll(r); !errors.Is(err, context.Canceled) {
		t.Fatalf("reading a canceled stored-block member = %v, want errors.Is context.Canceled", err)
	}
}

// TestReaderAcceptsTheSameStoredBlockMemberUncanceled is the positive control
// named on TestReaderObservesCancellationOnTheCompressedSide: the identical
// fixture, read under a live context, hands back the payload its final block
// carries.
func TestReaderAcceptsTheSameStoredBlockMemberUncanceled(t *testing.T) {
	t.Parallel()

	stream := buildStream([][]byte{storedBlockMember(13107, []byte("payload"))}, nil)

	r, err := NewReader(t.Context(), bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader = %v, want a reader", err)
	}
	defer func() {
		_ = r.Close()
	}()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the same member uncanceled = %v, want nil", err)
	}
	if string(got) != "payload" {
		t.Fatalf("read %q, want %q", got, "payload")
	}
}

// TestNewReaderNAppliesTheSameMemberRules drives NewReaderN through the member
// rule and its positive control, at the block sizing archive.ProbeTarGz asks
// for - spelled inline rather than imported, since this package must not
// depend on a caller of its own.
//
// It exists because the defenses NewReaderN installs were reachable only
// through a constructor no test called, and a flood of members carrying
// nothing cannot tell the two constructors apart: the refusal fires either
// way, and only the cost changes, from one stack frame to one per member. A
// fixture whose FIRST member carries content is what separates them, since
// pgzip's own loop then walks past the empty second member and reports
// success. That is row one; row two is its positive control, the same carrier
// twice, so the refusal names the second member's emptiness rather than a
// reader unable to cross a boundary at this sizing at all.
//
// Killing mutation, run: deleting z.Multistream(false) from NewReaderN alone -
// NewReader's own left in place, so every test above stays green - hands
// pgzip's member loop the second member and fails row one with
//
//	gzipstream_test.go:507: content then an empty member: error = <nil>, want gzip stream carries a member that produces no bytes
//
// The other defense this constructor installs, the compressed-side context
// reader, is deliberately NOT pinned here: neither row drives a canceled
// context, so a reader observing none reads both fixtures identically. What
// pins that one is archive.TestProbeTarGzReportsCancellationAsItself, which
// reaches this constructor through ProbeTarGz.
func TestNewReaderNAppliesTheSameMemberRules(t *testing.T) {
	t.Parallel()

	content := []byte("collection artifact bytes")
	carrier := contentMember(t, content)

	for _, tt := range []memberCase{
		{
			name:    "content then an empty member",
			stream:  buildStream([][]byte{carrier, emptyMember()}, nil),
			wantErr: helpers.ErrEmptyGzipMember,
		},
		{
			name:   "two members carrying content read back concatenated",
			stream: buildStream([][]byte{carrier, carrier}, nil),
			want:   append(append([]byte{}, content...), content...),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewReaderN(t.Context(), bytes.NewReader(tt.stream), 64<<10, 1)
			if err != nil {
				t.Fatalf("%s: NewReaderN = %v, want a reader", tt.name, err)
			}
			defer func() {
				_ = r.Close()
			}()

			got, err := io.ReadAll(r)
			assertMemberOutcome(t, tt, got, err)
		})
	}
}
