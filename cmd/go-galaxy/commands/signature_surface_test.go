package commands

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// signatureSurfaceFlags is the flag set a command that verifies signatures
// really mounts, taken from the command itself rather than reassembled here.
// A union rebuilt in this file would pass every test below while the shipped
// command mounted something else entirely, which is the one failure these
// tests exist to make impossible.
//
// install is the representative: it and warm are the two commands that verify,
// and both compose the same three sets in the same order.
func signatureSurfaceFlags() []cli.Flag {
	return Install().Flags
}

// TestSignatureFlagsRoundTrip pins that every value the four signature flags
// accept reaches the matching Config field, driven through the real flag
// declarations rather than a hand-built copy of them - which is the half
// internal/galaxy/config cannot cover, since it cannot import this layer.
func TestSignatureFlagsRoundTrip(t *testing.T) {
	neutralizeAnsibleDiscovery(t)

	args := []string{
		"--keyring=/keys.gpg",
		"--required-valid-signature-count=+2",
		"--ignore-signature-status-code=BADSIG",
		"--ignore-signature-status-code=NO_PUBKEY",
		"--disable-gpg-verify",
	}
	cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), args)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}

	assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "/keys.gpg")
	assertConfigField(t, "Signature.RequiredCount", cfg.Signature.RequiredCount, "+2")
	assertConfigField(t, "Signature.DisableGPGVerify", cfg.Signature.DisableGPGVerify, true)
	if got := cfg.Signature.IgnoreStatusCodes; !slices.Equal(got, []string{"BADSIG", "NO_PUBKEY"}) {
		t.Fatalf("Signature.IgnoreStatusCodes = %v, want [BADSIG NO_PUBKEY]", got)
	}
}

// TestKeyringEnvNames pins the keyring flag's two environment spellings and
// their order. The first row is the convention this repository states on its
// timeout and download-path flags - every flag accepts GO_GALAXY_<FLAG_NAME> -
// and a new flag arriving without one would be a fresh exception to it. The
// second row is what makes the first a statement about precedence rather than
// about membership.
func TestKeyringEnvNames(t *testing.T) {
	t.Run("the go-galaxy spelling is read", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_KEYRING", "/from-go-galaxy.gpg")

		cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "/from-go-galaxy.gpg")
	})

	t.Run("it outranks the ansible spelling", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_KEYRING", "/from-go-galaxy.gpg")
		t.Setenv("ANSIBLE_GALAXY_GPG_KEYRING", "/from-ansible.gpg")

		cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "/from-go-galaxy.gpg")
	})
}

// TestIgnoreStatusCodesEnvSplit pins third-party behavior this surface is
// built on, measured against urfave/cli v3.10.1: a StringSliceFlag fed from
// an environment variable splits the value on "," and does NOT trim the
// elements, so the second element here arrives with the space that followed
// the separator.
//
// That is why signature.ParseStatusCodes trims each element itself, and why
// removing that trim as redundant would break exactly this shape - the one an
// operator writes when they space out a list for readability. The value is
// spelled with a space for that reason: a list written without one would pass
// either way and prove nothing about the composition.
func TestIgnoreStatusCodesEnvSplit(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	t.Setenv("ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES", "BADSIG, NO_PUBKEY")

	cfg, err := buildConfigFor(t, "install", signatureSurfaceFlags(), nil)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	if got := cfg.Signature.IgnoreStatusCodes; !slices.Equal(got, []string{"BADSIG", " NO_PUBKEY"}) {
		t.Fatalf("Signature.IgnoreStatusCodes = %q, want [\"BADSIG\" \" NO_PUBKEY\"]", got)
	}
}

// runCommandWith runs the named command carrying flags and returns whatever
// app.Run produced, without failing the test on an error - which is what
// separates it from buildConfigFor, whose contract is that the parse succeeds.
func runCommandWith(t *testing.T, name string, flags []cli.Flag, args []string) error {
	t.Helper()

	app := &cli.Command{
		Name:  "go-galaxy",
		Flags: cliflags.CommonFlags(),
		Commands: []*cli.Command{
			{
				Name:   name,
				Flags:  flags,
				Action: func(_ context.Context, _ *cli.Command) error { return nil },
			},
		},
	}

	return app.Run(context.Background(), append([]string{"go-galaxy", name}, args...))
}

