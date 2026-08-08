package collections

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// presignedQuery is the shape of a real object-storage presigned URL's query
// string: a time-limited bearer capability that must never be persisted into
// the collections tree.
const presignedQuery = "?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeefcafe&X-Amz-Expires=600"

// newVersionInfo builds the version metadata writeGalaxyInfo consumes, with
// the given download and version URLs.
func newVersionInfo(downloadURL, href string) *types.GalaxyCollectionVersionInfo {
	info := &types.GalaxyCollectionVersionInfo{}
	info.DownloadURL = downloadURL
	info.Href = href
	info.Name = "widgets"
	info.Namespace.Name = "acme"
	info.Version = testVersion100
	return info
}

// TestBuildGalaxyYAMLStripsPresignedQuery asserts the capability-bearing
// query string of a presigned download URL is dropped while the part that
// merely says where the artifact came from is kept, and that the same holds
// for the version URL.
func TestBuildGalaxyYAMLStripsPresignedQuery(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Server: "https://hub.example.com/api/automation-hub"}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	meta := newVersionInfo(
		"https://objects.example.com/artifacts/acme-widgets-1.0.0.tar.gz"+presignedQuery,
		"https://hub.example.com/api/automation-hub/v3/collections/acme/widgets/versions/1.0.0/"+presignedQuery,
	)

	g := buildGalaxyYAML(cfg, col, meta)

	if want := "https://objects.example.com/artifacts/acme-widgets-1.0.0.tar.gz"; g.DownloadURL != want {
		t.Errorf("download_url = %q, want %q", g.DownloadURL, want)
	}
	if want := "https://hub.example.com/api/automation-hub/v3/collections/acme/widgets/versions/1.0.0/"; g.VersionURL != want {
		t.Errorf("version_url = %q, want %q", g.VersionURL, want)
	}
}

// TestBuildGalaxyYAMLKeepsQuerylessURLs asserts the strip is a no-op for the
// public Galaxy shape, whose download URL carries no query at all.
func TestBuildGalaxyYAMLKeepsQuerylessURLs(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Server: "https://galaxy.ansible.com"}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	downloadURL := "https://galaxy.ansible.com/download/acme-widgets-1.0.0.tar.gz"
	href := "https://galaxy.ansible.com/api/v3/collections/acme/widgets/versions/1.0.0/"
	meta := newVersionInfo(downloadURL, href)

	g := buildGalaxyYAML(cfg, col, meta)

	if g.DownloadURL != downloadURL {
		t.Errorf("download_url = %q, want it unchanged: %q", g.DownloadURL, downloadURL)
	}
	if g.VersionURL != href {
		t.Errorf("version_url = %q, want it unchanged: %q", g.VersionURL, href)
	}
}

// TestBuildGalaxyYAMLNilMetaUnchanged asserts the artifact-cache-hit fast
// path, which has no version metadata to strip anything from, still writes
// the same minimal document it always did.
func TestBuildGalaxyYAMLNilMetaUnchanged(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Server: "https://galaxy.ansible.com"}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}

	g := buildGalaxyYAML(cfg, col, nil)

	if g.DownloadURL != "" || g.VersionURL != "" {
		t.Errorf("download_url = %q and version_url = %q, want both empty", g.DownloadURL, g.VersionURL)
	}
	if g.Namespace != "acme" || g.Name != "widgets" || g.Version != "1.0.0" {
		t.Errorf("identity = %s.%s-%s, want acme.widgets-1.0.0", g.Namespace, g.Name, g.Version)
	}
	if g.Server != cfg.Server {
		t.Errorf("server = %q, want %q", g.Server, cfg.Server)
	}
}

// TestWriteGalaxyInfoPersistsNoSignature is the end-to-end guard: the bytes
// actually landing in the collections tree must not contain any part of the
// presigned query, since that file routinely outlives the run and is
// uploaded wholesale as a CI artifact.
func TestWriteGalaxyInfoPersistsNoSignature(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://hub.example.com/api/automation-hub", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	meta := newVersionInfo("https://objects.example.com/artifacts/acme-widgets-1.0.0.tar.gz"+presignedQuery, "")
	target := newTestInstallTarget(t, cfg, col)

	if err := writeGalaxyInfo(target, cfg, col, meta); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	path := filepath.Join(root, "ansible_collections", "acme.widgets-1.0.0.info", "GALAXY.yml")
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, marker := range []string{"X-Amz-Signature", "X-Amz-Algorithm", "X-Amz-Expires", "?"} {
		if strings.Contains(string(data), marker) {
			t.Errorf("GALAXY.yml contains %q, want the presigned query stripped:\n%s", marker, data)
		}
	}

	var g GalaxyYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal GALAXY.yml: %v", err)
	}
	if want := "https://objects.example.com/artifacts/acme-widgets-1.0.0.tar.gz"; g.DownloadURL != want {
		t.Errorf("download_url = %q, want %q", g.DownloadURL, want)
	}
}

