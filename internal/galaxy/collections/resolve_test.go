package collections

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const ver1260 = "12.6.0"

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

func TestSelectVersionConflictError(t *testing.T) {
	t.Parallel()
	versions := []string{ver1260, "11.5.0", "10.0.0"}
	sources := []constraintSource{
		{Constraint: ">=12.0.0,<13.0.0", Source: "amazon.aws"},
		{Constraint: ">=6.6.0,<12.0.0", Source: "community.aws"},
	}
	_, err := selectVersion(nil, "community.general", versions, sources, selectionMode{})
	if err == nil {
		t.Fatalf("expected conflict error")
	}
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("expected ErrNoVersionSatisfiesConstraints, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"community.general",
		">=12.0.0,<13.0.0",
		"required by amazon.aws",
		">=6.6.0,<12.0.0",
		"required by community.aws",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected error to contain %q, got:\n%s", want, msg)
		}
	}
}

func TestSelectVersionRootSourceLabel(t *testing.T) {
	t.Parallel()
	versions := []string{"1.0.0"}
	sources := []constraintSource{
		{Constraint: ">=2.0.0", Source: "root"},
	}
	_, err := selectVersion(nil, "ns.col", versions, sources, selectionMode{})
	if err == nil {
		t.Fatalf("expected conflict error")
	}
	if !strings.Contains(err.Error(), "required by requirements.yml") {
		t.Fatalf("expected root to render as 'requirements.yml', got: %s", err.Error())
	}
}

