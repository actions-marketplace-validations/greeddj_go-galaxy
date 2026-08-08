---
name: go-galaxy-check
description: Run static-analysis quality gates for the go-galaxy Go repo via Justfile - golangci-lint, vet, staticcheck, govulncheck, fieldalignment. Use when the user asks to lint the code, run quality gates, diagnose a failing analyzer, or verify changes before commit/PR. Do NOT use for running tests (see go-galaxy-test) or builds (see go-galaxy-build).
---

# go-galaxy - Quality Gates

## When to use

- "прогони линтер" / "lint" / "golangci"
- "проверь код" / "запусти проверки" / "quality gates"
- Diagnose a single analyzer: vet, staticcheck, govulncheck, fieldalignment
- Pre-commit / pre-PR validation of static checks (без тестов)

## Commands

| Intent | Command |
|---|---|
| Full lint pass | `just lint` |
| All gates in one go (vet + staticcheck + govulncheck + fieldalignment) | `just check` |
| Lint + check + tests + run | `just run` |

`just check` runs the four analyzers sequentially and **mutates nothing**. It does not chain `just deps`: a gate that rewrote `go.mod`, `go.sum` and `vendor/` before reading them could only agree with itself. If `vendor/` is out of sync with `go.mod`, the run fails on Go's own vendor-consistency check naming the module, and the fix is to run `just deps` yourself.

## Direct Go equivalents (tight loop)

When iterating on a fix, prefer the single failing gate over the full chain:

```sh
go vet ./...
go tool staticcheck ./...
go tool govulncheck ./...
go tool fieldalignment ./...
```

The analyzers are wired through `go tool` directives in `go.mod` - no separate install step needed.

## Repo-specific gotchas

- **`golangci-lint` v2 with `default: all`** minus a small disable list (see [.golangci.yml](.golangci.yml)). Common offenders: `lll` (140 cols), `fieldalignment` (struct field ordering), `revive`/`staticcheck` package-comment rules (already excluded for the latter).
- **The golangci-lint version is pinned, and spelled twice.** `GOLANGCI_LINT_VERSION` in the [Justfile](Justfile) and the `version:` input of the `golangci-lint-action` step in [.github/workflows/ci.yml](.github/workflows/ci.yml) must name the same exact release; `internal/ciaudit` fails `go test ./...` when the two drift or when either floats (`latest`, or a `vX.Y` with no patch). `just lint` refuses to run against any other binary on `PATH` - it prints what is required, what it found, and where to install it, rather than linting with whatever is there. Bumping the linter is a single commit editing both spellings and fixing whatever the new release reports (`default: all` means a new release can enable new linters).
- **Depguard allowlist is enforced.** Adding any new direct dependency requires adding the import path to `linters.settings.depguard.rules.main.allow` in `.golangci.yml`. Otherwise `just lint` fails. Read [.golangci.yml](.golangci.yml) for the current palette rather than a copy of it here: it is stdlib (`$gostd`), three `go/` packages `$gostd` does not expand to, and a handful of modules.
- **`fieldalignment`** can suggest reordering fields of public structs. Don't blindly apply `-fix` to types in `internal/galaxy/store` (snapshot-serialized) or `internal/galaxy/config` (env/CLI-bound) without checking. Use the `go-galaxy-deps` skill when fixing is intentional.
- **`govulncheck`** in `just check` runs at full strictness (no exit-code masking). Findings will fail the gate.

## Workflow

1. Classify scope: single analyzer failure → run that subtarget directly via `go tool …`; broad change → `just check`.
2. Iterate on the failing gate only; expand to `just check` once it passes.
3. After code changes, run `just lint` separately - it's not part of `just check`.
