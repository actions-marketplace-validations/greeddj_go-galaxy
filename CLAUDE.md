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
- The S3 backend's `LoadStore` checks the persisted schema version via a lightweight meta-only probe before decoding the full payload, matching the local backend's schema-first `Load` order.
- `Backend` also exposes `SweepTemp(ctx)`, called once by `initInstall` while the exclusive lock is held. The local backend removes leftover `.download-*` files from the cache directory; the S3 backend is a no-op, since its download temps live under the OS temp directory instead of the shared cache.
- **Trust model:** the S3 snapshot object and the project registry object are a trust boundary. `Backend.LoadStore`/`LoadProjectRegistry` serve cached Galaxy metadata (including an artifact's download URL and sha256) without re-validating it against the origin on every use, so a principal who can write the bucket can influence what a run installs. Operators must restrict bucket write access (a write-restricted ACL or dedicated credentials), just as they would protect the local cache directory; the local Bolt snapshot is implicitly trusted for the same reason, since writing it already requires local filesystem access to the cache directory. Both state-object reads are size-capped (`helpers.StateObjectMaxCompressedSize`, `helpers.StateObjectMaxDecompressedSize`), so a hostile-but-writable bucket cannot OOM the process with an oversized object or a gzip bomb.
- `warnIfOffServerDownloadHost` (in `internal/galaxy/collections`) warns, rather than blocks, when an artifact's download host differs from the configured Galaxy server host: the download URL can originate from a poisoned snapshot, but a legitimate deployment may still serve downloads from a separate content host or object storage.

`internal/cache/cache.go::New` is the factory that picks one based on `cfg.S3Cache.Enabled`. **Add new backends here**, and make sure they implement `Backend` and return an `ArtifactStore`.

### Snapshot store
[internal/galaxy/store](internal/galaxy/store) defines `Store` — the in-memory representation of cached state (API responses, deps cache, installed collections, dependency graph, resolved versions, project registry). It is bucket-mapped to BoltDB files locally and serialized as gzipped JSON for S3.

- `helpers.StoreSnapshotSchemaVersion` gates compatibility; bumping it requires a migration path. `store.ValidateSchema` rejects newer schemas.
- The current snapshot schema version is 4. At persist time, entries in the `APICache`, `DepsCache`, and `Versions` buckets are age-evicted against `helpers.CacheEntryMaxAge` (30 days) and stamped at write time, all inside the shared `snapshotData()` copy path, so both the local Bolt `Save` and the S3 `MarshalSnapshot` inherit the same eviction. The migration path for this bump is drop-and-rebuild: a snapshot persisted under an older schema is dropped rather than migrated in place, and its caches rebuild cold on the next run.
- All `Store` methods are protected by an internal `RWMutex`; do not access fields directly across goroutines.
- `RecordProject` keeps a per-project registry consumed by `cleanup` to compute reachability across all known requirements files.

### Install pipeline (`internal/galaxy/collections`)
`collections.Start` → `runInstall` orchestrates:

1. `initInstall` — open backend, acquire backend lock, load `Store`, optionally clear caches, record this project.
   After acquiring the lock and before loading the store, `initInstall` sweeps dead-run temp orphans: leftover `.download-*` artifact temps and the extracted store's `ingest-*` / `<sha>.tmp` directories. This is safe only because the exclusive lock guarantees no other run is mid-write, and it is best-effort - a sweep failure just logs a warning and the install proceeds.
2. `prepareInstallPlan` — load + parse `requirements.yml`, resolve transitive deps (`resolve.go`), build `collections` map keyed by `<ns>.<name>@<version>`, kick off the prefetcher, then topologically split into `levels` (`buildInstallLevels`).
3. `installLevels` - for each level, fan out installs to `cfg.Workers` goroutines (semaphore-bounded). On any failure within a level, the loop breaks before the next level. The prefetcher hands off downloaded artifact metadata via `prefetch.Wait(key)`. A cache-hit artifact that fails its pin/hash check or fails to extract is evicted (tarball plus sidecar) and refetched exactly once, then re-verified and re-extracted; offline runs never evict. The same bounded evict-and-refetch-once also covers a cache-resident object that fails a backend's read-time integrity check (the S3 store verifies a fetched object's sha256 on read, surfaced from `prepareInstall` itself), not only a pin/hash or extract failure, and the `warm` command shares this same bounded recovery through the same `prepareWithRecovery` helper, so the recovery is not limited to `installLevels`.
   The extracted store verifies that a tarball hashes to its sha key before ingesting it, so a rotted or tampered cached tarball can never poison the shared content-addressable store with content that does not hash to its key; do not reintroduce a permissive ingest that skips this check.
   `verifyPinnedSHA` checks a lockfile pin against `resolveArtifactSHA`'s result, which for a pinned collection always re-hashes the actual downloaded or on-disk bytes rather than trusting a cache-hit's recorded metadata sha, so a `--frozen` install is immune to a poisoned snapshot: a poisoned download URL or sha fails the install closed instead of installing attacker content.
