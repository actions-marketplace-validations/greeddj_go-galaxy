package progress

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNewProgressSpinnerCreation verifies the spinner is created only for
// the single (verbose=false, quiet=false, terminal=true) combination; every
// other combination of the three booleans must leave the spinner nil.
func TestNewProgressSpinnerCreation(t *testing.T) {
	var buf bytes.Buffer

	p := newProgress(false, false, true, &buf)
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
			p := newProgress(c.verbose, c.quiet, c.terminal, &buf)
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

// stateA builds a fresh Progress/buffer pair for state A (TTY normal: spinner active).
func stateA() (*Progress, *bytes.Buffer) {
	var buf bytes.Buffer
	return newProgress(false, false, true, &buf), &buf
}

// TestStateATransient covers Printf and Write for state A: both route through
// the spinner (suffix update / stop-print-restart) instead of the buffer directly.
func TestStateATransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, buf := stateA()
		defer p.Close()
		p.Printf("x")
		assertEmpty(t, buf)
		if p.s.Suffix != " x" {
			t.Fatalf("expected spinner suffix %q, got %q", " x", p.s.Suffix)
		}
	})

	t.Run("WriteWithMessage", func(t *testing.T) {
		p, buf := stateA()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, buf, "x\n")
	})

	t.Run("WriteEmptyMessage", func(t *testing.T) {
		p, buf := stateA()
		defer p.Close()
		n, err := p.Write([]byte("\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected n=1, got %d", n)
		}
		assertEmpty(t, buf)
	})
}

// TestStateAResult covers the result tier for state A: PersistentPrintf,
// Okf and Errorf must all emit even though a spinner is active.
func TestStateAResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, buf := stateA()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, buf, "x\n")
	})

	t.Run("Okf", func(t *testing.T) {
		p, buf := stateA()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, buf, ok+" x\n")
	})

	t.Run("Errorf", func(t *testing.T) {
		p, buf := stateA()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, buf, fail+" x\n")
	})
}

// TestStateADebug covers the debug tier for state A: debug output stays
// suppressed since verbose is false.
func TestStateADebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, buf := stateA()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, buf)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, buf := stateA()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, buf)
	})
}

// stateB builds a fresh Progress/buffer pair for state B (verbose, no spinner).
func stateB() (*Progress, *bytes.Buffer) {
	var buf bytes.Buffer
	return newProgress(true, false, true, &buf), &buf
}

// TestStateBTransient covers Printf and Write for state B: no spinner exists,
// so both write a full line directly.
func TestStateBTransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, buf := stateB()
		defer p.Close()
		p.Printf("x")
		assertBuf(t, buf, "x\n")
	})

	t.Run("Write", func(t *testing.T) {
		p, buf := stateB()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, buf, "x\n")
	})
}

// TestStateBResult covers the result tier for state B: all methods emit.
func TestStateBResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, buf := stateB()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, buf, "x\n")
	})

	t.Run("Okf", func(t *testing.T) {
		p, buf := stateB()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, buf, ok+" x\n")
	})

	t.Run("Errorf", func(t *testing.T) {
		p, buf := stateB()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, buf, fail+" x\n")
	})
}

// TestStateBDebug covers the debug tier for state B: verbose mode enables
// both Debugf and DebugSincef.
func TestStateBDebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, buf := stateB()
		defer p.Close()
		p.Debugf("x")
		assertBuf(t, buf, "🚧 Debug: x\n")
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, buf := stateB()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		out := buf.String()
		if !strings.HasPrefix(out, "⏱️ Debug Timing (") {
			t.Fatalf("expected prefix %q, got %q", "⏱️ Debug Timing (", out)
		}
		if !strings.HasSuffix(out, "): x\n") {
			t.Fatalf("expected suffix %q, got %q", "): x\n", out)
		}
	})
}

// stateC builds a fresh Progress/buffer pair for state C (quiet, no spinner).
func stateC() (*Progress, *bytes.Buffer) {
	var buf bytes.Buffer
	return newProgress(false, true, true, &buf), &buf
}

// TestStateCSuppressed covers the tiers suppressed by quiet mode: Printf,
// Write and both debug methods must stay silent.
func TestStateCSuppressed(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, buf := stateC()
		defer p.Close()
		p.Printf("x")
		assertEmpty(t, buf)
	})

	t.Run("Write", func(t *testing.T) {
		p, buf := stateC()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertEmpty(t, buf)
	})

	t.Run("Debugf", func(t *testing.T) {
		p, buf := stateC()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, buf)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, buf := stateC()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, buf)
	})
}

// TestStateCResult covers the result tier for state C: quiet mode never
// suppresses PersistentPrintf, Okf or Errorf.
func TestStateCResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, buf := stateC()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, buf, "x\n")
	})

	t.Run("Okf", func(t *testing.T) {
		p, buf := stateC()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, buf, ok+" x\n")
	})

	t.Run("Errorf", func(t *testing.T) {
		p, buf := stateC()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, buf, fail+" x\n")
	})
}

// stateD builds a fresh Progress/buffer pair for state D (non-TTY normal:
// the CI-defect regression case, no spinner, verbose=false, quiet=false).
func stateD() (*Progress, *bytes.Buffer) {
	var buf bytes.Buffer
	return newProgress(false, false, false, &buf), &buf
}

// TestStateDTransient covers Printf and Write for state D: no spinner
// exists and quiet is false, so both must emit a full line - this is the
// CI-defect regression case (output previously vanished with no TTY).
func TestStateDTransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, buf := stateD()
		defer p.Close()
		p.Printf("x")
		assertBuf(t, buf, "x\n")
	})

	t.Run("Write", func(t *testing.T) {
		p, buf := stateD()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, buf, "x\n")
	})
}

// TestStateDResult covers the result tier for state D: it must always emit,
// same as every other state.
func TestStateDResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, buf := stateD()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, buf, "x\n")
	})

	t.Run("Okf", func(t *testing.T) {
		p, buf := stateD()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, buf, ok+" x\n")
	})

	t.Run("Errorf", func(t *testing.T) {
		p, buf := stateD()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, buf, fail+" x\n")
	})
}

// TestStateDDebug covers the debug tier for state D: debug output stays
// suppressed since verbose is false.
func TestStateDDebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, buf := stateD()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, buf)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, buf := stateD()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, buf)
	})
}

// TestClose verifies Close does not panic whether or not a spinner exists.
func TestClose(*testing.T) {
	var buf bytes.Buffer

	withSpinner := newProgress(false, false, true, &buf)
	withSpinner.Close()

	withoutSpinner := newProgress(true, false, true, &buf)
	withoutSpinner.Close()
}

// TestPackageLevelOkfErrorf verifies the standalone package-level Okf/Errorf
// helpers write to os.Stdout, since they run before any Progress exists.
func TestPackageLevelOkfErrorf(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	Okf("hello %s", "world")
	Errorf("bye %s", "world")

	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("failed to close pipe writer: %v", closeErr)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read pipe: %v", err)
	}

	want := ok + " hello world\n" + fail + " bye world\n"
	if string(out) != want {
		t.Fatalf("expected %q, got %q", want, string(out))
	}
}
