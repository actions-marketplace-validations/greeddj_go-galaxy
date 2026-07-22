package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Config holds runtime settings for collection operations.
type Config struct {
	AnsibleConfigPath string
	RequirementsFile  string
	LockFile          string
	MetricsFile       string
	CacheDir          string
	DownloadPath      string
	Server            string
	Resolution        string
	// Warnings collects non-fatal configuration warnings (e.g. a
	// colon-separated collections_path with entries this tool ignores).
	// BuildCollectionConfig runs before the output printer exists, so
	// warnings are carried here and drained later through Infra.WarnConfig.
	Warnings                   []string
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
	if err := applyTimeout(cfg, c); err != nil {
		return nil, err
	}

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

// applyTimeout sets cfg.Timeout by parsing the --timeout flag with
// parseTimeout. Commands that do not register the flag (e.g. cleanup) read
// it as an empty string, which parseTimeout maps to the default, so they
// never end up with an unbounded-timeout HTTP client.
func applyTimeout(cfg *Config, c *cli.Command) error {
	timeout, err := parseTimeout(c.String("timeout"))
	if err != nil {
		return err
	}
	cfg.Timeout = timeout
	return nil
}

// parseTimeout parses a --timeout value in either of the two forms ansible
// and go-galaxy both need to support: a bare integer number of seconds
// (ansible's GALAXY_SERVER_TIMEOUT is an int, default 60) or a Go duration
// string (e.g. "90s", "1m30s"). An empty string yields the default timeout.
// Zero and negative values in either form are rejected as invalid, since a
// non-positive HTTP client timeout means "never time out".
func parseTimeout(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return helpers.FetchDefaultTimeout, nil
	}

	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("%w: %q", helpers.ErrInvalidTimeout, raw)
		}
		return time.Duration(n) * time.Second, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", helpers.ErrInvalidTimeout, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %q", helpers.ErrInvalidTimeout, raw)
	}
	return d, nil
}

// loadAnsibleConfigFromCLI resolves and loads ansible.cfg. An explicitly
// requested path (--ansible-config or GO_GALAXY_ANSIBLE_CONFIG) is treated
// strictly: a missing file is an error. Otherwise the path is discovered
// following ansible's own search order, where a missing candidate simply
// falls through to the next one rather than erroring.
func loadAnsibleConfigFromCLI(c *cli.Command) (ansibleConfig, string, error) {
	if c.IsSet("ansible-config") {
		path := c.String("ansible-config")
		cfg, loadedPath, err := loadAnsibleConfig(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ansibleConfig{}, "", fmt.Errorf("%w: %s", helpers.ErrAnsibleConfigNotFound, path)
			}
			return ansibleConfig{}, "", fmt.Errorf("failed to load ansible config: %w", err)
		}
		return cfg, loadedPath, nil
	}

	path := discoverAnsibleConfigPath()
	if path == "" {
		return ansibleConfig{}, "", nil
	}
	cfg, loadedPath, err := loadAnsibleConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The candidate existed during discovery but vanished before we
			// could open it (e.g. a concurrent process removed it); treat
			// this exactly like "no config found" rather than erroring.
			return ansibleConfig{}, "", nil
		}
		return ansibleConfig{}, "", fmt.Errorf("failed to load ansible config: %w", err)
	}
	return cfg, loadedPath, nil
}

// maxAnsibleConfigCandidates bounds the discovery candidate list: env var,
// cwd, home, and the system-wide path.
const maxAnsibleConfigCandidates = 4

// discoverAnsibleConfigPath returns the first existing candidate in
// ansible's documented config-file search order: $ANSIBLE_CONFIG, then
// ./ansible.cfg, then ~/.ansible.cfg, then /etc/ansible/ansible.cfg. It
// returns "" if none of the candidates exist.
func discoverAnsibleConfigPath() string {
	candidates := make([]string, 0, maxAnsibleConfigCandidates)
	if envPath := os.Getenv("ANSIBLE_CONFIG"); envPath != "" {
		candidates = append(candidates, envPath)
	}
	candidates = append(candidates, "ansible.cfg")
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".ansible.cfg"))
	}
	candidates = append(candidates, "/etc/ansible/ansible.cfg")

	for _, path := range candidates {
		if fileExists(path) {
			return path
		}
	}
	return ""
}

