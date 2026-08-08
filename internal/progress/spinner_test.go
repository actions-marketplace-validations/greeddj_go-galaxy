package progress

import (
	"bytes"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

// The escape sequences these tests assert on, spelled out by hand rather than
// taken from the production constants: an assertion built from the value it
// checks passes whatever that value becomes, so a rewritten sequence would
// silently keep every test green. termEnv and dumbTerm are here for that same
// reason and not because they are escape sequences - they are fixture inputs
// rather than expectations, and a row that named the production constant
// would follow it wherever it moved.
const (
	hideCursorSeq  = "\x1b[?25l"
	autowrapOffSeq = "\x1b[?7l"
	autowrapOnSeq  = "\x1b[?7h"
	eraseSeq       = "\r\x1b[K"
	leaveSeq       = "\x1b[?25h\r\x1b[K"
	greenSeq       = "\x1b[32m"
	resetSeq       = "\x1b[0m"
	termEnv        = "TERM"
	dumbTerm       = "dumb"
)

// Timings and workloads for the tests below. The two tick delays are far
// shorter than production's so a test does not spend a tenth of a second
// waiting for a frame; the drain budget is generous because it bounds a
// scheduler, not a computation.
const (
	staleTickDelay        = 5 * time.Millisecond
	staleHandoffWait      = 50 * time.Millisecond
	suffixRaceTickDelay   = time.Millisecond
	goroutineDrainTimeout = 2 * time.Second
	goroutinePollInterval = 5 * time.Millisecond
	frameWaitDeadline     = 2 * time.Second
	framePollInterval     = time.Millisecond
	minFramesBeforeHold   = 2
	spinnerRounds         = 3
	suffixRaceWorkers     = 8
	suffixRaceIterations  = 200
)

// syncBuffer is the fixture every test in this file writes frames into: a
// bytes.Buffer a render goroutine and the test goroutine may both touch,
// since a spinner under test is generally still running when its output is
// read. It counts Write calls as well as bytes, because how many writes a
// frame costs is itself a property one test asserts.
type syncBuffer struct {
	buf    bytes.Buffer
	writes int
	mu     sync.Mutex
}

// Write appends to the buffer under the fixture's lock.
func (b *syncBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes++
	return b.buf.Write(payload)
}

// String returns everything written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Writes returns how many Write calls the fixture has received.
func (b *syncBuffer) Writes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writes
}

// Reset empties the fixture so one buffer can back every row of a table.
func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes = 0
	b.buf.Reset()
}

// firstFrameGlyph is the glyph a freshly started spinner draws. It is derived
// from the production frame set rather than re-spelled, because which glyph
// comes first is not what any test here is about.
func firstFrameGlyph() string {
	return string([]rune(spinnerFrames)[0])
}

// TestSpinnerRendersOnlyWhenRenderingIsEnabled pins the gate every other
// behavior in this file sits behind: a spinner told not to render writes
// nothing at all, and one told to render draws a frame and hides the cursor
// before it returns from start.
func TestSpinnerRendersOnlyWhenRenderingIsEnabled(t *testing.T) {
	var out syncBuffer
	for _, tc := range []struct {
		name   string
		render bool
	}{
		{name: "rendering enabled", render: true},
		{name: "rendering disabled", render: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			s := newSpinner(&out, tc.render, false)
			s.start()
			defer s.stop()

			got := out.String()
			if !tc.render {
				// Mutation, run rather than predicted: dropping `!s.render ||`
				// from start's guard fails this row with
				// `spinner_test.go:125: rendering disabled, wrote "\x1b[?25l\x1b[?7l\r\x1b[K⠋\x1b[?7h"`.
				if got != "" {
					t.Fatalf("rendering disabled, wrote %q", got)
				}
				return
			}
			if got == "" {
				t.Fatal("rendering enabled, wrote nothing")
			}
			if glyph := firstFrameGlyph(); !strings.Contains(got, glyph) {
				t.Fatalf("frame glyph %q missing from %q", glyph, got)
			}
			if !strings.Contains(got, hideCursorSeq) {
				t.Fatalf("cursor was not hidden in %q", got)
			}
		})
	}
}