4. `finalizeInstall` — save the snapshot back to the backend; if any failures, return a wrapped `ErrInstallationFailed`.

### Cleanup pipeline (`internal/galaxy/cleanup`)
Walks every project recorded in the registry, scanning only the fixed `<collections_path>/ansible_collections/<namespace>/<name>/MANIFEST.json` layout (not a full `**` tree walk); a project whose `ansible_collections` workspace is absent is skipped, but an unreadable or unparseable `requirements.yml` for a present project aborts the whole run without deleting anything. Reachability is built from each project's requirements roots plus the transitive dependencies declared in each MANIFEST, and every on-disk copy of an unreachable `ns.name@version` key is removed together with its cached artifact, skipping (with a warning) any unsafe identifier and refusing any path resolving outside a project's `ansible_collections` directory. The extracted-store sweep keeps entries referenced by the persisted snapshot's installed set rather than an on-disk scan, so an absent workspace no longer wipes the extracted cache; `--dry-run` reports both the collection-removal and extracted-store sweep candidates without deleting.

### Output / progress
`progress.New` returns a `Printer` (also satisfies `io.Writer` so `log.SetOutput(p)` redirects all stdlib logging through it). In verbose mode, `Debugf` and `DebugSincef` are active; in quiet mode the transient tier (`Printf`, `Write`) and the debug tier are suppressed, while the result tier (`PersistentPrintf`, `Okf`, `Errorf`) always emits. In non-TTY normal mode (typical CI), all non-debug output is printed as plain lines with no spinner, instead of being silently dropped. Regular output goes to stdout; `Errorf` (both the method and the package-level helper) goes to stderr, so diagnostics do not contaminate stdout consumers. Use `Output.Errorf`/`Okf` from inside workers - the printer is goroutine-safe.

## Conventions

- **Dependencies are gated** by `depguard` in [.golangci.yml](.golangci.yml). Adding a new external dependency requires editing the allow-list. Standard library + the listed third-party modules are the entire palette.
- **Linter is strict**: `default: all` minus a small disable list. Common offenders to watch for: `lll` (140 cols), `fieldalignment` (struct field ordering), `revive`/`staticcheck` package comments. Run `just check` and `just lint` before committing.
- **Errors**: package-level sentinel errors live in `internal/galaxy/helpers` (e.g. `ErrInstallationFailed`, `ErrDuplicateCollectionKey`). Wrap with `%w` when crossing layers.
- **Concurrency**: per-level worker pools use a buffered-channel semaphore (`sem := make(chan struct{}, cfg.Workers)`) plus `sync.WaitGroup` (`wg.Go` from Go 1.26). Failures are tracked with `atomic.Int32`.
- **Bolt buckets and snapshot file names** are constants in `internal/galaxy/helpers`. Reuse them rather than string-literaling.
- The `Justfile`'s `LDFLAGS` injects `Version`, `Commit`, `Date`, `BuiltBy` into `cmd/go-galaxy/main.go`. Don't add module-time fallbacks elsewhere.

