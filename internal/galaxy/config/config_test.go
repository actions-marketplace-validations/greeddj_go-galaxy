package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Defaults used for the CLI flags built in newApplyAnsibleConfigCmd. They
// only need to be distinguishable from the ansible.cfg-sourced values used
// in the test cases below.
const (
	testDefaultDownloadPath = "/default/collections"
	testDefaultCacheDir     = "/default/cache"
	testDefaultServer       = "https://default.example"
	testAnsibleConfigPath   = "path"
)

// newApplyAnsibleConfigCmd builds a minimal *cli.Command exposing only the
// three flags applyAnsibleConfig reads (download-path, cache-dir, server),
// runs it with args, and returns the *cli.Command captured from inside the
// action so applyAnsibleConfig can be driven directly against it. Flags are
// built inline (mirroring cmd/go-galaxy/cliflags/flags_test.go's pattern)
// rather than importing the cliflags package, to avoid an import cycle.
func newApplyAnsibleConfigCmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "download-path", Value: testDefaultDownloadPath},
			&cli.StringFlag{Name: "cache-dir", Value: testDefaultCacheDir},
			&cli.StringFlag{Name: "server", Value: testDefaultServer},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// applyAnsibleConfigWant is the expected shape of *Config after
// applyAnsibleConfig runs: the three mapped fields, their "came from
// ansible.cfg" flags, and the recorded ansible.cfg path.
type applyAnsibleConfigWant struct {
	downloadPath      string
	cacheDir          string
	server            string
	ansibleConfigPath string
	collectionsUsed   bool
	cacheDirUsed      bool
	serverUsed        bool
}

// runApplyAnsibleConfig drives applyAnsibleConfig with the given CLI args
// and ansible.cfg values, returning the resulting *Config for assertion.
func runApplyAnsibleConfig(t *testing.T, args []string, ansCfg ansibleConfig) *Config {
	t.Helper()
	c := newApplyAnsibleConfigCmd(t, args)
	cfg := &Config{}
	applyAnsibleConfig(cfg, c, ansCfg, testAnsibleConfigPath)
	return cfg
}

// assertApplyAnsibleConfig checks got against want field by field, so a
// mismatch on any one mapping is reported without masking the others.
func assertApplyAnsibleConfig(t *testing.T, got *Config, want applyAnsibleConfigWant) {
	t.Helper()
	if got.DownloadPath != want.downloadPath {
		t.Errorf("DownloadPath = %q, want %q", got.DownloadPath, want.downloadPath)
	}
	if got.AnsibleCollectionsPathUsed != want.collectionsUsed {
		t.Errorf("AnsibleCollectionsPathUsed = %v, want %v", got.AnsibleCollectionsPathUsed, want.collectionsUsed)
	}
	if got.CacheDir != want.cacheDir {
		t.Errorf("CacheDir = %q, want %q", got.CacheDir, want.cacheDir)
	}
	if got.AnsibleCacheDirUsed != want.cacheDirUsed {
		t.Errorf("AnsibleCacheDirUsed = %v, want %v", got.AnsibleCacheDirUsed, want.cacheDirUsed)
	}
	if got.Server != want.server {
		t.Errorf("Server = %q, want %q", got.Server, want.server)
	}
	if got.AnsibleServerUsed != want.serverUsed {
		t.Errorf("AnsibleServerUsed = %v, want %v", got.AnsibleServerUsed, want.serverUsed)
	}
	if got.AnsibleConfigPath != want.ansibleConfigPath {
		t.Errorf("AnsibleConfigPath = %q, want %q", got.AnsibleConfigPath, want.ansibleConfigPath)
	}
}

// TestApplyAnsibleConfigDownloadPath checks the collections_path ->
// DownloadPath mapping: an ansible.cfg value is used only when
// --download-path was not explicitly set, an explicit flag always wins,
// and an empty ansible.cfg value falls back to the flag default. It also
// checks that AnsibleConfigPath is set from the ansiblePath argument.
func TestApplyAnsibleConfigDownloadPath(t *testing.T) {
	t.Run("flag unset, ansible.cfg value present", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "/ansible/collections"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: "/ansible/collections", collectionsUsed: true,
			cacheDir: testDefaultCacheDir, server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag set explicitly", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "/ansible/collections"}}
		got := runApplyAnsibleConfig(t, []string{"--download-path=/explicit/collections"}, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: "/explicit/collections",
			cacheDir:     testDefaultCacheDir, server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag unset, ansible.cfg empty", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath,
			cacheDir:     testDefaultCacheDir, server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})
}

// assertCollectionsPathSplit checks got's DownloadPath and Warnings count
// against want, for TestCollectionsPathSplit's cases.
func assertCollectionsPathSplit(t *testing.T, got *Config, wantDownloadPath string, wantWarnings int) {
	t.Helper()
	if got.DownloadPath != wantDownloadPath {
		t.Errorf("DownloadPath = %q, want %q", got.DownloadPath, wantDownloadPath)
	}
	if len(got.Warnings) != wantWarnings {
		t.Errorf("Warnings = %v, want %d entries", got.Warnings, wantWarnings)
	}
}

// assertWarningMentions checks that got's sole warning mentions substr (the
// ignored collections_path entries).
func assertWarningMentions(t *testing.T, got *Config, substr string) {
	t.Helper()
	if len(got.Warnings) != 1 {
		t.Fatalf("len(Warnings) = %d, want 1 (Warnings = %v)", len(got.Warnings), got.Warnings)
	}
	if !strings.Contains(got.Warnings[0], substr) {
		t.Errorf("Warnings[0] = %q, want it to mention %q", got.Warnings[0], substr)
	}
}

