package progress

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNewProgressSpinnerCreation verifies the spinner is created only for
// the single (verbose=false, quiet=false, terminal=true) combination; every
// other combination of the three booleans must leave the spinner nil.
func TestNewProgressSpinnerCreation(t *testing.T) {
	var out, errOut bytes.Buffer

	p := newProgress(false, false, true, &out, &errOut)
	if p.s == nil {
		t.Fatal("expected spinner to be created for verbose=false, quiet=false, terminal=true")
	}
	p.Close()

	combos := []struct {
		name           string
		verbose, quiet bool
		terminal       bool
	}{
		{"verbose", true, false, true},
		{"quiet", false, true, true},
		{"nonTerminal", false, false, false},
		{"verboseQuiet", true, true, true},
		{"verboseNonTerminal", true, false, false},
	}
	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			p := newProgress(c.verbose, c.quiet, c.terminal, &out, &errOut)
			if p.s != nil {
				t.Fatalf("expected nil spinner for verbose=%v quiet=%v terminal=%v", c.verbose, c.quiet, c.terminal)
			}
			p.Close()
		})
	}
}

// assertBuf fails the test unless buf holds exactly want.
func assertBuf(t *testing.T, buf *bytes.Buffer, want string) {
	t.Helper()
	if got := buf.String(); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// assertEmpty fails the test unless buf is empty.
func assertEmpty(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	if buf.Len() != 0 {
		t.Fatalf("expected empty buffer, got %q", buf.String())
	}
}

// stateA builds a Progress plus its stdout/stderr buffers for state A
// (TTY normal: spinner active).
func stateA() (*Progress, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return newProgress(false, false, true, &out, &errOut), &out, &errOut
}

// TestStateATransient covers Printf and Write for state A: both route through
// the spinner (suffix update / stop-print-restart) instead of the buffer directly.
func TestStateATransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		p.Printf("x")
		assertEmpty(t, out)
		if p.s.Suffix != " x" {
			t.Fatalf("expected spinner suffix %q, got %q", " x", p.s.Suffix)
		}
	})

	t.Run("WriteWithMessage", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, out, "x\n")
	})

	t.Run("WriteEmptyMessage", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		n, err := p.Write([]byte("\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected n=1, got %d", n)
		}
		assertEmpty(t, out)
	})
}

// TestStateAResult covers the result tier for state A: PersistentPrintf and Okf
// emit to stdout, Errorf and Warnf emit to stderr, all despite an active spinner.
func TestStateAResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, out, errOut := stateA()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, out, "x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Okf", func(t *testing.T) {
		p, out, errOut := stateA()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, out, ok+" x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Errorf", func(t *testing.T) {
		p, out, errOut := stateA()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, errOut, fail+" x\n")
		assertEmpty(t, out)
	})

	t.Run("Warnf", func(t *testing.T) {
		p, out, errOut := stateA()
		defer p.Close()
		p.Warnf("x")
		assertBuf(t, errOut, warn+" x\n")
		assertEmpty(t, out)
	})
}

// TestStateADebug covers the debug tier for state A: debug output stays
// suppressed since verbose is false.
func TestStateADebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, out)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, out)
	})
}

// stateB builds a Progress plus its buffers for state B (verbose, no spinner).
func stateB() (*Progress, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return newProgress(true, false, true, &out, &errOut), &out, &errOut
}

// TestStateBTransient covers Printf and Write for state B: no spinner exists,
// so both write a full line directly.
func TestStateBTransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, out, _ := stateB()
		defer p.Close()
		p.Printf("x")
		assertBuf(t, out, "x\n")
	})

	t.Run("Write", func(t *testing.T) {
		p, out, _ := stateB()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, out, "x\n")
	})
}

// TestStateBResult covers the result tier for state B: PersistentPrintf and Okf
// emit to stdout, Errorf to stderr.
func TestStateBResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, out, errOut := stateB()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, out, "x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Okf", func(t *testing.T) {
		p, out, errOut := stateB()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, out, ok+" x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Errorf", func(t *testing.T) {
		p, out, errOut := stateB()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, errOut, fail+" x\n")
		assertEmpty(t, out)
	})
}

// TestStateBDebug covers the debug tier for state B: verbose mode enables
// both Debugf and DebugSincef.
func TestStateBDebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, out, _ := stateB()
		defer p.Close()
		p.Debugf("x")
		assertBuf(t, out, "🚧 Debug: x\n")
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, out, _ := stateB()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		got := out.String()
		if !strings.HasPrefix(got, "⏱️ Debug Timing (") {
			t.Fatalf("expected prefix %q, got %q", "⏱️ Debug Timing (", got)
		}
		if !strings.HasSuffix(got, "): x\n") {
			t.Fatalf("expected suffix %q, got %q", "): x\n", got)
		}
	})
}

