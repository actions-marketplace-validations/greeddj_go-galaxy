package collections

// This file is the collections-tree dry-run/real-run agreement matrix: for
// every on-disk shape ansible_collections (and, separately, the acme
// namespace directory beneath it) can take, install --dry-run and a real
// install must agree on whether the run succeeds and, when it fails, on
// exitcode.FromError - proving install --dry-run aborts on exactly the
// conditions a real install would certainly fail on, not merely that both
// happen to return an error.
//
// Every shape's own setup is run through Start twice - once with
// cfg.DryRun=false, once with cfg.DryRun=true - each against its own fresh
// temp directory and fake server, so neither run can leak state into the
// other. The accepted shapes (absent, real directory, an in-root relative
// symlink to a directory, and - namespace-only - a dangling symlink) are the
// positive controls this matrix needs: without them, a refused shape proves
// nothing about whether the fixture could ever be accepted in the first
// place.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// collectionsTreeShape is one on-disk arrangement to probe against Start,
// for both a real run and a dry run.
type collectionsTreeShape struct {
	// setup places the shape under test at downloadPath (an already-created
	// directory), given outside - a directory this process controls but that
	// sits entirely outside downloadPath, for the escaping-symlink shapes.
	setup func(t *testing.T, downloadPath, outside string)
	name  string
	// accepted is true for a shape a real, non-dry-run install completes
	// successfully against - this matrix's positive control for that shape.
	accepted bool
}

// ansibleCollectionsShapes is the seven-shape table for ansible_collections
// itself, matching probeAnsibleCollectionsUsable's own doc comment table,
// which states the same real-run/probe agreement for both openCollectionsRoot
// branches; that table merges the two escaping forms into one row, which this
// table splits: absent, a real directory, an in-root relative symlink to a
// directory, an escaping symlink in each of its two forms (relative and
// absolute - both are refused, but a hostile checkout can only ever plant the
// relative one, which is why installroot.go's own doc comments single it out
// as the shape that matters), a dangling symlink, and a regular file. Each
// shape's setup is its own named function - see setupAnsibleCollectionsX
// below - rather than an inline closure, purely to keep this table-building
// function itself short.
func ansibleCollectionsShapes() []collectionsTreeShape {
	return []collectionsTreeShape{
		{name: "absent", accepted: true, setup: setupAnsibleCollectionsAbsent},
		{name: "real directory", accepted: true, setup: setupAnsibleCollectionsRealDir},
		{name: "symlink, relative, in-root, to a directory", accepted: true, setup: setupAnsibleCollectionsInRootSymlink},
		{name: "symlink, escaping, relative", accepted: false, setup: setupAnsibleCollectionsEscapingRelative},
		{name: "symlink, escaping, absolute", accepted: false, setup: setupAnsibleCollectionsEscapingAbsolute},
		{name: "symlink, dangling", accepted: false, setup: setupAnsibleCollectionsDangling},
		{name: "regular file", accepted: false, setup: setupAnsibleCollectionsRegularFile},
	}
}

func setupAnsibleCollectionsAbsent(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
}

func setupAnsibleCollectionsRealDir(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(downloadPath, "ansible_collections"))
}

func setupAnsibleCollectionsInRootSymlink(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(downloadPath, "real-target"))
	if err := os.Symlink("real-target", filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> real-target: %v", err)
	}
}

func setupAnsibleCollectionsEscapingRelative(t *testing.T, downloadPath, outside string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
	mustMkdirAll(t, outside)
	if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> ../outside: %v", err)
	}
}

func setupAnsibleCollectionsEscapingAbsolute(t *testing.T, downloadPath, outside string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> outside: %v", err)
	}
}

func setupAnsibleCollectionsDangling(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
	if err := os.Symlink("missing-target", filepath.Join(downloadPath, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections -> missing-target: %v", err)
	}
}

func setupAnsibleCollectionsRegularFile(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, downloadPath)
	mustWriteFile(t, filepath.Join(downloadPath, "ansible_collections"), []byte("not a directory"))
}

// namespaceShapes is the six-shape table for the acme namespace directory
// (ansible_collections/acme) - deliberately not the identical seven-shape
// table ansibleCollectionsShapes uses, because a dangling namespace symlink
// is ACCEPTED here (extractCollection reaches it through RemoveAll, which
// resolves a dangling symlink to nothing and succeeds, then MkdirAll creates
// it fresh) while a dangling ansible_collections symlink is refused (it is
// reached through a bare MkdirAll with no preceding RemoveAll, which has no
// existing valid directory to no-op against) - see
// probeAnsibleCollectionsUsable's and dryRunNamespaceProbe's own doc comments
// for the full reasoning. The escaping shape is exercised only in its
// relative form here: a hostile checkout can only ever plant that one (an
// absolute host path does not survive a clone), and
// ansibleCollectionsShapes above already covers both forms once. Each
// shape's setup is its own named function, mirroring
// ansibleCollectionsShapes's own reasoning for doing so.
func namespaceShapes() []collectionsTreeShape {
	return []collectionsTreeShape{
		{name: "absent", accepted: true, setup: setupNamespaceAbsent},
		{name: "real directory", accepted: true, setup: setupNamespaceRealDir},
		{name: "symlink, relative, in-root, to a directory", accepted: true, setup: setupNamespaceInRootSymlink},
		{name: "symlink, escaping, relative", accepted: false, setup: setupNamespaceEscapingRelative},
		{name: "symlink, dangling", accepted: true, setup: setupNamespaceDangling},
		{name: "regular file", accepted: false, setup: setupNamespaceRegularFile},
	}
}

