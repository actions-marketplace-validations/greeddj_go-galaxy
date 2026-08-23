package ciaudit

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// lintVersionJustfile is the second place this repository spells the
// golangci-lint release it is linted with, as a path relative to the module
// root.
const lintVersionJustfile = "Justfile"

// lintActionPrefix is the action whose `version` input is CI's spelling of
// that same release. The tag the action is itself used at (`@v9`) is
// deliberately outside this prefix: what this gate reads is the input, never
// the action's own pin.
const lintActionPrefix = "golangci/golangci-lint-action"

// lintWorkflowFile is the workflow carrying that step, as a slash path
// relative to the module root.
const lintWorkflowFile = ".github/workflows/ci.yml"

// justfileLintVersionPattern matches the Justfile's own spelling of the
// release, `GOLANGCI_LINT_VERSION := "vX.Y.Z"`, and captures the value. It is
// anchored per line rather than searched for anywhere, so the prose of a
// comment mentioning the variable cannot stand in for the assignment.
var justfileLintVersionPattern = regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION\s*:=\s*"([^"]+)"\s*$`)

// pinnedLintVersionPattern is the shape both spellings must have: one exact
// release. `latest` and a truncated `vX.Y` are refused because neither names a
// single binary - the first changes what CI enforces from one day to the next,
// and the second lets the two spellings agree while resolving differently.
var pinnedLintVersionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// lintFixtureVersion is the release the in-memory fixtures pin, and
// lintFixtureOther the one a drifted spelling names instead. Neither has to be
// the release this repository is actually on: the fixtures exist to be
// disagreed with.
const (
	lintFixtureVersion = "v2.11.4"
	lintFixtureOther   = "v2.12.2"
)

// lintWorkflow is one workflow, reduced to the steps this gate reads.
type lintWorkflow struct {
	Jobs map[string]lintJob `yaml:"jobs"`
}

// lintJob is one job, reduced to its steps.
type lintJob struct {
	Steps []lintStep `yaml:"steps"`
}

// lintStep is one step. With is a map of any rather than of string because a
// workflow's inputs are not all strings - `fetch-depth: 0` and
// `fail_ci_if_error: false` both live in this repository's own ci.yml, and a
// string-typed map would fail to decode the whole file over an input this gate
// does not even look at.
type lintStep struct {
	With map[string]any `yaml:"with"`
	Uses string         `yaml:"uses"`
}

// TestLintVersionIsPinnedAndAgrees is the gate. The golangci-lint release this
// repository is linted with is spelled in two files that nothing else compares,
// and both spellings must name one exact release.
func TestLintVersionIsPinnedAndAgrees(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	problems := auditLintVersion(readRepoFile(t, root, lintWorkflowFile), readRepoFile(t, root, lintVersionJustfile))
	if len(problems) > 0 {
		t.Fatalf("golangci-lint version pin:\n\t%s", strings.Join(problems, "\n\t"))
	}
}

// readRepoFile returns the contents of one of this repository's own files,
// named as a slash path relative to root. A file it cannot read is a failure
// rather than a skip: both of these are files the gate exists to compare, so a
// rename that hides one must not quietly pass.
func readRepoFile(t *testing.T, root, name string) []byte {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(name))
	// #nosec G304 -- the path is this module's own root plus a constant name
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return data
}

// moduleRoot walks up from this package's directory to the one holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// checkOneProblem asserts that the audit reported exactly the one problem
// containing want, or nothing at all when want is empty.
func checkOneProblem(t *testing.T, problems []string, want string) {
	t.Helper()

	if want == "" {
		if len(problems) != 0 {
			t.Fatalf("audit reported %d problems, want none: %v", len(problems), problems)
		}
		return
	}
	if len(problems) != 1 {
		t.Fatalf("audit reported %d problems, want 1 containing %q: %v", len(problems), want, problems)
	}
	if !strings.Contains(problems[0], want) {
		t.Fatalf("problem %q does not contain %q", problems[0], want)
	}
}

