package commands

import (
	"errors"
	"reflect"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestGitCredentialsRevealsPlaintext pins the one place a git Secret is
// revealed: every field of config.GitCredential reaches gitsource.Credential
// in the clear, and a nil config yields no credentials rather than a panic.
func TestGitCredentialsRevealsPlaintext(t *testing.T) {
	t.Parallel()

	basicURL, err := gitsource.ParsePrefix("https://git.example/org")
	if err != nil {
		t.Fatalf("ParsePrefix: %v", err)
	}
	sshURL, err := gitsource.ParsePrefix("ssh://git.example")
	if err != nil {
		t.Fatalf("ParsePrefix: %v", err)
	}
	cfg := &config.Config{GitCredentials: []config.GitCredential{
		{ID: "a", URL: basicURL, Username: "ci", Password: config.NewSecret("pw-plain"), Kind: config.GitCredentialBasic},
		{ID: "b", URL: sshURL, SSHKeyPEM: config.NewSecret("pem-plain"), SSHPassphrase: config.NewSecret("pp-plain"),
			Kind: config.GitCredentialSSHKey},
	}}

	got := gitCredentials(cfg)

	want := []gitsource.Credential{
		{URL: basicURL, Username: "ci", Password: "pw-plain", SSHKey: []byte{}},
		{URL: sshURL, SSHKey: []byte("pem-plain"), SSHPassphrase: "pp-plain"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gitCredentials() = %+v, want %+v", got, want)
	}
	if got := gitCredentials(nil); got != nil {
		t.Errorf("gitCredentials(nil) = %v, want nil", got)
	}
}

// TestGitCredentialsEndToEnd drives a credential from the environment through
// the real install flag set and BuildCollectionConfig to the plain form the
// fetcher consumes, and pins that a broken binding fails the whole config.
func TestGitCredentialsEndToEnd(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	t.Setenv("GO_GALAXY_GIT_CREDENTIALS", "hub")
	t.Setenv("GO_GALAXY_GIT_HUB_URL", "https://git.example/org/")
	t.Setenv("GO_GALAXY_GIT_HUB_USERNAME", "ci")
	t.Setenv("GO_GALAXY_GIT_HUB_PASSWORD", "pw-plain")

	cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	got := gitCredentials(cfg)
	if len(got) != 1 || got[0].URL.String() != "https://git.example/org" || got[0].Username != "ci" || got[0].Password != "pw-plain" {
		t.Fatalf("gitCredentials() = %+v, want the one declared Basic credential", got)
	}

	t.Setenv("GO_GALAXY_GIT_HUB_PASSWORD", "")
	_, err = buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
	if !errors.Is(err, galaxyhelpers.ErrGitCredentialInvalid) {
		t.Fatalf("BuildCollectionConfig() error = %v, want errors.Is helpers.ErrGitCredentialInvalid", err)
	}
}
