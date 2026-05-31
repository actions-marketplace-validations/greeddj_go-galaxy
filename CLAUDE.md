# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`go-galaxy` is a fast Ansible Galaxy collections installer aimed at CI. It only handles **Galaxy collection sources** — `roles`, git/url/file/dir sources are intentionally unsupported. Drop-in compatibility with a subset of `ansible.cfg` (`[defaults] collections_path`, `[galaxy] server`, `[galaxy] cache_dir`) is a hard requirement.

## Common commands

The project uses `just` (Justfile) as the task runner.

- `just deps` — `go mod tidy && go mod vendor`. The repo is **vendored**; commit `vendor/` updates with dependency changes.
- `just lint` — `golangci-lint run ./... --timeout=5m`.
- `just test` — `go test ./...`.
- `just check` — runs `go vet`, `staticcheck`, `govulncheck`, and `fieldalignment` (all wired through `go tool` directives in `go.mod`).
- `just fix` — `go fix` + `fieldalignment -fix`.
- `just run` — runs `check` + `lint` + `test`, then `go run -race ./cmd/go-galaxy/main.go`.
- `just build` / `just build_linux` — produce binaries under `dist/` with version ldflags injected.
- `just oci [executor=podman] [tag=local]` — builds a Linux binary then a local OCI image.

Run a single test: `go test ./internal/galaxy/store -run TestSnapshot -race -v` (substitute package and `-run` regex). CI (`.github/workflows/ci.yml`) runs `go vet`, `staticcheck`, `govulncheck`, `golangci-lint`, and `go test -race -coverprofile=...`.

## High-level architecture

### Entry flow
`cmd/go-galaxy/main.go` builds a `urfave/cli/v3` app with `install` (default) and `cleanup` subcommands. Both commands follow the same pattern (see [install.go](cmd/go-galaxy/commands/install.go) and [cleanup.go](cmd/go-galaxy/commands/cleanup.go)):

1. `config.BuildCollectionConfig(c)` — merges CLI flags, env vars, and `ansible.cfg` into `*config.Config`. Anything taken from `ansible.cfg` is flagged via `Ansible*Used` booleans for debug logging.
2. `progress.New(verbose, quiet)` — a `Printer` that captures `log` output and renders spinner/progress UI.
3. `infra.New(printer, http.Client)` — the runtime container (`Output`, `HTTP`, `Now`, `TempDir`) threaded through every subsystem. Prefer extending `Infra` rather than passing more globals.
4. Dispatches to `collections.Start` or `cleanup.Start`.

### Cache backend abstraction
The `cacheManager.Backend` interface in [internal/galaxy/cache/backend.go](internal/galaxy/cache/backend.go) is the single seam between business logic and persistence. Two implementations:

- [internal/cache/local](internal/cache/local) — filesystem under `cfg.CacheDir`; uses Bolt files for state and a directory layout for tarballs.
- [internal/cache/s3](internal/cache/s3) — S3-compatible object store (selected when `--s3-bucket` / `GO_GALAXY_S3_BUCKET` is set). Includes its own minimal S3 client, distributed locking via conditional `PUT` + lock TTL, and gzip-on-the-wire for the snapshot.

`internal/cache/cache.go::New` is the factory that picks one based on `cfg.S3Cache.Enabled`. **Add new backends here**, and make sure they implement `Backend` and return an `ArtifactStore`.

### Snapshot store
[internal/galaxy/store](internal/galaxy/store) defines `Store` — the in-memory representation of cached state (API responses, deps cache, installed collections, dependency graph, resolved versions, project registry). It is bucket-mapped to BoltDB files locally and serialized as gzipped JSON for S3.

- `helpers.StoreSnapshotSchemaVersion` gates compatibility; bumping it requires a migration path. `validateSnapshotSchema` rejects newer schemas.
- All `Store` methods are protected by an internal `RWMutex`; do not access fields directly across goroutines.
- `RecordProject` keeps a per-project registry consumed by `cleanup` to compute reachability across all known requirements files.

### Install pipeline (`internal/galaxy/collections`)
`collections.Start` → `runInstall` orchestrates:

