package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v3"
)

// testServerFlagDefault is the --server flag default used by every
// newServerCmd-built command below; it stands in for
// cmd/go-galaxy/cliflags/const.go's real defaultServerURL and only needs to
// be distinguishable from the values under test.
const testServerFlagDefault = "https://default.example"

// newServerCmd builds a *cli.Command exposing the "server" and "token"
// flags exactly as cmd/go-galaxy/cliflags/flags.go defines them - server
// defaulting to testServerFlagDefault and sourced from GO_GALAXY_SERVER
// alone, token with no default and sourced from GO_GALAXY_TOKEN - so
// c.IsSet and c.String behave the same way they do for a real command.
//
// ANSIBLE_GALAXY_SERVER is deliberately not a flag source on either side.
// cliflags.collectionPathFlags leaves it off because a flag source would
// make it outrank server_list - urfave/cli cannot tell a CLI-set flag from
// an env-set one - so internal/galaxy/config reads it as the [galaxy] server
// key's env spelling instead. Adding it here would hand this fixture a
// precedence the real command does not have, and that precedence is exactly
// what the pairing tables below turn on.
func newServerCmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "server",
				Value:   testServerFlagDefault,
				Sources: cli.EnvVars("GO_GALAXY_SERVER"),
			},
			&cli.StringFlag{
				Name:    "token",
				Sources: cli.EnvVars("GO_GALAXY_TOKEN"),
			},
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

// runResolveServers seeds cfg.Server, cfg.AnsibleServerUsed, and
// cfg.AnsibleServerEnvUsed the way applyAnsibleConfig would have -
// resolveServers's documented precondition for the no-server_list case,
// which buildImplicitServer's own urlFromAnsibleConfig provenance now
// depends on - then runs resolveServers.
func runResolveServers(t *testing.T, c *cli.Command, ansCfg ansibleConfig) (*Config, error) {
	t.Helper()
	cfg := &Config{}
	serverValue, serverFromEnv := ansibleGalaxyServer(ansCfg.Galaxy.Server)
	cfg.Server, cfg.AnsibleServerUsed = pickConfigValue(c, "server", serverValue)
	cfg.AnsibleServerEnvUsed = cfg.AnsibleServerUsed && serverFromEnv
	err := resolveServers(cfg, c, ansCfg)
	return cfg, err
}

// TestResolveServersImplicit checks the legitimately single-server case:
// no server_list configured anywhere, so Servers holds exactly one
// anonymous ("" id, no token, certs verified) entry built from whatever
// pickConfigValue already resolved into cfg.Server.
func TestResolveServersImplicit(t *testing.T) {
	t.Parallel()

	c := newServerCmd(t, nil)
	cfg, err := runResolveServers(t, c, ansibleConfig{})
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}

	want := []Server{{URL: "https://default.example"}}
	if !reflect.DeepEqual(cfg.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
	}
	if cfg.Server != cfg.Servers[0].URL {
		t.Errorf("Server = %q, want it to equal Servers[0].URL = %q", cfg.Server, cfg.Servers[0].URL)
	}
}

// TestResolveServersUnregisteredServerFlag checks the BuildCollectionConfig
// contract this package documents on Config.Server: a command that does not
// register the "server" flag at all (cleanup, today) reads it back as the
// Go zero value and never consults Config.Server or Config.Servers, so
// resolveServers must not fail its config build over an empty server URL -
// only a real, flag-registering command's non-empty default reaches
// buildImplicitServer in practice.
func TestResolveServersUnregisteredServerFlag(t *testing.T) {
	t.Parallel()

	// No "server" flag registered at all, mirroring cleanup's command tree.
	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"go-galaxy"}); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}

	cfg, err := runResolveServers(t, captured, ansibleConfig{})
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].URL != "" {
		t.Errorf("Servers = %+v, want a single entry with an empty URL", cfg.Servers)
	}
	if cfg.Server != "" {
		t.Errorf("Server = %q, want empty", cfg.Server)
	}
}

// TestResolveServersAnsibleServerFallback checks precedence rule 3: with no
// server_list and no explicit --server, [galaxy] server (already folded
// into cfg.Server by pickConfigValue before resolveServers runs) becomes
// the sole server.
func TestResolveServersAnsibleServerFallback(t *testing.T) {
	t.Parallel()

	c := newServerCmd(t, nil)
	ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://ansible.example"}}
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}

	want := []Server{{URL: "https://ansible.example", urlFromAnsibleConfig: true}}
	if !reflect.DeepEqual(cfg.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
	}
}

// singleServerList returns an ansibleConfig with one [galaxy_server.<id>]
// section (id "prod") and it listed in server_list, plus a "distractor"
// [galaxy] server value that a broken precedence chain would wrongly pick
// instead.
func singleServerList(kv map[string]string) ansibleConfig {
	return ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{Server: "https://distractor.example", ServerList: "prod"},
		GalaxyServers: map[string]map[string]string{"prod": kv},
	}
}

// TestResolveServersListBeatsAnsibleServer checks precedence rule 2: a
// non-empty server_list wins over [galaxy] server.
func TestResolveServersListBeatsAnsibleServer(t *testing.T) {
	t.Parallel()

	c := newServerCmd(t, nil)
	ansCfg := singleServerList(map[string]string{"url": "https://prod.example"})
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}

	// tokenFromAnsibleConfig is true even though no token is configured at
	// all: buildServer derives it from envOrIni's "did the environment
	// supply this" bool, which is false whether the ini value is a real
	// token or the empty string an absent key resolves to. That is safe
	// because tokenPairingOffense's own skip guard is additionally gated
	// on Token.IsSet(), so an unset token is never paired with anything.
	want := []Server{{ID: "prod", URL: "https://prod.example", urlFromAnsibleConfig: true, tokenFromAnsibleConfig: true}}
	if !reflect.DeepEqual(cfg.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
	}
	if cfg.Server != "https://prod.example" {
		t.Errorf("Server = %q, want %q", cfg.Server, "https://prod.example")
	}
}

// TestResolveServersExplicitByID checks precedence rule 1's id-match branch:
// --server naming a configured id selects that one server, with its own
// token and TLS setting, even though the list has other entries.
func TestResolveServersExplicitByID(t *testing.T) {
	t.Parallel()

	ansCfg := ansibleConfig{
		Galaxy: ansibleGalaxyConfig{ServerList: "prod,staging"},
		GalaxyServers: map[string]map[string]string{
			"prod":    {"url": "https://prod.example", "token": "prod-token", "validate_certs": "false"},
			"staging": {"url": "https://staging.example"},
		},
	}
	c := newServerCmd(t, []string{"--server=prod"})
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}

	if len(cfg.Servers) != 1 {
		t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
	}
	got := cfg.Servers[0]
	if got.ID != "prod" || got.URL != "https://prod.example" {
		t.Errorf("Servers[0] = {ID: %q, URL: %q}, want {ID: %q, URL: %q}", got.ID, got.URL, "prod", "https://prod.example")
	}
	if !got.Token.IsSet() || got.Token.Reveal() != "prod-token" {
		t.Errorf("Servers[0].Token.Reveal() = %q, want %q", got.Token.Reveal(), "prod-token")
	}
	if !got.InsecureSkipTLSVerify {
		t.Error("Servers[0].InsecureSkipTLSVerify = false, want true (validate_certs = false)")
	}
}

// TestResolveServersExplicitURLIgnoresList checks precedence rule 1's
// anonymous-URL branch: a --server value that names no configured id is
// used verbatim, and server_list - including a section broken enough to
// fail its own validation - is never even consulted.
func TestResolveServersExplicitURLIgnoresList(t *testing.T) {
	t.Parallel()

	ansCfg := ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "prod"},
		GalaxyServers: map[string]map[string]string{"prod": {"username": "not-supported"}}, // would hard-error if built
	}
	c := newServerCmd(t, []string{"--server=https://anon.example"})
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil (list must be ignored entirely)", err)
	}

	want := []Server{{URL: "https://anon.example"}}
	if !reflect.DeepEqual(cfg.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", cfg.Servers, want)
	}
}

// TestResolveServerListEnvBeatsIni checks that ANSIBLE_GALAXY_SERVER_LIST,
// when set at all, wins outright over [galaxy] server_list - including
// when set to an empty/all-whitespace value, which then means "no
// server_list" rather than falling back to the ini value.
func TestResolveServerListEnvBeatsIni(t *testing.T) {
	t.Run("env overrides ini", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_LIST", "from-env")
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "from-ini"}}
		got := resolveServerList(ansCfg)
		want := []string{"from-env"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("resolveServerList() = %v, want %v", got, want)
		}
	})

	t.Run("env set to whitespace means unset, not fall back to ini", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_LIST", "   ")
		ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "from-ini"}}
		got := resolveServerList(ansCfg)
		if got != nil {
			t.Errorf("resolveServerList() = %v, want nil", got)
		}
	})
}

// TestBuildServerEnvBeatsIniPerKey checks that each of the three
// env-overridable keys (URL, TOKEN, VALIDATE_CERTS) is individually
// overridden by ANSIBLE_GALAXY_SERVER_<ID>_<KEY> when set, independent of
// the other two.
func TestBuildServerEnvBeatsIniPerKey(t *testing.T) {
	kv := map[string]string{"url": "https://ini.example", "token": "ini-token", "validate_certs": "true"}

	t.Run("url", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_PROD_URL", "https://env.example")
		server, _, err := buildServer("prod", kv)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.URL != "https://env.example" {
			t.Errorf("URL = %q, want %q", server.URL, "https://env.example")
		}
	})

	t.Run("token", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_PROD_TOKEN", "env-token")
		server, _, err := buildServer("prod", kv)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.Token.Reveal() != "env-token" {
			t.Errorf("Token.Reveal() = %q, want %q", server.Token.Reveal(), "env-token")
		}
	})

	t.Run("validate_certs", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_PROD_VALIDATE_CERTS", "false")
		server, _, err := buildServer("prod", kv)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if !server.InsecureSkipTLSVerify {
			t.Error("InsecureSkipTLSVerify = false, want true (env overrides ini's true with false)")
		}
	})

	t.Run("a dash in the id is not translated", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_MY-HUB_URL", "https://env.example")
		server, _, err := buildServer("my-hub", map[string]string{"url": "https://ini.example"})
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.URL != "https://env.example" {
			t.Errorf("URL = %q, want %q", server.URL, "https://env.example")
		}
	})
}

// TestBuildServerEnvIDIsUpperCased pins the case half of the
// ANSIBLE_GALAXY_SERVER_<ID>_<KEY> name, which envOrIni composes by
// upper-casing the id - ansible's own rule, since its config manager builds
// the same name from section.upper().
//
// It is read from both sides deliberately, and the second half is the
// load-bearing one. Setting the upper-cased variable and asserting it wins
// holds under either rule - upper-casing or no translation at all - so that
// half alone measures nothing about case. Only the negative half separates
// them, by pinning that the id as written names no variable this code reads.
// A predecessor of this test asserted the positive half alone while its own
// name claimed the opposite rule, and passed throughout.
func TestBuildServerEnvIDIsUpperCased(t *testing.T) {
	const (
		fromEnv = "https://env.example"
		fromIni = "https://ini.example"
	)
	ini := map[string]string{"url": fromIni}

	t.Run("the upper-cased name is read", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_MYHUB_URL", fromEnv)
		server, _, err := buildServer("myHub", ini)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.URL != fromEnv {
			t.Errorf("URL = %q, want %q", server.URL, fromEnv)
		}
	})

	t.Run("the as-written name is not read", func(t *testing.T) {
		t.Setenv("ANSIBLE_GALAXY_SERVER_myHub_URL", fromEnv)
		server, _, err := buildServer("myHub", ini)
		if err != nil {
			t.Fatalf("buildServer() error = %v, want nil", err)
		}
		if server.URL != fromIni {
			t.Errorf("URL = %q, want %q: the id as written must not name the variable", server.URL, fromIni)
		}
	})
}

// TestBuildServerMissingURL checks that a listed server with no url from
// either ini or env is a hard error.
func TestBuildServerMissingURL(t *testing.T) {
	t.Parallel()
	_, _, err := buildServer("prod", map[string]string{"token": "x"})
	if !errors.Is(err, helpers.ErrMissingGalaxyServerURL) {
		t.Errorf("error = %v, want helpers.ErrMissingGalaxyServerURL", err)
	}
}

