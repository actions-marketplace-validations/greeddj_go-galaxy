package collections

// This file proves writeGalaxyInfo's reset (target.info: RemoveAll then
// MkdirAll, before the write) actually closes the hole a security audit
// found: os.Root only constrains which paths a method may traverse to reach
// target.info, it says nothing about what already sits at the GALAXY.yml
// leaf inside it once traversal succeeds. Before the reset, a symlink or a
// hardlink pre-planted at that leaf was written straight through. Each test
// here pre-plants one of the three measured shapes directly, without going
// through a real extraction, so the write site under test is exactly
// writeGalaxyInfo and nothing upstream of it.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"go.yaml.in/yaml/v3"
)

// TestWriteGalaxyInfoOverwritesRelativeInRootSymlink proves a relative,
// in-root symlink at the GALAXY.yml leaf - one whose target stays inside the
// collections root, so os.Root permits traversing it, unlike the escaping
// symlinks symlink_escape_test.go covers - is severed by the reset rather
// than followed. Without the reset, target.root.WriteFile would resolve the
// symlink and overwrite whatever real content the leaf pointed at; a
// hostile requirements.yml can plant this shape purely through its own
// namespace/name/version (the same three components a fresh checkout could
// ship a matching symlink for), no race required.
func TestWriteGalaxyInfoOverwritesRelativeInRootSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	infoDir := filepath.Join(root, target.info)
	mustMkdirAll(t, infoDir)
	victim := filepath.Join(root, "victim.txt")
	const victimContent = "# real content elsewhere in the collections root\n"
	mustWriteFile(t, victim, []byte(victimContent))

	// Relative to infoDir: ".." reaches "ansible_collections", ".." again
	// reaches root - the symlink stays inside the collections root the whole
	// way, so os.Root permits traversing it.
	leaf := filepath.Join(infoDir, galaxyYAMLFileName)
	if err := os.Symlink(filepath.Join("..", "..", "victim.txt"), leaf); err != nil {
		t.Fatalf("symlink GALAXY.yml -> ../../victim.txt: %v", err)
	}

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	// The reset is what makes this assertion pass: without it, the WriteFile
	// that follows would have resolved the symlink and overwritten victim.txt
	// with GALAXY.yml's own content instead.
	assertFileContent(t, victim, victimContent)

	info, err := os.Lstat(leaf)
	if err != nil {
		t.Fatalf("lstat %s: %v", leaf, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected %s to be a regular file after the reset, still a symlink", leaf)
	}
	assertGalaxyYAMLIdentity(t, leaf, col)
}

// TestWriteGalaxyInfoDoesNotCreateDanglingSymlinkTarget proves a dangling
// in-root symlink at the GALAXY.yml leaf - pointing at a path that does not
// exist yet, still inside the root - does not get its target materialized.
// Without the reset, target.root.WriteFile follows the symlink and creates a
// brand-new file at whatever location the attacker named, anywhere inside
// the collections root.
func TestWriteGalaxyInfoDoesNotCreateDanglingSymlinkTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	infoDir := filepath.Join(root, target.info)
	mustMkdirAll(t, infoDir)
	ghost := filepath.Join(root, "ghost.txt")

	leaf := filepath.Join(infoDir, galaxyYAMLFileName)
	if err := os.Symlink(filepath.Join("..", "..", "ghost.txt"), leaf); err != nil {
		t.Fatalf("symlink GALAXY.yml -> ../../ghost.txt (dangling): %v", err)
	}

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	assertPathAbsent(t, ghost)

	info, err := os.Lstat(leaf)
	if err != nil {
		t.Fatalf("lstat %s: %v", leaf, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected %s to be a regular file after the reset, still a symlink", leaf)
	}
	assertGalaxyYAMLIdentity(t, leaf, col)
}

// TestWriteGalaxyInfoDoesNotCorruptHardlinkedContentOutsideRoot proves a
// hardlink planted at the GALAXY.yml leaf - a second name for an inode whose
// other name lives entirely outside the collections root, standing in for
// the shared content-addressable extracted store under cfg.CacheDir - is
// unlinked, not written through. os.Root is path-based and cannot see a
// hardlink at all: unlike a symlink, there is no traversal for it to refuse,
// so the reset (an unlink of the in-root name) is the only defense that
// exists at this leaf. Before the reset, WriteFile followed the existing
// directory entry and wrote new bytes into the shared inode, corrupting
// content every other project hardlinking the same inode still relies on.
func TestWriteGalaxyInfoDoesNotCorruptHardlinkedContentOutsideRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.example.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	infoDir := filepath.Join(root, target.info)
	mustMkdirAll(t, infoDir)

	// outside stands in for cfg.CacheDir's content-addressable extracted
	// store: a real file with content other projects depend on, entirely
	// outside the collections root.
	outsideDir := t.TempDir()
	shared := filepath.Join(outsideDir, "shared-content.bin")
	const sharedContent = "# content another project's hardlink still relies on\n"
	mustWriteFile(t, shared, []byte(sharedContent))

	leaf := filepath.Join(infoDir, galaxyYAMLFileName)
	if err := os.Link(shared, leaf); err != nil {
		t.Fatalf("hardlink GALAXY.yml -> %s: %v", shared, err)
	}

	if err := writeGalaxyInfo(target, cfg, col, nil); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	// The reset's RemoveAll unlinks the in-root directory entry, which only
	// drops the shared inode's link count - it never touches the inode's
	// content, so the other name (shared) must read back unchanged.
	assertFileContent(t, shared, sharedContent)
	assertGalaxyYAMLIdentity(t, leaf, col)
}

// assertGalaxyYAMLIdentity re-reads leaf and checks it unmarshals into a
// GalaxyYAML whose identity matches col - the positive half of each test
// above: writeGalaxyInfo did not merely avoid corrupting something, it also
// actually produced the real sidecar content at that name.
func assertGalaxyYAMLIdentity(t *testing.T, leaf string, col collection) {
	t.Helper()
	data, err := os.ReadFile(leaf) // #nosec G304 -- leaf is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", leaf, err)
	}
	var g GalaxyYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal %s: %v", leaf, err)
	}
	if g.Namespace != col.Namespace || g.Name != col.Name || g.Version != col.Version {
		t.Errorf("identity = %s.%s-%s, want %s.%s-%s", g.Namespace, g.Name, g.Version, col.Namespace, col.Name, col.Version)
	}
}
