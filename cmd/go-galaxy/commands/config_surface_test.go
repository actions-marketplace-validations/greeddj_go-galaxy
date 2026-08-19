package commands

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// buildConfigFor runs config.BuildCollectionConfig(c) through a two-level
// *cli.Command tree mirroring main.go's real wiring: global flags on the
// root app (cliflags.CommonFlags), and subFlags on a single subcommand named
// sub. The subcommand's action does no I/O; it only captures
// BuildCollectionConfig's result for the caller to assert on.
func buildConfigFor(t *testing.T, sub string, subFlags []cli.Flag, args []string) (*config.Config, error) {
	t.Helper()

	var gotCfg *config.Config
	var gotErr error
	app := &cli.Command{
		Name:  "go-galaxy",
		Flags: cliflags.CommonFlags(),
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
// DryRun, CacheDir, and S3Cache; see internal/galaxy/cleanup).
func TestCleanupConfigSurface(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	cacheDir := t.TempDir()

	t.Run("local cache", func(t *testing.T) {
		args := []string{"--cache-dir=" + cacheDir, "--dry-run"}
		cfg, err := buildConfigFor(t, "cleanup", cliflags.S3Flags(), args)
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
		cfg, err := buildConfigFor(t, "cleanup", cliflags.S3Flags(), args)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "S3Cache.Enabled", cfg.S3Cache.Enabled, true)
		assertConfigField(t, "S3Cache.Bucket", cfg.S3Cache.Bucket, "b")
	})
}

// TestCollectionCommandConfigSurface locks in the documented
// BuildCollectionConfig contract for install/lock/warm/outdated: all four
// register the CollectionFlags+S3Flags union (see install.go, lock.go,
// warm.go, outdated.go), and the two of them that verify mount the signature
// flags on top of it, so one run through "install" covers what all four share.
// Every collection-level flag set here must round-trip into its Config field.
func TestCollectionCommandConfigSurface(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	cacheDir := t.TempDir()

	flags := append(cliflags.CollectionFlags(), cliflags.S3Flags()...)
	args := []string{
		"--server=https://explicit.example",
		"--download-path=/explicit/collections",
		"--requirements-file=req.yml",
		"--lock-file=req.lock.yml",
		"--timeout=45s",
		"--workers=1", // the one value inside the accepted range on every machine; see TestWorkersEnvShapes
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
	assertConfigField(t, "Workers", cfg.Workers, 1)
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

// TestAnsibleGalaxyServerDoesNotCollapseServerList pins one precedence rule:
// ANSIBLE_GALAXY_SERVER is the env spelling of the [galaxy] server key, so a
// configured server_list wins over it. Treating it as a source of the --server
// flag instead would make it precedence rule 1 - exporting it would silently
// reduce a two-server configuration to one anonymous server, and a private hub
// would simply disappear, with no warning even under --verbose.
//
// The two control rows are what make the first one mean something: on the same
// fixture, the two spellings that ARE rule 1 must still collapse the list, so
// "the list survived" cannot be the fixture failing to collapse anything.
//
// KILLING MUTATION, run and reverted: restoring ANSIBLE_GALAXY_SERVER to the
// server flag's Sources in cmd/go-galaxy/cliflags/flags.go. The first row fails:
//
//	config_surface_test.go:212: server ids = [], want [hub pub]
func TestAnsibleGalaxyServerDoesNotCollapseServerList(t *testing.T) {
	t.Run("the ansible env spelling leaves server_list intact", func(t *testing.T) {
		ansibleCfgWithServerList(t)
		t.Setenv("ANSIBLE_GALAXY_SERVER", "https://forced.example/")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
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

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if got := len(cfg.Servers); got != 1 {
			t.Fatalf("server count = %d (%v), want 1", got, serverIDs(cfg))
		}
	})

	t.Run("the flag still collapses it", func(t *testing.T) {
		ansibleCfgWithServerList(t)

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), []string{"--server=https://forced.example/"})
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if got := len(cfg.Servers); got != 1 {
			t.Fatalf("server count = %d (%v), want 1", got, serverIDs(cfg))
		}
	})
}

// aliasCfg builds the install config with no CLI arguments at all, so a value
// a row below asserts on can only have arrived through an env source.
func aliasCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	return cfg
}