// TestBuildServerHardErrorKeys checks that each of the four keys implying
// Basic auth or Keycloak/SSO token exchange is a hard error, naming the key.
func TestBuildServerHardErrorKeys(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"username", "password", "auth_url", "client_id"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			kv := map[string]string{"url": "https://x", key: "value"}
			_, _, err := buildServer("prod", kv)
			if !errors.Is(err, helpers.ErrUnsupportedGalaxyServerKey) {
				t.Errorf("error = %v, want helpers.ErrUnsupportedGalaxyServerKey", err)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %v, want it to name the key %q", err, key)
			}
		})
	}
}

// TestBuildServerNamesTheFirstHardErrorKeyInSortedOrder pins the ordering
// buildServer feeds checkHardErrorKeys. checkHardErrorKeys returns on the
// first hard-error key it meets, so with two of them in one section the key
// the operator is told about is decided entirely by that order: sorted, it
// is auth_url; collected straight off the map, it is whichever key Go's
// randomized iteration yielded first.
//
// Killing mutation: replacing sortedKeys' slices.Sorted(maps.Keys(kv)) with
// slices.Collect(maps.Keys(kv)) fails the sorted-order subtest under
// go test -race -count=1 with
//
//	error = unsupported galaxy_server key: configure a Galaxy API token instead: server "prod" key "password",
//	want it to name the first hard-error key in sorted order ("auth_url")
//
// while the positive-control subtest keeps passing, which is what makes the
// first subtest's assertion a claim about ordering rather than the vacuous
// "password is never named".
func TestBuildServerNamesTheFirstHardErrorKeyInSortedOrder(t *testing.T) {
	t.Parallel()

	t.Run("sorted order decides which of two hard-error keys is named", func(t *testing.T) {
		t.Parallel()
		kv := map[string]string{"url": "https://x", "password": "p", "auth_url": "https://a"}
		// Go randomizes map iteration order per range, so an unsorted
		// collection picks between the two hard-error keys afresh on every
		// call; repeating the call makes that a certain failure rather than
		// a coin flip.
		for range 64 {
			_, _, err := buildServer("prod", kv)
			if !errors.Is(err, helpers.ErrUnsupportedGalaxyServerKey) {
				t.Fatalf("error = %v, want helpers.ErrUnsupportedGalaxyServerKey", err)
			}
			if !strings.Contains(err.Error(), "auth_url") {
				t.Fatalf("error = %v, want it to name the first hard-error key in sorted order (%q)", err, "auth_url")
			}
		}
	})

	t.Run("positive control: the sole hard-error key is named", func(t *testing.T) {
		t.Parallel()
		kv := map[string]string{"url": "https://x", "password": "p"}
		_, _, err := buildServer("prod", kv)
		if !errors.Is(err, helpers.ErrUnsupportedGalaxyServerKey) {
			t.Fatalf("error = %v, want helpers.ErrUnsupportedGalaxyServerKey", err)
		}
		if !strings.Contains(err.Error(), "password") {
			t.Fatalf("error = %v, want it to name %q", err, "password")
		}
	})
}

// TestBuildServerAPIVersion checks that api_version = "v3" is accepted as a
// no-op and any other value is a hard error.
func TestBuildServerAPIVersion(t *testing.T) {
	t.Parallel()

	t.Run("v3 accepted", func(t *testing.T) {
		t.Parallel()
		_, _, err := buildServer("prod", map[string]string{"url": "https://x", "api_version": "v3"})
		if err != nil {
			t.Errorf("buildServer() error = %v, want nil", err)
		}
	})

	t.Run("other value errors", func(t *testing.T) {
		t.Parallel()
		_, _, err := buildServer("prod", map[string]string{"url": "https://x", "api_version": "v2"})
		if !errors.Is(err, helpers.ErrUnsupportedGalaxyServerAPIVersion) {
			t.Errorf("error = %v, want helpers.ErrUnsupportedGalaxyServerAPIVersion", err)
		}
	})
}

// TestBuildServerUnknownKeyWarns checks that an unrecognized key warns
// (naming the key and the section) rather than failing, and does not
// prevent the server from being built.
func TestBuildServerUnknownKeyWarns(t *testing.T) {
	t.Parallel()

	server, warnings, err := buildServer("prod", map[string]string{"url": "https://x", "some_future_key": "y"})
	if err != nil {
		t.Fatalf("buildServer() error = %v, want nil", err)
	}
	if server.URL != "https://x" {
		t.Errorf("URL = %q, want %q", server.URL, "https://x")
	}
	if len(warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1 (warnings = %v)", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "some_future_key") || !strings.Contains(warnings[0], "prod") {
		t.Errorf("warnings[0] = %q, want it to mention both the key and the server id", warnings[0])
	}
}

// TestBuildServerValidateCerts checks every ansible boolean spelling for
// validate_certs, case-insensitively, and that an unparseable value is
// always a hard error rather than a silent false.
func TestBuildServerValidateCerts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          string
		wantInsecure bool
		wantErr      bool
	}{
		{name: "true", raw: "true", wantInsecure: false},
		{name: "True mixed case", raw: "True", wantInsecure: false},
		{name: "false", raw: "false", wantInsecure: true},
		{name: "False mixed case", raw: "False", wantInsecure: true},
		{name: "yes", raw: "yes", wantInsecure: false},
		{name: "no", raw: "no", wantInsecure: true},
		{name: "on", raw: "on", wantInsecure: false},
		{name: "off", raw: "off", wantInsecure: true},
		{name: "1", raw: "1", wantInsecure: false},
		{name: "0", raw: "0", wantInsecure: true},
		{name: "unset defaults to certs verified", raw: "", wantInsecure: false},
		{name: "unparseable is an error", raw: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kv := map[string]string{"url": "https://x"}
			if tt.raw != "" {
				kv["validate_certs"] = tt.raw
			}
			server, _, err := buildServer("prod", kv)
			if tt.wantErr {
				if !errors.Is(err, helpers.ErrInvalidValidateCerts) {
					t.Errorf("error = %v, want helpers.ErrInvalidValidateCerts", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildServer() error = %v, want nil", err)
			}
			if server.InsecureSkipTLSVerify != tt.wantInsecure {
				t.Errorf("InsecureSkipTLSVerify = %v, want %v", server.InsecureSkipTLSVerify, tt.wantInsecure)
			}
		})
	}
}

// TestValidateServerIDs checks the id charset, plain duplicate, and
// case-collision invariants.
func TestValidateServerIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr error
		name    string
		ids     []string
	}{
		{name: "valid charset", ids: []string{"prod-1", "staging_2"}},
		{name: "dot is rejected", ids: []string{"bad.id"}, wantErr: helpers.ErrInvalidGalaxyServerID},
		{name: "space is rejected", ids: []string{"bad id"}, wantErr: helpers.ErrInvalidGalaxyServerID},
		{name: "exact duplicate is rejected", ids: []string{"prod", "prod"}, wantErr: helpers.ErrDuplicateGalaxyServerID},
		{name: "case-colliding ids are rejected", ids: []string{"Prod", "prod"}, wantErr: helpers.ErrDuplicateGalaxyServerID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateServerIDs(tt.ids)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("validateServerIDs() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("validateServerIDs() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestResolveServersInvalidIDPropagates checks the same id-charset
// invariant end to end, through resolveServers's server_list branch.
func TestResolveServersInvalidIDPropagates(t *testing.T) {
	t.Parallel()
	c := newServerCmd(t, nil)
	ansCfg := ansibleConfig{Galaxy: ansibleGalaxyConfig{ServerList: "bad.id"}}
	_, err := runResolveServers(t, c, ansCfg)
	if !errors.Is(err, helpers.ErrInvalidGalaxyServerID) {
		t.Errorf("resolveServers() error = %v, want helpers.ErrInvalidGalaxyServerID", err)
	}
}

// TestBuildServerUserinfoRejected checks that a server URL embedding
// userinfo is a config error, both for a named server and (via
// buildImplicitServer) the implicit single-server case.
func TestBuildServerUserinfoRejected(t *testing.T) {
	t.Parallel()

	t.Run("named server", func(t *testing.T) {
		t.Parallel()
		// #nosec G101 -- test fixture literal, not a real credential
		_, _, err := buildServer("prod", map[string]string{"url": "https://user:pass@hub.example/"})
		if !errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) {
			t.Errorf("error = %v, want helpers.ErrGalaxyServerURLUserinfo", err)
		}
		if strings.Contains(err.Error(), "pass") {
			t.Errorf("error = %v, must not echo the password", err)
		}
	})

	t.Run("implicit single server", func(t *testing.T) {
		t.Parallel()
		// #nosec G101 -- test fixture literal, not a real credential
		_, err := buildImplicitServer("https://user:pass@hub.example/", false)
		if !errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) {
			t.Errorf("error = %v, want helpers.ErrGalaxyServerURLUserinfo", err)
		}
		if strings.Contains(err.Error(), "pass") {
			t.Errorf("error = %v, must not echo the password", err)
		}
	})
}

// TestBuildServerInsecureTokenTransport checks that a token configured for
// a plaintext (http) origin is rejected unless that origin is loopback.
func TestBuildServerInsecureTokenTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "plaintext non-loopback is rejected", url: "http://hub.example", wantErr: true},
		{name: "plaintext localhost is allowed", url: "http://localhost:8080"},
		{name: "plaintext 127.0.0.1 is allowed", url: "http://127.0.0.1:8080"},
		{name: "plaintext 127.9.9.9 (127.0.0.0/8) is allowed", url: "http://127.9.9.9:8080"},
		{name: "plaintext ::1 is allowed", url: "http://[::1]:8080"},
		{name: "https non-loopback is allowed", url: "https://hub.example"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := buildServer("prod", map[string]string{"url": tt.url, "token": "tok"})
			if tt.wantErr {
				if !errors.Is(err, helpers.ErrInsecureTokenTransport) {
					t.Errorf("error = %v, want helpers.ErrInsecureTokenTransport", err)
				}
				return
			}
			if err != nil {
				t.Errorf("buildServer() error = %v, want nil", err)
			}
		})
	}
}

// TestResolveServersOriginConflicts checks both same-origin conflict
// errors: two servers sharing an origin must agree on validate_certs and
// on their token.
func TestResolveServersOriginConflicts(t *testing.T) {
	t.Parallel()

	t.Run("conflicting validate_certs", func(t *testing.T) {
		t.Parallel()
		ansCfg := twoServerAnsCfg(
			map[string]string{"url": "https://hub.example", "validate_certs": "false"},
			map[string]string{"url": "https://hub.example:443", "validate_certs": "true"},
		)
		_, err := runResolveServers(t, newServerCmd(t, nil), ansCfg)
		if !errors.Is(err, helpers.ErrConflictingServerTLSPolicy) {
			t.Errorf("error = %v, want helpers.ErrConflictingServerTLSPolicy", err)
		}
	})

	t.Run("conflicting token", func(t *testing.T) {
		t.Parallel()
		ansCfg := twoServerAnsCfg(
			map[string]string{"url": "https://hub.example", "token": "tok-1"},
			map[string]string{"url": "https://hub.example", "token": "tok-2"},
		)
		_, err := runResolveServers(t, newServerCmd(t, nil), ansCfg)
		if !errors.Is(err, helpers.ErrConflictingServerToken) {
			t.Errorf("error = %v, want helpers.ErrConflictingServerToken", err)
		}
	})

	t.Run("same origin, one token set and one unset, also conflicts", func(t *testing.T) {
		t.Parallel()
		ansCfg := twoServerAnsCfg(
			map[string]string{"url": "https://hub.example", "token": "tok-1"},
			map[string]string{"url": "https://hub.example"},
		)
		_, err := runResolveServers(t, newServerCmd(t, nil), ansCfg)
		if !errors.Is(err, helpers.ErrConflictingServerToken) {
			t.Errorf("error = %v, want helpers.ErrConflictingServerToken", err)
		}
	})
}

