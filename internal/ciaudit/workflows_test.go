package ciaudit

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// workflowsDir is where GitHub reads this repository's workflows from, as a
// slash path relative to the module root. It doubles as the key prefix every
// parsed workflow is held under, so a `uses: ./<path>` reference is matched
// against the same spelling GitHub resolves it with.
const workflowsDir = ".github/workflows"

// workflowFile is one workflow, reduced to what this gate resolves. `on` is
// kept as a raw node because GitHub accepts three shapes for it - a single
// trigger name, a list of them, or a mapping of trigger to configuration -
// and callable() has to answer the same question for all three.
type workflowFile struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
	On   yaml.Node              `yaml:"on"`
}

// callable reports whether this workflow may be invoked by another one.
func (w workflowFile) callable() bool {
	return nodeNames(&w.On, "workflow_call")
}

// workflowJob is one job, reduced to its two outgoing references.
type workflowJob struct {
	Uses  string     `yaml:"uses"`
	Needs stringList `yaml:"needs"`
}

// stringList is a field GitHub accepts either as one scalar or as a sequence
// of them, which `needs` is.
type stringList []string

// UnmarshalYAML accepts both shapes.
func (l *stringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*l = stringList{node.Value}
		return nil
	}
	var items []string
	if err := node.Decode(&items); err != nil {
		return err
	}
	*l = items
	return nil
}

// TestWorkflowReferencesResolve is the gate. Every `needs` and every local
// `uses` in every workflow of this repository must name something that exists.
//
// Run against the defect it was written for: with .github/workflows/release.yml
// carrying `needs: [tests_and_checks]` and no such job defined in that file -
// the job id lived in ci.yml, which `needs` cannot reach - this test failed
// with
//
//	unresolvable workflow references:
//	    .github/workflows/release.yml: job "release" needs "tests_and_checks", which no job in this file defines
//
// and passed again once release.yml called ci.yml as a job of its own. GitHub
// answers that same file with "Invalid workflow file", which is why the tag
// that was supposed to publish a release published nothing at all.
func TestWorkflowReferencesResolve(t *testing.T) {
	t.Parallel()

	problems := auditWorkflows(readWorkflows(t, moduleRoot(t)))
	if len(problems) > 0 {
		t.Fatalf("unresolvable workflow references:\n\t%s", strings.Join(problems, "\n\t"))
	}
}

// TestAuditResolvesJobNeeds pins the `needs` half against a fixture that is
// shown capable of resolving: the same caller is audited twice, differing only
// in the job id it depends on.
func TestAuditResolvesJobNeeds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		need        string
		wantProblem string
	}{
		{name: "defined", need: "gate", wantProblem: ""},
		{name: "undefined", need: "tests_and_checks", wantProblem: `needs "tests_and_checks", which no job in this file defines`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := map[string][]byte{
				workflowsDir + "/release.yml": []byte(callerSource(tc.need)),
				workflowsDir + "/ci.yml":      []byte(calleeSource(true)),
			}
			checkOneProblem(t, auditWorkflows(files), tc.wantProblem)
		})
	}
}

// TestAuditResolvesLocalUses pins the `uses` half the same way, over the three
// states a local reusable-workflow reference can be in. The first row is the
// positive control: without it, the two refusals below would be
// indistinguishable from a gate that never reached the check.
func TestAuditResolvesLocalUses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		callee      string
		wantProblem string
	}{
		{name: "callable", callee: calleeSource(true), wantProblem: ""},
		{name: "not_callable", callee: calleeSource(false), wantProblem: "does not declare the workflow_call trigger"},
		{name: "absent", callee: "", wantProblem: "which is not a workflow file in this repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := map[string][]byte{workflowsDir + "/release.yml": []byte(callerSource("gate"))}
			if tc.callee != "" {
				files[workflowsDir+"/ci.yml"] = []byte(tc.callee)
			}
			checkOneProblem(t, auditWorkflows(files), tc.wantProblem)
		})
	}
}

