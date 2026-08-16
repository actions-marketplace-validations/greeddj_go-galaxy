package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// signatureEnvNames returns every environment variable this surface reads,
// go-galaxy's own spellings and ansible's alike.
func signatureEnvNames() []string {
	return []string{
		"GO_GALAXY_KEYRING",
		"ANSIBLE_GALAXY_GPG_KEYRING",
		"GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
		"ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
		"GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE",
		"ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES",
		"GO_GALAXY_DISABLE_GPG_VERIFY",
		envDisableGPGVerifyAnsible,
	}
}

// clearSignatureEnv removes every signature variable from this test's
// environment, so a row's verdict is a function of the row rather than of the
// machine it runs on. t.Setenv is called first purely for the restore it
// registers; there is no t.Unsetenv to do the same.
func clearSignatureEnv(t *testing.T) {
	t.Helper()
	for _, name := range signatureEnvNames() {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v, want nil", name, err)
		}
	}
}

// newSignatureCmd builds a *cli.Command carrying the four signature flags in
// the shape cmd/go-galaxy/cliflags declares them: the count's default Value
// and, for each flag, go-galaxy's own environment spelling. It is a hand-built
// copy because this package cannot import the command layer, so the real flag
// wiring is pinned at the command level instead - the same split
// TestWorkersEnvShapes (cmd/go-galaxy/commands) already documents. What this
// fixture pins is the resolution, not the declaration.
func newSignatureCmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "keyring", Sources: cli.EnvVars("GO_GALAXY_KEYRING")},
			&cli.StringFlag{
				Name:    "required-valid-signature-count",
				Value:   helpers.DefaultRequiredValidSignatureCount,
				Sources: cli.EnvVars("GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT"),
			},
			&cli.StringSliceFlag{
				Name:    "ignore-signature-status-code",
				Sources: cli.EnvVars("GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE"),
			},
			&cli.BoolFlag{Name: "disable-gpg-verify", Sources: cli.EnvVars("GO_GALAXY_DISABLE_GPG_VERIFY")},
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

// signatureRow is one resolution scenario: which environment variable is
// exported and with what, and what the command line says. A row leaving both
// empty is the "nobody supplied a value" case, whose answer is the flag's own
// default.
type signatureRow struct {
	name     string
	envName  string
	envValue string
	args     []string
}

// resolveSignature runs one row through applySignatureConfig and returns the
// resolved Config, failing the test if the resolution was refused.
func resolveSignature(t *testing.T, row signatureRow) *Config {
	t.Helper()
	clearSignatureEnv(t)
	if row.envName != "" {
		t.Setenv(row.envName, row.envValue)
	}

	cfg := &Config{}
	if err := applySignatureConfig(cfg, newSignatureCmd(t, row.args)); err != nil {
		t.Fatalf("applySignatureConfig() error = %v, want nil", err)
	}
	return cfg
}

// keyringRow is a signatureRow plus the keyring path the resolution must land
// on.
type keyringRow struct {
	want string
	row  signatureRow
}

// TestApplySignatureConfigKeyringPrecedence pins the keyring's precedence: an
// explicitly set flag beats an environment variable, which beats the flag's own
// default. The rows are cumulative - the last adds the higher-precedence source
// on top of the one below - so its assertion also proves the lower source was
// present and lost.
func TestApplySignatureConfigKeyringPrecedence(t *testing.T) {
	rows := []keyringRow{
		{row: signatureRow{name: "nothing supplies it"}, want: ""},
		{
			row: signatureRow{
				name:    "the environment supplies it",
				envName: "GO_GALAXY_KEYRING", envValue: "/env.gpg",
			},
			want: "/env.gpg",
		},
		{
			row: signatureRow{
				name:    "the flag outranks the environment",
				envName: "GO_GALAXY_KEYRING", envValue: "/env.gpg",
				args: []string{"--keyring=/flag.gpg"},
			},
			want: "/flag.gpg",
		},
	}

	for _, row := range rows {
		t.Run(row.row.name, func(t *testing.T) {
			if got := resolveSignature(t, row.row).Signature.KeyringPath; got != row.want {
				t.Fatalf("KeyringPath = %q, want %q", got, row.want)
			}
		})
	}
}

