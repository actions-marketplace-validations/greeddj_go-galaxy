package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
	"gopkg.in/yaml.v3"
)

// GalaxyYAML represents the GALAXY.yml metadata file.
type GalaxyYAML struct {
	DownloadURL string `yaml:"download_url"`
	FormatVer   string `yaml:"format_version"`
	Name        string `yaml:"name"`
	Namespace   string `yaml:"namespace"`
	Server      string `yaml:"server"`
	Signatures  any    `yaml:"signatures"`
	Version     string `yaml:"version"`
	VersionURL  string `yaml:"version_url"`
}

// writeGalaxyInfo writes GALAXY.yml for the installed collection. When meta
// is nil (artifact-cache-hit fast path), a minimal GALAXY.yml is written
// using fields available from the collection identity.
func writeGalaxyInfo(cfg *config.Config, col collection, meta *types.GalaxyCollectionVersionInfo) error {
	g := buildGalaxyYAML(cfg, col, meta)
	infoDir := filepath.Join(
		cfg.DownloadPath,
		"ansible_collections",
		fmt.Sprintf("%s.%s-%s.info", g.Namespace, g.Name, g.Version),
	)
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		return err
	}
	data, err := yaml.Marshal(&g)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), data, helpers.FileMod)
}

func buildGalaxyYAML(cfg *config.Config, col collection, meta *types.GalaxyCollectionVersionInfo) GalaxyYAML {
	if meta == nil {
		return GalaxyYAML{
			FormatVer: "1.0.0",
			Name:      col.Name,
			Namespace: col.Namespace,
			Server:    cfg.Server,
			Version:   col.Version,
		}
	}
	return GalaxyYAML{
		DownloadURL: withoutQuery(meta.DownloadURL),
		FormatVer:   "1.0.0",
		Name:        meta.Name,
		Namespace:   meta.Namespace.Name,
		Server:      cfg.Server,
		Signatures:  meta.Signatures,
		Version:     meta.Version,
		VersionURL:  withoutQuery(meta.Href),
	}
}

// withoutQuery returns raw up to, but not including, its first "?".
//
// A Galaxy NG or Automation Hub deployment backs its artifacts with object
// storage and answers with a presigned download URL, whose query string is
// a time-limited bearer capability: anyone holding that exact URL can fetch
// the artifact without credentials. GALAXY.yml is written into the
// collections tree, which routinely outlives the run and gets uploaded
// wholesale as a CI artifact, so persisting the query there hands that
// capability to everyone who can read the build's output. The scheme, host
// and path are kept, since they are the informational part - where this
// collection actually came from - and carry no capability on their own.
//
// The cut is textual rather than a url.Parse round trip precisely because
// it cannot fail: the first literal "?" always begins the query, so a URL
// this tool could not parse still gets stripped rather than silently
// written through intact.
func withoutQuery(raw string) string {
	before, _, _ := strings.Cut(raw, "?")
	return before
}