// TestSpinnerFrameOccupiesOneLine pins the property that lets a single-line
// erase clear a whole frame: whatever the suffix carries, the frame is one
// physical line. The second assertion spells out every byte of that frame,
// and it reads the buffer as a prefix rather than a suffix so it stays
// deterministic - no tick can land at the production delay between start and
// the read, but a later one would append a second frame. The plain row is the
// positive control: it proves the fixture renders a suffix at all, so the
// newline row's verdict cannot be "nothing was drawn".
func TestSpinnerFrameOccupiesOneLine(t *testing.T) {
	var out syncBuffer
	for _, tc := range []struct {
		name   string
		suffix string
		want   string
	}{
		{name: "plain suffix", suffix: " ab", want: " ab"},
		{name: "embedded newline", suffix: " a\nb", want: " a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			s := newSpinner(&out, true, false)
			s.setSuffix(safeout.Clean(tc.suffix))
			s.start()
			defer s.stop()

			got := out.String()
			// Mutation, run rather than predicted: writing the suffix without
			// the firstLine call fails the newline row with
			// `spinner_test.go:172: frame spans more than one line: "\x1b[?25l\x1b[?7l\r\x1b[K⠋ a\nb\x1b[?7h"`.
			if strings.ContainsRune(got, '\n') {
				t.Fatalf("frame spans more than one line: %q", got)
			}
			want := hideCursorSeq + autowrapOffSeq + eraseSeq + firstFrameGlyph() + tc.want + autowrapOnSeq
			if !strings.HasPrefix(got, want) {
				t.Fatalf("frame = %q, want prefix %q", got, want)
			}
		})
	}
}

// TestSpinnerFrameCycleRepeatsAndWritesOnce drives the frame builder directly,
// with no clock and no goroutine, twice around the frame set and one frame
// further. It states the whole byte stream, which is what makes it the pin for
// the glyph index wrapping around instead of running off the end, for the
// erase leading every frame rather than only the first, and for the index
// advancing at all. The write count is asserted for its own sake: composing a
// frame in one buffer and handing it over whole is what keeps any reader of
// that writer from ever seeing half an escape sequence, so it is a promise
// rather than an implementation detail.
func TestSpinnerFrameCycleRepeatsAndWritesOnce(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, true, false)
	frames := []rune(spinnerFrames)
	count := 2*len(frames) + 1

	var want strings.Builder
	s.mu.Lock()
	for i := range count {
		s.writeFrameLocked()
		want.WriteString(autowrapOffSeq + eraseSeq + string(frames[i%len(frames)]) + autowrapOnSeq)
	}
	s.mu.Unlock()

	// Mutation, run rather than predicted: replacing the modulo with a bare
	// `s.next++` does not fail this test, it takes the process down with
	// `panic: runtime error: index out of range [10] with length 10 [recovered, repanicked]`.
	if got := out.String(); got != want.String() {
		t.Fatalf("frame stream = %q, want %q", got, want.String())
	}
	if got := out.Writes(); got != count {
		t.Fatalf("%d writes for %d frames, want one write each", got, count)
	}
}