// TestCollectionsPathSplit checks that applyAnsibleConfig honors ansible's
// POSIX ":"-separated collections_path list: only the first entry is used,
// and a warning naming the ignored entries is recorded exactly once. A
// single entry, a single entry with a trailing separator (an empty
// segment), and an empty value must all leave Warnings untouched.
func TestCollectionsPathSplit(t *testing.T) {
	t.Run("multiple entries: first wins, rest warned about", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "a:b:c"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertCollectionsPathSplit(t, got, "a", 1)
		assertWarningMentions(t, got, "[b c]")
	})

	t.Run("single entry: no split, no warning", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "a"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertCollectionsPathSplit(t, got, "a", 0)
	})

	t.Run("trailing separator: empty segment filtered, no warning", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "a:"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertCollectionsPathSplit(t, got, "a", 0)
	})

	t.Run("empty ansible.cfg value: falls back to flag default, no warning", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		assertCollectionsPathSplit(t, got, testDefaultDownloadPath, 0)
	})

	t.Run("explicit flag value also splits, matching ansible", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, []string{"--download-path=a:b"}, ansibleConfig{})
		assertCollectionsPathSplit(t, got, "a", 1)
	})
}

// TestApplyAnsibleConfigCacheDir checks the cache_dir -> CacheDir mapping
// with the same three precedence scenarios as TestApplyAnsibleConfigDownloadPath.
func TestApplyAnsibleConfigCacheDir(t *testing.T) {
	t.Run("flag unset, ansible.cfg value present", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{CacheDir: "/ansible/cache"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: "/ansible/cache", cacheDirUsed: true,
			server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag set explicitly", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{CacheDir: "/ansible/cache"}}
		got := runApplyAnsibleConfig(t, []string{"--cache-dir=/explicit/cache"}, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: "/explicit/cache",
			server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag unset, ansible.cfg empty", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: testDefaultCacheDir,
			server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})
}

// TestParseTimeout checks parseTimeout accepts both ansible's bare-integer
// seconds form (GALAXY_SERVER_TIMEOUT is an int) and Go duration strings,
// falls back to the default on an empty value, and rejects non-positive or
// unparsable input as helpers.ErrInvalidTimeout.
func TestParseTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "bare seconds", raw: "60", want: 60 * time.Second},
		{name: "bare seconds, other value", raw: "90", want: 90 * time.Second},
		{name: "go duration, minutes and seconds", raw: "1m30s", want: 90 * time.Second},
		{name: "go duration, seconds", raw: "45s", want: 45 * time.Second},
		{name: "empty falls back to default", raw: "", want: helpers.FetchDefaultTimeout},
		{name: "zero seconds is invalid", raw: "0", wantErr: true},
		{name: "negative seconds is invalid", raw: "-5", wantErr: true},
		{name: "not a number or duration is invalid", raw: "abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTimeout(tt.raw)
			if tt.wantErr {
				if !errors.Is(err, helpers.ErrInvalidTimeout) {
					t.Errorf("parseTimeout(%q) error = %v, want helpers.ErrInvalidTimeout", tt.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTimeout(%q) error = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("parseTimeout(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// newTimeoutCmd builds a *cli.Command for TestApplyTimeout. When
// registerFlag is true it exposes a --timeout StringFlag (mirroring how
// CollectionFlags registers it, with the same env sources and default
// helpers.FetchDefaultTimeout.String()); when false no such flag exists at
// all, mirroring commands like cleanup that never register --timeout, so
// c.String("timeout") reads as an empty string.
func newTimeoutCmd(t *testing.T, registerFlag bool, args []string) *cli.Command {
	t.Helper()

	var flags []cli.Flag
	if registerFlag {
		flags = []cli.Flag{&cli.StringFlag{
			Name:    "timeout",
			Value:   helpers.FetchDefaultTimeout.String(),
			Sources: cli.EnvVars("GO_GALAXY_SERVER_TIMEOUT", "GO_GALAXY_TIMEOUT", "ANSIBLE_GALAXY_SERVER_TIMEOUT"),
		}}
	}

	var captured *cli.Command
	cmd := &cli.Command{
		Name:  "go-galaxy",
		Flags: flags,
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// TestApplyTimeout checks that applyTimeout wires parseTimeout's result
// into cfg.Timeout: an explicit --timeout value (in Go duration form) is
// honored as-is, an unset flag falls back to the default, a command that
// never registers --timeout at all (e.g. cleanup) also falls back to the
// default, and a value supplied via ANSIBLE_GALAXY_SERVER_TIMEOUT (ansible's
// own env var) is picked up the same way an explicit flag would be.
func TestApplyTimeout(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		want         time.Duration
		registerFlag bool
	}{
		{
			name:         "small positive timeout is honored",
			registerFlag: true,
			args:         []string{"--timeout=5s"},
			want:         5 * time.Second,
		},
		{
			name:         "larger timeout passes through unchanged",
			registerFlag: true,
			args:         []string{"--timeout=45s"},
			want:         45 * time.Second,
		},
		{
			name:         "unset flag falls back to default",
			registerFlag: true,
			want:         helpers.FetchDefaultTimeout,
		},
		{
			name: "unregistered flag falls back to default",
			want: helpers.FetchDefaultTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTimeoutCmd(t, tt.registerFlag, tt.args)
			cfg := &Config{}
			if err := applyTimeout(cfg, c); err != nil {
				t.Fatalf("applyTimeout() error = %v, want nil", err)
			}
			if cfg.Timeout != tt.want {
				t.Errorf("Timeout = %v, want %v", cfg.Timeout, tt.want)
			}
		})
	}

	// t.Setenv forbids t.Parallel, so the env-driven case is its own
	// non-parallel subtest rather than part of the table above.
	t.Run("env var in ansible's bare-seconds form is honored", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_TIMEOUT", "60")
		c := newTimeoutCmd(t, true, nil)
		cfg := &Config{}
		if err := applyTimeout(cfg, c); err != nil {
			t.Fatalf("applyTimeout() error = %v, want nil", err)
		}
		if cfg.Timeout != 60*time.Second {
			t.Errorf("Timeout = %v, want %v", cfg.Timeout, 60*time.Second)
		}
	})
}

// newIntFlagCmd builds a *cli.Command exposing a single IntFlag under
// flagName (sourced from envName, with the given defaultValue) when
// registerFlag is true, mirroring how collectionBehaviorFlags registers
// --workers and --download-workers; when false no such flag exists at all,
// mirroring a command like cleanup that never registers either, so
// c.IsSet(flagName) reads false and c.Int(flagName) reads 0. Shared by
// TestApplyWorkers and TestDownloadWorkersDefault, since both need the
// identical minimal single-flag fixture shape against a different flag name
// and default.
//
// defaultValue is a hand-copy of whichever production flag's own Value the
// caller is mirroring, so it cannot pin that flag's Value on its own: that
// pin lives in TestWorkersEnvShapes (cmd/go-galaxy/commands), which drives
// the real cliflags.CollectionFlags(). It is copied anyway so the rows below
// see the shape production has - a declared-but-empty environment variable is
// marked set while its parse is skipped, so c.Int reads this Value for that
// shape, and a 0 here would look like a value some source supplied.
func newIntFlagCmd(t *testing.T, flagName, envName string, defaultValue int, registerFlag bool, args []string) *cli.Command {
	t.Helper()

	var flags []cli.Flag
	if registerFlag {
		flags = []cli.Flag{&cli.IntFlag{
			Name:    flagName,
			Value:   defaultValue,
			Sources: cli.EnvVars(envName),
		}}
	}

	var captured *cli.Command
	cmd := &cli.Command{
		Name:  "go-galaxy",
		Flags: flags,
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// workersRow is one shape a --workers value can arrive in: what the fixture
// registers and supplies, the worker count applyWorkers must settle on, and
// whether it must have queued a warning about the value it was handed.
type workersRow struct {
	name         string
	wantWarnHas  string
	args         []string
	procs        int
	wantWorkers  int
	registerFlag bool
	wantWarn     bool
}

// assertWorkersOutcome checks one row's settled worker count and the warning
// that must or must not sit beside it. It calls t.Helper, so every failure
// below is reported on the caller's line rather than on this function's own.
func assertWorkersOutcome(t *testing.T, cfg *Config, row workersRow) {
	t.Helper()

	if cfg.Workers != row.wantWorkers {
		t.Fatalf("cfg.Workers = %d, want %d", cfg.Workers, row.wantWorkers)
	}
	if !row.wantWarn {
		if len(cfg.Warnings) != 0 {
			t.Fatalf("len(cfg.Warnings) = %d, want 0", len(cfg.Warnings))
		}
		return
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("len(cfg.Warnings) = %d, want 1", len(cfg.Warnings))
	}
	if !strings.Contains(cfg.Warnings[0], row.wantWarnHas) {
		t.Fatalf("warning does not name the supplied value %q: %q", row.wantWarnHas, cfg.Warnings[0])
	}
	if !strings.Contains(cfg.Warnings[0], "--workers") {
		t.Fatalf("warning does not name the flag: %q", cfg.Warnings[0])
	}
}

// workersRows enumerates the shapes TestApplyWorkers drives applyWorkers
// with. It is a function of its own rather than a literal inside that test
// only to keep the test itself inside funlen's budget.
func workersRows() []workersRow {
	return []workersRow{
		{
			name:  "a value inside the range is honored",
			procs: 8, registerFlag: true, args: []string{"--workers=3"}, wantWorkers: 3,
		},
		{
			name:  "the lower bound is inclusive",
			procs: 8, registerFlag: true, args: []string{"--workers=1"}, wantWorkers: 1,
		},
		{
			name:  "the upper bound is inclusive",
			procs: 8, registerFlag: true, args: []string{"--workers=8"}, wantWorkers: 8,
		},
		{
			name:  "zero is replaced with a warning",
			procs: 8, registerFlag: true, args: []string{"--workers=0"}, wantWorkers: 8,
			wantWarn: true, wantWarnHas: "= 0",
		},
		{
			name:  "a negative value lands on the same outcome",
			procs: 8, registerFlag: true, args: []string{"--workers=-1"}, wantWorkers: 8,
			wantWarn: true, wantWarnHas: "= -1",
		},
		{
			name:  "a single permitted cpu still accepts two workers",
			procs: 1, registerFlag: true, args: []string{"--workers=2"}, wantWorkers: 2,
		},
		{
			name:  "a single permitted cpu replaces five",
			procs: 1, registerFlag: true, args: []string{"--workers=5"}, wantWorkers: 2,
			wantWarn: true, wantWarnHas: "= 5",
		},
		{
			name:  "above the ceiling the substitute is the default, not the ceiling",
			procs: 64, registerFlag: true, args: []string{"--workers=65"}, wantWorkers: 16,
			wantWarn: true, wantWarnHas: "= 65",
		},
		{
			name:  "the ceiling is the permitted cpu, not the default's own cap",
			procs: 64, registerFlag: true, args: []string{"--workers=32"}, wantWorkers: 32,
		},
		{name: "an unregistered flag takes the default silently", procs: 8, wantWorkers: 8},
	}
}

// TestApplyWorkers covers applyWorkers and nothing else: which --workers
// values it honors, which it replaces with helpers.DefaultInstallWorkers, and
// when it queues a warning about a replacement. Every want is hand-spelled
// rather than computed from helpers.DefaultInstallWorkers or
// helpers.MaxAcceptedInstallWorkers, since a want built from the function it
// checks moves with any mutation of that function and so could never fail
// against one.
//
// The first row is the positive control - without it, the replacements below
// would be indistinguishable from a fixture that never reaches the check at
// all - and the last is the cleanup shape, a command that registers no
// --workers flag and must take the default silently rather than be warned
// about a value nobody supplied.
//
// Two procs values carry rows procs=8 structurally cannot. At 8 the derived
// default and the accepted ceiling are the same number, so a substitution
// handing back the ceiling instead of the default is invisible there; the
// procs=64 rows separate the two (ceiling 64, default 16). At 8 the ceiling is
// procs itself, so the floor under it is invisible too; the procs=1 rows are
// where max(procs, MinDefaultInstallWorkers) is the only thing admitting 2.
//
// This test says nothing about which value the production flag resolves to;
// that is TestWorkersEnvShapes' subject (cmd/go-galaxy/commands), on the real
// cliflags.CollectionFlags().
//
// KILLING MUTATIONS, all four run and reverted.
//
// M1, applyWorkers' whole `if !c.IsSet("workers")` branch deleted. Of the
// rows above only the unregistered one fails, and it fails on its WARNING
// assertion rather than its value one: an unknown flag name reads 0, which the
// range arm then replaces with the very default the deleted branch would have
// taken, so the warning count is the only observable difference:
//
//	config_test.go:572: len(cfg.Warnings) = 1, want 0
//
// M2, helpers.MaxAcceptedInstallWorkers' body reduced to a bare `procs`. Of
// the rows above only procs=1, --workers=2 fails, since it is the only one
// whose honored value sits above procs and at or below the floor:
//
//	config_test.go:572: len(cfg.Warnings) = 1, want 0
//
// M3, the substitute changed from the default to the ceiling (`n = fallback`
// rewritten to `n = upper`). Only the procs=64, --workers=65 row fails; every
// procs=8 row survives it, which is exactly why that row exists at all:
//
//	config_test.go:572: cfg.Workers = 64, want 16
//
// M4, `n < 1` relaxed to `n < 0` in the range arm. Both zero shapes fail - the
// zero row quoted below and the environment subtest that follows it - while
// the negative row survives, pinning the boundary rather than replacement:
//
//	config_test.go:572: cfg.Workers = 0, want 8
func TestApplyWorkers(t *testing.T) {
	// The fixture flag's own Value. No row in the table reads it - each
	// registered row supplies --workers explicitly and the unregistered one has
	// no flag at all - so it matters only in the declared-but-empty subtest at
	// the end, where it stands in for the production flag's Value at procs 8.
	fixtureValue := helpers.DefaultInstallWorkers(8)

	tests := workersRows()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newIntFlagCmd(t, "workers", "GO_GALAXY_WORKERS", fixtureValue, tt.registerFlag, tt.args)
			cfg := &Config{}
			applyWorkers(cfg, c, tt.procs)
			assertWorkersOutcome(t, cfg, tt)
		})
	}

	// t.Setenv forbids t.Parallel, so the two environment-sourced shapes are
	// their own non-parallel subtests rather than rows in the table above.
	t.Run("a zero from the environment is replaced with a warning", func(t *testing.T) {
		t.Setenv("GO_GALAXY_WORKERS", "0")
		c := newIntFlagCmd(t, "workers", "GO_GALAXY_WORKERS", fixtureValue, true, nil)
		cfg := &Config{}
		applyWorkers(cfg, c, 8)
		assertWorkersOutcome(t, cfg, workersRow{wantWorkers: 8, wantWarn: true, wantWarnHas: "= 0"})
	})

	// The declared-but-empty shape urfave marks as set while skipping the parse
	// for. It reaches the range arm rather than the gate, and is honored there
	// because what c.Int reads is the flag's own Value - which is why this
	// subtest is the one place the fixture's Value has to be the production
	// derivation rather than an arbitrary number.
	t.Run("a declared but empty variable is honored on the flag's Value", func(t *testing.T) {
		t.Setenv("GO_GALAXY_WORKERS", "")
		c := newIntFlagCmd(t, "workers", "GO_GALAXY_WORKERS", fixtureValue, true, nil)
		cfg := &Config{}
		applyWorkers(cfg, c, 8)
		assertWorkersOutcome(t, cfg, workersRow{wantWorkers: 8})
	})
}

// TestDownloadWorkersDefault covers helpers.DefaultDownloadWorkers's own
// clamp (cpus * DownloadWorkersPerCPU, floored at MinDefaultDownloadWorkers
// and capped at MaxDefaultDownloadWorkers), plus newConfigFromCLI's
// DownloadWorkers fallback, which applies that identical default whenever no
// source supplied a positive --download-workers value. That fallback is
// silent and carries no ceiling above it, which is where it parts company
// with applyWorkers: a --workers value outside the range that one accepts is
// replaced with a warning, while a --download-workers value above the
// permitted CPU is a legitimate configuration rather than an oversubscription
// - this pool waits on the network instead of extracting a tree.
//
// KILLING MUTATION, run for real: the MinDefaultDownloadWorkers floor
// removed from DefaultDownloadWorkers (the inner `max(cpus*DownloadWorkersPerCPU,
// MinDefaultDownloadWorkers)` call rewritten to a bare `cpus*DownloadWorkersPerCPU`,
// leaving only the MaxDefaultDownloadWorkers cap). The 1-cpu row fails, because its
// raw product (1*4=4) sits below the floor and nothing clamps it back up:
//
//	config_test.go:639: DefaultDownloadWorkers(1) = 4, want 8
//
// The 2-cpu row does NOT fail this mutation: its raw product (2*4=8) already
// equals MinDefaultDownloadWorkers, so the floor was never the thing keeping
// that row at 8 in the first place - only the 1-cpu row's outcome actually
// depends on the floor existing.
func TestDownloadWorkersDefault(t *testing.T) {
	t.Run("derives from cpu count", func(t *testing.T) {
		tests := []struct {
			name string
			cpus int
			want int
		}{
			{name: "1 cpu floors to the minimum", cpus: 1, want: 8},
			{name: "2 cpus already equal the minimum", cpus: 2, want: 8},
			{name: "4 cpus scales linearly", cpus: 4, want: 16},
			{name: "8 cpus reaches the maximum exactly", cpus: 8, want: 32},
			{name: "16 cpus caps at the maximum", cpus: 16, want: 32},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := helpers.DefaultDownloadWorkers(tt.cpus); got != tt.want {
					t.Fatalf("DefaultDownloadWorkers(%d) = %d, want %d", tt.cpus, got, tt.want)
				}
			})
		}
	})

	t.Run("explicit positive value survives unoverridden", func(t *testing.T) {
		c := newIntFlagCmd(t, "download-workers", "GO_GALAXY_DOWNLOAD_WORKERS",
			helpers.DefaultDownloadWorkers(runtime.NumCPU()), true, []string{"--download-workers=3"})
		cfg := newConfigFromCLI(c)
		if cfg.DownloadWorkers != 3 {
			t.Fatalf("cfg.DownloadWorkers = %d, want 3", cfg.DownloadWorkers)
		}
	})

	t.Run("zero falls back to the default", func(t *testing.T) {
		c := newIntFlagCmd(t, "download-workers", "GO_GALAXY_DOWNLOAD_WORKERS",
			helpers.DefaultDownloadWorkers(runtime.NumCPU()), true, []string{"--download-workers=0"})
		cfg := newConfigFromCLI(c)
		want := helpers.DefaultDownloadWorkers(runtime.NumCPU())
		if cfg.DownloadWorkers != want {
			t.Fatalf("cfg.DownloadWorkers = %d, want %d (the default)", cfg.DownloadWorkers, want)
		}
	})

	t.Run("negative falls back to the default", func(t *testing.T) {
		c := newIntFlagCmd(t, "download-workers", "GO_GALAXY_DOWNLOAD_WORKERS",
			helpers.DefaultDownloadWorkers(runtime.NumCPU()), true, []string{"--download-workers=-1"})
		cfg := newConfigFromCLI(c)
		want := helpers.DefaultDownloadWorkers(runtime.NumCPU())
		if cfg.DownloadWorkers != want {
			t.Fatalf("cfg.DownloadWorkers = %d, want %d (the default)", cfg.DownloadWorkers, want)
		}
	})
}

// TestInstallWorkersDefault covers helpers.DefaultInstallWorkers's own clamp
// (procs, floored at MinDefaultInstallWorkers and capped at
// MaxDefaultInstallWorkers), plus the relation between that clamp and
// helpers.MaxAcceptedInstallWorkers' ceiling. It is TestDownloadWorkersDefault's
// sibling, against the other of the two worker pools. Which supplied values
// applyWorkers then honors or replaces is TestApplyWorkers' subject, not this
// one's.
//
// The rows are spelled as permitted CPUs rather than cores because that is
// what the parameter means: under a CFS quota runtime.NumCPU() reports the
// node while runtime.GOMAXPROCS(0) reports the quota, and this function is
// fed the latter (see helpers.DefaultInstallWorkers). Every want is
// hand-spelled rather than computed from the two constants, since a want
// built from the constant it checks moves with any mutation of that constant
// and so could never fail against one.
//
// Two rows the boundaries suggest collapse into one each, and that is a
// property of the values rather than a gap: the floor is 2, so "one below the
// floor" IS the 1-cpu row, and "the floor itself" IS the 2-cpu row.
//
// No row here drives a *cli.Command at all. The production flag's own Value is
// pinned by TestWorkersEnvShapes (cmd/go-galaxy/commands), which runs the real
// cliflags.CollectionFlags(); a fixture flag here would only carry a hand-copy
// of that Value and so could only compare it against itself.
//
// KILLING MUTATIONS, both run for real against helpers.DefaultInstallWorkers.
//
// M1, the floor removed - the whole body rewritten to
// `min(procs, MaxDefaultInstallWorkers)`. Only the 1-cpu row fails, since it
// is the only row whose input sits below the floor at all:
//
//	config_test.go:737: DefaultInstallWorkers(1) = 1, want 2
//
// The 2-cpu row does NOT fail this mutation: its input already equals
// MinDefaultInstallWorkers, so the floor was never what kept that row at 2 -
// only the 1-cpu row's outcome actually depends on the floor existing.
//
// M2, the cap removed - the body rewritten to
// `max(procs, MinDefaultInstallWorkers)`. Both rows above the cap fail while
// the row exactly at it survives, which is what makes the trio a pin on the
// boundary rather than on clamping in general:
//
//	config_test.go:737: DefaultInstallWorkers(17) = 17, want 16
//	config_test.go:737: DefaultInstallWorkers(128) = 128, want 16
func TestInstallWorkersDefault(t *testing.T) {
	t.Run("derives from permitted cpu", func(t *testing.T) {
		tests := []struct {
			name  string
			procs int
			want  int
		}{
			{name: "1 permitted cpu floors to the minimum", procs: 1, want: 2},
			{name: "2 permitted cpus already equal the minimum", procs: 2, want: 2},
			{name: "3 permitted cpus pass through unclamped", procs: 3, want: 3},
			{name: "15 permitted cpus stay one below the maximum", procs: 15, want: 15},
			{name: "16 permitted cpus reach the maximum exactly", procs: 16, want: 16},
			{name: "17 permitted cpus cap at the maximum", procs: 17, want: 16},
			{name: "a 128-cpu node caps at the maximum", procs: 128, want: 16},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := helpers.DefaultInstallWorkers(tt.procs); got != tt.want {
					t.Fatalf("DefaultInstallWorkers(%d) = %d, want %d", tt.procs, got, tt.want)
				}
			})
		}
	})

	// What this pins is the RELATION between the two functions; the ceiling
	// assertion ahead of it holds each hand-spelled wantCeiling to
	// MaxAcceptedInstallWorkers' own answer, so the relation is measured
	// against the real ceiling rather than a figure that drifted from it - and
	// hand-spelled for the same reason every want above is. The relation is
	// what a declared-but-empty GO_GALAXY_WORKERS= rests on: that shape reaches
	// applyWorkers' range arm carrying the flag's Value, so a derived default
	// outside the accepted range would warn about a value nobody wrote.
	t.Run("the derived default is always inside the accepted range", func(t *testing.T) {
		tests := []struct {
			name        string
			procs       int
			wantCeiling int
		}{
			{name: "1 permitted cpu is floored to two", procs: 1, wantCeiling: 2},
			{name: "2 permitted cpus are the floor itself", procs: 2, wantCeiling: 2},
			{name: "3 permitted cpus pass through", procs: 3, wantCeiling: 3},
			{name: "4 permitted cpus pass through", procs: 4, wantCeiling: 4},
			{name: "12 permitted cpus pass through", procs: 12, wantCeiling: 12},
			{name: "15 permitted cpus stay one below the default's cap", procs: 15, wantCeiling: 15},
			{name: "16 permitted cpus meet the default's cap", procs: 16, wantCeiling: 16},
			{name: "17 permitted cpus pass the default's cap", procs: 17, wantCeiling: 17},
			{name: "64 permitted cpus are accepted in full", procs: 64, wantCeiling: 64},
			{name: "128 permitted cpus are accepted in full", procs: 128, wantCeiling: 128},
			{name: "1024 permitted cpus are accepted in full", procs: 1024, wantCeiling: 1024},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := helpers.MaxAcceptedInstallWorkers(tt.procs); got != tt.wantCeiling {
					t.Fatalf("MaxAcceptedInstallWorkers(%d) = %d, want %d", tt.procs, got, tt.wantCeiling)
				}
				got := helpers.DefaultInstallWorkers(tt.procs)
				if got < 1 || got > tt.wantCeiling {
					t.Fatalf("DefaultInstallWorkers(%d) = %d, want inside 1..%d", tt.procs, got, tt.wantCeiling)
				}
			})
		}
	})
}

