package archive

// This file is a structural, source-auditing test, the same shape
// internal/proseaudit, internal/lockaudit and internal/galaxy/store's own
// dirty-flag audit already use elsewhere in this module: it parses archive.go
// itself and checks a property of its AST rather than driving the code at
// runtime. It gates exactly one function in one file, and it lives apart from
// archive_test.go so that go/ast, go/parser, go/token and slices stay out of
// that file's import block, where four added lines would shift every line
// number its comments cite. Its own block is not stdlib-only - the budget
// gate at the end of this file reads a helpers constant - so an edit to it
// shifts this file's own citations, which are re-run rather than renumbered.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// findFuncDecl returns the top-level function named name declared in file, or
// nil when the file declares no such function.
func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// gzipstreamSizingArgOffset is how many leading arguments every gzipstream
// constructor takes before its sizing ones: the context, then the reader.
const gzipstreamSizingArgOffset = 2

// decompressorCall is one internal/gzipstream call site: the constructor
// name, and how each sizing argument is spelled in the source.
type decompressorCall struct {
	name string
	args []string
}

// decompressorCalls returns, in source order, every gzipstream selector fn
// calls - NewReader, NewReaderN, or anything a future edit reaches for - so an
// assertion can name both what it found and what it wanted. Each call carries
// its own sizing arguments alongside its name, since the name on its own says
// nothing about the budget the call was handed.
//
// The package audited is gzipstream rather than pgzip because that is the one
// this file's own package may open a gzip reader through; internal/gzipstream
// carries the gate that keeps pgzip's own constructors to itself.
func decompressorCalls(fn *ast.FuncDecl) []decompressorCall {
	var found []decompressorCall
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return true
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "gzipstream" {
			return true
		}
		var args []string
		for i, arg := range call.Args {
			if i < gzipstreamSizingArgOffset {
				continue // the context and the reader, not sizing arguments
			}
			args = append(args, argSpelling(arg))
		}
		found = append(found, decompressorCall{name: sel.Sel.Name, args: args})
		return true
	})
	return found
}

// argSpelling names how one argument is written, without rendering source: an
// identifier by its name, a literal by its text, anything else - a shift
// expression, a call - as "expression". The spelling is what the gate needs
// rather than the value: a call handed inline literals is a call the constants'
// own bound no longer governs, however those literals happen to be written.
func argSpelling(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.BasicLit:
		return e.Value
	default:
		return "expression"
	}
}

// TestProbeTarGzUsesTheProbeSizedDecompressor parses this package's own
// archive.go and asserts that ProbeTarGz reaches its decompressor through
// gzipstream.NewReaderN, through nothing else, and with both sizing arguments
// spelled as the constants TestProbeGzipSizingStaysWithinItsBudget bounds. A
// ProbeTarGz this gate cannot find is a failure rather than a silent pass,
// since a rename would otherwise leave it auditing an empty set.
//
// The constructor and the arguments are two assertions rather than one because
// NewReaderN handed an inline 1 << 20 and 4 is the extractor's own 4 MiB
// reservation wearing this constructor's name: it satisfies the name and
// reserves every byte back, so a gate on the name alone says nothing about what
// the reader reserves. Together the two tests compose into that claim - this one
// requires the call to be spelled through the constants, that one bounds what
// the constants may be.
//
// It asserts on the source rather than on behavior because the reserved
// bytes themselves are unreachable from a test: pgzip exposes its block
// pool nowhere, and ProbeTarGz opens the file itself, so there is no reader
// seam to count consumed bytes through either. A behavioral gate could
// still reach their shadow in a probe's total allocation, a figure every
// unrelated allocation on the path moves. The sizing does change one
// observable outcome, deliberately left unpinned: pgzip discards the data
// of a block whose fill errored and delivers the error in its place, so the
// probe notices a hard mid-stream defect only within the first block it
// buffers, and a smaller block is a smaller window (measured, and recorded
// on ProbeTarGz itself). Where that window's edge falls is pgzip's block
// arithmetic, so pinning it would freeze an accident into a contract.
//
// Killing mutations, both run. Reverting the constructor to
// gzipstream.NewReader(ctx, file) fails this test with
//
//	probe_decompressor_test.go:157: ProbeTarGz builds its decompressor with [NewReader], want exactly [NewReaderN]
//
// and restoring the old reservation through the new constructor, as
// gzipstream.NewReaderN(ctx, file, 1<<20, 4), passes that first assertion and
// fails the second with
//
//	probe_decompressor_test.go:160: ProbeTarGz sizes its decompressor with [expression 4], want [probeGzipBlockSize probeGzipBlocks]
func TestProbeTarGzUsesTheProbeSizedDecompressor(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "archive.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing archive.go: %v", err)
	}
	fn := findFuncDecl(file, "ProbeTarGz")
	if fn == nil {
		t.Fatalf("archive.go declares no top-level ProbeTarGz to audit")
	}

	calls := decompressorCalls(fn)
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.name)
	}
	if !slices.Equal(names, []string{"NewReaderN"}) {
		t.Fatalf("ProbeTarGz builds its decompressor with %v, want exactly [NewReaderN]", names)
	}
	if got := calls[0].args; !slices.Equal(got, []string{"probeGzipBlockSize", "probeGzipBlocks"}) {
		t.Fatalf("ProbeTarGz sizes its decompressor with %v, want [probeGzipBlockSize probeGzipBlocks]", got)
	}
}

