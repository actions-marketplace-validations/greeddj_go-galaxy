// Package config resolves one *Config per run from the three places a setting
// can come from: the parsed CLI command, the environment, and an ansible.cfg.
// BuildCollectionConfig is the entry point and the only place that precedence
// is decided; discoverAnsibleConfigPath is where an ansible.cfg candidate is
// accepted or refused, and resolveServers is where the Galaxy server list and
// each server's credential and TLS policy are settled.
//
// Every credential this package produces is a Secret, which renders a fixed
// redacted placeholder on each serialization path and yields its plaintext
// only through Reveal. This package makes no network request and opens no
// cache backend, and it runs before an output printer exists - so a non-fatal
// problem is queued on Config.Warnings and drained later rather than printed
// from here.
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
	// Server is the derived "first effective server" URL, kept for the
	// consumers that only ever deal with one Galaxy server: metrics,
	// GALAXY.yml, the lockfile's Server field, and Meta.Server. It always
	// equals Servers[0].URL once resolveServers has run. A consumer that
	// needs to be multi-server-aware (credential/TLS dispatch per origin)
	// must read Servers instead.
	Server string
	// Warnings collects non-fatal configuration warnings (e.g. a
	// colon-separated collections_path with entries this tool ignores, or
	// a disabled-TLS-verification server). BuildCollectionConfig runs
	// before the output printer exists, so warnings are carried here and
	// drained later through Infra.WarnConfig.
	Warnings []string
	// Servers is the resolved, non-empty list of configured Galaxy
	// servers, in the precedence and list order documented on
	// resolveServers. When no server_list is configured it holds exactly
	// one entry with ID "" (the implicit single server), the URL resolved
	// by the same precedence this tool always used for Server, and no
	// token - the shape every release before multi-server support existed
	// effectively had.
	Servers []Server
	S3Cache S3CacheConfig
	Timeout time.Duration
	Workers int
	// DownloadWorkers bounds the artifact-download and cache-presence-probe
	// pool the prefetcher runs, separately from Workers: that pool only waits
	// on the network, never extracts a tree, so its useful size is not the
	// install/warm worker pool's own. See helpers.DefaultDownloadWorkers and
	// helpers.DefaultInstallWorkers for the two default derivations.
	DownloadWorkers            int
	Refresh                    bool
	NoCache                    bool
	NoDeps                     bool
	DryRun                     bool
	Verbose                    bool
	Quiet                      bool
	ClearCache                 bool
	Offline                    bool
	Frozen                     bool
	AnsibleCollectionsPathUsed bool
	AnsibleCacheDirUsed        bool
	AnsibleServerUsed          bool
	// AnsibleServerEnvUsed narrows AnsibleServerUsed: the ansible-side server
	// value was taken, and it came from ANSIBLE_GALAXY_SERVER rather than from
	// the ansible.cfg file, so debug output does not credit a file that did
	// not supply it.
	AnsibleServerEnvUsed bool
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