// twoServerAnsCfg returns an ansibleConfig listing two servers, "a" and
// "b", with the given per-section key/value maps - the shared fixture for
// TestResolveServersOriginConflicts and TestResolveServersOriginNoConflict.
func twoServerAnsCfg(a, b map[string]string) ansibleConfig {
	return ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "a,b"},
		GalaxyServers: map[string]map[string]string{"a": a, "b": b},
	}
}

// TestResolveServersOriginNoConflict checks the negative case for
// checkOriginConflicts: two servers legitimately sharing an origin (an
// implicit vs. explicit default port) with matching validate_certs and
// token settings are not a conflict.
func TestResolveServersOriginNoConflict(t *testing.T) {
	t.Parallel()

	ansCfg := twoServerAnsCfg(
		map[string]string{"url": "https://hub.example", "token": "same-token"},
		map[string]string{"url": "https://hub.example:443", "token": "same-token"},
	)
	cfg, err := runResolveServers(t, newServerCmd(t, nil), ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("len(Servers) = %d, want 2", len(cfg.Servers))
	}
}

// TestResolveServersTLSWarnings checks the two exact warning texts:
// certificate verification disabled, and - only when that server also
// carries a token - a token sent over that unverified connection.
func TestResolveServersTLSWarnings(t *testing.T) {
	t.Parallel()

	t.Run("insecure without token: one warning", func(t *testing.T) {
		t.Parallel()
		ansCfg := singleServerList(map[string]string{"url": "https://hub.example", "validate_certs": "false"})
		c := newServerCmd(t, nil)
		cfg, err := runResolveServers(t, c, ansCfg)
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		want := fmt.Sprintf(
			"TLS certificate verification is DISABLED for Galaxy server %q (%s); "+
				"this run cannot detect a man-in-the-middle on that host", "prod", "https://hub.example:443")
		if len(cfg.Warnings) != 1 {
			t.Fatalf("len(Warnings) = %d, want 1 (Warnings = %v)", len(cfg.Warnings), cfg.Warnings)
		}
		if cfg.Warnings[0] != want {
			t.Errorf("Warnings[0] = %q, want %q", cfg.Warnings[0], want)
		}
	})

	t.Run("insecure with token: two warnings", func(t *testing.T) {
		t.Parallel()
		// url, validate_certs, and token are all file-sourced here (file +
		// file + file), so TestTokenTLSPolicyPairing's refusal never applies
		// to this row: tokenPairingOffense exempts a token that came from the
		// same section as everything else. Do not "simplify" this fixture
		// into an env-sourced token - that would turn it into the refused
		// shape and silently delete this test's only coverage of the second
		// warning.
		ansCfg := singleServerList(map[string]string{
			"url": "https://hub.example", "validate_certs": "false", "token": "tok",
		})
		c := newServerCmd(t, nil)
		cfg, err := runResolveServers(t, c, ansCfg)
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		wantTLS := fmt.Sprintf(
			"TLS certificate verification is DISABLED for Galaxy server %q (%s); "+
				"this run cannot detect a man-in-the-middle on that host", "prod", "https://hub.example:443")
		wantToken := fmt.Sprintf(
			"A Galaxy API token is being sent to server %q (%s) over a connection whose certificate is not "+
				"verified; the token can be captured by an on-path attacker", "prod", "https://hub.example:443")
		if len(cfg.Warnings) != 2 {
			t.Fatalf("len(Warnings) = %d, want 2 (Warnings = %v)", len(cfg.Warnings), cfg.Warnings)
		}
		if cfg.Warnings[0] != wantTLS {
			t.Errorf("Warnings[0] = %q, want %q", cfg.Warnings[0], wantTLS)
		}
		if cfg.Warnings[1] != wantToken {
			t.Errorf("Warnings[1] = %q, want %q", cfg.Warnings[1], wantToken)
		}
	})

	t.Run("verified TLS: no warnings", func(t *testing.T) {
		t.Parallel()
		ansCfg := singleServerList(map[string]string{"url": "https://hub.example", "token": "tok"})
		c := newServerCmd(t, nil)
		cfg, err := runResolveServers(t, c, ansCfg)
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none", cfg.Warnings)
		}
	})
}

// secretRedactionPlaintext is the fixture token shared by every
// TestSecretRedaction* function below: never expected to appear in any of
// Secret's redacted renderings.
const secretRedactionPlaintext = "super-secret-token-xyz"

// TestSecretRedactionFmtVerbs checks that every fmt verb fmt routes through
// Stringer or GoStringer (%v %s %q %x %X %+v %#v) never renders a Secret's
// plaintext.
func TestSecretRedactionFmtVerbs(t *testing.T) {
	t.Parallel()
	set := NewSecret(secretRedactionPlaintext)

	// %x and %X route the Stringer's output through fmt's string-verb rule
	// too, but then render it as a hex dump rather than the literal text -
	// fmt.Sprintf("%x", "abc") is "616263", not "abc" - so they cannot be
	// expected to contain the literal word "REDACTED"; hexRedacted is what
	// hex-encoding "[REDACTED]" actually looks like, verifying the hex verbs
	// still went through the redacted placeholder rather than the plaintext.
	hexRedacted := fmt.Sprintf("%x", "[REDACTED]")
	fmtChecks := []struct {
		verb        string
		got         string
		wantLiteral string // "" for the hex verbs, which are checked separately below
	}{
		{"%v", fmt.Sprintf("%v", set), "REDACTED"},
		{"%s", set.String(), "REDACTED"},
		{"%q", fmt.Sprintf("%q", set), "REDACTED"},
		{"%x", fmt.Sprintf("%x", set), ""},
		{"%X", fmt.Sprintf("%X", set), ""},
		{"%+v", fmt.Sprintf("%+v", set), "REDACTED"},
		{"%#v", fmt.Sprintf("%#v", set), "REDACTED"},
	}
	for _, c := range fmtChecks {
		t.Run(c.verb, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(c.got, secretRedactionPlaintext) {
				t.Errorf("%s rendered the plaintext: %q", c.verb, c.got)
			}
			if c.wantLiteral != "" && !strings.Contains(c.got, c.wantLiteral) {
				t.Errorf("%s = %q, want it to mention %s", c.verb, c.got, c.wantLiteral)
			}
		})
	}
	t.Run("%x is the hex encoding of the redacted placeholder", func(t *testing.T) {
		t.Parallel()
		if got := fmt.Sprintf("%x", set); got != hexRedacted {
			t.Errorf("%%x = %q, want %q", got, hexRedacted)
		}
	})
	t.Run("%X is the uppercase hex encoding of the redacted placeholder", func(t *testing.T) {
		t.Parallel()
		if got, want := fmt.Sprintf("%X", set), strings.ToUpper(hexRedacted); got != want {
			t.Errorf("%%X = %q, want %q", got, want)
		}
	})
}

// TestSecretRedactionUnsetAndReveal checks that a zero Secret always reads
// the unset placeholder, and that Reveal is the one way back to the
// plaintext.
func TestSecretRedactionUnsetAndReveal(t *testing.T) {
	t.Parallel()
	const wantUnset = "[unset]"

	var unset Secret
	if unset.IsSet() {
		t.Error("zero Secret.IsSet() = true, want false")
	}
	if got := fmt.Sprintf("%v", unset); got != wantUnset {
		t.Errorf("zero Secret %%v = %q, want %q", got, wantUnset)
	}
	if got := fmt.Sprintf("%#v", unset); got != wantUnset {
		t.Errorf("zero Secret %%#v = %q, want %q", got, wantUnset)
	}

	set := NewSecret(secretRedactionPlaintext)
	if got := set.Reveal(); got != secretRedactionPlaintext {
		t.Errorf("Reveal() = %q, want %q", got, secretRedactionPlaintext)
	}
}

// TestSecretRedactionMarshal checks that json.Marshal and yaml.Marshal both
// render the redacted placeholder rather than the plaintext, standalone and
// nested inside a Config.
func TestSecretRedactionMarshal(t *testing.T) {
	t.Parallel()
	set := NewSecret(secretRedactionPlaintext)

	t.Run("json.Marshal, bare", func(t *testing.T) {
		t.Parallel()
		b, err := json.Marshal(set)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("json.Marshal() = %s, must not contain the plaintext", b)
		}
		if string(b) != `"[REDACTED]"` {
			t.Errorf("json.Marshal() = %s, want %q", b, `"[REDACTED]"`)
		}
	})

	t.Run("yaml.Marshal, bare", func(t *testing.T) {
		t.Parallel()
		b, err := yaml.Marshal(set)
		if err != nil {
			t.Fatalf("yaml.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("yaml.Marshal() = %s, must not contain the plaintext", b)
		}
		if !strings.Contains(string(b), "REDACTED") {
			t.Errorf("yaml.Marshal() = %s, want it to mention REDACTED", b)
		}
	})
}

// TestSecretRedactionMarshalNestedInConfig checks the same json.Marshal and
// yaml.Marshal redaction as TestSecretRedactionMarshal, but with the Secret
// nested inside a Config the way it appears in real runtime state, rather
// than marshaled bare.
func TestSecretRedactionMarshalNestedInConfig(t *testing.T) {
	t.Parallel()
	cfg := Config{Servers: []Server{{ID: "prod", Token: NewSecret(secretRedactionPlaintext)}}}

	t.Run("json.Marshal", func(t *testing.T) {
		t.Parallel()
		// Config and Server carry no json tags: they are runtime types, not a
		// serialization contract (that lives in internal/galaxy/lockfile's own
		// tagged types). This marshal only exercises that Secret's redaction
		// survives generic reflection-based marshaling when nested.
		b, err := json.Marshal(cfg) //nolint:musttag
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("json.Marshal() = %s, must not contain the plaintext", b)
		}
	})

	t.Run("yaml.Marshal", func(t *testing.T) {
		t.Parallel()
		b, err := yaml.Marshal(cfg) //nolint:musttag // see the json.Marshal case above
		if err != nil {
			t.Fatalf("yaml.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("yaml.Marshal() = %s, must not contain the plaintext", b)
		}
	})
}

// TestSecretRedactionDumpNestedInPointerConfig goes one level deeper still
// than TestSecretRedactionMarshalNestedInConfig: a *config.Config (the shape
// every real call site actually holds and could plausibly hand to a debug
// print or a panic value, never a bare Config) carrying a token nested two
// levels down (Config -> Servers -> Server -> Secret), rendered through
// every verb/marshaler a careless "%v of the whole config" debug line or
// crash dump could reach for. Every one of them must redact.
func TestSecretRedactionDumpNestedInPointerConfig(t *testing.T) {
	t.Parallel()
	cfg := &Config{Servers: []Server{{ID: "prod", URL: "https://hub.example", Token: NewSecret(secretRedactionPlaintext)}}}

	fmtChecks := []struct {
		verb string
		got  string
	}{
		{"%v", fmt.Sprintf("%v", cfg)},
		{"%+v", fmt.Sprintf("%+v", cfg)},
		{"%#v", fmt.Sprintf("%#v", cfg)},
	}
	for _, c := range fmtChecks {
		t.Run(c.verb, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(c.got, secretRedactionPlaintext) {
				t.Errorf("%s of *Config rendered the plaintext: %q", c.verb, c.got)
			}
		})
	}

	t.Run("json.Marshal", func(t *testing.T) {
		t.Parallel()
		b, err := json.Marshal(cfg) //nolint:musttag // see TestSecretRedactionMarshalNestedInConfig
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("json.Marshal() = %s, must not contain the plaintext", b)
		}
	})

	t.Run("yaml.Marshal", func(t *testing.T) {
		t.Parallel()
		b, err := yaml.Marshal(cfg) //nolint:musttag // see TestSecretRedactionMarshalNestedInConfig
		if err != nil {
			t.Fatalf("yaml.Marshal() error = %v, want nil", err)
		}
		if strings.Contains(string(b), secretRedactionPlaintext) {
			t.Errorf("yaml.Marshal() = %s, must not contain the plaintext", b)
		}
	})
}