// TestFlagNameEnvAliases pins the GO_GALAXY_<FLAG_NAME> env spelling on the
// only two flags that lacked one: --timeout and --download-path, whose env
// names were taken from ansible's own variables rather than derived from the
// flag name. Each accepts the flag-name-shaped spelling in second
// position, so the convention every other flag in cmd/go-galaxy/cliflags
// follows has no exception, while the name that shipped first keeps the
// precedence it already had.
//
// Three pairs of rows, each pair one path row and one timeout row. The
// pairs assert, in order, that the new name is read at all, that it does
// not outrank the name that shipped first, and that it does outrank the
// ANSIBLE_ spelling behind it. Both ordering directions are load-bearing:
// a chain carrying the new name first still passes the third pair, and one
// carrying it last still passes the second, so neither pins it alone.
//
// Two value choices are load-bearing and must survive a later tidy-up. The
// timeout values must not be 30s, since that is helpers.FetchDefaultTimeout
// and a row asserting it would pass with no env source read at all; the path
// values must not be ".collections", the --download-path default, for the
// identical reason, and must contain no ":", since firstCollectionsPath
// POSIX-splits the resolved value and would keep only what precedes the
// first one. t.TempDir() satisfies both.
//
// No row's asserted value depends on the /etc/ansible/ansible.cfg that
// neutralizeAnsibleDiscovery cannot control: each path row sets its flag
// through an env source, so pickConfigValue never reaches its ansible arm,
// and no ansible.cfg key feeds --timeout - applyTimeout reads it directly.
//
// KILLING MUTATIONS, all run and reverted, all on the Sources chains of the
// timeout and download-path flags in cmd/go-galaxy/cliflags. Quoted under
// TMPDIR=/tmp, which keeps the longest line here at 139 columns against
// lll's 140; the digits t.TempDir() appends differ on every run.
//
// M1, both chains cut back to their pre-change two-name form. It fails four
// rows rather than only the two that set the new spelling alone, because a
// row pitting it against the ANSIBLE_ spelling loses to that spelling once
// it is no longer a source at all:
//
//	config_surface_test.go:319: DownloadPath = .collections, want /tmp/TestFlagNameEnvAliases82068219/001/b
//	config_surface_test.go:335: DownloadPath = /tmp/TestFlagNameEnvAliases82068219/001/c, want /tmp/TestFlagNameEnvAliases82068219/001/b
//	config_surface_test.go:342: Timeout = 30s, want 1m30s
//	config_surface_test.go:358: Timeout = 1m0s, want 1m30s
//
// M2, positions 1 and 2 swapped in both chains. Only the two rows pitting
// the new name against the name that shipped first fail, which is what makes
// them a pin on ordering rather than on membership:
//
//	config_surface_test.go:327: DownloadPath = /tmp/TestFlagNameEnvAliases3589060566/001/b, want /tmp/TestFlagNameEnvAliases3589060566/001/a
//	config_surface_test.go:350: Timeout = 1m30s, want 45s
//
// M3, the new name demoted to last in both chains. Only the two rows pitting
// it against the ANSIBLE_ spelling fail, so second position is pinned from
// both sides and not merely membership in the chain:
//
//	config_surface_test.go:335: DownloadPath = /tmp/TestFlagNameEnvAliases4102758397/001/c, want /tmp/TestFlagNameEnvAliases4102758397/001/b
//	config_surface_test.go:358: Timeout = 1m0s, want 1m30s
func TestFlagNameEnvAliases(t *testing.T) {
	base := t.TempDir()
	pathA := filepath.Join(base, "a")
	pathB := filepath.Join(base, "b")
	pathC := filepath.Join(base, "c")

	t.Run("the flag-name spelling alone sets the path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_DOWNLOAD_PATH", pathB)

		assertConfigField(t, "DownloadPath", aliasCfg(t).DownloadPath, pathB)
	})

	t.Run("the name that shipped first outranks it for the path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_COLLECTIONS_PATH", pathA)
		t.Setenv("GO_GALAXY_DOWNLOAD_PATH", pathB)

		assertConfigField(t, "DownloadPath", aliasCfg(t).DownloadPath, pathA)
	})

	t.Run("it outranks the ansible spelling for the path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_DOWNLOAD_PATH", pathB)
		t.Setenv("ANSIBLE_COLLECTIONS_PATH", pathC)

		assertConfigField(t, "DownloadPath", aliasCfg(t).DownloadPath, pathB)
	})

	t.Run("the flag-name spelling alone sets the timeout", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_TIMEOUT", "90s")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 90*time.Second)
	})

	t.Run("the name that shipped first outranks it for the timeout", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_SERVER_TIMEOUT", "45s")
		t.Setenv("GO_GALAXY_TIMEOUT", "90s")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 45*time.Second)
	})

	t.Run("it outranks the ansible spelling for the timeout", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_TIMEOUT", "90s")
		t.Setenv("ANSIBLE_GALAXY_SERVER_TIMEOUT", "60")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 90*time.Second)
	})
}

