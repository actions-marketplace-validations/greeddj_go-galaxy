package fakegit

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Names and values the collection fixture AddCollection and the role fixture
// AddRole write. They are spelled here as plain literals rather than imported
// from internal/galaxy/collectionbuild or internal/galaxy/rolebuild, for the
// reason fakegalaxy gives for its own MANIFEST.json literals: a double that
// took its shape from the code under test could not catch that code drifting
// from what a real repository holds.
const (
	galaxyFileName       = "galaxy.yml"
	readmeFileName       = "README.md"
	runtimeFileName      = "meta/runtime.yml"
	moduleFileName       = "plugins/modules/hello.py"
	roleMetaFileName     = "meta/main.yml"
	roleTasksFileName    = "tasks/main.yml"
	roleDefaultsFileName = "defaults/main.yml"
	fixtureAuthor        = "fakegit"
	fixtureEmail         = "fakegit@example.invalid"
	// fixtureFileMode is the mode AddCollection stamps on every file it
	// writes, so a fixture's tree hash does not depend on a caller's umask.
	fixtureFileMode os.FileMode = 0o644
	// fixtureDirMode is the mode WriteFile creates intermediate directories
	// with; memfs records no mode for directories in a tree, so the value
	// only has to be one MkdirAll accepts.
	fixtureDirMode os.FileMode = 0o755
)

// fixedTime is the one instant every object this package writes is stamped
// with, so a fixture's commit hash is the same on every run and every
// machine. It is a var because time.Date is not a constant expression.
//
//nolint:gochecknoglobals // fixed, not runtime-mutable, state.
var fixedTime = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

// FixedTime returns the instant every commit, tag and signature this package
// writes carries, so a golden test that computes a hash by hand shares the
// fake's clock. Nothing in this package calls time.Now for content.
func FixedTime() time.Time {
	return fixedTime
}

// Repo is an in-memory git repository a Server serves: a memory.Storage
// holding the objects and references, with a memfs worktree the WriteFile,
// Symlink and Commit builders go through so a fixture is built the way a
// real working copy is. RawTreeCommit bypasses the worktree to write trees a
// worktree could never produce - a submodule entry, a name like ".." or
// "a/b", a duplicate - since those are exactly the shapes the code under
// test must refuse and no honest builder would emit them.
//
// Every method that fails does so through the testing.TB NewRepo was given:
// a builder error is a defect in the calling test's fixture, not a condition
// the test is exercising. mu serializes the builders against a Server that
// may be reading the storage to answer a request on another goroutine.
type Repo struct {
	tb   testing.TB
	repo *git.Repository
	st   *memory.Storage
	fs   billy.Filesystem
	mu   sync.Mutex
}

// NewRepo creates an empty repository over memory storage with a memfs
// worktree. HEAD is the symbolic reference go-git initializes, refs/heads/
// master; SetHEAD moves it. Taking testing.TB, like New, keeps the builder
// unreachable from production code.
func NewRepo(tb testing.TB) *Repo {
	tb.Helper()
	st := memory.NewStorage()
	fs := memfs.New()
	repo, err := git.Init(st, fs)
	if err != nil {
		tb.Fatalf("fakegit: init repository: %v", err)
	}
	return &Repo{tb: tb, repo: repo, st: st, fs: fs}
}

// WriteFile writes content at p in the worktree with mode, creating parent
// directories, and stages it. mode 0o755 stages an executable entry, any
// other mode a regular one; go-git records nothing finer.
func (r *Repo) WriteFile(p string, mode os.FileMode, content []byte) {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if dir := path.Dir(p); dir != "." {
		if err := r.fs.MkdirAll(dir, fixtureDirMode); err != nil {
			r.tb.Fatalf("fakegit: mkdir %s: %v", dir, err)
		}
	}
	if err := util.WriteFile(r.fs, p, content, mode); err != nil {
		r.tb.Fatalf("fakegit: write %s: %v", p, err)
	}
	r.stage(p)
}

// Symlink creates a symbolic link at p pointing at target and stages it.
// The target is recorded verbatim, so an absolute or escaping target is
// written exactly as given: the code under test decides what to do with it.
func (r *Repo) Symlink(target, p string) {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if dir := path.Dir(p); dir != "." {
		if err := r.fs.MkdirAll(dir, fixtureDirMode); err != nil {
			r.tb.Fatalf("fakegit: mkdir %s: %v", dir, err)
		}
	}
	if err := r.fs.Symlink(target, p); err != nil {
		r.tb.Fatalf("fakegit: symlink %s: %v", p, err)
	}
	r.stage(p)
}