// TestApplyAnsibleConfigServer checks the server -> Server mapping with the
// same three precedence scenarios as TestApplyAnsibleConfigDownloadPath.
func TestApplyAnsibleConfigServer(t *testing.T) {
	t.Run("flag unset, ansible.cfg value present", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ansible.example"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: testDefaultCacheDir,
			server: "https://ansible.example", serverUsed: true, ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag set explicitly", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ansible.example"}}
		got := runApplyAnsibleConfig(t, []string{"--server=https://explicit.example"}, ansCfg)
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: testDefaultCacheDir,
			server: "https://explicit.example", ansibleConfigPath: testAnsibleConfigPath,
		})
	})

	t.Run("flag unset, ansible.cfg empty", func(t *testing.T) {
		got := runApplyAnsibleConfig(t, nil, ansibleConfig{})
		assertApplyAnsibleConfig(t, got, applyAnsibleConfigWant{
			downloadPath: testDefaultDownloadPath, cacheDir: testDefaultCacheDir,
			server: testDefaultServer, ansibleConfigPath: testAnsibleConfigPath,
		})
	})
}

// newAnsibleConfigCmd builds a *cli.Command exposing only the "ansible-config"
// flag as it is really defined in cmd/go-galaxy/cliflags/flags.go: no default
// value, sourced only from GO_GALAXY_ANSIBLE_CONFIG (ANSIBLE_CONFIG is
// handled by discovery, not by the flag itself). Matching that shape matters
// here because c.IsSet("ansible-config") is exactly what distinguishes
// "explicitly requested" from "let discovery decide".
func newAnsibleConfigCmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "ansible-config", Sources: cli.EnvVars("GO_GALAXY_ANSIBLE_CONFIG")},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// writeAnsibleCfg writes a minimal, distinguishable ansible.cfg to path so
// tests can assert it was the one actually loaded.
func writeAnsibleCfg(t *testing.T, path, server string) {
	t.Helper()
	content := "[galaxy]\nserver = " + server + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
	}
}

