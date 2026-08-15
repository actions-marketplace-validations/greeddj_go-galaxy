package manifest

import (
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
)

// The synthetic artifact both benchmarks run over: files of incompressible
// bytes, so the gzip layer cannot make the measurement a property of how well
// the fixture happens to compress. Around 50 MB decompressed, which is far
// larger than a real collection and therefore the shape where a per-entry cost
// shows up at all.
const (
	benchChainFiles    = 2000
	benchChainFileSize = 25 << 10
	// benchChainSeed fixes the fixture's bytes, so two runs measure the same
	// artifact.
	benchChainSeed = 0x5a
)

// buildBenchChainArtifact writes the shared fixture and returns its path
// together with the manifest a signature would have covered.
//
// It is built per benchmark rather than once for the process, so each one's own
// temp directory reclaims 50 MB when it ends. Both callers pass through the
// same builder with the same seed, so the two are measuring identical bytes.
func buildBenchChainArtifact(b *testing.B) (string, []byte) {
	b.Helper()

	source := rand.NewChaCha8([32]byte{benchChainSeed})
	entries := make([]chainEntry, 0, benchChainFiles)
	for i := range benchChainFiles {
		body := make([]byte, benchChainFileSize)
		if _, err := source.Read(body); err != nil {
			b.Fatalf("failed to fill the fixture's body: %v", err)
		}
		entries = append(entries, chainEntry{name: fmt.Sprintf("plugins/modules/m%d.bin", i), content: body})
	}
	return buildChainArtifact(b, chainSpec{entries: entries})
}

// BenchmarkVerifyChain measures one whole chain check: the gzip and tar walk,
// a sha256 over every entry body, and the two document decodes.
func BenchmarkVerifyChain(b *testing.B) {
	artifact, manifestJSON := buildBenchChainArtifact(b)

	b.ReportAllocs()
	for b.Loop() {
		if err := VerifyChain(b.Context(), artifact, manifestJSON); err != nil {
			b.Fatalf("VerifyChain failed: %v", err)
		}
	}
}

// BenchmarkExtractTarGzForComparison runs the extractor over the identical
// artifact, so the chain check's cost can be read against the pass that already
// runs on every install rather than against nothing.
//
// It is a scale reference and not a like-for-like comparison: this one writes
// every entry to disk while the check above writes nothing, so the extractor's
// numbers carry filesystem work the check has no equivalent of. ns/op is the
// attributable metric, since the timer is stopped for the per-iteration
// directory setup and teardown; ReportAllocs spans the whole loop and so
// includes that setup's own allocations.
func BenchmarkExtractTarGzForComparison(b *testing.B) {
	artifact, _ := buildBenchChainArtifact(b)

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		// os.MkdirTemp rather than b.TempDir, which defers every iteration's
		// cleanup to the end of the benchmark: at 50 MB an iteration that would
		// fill a disk before anything was reclaimed.
		//nolint:usetesting // per-iteration cleanup, see comment above.
		dst, err := os.MkdirTemp("", "gg-chain-bench-")
		if err != nil {
			b.Fatalf("failed to create temp dir: %v", err)
		}
		b.StartTimer()

		if err := archive.ExtractTarGz(b.Context(), artifact, dst); err != nil {
			b.Fatalf("extraction failed: %v", err)
		}

		b.StopTimer()
		if err := os.RemoveAll(dst); err != nil {
			b.Fatalf("failed to remove temp dir: %v", err)
		}
		b.StartTimer()
	}
}