// stateC builds a Progress plus its buffers for state C (quiet, no spinner).
func stateC() (*Progress, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return newProgress(false, true, true, &out, &errOut), &out, &errOut
}

// TestStateCSuppressed covers the tiers suppressed by quiet mode: Printf,
// Write and both debug methods must stay silent.
func TestStateCSuppressed(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, out, _ := stateC()
		defer p.Close()
		p.Printf("x")
		assertEmpty(t, out)
	})

	t.Run("Write", func(t *testing.T) {
		p, out, _ := stateC()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertEmpty(t, out)
	})

	t.Run("Debugf", func(t *testing.T) {
		p, out, _ := stateC()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, out)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, out, _ := stateC()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, out)
	})
}

// TestStateCResult covers the result tier for state C: quiet mode never
// suppresses PersistentPrintf, Okf or Errorf; Errorf still targets stderr.
func TestStateCResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, out, errOut := stateC()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, out, "x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Okf", func(t *testing.T) {
		p, out, errOut := stateC()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, out, ok+" x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Errorf", func(t *testing.T) {
		p, out, errOut := stateC()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, errOut, fail+" x\n")
		assertEmpty(t, out)
	})
}

// stateD builds a Progress plus its buffers for state D (non-TTY normal:
// the CI-defect regression case, no spinner, verbose=false, quiet=false).
func stateD() (*Progress, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return newProgress(false, false, false, &out, &errOut), &out, &errOut
}

// TestStateDTransient covers Printf and Write for state D: no spinner
// exists and quiet is false, so both must emit a full line - this is the
// CI-defect regression case (output previously vanished with no TTY).
func TestStateDTransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, out, _ := stateD()
		defer p.Close()
		p.Printf("x")
		assertBuf(t, out, "x\n")
	})

	t.Run("Write", func(t *testing.T) {
		p, out, _ := stateD()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, out, "x\n")
	})
}

// TestStateDResult covers the result tier for state D: it must always emit,
// same as every other state; Errorf targets stderr.
func TestStateDResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, out, errOut := stateD()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, out, "x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Okf", func(t *testing.T) {
		p, out, errOut := stateD()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, out, ok+" x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Errorf", func(t *testing.T) {
		p, out, errOut := stateD()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, errOut, fail+" x\n")
		assertEmpty(t, out)
	})
}

// TestStateDDebug covers the debug tier for state D: debug output stays
// suppressed since verbose is false.
func TestStateDDebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, out, _ := stateD()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, out)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, out, _ := stateD()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, out)
	})
}

// TestClose verifies Close does not panic whether or not a spinner exists.
func TestClose(*testing.T) {
	var out, errOut bytes.Buffer

	withSpinner := newProgress(false, false, true, &out, &errOut)
	withSpinner.Close()

	withoutSpinner := newProgress(true, false, true, &out, &errOut)
	withoutSpinner.Close()
}

// TestPrintfSuffixRace drives concurrent spinner-suffix updates through Printf
// against concurrent reads that hold the spinner lock, mirroring how the render
// goroutine reads Suffix. It must stay clean under the race detector.
func TestPrintfSuffixRace(t *testing.T) {
	p := newProgress(false, false, true, io.Discard, io.Discard)
	if p.s == nil {
		t.Fatal("expected active spinner for the race scenario")
	}
	defer p.Close()

	const (
		workers    = 8
		iterations = 200
	)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range iterations {
				p.s.Lock()
				_ = p.s.Suffix
				p.s.Unlock()
			}
		})
		wg.Go(func() {
			for i := range iterations {
				p.Printf("tick %d", i)
			}
		})
	}
	wg.Wait()
}

// TestConcurrentEmissionSerialized drives every buffer-emitting method from
// many goroutines against shared writers. Without the printer mutex the
// concurrent writes and the Stop/print/Restart sequences race (the detector
// fires); with it, writes are serialized so each line stays intact and the
// emitted-line counts are exact. It also confirms Errorf lands on stderr and
// the stdout methods land on stdout under concurrency.
func TestConcurrentEmissionSerialized(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newProgress(false, false, true, &out, &errOut)
	if p.s == nil {
		t.Fatal("expected active spinner for state A")
	}
	defer p.Close()

	const (
		workers    = 8
		iterations = 25
	)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range iterations {
				p.PersistentPrintf("pp %d", i)
				p.Okf("ok %d", i)
				p.Errorf("err %d", i)
				_, _ = p.Write([]byte("wr\n"))
				p.Printf("pf %d", i) // suffix only, never reaches a buffer
			}
		})
	}
	wg.Wait()

	validStdout := func(line string) bool {
		return strings.HasPrefix(line, "pp ") || strings.HasPrefix(line, ok+" ok ") || line == "wr"
	}
	assertLines(t, out.String(), workers*iterations*3, validStdout, "stdout")

	validStderr := func(line string) bool {
		return strings.HasPrefix(line, fail+" err ")
	}
	assertLines(t, errOut.String(), workers*iterations, validStderr, "stderr")
}