// TestLoadAnsibleConfigFromCLIExplicit checks the strict handling of an
// explicitly requested --ansible-config: an existing file is loaded and its
// path returned, and a missing file is an error (helpers.ErrAnsibleConfigNotFound),
// unlike ansible.cfg discovery which tolerates a missing candidate.
func TestLoadAnsibleConfigFromCLIExplicit(t *testing.T) {
	t.Run("existing path is loaded", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "custom.cfg")
		writeAnsibleCfg(t, path, "https://explicit.example")

		c := newAnsibleConfigCmd(t, []string{"--ansible-config=" + path})
		cfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		if err != nil {
			t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
		}
		if gotPath != path {
			t.Errorf("path = %q, want %q", gotPath, path)
		}
		if cfg.Galaxy.Server != "https://explicit.example" {
			t.Errorf("Galaxy.Server = %q, want %q", cfg.Galaxy.Server, "https://explicit.example")
		}
	})

	t.Run("missing path is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.cfg")

		c := newAnsibleConfigCmd(t, []string{"--ansible-config=" + path})
		_, _, _, err := loadAnsibleConfigFromCLI(c)
		if !errors.Is(err, helpers.ErrAnsibleConfigNotFound) {
			t.Errorf("error = %v, want helpers.ErrAnsibleConfigNotFound", err)
		}
	})
}

