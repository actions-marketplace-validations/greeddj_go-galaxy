---
name: go-galaxy-test
description: Run tests for the go-galaxy Go repo via Justfile or direct go test. Use when the user asks to run tests, verify a fix, or iterate on a single package. Covers full-repo runs and tight package-level loops with -race. Do NOT use for lint/static analysis (see go-galaxy-check) or builds (see go-galaxy-build).
---

# go-galaxy - Tests

## When to use

- "прогони тесты" / "run tests" / "verify fix"
- Tight package-level loops on a failing test
- Reproducing CI failures locally

## Commands

| Intent | Command |
|---|---|
| Full test run | `just test` |
| Same as CI (race + coverprofile) | `go test -race -coverprofile=coverage.out ./...` |
| Tight loop on one package | `go test ./path/to/pkg -run <regex> -race -v` |
| Single test | `go test ./internal/galaxy/store -run TestSnapshot -race -v` |

`just test` is the simplest form (`go test ./...`); it does **not** chain `just check` or `just deps`. CI (`.github/workflows/ci.yml`) runs `go vet`, `staticcheck`, `govulncheck`, `golangci-lint`, **then** `go test -race -coverprofile=...` - match that locally before handing off.

## Where tests live

Tests are colocated with source. Notable suites:

- [internal/galaxy/archive/archive_test.go](internal/galaxy/archive/archive_test.go) - tarball extract.
- [internal/galaxy/store/](internal/galaxy/store/) - snapshot serialization + Bolt bucket round-trips.
- [internal/cache/local/](internal/cache/local/) - filesystem backend incl. local OCI artifacts.
- [internal/cache/s3/](internal/cache/s3/) - minimal S3 client + lock TTL semantics.
- [internal/galaxy/collections/](internal/galaxy/collections/) - install pipeline.

## Repo-specific gotchas

- **`-race` is non-negotiable** for concurrency-touching code. The install pipeline uses worker pools (`sem := make(chan struct{}, cfg.Workers)` + `wg.Go`), and `Store` is RWMutex-protected. CI runs with `-race`; reproduce locally with the same flag.
- **`Store` mutex** is internal - never access fields directly across goroutines. Tests that exercise concurrent install levels must go through the public methods.
- **Snapshot schema** is gated by `helpers.StoreSnapshotSchemaVersion`. Tests that load fixtures with a newer schema must construct fixtures matching the current version, or expect rejection from `store.ValidateSchema`.
- **S3 backend tests** rely on the in-process minimal client. Don't introduce real network I/O - keep them hermetic.
- **`Printer`/`Output`** captures `log` output. Tests asserting on log lines should drive output through the same `progress.New(verbose, quiet)` flow rather than reading `os.Stdout` directly.

## Workflow

1. Reproduce failure with `go test ./<failing-pkg> -race -run <Name> -v` (fast).
2. Fix → re-run package test.
3. Before handoff: `just test` (full repo). For CI parity, run with `-race -coverprofile=coverage.out`.
