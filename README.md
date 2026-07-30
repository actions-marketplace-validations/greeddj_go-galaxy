# go-galaxy

Fast Ansible Galaxy collections installer for CI.

> **Note:** This project was created in collaboration with the Claude Code.

## Benchmarks

`go-galaxy` vs `ansible-galaxy` on three requirements files (1 / 10 / 100 root
collections, all without transitive deps for an apples-to-apples fetch+extract
comparison). Numbers are mean ± σ from `hyperfine` (3 measured runs, 1 warmup
where applicable).

| Scenario               |            1 collection |         10 collections |        100 collections |
|:-----------------------|------------------------:|-----------------------:|-----------------------:|
| **cold cache**         |                         |                        |                        |
| ・ansible-galaxy       |           9.51 ± 1.66 s |         53.15 ± 5.04 s |       552.02 ± 79.82 s |
| ・go-galaxy            |           6.14 ± 0.44 s |          5.04 ± 0.16 s |         20.37 ± 1.39 s |
| ・**speedup**          |               **1.55×** |             **10.56×** |             **27.10×** |
| **warm cache**         |                         |                        |                        |
| ・ansible-galaxy       |           4.51 ± 0.15 s |         32.54 ± 1.63 s |        313.49 ± 9.85 s |
| ・go-galaxy            |           1.32 ± 0.02 s |          2.44 ± 0.17 s |         11.56 ± 0.07 s |
| ・**speedup**          |               **3.41×** |             **13.35×** |             **27.13×** |
| **frozen + offline**   |                         |                        |                        |
| ・go-galaxy            |           1.33 ± 0.00 s |          2.07 ± 0.10 s |         11.93 ± 0.34 s |

The speedup grows with the number of collections - `go-galaxy` parallelizes
downloads and extractions across `--workers` cores, uses hard links from a
content-addressable cache on warm runs, and skips the network entirely under
`--frozen --offline`. With a lockfile and warm caches, installing 100
collections takes ~12 s instead of ~5 minutes.

Reproduce locally:

```bash
brew install hyperfine
python3 -m venv .venv && .venv/bin/pip install ansible-core
go build -o ./dist/go-galaxy ./cmd/go-galaxy
testing/bench.sh                          # all sizes, all scenarios
SIZES=10 testing/bench.sh                 # one file
SCENARIOS="warm" RUNS=5 testing/bench.sh  # one scenario, more runs
```

<details>
<summary>Methodology</summary>

- **Tools:** `ansible-galaxy [core 2.20.5]`, `go-galaxy v1.0.2`, `hyperfine 1.20.0`.
- **Both tools** invoked with `--no-deps` so the benchmark measures fetch +
  extract (the dep-resolution paths in the two tools differ; in particular
  `requirements-100.yml` has transitive constraint conflicts that `ansible-
  galaxy` resolves leniently and `go-galaxy` rejects strictly - that's a
  separate comparison).
- **Cold cache:** `~/.ansible/galaxy_cache`, `~/.cache/go-galaxy` and the
  install dir wiped before each run. `ANSIBLE_COLLECTIONS_PATH=$TARGET` so
  `ansible-galaxy` doesn't see anything pre-installed in `~/.ansible/collections`.
- **Warm cache:** caches primed once, only the install dir wiped between runs.
- **Frozen + offline:** `go-galaxy lock` once, then `go-galaxy install --frozen
  --offline` - zero network calls.
- **Hardware:** single Apple Silicon laptop, runs on home Wi-Fi. Cold-cache
  numbers are network-bound; warm/frozen are CPU/IO-bound.

</details>

## Motivation

CI pipelines often spend minutes downloading and unpacking Galaxy collections.
go-galaxy is built to reduce that wait time with faster installs and smarter caching,
so pipelines finish sooner and changes ship faster.

## Scope