// fileExists reports whether path can be stat'd successfully. path is
// read-only here (existence check only, never opened or written), and it
// only ever comes from this file's own fixed candidate list (cwd, home,
// /etc/ansible) or from a user-supplied env var/flag value that is later
// opened the same way any explicit --ansible-config path already is.
func fileExists(path string) bool {
	// #nosec G703 -- path is the user's own ansible.cfg location (a fixed
	// candidate or a value they set via env/flag); this is a read-only
	// existence check that never opens or writes the file.
	_, err := os.Stat(path)
	return err == nil
}

func applyAnsibleConfig(cfg *Config, c *cli.Command, ansibleConfig ansibleConfig, ansiblePath string) {
	if ansiblePath != "" {
		cfg.AnsibleConfigPath = ansiblePath
	}
	cfg.DownloadPath, cfg.AnsibleCollectionsPathUsed = pickConfigValue(c, "download-path", ansibleConfig.Defaults.CollectionsPath)
	cfg.CacheDir, cfg.AnsibleCacheDirUsed = pickConfigValue(c, "cache-dir", ansibleConfig.Galaxy.CacheDir)
	cfg.Server, cfg.AnsibleServerUsed = pickConfigValue(c, "server", ansibleConfig.Galaxy.Server)

	// ansible accepts a POSIX ":"-separated list for collections_path (both
	// the [defaults] collections_path ansible.cfg key and the
	// ANSIBLE_COLLECTIONS_PATH env var), using only the first entry and
	// treating the rest as additional search roots this tool does not
	// support. Apply the split to the already-resolved value so both
	// sources are covered uniformly, matching ansible's own behavior for
	// an explicit --download-path a:b as well. first is assigned
	// unconditionally (a no-op when there was nothing to split, including a
	// bare trailing separator with no further entries); only a genuinely
	// ignored entry produces a warning.
	first, rest := firstCollectionsPath(cfg.DownloadPath)
	cfg.DownloadPath = first
	if len(rest) > 0 {
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("collections_path lists multiple paths; using %q and ignoring the rest: %v", first, rest))
	}
}

// firstCollectionsPath splits a POSIX ":"-separated collections_path value
// into its first entry and the remaining non-empty entries. It is not
// aware of Windows drive letters (e.g. "C:\path"), consistent with this
// tool's CI/Linux target.
func firstCollectionsPath(value string) (string, []string) {
	parts := strings.Split(value, ":")
	first := parts[0]

	var rest []string
	for _, p := range parts[1:] {
		if p != "" {
			rest = append(rest, p)
		}
	}
	return first, rest
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
Discovery order used by discoverAnsibleConfigPath when neither
--ansible-config nor GO_GALAXY_ANSIBLE_CONFIG is explicitly set, matching
ansible's own find_ini_config_file search (first existing file wins; a
missing $ANSIBLE_CONFIG target simply falls through to the next candidate):

  $ANSIBLE_CONFIG (environment variable, if set)
  ansible.cfg (in the current directory)
  ~/.ansible.cfg (in the home directory)
  /etc/ansible/ansible.cfg

[galaxy]
cache_dir // env:ANSIBLE_GALAXY_CACHE_DIR // default {{ ANSIBLE_HOME ~ "/galaxy_cache" }}
server // env:ANSIBLE_GALAXY_SERVER // default https://galaxy.ansible.com
*/

// loadAnsibleConfig loads and parses ansible.cfg if it exists.
func loadAnsibleConfig(configPath string) (ansibleConfig, string, error) {
	config := ansibleConfig{}

	f, err := os.Open(configPath) // #nosec G304 -- user-provided path
	if err != nil {
		return config, "", err
	}
	defer func() { _ = f.Close() }()

	config, err = parseAnsibleConfig(f)
	if err != nil {
		return config, "", fmt.Errorf("failed to parse ansible.cfg: %w", err)
	}
	return config, configPath, nil
}