// TestTokenFlagCases covers what the --token flag does to the resolved server
// list: which shapes hand a credential to a server, which are refused, and
// which leave an already-configured token alone. Each row carries its own
// assertions, since a refusal and a resolved *Config have nothing in common to
// state as one set of expected fields.
//
// Neither this test nor its subtests call t.Parallel(): several rows set an
// ANSIBLE_GALAXY_SERVER_<ID>_URL override via tc.env, and t.Setenv panics on
// a parallel test or one with a parallel ancestor.
func TestTokenFlagCases(t *testing.T) {
	for _, tc := range tokenFlagCases() {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := newServerCmd(t, tc.args)
			cfg, err := runResolveServers(t, c, tc.ansCfg)
			tc.check(t, cfg, err)
		})
	}
}

// tokenFlagCase is one row of TestTokenFlagCases: the command line the row
// runs, the ansible.cfg and environment it resolves against, and the row's
// own assertions over whatever resolveServers returned.
type tokenFlagCase struct {
	check  func(t *testing.T, cfg *Config, err error)
	env    map[string]string
	ansCfg ansibleConfig
	name   string
	args   []string
}

// hubWithINIToken is the ansible.cfg the rows about an already-configured
// credential resolve against: one server_list entry whose section carries a
// token, so the flag has something to override, to clear, or to leave alone.
func hubWithINIToken() ansibleConfig {
	return ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "hub"},
		GalaxyServers: map[string]map[string]string{"hub": {"url": "https://hub.example.com", "token": "from-ini"}},
	}
}

// tokenFlagCases enumerates what --token does to the resolved server list: the
// shapes that hand over, override or clear a credential, every shape that is
// refused outright, and the one that leaves a configured token untouched. The
// plaintext transport rule owns two rows rather than one - the non-loopback
// refusal and the loopback exception - because a single row asserting both
// outcomes hides the second behind whichever of the two fails first. Each
// row's assertions sit in their own named function below rather than in a
// closure here, so one row's branches are not counted against the whole table.
func tokenFlagCases() []tokenFlagCase {
	return []tokenFlagCase{
		{
			// checks the case --token exists for: one effective server,
			// credential handed to it on the command line rather than through
			// an ansible.cfg section.
			name:  "applies to single server",
			args:  []string{"--server=https://hub.example.com", "--token=s3cr3t"},
			check: checkTokenAppliesToSingleServer,
		},
		{
			// checks precedence when the url is the operator's own: an
			// explicit command-line or environment token still wins over one
			// configured in ansible.cfg, the same direction every other
			// explicitly-set value takes. The url is sourced from
			// ANSIBLE_GALAXY_SERVER_HUB_URL rather than from the
			// [galaxy_server.hub] section, which is what keeps this row out
			// of the token-destination pairing refusal covered below: that
			// rule only fires when the url itself is file-sourced.
			name:   "overrides configured token when the url is the operator's own",
			args:   []string{"--token=from-flag"},
			ansCfg: hubWithINIToken(),
			env:    map[string]string{"ANSIBLE_GALAXY_SERVER_HUB_URL": "https://hub.example.com"},
			check:  checkTokenOverridesConfiguredToken,
		},
		{
			// the identical override attempted against the url the file
			// itself supplies is refused instead: tokenPairingOffense
			// treats a section's own token as authorizing only itself, never
			// one from an operator channel layered on top of it. It differs
			// from the row above by exactly one thing: the env url that moved
			// that row's destination onto the operator's channel is gone.
			name:   "refused: an operator token does not authorize itself against a file-sourced url",
			args:   []string{"--token=from-flag"},
			ansCfg: hubWithINIToken(),
			check:  checkTokenRefusedAgainstFileSourcedURL,
		},
		{
			// checks that exporting GO_GALAXY_TOKEN= forces an anonymous run
			// without editing any config, which is the point of treating an
			// explicitly empty value as "no token" rather than as "unset".
			name:   "clears when explicitly empty",
			args:   []string{"--token="},
			ansCfg: hubWithINIToken(),
			check:  checkTokenClearsWhenExplicitlyEmpty,
		},
		{
			// checks the ambiguity refusal: with two servers in effect the flag
			// names neither, and guessing could send a private hub's credential
			// to the public Galaxy.
			name: "rejected with multiple servers",
			args: []string{"--token=s3cr3t"},
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "hub,public"},
				GalaxyServers: map[string]map[string]string{
					"hub":    {"url": "https://hub.example.com"},
					"public": {"url": "https://galaxy.ansible.com"},
				},
			},
			check: checkTokenRejectedWithMultipleServers,
		},
		{
			// checks that --token is held to the same transport rule as a
			// configured token: a credential must not be sent in the clear to
			// anything but loopback.
			name:  "rejected over plaintext non-loopback",
			args:  []string{"--server=http://hub.example.com", "--token=s3cr3t"},
			check: checkTokenRejectedOverPlaintextNonLoopback,
		},
		{
			// the loopback half of that same transport rule: 127.0.0.1 is the
			// one plaintext destination a credential may reach, so this shape
			// resolves rather than being refused.
			name:  "allowed over plaintext loopback",
			args:  []string{"--server=http://127.0.0.1:8080", "--token=s3cr3t"},
			check: checkTokenAllowedOverPlaintextLoopback,
		},
		{
			// checks that a command which never sets the flag - including one
			// that does not register it - changes nothing, so the flag cannot
			// silently strip a configured credential.
			name:   "unset leaves servers untouched",
			args:   nil,
			ansCfg: hubWithINIToken(),
			check:  checkTokenUnsetLeavesServersUntouched,
		},
	}
}

// checkTokenAppliesToSingleServer asserts the "applies to single server" row:
// resolution succeeds, yields exactly one server, and that server carries the
// flag's value.
func checkTokenAppliesToSingleServer(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
	}
	if got := cfg.Servers[0].Token.Reveal(); got != "s3cr3t" {
		t.Errorf("Servers[0].Token = %q, want %q", got, "s3cr3t")
	}
}

// checkTokenOverridesConfiguredToken asserts the "overrides configured token
// when the url is the operator's own" row: the resolved server carries the
// flag's value rather than the section's.
func checkTokenOverridesConfiguredToken(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if got := cfg.Servers[0].Token.Reveal(); got != "from-flag" {
		t.Errorf("Servers[0].Token = %q, want the flag value %q", got, "from-flag")
	}
}

// checkTokenRefusedAgainstFileSourcedURL asserts the "refused: an operator
// token does not authorize itself against a file-sourced url" row:
// resolveServers fails with helpers.ErrTokenDestinationFromAnsibleConfig,
// naming the "hub" server id.
func checkTokenRefusedAgainstFileSourcedURL(t *testing.T, _ *Config, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrTokenDestinationFromAnsibleConfig) {
		t.Fatalf("resolveServers() error = %v, want helpers.ErrTokenDestinationFromAnsibleConfig", err)
	}
	if !strings.Contains(err.Error(), `"hub"`) {
		t.Errorf("error = %v, want it to name server %q", err, "hub")
	}
}

// checkTokenClearsWhenExplicitlyEmpty asserts the "clears when explicitly
// empty" row: the resolved server ends up with no token at all.
func checkTokenClearsWhenExplicitlyEmpty(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if cfg.Servers[0].Token.IsSet() {
		t.Errorf("Servers[0].Token is set, want it cleared by an explicitly empty --token")
	}
}

// checkTokenRejectedWithMultipleServers asserts the "rejected with multiple
// servers" row: resolution fails with ErrAmbiguousGalaxyToken.
func checkTokenRejectedWithMultipleServers(t *testing.T, _ *Config, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrAmbiguousGalaxyToken) {
		t.Fatalf("resolveServers() error = %v, want ErrAmbiguousGalaxyToken", err)
	}
}

// checkTokenRejectedOverPlaintextNonLoopback asserts the "rejected over
// plaintext non-loopback" row: resolution fails with ErrInsecureTokenTransport.
func checkTokenRejectedOverPlaintextNonLoopback(t *testing.T, _ *Config, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrInsecureTokenTransport) {
		t.Fatalf("resolveServers() error = %v, want ErrInsecureTokenTransport", err)
	}
}

// checkTokenAllowedOverPlaintextLoopback asserts the "allowed over plaintext
// loopback" row: resolution succeeds.
func checkTokenAllowedOverPlaintextLoopback(t *testing.T, _ *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() over loopback error = %v, want nil", err)
	}
}

// checkTokenUnsetLeavesServersUntouched asserts the "unset leaves servers
// untouched" row: the resolved server still carries the configured token.
func checkTokenUnsetLeavesServersUntouched(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if got := cfg.Servers[0].Token.Reveal(); got != "from-ini" {
		t.Errorf("Servers[0].Token = %q, want the configured %q", got, "from-ini")
	}
}

