package config

import (
	"bufio"
	"io"
	"strings"
)

// ansibleBOM is the leading UTF-8 byte order mark some ansible.cfg files
// carry (e.g. when authored by editors that default to BOM-prefixed UTF-8).
// ansible's own configparser-based reader tolerates it, so we strip it too.
const ansibleBOM = "\uFEFF"

// ansibleGalaxyConfig maps the [galaxy] section from ansible.cfg (INI).
type ansibleGalaxyConfig struct {
	CacheDir   string
	Server     string
	ServerList string
}

// ansibleDefaultsConfig maps the [defaults] section from ansible.cfg (INI).
type ansibleDefaultsConfig struct {
	CollectionsPath string
}

// ansibleConfig represents the subset of ansible.cfg (INI) sections this
// tool understands: [defaults], [galaxy], and any [galaxy_server.<id>].
type ansibleConfig struct {
	GalaxyServers map[string]map[string]string
	Galaxy        ansibleGalaxyConfig
	Defaults      ansibleDefaultsConfig
}

// parseAnsibleConfig reads an ansible.cfg (INI-style) file and extracts the
// handful of keys this tool cares about. It deliberately mirrors CPython's
// configparser semantics as used by ansible: values are not unquoted and
// inline comments are not stripped, since ansible.cfg is not TOML and we
// aim for drop-in fidelity with how ansible itself reads it.
func parseAnsibleConfig(r io.Reader) (ansibleConfig, error) {
	cfg := ansibleConfig{}
	section := ""

	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, ansibleBOM)
			first = false
		}

		t := strings.TrimSpace(line)
		if t == "" || isCommentLine(t) {
			continue
		}

		if name, ok := sectionName(t); ok {
			section = name
			continue
		}

		if key, value, ok := splitKeyValue(t); ok {
			assignAnsibleValue(&cfg, section, key, value)
		}
	}
	if err := sc.Err(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// isCommentLine reports whether t is a full-line comment. ansible.cfg
// accepts both '#' and ';' as comment markers.
func isCommentLine(t string) bool {
	return strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";")
}

// sectionName reports whether t is a "[section]" header and, if so, returns
// its trimmed inner name. Section names are matched case-sensitively.
func sectionName(t string) (string, bool) {
	if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") {
		return "", false
	}
	name := strings.TrimSpace(t[1 : len(t)-1])
	return name, name != ""
}

// splitKeyValue splits t on the first '=' or ':' delimiter, whichever
// appears first in the line, into a (key, value, ok) triple. This matters
// for values that themselves contain a colon, e.g. "server = https://x"
// must split on '=', not on the colon inside the URL.
func splitKeyValue(t string) (string, string, bool) {
	eq := strings.IndexByte(t, '=')
	colon := strings.IndexByte(t, ':')

	idx := eq
	switch {
	case eq == -1:
		idx = colon
	case colon != -1 && colon < eq:
		idx = colon
	}
	if idx == -1 {
		return "", "", false
	}

	key := strings.ToLower(strings.TrimSpace(t[:idx]))
	value := strings.TrimSpace(t[idx+1:])
	return key, value, key != ""
}

// galaxyServerSectionPrefix is the fixed prefix of a per-server
// configuration section header, "[galaxy_server.<id>]"; everything after
// it is the server's id.
const galaxyServerSectionPrefix = "galaxy_server."

// assignAnsibleValue stores value into cfg for the known (section, key)
// pairs this tool consumes; anything else, including keys seen before any
// section header, is ignored. Later occurrences win over earlier ones. A
// section matching "galaxy_server.<id>" is captured in full via
// assignGalaxyServerValue rather than a fixed key whitelist, since the set
// of keys to recognize (and which ones are errors vs. warnings) is a
// concern of the config resolver, not this parser.
func assignAnsibleValue(cfg *ansibleConfig, section, key, value string) {
	switch section {
	case "defaults":
		if key == "collections_path" {
			cfg.Defaults.CollectionsPath = value
		}
	case "galaxy":
		switch key {
		case "cache_dir":
			cfg.Galaxy.CacheDir = value
		case "server":
			cfg.Galaxy.Server = value
		case "server_list":
			cfg.Galaxy.ServerList = value
		}
	default:
		if id, ok := strings.CutPrefix(section, galaxyServerSectionPrefix); ok {
			assignGalaxyServerValue(cfg, id, key, value)
		}
	}
}

// assignGalaxyServerValue stores key/value into the per-id map for a
// "[galaxy_server.<id>]" section, allocating the outer and inner maps
// lazily. Later occurrences of the same key within the same id win, same
// as every other key this parser tracks.
func assignGalaxyServerValue(cfg *ansibleConfig, id, key, value string) {
	if cfg.GalaxyServers == nil {
		cfg.GalaxyServers = make(map[string]map[string]string)
	}
	if cfg.GalaxyServers[id] == nil {
		cfg.GalaxyServers[id] = make(map[string]string)
	}
	cfg.GalaxyServers[id][key] = value
}