// assertLines splits content into non-trailing lines and asserts the exact
// count and that every line passes valid; stream names the writer for errors.
func assertLines(t *testing.T, content string, want int, valid func(string) bool, stream string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != want {
		t.Fatalf("expected %d %s lines, got %d", want, stream, len(lines))
	}
	for _, line := range lines {
		if !valid(line) {
			t.Fatalf("unexpected or interleaved %s line: %q", stream, line)
		}
	}
}

// TestPackageLevelOkf verifies the standalone package-level Okf helper writes
// to os.Stdout, since it runs before any Progress exists.
func TestPackageLevelOkf(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	Okf("hello %s", "world")

	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("failed to close pipe writer: %v", closeErr)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read pipe: %v", err)
	}

	if want := ok + " hello world\n"; string(got) != want {
		t.Fatalf("expected %q, got %q", want, string(got))
	}
}

// TestPackageLevelErrorf verifies the standalone package-level Errorf helper
// writes to os.Stderr, keeping diagnostics off stdout.
func TestPackageLevelErrorf(t *testing.T) {
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	Errorf("bye %s", "world")

	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("failed to close pipe writer: %v", closeErr)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read pipe: %v", err)
	}

	if want := fail + " bye world\n"; string(got) != want {
		t.Fatalf("expected %q, got %q", want, string(got))
	}
}

// hostileCallerText is a caller-supplied message carrying an ANSI escape
// and a lone CR, and hostileCallerTextClean is its expected sanitized form,
// spelled out by hand rather than computed by calling safeout.Clean: every
// test below asserts against this literal so a broken Clean cannot also
// break the expectation it is being checked against. Both are shared by
// every sanitization test in this file so a single fixture backs every
// tier.
const (
	hostileCallerText      = "before\x1b[31mred\rafter"
	hostileCallerTextClean = "before\ufffd[31mred\ufffdafter"
)

// capturePipe redirects *target (os.Stdout or os.Stderr) to a pipe for the
// duration of fn, and returns everything fn caused to be written to it.
// Used for the package-level Okf/Errorf helpers, which write directly to
// os.Stdout/os.Stderr rather than to an injectable io.Writer.
func capturePipe(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	orig := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	*target = w
	defer func() { *target = orig }()

	fn()

	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("failed to close pipe writer: %v", closeErr)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read pipe: %v", err)
	}
	return string(got)
}

// tierCase is one row of TestTiersSanitizeCallerText's table: a method that
// renders caller-supplied text, and the expected exact line it produces on
// whichever of stdout/stderr it targets (exactly one of wantOut/wantErr is
// set).
type tierCase struct {
	invoke  func(p *Progress, msg string)
	wantOut func(msg, clean string) string // non-empty when the tier lands on stdout
	wantErr func(msg, clean string) string // non-empty when the tier lands on stderr
	name    string
}

// sanitizingTiers is TestTiersSanitizeCallerText's table, factored out to a
// package-level function (rather than inlined in the test body) purely to
// keep that test function's own length within budget.
func sanitizingTiers() []tierCase {
	return []tierCase{
		{
			name:    "Printf",
			invoke:  func(p *Progress, msg string) { p.Printf("%s", msg) },
			wantOut: func(_, clean string) string { return clean + "\n" },
		},
		{
			name:    "PersistentPrintf",
			invoke:  func(p *Progress, msg string) { p.PersistentPrintf("%s", msg) },
			wantOut: func(_, clean string) string { return clean + "\n" },
		},
		{
			name:    "Okf",
			invoke:  func(p *Progress, msg string) { p.Okf("%s", msg) },
			wantOut: func(_, clean string) string { return okPrefix + clean + "\n" },
		},
		{
			name:    "Errorf",
			invoke:  func(p *Progress, msg string) { p.Errorf("%s", msg) },
			wantErr: func(_, clean string) string { return failPrefix + clean + "\n" },
		},
		{
			name:    "Warnf",
			invoke:  func(p *Progress, msg string) { p.Warnf("%s", msg) },
			wantErr: func(_, clean string) string { return warnPrefix + clean + "\n" },
		},
		{
			name:    "Debugf",
			invoke:  func(p *Progress, msg string) { p.Debugf("%s", msg) },
			wantOut: func(_, clean string) string { return debugPrefix + clean + "\n" },
		},
		{
			name:    "Write",
			invoke:  func(p *Progress, msg string) { _, _ = p.Write([]byte(msg)) },
			wantOut: func(_, clean string) string { return clean + "\n" },
		},
	}
}