func setupNamespaceAbsent(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(downloadPath, "ansible_collections"))
}

func setupNamespaceRealDir(t *testing.T, downloadPath, _ string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(downloadPath, "ansible_collections", "acme"))
}

func setupNamespaceInRootSymlink(t *testing.T, downloadPath, _ string) {
	t.Helper()
	acDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, filepath.Join(acDir, "real-ns-target"))
	if err := os.Symlink("real-ns-target", filepath.Join(acDir, "acme")); err != nil {
		t.Fatalf("symlink acme -> real-ns-target: %v", err)
	}
}

func setupNamespaceEscapingRelative(t *testing.T, downloadPath, outside string) {
	t.Helper()
	acDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, acDir)
	mustMkdirAll(t, outside)
	if err := os.Symlink(filepath.Join("..", "..", "outside"), filepath.Join(acDir, "acme")); err != nil {
		t.Fatalf("symlink acme -> ../../outside: %v", err)
	}
}

func setupNamespaceDangling(t *testing.T, downloadPath, _ string) {
	t.Helper()
	acDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, acDir)
	if err := os.Symlink("missing-ns-target", filepath.Join(acDir, "acme")); err != nil {
		t.Fatalf("symlink acme -> missing-ns-target: %v", err)
	}
}

func setupNamespaceRegularFile(t *testing.T, downloadPath, _ string) {
	t.Helper()
	acDir := filepath.Join(downloadPath, "ansible_collections")
	mustMkdirAll(t, acDir)
	mustWriteFile(t, filepath.Join(acDir, "acme"), []byte("not a directory"))
}

// runCollectionsTreeShape runs Start against shape's on-disk arrangement,
// under dryRun, in a fresh fixture: a fresh temp directory, a fresh fake
// server with one collection (acme.app, no dependencies) registered, and one
// requirements.yml requiring it. It returns the fake server's total request
// count and the run's error.
func runCollectionsTreeShape(t *testing.T, shape collectionsTreeShape, dryRun bool) (int, error) {
	t.Helper()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	outside := filepath.Join(root, "outside")
	shape.setup(t, downloadPath, outside)

	cacheDir := filepath.Join(root, "cache")
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.app\n    version: \"*\"\n"))

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          1,
		DryRun:           dryRun,
	}
	runtime := infra.New(noopPrinter{}, srv.Client())

	err := Start(context.Background(), cfg, runtime)
	return srv.Total(), err
}

// assertPreviewAgreesWithRealRun runs shape through runCollectionsTreeShape
// for both a real run and a dry run, and asserts: the real run's own
// error-ness matches shape.accepted (the fixture sanity check every positive
// and negative case here depends on); the dry run's error-ness matches the
// real run's; and, when both fail, exitcode.FromError agrees between them.
// For a refused ansible_collections-level shape it additionally asserts both
// runs made zero requests to the fake server - the proof that the preview
// aborts at the identical point in the pipeline as the real run (before
// resolution ever starts), not merely with the same error. That request-count
// assertion does not apply to a namespace-level shape: reaching the
// namespace write at all requires a real run to have already resolved and
// downloaded the artifact, so a nonzero request count there is expected on
// the real run, not a discriminator between the two.
func assertPreviewAgreesWithRealRun(t *testing.T, shape collectionsTreeShape, checkZeroRequests bool) {
	t.Helper()
	realRequests, realErr := runCollectionsTreeShape(t, shape, false)
	dryRequests, dryErr := runCollectionsTreeShape(t, shape, true)

	if (realErr == nil) != shape.accepted {
		t.Fatalf("fixture sanity: real run error-ness = %v, want accepted=%v; err=%v", realErr == nil, shape.accepted, realErr)
	}
	if (dryErr == nil) != (realErr == nil) {
		t.Errorf("dry run error-ness = %v, want it to match the real run's %v; dryErr=%v realErr=%v",
			dryErr == nil, realErr == nil, dryErr, realErr)
	}
	if gotDry, gotReal := exitcode.FromError(dryErr), exitcode.FromError(realErr); gotDry != gotReal {
		t.Errorf("exitcode.FromError(dry) = %d, exitcode.FromError(real) = %d, want equal; dryErr=%v realErr=%v",
			gotDry, gotReal, dryErr, realErr)
	}
	if checkZeroRequests && !shape.accepted {
		if realRequests != 0 {
			t.Errorf("real run server request count = %d, want 0 (the escape must be caught before resolution starts)", realRequests)
		}
		if dryRequests != 0 {
			t.Errorf("dry run server request count = %d, want 0 (the preview must abort at the same point in the pipeline)", dryRequests)
		}
	}
}

// TestCollectionsTreeMatrixAnsibleCollectionsAgreesBetweenPreviewAndRealRun
// is the seven-shape ansible_collections half of the matrix.
func TestCollectionsTreeMatrixAnsibleCollectionsAgreesBetweenPreviewAndRealRun(t *testing.T) {
	t.Parallel()
	for _, shape := range ansibleCollectionsShapes() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			assertPreviewAgreesWithRealRun(t, shape, true)
		})
	}
}

// TestCollectionsTreeMatrixNamespaceAgreesBetweenPreviewAndRealRun is the
// six-shape namespace half of the matrix.
func TestCollectionsTreeMatrixNamespaceAgreesBetweenPreviewAndRealRun(t *testing.T) {
	t.Parallel()
	for _, shape := range namespaceShapes() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			assertPreviewAgreesWithRealRun(t, shape, false)
		})
	}
}