func TestSelectVersionSucceeds(t *testing.T) {
	t.Parallel()
	versions := []string{ver1260, "11.5.0", "10.0.0"}
	sources := []constraintSource{
		{Constraint: ">=11.0.0", Source: "amazon.aws"},
		{Constraint: "<12.0.0", Source: "community.aws"},
	}
	got, err := selectVersion(nil, "community.general", versions, sources, selectionMode{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "11.5.0" {
		t.Fatalf("expected 11.5.0, got %q", got)
	}
}

func TestSelectVersionLenientFallback(t *testing.T) {
	t.Parallel()
	versions := []string{ver1260, "11.5.0", "10.0.0"}
	sources := []constraintSource{
		{Constraint: ">=12.0.0,<13.0.0", Source: "amazon.aws"},
		{Constraint: ">=6.6.0,<12.0.0", Source: "community.aws"},
		{Constraint: ">=10.0.0", Source: "ansible.posix"},
	}
	got, err := selectVersion(nil, "community.general", versions, sources, selectionMode{Lenient: true})
	if err != nil {
		t.Fatalf("expected lenient fallback to succeed, got: %v", err)
	}
	// Two of three constraints satisfied by both 12.6.0 and 11.5.0;
	// tie-break: highest version → 12.6.0.
	if got != ver1260 {
		t.Fatalf("expected 12.6.0 (highest with max satisfaction), got %q", got)
	}
}

func TestSelectVersionBacktrackBlamesOne(t *testing.T) {
	t.Parallel()
	versions := []string{ver1260, "11.5.0", "10.0.0"}
	// Three constraints. Two of them ("<11.0.0" and ">=12.0.0") can't co-exist
	// even after dropping one — so dropping ">=10.5.0" alone doesn't help.
	// Dropping "<11.0.0" leaves {">=12.0.0", ">=10.5.0"} → 12.6.0 satisfies.
	// Dropping ">=12.0.0" leaves {"<11.0.0", ">=10.5.0"} → 10.0.0? but "<11.0.0" + ">=10.5.0" → 10.0.0 is too low.
	// Actually with versions {12.6, 11.5, 10.0}: <11.0.0 + >=10.5.0 → no version (10.0 fails >=10.5).
	// Only one valid drop = "<11.0.0", giving 12.6.0.
	sources := []constraintSource{
		{Constraint: "<11.0.0", Source: "a"},
		{Constraint: ">=12.0.0", Source: "b"},
		{Constraint: ">=10.5.0", Source: "c"},
	}
	mode := selectionMode{Lenient: true, Backtrack: true}
	got, err := selectVersion(nil, "ns.col", versions, sources, mode)
	if err != nil {
		t.Fatalf("expected backtrack to succeed, got: %v", err)
	}
	if got != ver1260 {
		t.Fatalf("expected 12.6.0 (drops 'a' constraint), got %q", got)
	}
}

func TestSelectVersionBacktrackPrefersHigher(t *testing.T) {
	t.Parallel()
	versions := []string{ver1260, "11.5.0", "10.0.0"}
	sources := []constraintSource{
		{Constraint: "<12.0.0", Source: "a"},
		{Constraint: ">=12.0.0", Source: "b"},
	}
	// Dropping "a" → highest satisfying ">=12.0.0" is 12.6.0.
	// Dropping "b" → highest satisfying "<12.0.0" is 11.5.0.
	// Backtrack picks the higher → 12.6.0.
	mode := selectionMode{Lenient: true, Backtrack: true}
	got, err := selectVersion(nil, "x.y", versions, sources, mode)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != ver1260 {
		t.Fatalf("expected 12.6.0, got %q", got)
	}
}

func TestSelectVersionLenientFullSatisfactionWins(t *testing.T) {
	t.Parallel()
	versions := []string{ver1260, "11.5.0"}
	sources := []constraintSource{
		{Constraint: "<12.0.0", Source: "a"},
	}
	// Strict already succeeds — lenient must pick the strict winner.
	got, err := selectVersion(nil, "x.y", versions, sources, selectionMode{Lenient: true})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != "11.5.0" {
		t.Fatalf("expected 11.5.0 (strict winner), got %q", got)
	}
}

func TestParseDependenciesMalformedKey(t *testing.T) {
	t.Parallel()
	_, err := parseDependencies(map[string]string{"notanfqdn": ">=1.0.0"})
	if err == nil {
		t.Fatalf("expected error for malformed dependency key")
	}
	if !errors.Is(err, helpers.ErrInvalidDependencyKey) {
		t.Fatalf("expected ErrInvalidDependencyKey, got %v", err)
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
	cfg := &config.Config{Server: "https://galaxy.example.com"}
	rootsForward := []collection{
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0"},
		{Namespace: "acme", Name: "lib", Constraint: ">=2.0.0"},
	}
	rootsReversed := []collection{
		{Namespace: "acme", Name: "lib", Constraint: ">=2.0.0"},
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0"},
	}

	specForward := buildRequirementsSpec(cfg, rootsForward)
	specReversed := buildRequirementsSpec(cfg, rootsReversed)

	depsSigForward := requirementsSignatureFromSpec(specForward, false)
	depsSigReversed := requirementsSignatureFromSpec(specReversed, false)
	if depsSigForward != depsSigReversed {
		t.Fatalf("deps-mode signature is order-dependent: %q != %q", depsSigForward, depsSigReversed)
	}
	if depsSigForward != requirementsSignatureFromSpec(specForward, false) {
		t.Fatalf("deps-mode signature is not deterministic across repeated calls")
	}

	noDepsSigForward := requirementsSignatureFromSpec(specForward, true)
	noDepsSigReversed := requirementsSignatureFromSpec(specReversed, true)
	if noDepsSigForward != noDepsSigReversed {
		t.Fatalf("no-deps-mode signature is order-dependent: %q != %q", noDepsSigForward, noDepsSigReversed)
	}
	if noDepsSigForward != requirementsSignatureFromSpec(specForward, true) {
		t.Fatalf("no-deps-mode signature is not deterministic across repeated calls")
	}

	if depsSigForward == noDepsSigForward {
		t.Fatalf("expected --no-deps and deps-following signatures to differ, both got %q", depsSigForward)
	}
}