// TestAnsibleRequirementsFileEnvIsStillRead pins a deliberate exception rather
// than a compatibility guarantee. ANSIBLE_GALAXY_REQUIREMENTS_FILE sits in
// ansible's namespace without being an ansible name: ansible-core declares no
// requirements-file setting, and ansible-galaxy takes that path only as
// -r/--role-file. The ANSIBLE_ prefix invites an assumption of parity that
// does not hold here, so a cleanup acting on that assumption would delete
// this one to restore parity - and it would not fail the pipelines that set
// it, it would silently install whatever requirements.yml the working
// directory happens to hold. The keep is the decision; this test is what makes
// dropping it loud.
//
// The second row states the order alongside it, so the exception cannot be
// mistaken for a promotion: go-galaxy's own name still wins.
//
// KILLING MUTATION, run and reverted, on the requirements-file Sources chain
// in cmd/go-galaxy/cliflags - drop envRequirementsFileAnsible from it, which is
// exactly the "restore parity" edit this test exists to stop:
//
//	config_surface_test.go:400: RequirementsFile = requirements.yml, want /from-ansible.yml
//
// The second row survives that mutation, since GO_GALAXY_REQUIREMENTS_FILE is
// untouched by it - which is why the first row, not the pair, is the pin.
//
// Both paths are literals rather than t.TempDir() values, as the surface row
// above already does for this same field: nothing between the flag and
// cfg.RequirementsFile opens the path, so a real file buys nothing, and a
// literal keeps the quoted failure above reproducible instead of carrying
// digits that change on every run.
func TestAnsibleRequirementsFileEnvIsStillRead(t *testing.T) {
	const (
		ansiblePath  = "/from-ansible.yml"
		goGalaxyPath = "/from-go-galaxy.yml"
	)

	t.Run("the ansible-namespaced name is read", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("ANSIBLE_GALAXY_REQUIREMENTS_FILE", ansiblePath)

		assertConfigField(t, "RequirementsFile", aliasCfg(t).RequirementsFile, ansiblePath)
	})

	t.Run("go-galaxy's own name outranks it", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_REQUIREMENTS_FILE", goGalaxyPath)
		t.Setenv("ANSIBLE_GALAXY_REQUIREMENTS_FILE", ansiblePath)

		assertConfigField(t, "RequirementsFile", aliasCfg(t).RequirementsFile, goGalaxyPath)
	})
}

// workersEnvRow is one shape GO_GALAXY_WORKERS can arrive in from an ambient
// CI environment block: the exported value, the worker count the run must
// resolve to, and whether it must also have warned about the value it replaced.
type workersEnvRow struct {
	name     string
	value    string
	want     int
	wantWarn bool
}

// workersWarning returns the queued configuration warning naming --workers, or
// "" when the run queued none. It matches on the flag name the message carries
// rather than on a position in the slice - applyWorkers happens to queue ahead
// of every other config-load warning, and this assertion does not rest on that.
// "--download-workers" does not match: its only "--" is followed by "download".
func workersWarning(cfg *config.Config) string {
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "--workers") {
			return w
		}
	}
	return ""
}