// TestAuditReportsAnUnparseableWorkflow pins the one failure that is not a
// reference at all. A file this gate cannot read is reported rather than
// skipped, because skipping it would turn a broken workflow into a silently
// unaudited one - the exact outcome the gate exists to prevent. The control is
// the same fixture with its indentation repaired.
func TestAuditReportsAnUnparseableWorkflow(t *testing.T) {
	t.Parallel()

	broken := "jobs:\n  gate:\n   runs-on: ubuntu-latest\n  \tsteps: []\n"
	checkOneProblem(t, auditWorkflows(map[string][]byte{workflowsDir + "/ci.yml": []byte(broken)}), "does not parse as YAML")

	repaired := "jobs:\n  gate:\n    runs-on: ubuntu-latest\n    steps: []\n"
	checkOneProblem(t, auditWorkflows(map[string][]byte{workflowsDir + "/ci.yml": []byte(repaired)}), "")
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

// callerSource is a release-shaped workflow whose `release` job depends on
// need and whose `gate` job calls the other fixture workflow.
func callerSource(need string) string {
	return fmt.Sprintf(`name: Release
on:
  push:
    tags: ['v*']
jobs:
  gate:
    uses: ./%s/ci.yml
  release:
    runs-on: ubuntu-latest
    needs: [%s]
    steps:
      - run: echo release
`, workflowsDir, need)
}

// calleeSource is a CI-shaped workflow, callable by another workflow only when
// callable is set.
func calleeSource(callable bool) string {
	trigger := ""
	if callable {
		trigger = "  workflow_call:\n"
	}
	return fmt.Sprintf(`name: CI
on:
  push:
    branches: [main]
%sjobs:
  tests:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
`, trigger)
}

// auditWorkflows reports every unresolvable reference across a set of workflow
// files, keyed by their module-relative slash path. Files are audited in path
// order and jobs in id order, so the report is stable.
func auditWorkflows(files map[string][]byte) []string {
	parsed := make(map[string]workflowFile, len(files))
	names := slices.Sorted(maps.Keys(files))

	var problems []string
	for _, name := range names {
		var doc workflowFile
		if err := yaml.Unmarshal(files[name], &doc); err != nil {
			problems = append(problems, fmt.Sprintf("%s: does not parse as YAML: %v", name, err))
			continue
		}
		parsed[name] = doc
	}

	for _, name := range names {
		doc, ok := parsed[name]
		if !ok {
			continue
		}
		problems = append(problems, auditWorkflow(name, doc, parsed)...)
	}
	return problems
}

// auditWorkflow reports the unresolvable references of one workflow. all
// carries every workflow of the repository, since a local `uses` resolves
// across files while `needs` deliberately does not.
func auditWorkflow(name string, doc workflowFile, all map[string]workflowFile) []string {
	var problems []string
	for _, id := range slices.Sorted(maps.Keys(doc.Jobs)) {
		job := doc.Jobs[id]
		for _, need := range job.Needs {
			if _, ok := doc.Jobs[need]; !ok {
				problems = append(problems,
					fmt.Sprintf("%s: job %q needs %q, which no job in this file defines", name, id, need))
			}
		}
		if problem := checkLocalUses(name, id, job.Uses, all); problem != "" {
			problems = append(problems, problem)
		}
	}
	return problems
}

// checkLocalUses returns the problem with one job's reusable-workflow
// reference, or "" when there is none. A reference that does not start with
// "./" names another repository's workflow and is out of scope: resolving it
// would mean reaching the network.
func checkLocalUses(name, id, uses string, all map[string]workflowFile) string {
	target, ok := strings.CutPrefix(uses, "./")
	if !ok {
		return ""
	}
	doc, ok := all[path.Clean(target)]
	if !ok {
		return fmt.Sprintf("%s: job %q uses %q, which is not a workflow file in this repository", name, id, uses)
	}
	if !doc.callable() {
		return fmt.Sprintf("%s: job %q uses %q, which does not declare the workflow_call trigger", name, id, uses)
	}
	return ""
}

// nodeNames reports whether a workflow's `on` node names the given trigger, in
// any of the three shapes GitHub accepts for it. The document and alias arms
// are pass-throughs rather than answers of their own: neither shape reaches
// this function from a decoded struct field, but answering them by recursing
// is both shorter and more honest than answering them with false.
func nodeNames(node *yaml.Node, trigger string) bool {
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Value == trigger
	case yaml.SequenceNode:
		return slices.ContainsFunc(node.Content, func(item *yaml.Node) bool { return item.Value == trigger })
	case yaml.MappingNode:
		return mappingHasKey(node, trigger)
	case yaml.DocumentNode:
		return len(node.Content) == 1 && nodeNames(node.Content[0], trigger)
	case yaml.AliasNode:
		return node.Alias != nil && nodeNames(node.Alias, trigger)
	default:
		// The zero Kind, which is what an absent `on:` decodes to.
		return false
	}
}

// mappingHasKey reports whether a mapping node carries the given key. A
// mapping's Content alternates key, value, so only every other entry is one.
func mappingHasKey(node *yaml.Node, key string) bool {
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

// readWorkflows returns every workflow file under root, keyed by its
// module-relative slash path.
func readWorkflows(t *testing.T, root string) map[string][]byte {
	t.Helper()

	dir := filepath.Join(root, filepath.FromSlash(workflowsDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		// #nosec G304 -- name comes from this repository's own workflows directory
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Join(dir, name), err)
		}
		files[workflowsDir+"/"+name] = data
	}
	if len(files) == 0 {
		t.Fatalf("no workflow files under %s", dir)
	}
	return files
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
