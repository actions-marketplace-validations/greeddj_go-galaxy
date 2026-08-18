package archive

// This file is a structural, source-auditing test, the same shape
// internal/proseaudit, internal/lockaudit and internal/galaxy/store's own
// dirty-flag audit already use elsewhere in this module: it parses archive.go
// itself and checks a property of its AST rather than driving the code at
// runtime. It gates exactly one function in one file, and it lives apart from
// archive_test.go so that go/ast, go/parser, go/token and slices stay out of
// that file's import block, where four added lines would shift every line
// number its comments cite.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
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

// pgzipCall is one pgzip call site: the constructor name, and how each
// argument after the reader is spelled in the source.
type pgzipCall struct {
	name string
	args []string
}

// pgzipCalls returns, in source order, every pgzip selector fn calls -
// NewReader, NewReaderN, or anything a future edit reaches for - so an
// assertion can name both what it found and what it wanted. Each call carries
// its own sizing arguments alongside its name, since the name on its own says
// nothing about the budget the call was handed.
func pgzipCalls(fn *ast.FuncDecl) []pgzipCall {
	var found []pgzipCall
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
		if !isIdent || pkg.Name != "pgzip" {
			return true
		}
		var args []string
		for i, arg := range call.Args {
			if i == 0 {
				continue // the reader, not a sizing argument
			}
			args = append(args, argSpelling(arg))
		}
		found = append(found, pgzipCall{name: sel.Sel.Name, args: args})
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
// archive.go and asserts that ProbeTarGz reaches pgzip through NewReaderN,
// through nothing else, and with both sizing arguments spelled as the
// constants TestProbeGzipSizingStaysWithinItsBudget bounds. A ProbeTarGz this
// gate cannot find is a failure rather than a silent pass, since a rename
// would otherwise leave it auditing an empty set.
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
// pgzip.NewReader(file) fails this test with
//
//	probe_decompressor_test.go:145: ProbeTarGz builds its decompressor with [NewReader], want exactly [NewReaderN]
//
// and restoring the old reservation through the new constructor, as
// pgzip.NewReaderN(file, 1<<20, 4), passes that first assertion and fails the
// second with
//
//	probe_decompressor_test.go:148: ProbeTarGz sizes its decompressor with [expression 4], want [probeGzipBlockSize probeGzipBlocks]
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

	calls := pgzipCalls(fn)
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
// extractTarGzStream's own pgzip.NewReader takes - satisfies both and fails
// the third.
//
// Killing mutations, all three run. probeGzipBlockSize = 512:
//
//	probe_decompressor_test.go:186: probeGzipBlockSize = 512, want above 512: pgzip.NewReaderN coerces anything smaller to 1 MiB
//
// probeGzipBlocks = 0:
//
//	probe_decompressor_test.go:190: probeGzipBlocks = 0, want at least 1
//
// probeGzipBlockSize = 1 << 20 with probeGzipBlocks = 4:
//
//	probe_decompressor_test.go:193: the probe reserves 4194304 bytes per reader, want at most 262144
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
