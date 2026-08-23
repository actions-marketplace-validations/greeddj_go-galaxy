// Package ciaudit gates one value this repository spells twice: the
// golangci-lint release it is linted with.
//
// It holds no production code and nothing imports it: its entire content is
// tests, for the same reason internal/proseaudit's is. `go test ./...` already
// runs them, so the gate needs no Justfile target, no CI step, no new
// dependency, and no depguard allow-list entry.
//
// The release appears in .github/workflows/ci.yml, as the golangci-lint-action
// step's `version` input, and in the Justfile, as the only release `just lint`
// will run against. Nothing resolves those two against each other, so a bump
// that edits one and forgets the other is silent: CI and a developer's machine
// then lint the same tree with different releases, and under `default: all` a
// release difference is a findings difference. Each spelling must also name an
// exact release rather than a moving target, since `latest` cannot disagree
// with anything and still changes what CI enforces from one day to the next.
//
// What this package deliberately does NOT do is check the workflows
// themselves. It used to resolve two references GitHub alone resolves - a
// job's `needs`, and a local `uses:` naming a workflow that must declare
// `workflow_call` - written after a release workflow was rejected at dispatch
// and published nothing, silently. `go tool actionlint` now does that, and
// more: it resolves the same two, plus the inputs and secrets a reusable
// workflow call passes, plus expression syntax, runner labels and action
// inputs, which the hand-written version had ruled out of scope as belonging
// to a real linter. Two checks for one defect is one place too many to keep
// true, so the hand-written half is gone and `just check` runs actionlint.
//
// The version pin stays here because it is the one thing actionlint cannot
// know: it reads a step's `version` input, which a second file duplicates,
// never the `@v9` the action itself is pinned at, which nothing duplicates.
package ciaudit