// countRow is a signatureRow plus the count spec the resolution must land on.
type countRow struct {
	want string
	row  signatureRow
}

// TestApplySignatureConfigRequiredCountPrecedence pins the count spec's
// precedence, in the same cumulative shape as the keyring rows above. The
// values are deliberately different spellings of the grammar rather than
// different numbers, so a row cannot pass on a value some other source
// happened to supply.
func TestApplySignatureConfigRequiredCountPrecedence(t *testing.T) {
	rows := []countRow{
		{row: signatureRow{name: "nothing supplies it"}, want: helpers.DefaultRequiredValidSignatureCount},
		{
			row: signatureRow{
				name:    "the environment supplies it",
				envName: "GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", envValue: "+2",
			},
			want: "+2",
		},
		{
			row: signatureRow{
				name:    "the flag outranks the environment",
				envName: "GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", envValue: "+2",
				args: []string{"--required-valid-signature-count=+all"},
			},
			want: "+all",
		},
	}

	for _, row := range rows {
		t.Run(row.row.name, func(t *testing.T) {
			if got := resolveSignature(t, row.row).Signature.RequiredCount; got != row.want {
				t.Fatalf("RequiredCount = %q, want %q", got, row.want)
			}
		})
	}
}

// codesRow is a signatureRow plus the tolerated-status list the resolution
// must land on.
type codesRow struct {
	row  signatureRow
	want []string
}

// TestApplySignatureConfigIgnoreStatusCodesPrecedence pins the ignore list's
// precedence, and one thing besides: the environment row's value carries a
// space after its separator, which arrives unchanged, so the row also states
// that this layer hands the elements on exactly as the CLI library split them.
func TestApplySignatureConfigIgnoreStatusCodesPrecedence(t *testing.T) {
	rows := []codesRow{
		{row: signatureRow{name: "nothing supplies it"}, want: nil},
		{
			row: signatureRow{
				name:    "the environment supplies it",
				envName: "GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE", envValue: "BADSIG, NO_PUBKEY",
			},
			want: []string{"BADSIG", " NO_PUBKEY"},
		},
		{
			row: signatureRow{
				name:    "the flag outranks the environment",
				envName: "GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE", envValue: "BADSIG, NO_PUBKEY",
				args: []string{"--ignore-signature-status-code=EXPSIG"},
			},
			want: []string{"EXPSIG"},
		},
	}

	for _, row := range rows {
		t.Run(row.row.name, func(t *testing.T) {
			if got := resolveSignature(t, row.row).Signature.IgnoreStatusCodes; !slices.Equal(got, row.want) {
				t.Fatalf("IgnoreStatusCodes = %q, want %q", got, row.want)
			}
		})
	}
}

// disableRow is one ANSIBLE_GALAXY_DISABLE_GPG_VERIFY value and the verdict it
// must resolve to, or the refusal it must produce instead.
type disableRow struct {
	name     string
	value    string
	want     bool
	wantErr  bool
	unsetEnv bool
}