## Skills, commands & agents

Project tooling lives under [.claude/skills](.claude/skills) (lazy reference procedures), [.claude/commands](.claude/commands) (quick slash commands), [.claude/agents](.claude/agents) (role specialists), and [.claude/workflows](.claude/workflows) (orchestration). Use them rather than re-deriving the workflow from this file.

**Skills:**

- **[go-galaxy-build](.claude/skills/go-galaxy-build/SKILL.md)** - host build, Linux amd64 build, OCI image. Pre-build chains and `dist/` artifact paths.
- **[go-galaxy-check](.claude/skills/go-galaxy-check/SKILL.md)** - `just lint` / `just check` quality gates, individual analyzer commands, depguard allowlist gotchas.
- **[go-galaxy-deps](.claude/skills/go-galaxy-deps/SKILL.md)** - mutating ops only: `just deps`, `just fix`. Adding a new direct dependency.
- **[go-galaxy-test](.claude/skills/go-galaxy-test/SKILL.md)** - full and per-package test runs, `-race` policy, where suites live.
- **[go-galaxy-coverage](.claude/skills/go-galaxy-coverage/SKILL.md)** - coverage profiling and gap analysis (the tester's main tool).

**Commands (Just-based, run on demand):** `/gg-check`, `/gg-lint`, `/gg-test [scope]` run the matching `just` target and summarize; `/gg-pipeline <task>` launches the architect-gated delivery pipeline.

**Agents (the main thread delegates to these):**

- **[go-architect](.claude/agents/go-architect.md)** (opus, `effort: xhigh`) - lead engineer and design authority, the brain of the fleet. Delegate non-trivial work here first; it designs, gates every stage, and may reject a weak plan outright.
- **[go-developer](.claude/agents/go-developer.md)** (sonnet) - implements approved designs: code, English comments, unit tests, targeted gate runs.
- **[go-tester](.claude/agents/go-tester.md)** (sonnet) - runs unit/integration/e2e tests and audits coverage; reports gaps to cover or to justify.
- **[go-security](.claude/agents/go-security.md)** (opus, `effort: xhigh`) - audits for CVEs, dangerous code, external attack surface, and host harm, including cross-layer combinations.
- **[go-techwriter](.claude/agents/go-techwriter.md)** (sonnet) - verifies comments and writes docs; enforces English-only prose.

Permissions and a `gofmt` post-edit hook are pre-wired in [.claude/settings.json](.claude/settings.json).

## Multi-agent workflow & rules

- **Subagents cannot call each other.** Claude Code subagents are terminal: each returns a report and cannot invoke another agent. All routing goes through the main thread, or through the deterministic orchestrator in [.claude/workflows/architect-pipeline.js](.claude/workflows/architect-pipeline.js) (run via the `Workflow` tool or `/gg-pipeline`).
- **The architect gates everything.** For a non-trivial change the order is: architect (design, approve/reject) -> developer (implement) -> architect (review) -> tester (tests + coverage) -> architect -> security (audit) -> architect -> tech-writer (docs) -> architect (sign-off back to the main thread). The architect loops work back to the developer on any `rework`, and only signs off when it is "written ideally, it will not get better than this".
- **The architect defaults to NO.** Under-specified, architecturally wrong, unjustified-dependency, or low-value requests get rejected, not coded around.
- **English only.** All code, comments, tests, docs, and other prose are in English; the tech-writer enforces it.
- **Hyphen-minus only.** Never an em (U+2014) or en (U+2013) dash anywhere - prose, code, comments, or commit text.
- **Efficiency is a first-class requirement,** not a nice-to-have: minimal allocations per tick, pre-sized buffers, no needless copies on hot paths.

## Things that look stale but aren't

- The README's TODO mentions "мигрировать на github.com/urfave/cli/v3" but the project already uses `urfave/cli/v3`. The TODO line is outdated — verify before acting on it.
- `vendor/` is checked in. CI builds expect it; `just deps` regenerates it.
