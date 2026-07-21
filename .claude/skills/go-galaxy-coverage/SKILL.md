---
name: go-galaxy-coverage
description: Measure and analyze test coverage for the go-galaxy Go repo. Produces a coverage profile, prints per-function and per-package coverage, and surfaces uncovered branches so gaps can be justified or filled. Use when auditing how well a change is tested (the go-tester agent's main tool). Do NOT use for plain test runs (see go-galaxy-test) or static analysis (see go-galaxy-check).
---

# go-galaxy - Coverage Analysis

## When to use

- "какое покрытие" / "coverage" / "сколько покрыто тестами"
- Auditing a change before sign-off: which branches/error paths are untested
- Deciding whether a gap needs a new test or a justified "not tested" note

## Commands

| Intent | Command |
|---|---|
| Profile one package | `go test ./internal/galaxy/<pkg>/... -race -coverprofile=/tmp/cover.out` |
| Profile whole repo | `go test ./... -race -coverprofile=/tmp/cover.out -covermode=atomic` |
| Per-function breakdown | `go tool cover -func=/tmp/cover.out` |
| Total only | `go tool cover -func=/tmp/cover.out \| tail -1` |
| Visual HTML report | `go tool cover -html=/tmp/cover.out -o /tmp/cover.html` |
| Cross-package coverage | add `-coverpkg=./internal/galaxy/...` to attribute coverage from integration tests |

## Workflow

1. Profile the focused package first (`-coverprofile`), with `-race`.
2. Read the per-function output; zero-coverage funcs and `0.0%` lines in
   error/edge branches are the candidates.
3. For each gap decide: **must cover** -> name the concrete test case
   (package, input, assertion) for the developer; or **not worth testing**
   (external IO only, trivial wiring, platform-guarded) -> propose an explicit
   note in `CLAUDE.md` with the reason.
4. Re-profile after tests land to confirm the gap closed.

## Repo-specific notes

- Use `-covermode=atomic` whenever `-race` is on (count mode is not race-safe).
- The concurrency-heavy packages (`internal/galaxy/collections`,
  `internal/galaxy/store`, `internal/cache/s3`) are where uncovered error paths
  matter most - prioritize their branches.
- Integration coverage from driving `./dist/go-galaxy` against `testing/`
  fixtures will not show up in unit profiles; use `-coverpkg` and a built binary
  with `-cover` only if you specifically need it, otherwise judge those paths by
  inspection.
- Coverage is a signal, not a target. A missing test on a reachable error path
  is a finding; an uncovered `panic("unreachable")` is not.