// TestLoadAnsibleConfigFromCLIDiscovery checks ansible.cfg discovery when
// --ansible-config is not explicitly set: ./ansible.cfg in the current
// directory is found, $ANSIBLE_CONFIG is preferred over it when both exist,
// and a missing $ANSIBLE_CONFIG target falls through to the next candidate
// instead of erroring (unlike the explicit-flag case above).
//
// These subtests mutate the working directory and environment (t.Chdir,
// t.Setenv), so they cannot run in parallel with each other or anything
// else that depends on cwd/env.
func TestLoadAnsibleConfigFromCLIDiscovery(t *testing.T) {
	t.Run("cwd ansible.cfg is discovered", func(t *testing.T) {
		dir := t.TempDir()
		writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
		t.Chdir(dir)

		c := newAnsibleConfigCmd(t, nil)
		cfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		// discoverAnsibleConfigPath checks the literal relative candidate
		// "ansible.cfg", not an absolute path, since it relies on the
		// process's current directory the same way ansible's own discovery
		// does.
		assertAnsibleConfigLoaded(t, cfg, gotPath, err, "ansible.cfg", "https://cwd.example")
	})

	t.Run("ANSIBLE_CONFIG is discovered ahead of cwd", func(t *testing.T) {
		envDir := t.TempDir()
		envPath := filepath.Join(envDir, "env.cfg")
		writeAnsibleCfg(t, envPath, "https://env.example")
		t.Setenv("ANSIBLE_CONFIG", envPath)

		cwdDir := t.TempDir()
		writeAnsibleCfg(t, filepath.Join(cwdDir, "ansible.cfg"), "https://cwd.example")
		t.Chdir(cwdDir)

		c := newAnsibleConfigCmd(t, nil)
		cfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigLoaded(t, cfg, gotPath, err, envPath, "https://env.example")
	})

	t.Run("missing ANSIBLE_CONFIG falls through to cwd, no error", func(t *testing.T) {
		t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "missing.cfg"))

		cwdDir := t.TempDir()
		writeAnsibleCfg(t, filepath.Join(cwdDir, "ansible.cfg"), "https://cwd.example")
		t.Chdir(cwdDir)

		c := newAnsibleConfigCmd(t, nil)
		cfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigLoaded(t, cfg, gotPath, err, "ansible.cfg", "https://cwd.example")
	})

	t.Run("world-writable cwd is skipped with a warning", subtestWorldWritableCwdSkipped)
	t.Run("non-world-writable cwd is discovered", subtestNonWorldWritableCwdDiscovered)
	t.Run("world-writable cwd: a relative ANSIBLE_CONFIG still reads that file", subtestWorldWritableCwdEnvPathStillRead)

	t.Run("nothing found in cwd or ANSIBLE_CONFIG: falls through cleanly", func(t *testing.T) {
		// ANSIBLE_CONFIG points at a missing file and the cwd has no
		// ansible.cfg, so discovery must fall through to ~/.ansible.cfg and
		// then /etc/ansible/ansible.cfg without erroring. This test cannot
		// control whether those two machine-level files exist, so it only
		// asserts the one thing that must hold regardless of the host: no
		// error, and if a path was found, it is one of those two candidates
		// (production discovery order is not weakened to make this
		// assertion pass).
		t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "missing.cfg"))
		t.Chdir(t.TempDir())

		c := newAnsibleConfigCmd(t, nil)
		_, gotPath, _, err := loadAnsibleConfigFromCLI(c)
		assertDiscoveryFallsThroughCleanly(t, gotPath, err)
	})
}