// runTierCase drives one tierCase against a fresh non-spinner Progress and
// asserts the exact resulting line on whichever stream the tier targets,
// and that the other stream stayed empty.
func runTierCase(t *testing.T, tc tierCase, msg, clean string) {
	t.Helper()
	p, out, errOut := stateB()
	defer p.Close()
	tc.invoke(p, msg)
	if tc.wantOut != nil {
		assertBuf(t, out, tc.wantOut(msg, clean))
		assertEmpty(t, errOut)
		return
	}
	assertBuf(t, errOut, tc.wantErr(msg, clean))
	assertEmpty(t, out)
}

// TestTiersSanitizeCallerText is the core sanitization regression test: one
// table covering every tier that renders caller-supplied text, each with a
// benign row (the positive control, asserting the exact line including its
// own prefix) and a hostile row (asserting the exact sanitized line). The
// method tiers all run against a non-spinner Progress (state B, verbose) so
// Debugf's row actually emits and Printf's row exercises its non-spinner
// branch specifically - the spinner-suffix branch has its own test,
// TestPrintfSpinnerSuffixIsSanitized. DebugSincef and the two package-level
// helpers do not fit the same table shape (nondeterministic timing for the
// former, a different transport - os.Stdout/os.Stderr rather than an
// injectable io.Writer - for the latter) and are covered by their own
// dedicated subtests below instead of forcing an awkward fit.
func TestTiersSanitizeCallerText(t *testing.T) {
	t.Parallel()

	for _, tc := range sanitizingTiers() {
		t.Run(tc.name+"/benign", func(t *testing.T) {
			t.Parallel()
			runTierCase(t, tc, "benign text", "benign text")
		})
		t.Run(tc.name+"/hostile", func(t *testing.T) {
			t.Parallel()
			runTierCase(t, tc, hostileCallerText, hostileCallerTextClean)
		})
	}
}

// TestDebugSincefSanitizesCallerText covers DebugSincef separately from
// TestTiersSanitizeCallerText's table: its prefix embeds a nondeterministic
// elapsed-time string, so only the fixed prefix and fixed suffix around
// that timing text can be asserted, not the exact line.
func TestDebugSincefSanitizesCallerText(t *testing.T) {
	t.Parallel()

	const wantPrefix = "⏱️ Debug Timing ("

	check := func(t *testing.T, msg, wantSuffix string) {
		t.Helper()
		p, out, errOut := stateB()
		defer p.Close()
		p.DebugSincef(time.Now(), "%s", msg)
		got := out.String()
		if !strings.HasPrefix(got, wantPrefix) {
			t.Fatalf("expected prefix %q, got %q", wantPrefix, got)
		}
		if !strings.HasSuffix(got, wantSuffix) {
			t.Fatalf("expected suffix %q, got %q", wantSuffix, got)
		}
		assertEmpty(t, errOut)
	}

	t.Run("benign", func(t *testing.T) {
		t.Parallel()
		check(t, "benign text", "): benign text\n")
	})
	t.Run("hostile", func(t *testing.T) {
		t.Parallel()
		check(t, hostileCallerText, "): "+hostileCallerTextClean+"\n")
	})
}

// TestPackageLevelHelpersSanitizeCallerText covers the standalone
// package-level Okf/Errorf, which write directly to os.Stdout/os.Stderr
// (they run before any Progress exists) rather than to an injectable
// io.Writer, so they need capturePipe instead of the buffer-based tiers
// above.
func TestPackageLevelHelpersSanitizeCallerText(t *testing.T) {
	cases := []struct {
		name   string
		target **os.File
		invoke func(msg string)
		prefix string
	}{
		{"Okf", &os.Stdout, func(msg string) { Okf("%s", msg) }, okPrefix},
		{"Errorf", &os.Stderr, func(msg string) { Errorf("%s", msg) }, failPrefix},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/benign", func(t *testing.T) {
			got := capturePipe(t, tc.target, func() { tc.invoke("benign text") })
			want := tc.prefix + "benign text\n"
			if got != want {
				t.Fatalf("expected %q, got %q", want, got)
			}
		})
		t.Run(tc.name+"/hostile", func(t *testing.T) {
			got := capturePipe(t, tc.target, func() { tc.invoke(hostileCallerText) })
			want := tc.prefix + hostileCallerTextClean + "\n"
			if got != want {
				t.Fatalf("expected %q, got %q", want, got)
			}
		})
	}
}

