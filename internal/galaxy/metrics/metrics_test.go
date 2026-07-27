package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustReadFile reads path and fails the test on error. Centralizing the read
// keeps each Write test case focused on its own assertion and gives gosec's
// G304 (potential file inclusion via variable) a single call site to
// annotate instead of one per test.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	//nolint:gosec // path is built from this test's own t.TempDir fixture, never external input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return data
}

// TestWriteEmptyPathIsNoOp asserts Write("", ...) is a no-op returning nil so
// callers can pass cfg.MetricsFile unconditionally. There is nothing further
// to observe: the empty path is rejected before any filesystem call, and were
// the guard ever dropped, filepath.Dir("") is "." - the write would land in
// the process working directory and fail at the rename, which this nil check
// already catches.
func TestWriteEmptyPathIsNoOp(t *testing.T) {
	t.Parallel()

	if err := Write("", Report{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// TestWriteProducesReadableReport asserts Write creates any missing parent
// directory and produces a report that round-trips through JSON.
func TestWriteProducesReadableReport(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "sub", "metrics.json")

	r := Report{
		Command:     "install",
		Server:      "https://galaxy.ansible.com",
		Collections: 3,
		Failures:    0,
		StartedAt:   time.Unix(1000, 0).UTC(),
		FinishedAt:  time.Unix(1010, 0).UTC(),
	}
	if err := Write(path, r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data := mustReadFile(t, path)
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Command != r.Command {
		t.Fatalf("Command = %q, want %q", got.Command, r.Command)
	}
	if got.Server != r.Server {
		t.Fatalf("Server = %q, want %q", got.Server, r.Server)
	}
	if got.Collections != r.Collections {
		t.Fatalf("Collections = %d, want %d", got.Collections, r.Collections)
	}
}

// TestWriteReplacesSymlinkTarget is the operator-facing pin of the original
// security finding, exercised at the API an operator actually calls: a
// symlink planted at the metrics path is replaced rather than followed, the
// victim it pointed at is untouched, and the resulting file is a regular
// file whose content parses as a Report. This deliberately overlaps the
// helpers package's own symlink test - the duplication is the point: this
// test fails if Write ever regresses to os.WriteFile.
func TestWriteReplacesSymlinkTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	sentinel := []byte("do-not-touch")
	// 0o600: this is scratch fixture content, not the operator-facing report
	// this test is about, so it does not need to match FileMod.
	if err := os.WriteFile(victim, sentinel, 0o600); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}

	path := filepath.Join(dir, "metrics.json")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	r := Report{Command: "warm", Collections: 1}
	if err := Write(path, r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	gotVictim := mustReadFile(t, victim)
	if string(gotVictim) != string(sentinel) {
		t.Fatalf("victim content = %q, want unchanged %q", gotVictim, sentinel)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("metrics path is still a symlink after Write")
	}

	data := mustReadFile(t, path)
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Command != r.Command {
		t.Fatalf("Command = %q, want %q", got.Command, r.Command)
	}
}