// TestAuditLintVersionDetectsDrift pins the audit over in-memory fixtures. The
// first row is the positive control: every refusal below is built from the same
// two helpers and differs from it in one value, so without it a refusal would
// be indistinguishable from a gate that never reached its check.
func TestAuditLintVersionDetectsDrift(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		workflow    string
		justfile    string
		wantProblem string
	}{
		{
			name:        "agreeing",
			workflow:    lintWorkflowSource(lintFixtureVersion),
			justfile:    lintJustfileSource(lintFixtureVersion),
			wantProblem: "",
		},
		{
			name:        "floating_workflow_version",
			workflow:    lintWorkflowSource("latest"),
			justfile:    lintJustfileSource(lintFixtureVersion),
			wantProblem: `pins version "latest", which is not an exact`,
		},
		{
			name:        "truncated_workflow_version",
			workflow:    lintWorkflowSource("v2.12"),
			justfile:    lintJustfileSource(lintFixtureVersion),
			wantProblem: `pins version "v2.12", which is not an exact`,
		},
		// Run against the mutation this row exists to catch: with the two
		// captured versions compared nowhere in auditLintVersion and only the
		// pin-shape check left, it failed with
		//
		//	audit reported 0 problems, want 1 containing "pins golangci-lint v2.11.4 while Justfile pins v2.12.2": []
		//
		// because a workflow and a Justfile that each name a well-formed
		// release are, one at a time, beyond reproach.
		{
			name:        "disagreeing",
			workflow:    lintWorkflowSource(lintFixtureVersion),
			justfile:    lintJustfileSource(lintFixtureOther),
			wantProblem: "pins golangci-lint v2.11.4 while Justfile pins v2.12.2",
		},
		{
			name:        "justfile_without_the_variable",
			workflow:    lintWorkflowSource(lintFixtureVersion),
			justfile:    "PROJECT := \"go-galaxy\"\n\nlint:\n\tgolangci-lint run ./...\n",
			wantProblem: "names no golangci-lint release",
		},
		{
			name:        "workflow_without_the_action",
			workflow:    "jobs:\n  tests:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n",
			justfile:    lintJustfileSource(lintFixtureVersion),
			wantProblem: "no step uses golangci/golangci-lint-action",
		},
		{
			name:        "unparseable_workflow",
			workflow:    "jobs:\n  gate:\n   runs-on: ubuntu-latest\n  \tsteps: []\n",
			justfile:    lintJustfileSource(lintFixtureVersion),
			wantProblem: "does not parse as YAML",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			checkOneProblem(t, auditLintVersion([]byte(tc.workflow), []byte(tc.justfile)), tc.wantProblem)
		})
	}
}

// auditLintVersion reports every problem with how the two files spell the
// golangci-lint release this repository is linted with. Each half is read on
// its own so a file that names nothing usable is reported as that, rather than
// as a disagreement with the other file; the comparison then runs only over two
// values that both survived their own half.
func auditLintVersion(workflow, justfile []byte) []string {
	var problems []string

	workflowVersion, problem := workflowLintVersion(workflow)
	if problem != "" {
		problems = append(problems, problem)
	}

	justfileVersion, problem := justfileLintVersion(justfile)
	if problem != "" {
		problems = append(problems, problem)
	}

	if workflowVersion != "" && justfileVersion != "" && workflowVersion != justfileVersion {
		problems = append(problems, fmt.Sprintf(
			"%s pins golangci-lint %s while %s pins %s; the two spell one release or CI lints a tree nobody can reproduce",
			lintWorkflowFile, workflowVersion, lintVersionJustfile, justfileVersion))
	}
	return problems
}

// workflowLintVersion returns the release CI pins, or the one problem that
// stopped it from naming one.
func workflowLintVersion(workflow []byte) (string, string) {
	var doc lintWorkflow
	if err := yaml.Unmarshal(workflow, &doc); err != nil {
		return "", fmt.Sprintf("%s: does not parse as YAML: %v", lintWorkflowFile, err)
	}

	step, found := lintActionStep(doc)
	if !found {
		return "", fmt.Sprintf("%s: no step uses %s, so CI names no golangci-lint release at all",
			lintWorkflowFile, lintActionPrefix)
	}

	version, ok := step.With["version"].(string)
	if !ok {
		return "", fmt.Sprintf("%s: the %s step's with.version is %#v, not a string naming a release",
			lintWorkflowFile, lintActionPrefix, step.With["version"])
	}
	if !pinnedLintVersionPattern.MatchString(version) {
		return "", fmt.Sprintf("%s: the %s step pins version %q, which is not an exact vMAJOR.MINOR.PATCH release",
			lintWorkflowFile, lintActionPrefix, version)
	}
	return version, ""
}

// justfileLintVersion returns the release the Justfile requires, or the one
// problem that stopped it from naming one.
func justfileLintVersion(justfile []byte) (string, string) {
	match := justfileLintVersionPattern.FindSubmatch(justfile)
	if match == nil {
		return "", fmt.Sprintf("%s: no GOLANGCI_LINT_VERSION assignment, so it names no golangci-lint release "+
			"for `just lint` to require", lintVersionJustfile)
	}
	return string(match[1]), ""
}

// lintActionStep returns the first step using the linter action, over jobs in
// id order so a workflow declaring two of them is read the same way twice.
func lintActionStep(doc lintWorkflow) (lintStep, bool) {
	for _, id := range slices.Sorted(maps.Keys(doc.Jobs)) {
		for _, step := range doc.Jobs[id].Steps {
			if strings.HasPrefix(step.Uses, lintActionPrefix) {
				return step, true
			}
		}
	}
	return lintStep{}, false
}

// lintWorkflowSource is a CI-shaped workflow pinning version. It carries the
// non-string `fetch-depth` input this repository's own ci.yml has, so the
// fixture decodes only under the map of any that step's inputs need.
func lintWorkflowSource(version string) string {
	return fmt.Sprintf(`name: CI
on:
  push:
    branches: [main]
jobs:
  tests_and_checks:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout code
        uses: actions/checkout@v6
        with:
          fetch-depth: 0

      - name: Run golangci-lint
        uses: %s@v9
        with:
          version: %s
          install-mode: binary
`, lintActionPrefix, version)
}

// lintJustfileSource is a Justfile-shaped source requiring version.
func lintJustfileSource(version string) string {
	return fmt.Sprintf("PROJECT := \"go-galaxy\"\nGOLANGCI_LINT_VERSION := %q\n\nlint:\n\tgolangci-lint run ./...\n", version)
}