// TestProbeGzipSizingStaysWithinItsBudget bounds the two constants the gate
// above requires ProbeTarGz to be spelled through, which is what turns that
// gate into one on the budget rather than on a constructor's name.
//
// The 256 KiB ceiling is hand-spelled rather than derived from the constants
// it checks: an expectation computed from the value under test moves with
// every mutation of that value and can never fail. It is a ceiling with room
// in it rather than today's product, so a considered re-sizing within the
// same order of magnitude does not have to edit this test to stay green,
// while the extractor's own 4 MiB reservation still fails it.
//
// Each assertion is independently reachable, which a chain of t.Fatalf calls
// does not give for free: probeGzipBlockSize = 512 fails the first outright;
// probeGzipBlocks = 0 satisfies the first and fails the second; and
// probeGzipBlockSize = 1 << 20 with probeGzipBlocks = 4 - the sizing
// extractTarGzStream's own gzipstream.NewReader inherits from pgzip's own
// defaults - satisfies both and fails the third.
//
// Killing mutations, all three run. probeGzipBlockSize = 512:
//
//	probe_decompressor_test.go:198: probeGzipBlockSize = 512, want above 512: pgzip.NewReaderN coerces anything smaller to 1 MiB
//
// probeGzipBlocks = 0:
//
//	probe_decompressor_test.go:202: probeGzipBlocks = 0, want at least 1
//
// probeGzipBlockSize = 1 << 20 with probeGzipBlocks = 4:
//
//	probe_decompressor_test.go:205: the probe reserves 4194304 bytes per reader, want at most 262144
func TestProbeGzipSizingStaysWithinItsBudget(t *testing.T) {
	t.Parallel()

	blockSize, blocks := probeGzipBlockSize, probeGzipBlocks
	if blockSize <= 512 {
		t.Fatalf("probeGzipBlockSize = %d, want above 512: pgzip.NewReaderN coerces "+
			"anything smaller to 1 MiB", blockSize)
	}
	if blocks < 1 {
		t.Fatalf("probeGzipBlocks = %d, want at least 1", blocks)
	}
	if blockSize*blocks > 256<<10 {
		t.Fatalf("the probe reserves %d bytes per reader, want at most %d", blockSize*blocks, 256<<10)
	}
}

// TestArchiveProbeMaxBytesClearsTheMetaHeaderCeiling bounds
// helpers.ArchiveProbeMaxBytes from both sides, the way the sizing gate above
// bounds the probe's two decompressor constants: a floor no legitimate
// prologue may be refused under, and a ceiling that keeps the bound from
// becoming one in name only.
//
// The floor, 4,196,352, is how far archive/tar can be made to read before it
// returns its first header while every byte of that reading still says
// something: a 512-byte header block plus a maximal 1 MiB body for each of the
// three chainable meta kinds ('x', 'L', 'K'), then the 512-byte header the
// walk returns plus the largest sparse map archive/tar will read for it. Below
// that the probe would refuse an archive whose prologue carries what no
// shorter one could - see helpers.ArchiveProbeMaxBytes for the derivation, for
// the composite measured to reach it exactly, and for why the redundancy
// argument that bounds the meta chain at three bodies stops there and does not
// reach the sparse map.
//
// Both numbers are hand-spelled rather than derived from the constant they
// check, for the reason the sizing gate above gives: an expectation computed
// from the value under test moves with every mutation of that value and can
// never fail one. The ceiling is a ceiling with room in it rather than today's
// value, so a considered re-sizing inside the same order of magnitude does not
// have to edit this test to stay green.
//
// Each assertion is independently reachable, which a chain of t.Fatalf calls
// does not give for free: 2 << 20 fails the first outright, while 32 << 20
// satisfies the first and fails the second.
//
// Killing mutations, both run. helpers.ArchiveProbeMaxBytes = int64(2 << 20):
//
//	probe_decompressor_test.go:252: ArchiveProbeMaxBytes = 2097152, want at least 4196352
//
// helpers.ArchiveProbeMaxBytes = int64(32 << 20):
//
//	probe_decompressor_test.go:255: the probe reads up to 33554432 bytes before refusing, want at most 16777216
func TestArchiveProbeMaxBytesClearsTheMetaHeaderCeiling(t *testing.T) {
	t.Parallel()

	const (
		metaHeaderCeiling = int64(4_196_352)
		probeBudget       = int64(16 << 20)
	)
	if helpers.ArchiveProbeMaxBytes < metaHeaderCeiling {
		t.Fatalf("ArchiveProbeMaxBytes = %d, want at least %d", helpers.ArchiveProbeMaxBytes, metaHeaderCeiling)
	}
	if helpers.ArchiveProbeMaxBytes > probeBudget {
		t.Fatalf("the probe reads up to %d bytes before refusing, want at most %d",
			helpers.ArchiveProbeMaxBytes, probeBudget)
	}
}