// TestTokenDestinationPairing is the table for the destination arm of the
// credential pairing rule tokenPairingOffense enforces: a Galaxy token must
// never be paired with a server URL this run sourced from an ansible.cfg
// file rather than from the operator. checkTokenPairing is the walk that
// calls it. The rule's second arm - a token paired with a server whose
// certificate verification an ansible.cfg file disabled - has its own
// table, TestTokenTLSPolicyPairing, below; the two tables share the mutation
// methodology this doc comment describes but are kept separate rather than
// merged, since this one already runs to twelve rows with its own funlen
// three-group split.
//
// Six of these twelve rows sit beside a sibling that varies exactly one
// property, and in all three pairs that property is the url's provenance -
// so a passing refusal on each pair shows the pairing rule actually fired
// rather than merely that the fixture never reached the check: "refused:
// file server_list url with an env token" beside "accepted: adding an env
// url remedies the row above", "refused: file [galaxy] server with --token"
// beside "accepted: ANSIBLE_GALAXY_SERVER with --token", and "refused: an
// operator --token overrides a section token pairing" beside "accepted: an
// operator --token against an env url overrides a section token". The
// remaining two refusals have no such sibling: "refused: --server naming a
// server_list id with an env token" also adds --server=corp on top of the
// first pair's shape, and "refused: a later server_list entry offends, not
// the first" also grows the list from one entry to two, so neither differs
// from any accepted row by a single property alone. Their own fixtures are instead shown capable of acceptance
// by the "Inverting envOrIni's bool" mutation below, which turns every
// server_list-sourced refusal row - these two included - into
// "resolveServers() error = <nil>, want
// helpers.ErrTokenDestinationFromAnsibleConfig". A second, differently
// shaped mutation aimed at the identical purpose - hardcoding buildServer's
// urlFromAnsibleConfig field to false in place of !urlFromEnv - was also
// tried and is recorded below too, but it does not compile at all rather
// than failing a test, which is itself a stronger rejection than a test
// failure would have been: see that bullet for why.
//
// Neither this test nor its subtests call t.Parallel(): most rows set an
// ANSIBLE_GALAXY_SERVER_<ID>_URL or _TOKEN override via tc.env, and
// t.Setenv panics on a parallel test or one with a parallel ancestor.
//
// Every mutation below was run via go test -overlay against the whole
// internal/galaxy/config package, not just this table, so a kill in
// TestTokenFlagCases, in TestTokenTLSPolicyPairing, or in any other test in
// the package is caught too; each bullet's own "Scope:" sentence states
// exactly what it found beyond the rows named in the rest of that bullet.
// Several of those sentences reach TestTokenTLSPolicyPairing rows, because
// tokenPairingOffense's two arms share the one skip guard these mutations
// target. Two bullets are documentary rather than verified kills for the row
// each names first: the mutation leaves that row's own outcome unchanged,
// for a reason recorded alongside it rather than assumed - and the same
// bullet still records a real kill elsewhere.
//
//   - Deleting the "if s.urlFromAnsibleConfig" condition from
//     tokenPairingOffense, so every server that survives the skip guard
//     returns ErrTokenDestinationFromAnsibleConfig unconditionally and the
//     insecureFromAnsibleConfig arm below it is never reached at all,
//     leaves "section url + section token, nothing else", "env url +
//     section token", and "section url, no token anywhere" all still
//     passing: the first two both carry a file-sourced token, which alone
//     still satisfies the skip guard's tokenFromAnsibleConfig term
//     regardless of url provenance, and the third carries no token at all
//     (Token.IsSet() false), which alone still satisfies the guard's
//     Token.IsSet() term. It does kill three other rows of this table
//     instead - "accepted: adding an env url remedies the row above",
//     "accepted: ANSIBLE_GALAXY_SERVER with --token", and "accepted: an
//     operator --token against an env url overrides a section token" -
//     every row whose token is operator-sourced and whose url is ALSO
//     operator-sourced, since the destination sentinel is no longer
//     conditioned on url provenance at all:
//
//     resolveServers() error = galaxy server token destination came from ansible.cfg: server "" (https://opcorp.example:443), want nil
//
//     Scope: beyond this table, the identical mutation also fails three
//     TestTokenFlagCases rows whose token and url are both
//     operator-sourced, for the same reason, and seven
//     TestTokenTLSPolicyPairing rows: its four refused-for-TLS rows now
//     report the destination sentinel instead of the TLS one (a wrong
//     sentinel, not a missing error), and every one of its accepted rows
//     carrying an operator token - "accepted: an env VALIDATE_CERTS
//     override remedies the row above", "accepted: validate_certs = yes is
//     not a relaxation to pair against", and "accepted: the ordinary env
//     url + env token shape, no validate_certs" - is refused outright,
//     since the operator token alone is what this mutation now refuses,
//     regardless of the url's own operator-sourced provenance and
//     regardless of whether any validate_certs relaxation is in play. The
//     count is that predicate's, not a tally: the TLS table's three
//     remaining accepted rows carry a file-sourced token, no token, or an
//     explicitly emptied one, so the skip guard still exempts each.
//
//   - Deleting "s.tokenFromAnsibleConfig" from tokenPairingOffense's skip
//     guard (so a server is no longer exempted by a file-sourced token,
//     and is refused - on whichever of the two arms its own provenance
//     fields select - whenever Token.IsSet() alone holds) is the identical
//     code change this doc comment's own M6 mutation makes and reruns
//     below in the main MUTATIONS section; see there for the full real
//     output. In this table's own terms it turns "accepted: section url
//     with section token" into a refusal:
//
//     resolveServers() error = galaxy server token destination came from ansible.cfg: server "corp" (https://corp.example:443), want nil
//
//     Scope: the skip guard is shared with the TLS arm, so the reach runs
//     past this table: the identical mutation also fails one
//     TestTokenFlagCases row, one
//     TestTokenTLSPolicyPairing row ("accepted: section token pairs with
//     section validate_certs"), and three more config tests outright
//     (TestResolveServersOriginNoConflict,
//     TestResolveServersExplicitByID, and two subtests of
//     TestResolveServersTLSWarnings) whose fixtures configure a
//     file-sourced token alongside a file-sourced url or validate_certs
//     with no expectation of a pairing error at all.
//
//   - Deleting "!s.Token.IsSet() ||" from the same guard (so an unset token
//     is no longer a reason to skip) does not turn "section url, no token
//     anywhere" into a refusal: that row's server carries no ini token key
//     and no matching environment override, so tokenFromAnsibleConfig is
//     still true by the same envOrIni-derived default an absent key
//     resolves to (see the comment on
//     TestResolveServersListBeatsAnsibleServer's own want literal), which
//     alone still satisfies the mutated guard's remaining
//     tokenFromAnsibleConfig term - documentary, not a kill. It does kill
//     "accepted: an explicit empty --token forces anonymity" instead: that
//     row's --token= explicitly clears tokenFromAnsibleConfig to false
//     (applyTokenFlag), so once Token.IsSet() no longer shields it, the
//     guard's remaining term reads true:
//
//     resolveServers() error = galaxy server token destination came from ansible.cfg: server "corp" (https://corp.example:443), want nil
//
//     Scope: beyond this table, the identical mutation also fails one
//     TestTokenFlagCases row, TestTokenTLSPolicyPairing's own identically
//     named "accepted: an explicit empty --token forces anonymity" row,
//     and TestResolveServersAnsibleServerFallback outright, since that
//     test's implicit server carries no token at all and the mutated
//     guard no longer has a Token.IsSet() term to exempt it.
//
//   - Replacing resolveServerCandidates's buildImplicitServer(cfg.Server,
//     cfg.AnsibleServerUsed && !cfg.AnsibleServerEnvUsed) call with
//     buildImplicitServer(cfg.Server, false) turns "refused: file [galaxy]
//     server with --token" into an acceptance:
//
//     resolveServers() error = <nil>, want helpers.ErrTokenDestinationFromAnsibleConfig
//
//     Scope: this table's own outcome is the one row above, but it is not
//     the only test this mutation fails - TestResolveServersAnsibleServerFallback
//     also fails outright, since that test's own want literal pins
//     urlFromAnsibleConfig to true for a bare "[galaxy] server" value and
//     this mutation hardcodes the field false regardless. No
//     TestTokenFlagCases row, and no TestTokenTLSPolicyPairing row, is
//     affected.
//
//   - Deleting "&& !cfg.AnsibleServerEnvUsed" from that same call (so
//     $ANSIBLE_GALAXY_SERVER reads as file-sourced, exactly like the
//     ansible.cfg key it is the env spelling of) turns "accepted:
//     ANSIBLE_GALAXY_SERVER with --token" into a refusal:
//
//     resolveServers() error = galaxy server token destination came from ansible.cfg: server "" (https://opcorp.example:443), want nil
//
//     Scope: this table's own outcome is the one row above; no row of
//     TestTokenFlagCases, TestTokenTLSPolicyPairing, or any other test in
//     the package is affected - $ANSIBLE_GALAXY_SERVER never carries a
//     validate_certs value of its own, so the TLS arm has nothing here to
//     react to.
//
//   - Deleting "servers[0].tokenFromAnsibleConfig = false" from
//     applyTokenFlag turns "refused: an operator --token overrides a
//     section token pairing" into an acceptance:
//
//     resolveServers() error = <nil>, want helpers.ErrTokenDestinationFromAnsibleConfig
//
//     because the check then still reads servers[0]'s pre-override
//     tokenFromAnsibleConfig (true, from the section) rather than the
//     operator-channel value the override itself is supposed to record. It
//     leaves the file-[galaxy]-server and --server-by-id refusal rows
//     unaffected: buildImplicitServer never sets tokenFromAnsibleConfig at
//     all (the field is already false by construction there), and a row
//     with no --token flag never reaches this line to begin with.
//
//     Scope: beyond this table, the identical mutation also fails one
//     TestTokenFlagCases row and - the skip guard's exemption being shared
//     with the TLS arm - two TestTokenTLSPolicyPairing rows whose
//     fixtures override an unset section token via --token or
//     GO_GALAXY_TOKEN and expect the resulting operator token to be
//     refused against a file-sourced validate_certs.
//
//   - Inverting envOrIni's bool (returning the environment's value with
//     false, and the ini value with true - the opposite of what "the
//     environment supplied it" means) corrupts urlFromAnsibleConfig,
//     tokenFromAnsibleConfig, and now insecureFromAnsibleConfig everywhere
//     buildServer derives them, turning "refused: file server_list url
//     with an env token" into an acceptance:
//
//     resolveServers() error = <nil>, want helpers.ErrTokenDestinationFromAnsibleConfig
//
//     along with this table's other three refusal rows - "refused:
//     --server naming a server_list id with an env token", "refused: a
//     later server_list entry offends, not the first", and "refused: an
//     operator --token overrides a section token pairing" - and two of
//     this table's own acceptance rows, "accepted: env url with section
//     token" and "accepted: an operator --token against an env url
//     overrides a section token", both of which fail: resolveServers()
//     reports the pairing error in place of the nil these rows want,
//     naming server "corp" and origin https://corp-env.example:443.
//
//     What survives this mutation is not one row but a class: any pairing
//     whose two halves already share a single provenance is unaffected by
//     inverting both together, since they still agree afterward.
//     "accepted: adding an env url remedies the row above" (env+env, now
//     read as false+false, i.e. file+file) and "accepted: section url with
//     section token" (file+file, now read as true+true, i.e. env+env) are
//     both members of that class, and a pairing whose halves share one
//     provenance was always accepted regardless of which provenance that
//     is.
//
//     Scope: beyond this table, the identical mutation also fails two
//     TestTokenFlagCases rows - "overrides configured token when the url
//     is the operator's own" and "refused: an operator token does not
//     authorize itself against a file-sourced url" -
//     TestResolveServersListBeatsAnsibleServer outright, since that test's
//     want literal pins both provenance fields directly, and seven
//     TestTokenTLSPolicyPairing rows. Six are its own refusal and
//     acceptance rows breaking for the identical single-channel or
//     shared-channel reasons already argued above, this time on
//     insecureFromAnsibleConfig rather than urlFromAnsibleConfig or
//     tokenFromAnsibleConfig. The seventh is that table's own X1 row
//     ("refused (destination, not TLS): file url and file validate_certs,
//     env token"), which turns into an acceptance here for a variant of
//     the same class argument: its section supplies both url and
//     validate_certs together, envOrIni derives both provenance bits
//     through the identical mechanism, so inverting that mechanism flips
//     both from file-sourced to env-sourced together and they still agree
//     afterward - the pairing rule has nothing left to refuse.
//
//   - Hardcoding buildServer's urlFromAnsibleConfig field to false, in
//     place of !urlFromEnv, does not produce a test failure at all: it
//     fails to compile, because urlFromEnv - declared by "rawURL,
//     urlFromEnv := envOrIni(id, "URL", kv["url"])" - has no other use left
//     once the field literal stops reading it, and Go refuses an unused
//     local variable outright, naming that declaration in buildServer:
//
//     declared and not used: urlFromEnv
//
//     The compiler rejects the mutant before any test of this package ever
//     runs against it, which is a stronger form of rejection than a test
//     failure rather than a weaker one - so the bullet is worth recording,
//     but it cannot serve as one of the mutations this doc comment's own
//     second paragraph relies on for its "capable of acceptance" claim.
//     "Inverting envOrIni's bool" above is the one that carries that
//     claim.
//
//     Scope: this mutation does not build, so no test in this package or
//     any other ever runs against it.
//
//   - Moving the checkTokenPairing call in resolveServers above
//     applyTokenFlag (so it runs before an operator --token is folded into
//     servers[0]) turns both "refused: file [galaxy] server with --token"
//     and "refused: an operator --token overrides a section token
//     pairing" into acceptances:
//
//     resolveServers() error = <nil>, want helpers.ErrTokenDestinationFromAnsibleConfig
//
//     because the check then reads whatever token (or absence of one) the
//     server already carried before applyTokenFlag ever ran.
//
//     Scope: beyond this table, the identical mutation also fails one
//     TestTokenFlagCases row and two TestTokenTLSPolicyPairing rows -
//     "refused: --server by id, env url, env token, file validate_certs"
//     and "refused: env url + --token + file validate_certs" - both of
//     which override an otherwise-unset section token through
//     GO_GALAXY_TOKEN or --token and so read as carrying no token at all
//     when checkTokenPairing now runs first.
//
//   - Deleting both "!s.Token.IsSet() ||" and "s.tokenFromAnsibleConfig"
//     from tokenPairingOffense's skip guard (dropping its first "if"
//     entirely, so a server reaches the destination or TLS arm whether or
//     not a token was ever configured, and whether or not one that was
//     configured came from the file) is the only mutation in this set that
//     turns "accepted: section url with no token at all" into a refusal -
//     every mutation above leaves that row passing, since Token.IsSet()
//     and tokenFromAnsibleConfig each independently exempt it on their
//     own (see that row's own comment below). Reported at the shared
//     tc.check(t, cfg, err) call site every row's own check function
//     attributes its failure to, since each of those functions calls
//     t.Helper() itself, the same reason none of the quotes above name a
//     line:
//
//     resolveServers() error = galaxy server token destination came from ansible.cfg: server "corp" (https://corp.example:443), want nil
//
//     Scope: not measured within this table alone, since the guard it
//     deletes is shared with the TLS arm. It fails two
//     TestTokenFlagCases rows and three more TestTokenTLSPolicyPairing
//     acceptance rows for the identical reason - a server that was
//     exempted purely by carrying no token, or purely by carrying a
//     file-sourced one, is exempted no longer. Outside both tables it is
//     strictly wider than either single-term deletion above, which it has
//     to be, since it deletes both of their terms at once: it fails every
//     config test either one fails - TestResolveServersOriginNoConflict,
//     TestResolveServersExplicitByID, and TestResolveServersTLSWarnings
//     from the tokenFromAnsibleConfig deletion, TestResolveServersAnsibleServerFallback
//     from the Token.IsSet() one - plus
//     TestResolveServersListBeatsAnsibleServer, which neither fails alone,
//     and a third TestResolveServersTLSWarnings subtest the
//     tokenFromAnsibleConfig deletion leaves passing. It also reshapes rather
//     than merely repeats the destination table's own "refused: a later
//     server_list entry offends, not the first" row inside the TLS table:
//     with no skip guard left at all, that row's FIRST entry ("pub",
//     url-only, no token configured anywhere) now offends before the
//     second ("corp") is ever reached, so the failure names the wrong
//     server and the wrong sentinel at once -
//     "resolveServers() error = galaxy server token destination came from
//     ansible.cfg: server "pub" (https://pub.example:443), want
//     helpers.ErrTokenTLSPolicyFromAnsibleConfig" - rather than simply
//     failing to error at all.
func TestTokenDestinationPairing(t *testing.T) {
	for _, tc := range tokenDestinationCases() {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := newServerCmd(t, tc.args)
			cfg, err := runResolveServers(t, c, tc.ansCfg)
			tc.check(t, cfg, err)
		})
	}
}

