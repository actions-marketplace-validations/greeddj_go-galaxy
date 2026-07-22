package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.lock.yml")

	f := &File{
		Server: "https://galaxy.ansible.com",
		Collections: []Entry{
			{
				Name:    "community.general",
				Version: "11.1.0",
				Source:  "https://galaxy.ansible.com",
				SHA256:  "deadbeef",
				Deps:    []string{"ansible.posix"},
			},
			{
				Name:    "ansible.posix",
				Version: "2.0.0",
				Source:  "https://galaxy.ansible.com",
				SHA256:  "feedface",
			},
		},
	}
	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("schema=%d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if len(got.Collections) != 2 {
		t.Fatalf("expected 2 collections, got %d", len(got.Collections))
	}
	if got.Collections[0].Name != "ansible.posix" {
		t.Fatalf("expected sorted by name first=ansible.posix, got %q", got.Collections[0].Name)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "missing.yml"))
	if !IsNotExist(err) {
		t.Fatalf("expected not-exist, got %v", err)
	}
}

func TestLoadInvalidSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(path, []byte("schema_version: 9999\ncollections: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Fatalf("expected ErrLockfileInvalid, got %v", err)
	}
}

func TestHashIsStable(t *testing.T) {
	t.Parallel()
	a := &File{Collections: []Entry{
		{Name: "b.b", Version: "1.0.0"},
		{Name: "a.a", Version: "1.0.0"},
	}}
	b := &File{Collections: []Entry{
		{Name: "a.a", Version: "1.0.0"},
		{Name: "b.b", Version: "1.0.0"},
	}}
	ah, err := a.Hash()
	if err != nil {
		t.Fatal(err)
	}
	bh, err := b.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if ah != bh {
		t.Fatalf("expected stable hash regardless of input order, got %q vs %q", ah, bh)
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.lock.yml")

	f := &File{Collections: []Entry{{Name: "a.a", Version: "1.0.0"}}}
	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no leftover temp files, found %v", matches)
	}
}

func TestSaveDoesNotClobberOnFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.lock.yml")

	original := &File{Collections: []Entry{
		{Name: "a.a", Version: "1.0.0", SHA256: "original"},
	}}
	if err := Save(path, original); err != nil {
		t.Fatalf("Save (seed): %v", err)
	}

	//nolint:gosec // G302: 0o500 is a directory mode (read+traverse, no write), needed to force Save to fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod dir: %v", err)
	}
	defer func() {
		if err := os.Chmod(dir, helpers.DirMod); err != nil {
			t.Fatalf("restore dir mode: %v", err)
		}
	}()

	updated := &File{Collections: []Entry{
		{Name: "a.a", Version: "2.0.0", SHA256: "updated"},
	}}
	if err := Save(path, updated); err == nil {
		t.Fatalf("expected Save to fail against a read-only directory")
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after failed Save: %v", err)
	}
	if len(got.Collections) != 1 || got.Collections[0].SHA256 != "original" {
		t.Fatalf("expected original content to survive the failed Save, got %+v", got.Collections)
	}
}

func TestResolveDefaultPath(t *testing.T) {
	t.Parallel()
	if got := ResolveDefaultPath("/proj/requirements.yml", ""); got != "/proj/"+DefaultName {
		t.Fatalf("got %q", got)
	}
	if got := ResolveDefaultPath("/proj/requirements.yml", "/etc/lock.yml"); got != "/etc/lock.yml" {
		t.Fatalf("got %q", got)
	}
	if got := ResolveDefaultPath("", ""); got != DefaultName {
		t.Fatalf("got %q", got)
	}
}
