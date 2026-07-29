package collections

import (
	"errors"
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
	"gopkg.in/yaml.v3"
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

	if err := writeGalaxyInfo(cfg, col, meta); err != nil {
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

// TestWriteGalaxyInfoRefusesTraversingVersion proves the collectionInfoDir
// chokepoint closes the path-traversal primitive: a version identifier with
// enough ".." segments to escape DownloadPath must be refused before
// anything is written, not merely produce a directory somewhere unexpected.
//
// Five ".." segments, not three: filepath.Join fuses the version's first
// ".." into the synthetic "acme.widgets-.." element, consuming it for free,
// so climbing three real directories (DownloadPath's own depth here) takes
// one extra ".." beyond that. With only three, the write would stay inside
// DownloadPath and this test would pass for the wrong reason.
func TestWriteGalaxyInfoRefusesTraversingVersion(t *testing.T) {
	t.Parallel()

	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "project", "collections")
	victim := filepath.Join(sandbox, "victim")
	mustMkdirAll(t, victim)

	cfg := &config.Config{DownloadPath: downloadPath}
	col := collection{Namespace: "acme", Name: "widgets", Version: "../../../../../victim/pwned"}

	err := writeGalaxyInfo(cfg, col, nil)
	// (a) is what actually kills a deleted guard: with no chokepoint call,
	// writeGalaxyInfo returns nil (or some unrelated filesystem error), never
	// this sentinel.
	if !errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) {
		t.Fatalf("writeGalaxyInfo error = %v, want errors.Is helpers.ErrUnsafeCollectionIdentifier", err)
	}
	// (b) pins the ordering: against a mutation that defers the guard past
	// os.MkdirAll (but still returns the sentinel afterward), this is the
	// assertion that fires first, since the escape target would already exist.
	if _, statErr := os.Stat(filepath.Join(victim, "pwned.info")); !os.IsNotExist(statErr) {
		t.Fatalf("expected %s/pwned.info to not exist, stat error: %v", victim, statErr)
	}
	// (c) broadens (b) from the one expected escape name to the whole
	// directory: nothing at all was created under the escape target, not just
	// the specific "pwned.info" name a narrower mutation might have avoided.
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

	if err := writeGalaxyInfo(cfg, col, meta); err != nil {
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
	installPath := collectionInstallPath(cfg, col)
	mustMkdirAll(t, installPath)
	seedValidExtractMarker(t, installPath, validMarkerSHA)

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    installPath,
		Source:         col.Source,
		ArtifactSHA256: validMarkerSHA,
		InstalledAt:    time.Now().UTC(),
	})

	// meta's version deliberately disagrees with col's - benign server-side
	// normalization, the very case the design forbids comparing against.
	meta := newVersionInfo("https://galaxy.example.com/download/acme-widgets-1.0.0.tar.gz", "")
	meta.Version = "9.9.9"

	if err := writeGalaxyInfo(cfg, col, meta); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	if !installRecordMatches(cfg, col, installPath, st) {
		t.Fatal("expected installRecordMatches to agree with the GALAXY.yml writeGalaxyInfo actually wrote")
	}
}

