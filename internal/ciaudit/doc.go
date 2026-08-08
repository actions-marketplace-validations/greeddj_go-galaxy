// Package ciaudit gates the cross-references inside this repository's GitHub
// Actions workflows, and one value a workflow shares with the Justfile.
//
// It holds no production code and nothing imports it: its entire content is
// tests, for the same reason internal/proseaudit's is. `go test ./...` already
// runs them, so the gates need no Justfile target, no CI step, no new
// dependency, and no depguard allow-list entry.
//
// Two references in a workflow file are resolved by GitHub and by nothing
// else, which is what makes them worth a gate here. A job's `needs` names a
// job id defined in the SAME file - naming a job that lives in another
// workflow does not reach across, it invalidates the whole file - and a job's
// `uses: ./.github/workflows/<file>` names a workflow in this repository that
// must itself declare the `workflow_call` trigger. Both are checked only when
// a run is dispatched, and a workflow rejected at dispatch produces no failing
// job to notice: a release workflow that fires on a tag simply publishes
// nothing, silently, until somebody goes looking for the release.
//
// A third value is gated for a different reason: it is spelled twice. The
// golangci-lint release this repository is linted with appears in ci.yml, as
// the golangci-lint-action step's `version` input, and in the Justfile, as the
// only release `just lint` will run against. Nothing resolves those two
// against each other, so a bump that edits one and forgets the other is
// silent: CI and a developer's machine then lint the same tree with different
// releases, and under `default: all` a release difference is a findings
// difference. Each spelling must also name an exact release rather than a
// moving target, since `latest` cannot disagree with anything and still
// changes what CI enforces from one day to the next.
//
// The gates are deliberately not a workflow schema validator. They resolve
// those two reference kinds, compare that one duplicated input, and report a
// file that does not parse as YAML at all; everything else about a workflow -
// the version tag an action is itself used at, expression syntax, runner
// labels, permissions - is out of scope and belongs to a real linter. That
// exclusion is what the version gate turns on rather than an exception to it:
// it reads a step's `version` input, which a second file duplicates, never the
// `@v9` the action itself is pinned at, which nothing duplicates.
package ciaudit
