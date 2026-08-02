package collections

import (
	"errors"
	"sort"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestBuildInstallLevels(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{
		"A": {"B", "C"},
		"B": {"C"},
		"C": nil,
	}
	levels, err := buildInstallLevels(graph)
	if err != nil {
		t.Fatalf("buildInstallLevels error: %v", err)
	}
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
	assertLevel(t, levels[0], []string{"C"})
	assertLevel(t, levels[1], []string{"B"})
	assertLevel(t, levels[2], []string{"A"})
}

func TestBuildInstallLevelsCycle(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{
		"A": {"B"},
		"B": {"A"},
	}
	_, err := buildInstallLevels(graph)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrDependencyGraphHasACycle) {
		t.Fatalf("expected ErrDependencyGraphHasACycle, got %v", err)
	}
}

// TestParseDependenciesMalformedKey covers the shapes a Galaxy server can put
// in a dependency map that this program must not carry any further. The
// forged-line row is the reason the check is on the alphabet and not just on
// the split: that key has exactly one dot and two non-empty halves, so it used
// to pass, and the solver then printed it - on an ordinary run, with no flags,
// through "probing highest version of %s" - which is a line of the attacker's
// choosing on the operator's stderr.
func TestParseDependenciesMalformedKey(t *testing.T) {
	t.Parallel()
	for _, tc := range malformedDependencyKeyCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseDependencies(map[string]string{tc.key: ">=1.0.0"})
			if !errors.Is(err, helpers.ErrInvalidDependencyKey) {
				t.Fatalf("parseDependencies(%q) error = %v, want errors.Is helpers.ErrInvalidDependencyKey", tc.key, err)
			}
		})
	}
}

// malformedDependencyKeyCase is one row of TestParseDependenciesMalformedKey.
type malformedDependencyKeyCase struct {
	name string
	key  string
}

// malformedDependencyKeyCases covers a key that fails the split and three that
// pass it and fail the alphabet.
func malformedDependencyKeyCases() []malformedDependencyKeyCase {
	return []malformedDependencyKeyCase{
		{name: "no dot", key: "notanfqdn"},
		{name: "forged line", key: "evil.pkg\n[CRITICAL] FORGED DEP LINE"},
		{name: "uppercase half", key: "Evil.pkg"},
		{name: "path separator", key: "../../etc.passwd"},
	}
}

func TestParseDependenciesWellFormed(t *testing.T) {
	t.Parallel()
	got, err := parseDependencies(map[string]string{"ns.name": "  >=1.0.0  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"ns.name": ">=1.0.0"}
	if len(got) != len(want) || got["ns.name"] != want["ns.name"] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func assertLevel(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)
	for i := range gotCopy {
		if gotCopy[i] != wantCopy[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

// TestRequirementsSignatureModePartition proves the requirements signature
// partitions --no-deps snapshots from deps-following ones, and stays
// order-independent within a single mode. This is what makes a --no-deps
// resolve unable to satisfy (or be satisfied by) a deps-following resolve's
// RequirementsHash check.
func TestRequirementsSignatureModePartition(t *testing.T) {
	t.Parallel()
	rootsForward := []collection{
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0"},
		{Namespace: "acme", Name: "lib", Constraint: ">=2.0.0"},
	}
	rootsReversed := []collection{
		{Namespace: "acme", Name: "lib", Constraint: ">=2.0.0"},
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0"},
	}

	specForward := buildRequirementsSpec(rootsForward)
	specReversed := buildRequirementsSpec(rootsReversed)

	depsSigForward := requirementsSignatureFromSpec(specForward, false, "")
	depsSigReversed := requirementsSignatureFromSpec(specReversed, false, "")
	if depsSigForward != depsSigReversed {
		t.Fatalf("deps-mode signature is order-dependent: %q != %q", depsSigForward, depsSigReversed)
	}
	if depsSigForward != requirementsSignatureFromSpec(specForward, false, "") {
		t.Fatalf("deps-mode signature is not deterministic across repeated calls")
	}

	noDepsSigForward := requirementsSignatureFromSpec(specForward, true, "")
	noDepsSigReversed := requirementsSignatureFromSpec(specReversed, true, "")
	if noDepsSigForward != noDepsSigReversed {
		t.Fatalf("no-deps-mode signature is order-dependent: %q != %q", noDepsSigForward, noDepsSigReversed)
	}
	if noDepsSigForward != requirementsSignatureFromSpec(specForward, true, "") {
		t.Fatalf("no-deps-mode signature is not deterministic across repeated calls")
	}

	if depsSigForward == noDepsSigForward {
		t.Fatalf("expected --no-deps and deps-following signatures to differ, both got %q", depsSigForward)
	}
}

// TestRefreshBypassesSnapshot pins refreshBypassesSnapshot's own accept/veto
// table, including the nil-cfg case no e2e fixture ever reaches (every e2e
// test builds a real *config.Config), and the precedence its own doc comment
// states: --offline outranks --refresh, matching
// cache.PolicyForConstraint's identical IsOffline()-before-IsRefresh() order.
func TestRefreshBypassesSnapshot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cfg  *config.Config
		name string
		want bool
	}{
		{name: "nil cfg never vetoes", cfg: nil, want: false},
		{name: "refresh off", cfg: &config.Config{Refresh: false, Offline: false}, want: false},
		{name: "refresh on, online", cfg: &config.Config{Refresh: true, Offline: false}, want: true},
		{name: "refresh on, offline: offline outranks refresh", cfg: &config.Config{Refresh: true, Offline: true}, want: false},
		{name: "refresh off, offline: still no veto", cfg: &config.Config{Refresh: false, Offline: true}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := refreshBypassesSnapshot(tc.cfg); got != tc.want {
				t.Errorf("refreshBypassesSnapshot(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}