// chmodDir sets dir's mode, failing the test if it cannot: a silently
// unchanged mode would turn the subtests below into tests of nothing, since
// t.TempDir is 0o700 on most systems and the mode is their whole subject.
func chmodDir(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	// #nosec G302 -- the permission is the fixture: these subtests exist to
	// drive discovery against a world-writable working directory.
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatalf("os.Chmod(%q, %#o) error = %v, want nil", dir, mode, err)
	}
}

// subtestWorldWritableCwdSkipped proves ./ansible.cfg is not a discovery
// candidate when the working directory is world-writable, and that the run
// says so rather than falling silent.
// subtestNonWorldWritableCwdDiscovered is its positive control.
func subtestWorldWritableCwdSkipped(t *testing.T) {
	// The env candidate is pointed at a missing file so it cannot win and
	// mask what the cwd candidate did.
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "missing.cfg"))

	dir := t.TempDir()
	writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
	chmodDir(t, dir, 0o777)
	t.Chdir(dir)

	c := newAnsibleConfigCmd(t, nil)
	_, gotPath, warnings, err := loadAnsibleConfigFromCLI(c)
	if err != nil {
		t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
	}
	// Killing mutation, run: reducing cwdCandidate to an unconditional
	// `return cwdAnsibleCfgName, ""` fails this assertion with `path =
	// "ansible.cfg", want the cwd candidate to have been skipped`. The check
	// cannot be deleted on its own and still compile, since the stat result
	// would go unused - which is why the mutation is the whole body.
	if gotPath == "ansible.cfg" {
		t.Fatalf("path = %q, want the cwd candidate to have been skipped", gotPath)
	}
	if !warningMentions(warnings, "world-writable", dir) {
		t.Fatalf("warnings = %v, want one naming %q as world-writable", warnings, dir)
	}
}