// TestSpinnerFrameBracketsItsOwnAutowrap pins the scope of the autowrap mode,
// which is one frame and not one run: the off sequence precedes the glyph, the
// on sequence follows it, and the two are used the same number of times across
// the whole stream, so no frame can leave the mode set behind it. The trailing
// check is what states the rest of the restore - a stopped spinner ends on the
// sequence that shows the cursor again and clears its line.
func TestSpinnerFrameBracketsItsOwnAutowrap(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, true, false)
	s.start()
	s.stop()

	got := out.String()
	off := strings.Index(got, autowrapOffSeq)
	on := strings.Index(got, autowrapOnSeq)
	glyph := strings.Index(got, firstFrameGlyph())

	// Mutations, run rather than predicted, one per sequence, both dropped
	// from the frame builder. Without spinnerWrapOffSeq this test fails with
	// `spinner_test.go:239: no frame disabled autowrap: "\x1b[?25l\r\x1b[K⠋\x1b[?7h\x1b[?25h\r\x1b[K"`,
	// and without spinnerWrapOnSeq it fails with
	// `spinner_test.go:242: no frame restored autowrap: "\x1b[?25l\x1b[?7l\r\x1b[K⠋\x1b[?25h\r\x1b[K"`.
	if off < 0 {
		t.Fatalf("no frame disabled autowrap: %q", got)
	}
	if on < 0 {
		t.Fatalf("no frame restored autowrap: %q", got)
	}
	if glyph < 0 {
		t.Fatalf("no frame glyph in %q", got)
	}
	if off > glyph || on < glyph {
		t.Fatalf("frame does not bracket its glyph: off=%d glyph=%d on=%d", off, glyph, on)
	}
	offCount, onCount := strings.Count(got, autowrapOffSeq), strings.Count(got, autowrapOnSeq)
	if offCount != onCount {
		t.Fatalf("autowrap disabled %d times, restored %d times, in %q", offCount, onCount, got)
	}
	// Mutation, run rather than predicted: dropping `\x1b[?25h` from
	// spinnerLeaveSeq reaches this check and fails it with
	// `spinner_test.go:258: a stopped spinner did not end on its restore sequence: "\x1b[?25l\x1b[?7l\r\x1b[K⠋\x1b[?7h\r\x1b[K"`.
	if !strings.HasSuffix(got, leaveSeq) {
		t.Fatalf("a stopped spinner did not end on its restore sequence: %q", got)
	}
}

// TestSpinnerFrameColorFollowsTheTerminal covers both inputs to the color
// decision, each on its own row: the caller's verdict about the destination,
// and TERM. Every row asserts the glyph reached the buffer before it states
// the frame's exact bytes, so an uncolored verdict can never be a spinner that
// drew nothing; the first row is the positive control for the two that expect
// no color. The exact form is what makes the color assertion real - a
// containment check would still hold for a sequence that merely has the
// expected one as a tail.
func TestSpinnerFrameColorFollowsTheTerminal(t *testing.T) {
	var out syncBuffer
	for _, tc := range []struct {
		name         string
		term         string
		colorAllowed bool
		wantColor    bool
	}{
		{name: "color allowed on a capable terminal", term: "xterm-256color", colorAllowed: true, wantColor: true},
		{name: "dumb terminal", term: dumbTerm, colorAllowed: true},
		{name: "color not allowed by the destination", term: "xterm-256color"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(termEnv, tc.term)
			out.Reset()
			s := newSpinner(&out, true, tc.colorAllowed)
			s.start()
			defer s.stop()

			got := out.String()
			glyph := firstFrameGlyph()
			if !strings.Contains(got, glyph) {
				t.Fatalf("frame glyph %q missing from %q", glyph, got)
			}
			if tc.wantColor {
				glyph = greenSeq + glyph + resetSeq
			}
			want := hideCursorSeq + autowrapOffSeq + eraseSeq + glyph + autowrapOnSeq
			// Mutations, run rather than predicted, one per input, each
			// quoted with its expectation half cut short. Changing
			// dumbTerminal to a value no terminal reports fails the dumb row:
			// `spinner_test.go:305: frame = "\x1b[?25l\x1b[?7l\r\x1b[K\x1b[32m⠋\x1b[0m\x1b[?7h", want prefix "\x1b[?25l...`,
			// and setting spinnerColorSeq to ansiGreen fails the first row:
			// `spinner_test.go:305: frame = "\x1b[?25l\x1b[?7l\r\x1b[K\x1b[1m\x1b[32m⠋\x1b[0m\x1b[?7h", want prefix "...`.
			if !strings.HasPrefix(got, want) {
				t.Fatalf("frame = %q, want prefix %q", got, want)
			}
		})
	}
}

