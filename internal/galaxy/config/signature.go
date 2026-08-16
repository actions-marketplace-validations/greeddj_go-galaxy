package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/signature"
	"github.com/urfave/cli/v3"
)

// envDisableGPGVerifyAnsible is ansible's own name for the disable switch. It
// is read in this package rather than declared as a flag source; see
// resolveDisableGPGVerify for why.
const envDisableGPGVerifyAnsible = "ANSIBLE_GALAXY_DISABLE_GPG_VERIFY"

// SignatureConfig is one run's signature verification surface as configured:
// where the key material lives, how many signatures must verify, which failure
// statuses are tolerated, and whether verification is switched off outright.
//
// It holds raw, resolved values rather than a signature.Policy, because the
// consumer builds that policy where it uses it, beside the key material it
// loads. Field names deliberately match the policy's own, so the two sides of
// signature.NewPolicy read identically.
type SignatureConfig struct {
	// KeyringPath is the keyring location with a leading "~" already expanded.
	// Empty means no keyring was configured, which is a run that verifies
	// nothing rather than one that verifies and fails.
	KeyringPath string
	// RequiredCount is the required-valid-signature-count spec as written, not
	// a number: "all" and the "+" strict marker are part of the grammar
	// signature.ParseCountSpec reads. It is never empty once
	// applySignatureConfig has run.
	RequiredCount string
	// IgnoreStatusCodes names the verification failure statuses this run
	// tolerates, one element per configured code, unvalidated.
	IgnoreStatusCodes []string
	// DisableGPGVerify switches verification off even when a keyring is
	// configured, which is a different fact from having configured no keyring.
	DisableGPGVerify bool
}

// applySignatureConfig resolves the signature surface into cfg.Signature.
//
// The whole surface is configured from flags and their environment variables,
// and from no other source. ansible.cfg deliberately does not feed it: this
// program cannot establish whether a discovered ansible.cfg was authored by the
// operator or by the repository under test, and a setting that can relax a
// verification check must not come from a file whose author it cannot
// establish. See the same argument on cliflags.SignatureFlags, which is where
// the four flags and their environment spellings are declared.
//
// It runs last in BuildCollectionConfig. That placement is deliberate rather
// than incidental: every config error ahead of it keeps the precedence it
// already had, so which failure a broken configuration reports first does not
// move because a signature surface was added behind it.
func applySignatureConfig(cfg *Config, c *cli.Command) error {
	if err := checkSuppliedSignatureValues(c); err != nil {
		return err
	}

	cfg.Signature.KeyringPath = expandHome(c.String("keyring"))
	cfg.Signature.RequiredCount = resolveRequiredCount(c)
	cfg.Signature.IgnoreStatusCodes = resolveIgnoreStatusCodes(c)

	disabled, err := resolveDisableGPGVerify(c)
	if err != nil {
		return err
	}
	cfg.Signature.DisableGPGVerify = disabled

	if warning := disabledWithKeyringWarning(cfg.Signature); warning != "" {
		cfg.Warnings = append(cfg.Warnings, warning)
	}

	return validateSignatureConfig(cfg.Signature)
}

// AnsibleSignatureKeysWarning renders the one warning a discovered ansible.cfg
// carrying signature keys earns, or "" when it carried none.
//
// The keys are named and their values are not, because no value was ever read -
// see ansibleGalaxyConfig.SignatureKeys. The message says what the run does
// rather than what the file says, since the file said something this program
// deliberately did not listen to.
//
// It is returned rather than queued on Warnings, which every other warning this
// package produces uses. That queue is drained by runCollectionCommand for
// every command, so lock, outdated and cleanup would each emit a line about
// verification that could never have happened on them; this one belongs to the
// commands that verify, which is why it is a value their own setup asks for and
// prints once per run.
//
// The order is signatureKeyNames' own rather than the file's: the message reads
// the same for two files that carry the same keys in a different order.
func (c *Config) AnsibleSignatureKeysWarning() string {
	if c == nil || len(c.AnsibleSignatureKeys) == 0 {
		return ""
	}
	named := make([]string, 0, len(signatureKeyNames))
	for _, key := range signatureKeyNames {
		if slices.Contains(c.AnsibleSignatureKeys, key) {
			named = append(named, key)
		}
	}

	return fmt.Sprintf(
		"ansible.cfg %s configures signature verification (%s); go-galaxy reads none of it - "+
			"configure the keyring and its policy through --keyring and its sibling flags, or their environment variables",
		c.AnsibleConfigPath, strings.Join(named, ", "))
}

