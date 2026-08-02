// Package ciaudit gates the cross-references inside this repository's GitHub
// Actions workflows.
//
// It holds no production code and nothing imports it: its entire content is
// one test, for the same reason internal/proseaudit is one. `go test ./...`
// already runs it, so the gate needs no Justfile target, no CI step, no new
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
// The gate is deliberately not a workflow schema validator. It resolves those
// two reference kinds and reports a file that does not parse as YAML at all;
// everything else about a workflow - action versions, expression syntax,
// runner labels, permissions - is out of scope and belongs to a real linter.
package ciaudit