// TestApplySignatureConfigDisableAnsibleEnv pins the ansible spelling of the
// disable switch, which this layer reads itself rather than handing to the
// flag. The four rows that matter are ansible's own boolean vocabulary beyond
// Go's: yes, no, on and off each resolve here, where a urfave/cli bool source
// would abort the command over them.
//
// The true/false row is the control that the reading happens at all rather than
// this being a table of values nothing consults, and the unparseable row states
// that a value outside the vocabulary is refused rather than guessed. An empty
// value reads as absent, matching what the go-galaxy spelling of the same flag
// does with one.
func TestApplySignatureConfigDisableAnsibleEnv(t *testing.T) {
	rows := []disableRow{
		{name: "unset is false", unsetEnv: true},
		{name: "empty reads as absent", value: ""},
		{name: "true", value: "true", want: true},
		{name: "false", value: "false"},
		{name: "yes", value: "yes", want: true},
		{name: "no", value: "no"},
		{name: "on", value: "on", want: true},
		{name: "off", value: "off"},
		{name: "mixed case is accepted", value: "YeS", want: true},
		{name: "an unparseable value is refused", value: "maybe", wantErr: true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSignatureEnv(t)
			if !row.unsetEnv {
				t.Setenv(envDisableGPGVerifyAnsible, row.value)
			}

			cfg := &Config{}
			err := applySignatureConfig(cfg, newSignatureCmd(t, nil))
			if row.wantErr {
				if !errors.Is(err, helpers.ErrInvalidDisableGPGVerify) {
					t.Fatalf("applySignatureConfig() error = %v, want %v", err, helpers.ErrInvalidDisableGPGVerify)
				}
				return
			}
			if err != nil {
				t.Fatalf("applySignatureConfig() error = %v, want nil", err)
			}
			if cfg.Signature.DisableGPGVerify != row.want {
				t.Fatalf("DisableGPGVerify = %v, want %v", cfg.Signature.DisableGPGVerify, row.want)
			}
		})
	}
}

// TestApplySignatureConfigDisableFlagOutranksAnsibleEnv states the precedence
// between the two spellings of the switch, which the table above cannot: the
// flag wins whenever some source set it, and the row makes the two disagree so
// that only reading the flag produces the expected answer.
func TestApplySignatureConfigDisableFlagOutranksAnsibleEnv(t *testing.T) {
	clearSignatureEnv(t)
	t.Setenv(envDisableGPGVerifyAnsible, "no")

	cfg := &Config{}
	if err := applySignatureConfig(cfg, newSignatureCmd(t, []string{"--disable-gpg-verify"})); err != nil {
		t.Fatalf("applySignatureConfig() error = %v, want nil", err)
	}
	if !cfg.Signature.DisableGPGVerify {
		t.Fatal("DisableGPGVerify = false, want true (the flag outranks the ansible variable)")
	}
}

// TestExpandHome pins the whole rule and nothing beyond it: "~" and a "~/"
// prefix are expanded, and every other shape - including the "~user" form a
// shell would expand and a tilde that is not the first element - is returned
// unchanged.
//
// $HOME is redirected so the expansion has a value this test knows; the rows
// asserting no expansion are what keep "expanded" distinguishable from
// "returned whatever it was given".
func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rows := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "bare tilde becomes the home directory", in: "~", want: home},
		{name: "tilde slash is joined onto it", in: "~/keys.gpg", want: filepath.Join(home, "keys.gpg")},
		{name: "another user's tilde form is untouched", in: "~other/keys.gpg", want: "~other/keys.gpg"},
		{name: "a tilde that is not the first element is untouched", in: "a/~", want: "a/~"},
		{name: "an absolute path is untouched", in: "/etc/keys.gpg", want: "/etc/keys.gpg"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := expandHome(row.in); got != row.want {
				t.Fatalf("expandHome(%q) = %q, want %q", row.in, got, row.want)
			}
		})
	}
}

// validationRow is one malformed signature setting and the sentinel the config
// build must refuse it with, or - for the positive control - a row carrying no
// defect at all.
type validationRow struct {
	wantErr error
	name    string
	args    []string
}