// Commit records the staged index as a commit on the branch HEAD names,
// authored and committed by the fixed fakegit signature at FixedTime, and
// returns its hash. Both signatures are always set so go-git never consults
// the user's gitconfig, and an empty commit is allowed so two fixtures that
// differ only by message still yield two distinct commits.
func (r *Repo) Commit(msg string) plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	wt, err := r.repo.Worktree()
	if err != nil {
		r.tb.Fatalf("fakegit: worktree: %v", err)
	}
	sig := fixedSignature()
	h, err := wt.Commit(msg, &git.CommitOptions{Author: &sig, Committer: &sig, AllowEmptyCommits: true})
	if err != nil {
		r.tb.Fatalf("fakegit: commit: %v", err)
	}
	return h
}

// Branch points refs/heads/name at the commit at, creating or moving it.
func (r *Repo) Branch(name string, at plumbing.Hash) {
	r.tb.Helper()
	r.setRef(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), at))
}

// SetHEAD makes HEAD a symbolic reference to refs/heads/branch. The branch
// need not exist yet: a later Commit then creates it, as git does on an
// unborn branch. A detached HEAD is set with DetachHEAD.
func (r *Repo) SetHEAD(branch string) {
	r.tb.Helper()
	r.setRef(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch)))
}

// DetachHEAD points HEAD directly at the commit at, so the advertisement
// carries a HEAD hash but no symref capability.
func (r *Repo) DetachHEAD(at plumbing.Hash) {
	r.tb.Helper()
	r.setRef(plumbing.NewHashReference(plumbing.HEAD, at))
}

// Tag creates the lightweight tag refs/tags/name at the commit at.
func (r *Repo) Tag(name string, at plumbing.Hash) {
	r.tb.Helper()
	r.setRef(plumbing.NewHashReference(plumbing.NewTagReferenceName(name), at))
}

// AnnotatedTag creates a tag object named name whose target is at, with the
// fixed signature as tagger and name as message, points refs/tags/name at
// the tag object and returns the tag object's hash - the hash the
// advertisement lists for the reference, with at as its peeled value.
func (r *Repo) AnnotatedTag(name string, at plumbing.Hash) plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	sig := fixedSignature()
	ref, err := r.repo.CreateTag(name, at, &git.CreateTagOptions{Tagger: &sig, Message: name})
	if err != nil {
		r.tb.Fatalf("fakegit: annotated tag %s: %v", name, err)
	}
	return ref.Hash()
}

// Head returns the commit HEAD resolves to, or plumbing.ZeroHash when HEAD
// names a branch that has no commit yet.
func (r *Repo) Head() plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, err := storer.ResolveReference(r.st, plumbing.HEAD)
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

// Blob stores content as a blob object and returns its hash, for the entries
// a RawTreeCommit tree names.
func (r *Repo) Blob(content []byte) plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	obj := r.st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		r.tb.Fatalf("fakegit: blob writer: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		r.tb.Fatalf("fakegit: write blob: %v", err)
	}
	if err := w.Close(); err != nil {
		r.tb.Fatalf("fakegit: close blob: %v", err)
	}
	h, err := r.st.SetEncodedObject(obj)
	if err != nil {
		r.tb.Fatalf("fakegit: store blob: %v", err)
	}
	return h
}

// RawTreeCommit encodes entries as one tree object by hand, then a commit on
// that tree with no parent, and returns the commit's hash. The caller then
// names it with Branch or Tag. Entries are sorted the way git orders a tree
// before encoding, since go-git refuses an unsorted tree; beyond that the
// encoder rejects only a NUL byte in a name, so "..", ".git", "a/b", an empty
// name, a duplicate, or a filemode.Submodule entry all encode, which is the
// point: these are the trees the code under test must refuse, and no honest
// builder writes them. An entry's hash is not checked against the storage,
// so a submodule entry may name any commit.
func (r *Repo) RawTreeCommit(entries []object.TreeEntry) plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	sorted := make([]object.TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Sort(object.TreeEntrySorter(sorted))

	tree := &object.Tree{Entries: sorted}
	treeObj := r.st.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		r.tb.Fatalf("fakegit: encode raw tree: %v", err)
	}
	treeHash, err := r.st.SetEncodedObject(treeObj)
	if err != nil {
		r.tb.Fatalf("fakegit: store raw tree: %v", err)
	}

	sig := fixedSignature()
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   "raw tree\n",
		TreeHash:  treeHash,
	}
	commitObj := r.st.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		r.tb.Fatalf("fakegit: encode raw commit: %v", err)
	}
	commitHash, err := r.st.SetEncodedObject(commitObj)
	if err != nil {
		r.tb.Fatalf("fakegit: store raw commit: %v", err)
	}
	return commitHash
}

