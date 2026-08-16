package config

import (
	"strings"
	"testing"
)

// TestAnsibleSignatureKeysWarningNamesKeysInDeclarationOrder pins the two
// properties the message itself carries: it names the keys in
// signatureKeyNames' own order whatever order the file wrote them in, and it
// carries no value any of them was set to.
//
// The order matters because the file's order is chosen by whoever wrote the
// file, so two ansible.cfg files carrying the same four keys would otherwise
// produce two different lines for one identical situation.
//
// The no-value half is asserted against the parsed file rather than a
// hand-built Config, so it covers the parser and the renderer together: a
// parser that started storing values would have to be met by a renderer that
// refused to print them for this to keep passing.
//
// The empty row is the control that keeps the first two meaningful: a file
// carrying none of these keys produces no line at all, so "it warned" is a fact
// about the keys rather than about every run.
func TestAnsibleSignatureKeysWarningNamesKeysInDeclarationOrder(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name: "declaration order, not file order",
			input: "[galaxy]\n" +
				"disable_gpg_verify = yes\n" +
				"gpg_keyring = /repo/keys.gpg\n",
			want: []string{"gpg_keyring", "disable_gpg_verify"},
		},
		{
			name:  "one key",
			input: "[galaxy]\nrequired_valid_signature_count = 0\n",
			want:  []string{"required_valid_signature_count"},
		},
		{
			name:  "no signature keys, no warning",
			input: "[galaxy]\nserver = https://x\n",
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := parseAnsibleConfig(strings.NewReader(row.input))
			if err != nil {
				t.Fatalf("parseAnsibleConfig() error = %v, want nil", err)
			}
			cfg := &Config{AnsibleConfigPath: "/etc/ansible/ansible.cfg", AnsibleSignatureKeys: parsed.Galaxy.SignatureKeys}

			got := cfg.AnsibleSignatureKeysWarning()
			if len(row.want) == 0 {
				if got != "" {
					t.Fatalf("AnsibleSignatureKeysWarning() = %q, want an empty string", got)
				}
				return
			}
			if want := "(" + strings.Join(row.want, ", ") + ")"; !strings.Contains(got, want) {
				t.Fatalf("AnsibleSignatureKeysWarning() = %q, want it to name %s", got, want)
			}
			for _, value := range []string{"/repo/keys.gpg", "yes", "0"} {
				if strings.Contains(got, value) {
					t.Fatalf("AnsibleSignatureKeysWarning() = %q, must not carry the configured value %q", got, value)
				}
			}
		})
	}
}