// TestApplySignatureConfigValidation pins that a signature setting no consumer
// could act on is refused where it is configured, under a named sentinel,
// rather than reaching an install worker.
//
// The last row is the positive control, and it is the same fixture shape as the
// two above it with valid values in place of the malformed ones: without it,
// "refused" would be indistinguishable from a fixture that never reaches the
// check at all.
//
// KILLING MUTATION, run and reverted: deleting the validateSignatureConfig call
// from applySignatureConfig, so it returns nil in its place. Both refusal rows
// fail:
//
//	signature_test.go:396: applySignatureConfig() error = <nil>, want invalid required valid signature count
//	signature_test.go:396: applySignatureConfig() error = <nil>, want unknown signature status code
func TestApplySignatureConfigValidation(t *testing.T) {
	rows := []validationRow{
		{
			name:    "a malformed count spec is refused",
			args:    []string{"--required-valid-signature-count=some"},
			wantErr: helpers.ErrInvalidSignatureCount,
		},
		{
			name:    "an unknown status code is refused",
			args:    []string{"--ignore-signature-status-code=NOT_A_CODE"},
			wantErr: helpers.ErrUnknownSignatureStatusCode,
		},
		{
			name: "the same fixture with valid values builds cleanly",
			args: []string{"--required-valid-signature-count=+1", "--ignore-signature-status-code=NO_PUBKEY"},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSignatureEnv(t)
			cfg := &Config{}
			err := applySignatureConfig(cfg, newSignatureCmd(t, row.args))
			if row.wantErr == nil {
				if err != nil {
					t.Fatalf("applySignatureConfig() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, row.wantErr) {
				t.Fatalf("applySignatureConfig() error = %v, want %v", err, row.wantErr)
			}
		})
	}
}

// emptyValueRow is one way a setting can arrive empty, and whether the config
// build must refuse it.
type emptyValueRow struct {
	name     string
	envName  string
	args     []string
	wantErr  bool
	setEmpty bool
}

// TestApplySignatureConfigRefusesEmptyValues pins the difference between
// omitting a setting and supplying it as an expression that evaluated to
// nothing. The shape this exists for is a CI block writing a secret into an
// environment variable on a fork's pull request, where secrets are withheld: an
// empty keyring would read as "verify nothing", and an empty count would
// silently replace a configured "+all" with the default.
//
// The rows come in pairs. Each refusal is set through the environment and
// through the flag, since only IsSet distinguishes them from an omission, and
// the two omission rows are the control - without them, "empty is refused"
// could not be told from "this fixture refuses everything". The last two rows
// are the negative half of the predicate: the switch and the list have a
// well-defined empty meaning in the safe direction and must keep resolving.
func TestApplySignatureConfigRefusesEmptyValues(t *testing.T) {
	rows := []emptyValueRow{
		{name: "an empty keyring flag is refused", args: []string{"--keyring="}, wantErr: true},
		{name: "an empty keyring variable is refused", envName: "GO_GALAXY_KEYRING", setEmpty: true, wantErr: true},
		{name: "an empty count flag is refused", args: []string{"--required-valid-signature-count="}, wantErr: true},
		{
			name:    "an empty count variable is refused",
			envName: "GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", setEmpty: true, wantErr: true,
		},
		{name: "an omitted keyring is accepted"},
		{name: "an omitted count is accepted"},
		{name: "an empty disable variable is accepted", envName: "GO_GALAXY_DISABLE_GPG_VERIFY", setEmpty: true},
		{
			name:    "an empty ignore list variable is accepted",
			envName: "GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE", setEmpty: true,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSignatureEnv(t)
			if row.setEmpty {
				t.Setenv(row.envName, "")
			}

			err := applySignatureConfig(&Config{}, newSignatureCmd(t, row.args))
			if row.wantErr {
				if !errors.Is(err, helpers.ErrEmptySignatureValue) {
					t.Fatalf("applySignatureConfig() error = %v, want %v", err, helpers.ErrEmptySignatureValue)
				}
				return
			}
			if err != nil {
				t.Fatalf("applySignatureConfig() error = %v, want nil", err)
			}
		})
	}
}

