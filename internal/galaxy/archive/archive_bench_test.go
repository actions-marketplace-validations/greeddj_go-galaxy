package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// benchFileMode is the fixed permission bits stamped on every generated
// benchmark tar entry, matching a typical extracted Ansible collection file.
const benchFileMode = 0o644

// buildBenchmarkArchive builds a deterministic in-memory tar.gz containing
// files small regular files, all nested under one shared depth-level
// directory chain (d0/d1/.../d{depth-1}/f{i}.txt). Every file shares the
// same parent chain, which is the maximum-redundancy shape the
// ensureNoSymlinkParents memo targets: without memoization, extracting this
// archive re-Lstats the same depth ancestors once per file.
func buildBenchmarkArchive(tb testing.TB, files, depth int) []byte {
	tb.Helper()

	// benchModTime stamps every generated tar entry so the archive bytes -
	// and therefore the benchmark's decompression cost - do not depend on
	// wall-clock time.
	benchModTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	dirParts := make([]string, depth)
	for i := range depth {
		dirParts[i] = fmt.Sprintf("d%d", i)
	}
	prefix := strings.Join(dirParts, "/")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte("x")
	for i := range files {
		header := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     prefix + "/" + fmt.Sprintf("f%d.txt", i),
			Size:     int64(len(content)),
			Mode:     benchFileMode,
			ModTime:  benchModTime,
		}
		if err := tw.WriteHeader(header); err != nil {
			tb.Fatalf("failed to write tar header: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			tb.Fatalf("failed to write tar content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		tb.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		tb.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// BenchmarkExtractTarGzStream measures full-archive extraction cost for a
// deep, highly redundant directory chain, the shape ensureNoSymlinkParents'
// parent-chain memoization targets.
//
// ReportAllocs measures allocations over the whole b.N loop, not scoped to
// the StartTimer/StopTimer bracket around the extraction call, so
// allocs/op includes minor MkdirTemp/RemoveAll setup-and-teardown noise
// alongside the extraction itself. ns/op is the attributable metric here,
// since the timer is stopped during setup and teardown.
func BenchmarkExtractTarGzStream(b *testing.B) {
	sizes := []struct {
		files int
		depth int
	}{
		{files: 1000, depth: 8},
		{files: 5000, depth: 8},
	}

	for _, sz := range sizes {
		archiveBytes := buildBenchmarkArchive(b, sz.files, sz.depth)
		b.Run(fmt.Sprintf("files=%d/depth=%d", sz.files, sz.depth), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				// os.MkdirTemp+os.RemoveAll (rather than b.TempDir, which
				// defers all cleanup to the end of the benchmark) frees each
				// iteration's extracted tree immediately, so a large b.N
				// does not accumulate thousands of extracted trees on disk
				// before cleanup runs.
				//nolint:usetesting // per-iteration cleanup, see comment above.
				dst, err := os.MkdirTemp("", "gg-extract-bench-")
				if err != nil {
					b.Fatalf("failed to create temp dir: %v", err)
				}
				b.StartTimer()

				if err := ExtractTarGzStream(context.Background(), bytes.NewReader(archiveBytes), dst); err != nil {
					b.Fatalf("extraction failed: %v", err)
				}

				b.StopTimer()
				if err := os.RemoveAll(dst); err != nil {
					b.Fatalf("failed to remove temp dir: %v", err)
				}
			}
		})
	}
}
