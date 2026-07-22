package config

import (
	"context"
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
// built inline (mirroring cmd/go-galaxy/helpers/flags_test.go's pattern)
// rather than importing the cmd helpers package, to avoid an import cycle.
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

// newTimeoutCmd builds a *cli.Command for TestApplyTimeout. When
// registerFlag is true it exposes a --timeout DurationFlag (mirroring how
// CollectionFlags registers it, default helpers.FetchDefaultTimeout);
// when false no such flag exists at all, mirroring commands like cleanup
// that never register --timeout, so c.Duration("timeout") reads as zero.
func newTimeoutCmd(t *testing.T, registerFlag bool, args []string) *cli.Command {
	t.Helper()

	var flags []cli.Flag
	if registerFlag {
		flags = []cli.Flag{&cli.DurationFlag{Name: "timeout", Value: helpers.FetchDefaultTimeout}}
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

// TestApplyTimeout checks that applyTimeout only falls back to the default
// on the absent/zero case: a small positive --timeout is honored as-is
// (the regression this change fixes - it used to be silently floored to
// the default), a larger value passes through unchanged, an unset flag
// falls back to the default, and a command that never registers --timeout
// at all (e.g. cleanup) also falls back to the default rather than
// building an unbounded-timeout HTTP client.
func TestApplyTimeout(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		want         time.Duration
		registerFlag bool
	}{
		{
			name:         "small positive timeout is honored, not floored",
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
			applyTimeout(cfg, c)
			if cfg.Timeout != tt.want {
				t.Errorf("Timeout = %v, want %v", cfg.Timeout, tt.want)
			}
		})
	}
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
