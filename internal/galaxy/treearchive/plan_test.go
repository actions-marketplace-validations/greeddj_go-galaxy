package treearchive_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
	"github.com/greeddj/go-galaxy/internal/testing/faketree"
)

// fixture is a tree with every entry kind the planner distinguishes.
func fixture() *faketree.Tree {
	return faketree.New().
		File("README.md", "# app\n").
		Exec("bin/tool.sh", "#!/bin/sh\n").
		File("notes.bak", "ignored").
		Dir("empty").
		Dir(".git").
		File(".git/HEAD", "ref: refs/heads/main\n").
		Symlink("docs/readme-link.md", "../README.md").
		Symlink("bin-link", "bin").
		Symlink("up", "../outside").
		Submodule("vendor/lib")
}

func fixtureRules() treearchive.Rules {
	return treearchive.Rules{Patterns: []string{"*.bak"}, DirNames: map[string]struct{}{".git": {}}}
}

// writeTo plans src with opts and writes it under dir, returning the
// artifact path and its digest.
func writeTo(t *testing.T, dir string, src treearchive.Source, opts treearchive.Options, lead []treearchive.Document) (string, string) {
	t.Helper()
	plan, err := treearchive.PlanTree(context.Background(), src, opts)
	if err != nil {
		t.Fatalf("PlanTree: %v", err)
	}
	f, err := os.CreateTemp(dir, "artifact-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	digest, err := plan.Write(context.Background(), f, lead)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return f.Name(), digest
}

// listNames decompresses the artifact at p and returns its entry names in
// order, with the link targets beside symlinks.
func listNames(t *testing.T, p string) []string {
	t.Helper()
	//nolint:gosec // p is an artifact this test just built under its own temp directory.
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Fatalf("gzip header carries name %q and mtime %v, want both zero", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, describeHeader(t, hdr))
	}
}

// describeHeader checks the fixed header shape and renders the entry's name,
// with the link target beside a symlink.
func describeHeader(t *testing.T, hdr *tar.Header) string {
	t.Helper()
	if hdr.Uname != "" || hdr.Gname != "" || hdr.Uid != 0 || hdr.Gid != 0 {
		t.Fatalf("%s carries an owner", hdr.Name)
	}
	if !hdr.ModTime.Equal(faketree.FixedCommitTime()) {
		t.Fatalf("%s mtime = %v, want the commit time", hdr.Name, hdr.ModTime)
	}
	if hdr.Typeflag == tar.TypeSymlink {
		return hdr.Name + " -> " + hdr.Linkname
	}
	return hdr.Name
}

func TestPlanTreeShapeWithoutDigests(t *testing.T) {
	t.Parallel()
	src := fixture()
	opts := treearchive.Options{Rules: fixtureRules(), Subject: "the role"}
	plan, err := treearchive.PlanTree(context.Background(), src, opts)
	if err != nil {
		t.Fatalf("PlanTree: %v", err)
	}
	wantRows := []treearchive.Row{
		{Name: "README.md"}, {Name: "bin", Dir: true}, {Name: "bin/tool.sh"}, {Name: "bin-link", Dir: true},
		{Name: "docs", Dir: true}, {Name: "docs/readme-link.md"}, {Name: "empty", Dir: true}, {Name: "vendor", Dir: true},
	}
	if got := plan.Rows(); len(got) != len(wantRows) {
		t.Fatalf("rows = %+v, want %+v", got, wantRows)
	} else {
		for i := range got {
			if got[i] != wantRows[i] {
				t.Fatalf("row %d = %+v, want %+v", i, got[i], wantRows[i])
			}
		}
	}
	wantWarnings := []string{
		"skipping symlink up: target outside the role",
		"skipping submodule vendor/lib: submodules are never fetched",
	}
	if got := plan.Warnings(); strings.Join(got, "|") != strings.Join(wantWarnings, "|") {
		t.Fatalf("warnings = %q, want %q", got, wantWarnings)
	}

	dir := t.TempDir()
	p, first := writeTo(t, dir, src, opts, nil)
	wantNames := []string{
		"README.md", "bin", "bin/tool.sh", "bin-link -> bin", "docs", "docs/readme-link.md -> ../README.md", "empty", "vendor",
	}
	if got := listNames(t, p); strings.Join(got, "|") != strings.Join(wantNames, "|") {
		t.Fatalf("entries = %q, want %q", got, wantNames)
	}
	_, second := writeTo(t, dir, fixture(), opts, nil)
	if first != second {
		t.Fatalf("two builds of one tree differ: %s vs %s", first, second)
	}
}