// TestWorkersEnvShapes pins how each GO_GALAXY_WORKERS shape resolves, driven
// through the real cliflags.CollectionFlags() rather than a hand-copied flag -
// which is the whole reason this test exists alongside TestApplyWorkers
// (internal/galaxy/config), whose fixture builds its own --workers flag and so
// can only pin the predicate, never the production flag's fields.
//
// Every want naming the derived default is computed from
// galaxyhelpers.DefaultInstallWorkers rather than hand-spelled, the opposite
// of TestApplyWorkers' rows, and forced rather than a matter of taste: this
// surface cannot fabricate a procs value, since BuildCollectionConfig reads
// runtime.GOMAXPROCS(0) itself, so a hand-spelled want would only pin
// whichever CPU count the test machine happens to permit.
//
// The "1" row is the positive control for the empty-value row specifically:
// without it, "the declared-but-empty variable was honored" would be
// indistinguishable from "this harness never reads the environment at all",
// since a harness ignoring the environment entirely would resolve that row to
// the very same derived default. 1 is the value it is because it is the only
// one both inside the accepted range on every machine - the ceiling is at
// least 2 - and never equal to the derived default, whose own floor is 2.
//
// The "1000000" row assumes no machine running this suite permits a million
// CPUs, which is what makes it an above-the-ceiling row rather than an
// ordinary accepted value.
//
// KILLING MUTATIONS, both run and reverted.
//
// M-A, the `Value` field deleted outright from the workers IntFlag in
// cmd/go-galaxy/cliflags. Only the empty-value row fails, and it fails on its
// warning assertion rather than its worker count: with no Value that row reads
// 0, which applyWorkers replaces with the same derived default the row already
// wanted, so the warning is the only observable difference:
//
//	config_surface_test.go:500: warned about --workers = true, want false
//
// M-B, the `if !c.IsSet("workers")` branch deleted from applyWorkers
// (internal/galaxy/config). Not one row of the table fails - none of them is
// the unset shape - and the cleanup subtest at the end fails instead, which is
// exactly the coverage that subtest exists to carry:
//
//	config_surface_test.go:521: warned about --workers, want no such warning
func TestWorkersEnvShapes(t *testing.T) {
	derived := galaxyhelpers.DefaultInstallWorkers(runtime.GOMAXPROCS(0))
	rows := []workersEnvRow{
		{name: "a value inside the range is read", value: "1", want: 1},
		{name: "a declared but empty value reads the flag default", value: "", want: derived},
		{name: "zero is replaced with a warning", value: "0", want: derived, wantWarn: true},
		{name: "a negative value is replaced with a warning", value: "-1", want: derived, wantWarn: true},
		{name: "a value far above the ceiling is replaced with a warning", value: "1000000", want: derived, wantWarn: true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			neutralizeAnsibleDiscovery(t)
			t.Setenv("GO_GALAXY_WORKERS", row.value)

			cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
			if err != nil {
				t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
			}
			assertConfigField(t, "Workers", cfg.Workers, row.want)

			warned := workersWarning(cfg)
			if (warned != "") != row.wantWarn {
				t.Fatalf("warned about --workers = %v, want %v", warned != "", row.wantWarn)
			}
			if row.wantWarn && !strings.Contains(warned, row.value) {
				t.Fatalf("warning does not name the supplied value %q: %q", row.value, warned)
			}
		})
	}

	// The cleanup shape, on the real flag set rather than a fixture: no
	// --workers flag is registered at all, so no source supplied a value and
	// nothing may be warned about one. Kept here rather than in
	// TestCleanupConfigSurface because what it pins belongs to this test's
	// subject - it is the only place in this package M-B is observable.
	t.Run("a command registering no workers flag warns about none", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)

		cfg, err := buildConfigFor(t, "cleanup", cliflags.S3Flags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if workersWarning(cfg) != "" {
			t.Fatalf("warned about --workers, want no such warning")
		}
	})
}