// TestNewInstallTargetRefusesTraversingVersionBeforeWriteGalaxyInfo proves
// the identity guard lives in newInstallTarget, one call before
// writeGalaxyInfo ever runs: a version identifier with enough ".." segments
// to escape DownloadPath must be refused there, before a target (and so
// before any write) ever exists, not merely produce a directory somewhere
// unexpected. writeGalaxyInfo itself is deliberately not called here - it
// trusts target's already-validated identity (see its own doc comment) -
// the discriminating proof for this guard is newInstallTarget's own
// ok=false return plus the untouched victim directory.
//
// Five ".." segments, not three: path.Join fuses the version's first ".."
// into the synthetic "acme.widgets-.." element, consuming it for free, so
// climbing three real directories (DownloadPath's own depth here) takes one
// extra ".." beyond that. With only three, the (never computed) join would
// stay inside DownloadPath and this test would pass for the wrong reason.
func TestNewInstallTargetRefusesTraversingVersionBeforeWriteGalaxyInfo(t *testing.T) {
	t.Parallel()

	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "project", "collections")
	victim := filepath.Join(sandbox, "victim")
	mustMkdirAll(t, victim)
	mustMkdirAll(t, downloadPath)

	cfg := &config.Config{DownloadPath: downloadPath}
	col := collection{Namespace: "acme", Name: "widgets", Version: "../../../../../victim/pwned"}
	root := newTestCollectionsRoot(t, downloadPath)

	if _, ok := newInstallTarget(root, cfg, col); ok {
		t.Fatal("expected newInstallTarget to reject a traversing version, got ok=true")
	}
	// (b) pins that nothing was ever created under the escape target: with no
	// chokepoint call, the join would land inside victim and this assertion is
	// what actually discriminates a deleted guard from a working one.
	if _, statErr := os.Stat(filepath.Join(victim, "pwned.info")); !os.IsNotExist(statErr) {
		t.Fatalf("expected %s/pwned.info to not exist, stat error: %v", victim, statErr)
	}
	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatalf("read dir %s: %v", victim, err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected %s to remain empty, got %d entries", victim, len(entries))
	}
}

// TestWriteGalaxyInfoIgnoresMetaIdentity proves buildGalaxyYAML's identity
// block is col-sourced, not meta-sourced: the file lands under a path built
// from col, and its body's identity fields are col's, even though meta
// carries a different, individually valid identity of its own. All three of
// meta's identity fields are deliberately valid path elements so
// TestWriteGalaxyInfoRefusesTraversingVersion's guard cannot fire here and
// mask this test's own regression.
func TestWriteGalaxyInfoIgnoresMetaIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.ansible.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	meta := newVersionInfo("https://objects.example.com/artifacts/evil-other-9.9.9.tar.gz"+presignedQuery, "")
	meta.Name = "other"
	meta.Namespace.Name = "evil"
	meta.Version = "9.9.9"
	target := newTestInstallTarget(t, cfg, col)

	if err := writeGalaxyInfo(target, cfg, col, meta); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	wantPath := filepath.Join(root, "ansible_collections", "acme.widgets-1.0.0.info", "GALAXY.yml")
	data, err := os.ReadFile(wantPath) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", wantPath, err)
	}
	metaPath := filepath.Join(root, "ansible_collections", "evil.other-9.9.9.info")
	if _, statErr := os.Stat(metaPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected %s to not exist, stat error: %v", metaPath, statErr)
	}

	var g GalaxyYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal GALAXY.yml: %v", err)
	}
	if g.Namespace != col.Namespace || g.Name != col.Name || g.Version != col.Version {
		t.Errorf("identity = %s.%s-%s, want %s.%s-%s", g.Namespace, g.Name, g.Version, col.Namespace, col.Name, col.Version)
	}
	if want := "https://objects.example.com/artifacts/evil-other-9.9.9.tar.gz"; g.DownloadURL != want {
		t.Errorf("download_url = %q, want %q (meta-sourced fields must survive)", g.DownloadURL, want)
	}
}

