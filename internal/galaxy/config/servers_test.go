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
	"gopkg.in/yaml.v3"
)

// testServerFlagDefault is the --server flag default used by every
// newServerCmd-built command below; it stands in for
// cmd/go-galaxy/helpers/flags.go's real defaultServerURL and only needs to
// be distinguishable from the values under test.
const testServerFlagDefault = "https://default.example"

// newServerCmd builds a *cli.Command exposing the "server" and "token"
// flags exactly as cmd/go-galaxy/helpers/flags.go defines them - server
// defaulting to testServerFlagDefault and sourced from GO_GALAXY_SERVER and
// ANSIBLE_GALAXY_SERVER, token with no default and sourced from
// GO_GALAXY_TOKEN - so c.IsSet and c.String behave the same way they do for
// a real command.
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

// runResolveServers seeds cfg.Server the way applyAnsibleConfig would have
// (via pickConfigValue, resolveServers's documented precondition for the
// no-server_list case), then runs resolveServers.
func runResolveServers(t *testing.T, c *cli.Command, ansCfg ansibleConfig) (*Config, error) {
	t.Helper()
	cfg := &Config{}
	cfg.Server, _ = pickConfigValue(c, "server", ansCfg.Galaxy.Server)
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

	want := []Server{{URL: "https://ansible.example"}}
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

	want := []Server{{ID: "prod", URL: "https://prod.example"}}
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

	t.Run("id is used verbatim, no case or dash translation", func(t *testing.T) {
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
		_, err := buildImplicitServer("https://user:pass@hub.example/")
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

// TestTokenFlagAppliesToSingleServer checks the case --token exists for:
// one effective server, credential handed to it on the command line rather
// than through an ansible.cfg section.
func TestTokenFlagAppliesToSingleServer(t *testing.T) {
	t.Parallel()

	c := newServerCmd(t, []string{"--server=https://hub.example.com", "--token=s3cr3t"})
	cfg, err := runResolveServers(t, c, ansibleConfig{})
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

// TestTokenFlagOverridesConfiguredToken checks precedence: an explicit
// command-line or environment token wins over one configured in
// ansible.cfg, the same direction every other explicitly-set value takes.
func TestTokenFlagOverridesConfiguredToken(t *testing.T) {
	t.Parallel()

	ansCfg := ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "hub"},
		GalaxyServers: map[string]map[string]string{"hub": {"url": "https://hub.example.com", "token": "from-ini"}},
	}
	c := newServerCmd(t, []string{"--token=from-flag"})
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if got := cfg.Servers[0].Token.Reveal(); got != "from-flag" {
		t.Errorf("Servers[0].Token = %q, want the flag value %q", got, "from-flag")
	}
}

// TestTokenFlagClearsWhenExplicitlyEmpty checks that exporting
// GO_GALAXY_TOKEN= forces an anonymous run without editing any config,
// which is the point of treating an explicitly empty value as "no token"
// rather than as "unset".
func TestTokenFlagClearsWhenExplicitlyEmpty(t *testing.T) {
	t.Parallel()

	ansCfg := ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "hub"},
		GalaxyServers: map[string]map[string]string{"hub": {"url": "https://hub.example.com", "token": "from-ini"}},
	}
	c := newServerCmd(t, []string{"--token="})
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if cfg.Servers[0].Token.IsSet() {
		t.Errorf("Servers[0].Token is set, want it cleared by an explicitly empty --token")
	}
}

// TestTokenFlagRejectedWithMultipleServers checks the ambiguity refusal:
// with two servers in effect the flag names neither, and guessing could
// send a private hub's credential to the public Galaxy.
func TestTokenFlagRejectedWithMultipleServers(t *testing.T) {
	t.Parallel()

	ansCfg := ansibleConfig{
		Galaxy: ansibleGalaxyConfig{ServerList: "hub,public"},
		GalaxyServers: map[string]map[string]string{
			"hub":    {"url": "https://hub.example.com"},
			"public": {"url": "https://galaxy.ansible.com"},
		},
	}
	c := newServerCmd(t, []string{"--token=s3cr3t"})
	if _, err := runResolveServers(t, c, ansCfg); !errors.Is(err, helpers.ErrAmbiguousGalaxyToken) {
		t.Fatalf("resolveServers() error = %v, want ErrAmbiguousGalaxyToken", err)
	}
}

// TestTokenFlagRejectedOverPlaintext checks that --token is held to the
// same transport rule as a configured token: a credential must not be sent
// in the clear to anything but loopback.
func TestTokenFlagRejectedOverPlaintext(t *testing.T) {
	t.Parallel()

	c := newServerCmd(t, []string{"--server=http://hub.example.com", "--token=s3cr3t"})
	if _, err := runResolveServers(t, c, ansibleConfig{}); !errors.Is(err, helpers.ErrInsecureTokenTransport) {
		t.Fatalf("resolveServers() error = %v, want ErrInsecureTokenTransport", err)
	}

	loopback := newServerCmd(t, []string{"--server=http://127.0.0.1:8080", "--token=s3cr3t"})
	if _, err := runResolveServers(t, loopback, ansibleConfig{}); err != nil {
		t.Fatalf("resolveServers() over loopback error = %v, want nil", err)
	}
}

// TestTokenFlagUnsetLeavesServersUntouched checks that a command which
// never sets the flag - including one that does not register it - changes
// nothing, so the flag cannot silently strip a configured credential.
func TestTokenFlagUnsetLeavesServersUntouched(t *testing.T) {
	t.Parallel()

	ansCfg := ansibleConfig{
		Galaxy:        ansibleGalaxyConfig{ServerList: "hub"},
		GalaxyServers: map[string]map[string]string{"hub": {"url": "https://hub.example.com", "token": "from-ini"}},
	}
	c := newServerCmd(t, nil)
	cfg, err := runResolveServers(t, c, ansCfg)
	if err != nil {
		t.Fatalf("resolveServers() error = %v, want nil", err)
	}
	if got := cfg.Servers[0].Token.Reveal(); got != "from-ini" {
		t.Errorf("Servers[0].Token = %q, want the configured %q", got, "from-ini")
	}
}
