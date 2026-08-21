# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

go-galaxy is a fast Ansible Galaxy collections installer for CI, written in Go (module `github.com/greeddj/go-galaxy`). Collections only, from Galaxy API servers and from git repositories; roles are ignored with a warning. `vendor/` is git-ignored, not committed: it is regenerated locally by `just deps` and is absent in CI, so a local build resolves through it while CI downloads modules.

## Documentation

`README.md` is a landing page and an index; the reference material lives in
`docs/`. Keep them true after a behavior change - `docs/cli.md` (commands and
options), `docs/configuration.md` (ansible.cfg and the environment surface),
`docs/servers-and-auth.md`, `docs/signatures.md`,
`docs/ansible-galaxy-compat.md` (every deliberate divergence),
`docs/caching.md`, `docs/ci.md`, `docs/exit-codes.md`, `docs/metrics.md`,
`docs/security.md`, `docs/benchmarks.md`, `docs/architecture.md` (how it
works), `docs/development.md` (tests, gates, lint). `cmd/go-galaxy/main.go`'s
`--help` exit-code index is generated from the same phrases as
`docs/exit-codes.md` and must not drift from it.

## Commands

Development is driven by the Justfile:

```bash
just test          # go test ./...
just check         # go vet, staticcheck, govulncheck, fieldalignment (via go tool)
just lint          # golangci-lint run ./... (requires the exact pinned version, see below)
just fix           # go fix + fieldalignment -fix (autofixes struct field ordering)
just deps          # go mod tidy && go mod vendor - run after any dependency change
just build         # runs check+lint+test, then builds ./dist/go-galaxy
```

Single test: `go test ./internal/galaxy/archive/ -run 'TestName'` (standard Go; tests live beside the code, in-package by default - the exception is the `internal/galaxy/collections` e2e suite, which is `package collections_test` so it drives the public API from outside; the `testpackage` linter is disabled so both styles are legal).

CI (`.github/workflows/ci.yml`) runs the same checks plus `go test -v -race -coverprofile=...`, so run tests with `-race` before considering concurrency work done.

Benchmarks: `testing/bench.sh` (needs `hyperfine`, a `.venv` with ansible-core, and `docker compose -f testing/docker-compose.yaml up -d minio-svc` for the s3-* scenarios). See README "Benchmarks".

## Things that fail the build in non-obvious ways

- **The test suite audits the source itself.** Several packages contain only tests that parse the repo with go/ast or enumerate files through git; `go test ./...` runs them all:
  - `internal/proseaudit`: no committed file may contain an em dash (U+2014) or en dash (U+2013) - hyphen-minus only, in prose, code, comments, and commit text. Also: a comment may cite a `foo_test.go:NNN` line only if a test failure could be attributed to that line, and may never cite a production file's line number - reference production code by identifier instead.
  - `internal/lockaudit`: every command that takes the cache backend's exclusive lock must run its work under the holder context that lock returned, judged via `cacheManager.LockLostError`. Checked over closed tables - adding or renaming such a command means updating the table, and a table entry naming a missing function fails rather than skips.
  - `internal/ciaudit`: gates workflow cross-references and the golangci-lint version literal, which is spelled in both the Justfile (`GOLANGCI_LINT_VERSION`) and `.github/workflows/ci.yml` and must match. Bump both in one commit.
  - `internal/galaxy/store` has a dirty-flag audit (every write-locked `*Store` method must set the dirty flag; a new bucket also needs `helpers.StoreSnapshotSchemaVersion` bumped, drop-and-rebuild), `internal/galaxy/archive` gates its probe's decompressor path, and `internal/gzipstream` has a monopoly gate: no package outside it may import klauspost/pgzip for reading.
- **golangci-lint runs with `default: all`** and a short disable list (`.golangci.yml`). depguard has an explicit import allow-list - importing a new module requires adding it there, plus `just deps` to update go.mod/vendor.
- **fieldalignment is enforced** (`just check` and CI), so struct field order matters; `just fix` reorders automatically.

## Architecture

The layering is: `cmd/go-galaxy` is wiring, `internal/galaxy/*` is the work, `internal/cache/*` is persistence behind a seam, and a few leaf packages hold cross-cutting concerns.