// TestInstallRecordMatchesAgreesWithWriteGalaxyInfo is the correctness half:
// the test that would have caught the original bug, where writeGalaxyInfo
// built its directory from meta's version while installRecordMatches stats
// one built from col's version, so a collection could never be skipped. Both
// real functions are exercised, and no path is built by hand - the point is
// that the two agree.
func TestInstallRecordMatchesAgreesWithWriteGalaxyInfo(t *testing.T) {
	t.Parallel()

	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "install")
	cfg := &config.Config{DownloadPath: downloadPath}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100, Source: "https://galaxy.example.com"}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)
	seedValidExtractMarker(t, target, validMarkerSHA)

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		Source:         col.Source,
		ArtifactSHA256: validMarkerSHA,
		InstalledAt:    time.Now().UTC(),
	})

	// meta's version deliberately disagrees with col's - benign server-side
	// normalization, the very case the design forbids comparing against.
	meta := newVersionInfo("https://galaxy.example.com/download/acme-widgets-1.0.0.tar.gz", "")
	meta.Version = "9.9.9"

	if err := writeGalaxyInfo(target, cfg, col, meta); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	if !installRecordMatches(target, col, st) {
		t.Fatal("expected installRecordMatches to agree with the GALAXY.yml writeGalaxyInfo actually wrote")
	}
}

// An unsafe col.Version (e.g. "../../../../victim/pwned") never reaches
// installRecordMatches at all: newInstallTarget - the single chokepoint both
// the install path and the .info sidecar path go through - refuses to
// build a target for such an identity in the first place, so there is no
// coincidentally-matching decoy for installRecordMatches to be tricked by.
// See TestNewInstallTargetRejectsUnsafeIdentifiers (installroot_test.go) for
// that guard's own coverage, independent per component.

// TestWriteGalaxyInfoIfPresentWarnsOnCollectionsPathEscape proves the
// log-tier split for the reachable escape arm: after target was built
// validly, ansible_collections is swapped for a symlink pointing entirely
// outside DownloadPath, so writeGalaxyInfo's own rooted MkdirAll refuses it.
// This is a security-relevant signal and must reach Warnf (which survives
// --quiet), never the best-effort Printf tier ordinary I/O failures use, and
// the function still returns normally either way.
func TestWriteGalaxyInfoIfPresentWarnsOnCollectionsPathEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(root, "ansible_collections")); err != nil {
		t.Fatalf("remove ansible_collections: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections: %v", err)
	}

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	writeGalaxyInfoIfPresent(runtime, target, cfg, col, nil)

	if len(printer.warns) != 1 {
		t.Fatalf("warns = %v, want exactly one warning", printer.warns)
	}
	if !strings.Contains(printer.warns[0], "Refusing to write GALAXY.yml") {
		t.Errorf("warns[0] = %q, want it to mention refusing to write GALAXY.yml", printer.warns[0])
	}
	if len(printer.prints) != 0 {
		t.Errorf("prints = %v, want none: a path escape must not also hit the ordinary-failure tier", printer.prints)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("expected nothing written outside DownloadPath, entries=%v err=%v", entries, err)
	}
}

// TestWriteGalaxyInfoIfPresentPrintsOnOrdinaryFailure is the mirror of
// TestWriteGalaxyInfoIfPresentWarnsOnCollectionsPathEscape: an ordinary I/O
// failure - col's identity is safe and target.root sees no symlink - must
// land on the best-effort Printf tier, never Warnf. Neither test alone pins
// the split; a mutation that collapses both arms onto one tier passes one of
// the two and fails the other.
//
// The failure is manufactured by stripping write permission from
// "ansible_collections" itself, rather than by pre-seeding target.info with a
// regular file: writeGalaxyInfo resets target.info (RemoveAll then MkdirAll)
// before writing, and RemoveAll happily unlinks a lone regular file sitting
// at that name, so a pre-seeded regular file would not produce a failure at
// all - MkdirAll would simply succeed once the file is removed. A
// permission-denied MkdirAll survives the reset unaffected: there is nothing
// at target.info to remove, and creating it is what fails.
func TestWriteGalaxyInfoIfPresentPrintsOnOrdinaryFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based write guard cannot be tested")
	}

	root := t.TempDir()
	cfg := &config.Config{DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	collectionsDir := filepath.Join(root, "ansible_collections")
	//nolint:gosec // G302: 0o555 is this test's own fixture permission, restored in t.Cleanup below.
	if err := os.Chmod(collectionsDir, 0o555); err != nil {
		t.Fatalf("chmod ansible_collections: %v", err)
	}
	// Restored before t.TempDir's own cleanup runs, which needs to remove
	// ansible_collections itself.
	t.Cleanup(func() {
		if err := os.Chmod(collectionsDir, helpers.DirMod); err != nil {
			t.Errorf("restore ansible_collections perms: %v", err)
		}
	})

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	writeGalaxyInfoIfPresent(runtime, target, cfg, col, nil)

	if len(printer.prints) != 1 {
		t.Fatalf("prints = %v, want exactly one", printer.prints)
	}
	if !strings.Contains(printer.prints[0], "Failed to write GALAXY.yml") {
		t.Errorf("prints[0] = %q, want it to mention the write failure", printer.prints[0])
	}
	if len(printer.warns) != 0 {
		t.Errorf("warns = %v, want none: an ordinary I/O failure must not also hit the security-signal tier", printer.warns)
	}
}