// TestSpinnerStaleRenderGoroutineWritesNothing forces the one interleaving
// that the render goroutine's identity check exists for, and it is contrived
// on purpose: the ordinary schedule does not produce it often enough to be
// tested by repetition. Holding the spinner's own mutex from the test queues
// stop first and the woken render goroutine behind it, so the goroutine gets
// the writer only after the restore sequence has already been written - the
// exact moment a stale frame would land on a terminal this spinner no longer
// owns. The wait for a second frame ahead of that hold is what keeps the
// verdict from being vacuous: a fixture whose ticker never fired would satisfy
// the final check without the interleaving ever happening.
func TestSpinnerStaleRenderGoroutineWritesNothing(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, true, false)
	s.delay = staleTickDelay
	s.start()

	deadline := time.Now().Add(frameWaitDeadline)
	for frameCount(&out) < minFramesBeforeHold && time.Now().Before(deadline) {
		time.Sleep(framePollInterval)
	}
	if drawn := frameCount(&out); drawn < minFramesBeforeHold {
		t.Fatalf("%d frames drawn before the hold, want at least %d", drawn, minFramesBeforeHold)
	}

	s.mu.Lock()
	var wg sync.WaitGroup
	wg.Go(s.stop)
	// Long enough for the ticker to fire and the render goroutine to queue on
	// the mutex behind stop, which is what puts it after stop in the handoff.
	time.Sleep(staleHandoffWait)
	s.mu.Unlock()
	wg.Wait()
	time.Sleep(staleHandoffWait)

	// Mutation, run rather than predicted: replacing `s.stopCh == stop` in run
	// with `true` fails this test with (this tail in four of five runs, one
	// carrying a further stale frame; the dump's opening frames are cut)
	// `spinner_test.go:350: a frame landed after the restore sequence: "...\x1b[?25h\r\x1b[K\x1b[?7l\r\x1b[K⠹\x1b[?7h"`.
	if got := out.String(); !strings.HasSuffix(got, leaveSeq) {
		t.Fatalf("a frame landed after the restore sequence: %q", got)
	}
}

// frameCount reports how many frames have reached the fixture, counting the
// one sequence every frame opens with and nothing else writes.
func frameCount(out *syncBuffer) int {
	return strings.Count(out.String(), autowrapOffSeq)
}

// TestSpinnerStopIsIdempotentAndReleasesTheGoroutine covers the three ways the
// lifecycle can be driven badly and must still settle: a second start with no
// stop between it and the first, which must not orphan the first goroutine's
// channel and leave it running forever; repeated restarts; and a second stop
// on a spinner already stopped, which must neither panic on a channel that was
// closed nor write a second restore. What it asserts is the goroutine count
// returning to where it began, so it is the pin for stop actually releasing
// what start spawned.
func TestSpinnerStopIsIdempotentAndReleasesTheGoroutine(t *testing.T) {
	var out syncBuffer
	baseline := runtime.NumGoroutine()

	s := newSpinner(&out, true, false)
	s.delay = staleTickDelay
	s.start()
	s.start()
	for range spinnerRounds {
		s.restart()
	}
	s.stop()
	s.stop()

	deadline := time.Now().Add(goroutineDrainTimeout)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(goroutinePollInterval)
	}
	// Mutation, run rather than predicted: deleting `close(s.stopCh)` from
	// stop leaves every render goroutine parked on its ticker and fails this
	// test with
	// `spinner_test.go:391: goroutine count 6 never returned to the baseline 2`.
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutine count %d never returned to the baseline %d", got, baseline)
	}
}

// TestSpinnerSuffixRoundTrips covers the accessor pair on its own: a fresh
// spinner carries no suffix, and what setSuffix stores is what suffixText
// reports.
func TestSpinnerSuffixRoundTrips(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, false, false)
	if got := s.suffixText(); got != "" {
		t.Fatalf("fresh spinner suffix = %q, want empty", got)
	}

	want := safeout.Clean(" resolving community.general")
	s.setSuffix(want)
	if got := s.suffixText(); got != want {
		t.Fatalf("suffix = %q, want %q", got, want)
	}
}