// TestPrintfSpinnerSuffixIsSanitized covers Printf's spinner branch (state
// A): a hostile message must reach the spinner's Suffix field already
// sanitized, and must never reach out directly. The benign row is this
// test's positive control, on the same fixture shape, proving the suffix
// carries real content rather than always being empty or replaced.
func TestPrintfSpinnerSuffixIsSanitized(t *testing.T) {
	t.Parallel()

	t.Run("benign", func(t *testing.T) {
		t.Parallel()
		p, out, _ := stateA()
		defer p.Close()
		p.Printf("%s", "benign text")
		if want := " benign text"; p.s.Suffix != want {
			t.Fatalf("suffix = %q, want %q", p.s.Suffix, want)
		}
		assertEmpty(t, out)
	})

	t.Run("hostile", func(t *testing.T) {
		t.Parallel()
		p, out, _ := stateA()
		defer p.Close()
		p.Printf("%s", hostileCallerText)
		want := " " + hostileCallerTextClean
		if p.s.Suffix != want {
			t.Fatalf("suffix = %q, want %q", p.s.Suffix, want)
		}
		assertEmpty(t, out)
	})
}

// TestResultMarkerEscapesSurviveAHostileMessage covers Okf/Errorf/Warnf
// against a hostile message with two independently reachable assertions in
// a t.Fatalf chain: (1) the line still starts with the raw marker constant
// (its own ANSI escape bytes intact), reachable on its own by a
// writer-level sanitizer that would strip the message but also corrupt a
// decoration this file adds; and (2), reached only once (1) already holds,
// that the message half is exactly the sanitized form, reachable on its
// own by a missing Clean call that leaves the marker intact but the
// message raw. Each assertion therefore pins a distinct failure mode
// rather than restating the other.
func TestResultMarkerEscapesSurviveAHostileMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		invoke func(p *Progress, msg string)
		stream func(out, errOut *bytes.Buffer) *bytes.Buffer
		name   string
		prefix string
	}{
		{
			name: "Okf", prefix: okPrefix,
			invoke: func(p *Progress, msg string) { p.Okf("%s", msg) },
			stream: func(out, _ *bytes.Buffer) *bytes.Buffer { return out },
		},
		{
			name: "Errorf", prefix: failPrefix,
			invoke: func(p *Progress, msg string) { p.Errorf("%s", msg) },
			stream: func(_, errOut *bytes.Buffer) *bytes.Buffer { return errOut },
		},
		{
			name: "Warnf", prefix: warnPrefix,
			invoke: func(p *Progress, msg string) { p.Warnf("%s", msg) },
			stream: func(_, errOut *bytes.Buffer) *bytes.Buffer { return errOut },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, out, errOut := stateB()
			defer p.Close()
			tc.invoke(p, hostileCallerText)
			got := tc.stream(out, errOut).String()

			// (1) the marker's own bytes are intact.
			if !strings.HasPrefix(got, tc.prefix) {
				t.Fatalf("line %q does not start with raw marker %q", got, tc.prefix)
			}
			// (2) reached only when (1) held: the message half is
			// sanitized.
			want := tc.prefix + hostileCallerTextClean + "\n"
			if got != want {
				t.Fatalf("line = %q, want %q", got, want)
			}
		})
	}
}

// TestSpinnerWritesOutsideThisPackagesWriters pins the spinner-frame
// guarantee this package relies on: newProgress never assigns p.s.Writer,
// so spinner frames render through a writer this package does not own -
// neither out nor errOut - and are not its responsibility to sanitize.
// Only the lines this package writes itself are. The assertions below check
// exactly that, and stay valid however the spinner chooses its own writer.
func TestSpinnerWritesOutsideThisPackagesWriters(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	p := newProgress(false, false, true, &out, &errOut)
	defer p.Close()
	if p.s == nil {
		t.Fatal("expected spinner to be created for verbose=false, quiet=false, terminal=true")
	}
	if p.s.Writer == io.Writer(&out) {
		t.Fatal("spinner.Writer must not be this package's out buffer")
	}
	if p.s.Writer == io.Writer(&errOut) {
		t.Fatal("spinner.Writer must not be this package's errOut buffer")
	}
}