1. `initInstall` — open backend, acquire backend lock, load `Store`, optionally clear caches, record this project.
2. `prepareInstallPlan` — load + parse `requirements.yml`, resolve transitive deps (`resolve.go`), build `collections` map keyed by `<ns>.<name>@<version>`, kick off the prefetcher, then topologically split into `levels` (`buildInstallLevels`).
3. `installLevels` — for each level, fan out installs to `cfg.Workers` goroutines (semaphore-bounded). On any failure within a level, the loop breaks before the next level. The prefetcher hands off downloaded artifact metadata via `prefetch.Wait(key)`.
4. `finalizeInstall` — save the snapshot back to the backend; if any failures, return a wrapped `ErrInstallationFailed`.

### Cleanup pipeline (`internal/galaxy/cleanup`)
Walks every project recorded in the registry, scans `<collections_path>/ansible_collections/**/MANIFEST.json`, builds a reachability set from each project's `requirements.yml`, and removes both the on-disk install and the cached artifact for unreachable `ns.name@version` keys. `--dry-run` prints candidates without deleting.

### Output / progress
`progress.New` returns a `Printer` (also satisfies `io.Writer` so `log.SetOutput(p)` redirects all stdlib logging through it). In verbose mode, `Debugf` and `DebugSincef` are active; in quiet mode, only `PersistentPrintf` survives. Use `Output.Errorf`/`Okf` from inside workers — the printer is goroutine-safe.

## Conventions

- **Dependencies are gated** by `depguard` in [.golangci.yml](.golangci.yml). Adding a new external dependency requires editing the allow-list. Standard library + the listed third-party modules are the entire palette.
- **Linter is strict**: `default: all` minus a small disable list. Common offenders to watch for: `lll` (140 cols), `fieldalignment` (struct field ordering), `revive`/`staticcheck` package comments. Run `just check` and `just lint` before committing.
- **Errors**: package-level sentinel errors live in `internal/galaxy/helpers` (e.g. `ErrInstallationFailed`, `ErrDuplicateCollectionKey`). Wrap with `%w` when crossing layers.
- **Concurrency**: per-level worker pools use a buffered-channel semaphore (`sem := make(chan struct{}, cfg.Workers)`) plus `sync.WaitGroup` (`wg.Go` from Go 1.26). Failures are tracked with `atomic.Int32`.
- **Bolt buckets and snapshot file names** are constants in `internal/galaxy/helpers`. Reuse them rather than string-literaling.
- The `Justfile`'s `LDFLAGS` injects `Version`, `Commit`, `Date`, `BuiltBy` into `cmd/go-galaxy/main.go`. Don't add module-time fallbacks elsewhere.

## Skills & agents

Project-local skills live under [.claude/skills](.claude/skills) and a code-reviewer agent under [.claude/agents](.claude/agents). Use them rather than re-deriving the workflow from this file:

- **[go-galaxy-build](.claude/skills/go-galaxy-build/SKILL.md)** — host build, Linux amd64 build, OCI image. Pre-build chains and `dist/` artifact paths.
- **[go-galaxy-check](.claude/skills/go-galaxy-check/SKILL.md)** — `just lint` / `just check` quality gates, individual analyzer commands, depguard allowlist gotchas.
- **[go-galaxy-deps](.claude/skills/go-galaxy-deps/SKILL.md)** — mutating ops only: `just deps`, `just fix`. Adding a new direct dependency.
- **[go-galaxy-test](.claude/skills/go-galaxy-test/SKILL.md)** — full and per-package test runs, `-race` policy, where suites live.
- **[go-galaxy-reviewer](.claude/agents/go-galaxy-reviewer.md)** — agent that audits non-trivial diffs against layer discipline and the hard rules below before commit/PR.

Permissions and a `gofmt` post-edit hook are pre-wired in [.claude/settings.json](.claude/settings.json).

## Things that look stale but aren't

- The README's TODO mentions "мигрировать на github.com/urfave/cli/v3" but the project already uses `urfave/cli/v3`. The TODO line is outdated — verify before acting on it.
- `vendor/` is checked in. CI builds expect it; `just deps` regenerates it.
