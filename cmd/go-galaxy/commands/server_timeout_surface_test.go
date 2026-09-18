package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ansibleCfgWithServerTimeout writes an ansible.cfg whose [galaxy] section
// sets server_timeout to value, points $ANSIBLE_CONFIG at it, and returns
// its path. Like ansibleCfgWithServerList it neutralizes discovery first and
// then overrides the variable that helper points at a missing file.
func ansibleCfgWithServerTimeout(t *testing.T, value string) string {
	t.Helper()
	neutralizeAnsibleDiscovery(t)
	path := filepath.Join(t.TempDir(), "ansible.cfg")
	body := "[galaxy]\nserver = https://hub.example/galaxy/ansible\nserver_timeout = " + value + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write ansible.cfg: %v", err)
	}
	t.Setenv("ANSIBLE_CONFIG", path)
	return path
}

// TestAnsibleServerTimeoutIsRead pins that ansible.cfg's [galaxy]
// server_timeout sets the request budget, in the precedence ansible applies
// to it: --timeout, then its environment spellings, then the file, then the
// default. It was once read by nothing at all, so a hub configured for 175
// seconds got 30 here, silently, while ansible-galaxy honored the same file.
//
// Each outranking row keeps the file's value in place, so it can only pass by
// the source it names winning over a file that was read - the first row is
// what shows the file is read at all. ANSIBLE_GALAXY_SERVER_TIMEOUT gets a row
// of its own because it is the one that is ansible's order and not merely
// this tool's.
func TestAnsibleServerTimeoutIsRead(t *testing.T) {
	t.Run("the file alone sets the timeout", func(t *testing.T) {
		ansibleCfgWithServerTimeout(t, "175")

		cfg := aliasCfg(t)
		assertConfigField(t, "Timeout", cfg.Timeout, 175*time.Second)
		assertConfigField(t, "AnsibleServerTimeoutUsed", cfg.AnsibleServerTimeoutUsed, true)
	})

	t.Run("a Go duration in the file is read like the flag's", func(t *testing.T) {
		ansibleCfgWithServerTimeout(t, "2m")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 2*time.Minute)
	})

	t.Run("the flag outranks the file", func(t *testing.T) {
		ansibleCfgWithServerTimeout(t, "175")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), []string{"--timeout=45s"})
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "Timeout", cfg.Timeout, 45*time.Second)
		assertConfigField(t, "AnsibleServerTimeoutUsed", cfg.AnsibleServerTimeoutUsed, false)
	})

	t.Run("ansible's own variable outranks the file", func(t *testing.T) {
		ansibleCfgWithServerTimeout(t, "175")
		t.Setenv("ANSIBLE_GALAXY_SERVER_TIMEOUT", "60")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 60*time.Second)
	})

	t.Run("this tool's variable outranks the file", func(t *testing.T) {
		ansibleCfgWithServerTimeout(t, "175")
		t.Setenv("GO_GALAXY_SERVER_TIMEOUT", "90s")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 90*time.Second)
	})

	t.Run("a file without the key leaves the default", func(t *testing.T) {
		ansibleCfgWithServerList(t)

		cfg := aliasCfg(t)
		assertConfigField(t, "Timeout", cfg.Timeout, galaxyhelpers.FetchDefaultTimeout)
		assertConfigField(t, "AnsibleServerTimeoutUsed", cfg.AnsibleServerTimeoutUsed, false)
	})
}

// TestAnsibleServerTimeoutRefusesAMalformedValue pins what a server_timeout
// the timeout grammar refuses does: it fails the run as the invalid timeout
// it is, naming the file and the key, rather than falling back to a default
// the operator never chose - which is what ignoring the key amounted to.
//
// The two rows after it bound that refusal to the runs the value could have
// affected. A flag that outranks the file leaves the file's value unused, and
// cleanup registers no --timeout and makes no request the budget bounds, so
// neither may fail over it.
func TestAnsibleServerTimeoutRefusesAMalformedValue(t *testing.T) {
	for _, value := range []string{"soon", "0", "-5"} {
		t.Run("install refuses "+value, func(t *testing.T) {
			path := ansibleCfgWithServerTimeout(t, value)

			_, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
			if !errors.Is(err, galaxyhelpers.ErrInvalidTimeout) {
				t.Fatalf("BuildCollectionConfig() error = %v, want ErrInvalidTimeout", err)
			}
			if msg := err.Error(); !strings.Contains(msg, path) || !strings.Contains(msg, "server_timeout") {
				t.Errorf("error %q does not name both the file and the key", msg)
			}
		})
	}

	t.Run("an outranking flag leaves it unread", func(t *testing.T) {
		ansibleCfgWithServerTimeout(t, "soon")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), []string{"--timeout=45s"})
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "Timeout", cfg.Timeout, 45*time.Second)
	})

	t.Run("cleanup does not read it", func(t *testing.T) {
		ansibleCfgWithServerTimeout(t, "soon")

		cfg, err := buildConfigFor(t, "cleanup", cliflags.S3Flags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "AnsibleServerTimeoutUsed", cfg.AnsibleServerTimeoutUsed, false)
	})
}