// TestInstallRecordMatchesRefusesUnsafeIdentifier proves the checker half of
// the chokepoint: an unsafe col.Version must make installRecordMatches
// report false even though the store entry lookup and the extract marker
// stat both succeed, so neither of those can shadow the collectionInfoDir
// guard's own contribution to the result.
//
// A decoy is seeded at exactly the location the pre-fix inline
// filepath.Join(cfg.DownloadPath, "ansible_collections",
// fmt.Sprintf("%s.%s-%s.info", col.Namespace, col.Name, col.Version)) would
// have found present, so a bare os.Stat-based check would have coincidentally
// succeeded and falsely reported a match - the same shape as
// TestInstallRecordMatchesRefusesUnsafeMarkerSHA uses for the marker sha.
// With DownloadPath = <sandbox>/install and col.Version =
// "../../../../victim/pwned", the inline join's third path element,
// "acme.widgets-../../../../victim/pwned.info", is not a single literal
// element at all: it contains "/" and gets split by filepath.Join like any
// other path segment. Its first ".." fuses into the synthetic
// "acme.widgets-.." element (absorbed for free, same as elsewhere in this
// file), and the remaining three ".." pop back up through
// ansible_collections, install, and DownloadPath's own parent, landing at
// <sandbox>. From there "victim" and "pwned.info" resolve literally, so the
// decoy sits at <sandbox>/victim/pwned.info - the ".info" suffix lands on the
// final path component ("pwned"), not on a synthetic "<ns>.<name>-<version>"
// element the way it does for a safe identifier.
func TestInstallRecordMatchesRefusesUnsafeIdentifier(t *testing.T) {
	t.Parallel()

	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "install")
	cfg := &config.Config{DownloadPath: downloadPath}
	col := collection{
		Namespace: "acme", Name: "widgets", Version: "../../../../victim/pwned",
		Source: "https://galaxy.example.com",
	}
	installPath := collectionInstallPath(cfg, col)
	mustMkdirAll(t, installPath)
	seedValidExtractMarker(t, installPath, validMarkerSHA)

	decoyDir := filepath.Join(sandbox, "victim", "pwned.info")
	mustMkdirAll(t, decoyDir)
	mustWriteFile(t, filepath.Join(decoyDir, galaxyYAMLFileName), []byte("format_version: 1.0.0\n"))

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    installPath,
		Source:         col.Source,
		ArtifactSHA256: validMarkerSHA,
		InstalledAt:    time.Now().UTC(),
	})

	if installRecordMatches(cfg, col, installPath, st) {
		t.Fatal("expected installRecordMatches to refuse an unsafe col.Version rather than coincidentally match the decoy")
	}
}

// TestWriteGalaxyInfoIfPresentWarnsOnUnsafeIdentifier proves the log-tier
// split: an unsafe identifier is a security-relevant signal that must reach
// Warnf (which survives --quiet), never the best-effort Printf tier ordinary
// I/O failures use, and the function still returns normally either way.
func TestWriteGalaxyInfoIfPresentWarnsOnUnsafeIdentifier(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: "../etc"}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	writeGalaxyInfoIfPresent(runtime, cfg, col, nil)

	if len(printer.warns) != 1 {
		t.Fatalf("warns = %v, want exactly one warning", printer.warns)
	}
	if !strings.Contains(printer.warns[0], "Refusing to write GALAXY.yml") {
		t.Errorf("warns[0] = %q, want it to mention refusing to write GALAXY.yml", printer.warns[0])
	}
	if len(printer.prints) != 0 {
		t.Errorf("prints = %v, want none: an unsafe identifier must not also hit the ordinary-failure tier", printer.prints)
	}
}

// TestWriteGalaxyInfoIfPresentPrintsOnOrdinaryFailure is the mirror of
// TestWriteGalaxyInfoIfPresentWarnsOnUnsafeIdentifier: an ordinary I/O
// failure - col's identity is safe, so writeGalaxyInfo never returns
// helpers.ErrUnsafeCollectionIdentifier - must land on the best-effort Printf
// tier, never Warnf. Neither test alone pins the split; a mutation that
// collapses both arms onto one tier passes one of the two and fails the
// other.
func TestWriteGalaxyInfoIfPresentPrintsOnOrdinaryFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}

	// A regular file where infoDir's parent directory ("ansible_collections")
	// needs to be makes os.MkdirAll fail with ENOTDIR - a *fs.PathError that
	// is definitively not helpers.ErrUnsafeCollectionIdentifier. Chosen over a
	// permission-bit mutation because it needs no chmod and no privileged
	// state to set up or tear down.
	mustWriteFile(t, filepath.Join(root, "ansible_collections"), []byte("not a directory"))

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	writeGalaxyInfoIfPresent(runtime, cfg, col, nil)

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