// TestPlanTreeDigestsReadBlobsOnce pins what Options.Digests costs: with it,
// every blob is opened once by the plan and once by the write; without it,
// only by the write.
func TestPlanTreeDigestsReadBlobsOnce(t *testing.T) {
	t.Parallel()
	for _, digests := range []bool{false, true} {
		opens := 0
		src := faketree.New().File("a", "x").File("b", "y").SetOnOpen(func(p string) {
			if p == "a" || p == "b" {
				opens++
			}
		})
		plan, err := treearchive.PlanTree(context.Background(), src, treearchive.Options{Digests: digests})
		if err != nil {
			t.Fatalf("PlanTree: %v", err)
		}
		want := ""
		if digests {
			want = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
		}
		if got := plan.Rows()[0].Digest; got != want {
			t.Fatalf("digests=%t: row digest = %q, want %q", digests, got, want)
		}
		// writeTo plans again and writes: two blobs read once each by the
		// write, plus once each by the plan when digests were asked for.
		opens = 0
		writeTo(t, t.TempDir(), src, treearchive.Options{Digests: digests}, nil)
		wantOpens := 2
		if digests {
			wantOpens = 4
		}
		if opens != wantOpens {
			t.Fatalf("digests=%t: blobs opened %d times, want %d", digests, opens, wantOpens)
		}
	}
}

func TestWriteLeadDocumentsComeFirst(t *testing.T) {
	t.Parallel()
	src := faketree.New().File("a", "x")
	lead := []treearchive.Document{{Name: "MANIFEST.json", Data: []byte("{}")}, {Name: "FILES.json", Data: []byte("[]")}}
	p, _ := writeTo(t, t.TempDir(), src, treearchive.Options{Reserved: int64(len(lead))}, lead)
	want := []string{"MANIFEST.json", "FILES.json", "a"}
	if got := listNames(t, p); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
}

func TestPlanTreeReservedCountsAgainstTheEntryBudget(t *testing.T) {
	t.Parallel()
	src := faketree.New().File("a", "x")
	_, err := treearchive.PlanTree(context.Background(), src, treearchive.Options{Reserved: helpers.ArchiveMaxEntryCount})
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanTreeSubjectDefaultsToTheTree(t *testing.T) {
	t.Parallel()
	plan, err := treearchive.PlanTree(context.Background(), faketree.New().Symlink("up", "/etc/passwd"), treearchive.Options{})
	if err != nil {
		t.Fatalf("PlanTree: %v", err)
	}
	if got := plan.Warnings(); len(got) != 1 || got[0] != "skipping symlink up: target outside the tree" {
		t.Fatalf("warnings = %q", got)
	}
}

func TestWriteRefusesAGrownBlob(t *testing.T) {
	t.Parallel()
	src := faketree.New().File("f", "abc")
	plan, err := treearchive.PlanTree(context.Background(), src, treearchive.Options{})
	if err != nil {
		t.Fatalf("PlanTree: %v", err)
	}
	src.File("f", "abcdef")
	f, err := os.CreateTemp(t.TempDir(), "artifact-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := plan.Write(context.Background(), f, nil); !errors.Is(err, helpers.ErrGitCommitMismatch) {
		t.Fatalf("error = %v", err)
	}
}

func TestWriteObservesCancellation(t *testing.T) {
	t.Parallel()
	src := faketree.New().File("a", "x").File("b", "y")
	plan, err := treearchive.PlanTree(context.Background(), src, treearchive.Options{})
	if err != nil {
		t.Fatalf("PlanTree: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f, err := os.CreateTemp(t.TempDir(), "artifact-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := plan.Write(ctx, f, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Clean(f.Name())); err != nil {
		t.Fatalf("Write must leave the caller's file in place: %v", err)
	}
}

func TestJoinPathAndDisplayPath(t *testing.T) {
	t.Parallel()
	if got := treearchive.JoinPath("", "a"); got != "a" {
		t.Errorf("treearchive.JoinPath(\"\", a) = %q", got)
	}
	if got := treearchive.JoinPath("a", ""); got != "a" {
		t.Errorf("treearchive.JoinPath(a, \"\") = %q", got)
	}
	if got := treearchive.JoinPath("a", "b"); got != "a/b" {
		t.Errorf("treearchive.JoinPath(a, b) = %q", got)
	}
	if got := treearchive.DisplayPath(""); got != "the repository root" {
		t.Errorf("treearchive.DisplayPath(\"\") = %q", got)
	}
	if got := treearchive.DisplayPath("a\x1b[31mb"); strings.ContainsRune(got, 0x1b) {
		t.Errorf("treearchive.DisplayPath kept a control rune: %q", got)
	}
}
