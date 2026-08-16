package commands

// This file pins the ansible.cfg half of the signature surface at the seam an
// operator actually crosses: a real file on disk, discovered the way discovery
// discovers one, resolved through the real BuildCollectionConfig with the flag
// set install really mounts.
//
// internal/galaxy/config pins the parser and the warning text; what neither can
// pin from inside that package is that the parsed names survive the whole
// config build and land on the Config a command consumes.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ansibleCfgWithSignatureKeys writes an ansible.cfg carrying all four of
// ansible's signature keys, each with a value this program must not read, and
// points $ANSIBLE_CONFIG at it.
//
// It runs neutralizeAnsibleDiscovery first and then overrides $ANSIBLE_CONFIG,
// for the reason ansibleCfgWithServerList already gives: that helper points the
// variable at a file that does not exist, which is the opposite of what this
// needs.
func ansibleCfgWithSignatureKeys(t *testing.T) string {
	t.Helper()
	neutralizeAnsibleDiscovery(t)
	path := filepath.Join(t.TempDir(), "ansible.cfg")
	body := "[galaxy]\n" +
		"gpg_keyring = /repo/keys.gpg\n" +
		"required_valid_signature_count = 0\n" +
		"ignore_signature_status_codes = BADSIG\n" +
		"disable_gpg_verify = yes\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write ansible.cfg: %v", err)
	}
	t.Setenv("ANSIBLE_CONFIG", path)

	return path
}

// TestAnsibleSignatureKeysReachTheConfig proves the detection half of the
// ansible.cfg story is wired: a discovered file carrying the four signature
// keys leaves their NAMES on the Config a verifying command reads, which is
// what its one warning is rendered from.
//
// The second half of the assertion is the security property, and it is checked
// through the same real config build rather than against the parser alone: not
// one of the four VALUES reaches cfg.Signature. The keyring stays empty, the
// count resolves to the flag default rather than to the file's 0, and the
// disable switch stays false rather than following the file's "yes" - so a
// repository that drops an ansible.cfg into a checkout can neither point the
// keyring somewhere else nor relax the policy nor switch verification off.
//
// KILLING MUTATION, run and reverted: the assignment
// `cfg.AnsibleSignatureKeys = ansibleConfig.Galaxy.SignatureKeys` deleted from
// applyAnsibleConfig (internal/galaxy/config/config.go), which leaves the
// parser recording the names and nothing carrying them to the run:
//
//	ansible_signature_keys_surface_test.go:75: AnsibleSignatureKeys = [], want the four ansible signature key names
func TestAnsibleSignatureKeysReachTheConfig(t *testing.T) {
	path := ansibleCfgWithSignatureKeys(t)

	cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), nil)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}

	want := []string{"gpg_keyring", "required_valid_signature_count", "ignore_signature_status_codes", "disable_gpg_verify"}
	if got := cfg.AnsibleSignatureKeys; !slices.Equal(got, want) {
		t.Fatalf("AnsibleSignatureKeys = %v, want the four ansible signature key names", got)
	}
	assertConfigField(t, "AnsibleConfigPath", cfg.AnsibleConfigPath, path)

	assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "")
	assertConfigField(t, "Signature.RequiredCount", cfg.Signature.RequiredCount,
		galaxyhelpers.DefaultRequiredValidSignatureCount)
	assertConfigField(t, "Signature.DisableGPGVerify", cfg.Signature.DisableGPGVerify, false)
	if len(cfg.Signature.IgnoreStatusCodes) != 0 {
		t.Fatalf("Signature.IgnoreStatusCodes = %v, want none: ansible.cfg must contribute no signature value", cfg.Signature.IgnoreStatusCodes)
	}
}
