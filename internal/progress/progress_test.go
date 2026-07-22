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