// tokenDestinationCase is one row of TestTokenDestinationPairing: a full
// resolveServers run against its own ansible.cfg fixture, environment
// overrides, and command line, checked by its own assertions.
type tokenDestinationCase struct {
	check  func(t *testing.T, cfg *Config, err error)
	env    map[string]string
	ansCfg ansibleConfig
	name   string
	args   []string
}

// tokenDestinationRefused builds a tokenDestinationCase assertion for a
// refused pairing: resolveServers fails with
// helpers.ErrTokenDestinationFromAnsibleConfig, naming wantID and
// wantOrigin, and never the plaintext of wantToken.
//
// That last assertion is documentary rather than pinned: no fixture in this
// table can fail it while checkTokenPairing's format string takes only the
// sentinel, the server id, and the origin as operands - the token is never
// one - and Secret redacts every rendering one does reach. Two
// changes could make it fail, and a new row has to avoid the second: a
// production change handing the token to that format string, or a row whose
// token plaintext is itself a substring of the id or origin it asserts.
func tokenDestinationRefused(wantID, wantOrigin, wantToken string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, _ *Config, err error) {
		t.Helper()
		if !errors.Is(err, helpers.ErrTokenDestinationFromAnsibleConfig) {
			t.Fatalf("resolveServers() error = %v, want helpers.ErrTokenDestinationFromAnsibleConfig", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, fmt.Sprintf("%q", wantID)) {
			t.Errorf("error = %q, want it to name server %q", msg, wantID)
		}
		if !strings.Contains(msg, wantOrigin) {
			t.Errorf("error = %q, want it to name origin %q", msg, wantOrigin)
		}
		if strings.Contains(msg, wantToken) {
			t.Errorf("error = %q, must not contain the token plaintext %q", msg, wantToken)
		}
	}
}

// tokenDestinationRefusedNotTLS wraps tokenDestinationRefused with the one
// extra assertion that mirrors, from this table's side, what X1
// (TestTokenTLSPolicyPairing) asserts from the other: a destination refusal
// is never also readable as the TLS-policy sentinel. Used on exactly one row
// below, since the point is pinning that checkTokenPairing's two-sentinel
// split returns one or the other, never both - not re-asserting it on every
// row this table already has.
func tokenDestinationRefusedNotTLS(wantID, wantOrigin, wantToken string) func(t *testing.T, cfg *Config, err error) {
	inner := tokenDestinationRefused(wantID, wantOrigin, wantToken)
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		inner(t, cfg, err)
		if errors.Is(err, helpers.ErrTokenTLSPolicyFromAnsibleConfig) {
			t.Errorf("resolveServers() error = %v, must not also be helpers.ErrTokenTLSPolicyFromAnsibleConfig", err)
		}
	}
}

// tokenDestinationAccepted builds a tokenDestinationCase assertion for an
// accepted pairing: resolveServers succeeds, and the resolved single
// server's URL and revealed token equal wantURL and wantToken.
func tokenDestinationAccepted(wantURL, wantToken string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
		}
		if got := cfg.Servers[0].URL; got != wantURL {
			t.Errorf("Servers[0].URL = %q, want %q", got, wantURL)
		}
		if got := cfg.Servers[0].Token.Reveal(); got != wantToken {
			t.Errorf("Servers[0].Token.Reveal() = %q, want %q", got, wantToken)
		}
	}
}

// tokenDestinationAcceptedNoToken builds a tokenDestinationCase assertion
// for an accepted pairing whose token ends up cleared entirely: A8 (no
// token was ever configured) and A10 (an operator --token= forced anonymity
// against a file-sourced url, which is never refused) both resolve to a
// server carrying no token at all.
func tokenDestinationAcceptedNoToken(wantURL string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
		}
		if got := cfg.Servers[0].URL; got != wantURL {
			t.Errorf("Servers[0].URL = %q, want %q", got, wantURL)
		}
		if cfg.Servers[0].Token.IsSet() {
			t.Errorf("Servers[0].Token is set, want it cleared or never configured")
		}
	}
}

// tokenDestinationCases enumerates the pairing rule's twelve reachable
// shapes: a file-sourced url paired with an operator-sourced token is
// refused in every channel that can produce it (server_list plus a section,
// the bare [galaxy] server fallback, and --server naming a server_list id;
// position within a multi-entry server_list does not exempt a later entry
// either), while a file-sourced url paired with a file-sourced token, an
// operator-sourced url paired with any token, and a token forced to
// anonymous by an explicit empty --token are all accepted. The rows split
// across three producer functions of four rows each, purely to stay under
// funlen's line budget; the split carries no meaning of its own.
func tokenDestinationCases() []tokenDestinationCase {
	cases := tokenDestinationCasesGroupOne()
	cases = append(cases, tokenDestinationCasesGroupTwo()...)
	return append(cases, tokenDestinationCasesGroupThree()...)
}

// tokenDestinationCasesGroupOne is the first four rows of
// tokenDestinationCases: the leaking shape and its remedy (R1, A2), and the
// bare [galaxy] server fallback refused and accepted (R3, A4).
func tokenDestinationCasesGroupOne() []tokenDestinationCase {
	return []tokenDestinationCase{
		{
			// file server_list + section url, operator-channel token via
			// ANSIBLE_GALAXY_SERVER_CORP_TOKEN: the simplest leaking shape -
			// no --server, no --token, nothing naming "corp" but the
			// ansible.cfg itself. Also asserts the destination refusal is
			// never also the TLS-policy sentinel, on a fixture that carries
			// no validate_certs key at all - the mirror image of
			// TestTokenTLSPolicyPairing's own X1 row, which asserts the same
			// exclusion in the other direction on a fixture where both keys
			// are file-sourced.
			name: "refused: file server_list url with an env token",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r1-secret-token"},
			check: tokenDestinationRefusedNotTLS("corp", "https://corp.example:443", "r1-secret-token"),
		},
		{
			// the documented remedy for the row above: naming the url
			// through ANSIBLE_GALAXY_SERVER_CORP_URL moves it onto the
			// operator channel too, so the identical token is now accepted.
			// This acceptance is pinned by exactly one mutation below (the
			// urlFromAnsibleConfig-term deletion): it survives the
			// envOrIni-inversion mutation, since that one corrupts both
			// halves together and a pairing whose halves already agree
			// stays accepted regardless of which provenance they agree on.
			// So this row alone does not certify that this program
			// recognizes the environment as the operator channel - only
			// that an agreeing pairing passes, plus url precedence (env
			// beats section). The asymmetric fact that env IS the operator
			// channel and ini is not is what "refused: file server_list
			// url with an env token" (refusal) and "accepted: env url with
			// section token" and "accepted: an operator --token against an
			// env url overrides a section token" (acceptance) carry
			// instead, since each of those rows disagrees between its two
			// halves and each does break under the envOrIni-inversion
			// mutation.
			name: "accepted: adding an env url remedies the row above",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "a2-secret-token",
				"ANSIBLE_GALAXY_SERVER_CORP_URL":   "https://corp-env.example",
			},
			check: tokenDestinationAccepted("https://corp-env.example", "a2-secret-token"),
		},
		{
			// [galaxy] server from the file (no server_list at all) with an
			// operator --token: the implicit single server's id is "",
			// which still has to be named correctly in the refusal.
			name:   "refused: file [galaxy] server with --token",
			ansCfg: ansibleConfig{Galaxy: ansibleGalaxyConfig{Server: "https://filecorp.example"}},
			args:   []string{"--token=r3-secret-token"},
			check:  tokenDestinationRefused("", "https://filecorp.example:443", "r3-secret-token"),
		},
		{
			// the same shape with the server value sourced from
			// $ANSIBLE_GALAXY_SERVER instead of the ansible.cfg file key:
			// an operator channel, so the pairing with --token is accepted.
			name:  "accepted: ANSIBLE_GALAXY_SERVER with --token",
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER": "https://opcorp.example"},
			args:  []string{"--token=a4-secret-token"},
			check: tokenDestinationAccepted("https://opcorp.example", "a4-secret-token"),
		},
	}
}

// tokenDestinationCasesGroupTwo is the next four rows of
// tokenDestinationCases: --server naming a server_list id (R5), the
// file+file pairing that is always accepted (A6), an operator-sourced url
// shielding a file-sourced token (A7), and the Token.IsSet() guard alone
// (A8).
func tokenDestinationCasesGroupTwo() []tokenDestinationCase {
	return []tokenDestinationCase{
		{
			// --server=corp names a server_list id, which is still an
			// operator channel for the URL SELECTION, but not for the URL
			// VALUE itself: the section's own url is still file-sourced, so
			// an id match is not an address and the env token is refused.
			name: "refused: --server naming a server_list id with an env token",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r5-secret-token"},
			args:  []string{"--server=corp"},
			check: tokenDestinationRefused("corp", "https://corp.example:443", "r5-secret-token"),
		},
		{
			// section url + section token, nothing else: one author
			// supplied both halves, so the pairing is accepted unchanged.
			name: "accepted: section url with section token",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"url": "https://corp.example", "token": "a6-ini-token"},
				},
			},
			check: tokenDestinationAccepted("https://corp.example", "a6-ini-token"),
		},
		{
			// env url + section token: the url is operator-sourced, so the
			// pairing rule never even inspects the token's own provenance.
			name: "accepted: env url with section token",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"token": "a7-ini-token"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp-env.example"},
			check: tokenDestinationAccepted("https://corp-env.example", "a7-ini-token"),
		},
		{
			// section url, no token anywhere: this acceptance is
			// over-determined rather than resting on one guard term -
			// Token.IsSet() is false, and tokenFromAnsibleConfig defaults
			// to true for this absent key regardless (see the
			// "!s.Token.IsSet() ||" deletion bullet above), so either term
			// alone would exempt this row.
			name: "accepted: section url with no token at all",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"url": "https://corp.example"}},
			},
			check: tokenDestinationAcceptedNoToken("https://corp.example"),
		},
	}
}