// emptyRefusingSignatureFlag is one flag whose explicitly supplied empty value
// is refused, together with the remedy its refusal names.
type emptyRefusingSignatureFlag struct {
	name   string
	remedy string
}

// checkSuppliedSignatureValues refuses an explicitly supplied empty value for
// the two signature settings that name something rather than switch something.
//
// The predicate is "some source set this flag AND the value is empty", which is
// the only shape that distinguishes a failed expression from an omission: a
// flag nobody set reads its default here and is left alone. No trimming is
// applied, because the two whitespace-only values already fail with better
// messages than this one could - a count against ParseCountSpec's anchored
// grammar, a keyring at the open that names the literal path.
//
// The list is those two settings and stays those two. An empty
// disable-gpg-verify reads false and an empty ignore list tolerates nothing:
// each has a well-defined meaning in the direction that keeps verification on,
// so neither is a failed expression this could recognize. See
// helpers.ErrEmptySignatureValue for what the refusal buys.
func checkSuppliedSignatureValues(c *cli.Command) error {
	flags := [...]emptyRefusingSignatureFlag{
		{name: "keyring", remedy: "omit it entirely to run without signature verification"},
		{name: "required-valid-signature-count", remedy: "omit it entirely to use the default"},
	}

	for _, flag := range flags {
		if c.IsSet(flag.name) && c.String(flag.name) == "" {
			return fmt.Errorf("%w: --%s (or the environment variable feeding it) is empty; %s",
				helpers.ErrEmptySignatureValue, flag.name, flag.remedy)
		}
	}

	return nil
}

// resolveRequiredCount resolves the required-valid-signature-count spec,
// falling back to helpers.DefaultRequiredValidSignatureCount when nothing
// supplied a value at all.
//
// That fallback is load-bearing rather than defensive, and the predicate it
// covers is a property of the command rather than a count of them: a command
// that does not register the flag reads the Go zero value of an unknown flag
// name - "" - so the flag's own Value never applies to it. Without the
// fallback, every run of such a command would hand signature.ParseCountSpec an
// empty spec, which is not a spelling its grammar accepts, and
// BuildCollectionConfig - which validates the count for every command
// regardless of what that command declared - would fail on every invocation.
//
// It never covers a value some source supplied as empty:
// checkSuppliedSignatureValues has already refused that shape by the time this
// runs, which is what keeps "nothing was configured" and "a configured
// expression evaluated to nothing" from resolving the same way.
func resolveRequiredCount(c *cli.Command) string {
	if count := c.String("required-valid-signature-count"); count != "" {
		return count
	}

	return helpers.DefaultRequiredValidSignatureCount
}

// resolveIgnoreStatusCodes resolves the tolerated-status list, which the flag
// and its environment variables are the only sources of. Each element is passed
// on exactly as the CLI library produced it, leaving
// signature.ParseStatusCodes the single authority over what an element may be -
// including the trimming an environment list needs, since urfave/cli splits an
// environment value on "," without trimming what it produces.
func resolveIgnoreStatusCodes(c *cli.Command) []string {
	return c.StringSlice("ignore-signature-status-code")
}

// resolveDisableGPGVerify resolves whether verification is switched off: the
// flag wins whenever some source set it, otherwise envDisableGPGVerifyAnsible
// is read here, and an absent value is false.
//
// That variable is read in this layer instead of being a source of the flag,
// which is the shape ansibleGalaxyServer already uses for
// ANSIBLE_GALAXY_SERVER and whose doc comment holds the general argument. The
// reason here is grammar rather than precedence: ansible's boolean vocabulary
// (true/false, yes/no, on/off, 1/0) is wider than the one urfave/cli's bool
// source accepts, so "yes" reaching it as a flag source aborts the whole
// command with a parse failure. That is fail-closed but it is not drop-in, and
// an environment already exporting "no" for ansible would make every go-galaxy
// command fail - pressure to delete the variable, which is the unsafe
// direction. The other three ansible spellings need no such handling: they are
// a string, a string and a list, all of which urfave/cli parses correctly.
// Declaring this one as a string flag instead was considered and rejected,
// since it would cost --disable-gpg-verify its bare-switch form.
//
// A value set but empty reads as absent rather than as a refusal, matching what
// the go-galaxy spelling of the same flag already does with an empty value and
// keeping the two spellings of one setting from answering the same input
// differently. An unparseable value is a hard error naming it; see
// helpers.ErrInvalidDisableGPGVerify for why it is never guessed.
func resolveDisableGPGVerify(c *cli.Command) (bool, error) {
	if c.IsSet("disable-gpg-verify") {
		return c.Bool("disable-gpg-verify"), nil
	}

	raw := os.Getenv(envDisableGPGVerifyAnsible)
	if raw == "" {
		return false, nil
	}
	disabled, ok := parseAnsibleBool(raw)
	if !ok {
		return false, fmt.Errorf("%w: $%s = %q", helpers.ErrInvalidDisableGPGVerify, envDisableGPGVerifyAnsible, raw)
	}

	return disabled, nil
}