// BuildCollectionConfig builds Config from CLI flags and ansible.cfg.
//
// Contract: it reads the full union of flags a command may register for
// this path - the collection flags (server, timeout, download-path,
// requirements-file, ansible-config, workers, download-workers, no-cache,
// refresh, clear-cache, no-deps, offline, resolution, lock-file, frozen,
// metrics-file), the S3 cache flags (s3-bucket and friends), and the
// global flags (verbose, quiet, dry-run, cache-dir). A command is not
// required to register every one of them - cleanup, for example, registers
// only the S3 flags plus the globals, since it drives its work from the
// per-project registry and consumes just CacheDir, DryRun, and S3Cache
// from the result. A flag a command does not register simply resolves to
// its Go zero value here (c.String/c.Bool/c.Int return the zero value for
// an unknown flag name); that is safe only as long as the command does not
// read the matching Config field. Any command wired into this path in the
// future must register every flag whose Config field it reads, or it will
// silently observe a zero value instead of an error.
func BuildCollectionConfig(c *cli.Command) (*Config, error) {
	cfg := newConfigFromCLI(c)
	if err := applyTimeout(cfg, c); err != nil {
		return nil, err
	}
	if err := applyWorkers(c); err != nil {
		return nil, err
	}

	ansibleConfig, ansiblePath, ansibleWarnings, err := loadAnsibleConfigFromCLI(c)
	if err != nil {
		return nil, err
	}
	// Appended before applyAnsibleConfig runs, so the warning order follows
	// the order the events happened in: what discovery declined to read comes
	// ahead of what applying the file it did read had to say.
	cfg.Warnings = append(cfg.Warnings, ansibleWarnings...)
	applyAnsibleConfig(cfg, c, ansibleConfig, ansiblePath)

	if err := resolveServers(cfg, c, ansibleConfig); err != nil {
		return nil, err
	}

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
		DownloadWorkers:  c.Int("download-workers"),
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
		DownloadPath:     c.String("download-path"),
	}

	// Two shapes reach this, and only one of them survives. A command that
	// does not register --workers (cleanup) reads the zero value of an
	// unknown flag name and genuinely needs the fallback. A registering
	// command whose source supplied a non-positive value reaches it too,
	// since this runs before applyWorkers - but that config is discarded, so
	// the fallback never reaches a caller for such a value. A registering
	// command with no source filling the flag does not reach this at all: it
	// reads the flag's own Value, which is this same derivation.
	if cfg.Workers < 1 {
		cfg.Workers = helpers.DefaultInstallWorkers(runtime.GOMAXPROCS(0))
	}
	// Same fallback shape as Workers above, and for the identical two
	// reasons: a command that does not register --download-workers (cleanup)
	// reads the zero value of an unknown flag name, and a registering command
	// whose source supplied a non-positive value reaches it too. Unlike
	// Workers, no companion applyWorkers-style function rejects an explicit
	// non-positive value for this flag - it is silently replaced by the
	// default instead, since download concurrency is a performance knob, not
	// one whose zero value would otherwise mean "unbounded" the way a
	// non-positive worker count could be misread.
	if cfg.DownloadWorkers < 1 {
		cfg.DownloadWorkers = helpers.DefaultDownloadWorkers(runtime.GOMAXPROCS(0))
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

// applyWorkers refuses a --workers value some source actually supplied and
// that is not a positive integer. It writes nothing: the value newConfigFromCLI
// already read is either accepted as-is or the whole config load fails, so this
// takes no *Config at all.
//
// The predicate is "present and non-positive", which two facts make correct.
// A command that does not register --workers (cleanup) reads IsSet == false
// and is skipped entirely, keeping the helpers.DefaultInstallWorkers fallback
// newConfigFromCLI applies to its zero value. A command that does register the
// flag, with no source filling it, reads that same default from the flag's own
// Value rather than 0 (see collectionBehaviorFlags in
// cmd/go-galaxy/cliflags/flags.go), so n < 1 here is reachable only for a
// value some source genuinely supplied.
//
// The message names both the flag and the environment variable because
// c.IsSet cannot tell argv from env: an operator whose CI block exports
// GO_GALAXY_WORKERS=0 would otherwise be pointed at a flag they never typed,
// and urfave prints nothing of its own for an env-sourced value.
func applyWorkers(c *cli.Command) error {
	if !c.IsSet("workers") {
		return nil
	}
	if n := c.Int("workers"); n < 1 {
		return fmt.Errorf("%w: --workers (or $GO_GALAXY_WORKERS) = %d, want a positive integer",
			helpers.ErrInvalidWorkers, n)
	}
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
//
// The third return value carries the warnings discovery produced. The
// explicit-path branch returns none by construction: a path the operator
// named passes through no heuristic and must not acquire one.
func loadAnsibleConfigFromCLI(c *cli.Command) (ansibleConfig, string, []string, error) {
	if c.IsSet("ansible-config") {
		path := c.String("ansible-config")
		cfg, loadedPath, err := loadAnsibleConfig(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ansibleConfig{}, "", nil, fmt.Errorf("%w: %s", helpers.ErrAnsibleConfigNotFound, path)
			}
			return ansibleConfig{}, "", nil, fmt.Errorf("failed to load ansible config: %w", err)
		}
		return cfg, loadedPath, nil, nil
	}

	path, warnings := discoverAnsibleConfigPath()
	if path == "" {
		return ansibleConfig{}, "", warnings, nil
	}
	cfg, loadedPath, err := loadAnsibleConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The candidate existed during discovery but vanished before we
			// could open it (e.g. a concurrent process removed it); treat
			// this exactly like "no config found" rather than erroring.
			return ansibleConfig{}, "", warnings, nil
		}
		return ansibleConfig{}, "", warnings, fmt.Errorf("failed to load ansible config: %w", err)
	}
	return cfg, loadedPath, warnings, nil
}

// maxAnsibleConfigCandidates bounds the discovery candidate list: env var,
// cwd, home, and the system-wide path.
const maxAnsibleConfigCandidates = 4

// cwdAnsibleCfgName is the current-directory candidate, kept relative
// exactly as ansible's own discovery keeps it: it resolves against the
// process's working directory at open time rather than at discovery time.
const cwdAnsibleCfgName = "ansible.cfg"

// worldWritablePerm is the permission bit that makes a directory writable by
// any principal on the machine. A directory carrying it cannot be trusted to
// hold a configuration file this process obeys.
const worldWritablePerm = 0o002