// tokenDestinationCasesGroupThree is the final four rows of
// tokenDestinationCases: position independence in a multi-entry
// server_list (R9), forced anonymity never being refused (A10), an
// operator --token refused against a file-sourced url even with a section
// token present (R11), and that same override accepted once the url is
// operator-sourced (A12).
func tokenDestinationCasesGroupThree() []tokenDestinationCase {
	return []tokenDestinationCase{
		{
			// a two-entry server_list where only the second entry offends:
			// checkTokenPairing must walk the whole list rather than
			// stopping at (or only ever checking) the first entry.
			name: "refused: a later server_list entry offends, not the first",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "pub,corp"},
				GalaxyServers: map[string]map[string]string{
					"pub":  {},
					"corp": {"url": "https://corp.example"},
				},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_PUB_URL":    "https://pub.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r9-secret-token",
			},
			check: tokenDestinationRefused("corp", "https://corp.example:443", "r9-secret-token"),
		},
		{
			// section url + section token + an explicit --token= (empty):
			// forcing anonymity is never refused, even against a
			// file-sourced url - there is no destination left to pair.
			name: "accepted: an explicit empty --token forces anonymity",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"url": "https://corp.example", "token": "a10-ini-token"},
				},
			},
			args:  []string{"--token="},
			check: tokenDestinationAcceptedNoToken("https://corp.example"),
		},
		{
			// section url AND section token, overridden by an operator
			// --token: the section's own token authorizes only itself, not
			// a credential from an operator channel layered on top of it.
			name: "refused: an operator --token overrides a section token pairing",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"url": "https://corp.example", "token": "r11-ini-token"},
				},
			},
			args:  []string{"--token=from-flag"},
			check: tokenDestinationRefused("corp", "https://corp.example:443", "from-flag"),
		},
		{
			// env url + section token + an operator --token: the url is
			// operator-sourced, so the flag's override is accepted - the
			// same precedence TestTokenFlagCases documents, on a fixture
			// built for this table.
			name: "accepted: an operator --token against an env url overrides a section token",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"token": "a12-ini-token"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp-env.example"},
			args:  []string{"--token=from-flag"},
			check: tokenDestinationAccepted("https://corp-env.example", "from-flag"),
		},
	}
}

// tlsPolicyServerID is the [galaxy_server.<id>] section id every fixture in
// tokenTLSPolicyCases uses; see tlsPolicyRefused for why the TLS arm can
// only ever be reached through such a section.
const tlsPolicyServerID = "corp"

// tlsPolicyRefused builds a tokenTLSPolicyCase assertion for a refused
// pairing: resolveServers fails with helpers.ErrTokenTLSPolicyFromAnsibleConfig,
// naming the offending server and wantOrigin, and never the plaintext of
// wantToken - the same three properties tokenDestinationRefused asserts for
// the destination sentinel, mirrored onto the TLS one.
//
// The server id is fixed here rather than taken as a parameter, and the two
// halves of that split between the arm and this table. The arm guarantees
// only that the id is non-empty: [galaxy] carries no validate_certs key at
// all, so nothing but a [galaxy_server.<id>] section can ever reach the TLS
// arm, and a section always has an id. That the id is tlsPolicyServerID
// specifically is a property of the fixtures, every one of which names its
// section that, and a new row must keep doing so or take the id back as a
// parameter. The destination sibling does take one, since the bare [galaxy]
// server path it also covers resolves to the empty id instead.
func tlsPolicyRefused(wantOrigin, wantToken string) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, _ *Config, err error) {
		t.Helper()
		if !errors.Is(err, helpers.ErrTokenTLSPolicyFromAnsibleConfig) {
			t.Fatalf("resolveServers() error = %v, want helpers.ErrTokenTLSPolicyFromAnsibleConfig", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, fmt.Sprintf("%q", tlsPolicyServerID)) {
			t.Errorf("error = %q, want it to name server %q", msg, tlsPolicyServerID)
		}
		if !strings.Contains(msg, wantOrigin) {
			t.Errorf("error = %q, want it to name origin %q", msg, wantOrigin)
		}
		if strings.Contains(msg, wantToken) {
			t.Errorf("error = %q, must not contain the token plaintext %q", msg, wantToken)
		}
	}
}

// tlsPolicyAccepted builds a tokenTLSPolicyCase assertion for an accepted
// pairing: resolveServers succeeds, and the resolved single server's URL,
// revealed token, and InsecureSkipTLSVerify equal wantURL, wantToken, and
// wantInsecure. The InsecureSkipTLSVerify check is what tokenDestinationAccepted
// never makes, since the destination rule alone has no TLS dimension to
// assert on.
func tlsPolicyAccepted(wantURL, wantToken string, wantInsecure bool) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
		}
		if got := cfg.Servers[0].URL; got != wantURL {
			t.Errorf("Servers[0].URL = %q, want %q", got, wantURL)
		}
		if got := cfg.Servers[0].Token.Reveal(); got != wantToken {
			t.Errorf("Servers[0].Token.Reveal() = %q, want %q", got, wantToken)
		}
		if got := cfg.Servers[0].InsecureSkipTLSVerify; got != wantInsecure {
			t.Errorf("Servers[0].InsecureSkipTLSVerify = %v, want %v", got, wantInsecure)
		}
	}
}

// tlsPolicyAcceptedNoToken builds a tokenTLSPolicyCase assertion for an
// accepted pairing whose token ends up cleared or never configured - A4 (no
// token was ever supplied) and A5 (an explicit --token= forced anonymity) -
// while still checking InsecureSkipTLSVerify against wantInsecure, since both
// rows exist specifically to show a file-sourced TLS policy surviving
// unchanged once there is no operator token left for the pairing rule to
// protect.
func tlsPolicyAcceptedNoToken(wantURL string, wantInsecure bool) func(t *testing.T, cfg *Config, err error) {
	return func(t *testing.T, cfg *Config, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("resolveServers() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 {
			t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
		}
		if got := cfg.Servers[0].URL; got != wantURL {
			t.Errorf("Servers[0].URL = %q, want %q", got, wantURL)
		}
		if cfg.Servers[0].Token.IsSet() {
			t.Errorf("Servers[0].Token is set, want it cleared or never configured")
		}
		if got := cfg.Servers[0].InsecureSkipTLSVerify; got != wantInsecure {
			t.Errorf("Servers[0].InsecureSkipTLSVerify = %v, want %v", got, wantInsecure)
		}
	}
}

// tokenTLSPolicyCase is one row of TestTokenTLSPolicyPairing, structured
// exactly like tokenDestinationCase: a full resolveServers run against its
// own ansible.cfg fixture, environment overrides, and command line, checked
// by its own assertions.
type tokenTLSPolicyCase struct {
	check  func(t *testing.T, cfg *Config, err error)
	env    map[string]string
	ansCfg ansibleConfig
	name   string
	args   []string
}

// TestTokenTLSPolicyPairing is the table for the TLS-policy half of the
// pairing rule tokenPairingOffense enforces: a Galaxy token must never be
// paired with a server whose certificate verification an ansible.cfg file
// disabled for it, unless the token itself came from that same file's own
// [galaxy_server.<id>] section. It sits beside TestTokenDestinationPairing
// rather than inside it, since that table's own doc comment already runs to
// twelve rows with a funlen three-group split, and the two tables check two
// different arms of the same predicate rather than one shared shape.
//
// Three of these eleven rows sit beside a sibling varying exactly one
// property of the refused fixture, which is what shows the check actually
// fired rather than that the fixture never reached it - though the three
// vary three different properties. R1 ("refused: env url + env token + a
// file validate_certs") beside A1 varies the channel the rule reads: the
// same fixture plus an env VALIDATE_CERTS override. R2 ("refused: --server
// by id, env url, env token, file validate_certs") beside A2 varies the
// key's value instead, validate_certs = yes in place of no, which pins that
// the predicate is the RELAXATION and not the key's mere presence. R3
// ("refused: env url + --token + file validate_certs") beside A5 varies the
// token's presence, --token= forcing anonymity, which pins that the refusal
// needs an operator credential to protect before it fires at all. X1
// ("refused (destination, not TLS): file url and file validate_certs, env
// token") pins the ordering between the two sentinels instead: its fixture makes both url and validate_certs
// file-sourced, and it asserts the destination sentinel fires while this
// table's own TLS sentinel is NOT also readable through errors.Is on the
// same error. Its mirror lives in TestTokenDestinationPairing's own
// "refused: file server_list url with an env token" row instead of here,
// asserting the same exclusion from the other direction: a destination
// refusal, on a fixture with no validate_certs key at all, is never also
// the TLS sentinel.
//
// R4 ("refused: a later server_list entry offends, not the first") has no
// such single-property sibling in this table - the same situation
// TestTokenDestinationPairing's own doc comment records for two of its own
// rows: growing a list from one entry to two is not a single-property
// difference from any accepted row here. Its fixture is instead shown
// capable of acceptance by the M1 mutation recorded below (deleting the "if
// s.insecureFromAnsibleConfig" arm from tokenPairingOffense): every refusal
// row in this table - R1, R2, R3, and R4 alike - turns into
// "resolveServers() error = <nil>, want
// helpers.ErrTokenTLSPolicyFromAnsibleConfig" under it, since the sentinel
// that arm alone can return is never reached at all. A hardcoded-false
// variant of insecureFromAnsibleConfig itself (M4, also below) was tried
// for the identical purpose first and does not compile, for the reason
// recorded on M4's own bullet; M1 is what actually demonstrates this row's
// fixture is capable of acceptance.
//
// Neither this test nor its subtests call t.Parallel(), for the identical
// reason TestTokenDestinationPairing does not: most rows set an
// ANSIBLE_GALAXY_SERVER_<ID>_URL, _TOKEN, or _VALIDATE_CERTS override via
// tc.env, and t.Setenv panics on a parallel test or one with a parallel
// ancestor.
//
// M1 through M6 below are the mutations targeting tokenPairingOffense and
// buildServer's insecureFromAnsibleConfig derivation directly, each run via
// go test -overlay against the whole internal/galaxy/config package; M1 and
// M5 were additionally run against ./cmd/go-galaxy/exitcode and
// ./cmd/go-galaxy/commands, since those are the two other packages a
// pairing-rule regression could plausibly reach - exitcode through its own
// sentinel-to-exit-class table, commands through
// TestTokenDestinationEndToEnd and TestTokenTLSPolicyEndToEnd. No kill set
// was predicted before any of these ran; every result below is what the run
// actually reported.
//
//   - M1: deleting the "if s.insecureFromAnsibleConfig" arm from
//     tokenPairingOffense turns all four of this table's refusal rows (R1,
//     R2, R3, R4) into acceptances:
//
//     resolveServers() error = <nil>, want helpers.ErrTokenTLSPolicyFromAnsibleConfig
//
//     Scope: no other config-package test is affected. Against
//     ./cmd/go-galaxy/exitcode, the whole package reports cached/unaffected -
//     that package imports helpers, not config, so a config-only mutation
//     cannot reach it at all. Against ./cmd/go-galaxy/commands, exactly one
//     test fails: TestTokenTLSPolicyEndToEnd's own refusal subtest, with
//     "BuildCollectionConfig() error = <nil>, want
//     helpers.ErrTokenTLSPolicyFromAnsibleConfig".
//
//   - M2: changing buildServer's "insecure && !validateCertsFromEnv" to
//     "!validateCertsFromEnv" turns "accepted: validate_certs = yes is not
//     a relaxation to pair against" into a refusal, since the mutated
//     expression no longer consults insecure at all and reads true for
//     any file-sourced validate_certs key regardless of its value:
//
//     resolveServers() error = galaxy server certificate verification was
//     disabled by ansible.cfg for a token it did not supply: server "corp"
//     (https://corp.example:443), want nil
//
//     Scope: beyond this table, the identical mutation also fails one
//     TestTokenFlagCases row, two TestTokenDestinationPairing rows
//     ("accepted: adding an env url remedies the row above" and "accepted:
//     an operator --token against an env url overrides a section token" -
//     both carry an env-sourced url with no validate_certs key at all, so
//     validateCertsFromEnv is false and the mutated expression reads true
//     unconditionally), one more TestTokenTLSPolicyPairing row ("accepted:
//     the ordinary env url + env token shape, no validate_certs" - the
//     identical no-key reasoning), and TestResolveServersListBeatsAnsibleServer
//     outright, whose want literal pins insecureFromAnsibleConfig to
//     false for a server with no validate_certs key configured anywhere.
//
//   - M3: changing the same expression to "insecure" alone does not
//     produce a test failure - it does not compile, since
//     validateCertsFromEnv then has no remaining use, so the compiler
//     names that declaration in buildServer:
//
//     declared and not used: validateCertsFromEnv
//
//     Scope: this mutation does not build, so no test in this package
//     ever runs against it.
//
//   - M4: hardcoding the field to "insecureFromAnsibleConfig: false" fails
//     to compile for the identical reason as M3 - validateCertsFromEnv
//     becomes unused the moment nothing in the literal reads it, and the
//     compiler names the same declaration:
//
//     declared and not used: validateCertsFromEnv
//
//     Scope: this mutation does not build either. Between M3 and M4, no
//     mutation that removes validateCertsFromEnv's only read can ever
//     reach a test in this package - Go's own compiler rejects it first,
//     which is why R4's own "capable of acceptance" claim above relies on
//     M1 instead.
//
//   - M5: swapping the two sentinel returns inside tokenPairingOffense (the
//     urlFromAnsibleConfig arm returning ErrTokenTLSPolicyFromAnsibleConfig,
//     the insecureFromAnsibleConfig arm returning
//     ErrTokenDestinationFromAnsibleConfig) turns this table's own X1 row
//     into a destination-sentinel-shaped failure even though X1 itself
//     expects the destination sentinel, because the swap fires on the
//     WRONG arm for a fixture where url is file-sourced (the first arm
//     checked): X1's own t.Fatalf reports
//
//     resolveServers() error = galaxy server certificate verification was
//     disabled by ansible.cfg for a token it did not supply: server "corp"
//     (https://corp.example:443), want helpers.ErrTokenDestinationFromAnsibleConfig
//
//     along with all four of this table's own refusal rows reporting the
//     opposite sentinel from the one their fixture is built for.
//
//     Scope: beyond this table, the identical mutation also fails one
//     TestTokenFlagCases row and five TestTokenDestinationPairing rows,
//     each now reporting the TLS sentinel where the destination one was
//     wanted. Against ./cmd/go-galaxy/exitcode, nothing fails: both
//     sentinels classify ExitUsage through the identical
//     isGalaxyServerPolicyError disjunct, so a swap between them is
//     invisible to that package's own tests by construction - proving the
//     swap has no observable effect there is itself the result, not an
//     absence of one. Against ./cmd/go-galaxy/commands, both
//     TestTokenDestinationEndToEnd and TestTokenTLSPolicyEndToEnd fail
//     their own refusal subtest, each reporting the other test's sentinel.
//
//   - M6: deleting "s.tokenFromAnsibleConfig" from tokenPairingOffense's
//     skip guard is the identical code change TestTokenDestinationPairing's
//     own restated second bullet makes and reruns; see that bullet, in
//     this file above, for the full real output and scope, which now spans
//     both tables plus three more config-package tests
//     (TestResolveServersOriginNoConflict, TestResolveServersExplicitByID,
//     and two subtests of TestResolveServersTLSWarnings).
func TestTokenTLSPolicyPairing(t *testing.T) {
	for _, tc := range tokenTLSPolicyCases() {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := newServerCmd(t, tc.args)
			cfg, err := runResolveServers(t, c, tc.ansCfg)
			tc.check(t, cfg, err)
		})
	}
}

