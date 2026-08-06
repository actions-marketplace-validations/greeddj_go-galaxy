package commands

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

// ansibleCfgWithServerList writes an ansible.cfg carrying a two-entry
// server_list plus a section for each id, points $ANSIBLE_CONFIG at it, and
// returns nothing: what the caller needs is the environment, not the path.
//
// It runs neutralizeAnsibleDiscovery first and then overrides $ANSIBLE_CONFIG,
// because that helper deliberately points the variable at a file that does not
// exist - which is the opposite of what these rows need.
func ansibleCfgWithServerList(t *testing.T) {
	t.Helper()
	neutralizeAnsibleDiscovery(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ansible.cfg")
	body := "[galaxy]\nserver_list = hub, pub\n\n" +
		"[galaxy_server.hub]\nurl = https://hub.example/\n\n" +
		"[galaxy_server.pub]\nurl = https://pub.example/\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write ansible.cfg: %v", err)
	}
	t.Setenv("ANSIBLE_CONFIG", path)
}

// serverIDs returns the resolved server ids in order, so a row can assert the
// whole list rather than one field of one entry.
func serverIDs(cfg *config.Config) []string {
	ids := make([]string, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		ids = append(ids, s.ID)
	}
	return ids
}

// TestAnsibleGalaxyServerDoesNotCollapseServerList pins the precedence fix:
// ANSIBLE_GALAXY_SERVER is the env spelling of the [galaxy] server key, so a
// configured server_list still wins over it. Before the fix it was a source of
// the --server flag, which made it precedence rule 1 - exporting it silently
// reduced a two-server configuration to one anonymous server, and a private hub
// simply disappeared, with no warning even under --verbose.
//
// The two control rows are what make the first one mean something: on the same
// fixture, the two spellings that ARE rule 1 must still collapse the list, so
// "the list survived" cannot be the fixture failing to collapse anything.
//
// KILLING MUTATION, run and reverted: restoring ANSIBLE_GALAXY_SERVER to the
// server flag's Sources in cmd/go-galaxy/helpers/flags.go. The first row fails:
//
//	config_surface_test.go:209: server ids = [], want [hub pub]
func TestAnsibleGalaxyServerDoesNotCollapseServerList(t *testing.T) {
	t.Run("the ansible env spelling leaves server_list intact", func(t *testing.T) {
		ansibleCfgWithServerList(t)
		t.Setenv("ANSIBLE_GALAXY_SERVER", "https://forced.example/")

		cfg, err := buildConfigFor(t, "install", helpers.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if got := serverIDs(cfg); !slices.Equal(got, []string{"hub", "pub"}) {
			t.Fatalf("server ids = %v, want [hub pub]", got)
		}
	})

	t.Run("the go-galaxy env spelling still collapses it", func(t *testing.T) {
		ansibleCfgWithServerList(t)
		t.Setenv("GO_GALAXY_SERVER", "https://forced.example/")

		cfg, err := buildConfigFor(t, "install", helpers.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if got := len(cfg.Servers); got != 1 {
			t.Fatalf("server count = %d (%v), want 1", got, serverIDs(cfg))
		}
	})

	t.Run("the flag still collapses it", func(t *testing.T) {
		ansibleCfgWithServerList(t)

		cfg, err := buildConfigFor(t, "install", helpers.CollectionFlags(), []string{"--server=https://forced.example/"})
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if got := len(cfg.Servers); got != 1 {
			t.Fatalf("server count = %d (%v), want 1", got, serverIDs(cfg))
		}
	})
}
