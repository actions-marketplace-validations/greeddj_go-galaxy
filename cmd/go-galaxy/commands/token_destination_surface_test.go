package commands

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// writeCWDAnsibleConfig neutralizes ansible.cfg discovery exactly as
// neutralizeAnsibleDiscovery does ($ANSIBLE_CONFIG pointed at a path that
// does not exist, $HOME redirected to an empty temp directory), then adds
// what that helper deliberately does not: a real ./ansible.cfg, written into
// a 0o755 (not world-writable, so cwdCandidate keeps it as a discovery
// candidate) directory this test chdirs into. Real discovery therefore finds
// this file through the same ./ansible.cfg candidate a checked-out
// repository would use, rather than through the explicit --ansible-config
// path config_surface_test.go's other rows exercise.
func writeCWDAnsibleConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	// #nosec G302 -- the permission is the fixture: cwdCandidate must accept
	// this directory as a discovery source, which requires it not be
	// world-writable (t.TempDir defaults to 0o700 on most systems, already
	// satisfying that, but the mode is pinned explicitly here since it is
	// what this test's discovery depends on rather than an accident of the
	// test harness).
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod cwd: %v", err)
	}
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "absent.cfg"))
	t.Setenv("HOME", t.TempDir())
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "ansible.cfg"), []byte(body), 0o600); err != nil {
		t.Fatalf("write ansible.cfg: %v", err)
	}
}

// bareGalaxyServerAnsibleCfg is the simplest leaking shape the token
// destination pairing rule refuses: no server_list, no [galaxy_server.<id>]
// section, no id for an operator to guess - just a bare [galaxy] server line
// naming an address, exactly what a repository's own ansible.cfg can commit
// with no involvement from whoever later runs a CI job against it.
const bareGalaxyServerAnsibleCfg = "[galaxy]\nserver = https://corp.example\n"

// TestTokenDestinationEndToEnd is the one place real ansible.cfg discovery,
// full config resolution, and the value that would reach the wire are all
// asserted together - the shape that would have caught the credential
// redirection config.checkTokenPairing now refuses, since
// config_surface_test.go's own rows never resolve a real cwd ansible.cfg
// against a real token and inspect what serverAuths does with it.
func TestTokenDestinationEndToEnd(t *testing.T) {
	t.Run("refused: a bare file server plus GO_GALAXY_TOKEN", func(t *testing.T) {
		writeCWDAnsibleConfig(t, bareGalaxyServerAnsibleCfg)
		t.Setenv("GO_GALAXY_TOKEN", "leaked-if-this-ever-resolves")

		_, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if !errors.Is(err, helpers.ErrTokenDestinationFromAnsibleConfig) {
			t.Fatalf("BuildCollectionConfig() error = %v, want helpers.ErrTokenDestinationFromAnsibleConfig", err)
		}
	})

	t.Run("accepted: the documented remedy resolves and the token reaches the operator's own origin", func(t *testing.T) {
		writeCWDAnsibleConfig(t, bareGalaxyServerAnsibleCfg)
		t.Setenv("GO_GALAXY_TOKEN", "s3cr3t-operator-token")
		// The documented remedy: name the identical address through the
		// operator channel ansible itself already defines for [galaxy]
		// server, rather than through a switch this tool would have to
		// invent. The url does not change; only who supplied it does.
		t.Setenv("ANSIBLE_GALAXY_SERVER", "https://corp.example")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 || cfg.Servers[0].URL != "https://corp.example" {
			t.Fatalf("Servers = %+v, want one server at https://corp.example", cfg.Servers)
		}

		parsed, err := url.Parse(cfg.Servers[0].URL)
		if err != nil {
			t.Fatalf("url.Parse(%q) error = %v, want nil", cfg.Servers[0].URL, err)
		}
		wantOrigin := helpers.Origin(parsed)

		auths := serverAuths(cfg.Servers)
		if len(auths) != 1 {
			t.Fatalf("len(serverAuths()) = %d, want 1", len(auths))
		}
		if auths[0].Origin != wantOrigin {
			t.Errorf("serverAuths()[0].Origin = %q, want %q", auths[0].Origin, wantOrigin)
		}
		if auths[0].Token != "s3cr3t-operator-token" {
			t.Errorf("serverAuths()[0].Token = %q, want %q", auths[0].Token, "s3cr3t-operator-token")
		}
	})
}

// tlsPolicyGalaxyServerAnsibleCfg is the leaking shape the TLS-policy half
// of the pairing rule refuses: a server_list entry whose section names
// nothing but validate_certs = no - no url, no token - so both of those
// still have to come from the operator's own environment for the fixture to
// resolve into anything installable at all.
const tlsPolicyGalaxyServerAnsibleCfg = "[galaxy]\nserver_list = corp\n\n[galaxy_server.corp]\nvalidate_certs = no\n"

// TestTokenTLSPolicyEndToEnd is TestTokenDestinationEndToEnd's sibling for
// the TLS-policy half of the pairing rule: real ansible.cfg discovery, full
// config resolution, and the exact fetch.ServerAuth combination that would
// reach the wire, asserted together. The refusal alone would only prove the
// gate fires; the positive control is what proves it fires for the right
// reason - that this combination (an operator's real token, sent to a
// connection whose certificate this run cannot verify) is exactly what the
// refusal exists to keep out of serverAuths, not merely out of cfg.Servers.
func TestTokenTLSPolicyEndToEnd(t *testing.T) {
	t.Run("refused: a file validate_certs=no with the operator's own url and token", func(t *testing.T) {
		writeCWDAnsibleConfig(t, tlsPolicyGalaxyServerAnsibleCfg)
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_URL", "https://real-hub.example")
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_TOKEN", "leaked-if-this-ever-resolves")

		_, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if !errors.Is(err, helpers.ErrTokenTLSPolicyFromAnsibleConfig) {
			t.Fatalf("BuildCollectionConfig() error = %v, want helpers.ErrTokenTLSPolicyFromAnsibleConfig", err)
		}
	})

	t.Run("accepted: the documented remedy resolves, and the token still reaches an unverified connection", func(t *testing.T) {
		writeCWDAnsibleConfig(t, tlsPolicyGalaxyServerAnsibleCfg)
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_URL", "https://real-hub.example")
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_TOKEN", "s3cr3t-operator-token")
		// The documented remedy: name the identical validate_certs value
		// through the operator's own environment channel, moving the TLS
		// policy off the file and onto the operator - the value itself does
		// not change, only who supplied it does.
		t.Setenv("ANSIBLE_GALAXY_SERVER_CORP_VALIDATE_CERTS", "no")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if len(cfg.Servers) != 1 || cfg.Servers[0].URL != "https://real-hub.example" {
			t.Fatalf("Servers = %+v, want one server at https://real-hub.example", cfg.Servers)
		}

		// This is the exact fetch.ServerAuth combination the refusal exists
		// to prevent from being file-chosen: an operator token, sent to an
		// origin whose certificate this run will not verify. Asserted here
		// because this is the only place the value that reaches the wire -
		// rather than cfg.Servers's own InsecureSkipTLSVerify bool - can be
		// checked at all.
		auths := serverAuths(cfg.Servers)
		if len(auths) != 1 {
			t.Fatalf("len(serverAuths()) = %d, want 1", len(auths))
		}
		if auths[0].Token != "s3cr3t-operator-token" {
			t.Errorf("serverAuths()[0].Token = %q, want %q", auths[0].Token, "s3cr3t-operator-token")
		}
		if !auths[0].InsecureTLS {
			t.Errorf("serverAuths()[0].InsecureTLS = false, want true")
		}
	})
}
