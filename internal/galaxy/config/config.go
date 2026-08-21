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
	// AnsibleSignatureKeys names the signature keys the discovered ansible.cfg
	// carried, and never a value any of them was set to - see
	// ansibleGalaxyConfig.SignatureKeys for why the names alone are recorded.
	// It is read by the one thing that can act on it, the warning a verifying
	// command emits once per run through AnsibleSignatureKeysWarning.
	AnsibleSignatureKeys []string
	// Servers is the resolved, non-empty list of configured Galaxy
	// servers, in the precedence and list order documented on
	// resolveServers. When no server_list is configured it holds exactly
	// one entry with ID "" (the implicit single server), the URL resolved
	// by the same precedence this tool always used for Server, and no
	// token - the shape every release before multi-server support existed
	// effectively had.
	Servers []Server
	// GitCredentials is the operator's host-bound git credentials from the
	// GO_GALAXY_GIT_* environment surface, in GO_GALAXY_GIT_CREDENTIALS list
	// order; nil when none is declared. See loadGitCredentials for the
	// grammar and the rules each entry has already passed.
	GitCredentials []GitCredential
	S3Cache        S3CacheConfig
	// Signature is this run's resolved signature verification surface: the
	// keyring location, how many signatures must verify, which failure
	// statuses are tolerated, and whether verification is switched off.
	Signature SignatureConfig
	Timeout   time.Duration
	Workers   int
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
// metrics-file), the signature flags (keyring,
// required-valid-signature-count, ignore-signature-status-code,
// disable-gpg-verify), the S3 cache flags (s3-bucket and friends), and the
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
//
// The signature flags are the one group whose zero value is not merely unread
// but unusable: an empty required-valid-signature-count is not a spec any
// grammar accepts, so applySignatureConfig falls back to
// helpers.DefaultRequiredValidSignatureCount rather than validating "". That
// fallback is what lets a command registering none of these flags build a
// config at all, since this function validates the count for every command
// regardless of which flags that command declared.
func BuildCollectionConfig(c *cli.Command) (*Config, error) {
	cfg := newConfigFromCLI(c)
	if err := applyTimeout(cfg, c); err != nil {
		return nil, err
	}
	applyWorkers(cfg, c, runtime.GOMAXPROCS(0))

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

	// After the servers and before the S3 cache: a git credential is part of
	// the same "where may a secret go" surface as a server token, and an
	// error there keeps the precedence a broken server configuration already
	// has over a broken cache one.
	if err := loadGitCredentials(cfg); err != nil {
		return nil, err
	}

	s3Cfg, err := loadS3CacheConfig(c)
	if err != nil {
		return nil, err
	}
	cfg.S3Cache = s3Cfg

	// Last, deliberately: every config error above keeps the precedence it
	// already had, so which failure a broken configuration reports first does
	// not change because a signature surface was added behind it.
	if err := applySignatureConfig(cfg, c); err != nil {
		return nil, err
	}

	return cfg, nil
}