- Collections only (Galaxy API sources).
- `requirements.yml` must contain a `collections` list.
- `roles` entries are ignored with a warning.
- ansible.cfg options supported:
  - `[defaults] collections_path`
  - `[galaxy] server`
  - `[galaxy] server_list`
  - `[galaxy] cache_dir`
  - `[galaxy_server.<id>]` sections (`url`, `token`, `validate_certs`; see
    [Galaxy servers and authentication](#galaxy-servers-and-authentication))

## Features

- Dependency resolution with snapshot reuse.
- API and tarball caches.
- Skip install if already extracted.
- Parallel downloads and extraction.
- `cleanup` command to remove unreachable collections.

## Install

### Go

```bash
go install github.com/greeddj/go-galaxy/cmd/go-galaxy@latest
```

Binary is installed into `$(go env GOPATH)/bin` (usually `~/go/bin`).

### Homebrew (macOS)

```bash
brew tap greeddj/tap
brew install go-galaxy
```

### Podman

```bash
podman run --rm -v "$PWD":/work -w /work ghcr.io/greeddj/go-galaxy:latest --help
```

Example install:

```bash
podman run --rm -v "$PWD":/work -w /work ghcr.io/greeddj/go-galaxy:latest i -r requirements.yml -p ./collections
```

### Build from source

```bash
go build -o ./dist/go-galaxy ./cmd/go-galaxy
```

## Usage

```bash
./dist/go-galaxy i -r requirements.yml -p ./collections
```

Clean unreachable collections:

```bash
./dist/go-galaxy c
```

### Commands

- `install` (`i`) - install collections from `requirements.yml`.
- `lock` (`l`) - resolve and write `requirements.lock.yml` for reproducible CI. `--frozen` has no effect on `lock` - the lockfile is always regenerated from a fresh resolve and the run warns on stderr; use `install --frozen` or `warm --frozen` to actually consume an existing lockfile. `--dry-run` is not implemented on `lock` - see the breaking-change note under [install options](#install-options).
- `warm` (`w`) - populate the artifact + extracted caches without installing (for CI image bake). Requires a cache: `--no-cache` is rejected as a usage error rather than downloading everything and discarding it. A warmed collection's extracted tree is protected from `cleanup` for 30 days after its last warm, so a machine that warms and then stops warming eventually reclaims the space. Under `--dry-run`, `warm` reports per collection whether it is already warm or would be warmed, downloads no artifact, and writes no warmed entry; it still rejects `--no-cache` as a usage error regardless of `--dry-run`, since `--no-cache` leaves warm nothing to do either way - see [install options](#install-options) for the full `--dry-run` semantics.
- `hash` (`h`) - print a deterministic cache key (`sha256:…`) for use as a CI cache key.
- `tree` (`t`) - print the resolved dependency tree from the lockfile; requires a lockfile and fails if one is absent.
- `explain` (`why`) - takes `<namespace.name>`; prints the locked version, source, and sha256, what requires it, and what it depends on, all read from the lockfile.
- `outdated` (`o`) - compare each lockfile entry against the latest version on its Galaxy server; requires network and is refused under `--offline`.
- `cleanup` (`c`) - remove unused cached collections across projects.

### Global options

- `--help, -h`
- `--version, -v`

### install options

- `--verbose` - verbose output (`$GO_GALAXY_VERBOSE`)
- `--quiet, -q` - quiet mode (`$GO_GALAXY_QUIET`)
- `--dry-run` (`$GO_GALAXY_DRY_RUN`) - report what `install` or `warm` would do, without
  downloading any artifact, creating any install tree or extracted tree, recording any install,
  writing any warmed entry, registering the project, honoring `--clear-cache`, or writing the
  metrics report. It still takes the exclusive cache lock. It updates the resolve-side metadata
  caches, but only when a persisted snapshot already existed for this cache; against a cache that
  was never saved before, the run saves nothing and prints a stderr warning that the caches it
  built are discarded - a preview must never leave behind a persisted-and-empty snapshot that a
  later `cleanup` would read as evidence that nothing is installed or warmed anywhere. Each
  collection is reported as would install/would warm, already up to date/already warm, or would
  fail; a would-fail verdict means the artifact isn't cached and `--offline` forbids downloading
  it, and a nonzero would-fail count exits with the install-failure code (`5`) - the same code a
  real run would exit with, because that install or warm would certainly fail. The dry-run banner
  is printed to stderr and survives `--quiet`, because the flag is env-sourced
  (`$GO_GALAXY_DRY_RUN`) and an org-wide CI environment block would otherwise turn every install
  or warm into a silent no-op.
  A dry run reports whether an artifact is cached, not whether it still matches its lockfile pin.
  Under `--frozen --offline`, any line that reports the artifact as cached - `Already warm`,
  `Would warm (artifact cached)`, or `Would install (artifact cached)` - states presence only: a
  cached tarball whose bytes have drifted off their pin fails the real run closed with a
  checksum-mismatch error, since it cannot refetch while offline. `Up to date` is not affected,
  because a real install skips such a collection without ever opening its tarball. The run prints
  a one-time stderr warning whenever both flags are set together, naming this exact gap; it is a
  disclosure, not a fix.
  **Breaking change:** before this release, `--dry-run` (and `$GO_GALAXY_DRY_RUN`) had no effect
  on `install`, `warm`, or `lock` - only `cleanup` implemented it - so `install --dry-run` performed
  a full, real install and `warm --dry-run` performed a full, real warm. They now install and warm
  nothing, and preview instead. A CI job that carried an ambient `$GO_GALAXY_DRY_RUN` and was
  really installing collections will now finish successfully with nothing installed; a bake job
  carrying the same ambient variable will now finish successfully with an empty cache, producing a
  green build and an empty image. In both cases the stderr banner above is the only signal, so a
  job that branches on the exit code alone will not notice. If a shared `$GO_GALAXY_DRY_RUN` CI
  environment variable is set, scope it to the jobs that actually want it, or unset it for
  `install` and `warm` jobs where it must not silently install or warm nothing. `--dry-run` is
  also a global flag inherited by every subcommand from the root command, so `lock` accepts it
  too - but it refuses to run under it, exiting with the usage code (`2`) and `--dry-run is not
  implemented for this command`, rather than silently overwriting the lockfile. `cleanup`
  implements its own `--dry-run` (see [cleanup options](#cleanup-options)); the read-only commands
  (`hash`, `tree`, `explain`, `outdated`) ignore the flag, since they have no product for
  `--dry-run` to suppress.
- `--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`)
- `--server` (`$GO_GALAXY_SERVER`, `$ANSIBLE_GALAXY_SERVER`)
- `--token` (`$GO_GALAXY_TOKEN`) - Galaxy API token for the single effective server;
  an error if a multi-entry `server_list` is configured, and setting it to the
  empty string clears a previously configured token (see
  [Galaxy servers and authentication](#galaxy-servers-and-authentication))
- `--timeout` (`$GO_GALAXY_SERVER_TIMEOUT`, `$ANSIBLE_GALAXY_SERVER_TIMEOUT`)
  `--timeout` is a no-progress budget - it bounds the response-header wait and the gap between two
  body reads. It bounds neither total transfer time nor a byte-drip: a server that keeps dribbling a
  few bytes into every idle window counts as making progress on every single read, so it never trips
  this timeout and can drag a download out indefinitely. Every artifact acquisition additionally
  carries a fixed, non-configurable 15-minute ceiling on the whole acquisition - the response, the
  streamed body, extraction running alongside it, the cache commit, and every retry attempt and
  backoff sleep together, not per attempt - which is what actually bounds a slow-drip transfer. This
  ceiling also covers a cache-resident artifact fetched from an S3-backed cache, since that is a full
  artifact body transfer over HTTP too, not a cheap metadata check. An ordinary collection spends
  this budget twice - once when the background prefetcher acquires it, once again when the install
  worker acquires it - so the effective per-collection ceiling is 30 minutes. On a slow enough link
  the collection fails closed with `artifact download deadline exceeded` and is not installed, rather
  than hanging or completing arbitrarily late: a maximum-size (4 GiB) artifact needs roughly
  38 Mbit/s sustained to finish inside the budget, while a real collection needs well under 1 Mbit/s.
  This ceiling is not configurable, unlike `--timeout` above: a knob on a safety ceiling is a knob an
  operator would raise in direct response to a truncation, which is exactly how the slow-drip attack
  this closes would succeed. A collection whose download stalls or drips fails that collection and the
  run exits with the install-failure code (`5`); a stall outside the per-collection install path - a
  metadata fetch during resolution, an S3 state-object read - exits with the network code (`4`)
  instead. It is never reported as an interrupt.

  Two more ceilings complete this family, both fixed and non-configurable for the identical reason: a
  knob on a safety ceiling is one an operator raises in response to a truncation, which is how the
  attack succeeds. A single Galaxy metadata request - the response, the size-limited body read, and
  every retry attempt and backoff sleep, as one shared budget - carries a fixed 2-minute ceiling: a
  maximum-size (16 MiB) response needs roughly 1.12 Mbit/s sustained to finish inside it, while the
  largest realistic response (a 10,000-version list, about 1.5 MB) needs only about 0.1 Mbit/s. The
  versions-list paging loop shares a single one of these budgets across every page it fetches, rather
  than spending a fresh one per page, since the server itself controls how many pages a resolve issues.
  A single persisted cache-state operation (loading or saving the snapshot, loading or recording the
  project registry) carries a fixed 60-second ceiling: a maximum-size (256 MiB compressed) state object
  needs roughly 35.8 Mbit/s sustained to finish inside it, while a large real snapshot (16 MiB
  compressed) needs only about 2.2 Mbit/s. This ceiling also protects every other runner sharing an
  S3-backed cache, not just the one that is stalling: these operations run while the backend's
  distributed lock is held, so an unbounded one blocks every other runner against that bucket until it
  gives up waiting for the lock - which is why its budget is tighter than the artifact ceiling above.
- `--download-path, -p` (`$GO_GALAXY_COLLECTIONS_PATH`, `$ANSIBLE_COLLECTIONS_PATH`)
- `--requirements-file, -r` (`$GO_GALAXY_REQUIREMENTS_FILE`, `$ANSIBLE_GALAXY_REQUIREMENTS_FILE`)
- `--ansible-config` (`$GO_GALAXY_ANSIBLE_CONFIG`, `$ANSIBLE_CONFIG`)
- `--workers` (`$GO_GALAXY_WORKERS`)
- `--no-cache` (`$GO_GALAXY_NO_CACHE`)
- `--refresh` (`$GO_GALAXY_REFRESH`)
- `--clear-cache` (`$GO_GALAXY_CLEAR_CACHE`)
- `--no-deps` (`$GO_GALAXY_NO_DEPS`)
- `--offline` (`$GO_GALAXY_OFFLINE`) - fail on any network access (cached state only)
- `--lock-file` (`$GO_GALAXY_LOCK_FILE`)
- `--frozen` (`$GO_GALAXY_FROZEN`) - require a lockfile and verify each installed or cached artifact's SHA256 against its lockfile pin, aborting the run on any mismatch; no effect on lock, which always regenerates the lockfile (the run warns)
- `--metrics-file` (`$GO_GALAXY_METRICS_FILE`) - emit JSON run report

S3 cache options (if `--s3-bucket` is set, S3 backend is used):

- `--s3-bucket` (`$GO_GALAXY_S3_BUCKET`)
- `--s3-region` (`$GO_GALAXY_S3_REGION`)
- `--s3-prefix` (`$GO_GALAXY_S3_PREFIX`)
- `--s3-access-key` (`$GO_GALAXY_S3_ACCESS_KEY`, `$AWS_ACCESS_KEY_ID`)
- `--s3-secret-key` (`$GO_GALAXY_S3_SECRET_KEY`, `$AWS_SECRET_ACCESS_KEY`)
- `--s3-endpoint` (`$GO_GALAXY_S3_ENDPOINT`)
- `--s3-session-token` (`$GO_GALAXY_S3_SESSION_TOKEN`, `$AWS_SESSION_TOKEN`)
- `--s3-path-style-disabled` (`$GO_GALAXY_S3_PATH_STYLE_DISABLED`)

### cleanup options

- `--verbose` - verbose output (`$GO_GALAXY_VERBOSE`)
- `--quiet, -q` - quiet mode (`$GO_GALAXY_QUIET`)
- `--dry-run` (`$GO_GALAXY_DRY_RUN`) - report the collections `cleanup` would remove and the
  extracted-store entries it would sweep, without deleting anything and without saving the
  snapshot; the summary line prints a candidate count instead of a removed count.
- `--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`)
- `--s3-bucket` (`$GO_GALAXY_S3_BUCKET`)
- `--s3-region` (`$GO_GALAXY_S3_REGION`)
- `--s3-prefix` (`$GO_GALAXY_S3_PREFIX`)
- `--s3-access-key` (`$GO_GALAXY_S3_ACCESS_KEY`, `$AWS_ACCESS_KEY_ID`)
- `--s3-secret-key` (`$GO_GALAXY_S3_SECRET_KEY`, `$AWS_SECRET_ACCESS_KEY`)
- `--s3-endpoint` (`$GO_GALAXY_S3_ENDPOINT`)
- `--s3-session-token` (`$GO_GALAXY_S3_SESSION_TOKEN`, `$AWS_SESSION_TOKEN`)
- `--s3-path-style-disabled` (`$GO_GALAXY_S3_PATH_STYLE_DISABLED`)

`cleanup` aborts with a non-zero exit and deletes nothing if a recorded project's `requirements.yml` is present but cannot be read or parsed.

`cleanup`'s extracted-cache sweep keeps a collection warmed within the last 30 days even if no project currently installs it, so a `warm`-only machine does not lose the extracted trees it exists to produce; a warmed entry that goes stale (no warm run for 30 days) is swept like any other unreferenced entry.

**Upgrade note:** this release bumps the cache snapshot schema, so the first `install`, `lock`, or `warm` run after upgrading rebuilds its metadata caches cold. If you run `cleanup` before that first run, it finds no persisted snapshot - the old one was dropped by the schema bump - so it skips the extracted-cache sweep entirely and leaves the snapshot untouched; the metadata caches still rebuild cold on the first `install`, `lock`, or `warm`.

## requirements.yml

```yaml
---
collections:
  - name: community.general
    version: "11.1.0"
  - name: ansible.posix
    version: "2.0.0"
    source: https://galaxy.ansible.com
```

## ansible.cfg

```ini
[defaults]
collections_path = ./collections

[galaxy]
server = https://galaxy.ansible.com
cache_dir = /home/ci/.cache/go-galaxy
```

## Galaxy servers and authentication

Beyond a single `[galaxy] server`, go-galaxy understands ansible's multi-server
configuration surface, so a fleet of CI jobs can share one `ansible.cfg` with a
private Automation Hub and the public Galaxy both configured.

### server_list and per-server sections

`[galaxy] server_list` (or `ANSIBLE_GALAXY_SERVER_LIST`, which wins outright
whenever it is set at all, even to an empty string) is a comma-separated list of
server ids. Each id gets its own `[galaxy_server.<id>]` section:

```ini
[galaxy]
server_list = automation_hub, release_galaxy

[galaxy_server.automation_hub]
url = https://hub.example.internal/api/galaxy

[galaxy_server.release_galaxy]
url = https://galaxy.ansible.com
```

```bash
export ANSIBLE_GALAXY_SERVER_AUTOMATION_HUB_TOKEN=xxxxxxxxxxxxxxxx
go-galaxy install
```

Every collection is resolved independently against `automation_hub` first, falling
back to `release_galaxy` only if the private hub doesn't have it - so one install
can legitimately draw some collections from the private hub and the rest from the
public Galaxy. go-galaxy also auto-discovers the API root under `url`, trying
`/api/v3` (galaxy.ansible.com's shape) and then the bare `/v3` a Galaxy NG /
Automation Hub deployment mounts directly under its own base path, so the same
`url` works for either shape without an extra option.

`[galaxy_server.<id>]` keys, and what go-galaxy does with them:

| Key                     | Support                                                                                                                   |
|:------------------------|:--------------------------------------------------------------------------------------------------------------------------|
| `url`                   | Supported, required.                                                                                                      |
| `token`                 | Supported.                                                                                                                |
| `validate_certs`        | Supported (see TLS below).                                                                                                |
| `api_version`           | Accepted only as `v3` (a no-op; this tool always speaks the v3 API); any other value is a config-load error.              |
| `username`, `password`  | Hard config-load error naming the key: this is ansible's Basic auth, which this tool does not implement.                  |
| `auth_url`, `client_id` | Hard config-load error naming the key: this is ansible's Keycloak/SSO token exchange, which this tool does not implement. |
| anything else           | Warned about and ignored.                                                                                                 |

Basic auth and Keycloak/SSO are refused outright rather than silently sending an
unauthenticated request and surfacing a confusing 401 later - the error names the
offending key so you know to configure a plain API token instead.

Every key also has a per-id environment override, using the id **exactly as
written** in `server_list` (no case- or dash-normalization):
`ANSIBLE_GALAXY_SERVER_<ID>_URL`, `_TOKEN`, `_VALIDATE_CERTS`. An id with no
`[galaxy_server.<id>]` section at all builds purely from its env overrides,
which is the common shape in containerized CI where you'd rather not template
an ansible.cfg for a secret.

### --token

`--token` / `GO_GALAXY_TOKEN` is go-galaxy's own convenience for the common
single-server case - point the tool at one hub and hand it a credential without
writing an `ansible.cfg` section for it. It only applies when exactly one server
is effective (the built-in default, `[galaxy] server`, or `--server` - including
`--server` naming a single `server_list` id); with a multi-entry `server_list`
in effect it is a hard error, since there is no way to tell which server the
credential belongs to - configure that server's own `[galaxy_server.<id>]`
token instead. When it does apply, it overrides that one server's own
configured token; setting it to the empty string clears the token entirely,
letting a pipeline force an anonymous run by exporting `GO_GALAXY_TOKEN=`
without editing any config.

### Precedence

Highest wins:

1. An explicit `--server` collapses everything to one server: if its value
   matches a configured `server_list` id exactly, that server's own token and
   `validate_certs` apply; otherwise the value is used verbatim as an anonymous
   URL, and `server_list` plays no further part - not even to validate it.
2. Otherwise a non-empty `server_list` wins, in list order.
3. Otherwise `[galaxy] server` from ansible.cfg.
4. Otherwise the `--server` flag's built-in default.

### Server selection at resolve time

Resolving a collection against `server_list` deviates from ansible in two
deliberate ways:

- **First match wins, not a union.** For an unpinned collection, go-galaxy walks
  the effective server list in order and installs from the first server that
  has it. Unlike ansible, which unions results across every configured server,
  go-galaxy never merges: a collection published on more than one server always
  comes from the earliest one that has it.
- **Fail closed, not fall through.** Only a 404 across all of one server's API
  root candidates means "this server doesn't have it, try the next one". A
  401/403 response, or a 5xx/network failure that survives the retry budget,
  aborts the whole run naming the server that failed and exits with the network
  exit code (`4`) - it never silently advances to the next server. A wrong
  token or a brief outage on your private hub must never quietly redirect an
  install to the public Galaxy instead.

A `requirements.yml` collection's `source:` pins it to one server for the whole
run: an exact `server_list` id match, or a URL matching a configured server's
network origin (so `source: https://hub.example.internal/content/published/`
still gets that server's own token and TLS policy, even though the path differs
from the configured `url`).

### TLS: validate_certs

`validate_certs = false` really disables certificate verification for that
server - but only for that one server's own network origin, never globally and
never for a download host on a different origin. The run warns loudly about
it, even in quiet mode (twice, if that server also carries a token, since the
token would then cross a connection this run cannot authenticate). Prefer
trusting a self-signed hub's CA instead of disabling verification: point
`SSL_CERT_FILE` or `SSL_CERT_DIR` at it and leave `validate_certs` unset.

### Rejected as configuration errors

These are refused before any request is made, exiting with the usage exit code
(`2`) - the operator has to fix `ansible.cfg`, an environment variable, or
`requirements.yml`, not retry:

- A server URL, or a `requirements.yml` `source:`, with embedded userinfo
  (`https://user:pass@hub/`).
- A token configured for a plaintext (`http://`) origin that isn't loopback.
- Two configured servers that share a network origin but disagree on their
  token or their `validate_certs`.

By contrast, an auth failure (401/403) or an unavailable server exits with the
network exit code (`4`) instead, since that's a runtime condition to retry or
investigate, not a configuration mistake.

## Notes

- Non-Galaxy sources (git/url/file/dir) are not supported.
- `roles` in requirements.yml are ignored.

## S3 Cache (optional)

When `--s3-bucket` (or `GO_GALAXY_S3_BUCKET`) is set, go-galaxy uses S3 as the cache backend.
Artifacts and cache metadata are stored in S3; collections are still installed locally.

## Security / Trust model

- The shared S3 snapshot object and the project registry object are a trust boundary:
  go-galaxy serves cached Galaxy metadata (including a collection's download URL and
  sha256) from them without re-validating against the origin on every use, so anyone
  who can write to the bucket can influence what a run installs. Restrict bucket write
  access (a write-restricted ACL, or dedicated credentials) just as you would protect
  a local cache directory - the local Bolt snapshot is implicitly trusted for the same
  reason, since writing it already requires local filesystem access to the cache
  directory.
- Snapshot and project-registry reads are size-capped (a compressed-size ceiling and a
  decompressed-size ceiling), so a hostile-but-writable bucket cannot OOM the process
  with an oversized object or a gzip bomb.
- An artifact download whose host differs from the configured Galaxy server is warned
  about (a visible signal in CI logs) rather than blocked, so deployments that serve
  downloads from a separate content host or object storage still work.
- Pinned (`--frozen`) installs are already immune to a poisoned snapshot: for a
  lockfile-pinned collection, go-galaxy hashes the actually downloaded (or on-disk)
  bytes and compares them to the sha256 recorded in the in-repo lockfile, not to the
  cacheable metadata sha, so a poisoned download URL or sha causes the install to fail
  closed instead of installing attacker content. Use `--frozen` with a committed
  lockfile in CI as the robust mitigation against a compromised cache.
- **A cache must not be shared between principals holding different Galaxy
  credentials.** A local cache directory or an S3 bucket is not scoped to a
  credential: cached API responses, resolved versions, dependency graphs, and
  artifacts are keyed by server, not by which token fetched them, so an entry a
  privileged run fetched from a private server is served as-is to a later run
  against the same server with no token at all. Keying by a token fingerprint
  instead was considered and rejected: it would put a credential-linked
  identifier into a shared, less-trusted store, which is exactly the trust
  boundary above this design keeps clean. Give each distinct credential its own
  cache directory or S3 prefix/bucket.
- **No token ever reaches persisted state.** Neither a token nor any value
  derived from one - not even a hash - is ever written to the local snapshot,
  the S3 snapshot, the project registry, the lockfile, the metrics file, or
  GALAXY.yml. A configured token is rendered as a fixed redacted placeholder by
  every serialization path (`fmt`, JSON, YAML), and its plaintext is reachable
  through exactly one call site in the whole program, immediately before it is
  attached to an outgoing request.

## Reproducible CI

Pin transitive collections with a lockfile, then drive CI from it:

```bash
# once, when you change requirements.yml:
go-galaxy lock                       # writes requirements.lock.yml

# in CI:
go-galaxy install --frozen --offline # use lockfile, no network calls
```

A frozen install fails loudly if a cached or downloaded artifact does not match the
lockfile's recorded SHA256, so a poisoned cache or a mutated upstream artifact cannot
install silently; lockfiles with no recorded SHA (older lockfiles) are not pin-checked.

`go-galaxy hash` prints a deterministic `sha256:…` of the lockfile (or `requirements.yml`
when no lockfile is present) - perfect as a CI cache key.

### GitHub Actions

```yaml
name: ansible-collections
on: [push, pull_request]

jobs:
  install:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Install go-galaxy
        run: |
          curl -sSL https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64 \
            -o /usr/local/bin/go-galaxy
          chmod +x /usr/local/bin/go-galaxy

      - name: Compute cache key
        id: gg
        run: echo "key=$(go-galaxy hash)" >> "$GITHUB_OUTPUT"

      - name: Restore go-galaxy cache
        uses: actions/cache@v4
        with:
          path: ~/.cache/go-galaxy
          key: go-galaxy-${{ runner.os }}-${{ steps.gg.outputs.key }}
          restore-keys: |
            go-galaxy-${{ runner.os }}-

      - name: Install collections (frozen + offline)
        run: go-galaxy install --frozen --offline -p ./collections
```

### GitLab CI

```yaml
stages: [install]

variables:
  GO_GALAXY_CACHE_DIR: "$CI_PROJECT_DIR/.cache/go-galaxy"

install_collections:
  stage: install
  image: ghcr.io/greeddj/go-galaxy:latest
  before_script:
    - export CACHE_KEY="$(go-galaxy hash)"
    - echo "cache key = $CACHE_KEY"
  cache:
    key: "go-galaxy-$CACHE_KEY"
    paths:
      - .cache/go-galaxy
  script:
    - go-galaxy install --frozen --offline -p ./collections
```

### Container image bake

Pre-warm caches in your CI base image so jobs only hardlink into place:

```dockerfile
FROM debian:stable-slim
COPY --from=ghcr.io/greeddj/go-galaxy:latest /usr/local/bin/go-galaxy /usr/local/bin/
COPY requirements.yml requirements.lock.yml ./
RUN go-galaxy warm --frozen
```

## Exit codes

`go-galaxy` exits with a class-specific code instead of a flat `1`, so CI
pipelines can branch on failure type without parsing log output:

| Code | Meaning                                                                                                                                              |
|-----:|------------------------------------------------------------------------------------------------------------------------------------------------------|
|    0 | Success                                                                                                                                              |
|    1 | Generic failure (does not match any class below)                                                                                                     |
|    2 | Usage or configuration error (invalid flags, requirements, `ansible.cfg`, or a flag a command does not implement, e.g. `--dry-run` on `lock`)        |
|    3 | Dependency resolution failure (conflicts, missing candidates, cycle)                                                                                 |
|    4 | Network or Galaxy API failure (timeouts, stalled transfers, metadata and cache-state deadlines, offline-mode violations)                             |
|    5 | Install-time failure (unsafe archive/symlink content, empty file, missing artifact cache)                                                            |
|    6 | Lockfile error (missing, invalid, or mismatched with requirements)                                                                                   |
|    7 | Artifact-integrity failure (content does not authenticate against its naming sha256, or the digest is malformed)                                     |
|  130 | Interrupted (a caught SIGINT, or the caller's own context canceled)                                                                                  |

Exit `7` covers content that failed to authenticate against the sha256 that
named it - a lockfile pin, a Galaxy server's declared digest, a cache sidecar,
or the extracted store's content-address key - or a digest that was
structurally malformed. It is a stop-and-alert class: do not retry it
automatically. A retry cannot repair it, because the bytes or the digest are
wrong at the source, not transiently unavailable. It also outranks the network
class: a run that hits both an integrity failure and a network failure exits
`7`, not `4`.

A stalled or byte-dripped transfer is never reported as an interrupt, even
though the underlying mechanism that unblocks it is a context cancellation:
the tool distinguishes its own no-progress cancellation from a genuine SIGINT
or caller cancellation, and only the latter exits `130`. The same holds for
the metadata and cache-state ceilings above: grep the run's output for
`galaxy metadata fetch deadline exceeded` or `cache state object deadline
exceeded` to tell one of these deadlines apart from a genuine interrupt or
from any other network failure sharing exit code `4`.

## Metrics

Pass `--metrics-file path/to/run.json` to install/warm/lock to emit a JSON report
suitable for CI dashboards:

```json
{
  "started_at":       "2026-04-28T10:00:00Z",
  "finished_at":      "2026-04-28T10:00:08Z",
  "command":          "install",
  "server":           "https://galaxy.ansible.com",
  "lockfile":         "requirements.lock.yml",
  "lockfile_hash":    "<sha256-hex>",
  "duration_ns":      8123456789,
  "cache_hits":       12,
  "cache_misses":     5,
  "bytes_downloaded": 4831201,
  "collections":      17,
  "failures":         0,
  "frozen":           true
}
```

The report is written atomically (temp file plus rename), so a consumer never
reads a partial JSON, and a symlink at the operator-specified path is replaced
rather than followed.

The report is written whenever a run reaches its finalize step - including a
run that failed to install some collections and a run whose snapshot save
itself failed - and is not written when the run aborts earlier (unreadable
`requirements.yml`, a resolution failure, or a missing lockfile). Its
existence is therefore not a success signal: gate automation on the process
exit code (see the table above), never on whether the metrics file exists or
looks clean. This matters most for `lock`: `failures` is always `0` in a
`lock` report, so the report carries no failure signal at all for that
command, and a `lock` run whose snapshot save failed still leaves a
clean-looking report next to a nonzero exit code. `frozen` is also never set
in a `lock` report, even when `--frozen` was passed: `lock` accepts the flag
because it shares the same flag set as `install`/`warm`, but it never
consumes the lockfile it writes, so the report never claims a frozen run.

`cache_hits`, `cache_misses`, and `bytes_downloaded` are artifact-level counters,
not collection-level: a hit is one artifact served from the artifact cache and a
miss is one artifact fetched from the origin, so `cache_hits + cache_misses`
counts artifact acquisitions rather than collections. That sum can exceed
`collections` - the bounded evict-and-refetch recovery path makes one collection
contribute both a hit (the cache-resident artifact that turned out corrupt) and
a miss (the refetch that replaced it) - and it can also fall below `collections`,
since a collection whose install is skipped touches no artifact at all. A cache
hit always contributes zero bytes to `bytes_downloaded`, including an S3 cache
hit: that object transfer is a real network round trip to the cache backend,
but it is not artifact-download traffic, so it is deliberately excluded. The
`lock` command never fetches an artifact, so its report always has
`cache_hits`, `cache_misses`, and `bytes_downloaded` at `0`.
