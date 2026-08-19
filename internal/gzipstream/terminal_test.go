package gzipstream

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

// terminalReadDeadline is how long a Read past a stream's own ending is given
// to come back before this file calls it a hang. It is generous rather than
// tight, and that costs nothing: what it separates is "returns" from "never
// returns", the failure it exists to catch being a channel send with no select
// and no context, so a Read that returns at all returns in microseconds.
const terminalReadDeadline = 3 * time.Second

// readPastTheEnd calls Read on r from a goroutine and reports its outcome,
// failing the test rather than hanging when the call does not come back within
// terminalReadDeadline.
//
// The blocked goroutine is deliberately leaked on that failure, and the caller
// deliberately closes r only after this returns: the send it parks on is into
// pgzip's own block pool, which nothing reachable from here drains, and
// closing the decompressor underneath a live Read would report a data race in
// place of the hang.
func readPastTheEnd(t *testing.T, r *Reader) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		var buf [32]byte
		_, err := r.Read(buf[:])
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(terminalReadDeadline):
		t.Fatalf("Read past the stream's ending did not return within %v, want its verdict repeated", terminalReadDeadline)
		return nil
	}
}

// terminalCase is one row of the table below: a stream, and whether reading it
// to its end is a refusal or an ordinary ending. Both are terminal, which is
// what this table is about; what differs is the verdict that has to come back.
type terminalCase struct {
	name    string
	stream  []byte
	refused bool
}

// terminalCases builds that table. The three accepted rows are the three
// shapes a stream can reach its ending in - one member, one member far larger
// than pgzip's own block, and several members - and the two refused rows are
// the other two terminal returns Read has.
func terminalCases(t *testing.T) []terminalCase {
	t.Helper()

	carrier := contentMember(t, []byte("collection artifact bytes"))
	// 8 MiB is far past pgzip's own default block - 1 MiB, four of them
	// pooled - so this row reaches its ending having cycled the block pool many
	// times rather than inside the first block the small rows never leave.
	large := contentMember(t, bytes.Repeat([]byte("x"), 8<<20))
	garbage := []byte("no member starts here")

	return []terminalCase{
		{name: "a single small member", stream: buildStream([][]byte{carrier}, nil)},
		{name: "one 8 MiB member", stream: buildStream([][]byte{large}, nil)},
		{name: "three members", stream: buildStream([][]byte{carrier, carrier, carrier}, nil)},
		{name: "a single empty member", stream: buildStream([][]byte{emptyMember()}, nil), refused: true},
		{name: "content then trailing garbage", stream: buildStream([][]byte{carrier}, garbage), refused: true},
	}
}

// TestReaderRepeatsItsTerminalVerdict reads each stream to its own ending and
// then reads once more.
//
// Raw pgzip with multistream left on answers that second read idempotently -
// measured on go1.26.6, darwin/arm64 (Apple M3 Pro), (0, io.EOF) twice over
// against the single-member row's fixture - and this package's member loop
// did not. Read's own doc comment holds the mechanism, and it is two rather
// than one across this table: four rows cross a boundary Reset, while the
// empty-member row reaches its verdict with none having run. Both leave a
// later read parked on a send into a full block pool, with no select and no
// context to break it; on the S3 path, read under the distributed lock, that
// leaves the holder's heartbeat renewing a lock this run never releases. What
// the stickiness keeps out is pgzip's own hang in the mode this loop uses.
//
// It is not a claim any caller needs: the readers in this module today all
// stop at the first error. It is a claim about this type, made because the
// property it replaced was pgzip's own and the replacement dropped it.
//
// Killing mutation, run: deleting Read's own `if r.err != nil` guard - the
// three assignments below it left in place, so the field is written and never
// read - fails every row of this table, each of them with
//
//	terminal_test.go:106: Read past the stream's ending did not return within 3s, want its verdict repeated
func TestReaderRepeatsItsTerminalVerdict(t *testing.T) {
	t.Parallel()

	for _, tt := range terminalCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertTerminalVerdictRepeats(t, tt)
		})
	}
}

// assertTerminalVerdictRepeats drains one row's stream and then reads once
// more, checking that the second read repeats the first outcome rather than
// blocking. It is a function of its own only so the loop above stays inside
// the length budget the linter enforces.
//
// What the drain asserts is deliberately coarse - a refusal row refused, an
// accepted row did not - because TestReaderMemberRules already pins which
// verdict each of these fixtures produces. What this one adds is that the
// verdict survives being asked for twice.
func assertTerminalVerdictRepeats(t *testing.T, tt terminalCase) {
	t.Helper()

	r, err := NewReader(t.Context(), bytes.NewReader(tt.stream))
	if err != nil {
		t.Fatalf("%s: NewReader = %v, want a reader", tt.name, err)
	}

	_, drainErr := io.ReadAll(r)
	switch {
	case tt.refused && drainErr == nil:
		t.Fatalf("%s: reading to the end = nil, want a refusal", tt.name)
	case !tt.refused && drainErr != nil:
		t.Fatalf("%s: reading to the end = %v, want nil", tt.name, drainErr)
	}

	// A refusal has to come back as the very error it came back as the first
	// time; an ordinary ending has to come back as io.EOF, which io.ReadAll
	// swallows above and so is named here rather than taken from drainErr.
	want := drainErr
	if want == nil {
		want = io.EOF
	}
	if again := readPastTheEnd(t, r); !errors.Is(again, want) {
		t.Fatalf("%s: reading once more = %v, want %v", tt.name, again, want)
	}

	// Closed here rather than deferred, for the reason readPastTheEnd states:
	// a reader whose Read is still parked must be left alone.
	if err := r.Close(); err != nil {
		t.Fatalf("%s: Close = %v, want nil", tt.name, err)
	}
}
