package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Config holds runtime settings for collection operations.
type Config struct {
	AnsibleConfigPath          string
	RequirementsFile           string
	LockFile                   string
	MetricsFile                string
	CacheDir                   string
	DownloadPath               string
	Server                     string
	Resolution                 string
	S3Cache                    S3CacheConfig
	Timeout                    time.Duration
	Workers                    int
	Refresh                    bool
	NoCache                    bool
	NoDeps                     bool
	DryRun                     bool
	Verbose                    bool
	Quiet                      bool
	ClearCache                 bool
	Offline                    bool
	Frozen                     bool
	WarmOnly                   bool
	AnsibleCollectionsPathUsed bool
	AnsibleCacheDirUsed        bool
	AnsibleServerUsed          bool
}

// IsNoCache reports whether cache reads and writes are disabled.
func (c *Config) IsNoCache() bool {
	if c == nil {
		return false
	}
	return c.NoCache
}

// IsRefresh reports whether cache refresh is requested.
func (c *Config) IsRefresh() bool {
	if c == nil {
		return false
	}
	return c.Refresh
}

// IsOffline reports whether network access is forbidden.
func (c *Config) IsOffline() bool {
	if c == nil {
		return false
	}
	return c.Offline
}

// IsLenient reports whether the resolver should fall back to best-effort
// version selection when strict constraint satisfaction is impossible.
func (c *Config) IsLenient() bool {
	if c == nil {
		return false
	}
	return c.Resolution == "lenient" || c.Resolution == "backtrack"
}

// IsBacktrack reports whether the resolver should attempt single-constraint
// backtracking before falling back to max-satisfaction.
func (c *Config) IsBacktrack() bool {
	if c == nil {
		return false
	}
	return c.Resolution == "backtrack"
}

// CollectionOptions captures collection install options before normalization.
type CollectionOptions struct {
	CacheDir            string
	RequirementsFile    string
	LockFile            string
	MetricsFile         string
	Server              string
	DownloadPath        string
	Timeout             time.Duration
	ClearCache          bool
	Verbose             bool
	NoCache             bool
	Refresh             bool
	NoDeps              bool
	Offline             bool
	Frozen              bool
	WarmOnly            bool
	DownloadPathSet     bool
	RequirementsFileSet bool
	Quiet               bool
}

// BuildCollectionConfig builds Config from CLI flags and ansible.cfg.
func BuildCollectionConfig(c *cli.Command) (*Config, error) {
	cfg := newConfigFromCLI(c)
	applyTimeout(cfg, c)

	ansibleConfig, ansiblePath, err := loadAnsibleConfigFromCLI(c)
	if err != nil {
		return nil, err
	}
	applyAnsibleConfig(cfg, c, ansibleConfig, ansiblePath)

	s3Cfg, err := loadS3CacheConfig(c)
	if err != nil {
		return nil, err
	}
	cfg.S3Cache = s3Cfg

	return cfg, nil
}

func newConfigFromCLI(c *cli.Command) *Config {
	cfg := &Config{
		Workers:          c.Int("workers"),
		RequirementsFile: c.String("requirements-file"),
		LockFile:         c.String("lock-file"),
		MetricsFile:      c.String("metrics-file"),
		ClearCache:       c.Bool("clear-cache"),
		NoCache:          c.Bool("no-cache"),
		Refresh:          c.Bool("refresh"),
		NoDeps:           c.Bool("no-deps"),
		DryRun:           c.Bool("dry-run"),
		Offline:          c.Bool("offline"),
		Frozen:           c.Bool("frozen"),
		Resolution:       c.String("resolution"),
		DownloadPath:     c.String("download-path"),
	}

	if cfg.Workers < 1 {
		cfg.Workers = runtime.NumCPU()
	}
	cfg.Verbose = c.Bool("verbose")
	cfg.Quiet = !cfg.Verbose && c.Bool("quiet")
	return cfg
}

func applyTimeout(cfg *Config, c *cli.Command) {
	cfg.Timeout = c.Duration("timeout")
	cfg.Timeout = max(cfg.Timeout, helpers.FetchDefaultTimeout)
}

func loadAnsibleConfigFromCLI(c *cli.Command) (ansibleConfig, string, error) {
	ansibleConfig, ansiblePath, err := loadAnsibleConfig(c.String("ansible-config"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ansibleConfig, "", fmt.Errorf("failed to load ansible config: %w", err)
	}
	return ansibleConfig, ansiblePath, nil
}

func applyAnsibleConfig(cfg *Config, c *cli.Command, ansibleConfig ansibleConfig, ansiblePath string) {
	if ansiblePath != "" {
		cfg.AnsibleConfigPath = ansiblePath
	}
	cfg.DownloadPath, cfg.AnsibleCollectionsPathUsed = pickConfigValue(c, "download-path", ansibleConfig.Defaults.CollectionsPath)
	cfg.CacheDir, cfg.AnsibleCacheDirUsed = pickConfigValue(c, "cache-dir", ansibleConfig.Galaxy.CacheDir)
	cfg.Server, cfg.AnsibleServerUsed = pickConfigValue(c, "server", ansibleConfig.Galaxy.Server)
}

// pickConfigValue picks a string config value with precedence:
// explicit CLI/ENV (IsSet) > ansible.cfg > CLI default. The bool reports
// whether the value came from ansible.cfg.
func pickConfigValue(c *cli.Command, flag, ansibleValue string) (string, bool) {
	if c.IsSet(flag) {
		return c.String(flag), false
	}
	if ansibleValue != "" {
		return ansibleValue, true
	}
	return c.String(flag), false
}

/*
env: ANSIBLE_CONFIG (environment variable if set)
ansible.cfg (in the current directory)
~/.ansible.cfg (in the home directory)
/etc/ansible/ansible.cfg


[galaxy]
cache_dir // env:ANSIBLE_GALAXY_CACHE_DIR // default {{ ANSIBLE_HOME ~ "/galaxy_cache" }}
server // env:ANSIBLE_GALAXY_SERVER // default https://galaxy.ansible.com
*/

// ansibleGalaxyConfig maps the [galaxy] section from ansible.cfg.
type ansibleGalaxyConfig struct {
	CacheDir string `toml:"cache_dir"`
	Server   string `toml:"server"`
}

// ansibleDefaultsConfig maps the [defaults] section from ansible.cfg.
type ansibleDefaultsConfig struct {
	CollectionsPath string `toml:"collections_path"`
}

// ansibleConfig represents the parsed ansible.cfg structure.
type ansibleConfig struct {
	Defaults ansibleDefaultsConfig `toml:"defaults"`
	Galaxy   ansibleGalaxyConfig   `toml:"galaxy"`
}

// loadAnsibleConfig loads ansible.cfg if it exists.
func loadAnsibleConfig(configPath string) (ansibleConfig, string, error) {
	config := ansibleConfig{}
	if _, err := os.Stat(configPath); err != nil {
		return config, "", err
	}
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		return config, "", fmt.Errorf("failed parse ansible.cfg: %w", err)
	}
	return config, configPath, nil
}