- `cmd/go-galaxy`: main.go (signal handling, exit-code decision via `exitcode`), `cliflags` (flag declarations), `commands` (urfave/cli command tree: install, cleanup, lock, warm, hash, tree, explain, outdated). install/cleanup/lock/warm/outdated share `runCollectionCommand`, which builds the `*config.Config`, the progress printer, and the `*infra.Infra`. hash/tree/explain work straight from files on disk.
- `internal/galaxy/collections`: the resolve-download-verify-extract-record pipeline behind install/warm/lock/outdated. Cache access funnels through `withBackend` (open, take exclusive lock, run under the holder context). A git requirement is expanded by `expandGitRoots` (git_discovery.go) before the solver runs: its repository is fetched, its collections built and committed to the artifact store, and each becomes an exact-pin root whose `Source` is the locator `git+<url>#<subdir>@<commit>` - the one string every downstream consumer (artifact key, installed record, snapshot, cleanup) keys on.
- Git sources, three leaf packages: `internal/galaxy/gitsource` is the grammar (URL, ref, subdir, locator, credential matching) and the `Client` interface, and imports no go-git; `internal/galaxy/gitfetch` is the only production importer of go-git (advertise once, fetch by hash through the upload-pack session into a byte-capped on-disk store, read the tree without a checkout) and implements `Client`; `internal/galaxy/collectionbuild` turns a tree into the deterministic `tar.gz` with `MANIFEST.json`/`FILES.json` that `ansible-galaxy collection build` would produce, and is the only production tar writer. `gitfetch.New` uninstalls go-git's `file` and `git` transports and switches off its `~/.ssh/config` reader, once per process; a test pins both.
- **Cache seam**: `internal/galaxy/cache` declares the interfaces (`Backend` for state/locking, `ArtifactStore` for tarballs) plus decorators and the cache-policy-aware JSON fetch. `internal/cache` is the only factory choosing a concrete backend: `internal/cache/local` (BoltDB snapshot + JSON registry + flock) or `internal/cache/s3` (gzipped-JSON objects, hand-rolled SigV4 signing - no AWS SDK - and a distributed lock on conditional writes with a heartbeat).
- `internal/galaxy/helpers`: bottom of the import graph. Sentinel errors (matched with `errors.Is` across layers), size caps and tuning constants, and validation predicates (`IsPathElement`, `IsCollectionName`, `IsSHA256Hex`, `IsExactVersion`). A value's shape is judged by one rule at the boundary it enters through.
- `internal/galaxy/infra`: per-run DI container (printer, shared HTTP client, the git client and revealed git credentials, clock, metrics counters, test-only deadline overrides). Extend Infra rather than adding a global or widening signatures.
- `internal/galaxy/solver`: pure, deterministic PubGrub-style version solver; all metadata comes through its Provider seam, no I/O.
- `internal/galaxy/lockfile`: pins every transitive collection to exact version + SHA256, or, for a git source, to its commit (`type: git`, `ref`, `commit`, `subdir`, no sha256); a file holding a git entry is schema 2, one without stays schema 1 byte for byte. With a lockfile, install reads only the cache, never the Galaxy API or the remote.
- Output: `internal/galaxy/output` declares the `Printer` interface (tiers behave differently under --quiet/--verbose); `internal/progress` implements it (spinner only on a TTY). `internal/safeout` strips terminal control sequences from any text of external origin before it is printed - use it for anything derived from network responses, archives, or paths.
- `internal/gzipstream`: the single place a gzip reader is opened over untrusted bytes (context observed on the compressed side, zero-byte members refused).
- `internal/testing/fakegalaxy`: in-memory Galaxy v3 API double with fault injection, request counting, and auth capture - use it for any test touching HTTP paths; nothing in tests hits a real network. `internal/testing/fakegit` is its sibling for a git remote: an in-process smart-HTTP server and ssh listener (plus agent and known_hosts fixtures) over in-memory repositories with deterministic commits; the collections suite uses an in-memory `gitsource.Client` double instead and leaves the transport to `gitfetch`'s own tests.

## Security posture (deliberate boundaries, keep them)

Untrusted input is validated where it enters (`requirements` parsing, `manifest` reading, `archive` extraction with sentinel-named refusals and path/symlink/size caps). Filesystem writes under the cache go through `os.Root` so nothing beneath it can redirect a write or delete outside the configured tree. Credentials are `Secret` values that redact on every serialization path (`config`). `manifest` and `signature` are read-only by design. A server URL or relaxed TLS setting from ansible.cfg is never paired with a token from another source. A git repository URL is repository content: it may carry no credential, its path is held to a conservative alphabet, and the credential it gets is bound to a host through the environment (`GO_GALAXY_GIT_*`) and matched by origin and path prefix; git traffic runs on its own HTTP client (`fetch.NewGit`) that carries no Galaxy token and refuses cross-origin redirects, an ssh host key must be in known_hosts, no process is ever executed, and a fetched tree is validated entry by entry before anything is built from it. Don't casually loosen any of these; each package's doc comment states the boundary it owns.

## Conventions

- Package doc comments here are load-bearing and unusually thorough: read the package comment (`go doc ./internal/<pkg>`) before editing a package, and keep it true after your change - proseaudit gates some comment/code cross-references.
- Comments state constraints and reasons, not narration; match that register.
- Only hyphen-minus in everything committed (enforced by test, see above).