// TestApplySignatureConfigEmptyCountKeepsNoDefault states what the refusal
// above buys, which the refusal rows themselves do not: an empty count must not
// resolve to the default, because the value it would replace is a stricter
// policy the operator configured and the substitution would be silent.
func TestApplySignatureConfigEmptyCountKeepsNoDefault(t *testing.T) {
	clearSignatureEnv(t)
	t.Setenv("GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", "")

	cfg := &Config{}
	err := applySignatureConfig(cfg, newSignatureCmd(t, nil))
	if !errors.Is(err, helpers.ErrEmptySignatureValue) {
		t.Fatalf("applySignatureConfig() error = %v, want %v", err, helpers.ErrEmptySignatureValue)
	}
	if cfg.Signature.RequiredCount == helpers.DefaultRequiredValidSignatureCount {
		t.Fatalf("RequiredCount = %q, want the run refused rather than defaulted", cfg.Signature.RequiredCount)
	}
}

// warningRow is one combination of the switch and the keyring, and whether the
// run must say something about it.
type warningRow struct {
	name        string
	args        []string
	wantWarning bool
}

// TestApplySignatureConfigDisabledWithKeyringWarning pins the one warning this
// surface produces: verification switched off while a keyring is configured is
// a run that verifies nothing with a configuration that says it should.
//
// The two silent rows are the control. Without them, "it warned" could not be
// told from "it warns on every run", which is the failure mode that would make
// the warning worthless in the case it exists for.
func TestApplySignatureConfigDisabledWithKeyringWarning(t *testing.T) {
	rows := []warningRow{
		{
			name:        "disabled with a keyring warns",
			args:        []string{"--disable-gpg-verify", "--keyring=/keys.gpg"},
			wantWarning: true,
		},
		{name: "disabled with no keyring is silent", args: []string{"--disable-gpg-verify"}},
		{name: "a keyring with verification on is silent", args: []string{"--keyring=/keys.gpg"}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSignatureEnv(t)
			cfg := &Config{}
			if err := applySignatureConfig(cfg, newSignatureCmd(t, row.args)); err != nil {
				t.Fatalf("applySignatureConfig() error = %v, want nil", err)
			}
			if got := len(cfg.Warnings) > 0; got != row.wantWarning {
				t.Fatalf("warnings = %v, want a warning: %v", cfg.Warnings, row.wantWarning)
			}
		})
	}
}

// newFlaglessCmd builds a *cli.Command registering no flags at all, which is
// what every command looks like to this resolution today: nothing mounts the
// signature flags yet, so each of them reads the Go zero value of an unknown
// flag name.
func newFlaglessCmd(t *testing.T) *cli.Command {
	t.Helper()

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
	return captured
}

// TestApplySignatureConfigCountFallback pins the fallback that lets a command
// registering none of these flags build a config at all: with no flag to read a
// default from, the count still resolves to the default spec rather than to the
// empty string no grammar accepts.
//
// The expected value is spelled out rather than taken from
// helpers.DefaultRequiredValidSignatureCount, so that changing that constant is
// a decision this test reports instead of one it silently follows.
//
// The error check sits after the value check on purpose: the refusal an empty
// spec produces renders the whole grammar, which says less about what broke than
// the resolved value does. It is documentary rather than pinned - on this
// fixture nothing can fail it while the assertion above passes.
//
// KILLING MUTATION, run and reverted: deleting the empty-value fallback from
// resolveRequiredCount, so it returns whatever the flag lookup produced:
//
//	signature_test.go:567: RequiredCount = "", want "1"
func TestApplySignatureConfigCountFallback(t *testing.T) {
	clearSignatureEnv(t)

	cfg := &Config{}
	err := applySignatureConfig(cfg, newFlaglessCmd(t))
	if got := cfg.Signature.RequiredCount; got != "1" {
		t.Fatalf("RequiredCount = %q, want %q", got, "1")
	}
	if err != nil {
		t.Fatalf("applySignatureConfig() error = %v, want nil", err)
	}
}