func newConfigFromCLI(c *cli.Command) *Config {
	cfg := &Config{
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

	// Two shapes reach this fallback. A command that does not register
	// --download-workers (cleanup) reads the zero value of an unknown flag
	// name, and a registering command whose source supplied a non-positive
	// value reaches it too; a registering command with no source filling the
	// flag does not reach it at all, since it reads the flag's own Value,
	// which is this same derivation. Both shapes are replaced silently and
	// against no ceiling, which is where this parts company with applyWorkers:
	// that one enforces an upper bound as well, and warns whenever the value
	// it replaces came from a source rather than from an unregistered flag
	// name, because an install worker competes for the CPU it extracts on.
	// Download concurrency is a performance knob that waits on the network, so
	// a value above the permitted CPU is a legitimate configuration here rather
	// than an oversubscription, and a non-positive one names no runnable pool
	// rather than an unbounded one.
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

// applyWorkers is the single owner of cfg.Workers: nothing else on the
// config-load path writes that field, so every shape --workers can arrive in
// is settled in one place. A Config built programmatically rather than loaded
// - a test's own struct literal - is outside that path and carries whatever it
// was given.
//
// A value some source supplied is accepted only inside
// 1..helpers.MaxAcceptedInstallWorkers(procs). Outside it, in EITHER
// direction, the operator's value is replaced by
// helpers.DefaultInstallWorkers(procs) and one warning is queued - the same
// outcome for 0, for -1 and for a count far above what the machine can run,
// since all three name a pool this run will not start and none of them is a
// reason to fail a CI job that would otherwise install correctly. The ceiling
// is floored at helpers.MinDefaultInstallWorkers so the substitute is always
// itself inside the range being enforced; see MaxAcceptedInstallWorkers for
// why that floor is what makes the two consistent on a single permitted CPU.
//
// c.IsSet gates the substitution because the two branches answer different
// questions. Two shapes read IsSet == false, and neither may be warned about,
// since nobody supplied either: a command that does not register --workers
// (cleanup) reads the Go zero value of an unknown flag name, which that branch
// replaces with the derived default, and a registering command whose sources
// filled nothing reads the flag's own Value, which is already that same
// derivation and is kept as it stands. A command that does register the flag
// reaches the IsSet branch only for a value some source genuinely supplied, so
// a warning there always names something an operator wrote.
//
// The message names both the flag and the environment variable because
// c.IsSet cannot tell argv from env: an operator whose CI block exports
// GO_GALAXY_WORKERS=0 would otherwise be pointed at a flag they never typed,
// and urfave prints nothing of its own for an env-sourced value.
//
// A declared-but-empty GO_GALAXY_WORKERS= is IsSet too - urfave marks the
// flag set and skips the parse for it - so that shape is judged by this
// range rather than skipped by the gate. It passes because the flag's own
// Value is what c.Int then reads, and that Value is
// helpers.DefaultInstallWorkers(procs) against a ceiling of
// helpers.MaxAcceptedInstallWorkers(procs) for the same procs, which is
// arithmetic rather than a coincidence of one machine's CPU count: see
// collectionBehaviorFlags in cmd/go-galaxy/cliflags/flags.go, which holds the
// proof on the field it rests on.
//
// Documented-uncovered: what no test pins is the argument
// BuildCollectionConfig passes for procs - runtime.GOMAXPROCS(0) rather than
// runtime.NumCPU(). procs is a parameter precisely so the range logic can be
// driven at any CPU count, so the logic is covered and the call site's choice
// of argument is not. The divergence this call site's argument exists for is a
// CFS bandwidth quota narrowing one and not the other, which no fixture can
// arrange. A test could instead narrow GOMAXPROCS itself, and that is rejected
// rather than merely unwritten: it is a process-global mutation racing every
// parallel test in the package, and it proves nothing on a runner permitting
// two CPUs or fewer, where the ceiling's own floor makes both answers the same
// number. What a regression to NumCPU() would cost is measured on
// helpers.DefaultInstallWorkers' own doc comment: a pod limited to 2 CPUs on a
// 64-core node would size both answers from 64 rather than from 2, accepting
// an operator's own 64 through helpers.MaxAcceptedInstallWorkers and deriving
// 16, the cap helpers.MaxDefaultInstallWorkers imposes - where the right
// answer to both is 2.
func applyWorkers(cfg *Config, c *cli.Command, procs int) {
	fallback := helpers.DefaultInstallWorkers(procs)
	n := c.Int("workers")
	if !c.IsSet("workers") {
		// The cleanup shape: an unregistered flag name reads 0, which is the
		// absence of a value rather than a value to warn about.
		if n < 1 {
			n = fallback
		}
		cfg.Workers = n
		return
	}

	upper := helpers.MaxAcceptedInstallWorkers(procs)
	if n < 1 || n > upper {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"--workers (or $GO_GALAXY_WORKERS) = %d is outside 1..%d, the range this machine "+
				"accepts (the ceiling is the CPU this process is permitted to use, at least %d); "+
				"using %d instead",
			n, upper, helpers.MinDefaultInstallWorkers, fallback))
		n = fallback
	}
	cfg.Workers = n
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
// The warning names the CANDIDATE it dropped rather than the file, and that
// distinction is load-bearing rather than pedantic. This check is scoped to
// the ./ansible.cfg candidate alone, because it lives in the function that
// produces that one candidate; every other candidate discoverAnsibleConfigPath
// assembles is appended untouched. So a relative $ANSIBLE_CONFIG=ansible.cfg
// resolves against this same working directory and loads the very file whose
// candidate was dropped. Where the env candidate sits in the list has nothing
// to do with it: it would escape this check appended first, last, or not at
// all.
//
// That the exemption is intended rather than an oversight is grounded in this
// repository rather than inferred: docs/configuration.md instructs an operator
// whose workspace is world-writable to name the file through --ansible-config
// or $ANSIBLE_CONFIG, which is that path being prescribed rather than merely
// tolerated. Whether ansible's own find_ini_config_file scopes its check the
// same way is a parity question this file does not answer - the paragraphs
// here claim parity for the exception itself, not for its edges.
//
// What would be wrong, then, is not the behavior but a warning asserting the
// file is ignored in a run that in fact loaded it. The text states what was
// dropped, that discovery continues, and that an explicitly named path still
// reaches the file - which the operator who wants the strict reading needs in
// order to close that door, by unsetting the variable or tightening the mode,
// as much as the one who wants to use it.
//
// Disclosed residual: the message states the rule, never the outcome, because
// it is composed here - before discoverAnsibleConfigPath's candidate loop has
// run, and so before anything is known about which candidate wins. An
// operator whose $ANSIBLE_CONFIG arrived ambiently from a CI image therefore
// reads a conditional clause and is never told the planted file was in fact
// loaded; the winning path surfaces only through Infra.DebugAnsibleConfig, at
// debug level. Making the message outcome-aware would move its ownership to
// discoverAnsibleConfigPath, which is a wider change than repairing a text
// that lied.
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
		"the current directory %q is world-writable; dropping ./ansible.cfg from the discovery search path - "+
			"discovery continues with its remaining candidates, and a path named explicitly by $ANSIBLE_CONFIG, "+
			"--ansible-config or $GO_GALAXY_ANSIBLE_CONFIG is still read, even one resolving to that same file",
		cwd,
	)
}

// fileExists reports whether path can be stat'd successfully. path is
// read-only here (existence check only, never opened or written), and it
// only ever comes from this file's own fixed candidate list (cwd, home,
// /etc/ansible) or from $ANSIBLE_CONFIG, which is later opened the same way
// any explicit --ansible-config path already is. A flag value never reaches
// here at all: --ansible-config and $GO_GALAXY_ANSIBLE_CONFIG are handled by
// loadAnsibleConfigFromCLI's own branch, which returns before discovery runs.
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
	cfg.AnsibleSignatureKeys = ansibleConfig.Galaxy.SignatureKeys
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
