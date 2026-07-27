package collections

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
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
