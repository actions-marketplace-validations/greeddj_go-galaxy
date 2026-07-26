package commands

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/urfave/cli/v3"
)

// buildConfigFor runs config.BuildCollectionConfig(c) through a two-level
// *cli.Command tree mirroring main.go's real wiring: global flags on the
// root app (helpers.CommonFlags), and subFlags on a single subcommand named
// sub. The subcommand's action does no I/O; it only captures
// BuildCollectionConfig's result for the caller to assert on.
func buildConfigFor(t *testing.T, sub string, subFlags []cli.Flag, args []string) (*config.Config, error) {
	t.Helper()

	var gotCfg *config.Config
	var gotErr error
	app := &cli.Command{
		Name:  "go-galaxy",
		Flags: helpers.CommonFlags(),
		Commands: []*cli.Command{
			{
				Name:  sub,
				Flags: subFlags,
				Action: func(_ context.Context, c *cli.Command) error {
					gotCfg, gotErr = config.BuildCollectionConfig(c)
					return nil
				},
			},
		},
	}

	fullArgs := append([]string{"go-galaxy", sub}, args...)
	if err := app.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("app.Run() error = %v, want nil", err)
	}
	return gotCfg, gotErr
}

// neutralizeAnsibleDiscovery isolates ansible.cfg discovery from whatever
// happens to exist on the machine running these tests: it points
// $ANSIBLE_CONFIG at a path that does not exist, redirects $HOME to an
// empty temp directory, and changes into an empty temp directory, so
// neither ./ansible.cfg nor ~/.ansible.cfg is picked up. It cannot control
// /etc/ansible/ansible.cfg, so these tests only assert fields that were
// explicitly set via a flag - those always win over any ansible.cfg value,
// since explicit CLI/env beats ansible.cfg via cli.Command.IsSet.
func neutralizeAnsibleDiscovery(t *testing.T) {
	t.Helper()
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "absent.cfg"))
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
}

// assertConfigField fails the test unless got == want, naming field in the
// failure message. Kept generic so each call site stays a single line,
// which is what keeps the surface tests below within funlen/gocognit.
func assertConfigField[T comparable](t *testing.T, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}

// TestCleanupConfigSurface locks in the documented BuildCollectionConfig
// contract for cleanup: it registers only S3Flags (plus the global
// CommonFlags), and BuildCollectionConfig still reads the full flag union
// without erroring - install-only flags it never registers (server,
// download-path, and so on) simply resolve to their zero values, which is
// safe because cleanup never reads those Config fields (it consumes only
// DryRun, CacheDir, and S3Cache; see internal/galaxy/cleanup/cleanup.go).
func TestCleanupConfigSurface(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	cacheDir := t.TempDir()

	t.Run("local cache", func(t *testing.T) {
		args := []string{"--cache-dir=" + cacheDir, "--dry-run"}
		cfg, err := buildConfigFor(t, "cleanup", helpers.S3Flags(), args)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if cfg == nil {
			t.Fatal("BuildCollectionConfig() cfg = nil, want non-nil")
		}
		assertConfigField(t, "CacheDir", cfg.CacheDir, cacheDir)
		assertConfigField(t, "DryRun", cfg.DryRun, true)
		assertConfigField(t, "S3Cache.Enabled", cfg.S3Cache.Enabled, false)
	})

	t.Run("S3 cache", func(t *testing.T) {
		args := []string{
			"--cache-dir=" + cacheDir,
			"--s3-bucket=b",
			"--s3-access-key=k",
			"--s3-secret-key=s",
		}
		cfg, err := buildConfigFor(t, "cleanup", helpers.S3Flags(), args)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "S3Cache.Enabled", cfg.S3Cache.Enabled, true)
		assertConfigField(t, "S3Cache.Bucket", cfg.S3Cache.Bucket, "b")
	})
}

// TestCollectionCommandConfigSurface locks in the documented
// BuildCollectionConfig contract for install/lock/warm/outdated: they all
// register the identical CollectionFlags+S3Flags union (see install.go,
// lock.go, warm.go, outdated.go), so one representative run through
// "install" covers all four. Every collection-level flag set here must
// round-trip into the matching Config field.
func TestCollectionCommandConfigSurface(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	cacheDir := t.TempDir()

	flags := append(helpers.CollectionFlags(), helpers.S3Flags()...)
	args := []string{
		"--server=https://explicit.example",
		"--download-path=/explicit/collections",
		"--requirements-file=req.yml",
		"--lock-file=req.lock.yml",
		"--timeout=45s",
		"--workers=3",
		"--offline",
		"--cache-dir=" + cacheDir,
	}
	cfg, err := buildConfigFor(t, "install", flags, args)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	if cfg == nil {
		t.Fatal("BuildCollectionConfig() cfg = nil, want non-nil")
	}

	assertConfigField(t, "Server", cfg.Server, "https://explicit.example")
	assertConfigField(t, "DownloadPath", cfg.DownloadPath, "/explicit/collections")
	assertConfigField(t, "RequirementsFile", cfg.RequirementsFile, "req.yml")
	assertConfigField(t, "LockFile", cfg.LockFile, "req.lock.yml")
	assertConfigField(t, "Timeout", cfg.Timeout, 45*time.Second)
	assertConfigField(t, "Workers", cfg.Workers, 3)
	assertConfigField(t, "Offline", cfg.Offline, true)
	assertConfigField(t, "CacheDir", cfg.CacheDir, cacheDir)
}