// AddCollection writes a minimal but complete collection source tree under
// dir ("" for the repository root) and stages it: galaxy.yml naming
// namespace, name and version with readme README.md, authors [fakegit], the
// given dependencies and build_ignore patterns, plus README.md, one module
// under plugins/modules and meta/runtime.yml. It does not commit, so a
// caller can add several collections, or extra files, before one Commit.
func (r *Repo) AddCollection(dir, namespace, name, version string, deps map[string]string, buildIgnore []string) {
	r.tb.Helper()
	join := func(p string) string {
		if dir == "" {
			return p
		}
		return path.Join(dir, p)
	}
	r.WriteFile(join(galaxyFileName), fixtureFileMode, galaxyYML(namespace, name, version, deps, buildIgnore))
	r.WriteFile(join(readmeFileName), fixtureFileMode, []byte("# "+namespace+"."+name+"\n"))
	r.WriteFile(join(moduleFileName), fixtureFileMode, []byte("#!/usr/bin/python\nDOCUMENTATION = ''\n"))
	r.WriteFile(join(runtimeFileName), fixtureFileMode, []byte("---\nrequires_ansible: '>=2.15.0'\n"))
}

// AddRole writes a minimal role tree at the repository root and stages it:
// meta/main.yml with galaxy_info naming the fixture author and, when roleName
// is not empty, role_name, plus the dependencies as plain strings in the
// order given; tasks/main.yml with one debug task; and an empty
// defaults/main.yml. It does not commit, so a caller can add files before one
// Commit. A role lives at the root alone, which is why there is no dir
// parameter.
func (r *Repo) AddRole(deps []string, roleName string) {
	r.tb.Helper()
	r.WriteFile(roleMetaFileName, fixtureFileMode, roleMetaYML(deps, roleName))
	r.WriteFile(roleTasksFileName, fixtureFileMode, []byte("- debug: msg=hi\n"))
	r.WriteFile(roleDefaultsFileName, fixtureFileMode, []byte("---\n"))
}

// stage adds p to the index. The caller holds r.mu.
func (r *Repo) stage(p string) {
	r.tb.Helper()
	wt, err := r.repo.Worktree()
	if err != nil {
		r.tb.Fatalf("fakegit: worktree: %v", err)
	}
	if _, err := wt.Add(p); err != nil {
		r.tb.Fatalf("fakegit: stage %s: %v", p, err)
	}
}

// setRef stores ref, replacing any reference of the same name.
func (r *Repo) setRef(ref *plumbing.Reference) {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.st.SetReference(ref); err != nil {
		r.tb.Fatalf("fakegit: set %s: %v", ref.Name(), err)
	}
}

// fixedSignature is the author, committer and tagger of everything this
// package writes.
func fixedSignature() object.Signature {
	return object.Signature{Name: fixtureAuthor, Email: fixtureEmail, When: fixedTime}
}

// galaxyYML renders the galaxy.yml AddCollection writes. Dependencies are
// emitted in sorted key order so the file, and the tree hash over it, is
// deterministic. Values are quoted, since a version constraint such as
// ">=1.0.0" is not a bare YAML scalar.
func galaxyYML(namespace, name, version string, deps map[string]string, buildIgnore []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "namespace: %s\nname: %s\nversion: %q\nreadme: %s\nauthors:\n  - %s\n",
		namespace, name, version, readmeFileName, fixtureAuthor)
	if len(deps) == 0 {
		b.WriteString("dependencies: {}\n")
	} else {
		keys := make([]string, 0, len(deps))
		for k := range deps {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("dependencies:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %q: %q\n", k, deps[k])
		}
	}
	if len(buildIgnore) == 0 {
		b.WriteString("build_ignore: []\n")
	} else {
		b.WriteString("build_ignore:\n")
		for _, pattern := range buildIgnore {
			fmt.Fprintf(&b, "  - %q\n", pattern)
		}
	}
	return []byte(b.String())
}

// roleMetaYML renders the meta/main.yml AddRole writes. Dependencies are
// quoted so a spec carrying a colon, such as a repository URL, stays one
// YAML string.
func roleMetaYML(deps []string, roleName string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "galaxy_info:\n  author: %s\n", fixtureAuthor)
	if roleName != "" {
		fmt.Fprintf(&b, "  role_name: %s\n", roleName)
	}
	if len(deps) == 0 {
		b.WriteString("dependencies: []\n")
		return []byte(b.String())
	}
	b.WriteString("dependencies:\n")
	for _, dep := range deps {
		fmt.Fprintf(&b, "  - %q\n", dep)
	}
	return []byte(b.String())
}