// tokenTLSPolicyCases enumerates the TLS-policy half of the pairing rule's
// eleven reachable shapes, mirroring tokenDestinationCases's own structure:
// a file-sourced validate_certs relaxation paired with an operator-sourced
// token is refused in every channel that can produce it, while a
// section-sourced token, an unrelaxed (or absent) validate_certs, a token
// forced anonymous, and the ordinary no-validate_certs shape are all
// accepted. The rows split across four producer functions, three of three
// rows and one of two, purely to stay under funlen's line budget; the split
// carries no meaning of its own. It is four groups rather than the
// destination table's three because these rows carry more setup each, not
// because there are more of them.
func tokenTLSPolicyCases() []tokenTLSPolicyCase {
	cases := tokenTLSPolicyCasesGroupOne()
	cases = append(cases, tokenTLSPolicyCasesGroupTwo()...)
	cases = append(cases, tokenTLSPolicyCasesGroupThree()...)
	return append(cases, tokenTLSPolicyCasesGroupFour()...)
}

// tokenTLSPolicyCasesGroupOne is the first three rows of
// tokenTLSPolicyCases: the leaking shape and its remedy (R1, A1), and the
// --server-by-id refusal (R2).
func tokenTLSPolicyCasesGroupOne() []tokenTLSPolicyCase {
	return []tokenTLSPolicyCase{
		{
			// this is the exact shape reported against a live reproduction:
			// a section naming ONLY validate_certs = no, with both the url
			// and the token supplied through the operator's own env
			// channel - precisely the remedy this rule's own message tells
			// an operator to adopt for the destination half. Both halves of
			// the pairing being the operator's own does not exempt this
			// server, because the third value - validate_certs - is not
			// part of that pairing at all.
			name: "refused: env url + env token + a file validate_certs",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL":   "https://real-hub.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r1-secret-token",
			},
			check: tlsPolicyRefused("https://real-hub.example:443", "r1-secret-token"),
		},
		{
			// the documented remedy for the row above: naming the identical
			// validate_certs value through ANSIBLE_GALAXY_SERVER_CORP_VALIDATE_CERTS
			// moves the TLS policy onto the operator channel too, so the
			// pairing is now accepted - and InsecureSkipTLSVerify is still
			// true, since the remedy moves who supplied the relaxation, not
			// the relaxation itself.
			name: "accepted: an env VALIDATE_CERTS override remedies the row above",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL":            "https://real-hub.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN":          "a1-secret-token",
				"ANSIBLE_GALAXY_SERVER_CORP_VALIDATE_CERTS": "no",
			},
			check: tlsPolicyAccepted("https://real-hub.example", "a1-secret-token", true),
		},
		{
			// --server=corp names a server_list id - an operator channel for
			// SELECTING the section - while the url is also env-sourced, so
			// neither URL-side channel is file-sourced; only validate_certs
			// is. This pins that the TLS-policy check runs independently of
			// the destination one rather than only ever firing alongside it.
			name: "refused: --server by id, env url, env token, file validate_certs",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp.example",
				"GO_GALAXY_TOKEN":                "r2-secret-token",
			},
			args:  []string{"--server=corp"},
			check: tlsPolicyRefused("https://corp.example:443", "r2-secret-token"),
		},
	}
}

// tokenTLSPolicyCasesGroupTwo is the next three rows of
// tokenTLSPolicyCases: validate_certs = yes as a non-relaxation (A2), and
// the two shapes the skip guard exempts - a section token paired with a
// section validate_certs (A3), and no token at all (A4).
func tokenTLSPolicyCasesGroupTwo() []tokenTLSPolicyCase {
	return []tokenTLSPolicyCase{
		{
			// the same fixture with validate_certs = yes instead of no: the
			// key is still file-sourced, but resolveValidateCerts now
			// reports InsecureSkipTLSVerify false, so
			// insecureFromAnsibleConfig's own conjunction reads false too.
			// This is what pins the predicate to the RELAXATION rather than
			// to the key's mere presence in the section.
			name: "accepted: validate_certs = yes is not a relaxation to pair against",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "yes"}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp.example",
				"GO_GALAXY_TOKEN":                "a2-secret-token",
			},
			args:  []string{"--server=corp"},
			check: tlsPolicyAccepted("https://corp.example", "a2-secret-token", false),
		},
		{
			// section token + section validate_certs = no, url env-sourced:
			// the token is file-sourced, so tokenPairingOffense's own
			// tokenFromAnsibleConfig guard exempts this server before it
			// ever inspects insecureFromAnsibleConfig - the file supplied
			// the credential, so it is free to pair it with its own
			// relaxed TLS policy too.
			name: "accepted: section token pairs with section validate_certs",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"token": "a3-ini-token", "validate_certs": "no"},
				},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp-env.example"},
			check: tlsPolicyAccepted("https://corp-env.example", "a3-ini-token", true),
		},
		{
			// env url, no token configured anywhere, section
			// validate_certs = no: Token.IsSet() is false, so
			// tokenPairingOffense exempts this server outright - the
			// unauthenticated half of the ansible.cfg drop-in promise
			// survives untouched, and InsecureSkipTLSVerify still reads
			// true.
			name: "accepted: no token at all leaves a file validate_certs untouched",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp-env.example"},
			check: tlsPolicyAcceptedNoToken("https://corp-env.example", true),
		},
	}
}

// tokenTLSPolicyCasesGroupThree is the next three rows of
// tokenTLSPolicyCases: the --token refusal and its forced-anonymous
// sibling (R3, A5), and the later-server_list-entry refusal (R4).
func tokenTLSPolicyCasesGroupThree() []tokenTLSPolicyCase {
	return []tokenTLSPolicyCase{
		{
			// env url + an operator --token + section validate_certs = no:
			// the same shape as R2, through --token instead of
			// GO_GALAXY_TOKEN, and through the implicit single-entry
			// server_list path instead of an explicit --server=corp.
			name: "refused: env url + --token + file validate_certs",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp.example"},
			args:  []string{"--token=r3-secret-token"},
			check: tlsPolicyRefused("https://corp.example:443", "r3-secret-token"),
		},
		{
			// the row above with --token= (empty): forcing anonymity is
			// never refused, even against a file-sourced validate_certs -
			// there is no destination left to pair, and the relaxed policy
			// itself is left exactly as the file configured it.
			name: "accepted: an explicit empty --token forces anonymity",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {"validate_certs": "no"}},
			},
			env:   map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_URL": "https://corp.example"},
			args:  []string{"--token="},
			check: tlsPolicyAcceptedNoToken("https://corp.example", true),
		},
		{
			// a two-entry server_list where only the second entry offends:
			// tokenPairingOffense must be evaluated per server across the
			// whole walk, not only against the first entry.
			name: "refused: a later server_list entry offends, not the first",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "pub,corp"},
				GalaxyServers: map[string]map[string]string{
					"pub":  {"url": "https://pub.example"},
					"corp": {"validate_certs": "no"},
				},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL":   "https://corp.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "r4-secret-token",
			},
			check: tlsPolicyRefused("https://corp.example:443", "r4-secret-token"),
		},
	}
}

// tokenTLSPolicyCasesGroupFour is the last two rows of
// tokenTLSPolicyCases: the sentinel-ordering row (X1) and the ordinary
// no-validate_certs shape that must not regress (A6).
func tokenTLSPolicyCasesGroupFour() []tokenTLSPolicyCase {
	return []tokenTLSPolicyCase{
		{
			// section url AND section validate_certs = no, env token: this
			// is TestTokenDestinationPairing's own "refused: file
			// server_list url with an env token" fixture (X1's twin) with
			// validate_certs = no added - the destination sentinel fires
			// first, and the TLS one must not fire at all.
			name: "refused (destination, not TLS): file url and file validate_certs, env token",
			ansCfg: ansibleConfig{
				Galaxy: ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{
					"corp": {"url": "https://corp.example", "validate_certs": "no"},
				},
			},
			env: map[string]string{"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "x1-secret-token"},
			check: func(t *testing.T, _ *Config, err error) {
				t.Helper()
				if !errors.Is(err, helpers.ErrTokenDestinationFromAnsibleConfig) {
					t.Fatalf("resolveServers() error = %v, want helpers.ErrTokenDestinationFromAnsibleConfig", err)
				}
				if errors.Is(err, helpers.ErrTokenTLSPolicyFromAnsibleConfig) {
					t.Errorf("resolveServers() error = %v, must not also be helpers.ErrTokenTLSPolicyFromAnsibleConfig", err)
				}
			},
		},
		{
			// env url + env token, no validate_certs key anywhere: the
			// ordinary shape TestTokenDestinationPairing already covers on
			// its own "accepted: adding an env url remedies the row above"
			// row must not regress once insecureFromAnsibleConfig exists -
			// an absent key leaves InsecureSkipTLSVerify false by
			// resolveValidateCerts's own default, so the conjunction reads
			// false regardless of provenance.
			name: "accepted: the ordinary env url + env token shape, no validate_certs",
			ansCfg: ansibleConfig{
				Galaxy:        ansibleGalaxyConfig{ServerList: "corp"},
				GalaxyServers: map[string]map[string]string{"corp": {}},
			},
			env: map[string]string{
				"ANSIBLE_GALAXY_SERVER_CORP_URL":   "https://corp-env.example",
				"ANSIBLE_GALAXY_SERVER_CORP_TOKEN": "a6-secret-token",
			},
			check: tlsPolicyAccepted("https://corp-env.example", "a6-secret-token", false),
		},
	}
}