// TestSignatureFlagsAreNotPartOfCollectionFlags pins the split: the signature
// flags are their own constructor, so a command mounting CollectionFlags alone
// does not silently advertise settings it cannot honor.
//
// The second row is the positive control on the same harness and the same
// argument: the flag set install really mounts makes the identical command
// line parse, so the refusal above is the flag set's doing rather than the
// harness rejecting everything.
func TestSignatureFlagsAreNotPartOfCollectionFlags(t *testing.T) {
	neutralizeAnsibleDiscovery(t)

	t.Run("collection flags alone refuse it", func(t *testing.T) {
		err := runCommandWith(t, "install", cliflags.CollectionFlags(), []string{"--keyring=/keys.gpg"})
		if err == nil {
			t.Fatal("app.Run() error = nil, want an unknown-flag refusal")
		}
		if !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("app.Run() error = %v, want an unknown-flag refusal", err)
		}
	})

	t.Run("adding the signature flags accepts it", func(t *testing.T) {
		if err := runCommandWith(t, "install", signatureSurfaceFlags(), []string{"--keyring=/keys.gpg"}); err != nil {
			t.Fatalf("app.Run() error = %v, want nil", err)
		}
	})
}

// TestVerifyingCommandsMountTheSignatureFlags pins the mount itself, on both
// commands that verify: cliflags.SignatureFlags' own rule is that a command
// mounts this set exactly when it verifies, and install and warm are the two
// that do. Driven through each real command rather than through a rebuilt
// union, so a mount deleted from either one fails here.
//
// warm is checked as well as install rather than assumed to follow it, because
// the two are separate constructors: an operator scripting both commands with
// one flag block is exactly who a missing mount on either would break, with
// "flag provided but not defined" rather than an ignored setting.
//
// KILLING MUTATION, run and reverted: the SignatureFlags append deleted from
// Warm (cmd/go-galaxy/commands/warm.go). Only the warm row fails:
//
//	signature_surface_test.go:176: warm does not mount --keyring
func TestVerifyingCommandsMountTheSignatureFlags(t *testing.T) {
	for _, cmd := range []*cli.Command{Install(), Warm()} {
		t.Run(cmd.Name, func(t *testing.T) {
			if !mountsFlag(cmd, "keyring") {
				t.Fatalf("%s does not mount --keyring", cmd.Name)
			}
			for _, name := range []string{"required-valid-signature-count", "ignore-signature-status-code", "disable-gpg-verify"} {
				if !mountsFlag(cmd, name) {
					t.Errorf("%s does not mount --%s", cmd.Name, name)
				}
			}
		})
	}
}

// mountsFlag reports whether cmd declares a flag by that name, matching on the
// name urfave/cli itself parses rather than on the flag's Go type, so a flag
// whose type changes still counts as mounted.
func mountsFlag(cmd *cli.Command, name string) bool {
	return slices.ContainsFunc(cmd.Flags, func(flag cli.Flag) bool {
		return slices.Contains(flag.Names(), name)
	})
}

// TestCleanupSignatureDefaults pins the resolution for a command that registers
// none of these flags. cleanup is one such command - lock and outdated are the
// others - and the shape it stands for is every command that does not verify.
// The count must still be a spec the grammar accepts, since
// BuildCollectionConfig validates it for every command regardless of which
// flags that command declared.
func TestCleanupSignatureDefaults(t *testing.T) {
	neutralizeAnsibleDiscovery(t)

	cfg, err := buildConfigFor(t, "cleanup", cliflags.S3Flags(), []string{"--cache-dir=" + t.TempDir()})
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	assertConfigField(t, "Signature.RequiredCount", cfg.Signature.RequiredCount,
		galaxyhelpers.DefaultRequiredValidSignatureCount)
	assertConfigField(t, "Signature.KeyringPath", cfg.Signature.KeyringPath, "")
}

// TestWarmParsesTheSignatureFlags is the parse-level half of warm's mount: an
// operator scripting install and warm with one flag block must not have warm
// die with "flag provided but not defined". It drives the real Warm().Flags
// through a no-op action, so what is exercised is the flag set the shipped
// command carries rather than a union rebuilt here.
func TestWarmParsesTheSignatureFlags(t *testing.T) {
	neutralizeAnsibleDiscovery(t)

	args := []string{
		"--keyring=/keys.gpg",
		"--required-valid-signature-count=+all",
		"--ignore-signature-status-code=BADSIG",
		"--disable-gpg-verify",
	}
	if err := runCommandWith(t, "warm", Warm().Flags, args); err != nil {
		t.Fatalf("app.Run(warm) error = %v, want nil", err)
	}
}
