package infra

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// recordingPrinter is a minimal output.Printer stub that records every
// Debugf line verbatim (format expanded via fmt semantics), so a test can
// assert on the exact rendered bytes rather than on call shape alone.
type recordingPrinter struct {
	debugLines []string
}

func (p *recordingPrinter) Printf(string, ...any)           {}
func (p *recordingPrinter) PersistentPrintf(string, ...any) {}
func (p *recordingPrinter) Okf(string, ...any)              {}
func (p *recordingPrinter) Errorf(string, ...any)           {}
func (p *recordingPrinter) Warnf(string, ...any)            {}

func (p *recordingPrinter) Debugf(format string, args ...any) {
	p.debugLines = append(p.debugLines, fmt.Sprintf(format, args...))
}

func (p *recordingPrinter) DebugSincef(time.Time, string, ...any) {}

// assertContainsAll fails the test unless line contains every one of want,
// naming the whole line once rather than repeating it per missing
// substring - kept as its own helper so the caller's branching stays flat
// enough for the linter's cyclomatic-complexity budget.
func assertContainsAll(t *testing.T, line string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(line, w) {
			t.Fatalf("debug line %q missing %q", line, w)
		}
	}
}

// TestDebugAnsibleConfigReportsServerListWithoutLeakingToken pins the
// invariant that a verbose run's resolved-server-list debug line names
// every server's id, URL, and TLS policy, and reports token presence as a
// boolean - never the plaintext token, however loud the run's verbosity.
func TestDebugAnsibleConfigReportsServerListWithoutLeakingToken(t *testing.T) {
	t.Parallel()
	const secretToken = "tok3n-must-not-appear-in-debug-output"

	printer := &recordingPrinter{}
	i := New(printer, nil)
	cfg := &config.Config{
		Servers: []config.Server{
			{ID: "a", URL: "https://a.example", Token: config.NewSecret(secretToken)},
			{ID: "b", URL: "https://b.example", InsecureSkipTLSVerify: true},
			{URL: "https://c.example"},
		},
	}

	i.DebugAnsibleConfig(cfg)

	if len(printer.debugLines) != 3 {
		t.Fatalf("expected 3 debug lines, got %d: %v", len(printer.debugLines), printer.debugLines)
	}
	for _, line := range printer.debugLines {
		if strings.Contains(line, secretToken) {
			t.Fatalf("debug line leaked the token: %q", line)
		}
	}

	assertContainsAll(t, printer.debugLines[0], `"a"`, "url=https://a.example", "token=true", "insecure_skip_tls_verify=false")
	assertContainsAll(t, printer.debugLines[1], `"b"`, "url=https://b.example", "token=false", "insecure_skip_tls_verify=true")
	assertContainsAll(t, printer.debugLines[2], `""`, "url=https://c.example", "token=false", "insecure_skip_tls_verify=false")
}

// TestDebugAnsibleConfigNilSafe checks the existing nil-guard contract
// (nil Infra, nil Output, nil cfg) still holds now that the method does
// more than the ansible.cfg-sourced branch.
func TestDebugAnsibleConfigNilSafe(t *testing.T) {
	t.Parallel()
	var nilInfra *Infra
	nilInfra.DebugAnsibleConfig(&config.Config{})

	i := &Infra{}
	i.DebugAnsibleConfig(&config.Config{})
	i.DebugAnsibleConfig(nil)
}