// subtestNonWorldWritableCwdDiscovered is the positive control described on
// subtestWorldWritableCwdSkipped: the same directory and the same file, with
// only the mode differing, must be discovered and must warn about nothing.
// Without it, "skipped" would be indistinguishable from a fixture discovery
// never reaches at all.
func subtestNonWorldWritableCwdDiscovered(t *testing.T) {
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "missing.cfg"))

	dir := t.TempDir()
	writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
	chmodDir(t, dir, 0o755)
	t.Chdir(dir)

	c := newAnsibleConfigCmd(t, nil)
	cfg, gotPath, warnings, err := loadAnsibleConfigFromCLI(c)
	assertAnsibleConfigLoaded(t, cfg, gotPath, err, "ansible.cfg", "https://cwd.example")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// subtestWorldWritableCwdEnvPathStillRead pins that the world-writable
// warning discloses the escape hatch rather than stopping at what it
// dropped. It pins only that half: a regression keeping the old "ignoring
// ./ansible.cfg" wording while appending a mention of $ANSIBLE_CONFIG would
// satisfy every assertion here. Pinning the absence of that wording was
// considered and rejected, because warningMentions exists precisely so this
// file does not own wording it has no reason to own.
//
// The fixture is built identically to subtestWorldWritableCwdSkipped's - a
// fresh t.TempDir holding the same planted file at the same 0o777 mode -
// and differs in one input: $ANSIBLE_CONFIG names that file relatively
// instead of naming a missing one. The check discoverAnsibleConfigPath
// applies is scoped to the cwd candidate alone (see cwdCandidate), so the
// env candidate reaches the same file untouched and the run loads it.
//
// gotPath cannot discriminate between the two candidates here, and that is
// what makes the warning assertion load-bearing rather than decorative:
// $ANSIBLE_CONFIG is set to the relative "ansible.cfg", so the winning path
// is byte-identical to cwdAnsibleCfgName whichever candidate produced it.
// The sibling subtest is what covers the direction this one structurally
// cannot see - a regression that warns while KEEPING the cwd candidate
// fails there and passes here.
//
// Killing mutation, run: restoring cwdCandidate's former warning text ("the
// current directory %q is world-writable; ignoring ./ansible.cfg as a
// configuration source") leaves this subtest's first two assertions passing -
// the file still loads, and the text still names the directory as
// world-writable - and fails the third, at config_test.go:1073:
//
//	warnings = [the current directory "/var/folders/..." is world-writable;
//	ignoring ./ansible.cfg as a configuration source], want one naming
//	$ANSIBLE_CONFIG as a path still read
func subtestWorldWritableCwdEnvPathStillRead(t *testing.T) {
	dir := t.TempDir()
	writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
	chmodDir(t, dir, 0o777)
	t.Chdir(dir)
	// Relative on purpose. An absolute path into the same directory would
	// reach the same file, but the relative form makes that identity
	// self-evident without the assertion depending on a temp path, and it is
	// the form cwdAnsibleCfgName documents as resolving at open time.
	t.Setenv("ANSIBLE_CONFIG", "ansible.cfg")

	c := newAnsibleConfigCmd(t, nil)
	cfg, gotPath, warnings, err := loadAnsibleConfigFromCLI(c)
	assertAnsibleConfigLoaded(t, cfg, gotPath, err, "ansible.cfg", "https://cwd.example")
	if !warningMentions(warnings, "world-writable", dir) {
		t.Fatalf("warnings = %v, want one naming %q as world-writable", warnings, dir)
	}
	if !warningMentions(warnings, "$ANSIBLE_CONFIG") {
		t.Fatalf("warnings = %v, want one naming $ANSIBLE_CONFIG as a path still read", warnings)
	}
}