// disabledWithKeyringWarning returns the one warning this surface produces, or
// "" when it does not apply.
//
// It exists because this tool honors the switch, which is a divergence in the
// less safe direction: an operator who configured a keyring configured it to
// have artifacts verified, and a run that quietly skips every verification
// while that keyring sits in the configuration looks exactly like a run that
// verified everything. Making the divergence loud costs one line on stderr and
// is the only thing standing between the two.
//
// The predicate is both halves rather than the switch alone: with no keyring
// configured, verification was never going to happen, so a warning there would
// fire on the default state of every run and become noise the one case that
// matters hides in. It states this run's own fact and nothing about what
// another tool would do with the same configuration, which is a claim about
// software this project does not control and cannot keep current.
func disabledWithKeyringWarning(sc SignatureConfig) string {
	if !sc.DisableGPGVerify || sc.KeyringPath == "" {
		return ""
	}

	return fmt.Sprintf(
		"signature verification is disabled while a keyring is configured (%q); no collection will be verified on this run",
		sc.KeyringPath)
}

// expandHome expands a leading "~" in a configured path, and nothing else.
//
// The whole rule: "" stays "", a bare "~" becomes the home directory, a "~/"
// prefix is joined onto it, and every other value - "~user/x" included - is
// returned unchanged. When the home directory cannot be determined the value
// is returned unchanged too, with no warning queued: the keyring open that
// follows fails with helpers.ErrKeyringUnreadable naming the literal value,
// which is both louder and more actionable than a warning about a path nobody
// asked about yet.
//
// "$VAR" is deliberately not expanded, even though ansible's own path type
// expands it. A second grammar over a security-relevant path buys an operator
// nothing that "~" does not, and an unexpanded "$HOME/x" fails loudly with the
// offending value named rather than quietly resolving somewhere unintended.
//
// The asymmetry with the rest of this package is stated rather than hidden:
// this expands the keyring path only. [defaults] collections_path and [galaxy]
// cache_dir are not expanded today, and changing that is an
// established-behavior change belonging to its own decision rather than a
// tidy-up riding along with this one.
func expandHome(path string) string {
	if path == "" {
		return path
	}
	rest, isHomeRelative := strings.CutPrefix(path, "~/")
	if path != "~" && !isHomeRelative {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}

	return filepath.Join(home, rest)
}

// validateSignatureConfig refuses a signature configuration the consumer could
// not act on, by building the policy it will build and keeping only the error.
//
// The built policy is dropped on purpose: the consumer assembles its own where
// it uses it, beside the key material it loads, and a second copy carried from
// here could only disagree with that one. What this call buys is WHEN the
// refusal happens - a malformed count spec or an unknown status code fails at
// config-build time, under the usage exit class, instead of inside an install
// worker after a run has already downloaded artifacts.
//
// It does not stat the keyring. Config opens exactly one file, the exit class
// is the same either way, and a stat here would only add a second answer about
// the keyring that can disagree with the real open.
//
// This is not a no-op to be deleted, and it does not make the consumer's own
// NewPolicy error arm unreachable: that arm must still be handled, because two
// call sites building the same policy from the same values is a disagreement
// nothing would report if one of them stopped checking.
func validateSignatureConfig(sc SignatureConfig) error {
	_, err := signature.NewPolicy(sc.KeyringPath, sc.RequiredCount, sc.IgnoreStatusCodes, sc.DisableGPGVerify)

	return err
}