// TestSpinnerSuffixRace drives concurrent writers and readers of the suffix
// against a render goroutine reading it on every tick, which is the shape
// Printf produces in production. It must stay clean under the race detector.
// The closing assertion is not the point of the test but is not decoration
// either: every writer stores a non-empty value and nothing clears one, so a
// suffix that ended up empty means an update was lost rather than merely
// overwritten.
func TestSpinnerSuffixRace(t *testing.T) {
	var out syncBuffer
	s := newSpinner(&out, true, false)
	s.delay = suffixRaceTickDelay
	s.start()
	defer s.stop()

	var wg sync.WaitGroup
	for range suffixRaceWorkers {
		wg.Go(func() {
			for i := range suffixRaceIterations {
				s.setSuffix(safeout.Clean(" tick " + strconv.Itoa(i)))
			}
		})
		wg.Go(func() {
			for range suffixRaceIterations {
				_ = s.suffixText()
			}
		})
	}
	wg.Wait()

	if s.suffixText() == "" {
		t.Fatal("the suffix is empty after every writer stored a non-empty one")
	}
}

// TestEmitRestoresAutowrapAroundThePersistentLine proves that a line printed
// through a Progress lands between two frames rather than inside one frame's
// autowrap window: the mode is restored by the frame before the line, and
// disabled again only by the frame after it, so the line itself reaches a
// terminal that can wrap it.
func TestEmitRestoresAutowrapAroundThePersistentLine(t *testing.T) {
	var out syncBuffer
	var errOut bytes.Buffer
	p := newProgress(false, false, true, &out, &errOut)
	// The spinner a Progress builds for itself draws on os.Stdout, where this
	// interleaving is not observable. Pointing one at the same buffer the
	// printed line lands in is what makes the order of the two writes visible.
	p.s = newSpinner(&out, true, false)
	p.s.start()

	p.Okf("hello")
	p.Close()

	got := out.String()
	on := strings.Index(got, autowrapOnSeq)
	line := strings.Index(got, "hello")
	off := strings.LastIndex(got, autowrapOffSeq)

	// Mutations, run rather than predicted, the same pair
	// TestSpinnerFrameBracketsItsOwnAutowrap documents, quoted here with each
	// buffer dump cut short. Dropping spinnerWrapOnSeq from the frame builder:
	// `spinner_test.go:476: autowrap was never restored: "\x1b[?25l\x1b[?7l\r\x1b[K⠋\x1b[?25h\r\x1b[K...`,
	// and dropping spinnerWrapOffSeq from it fails this test with
	// `spinner_test.go:479: autowrap was never disabled again: "\x1b[?25l\r\x1b[K⠋\x1b[?7h\x1b[?25h...`.
	if on < 0 {
		t.Fatalf("autowrap was never restored: %q", got)
	}
	if off < 0 {
		t.Fatalf("autowrap was never disabled again: %q", got)
	}
	if line < 0 {
		t.Fatalf("the printed line never reached the buffer: %q", got)
	}
	if on > line || line > off {
		t.Fatalf("expected restore at %d before the line at %d before disable at %d", on, line, off)
	}
}

// TestCloseDropsTheSpinnerAndKeepsResultOutput covers the two halves of what
// Close has to do at once. Dropping the spinner is what makes its restore
// final, since emit restarts a spinner it still finds and would hide the
// cursor again after the run's last line. The second assertion is what keeps
// that from being bought by silencing the result tier: a Progress with no
// spinner still prints, and Okf after Close must reach stdout exactly as it
// would before.
func TestCloseDropsTheSpinnerAndKeepsResultOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newProgress(false, false, true, &out, &errOut)
	if p.s == nil {
		t.Fatal("expected a spinner for verbose=false, quiet=false, terminal=true")
	}

	p.Close()
	if p.s != nil {
		t.Fatal("Close left the spinner in place, so a later line would restart it")
	}

	p.Okf("after close")
	if want := okMark() + "after close\n"; out.String() != want {
		t.Fatalf("out = %q, want %q", out.String(), want)
	}
}
