---
name: go-galaxy-deps
description: Run mutating maintenance commands for the go-galaxy Go repo — go mod tidy/vendor sync and automated fixers (go fix, fieldalignment -fix). Use ONLY when the user explicitly asks to update dependencies, re-vendor, or apply automated code fixes. These commands rewrite tracked files (vendor/, go.mod, go.sum, source files) — do not invoke them as part of routine validation.
---

# go-galaxy — Dependency Sync & Auto-Fix (mutating)

## When to use

- "обнови зависимости" / "update deps" / "re-vendor"
- "примени fieldalignment -fix" / "apply auto-fix"
- After `go get` of a new module, before lint

**Do not run these as part of routine checks.** They mutate tracked files. Note that `just check`, `just build`, `just build_linux`, and `just run` *implicitly* run `just deps` already — call out vendor churn before invoking those if the user is mid-PR.

## Commands

| Intent | Command | Mutates |
|---|---|---|
| Sync go.mod and vendor/ | `just deps` | `go.mod`, `go.sum`, `vendor/` |
| Apply `go fix` + `fieldalignment -fix` | `just fix` | source files across the repo |

`just deps` runs:

```
go mod tidy
go mod vendor
```

`just fix` runs:

```
go fix ./...
go tool fieldalignment -fix ./...
```

## Adding a new direct dependency

1. `go get <module>@<version>`.
2. Add the **exact import path** to `linters.settings.depguard.rules.main.allow` in [.golangci.yml](.golangci.yml). **`just lint` fails otherwise** — the allowlist is enforced.
3. `just deps` to sync `vendor/`.
4. `just lint` to confirm.

The current allowlist (stdlib `$gostd` plus): `github.com/greeddj/go-galaxy`, `github.com/BurntSushi/toml`, `github.com/Masterminds/semver`, `github.com/briandowns/spinner`, `github.com/klauspost/pgzip`, `github.com/psvmcc/hub`, `github.com/urfave/cli/v3`, `go.etcd.io/bbolt`, `gopkg.in/yaml.v3`.

## fieldalignment -fix caveats

`fieldalignment -fix` reorders struct fields. This is normally safe for internal types, but be careful with:

- **`internal/galaxy/store`** — `Store`/snapshot types are JSON-serialized for the S3 backend and BoltDB-bucketed locally. Field order doesn't affect JSON, but a bumped `helpers.StoreSnapshotSchemaVersion` is required if semantics change. Don't change the meaning under cover of `-fix`.
- **`internal/galaxy/config`** — fields are tagged for env/CLI bindings (`urfave/cli/v3`). Tags travel with fields, but review the diff to make sure tag-bound fields still group logically.
- **`cacheManager.Backend` and `ArtifactStore` interfaces** ([internal/galaxy/cache/backend.go](internal/galaxy/cache/backend.go)) — interfaces themselves aren't reordered, but implementations in `internal/cache/{local,s3}` should keep reviewable diffs.

Apply selectively: run `go tool fieldalignment ./...` first to see suggestions, then decide whether `-fix` makes sense package-by-package.

## Workflow

1. State explicitly to the user that this is a mutating operation before running.
2. Run the targeted command.
3. Show `git diff --stat` afterward so the user can review scope.
4. Suggest `just lint && just test` to confirm nothing regressed.
