package collectionbuild

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"time"
)

// errMemSource is the test double's own refusal for a path it does not hold.
var errMemSource = errors.New("memsource: no such entry")

// memEntry is one entry of the in-memory tree: the blob for a file or a
// symlink (the target), nothing for a directory or a submodule. sizeOverride,
// when set, is what ReadDir declares instead of the blob length, so a test
// can declare a size no blob backs.
type memEntry struct {
	data         string
	sizeOverride int64
	kind         EntryKind
}

// memSource is a Source over a map of "/"-joined paths. Directories are
// implied by their children and may also be declared empty; ReadDir lists in
// byte order so every test sees one order.
type memSource struct {
	entries map[string]memEntry
	onOpen  func(path string)
	when    time.Time
}

// fixedCommitTime is the committer time every test source reports.
func fixedCommitTime() time.Time {
	return time.Date(2024, time.March, 5, 12, 30, 45, 0, time.UTC)
}

func newMemSource() *memSource {
	return &memSource{entries: map[string]memEntry{}, when: fixedCommitTime()}
}

func (m *memSource) CommitTime() time.Time { return m.when }

func (m *memSource) ReadDir(p string) ([]Entry, error) {
	if p != "" {
		e, ok := m.entries[p]
		if !ok || e.kind != EntryDir {
			return nil, errMemSource
		}
	}
	var names []string
	for name := range m.entries {
		if parentOf(name) == p {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]Entry, 0, len(names))
	for _, name := range names {
		out = append(out, Entry{Name: path.Base(name), Kind: m.entries[name].kind, Size: m.entries[name].declaredSize()})
	}
	return out, nil
}

func (e memEntry) declaredSize() int64 {
	switch {
	case e.sizeOverride != 0:
		return e.sizeOverride
	case e.kind == EntryFile || e.kind == EntryExecutable || e.kind == EntrySymlink:
		return int64(len(e.data))
	default:
		return 0
	}
}

func parentOf(p string) string {
	dir := path.Dir(p)
	if dir == "." {
		return ""
	}
	return dir
}

func (m *memSource) Open(p string) (io.ReadCloser, error) {
	if m.onOpen != nil {
		m.onOpen(p)
	}
	e, ok := m.entries[p]
	if !ok || e.kind == EntryDir || e.kind == EntrySubmodule {
		return nil, errMemSource
	}
	return io.NopCloser(strings.NewReader(e.data)), nil
}

// put stores one entry and implies its parent directories.
func (m *memSource) put(p string, kind EntryKind, data string) *memSource {
	m.entries[p] = memEntry{kind: kind, data: data}
	for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if _, ok := m.entries[dir]; !ok {
			m.entries[dir] = memEntry{kind: EntryDir}
		}
	}
	return m
}

func (m *memSource) file(p, data string) *memSource      { return m.put(p, EntryFile, data) }
func (m *memSource) exec(p, data string) *memSource      { return m.put(p, EntryExecutable, data) }
func (m *memSource) dir(p string) *memSource             { return m.put(p, EntryDir, "") }
func (m *memSource) symlink(p, target string) *memSource { return m.put(p, EntrySymlink, target) }
func (m *memSource) submodule(p string) *memSource       { return m.put(p, EntrySubmodule, "") }

func (m *memSource) declareSize(p string, size int64) *memSource {
	e := m.entries[p]
	e.sizeOverride = size
	m.entries[p] = e
	return m
}

// tempFileIn returns a TempFileFunc creating files under dir.
func tempFileIn(dir string) TempFileFunc {
	return func(_ context.Context) (*os.File, func(), error) {
		f, err := os.CreateTemp(dir, ".download-*")
		if err != nil {
			return nil, nil, err
		}
		name := f.Name()
		return f, func() { _ = os.Remove(name) }, nil
	}
}

// minimalGalaxyYML is a galaxy.yml every build fixture starts from.
const minimalGalaxyYML = "namespace: acme\nname: app\nversion: 1.2.3\nreadme: README.md\nauthors:\n  - A. Author\n"

// candidateFor discovers the one candidate under subdir or fails the test.
func candidateFor(t *testing.T, src Source, subdir string) Candidate {
	t.Helper()
	cands, _, err := Discover(src, subdir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("Discover returned %d candidates, want 1", len(cands))
	}
	return cands[0]
}