// warningMentions reports whether any warning contains every one of parts.
// Matching by fragment rather than by the whole line keeps the test from
// pinning wording it has no reason to own, while still requiring the warning
// to name both what is wrong and which directory it is wrong about.
func warningMentions(warnings []string, parts ...string) bool {
	for _, w := range warnings {
		matched := true
		for _, part := range parts {
			if !strings.Contains(w, part) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// assertAnsibleConfigLoaded checks that loadAnsibleConfigFromCLI succeeded
// and returned the expected path and Galaxy.Server value.
func assertAnsibleConfigLoaded(t *testing.T, cfg ansibleConfig, gotPath string, err error, wantPath, wantServer string) {
	t.Helper()
	if err != nil {
		t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
	}
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if cfg.Galaxy.Server != wantServer {
		t.Errorf("Galaxy.Server = %q, want %q", cfg.Galaxy.Server, wantServer)
	}
}

// assertDiscoveryFallsThroughCleanly checks that discovery produced no
// error, and that any discovered path is one of the two machine-level
// candidates (~/.ansible.cfg, /etc/ansible/ansible.cfg) this test cannot
// control the presence of; see the caller for why.
func assertDiscoveryFallsThroughCleanly(t *testing.T, gotPath string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
	}
	if gotPath == "" {
		return
	}
	home, homeErr := os.UserHomeDir()
	wantHomePath := ""
	if homeErr == nil {
		wantHomePath = filepath.Join(home, ".ansible.cfg")
	}
	if gotPath != wantHomePath && gotPath != "/etc/ansible/ansible.cfg" {
		t.Errorf("path = %q, want %q, %q, or empty", gotPath, wantHomePath, "/etc/ansible/ansible.cfg")
	}
}

// TestAnsibleGalaxyServerEnv pins where ANSIBLE_GALAXY_SERVER sits in the
// precedence chain: it is the env spelling of the [galaxy] server key, so it
// outranks that key and nothing above it. The rows are the three states the
// variable can be in, and the third is the one that decides a design question
// rather than restating the other two - an exported but empty value must not
// name a server with no URL, so it has to read as absent rather than as an
// override that won.
//
// No t.Parallel anywhere here: t.Setenv forbids it, and the neighboring
// applyAnsibleConfig tests do not use it either.
//
// KILLING MUTATION, run and reverted: making ansibleGalaxyServer prefer the
// ini value over the env one (returning ini whenever it is non-empty). Two
// rows fail - the first, on the precedence itself:
//
//	config_test.go:1162: Server = "https://ini.example", want "https://env.example"
//
// and the third, because a non-empty ini value shadows the empty-env case too:
//
//	config_test.go:1187: Server = "https://ini.example", want the flag default "https://default.example"
func TestAnsibleGalaxyServerEnv(t *testing.T) {
	const envServer = "https://env.example"

	t.Run("env beats the ansible.cfg key", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER", envServer)
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ini.example"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		if got.Server != envServer {
			t.Fatalf("Server = %q, want %q", got.Server, envServer)
		}
		if !got.AnsibleServerUsed || !got.AnsibleServerEnvUsed {
			t.Fatalf("AnsibleServerUsed = %v, AnsibleServerEnvUsed = %v, want both true",
				got.AnsibleServerUsed, got.AnsibleServerEnvUsed)
		}
	})

	t.Run("env unset keeps the ansible.cfg key", func(t *testing.T) {
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ini.example"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		if got.Server != "https://ini.example" {
			t.Fatalf("Server = %q, want %q", got.Server, "https://ini.example")
		}
		if !got.AnsibleServerUsed || got.AnsibleServerEnvUsed {
			t.Fatalf("AnsibleServerUsed = %v, AnsibleServerEnvUsed = %v, want true and false",
				got.AnsibleServerUsed, got.AnsibleServerEnvUsed)
		}
	})

	t.Run("env set empty falls through to the flag default", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER", "")
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ini.example"}}
		got := runApplyAnsibleConfig(t, nil, ansCfg)
		if got.Server != testDefaultServer {
			t.Fatalf("Server = %q, want the flag default %q", got.Server, testDefaultServer)
		}
		if got.AnsibleServerUsed || got.AnsibleServerEnvUsed {
			t.Fatalf("AnsibleServerUsed = %v, AnsibleServerEnvUsed = %v, want both false",
				got.AnsibleServerUsed, got.AnsibleServerEnvUsed)
		}
	})
}