// discoverAnsibleConfigPath returns the first existing candidate in
// ansible's documented config-file search order: $ANSIBLE_CONFIG, then
// ./ansible.cfg, then ~/.ansible.cfg, then /etc/ansible/ansible.cfg. It
// returns "" if none of the candidates exist, alongside any warnings the
// discovery itself produced.
//
// The order carries one exception, the same one ansible's own
// find_ini_config_file makes: ./ansible.cfg is not a candidate at all when
// the current directory is world-writable, because any other principal on
// the machine can put a file there. Discovery then continues with the
// remaining candidates rather than stopping.
func discoverAnsibleConfigPath() (string, []string) {
	var warnings []string
	candidates := make([]string, 0, maxAnsibleConfigCandidates)
	if envPath := os.Getenv("ANSIBLE_CONFIG"); envPath != "" {
		candidates = append(candidates, envPath)
	}
	cwdPath, cwdWarning := cwdCandidate()
	if cwdPath != "" {
		candidates = append(candidates, cwdPath)
	}
	if cwdWarning != "" {
		warnings = append(warnings, cwdWarning)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".ansible.cfg"))
	}
	candidates = append(candidates, "/etc/ansible/ansible.cfg")

	for _, path := range candidates {
		if fileExists(path) {
			return path, warnings
		}
	}
	return "", warnings
}

// cwdCandidate returns the current-directory ansible.cfg candidate, or ""
// plus a warning when the current directory is world-writable.
//
// What such a directory costs is concrete: a config this process obeys sets
// collections_path (the root the install tree is written under), cache_dir
// (the root internal/galaxy/extracted removes entries beneath), and the
// server list a run fetches from. On a shared CI runner, letting any other
// principal choose those by dropping a file in a scratch directory is the
// vector ansible's own exception exists for.
//
// Two decisions here are deliberate. The candidate is kept when Getwd or
// Stat fails: not being able to learn a directory's mode is a reason to
// behave as before, not to silently drop a configuration source an ordinary
// environment depends on. And the warning is emitted whether or not an
// ansible.cfg is actually sitting there, since checking first would keep
// quiet in exactly the moment before the file is planted.
//
// The sticky bit is deliberately not an exemption, which is also what
// ansible does. Sticky stops another principal from replacing or removing a
// file that already exists; it does nothing to stop one from creating
// ansible.cfg where none exists yet, which is this vector's main shape. A
// world-writable /tmp is therefore skipped like any other world-writable
// directory.
func cwdCandidate() (string, string) {
	cwd, err := os.Getwd()
	if err != nil {
		return cwdAnsibleCfgName, ""
	}
	// #nosec G703 -- cwd is this process's own working directory; this is a
	// read-only mode check that never opens or writes anything.
	info, err := os.Stat(cwd)
	if err != nil {
		return cwdAnsibleCfgName, ""
	}
	if info.Mode().Perm()&worldWritablePerm == 0 {
		return cwdAnsibleCfgName, ""
	}
	return "", fmt.Sprintf(
		"the current directory %q is world-writable; ignoring ./ansible.cfg as a configuration source",
		cwd,
	)
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
	serverValue, serverFromEnv := ansibleGalaxyServer(ansibleConfig.Galaxy.Server)
	cfg.Server, cfg.AnsibleServerUsed = pickConfigValue(c, "server", serverValue)
	cfg.AnsibleServerEnvUsed = cfg.AnsibleServerUsed && serverFromEnv

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
// ansibleGalaxyServer resolves the ansible-side galaxy server value: the
// ANSIBLE_GALAXY_SERVER env var when it is set at all, otherwise the
// [galaxy] server key from ansible.cfg. It reports whether the value came
// from the environment.
//
// The env var wins outright over the ini key whenever set, which is the rule
// resolveServerList already applies to ANSIBLE_GALAXY_SERVER_LIST. What it
// does NOT do is outrank server_list or --server, which is why this is read
// here rather than declared as a flag source: ansible treats the variable as
// the env spelling of the [galaxy] server config, so it belongs to that slot
// in the precedence chain (see resolveServers) and nowhere earlier.
//
// An empty env value resolves to the empty string, which pickConfigValue
// treats as absent, so it falls through to the flag default rather than
// naming a server with no URL.
func ansibleGalaxyServer(ini string) (string, bool) {
	if v, ok := os.LookupEnv("ANSIBLE_GALAXY_SERVER"); ok {
		return v, true
	}
	return ini, false
}

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

The cwd candidate is dropped, with a warning, when the current directory is
world-writable - the same exception ansible makes, for the same reason - and
discovery continues with the remaining candidates. See cwdCandidate.

[galaxy]
cache_dir // env:ANSIBLE_GALAXY_CACHE_DIR // default {{ ANSIBLE_HOME ~ "/galaxy_cache" }}
server // env:ANSIBLE_GALAXY_SERVER // default https://galaxy.ansible.com
server_list // env:ANSIBLE_GALAXY_SERVER_LIST // comma-separated ids, each resolved
            // against its own [galaxy_server.<id>] section; see servers.go.

[galaxy_server.<id>]
url // env:ANSIBLE_GALAXY_SERVER_<ID>_URL
token // env:ANSIBLE_GALAXY_SERVER_<ID>_TOKEN
validate_certs // env:ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS
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
