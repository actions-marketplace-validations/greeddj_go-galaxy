package helpers

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestNewSizeLimitedReaderWholeStream drives NewSizeLimitedReader through
// io.ReadAll, which itself performs multiple Read calls with a growing
// internal buffer, to pin the three whole-stream outcomes: comfortably under
// the cap, landing exactly on it, and overrunning it by a small or large
// margin. In every overrun case the returned error must be
// ErrArtifactTooLarge rather than a silent truncation (which is exactly what
// a bare io.LimitReader would produce instead).
func TestNewSizeLimitedReaderWholeStream(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		data      []byte
		max       int64
		wantErr   bool
		wantBytes int
	}{
		{name: "well under the cap", data: bytes.Repeat([]byte{'a'}, 10), max: 100, wantErr: false, wantBytes: 10},
		{name: "exactly at the cap", data: bytes.Repeat([]byte{'b'}, 10), max: 10, wantErr: false, wantBytes: 10},
		{name: "one byte over the cap", data: bytes.Repeat([]byte{'c'}, 6), max: 5, wantErr: true, wantBytes: 6},
		{name: "far over the cap", data: bytes.Repeat([]byte{'d'}, 4096), max: 5, wantErr: true, wantBytes: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewSizeLimitedReader(bytes.NewReader(tc.data), tc.max)
			got, err := io.ReadAll(r)
			if tc.wantErr {
				if !errors.Is(err, ErrArtifactTooLarge) {
					t.Fatalf("io.ReadAll() error = %v, want ErrArtifactTooLarge", err)
				}
			} else if err != nil {
				t.Fatalf("io.ReadAll() unexpected error: %v", err)
			}
			// wantBytes < 0 means "do not assert an exact count", used for the
			// far-over-the-cap case where the internal buffer growth of
			// io.ReadAll makes the exact byte count read before failing an
			// implementation detail rather than something worth pinning; what
			// matters there is only that it errors instead of returning all
			// 4096 bytes as a silently truncated success.
			if tc.wantBytes >= 0 && len(got) != tc.wantBytes {
				t.Fatalf("io.ReadAll() returned %d bytes, want %d", len(got), tc.wantBytes)
			}
			if !tc.wantErr && !bytes.Equal(got, tc.data) {
				t.Fatalf("io.ReadAll() = %q, want %q", got, tc.data)
			}
		})
	}
}

// TestSizeLimitedReaderCrossesBoundaryMidStream pins the per-call behavior
// precisely: reads that stay under the cap must pass through untouched, and
// the exact Read call whose cumulative total first exceeds the cap must
// surface ErrArtifactTooLarge on that same call - not one call later, and not
// silently swallowed into a clean EOF.
func TestSizeLimitedReaderCrossesBoundaryMidStream(t *testing.T) {
	t.Parallel()

	data := []byte("0123456789") // 10 bytes
	const limit = int64(7)
	r := NewSizeLimitedReader(bytes.NewReader(data), limit)
	buf := make([]byte, 3)

	n, err := r.Read(buf)
	if err != nil || n != 3 {
		t.Fatalf("first read = (%d, %v), want (3, nil)", n, err)
	}
	if got, want := string(buf[:n]), "012"; got != want {
		t.Fatalf("first read data = %q, want %q", got, want)
	}

	n, err = r.Read(buf)
	if err != nil || n != 3 {
		t.Fatalf("second read (cumulative 6, still under cap 7) = (%d, %v), want (3, nil)", n, err)
	}

	// Third read pushes cumulative bytes to 9, past the cap of 7: it must
	// still return the 3 bytes it actually read from the underlying reader,
	// but the error must surface on this exact call.
	n, err = r.Read(buf)
	if n != 3 {
		t.Fatalf("crossing read returned %d bytes, want 3", n)
	}
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("crossing read error = %v, want ErrArtifactTooLarge", err)
	}
}

// TestSizeLimitedReaderZeroMax verifies the degenerate case of a zero-byte
// budget: any read that returns data at all overruns it immediately.
func TestSizeLimitedReaderZeroMax(t *testing.T) {
	t.Parallel()

	r := NewSizeLimitedReader(bytes.NewReader([]byte("x")), 0)
	buf := make([]byte, 1)
	n, err := r.Read(buf)
	if n != 1 {
		t.Fatalf("Read() n = %d, want 1", n)
	}
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("Read() error = %v, want ErrArtifactTooLarge", err)
	}
}
