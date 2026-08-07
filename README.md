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
downloads and cache presence probes across `--download-workers` (network-bound,
sized well above core count by default) and extractions across `--workers`
cores (CPU-bound), uses hard links from a content-addressable cache on warm
runs, and skips the network entirely under `--frozen --offline`. With a
lockfile and warm caches, installing 100 collections takes ~12 s instead of
~5 minutes.

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

## Compatibility with ansible-galaxy

Drop-in means the same `requirements.yml`, the same `ansible.cfg` keys and the
same `ANSIBLE_*` environment variables, for the collections subset above. It
does not mean identical behavior everywhere: three things differ on purpose,
and each is called out below rather than left to be discovered in CI.

### Configuration go-galaxy reads

`ansible.cfg` is discovered in ansible's own order - `$ANSIBLE_CONFIG`,
`./ansible.cfg`, `~/.ansible.cfg`, `/etc/ansible/ansible.cfg` - and parsed as
INI the way ansible parses it (CPython's `configparser`), not as TOML. Two
consequences follow from matching ansible rather than a stricter parser: a
quoted value keeps its quotes, so `collections_path = "./c"` sets the literal
`"./c"` and you should drop the quotes; and an inline comment is part of the
value, so `server = https://galaxy.ansible.com # note` is a bad URL rather
than a URL with a note.

Discovery keeps one of ansible's exceptions too: `./ansible.cfg` is not
considered at all when the current directory is world-writable, since any
other user on the machine could put a file there, and the run says so on
stderr rather than skipping it silently. The remaining candidates are still
tried. A container CI job whose workspace is `0777` therefore stops picking up
a workspace `ansible.cfg`; pass `--ansible-config` (or `$ANSIBLE_CONFIG`) to
name it explicitly, or tighten the directory's mode.

| Setting                            | Environment                                                   |
|:-----------------------------------|:--------------------------------------------------------------|
| `[defaults] collections_path`      | `ANSIBLE_COLLECTIONS_PATH`                                    |
| `[galaxy] server`                  | `ANSIBLE_GALAXY_SERVER`                                       |
| `[galaxy] server_list`             | `ANSIBLE_GALAXY_SERVER_LIST`                                  |
| `[galaxy] cache_dir`               | `ANSIBLE_GALAXY_CACHE_DIR`                                    |
| `[galaxy_server.<id>]`             | `ANSIBLE_GALAXY_SERVER_<ID>_URL`, `_TOKEN`, `_VALIDATE_CERTS` |
| (the config file itself)           | `ANSIBLE_CONFIG`                                              |
| (request timeout)                  | `ANSIBLE_GALAXY_SERVER_TIMEOUT`                               |

One variable go-galaxy reads is deliberately absent from that table.
`ANSIBLE_GALAXY_REQUIREMENTS_FILE` sits in ansible's namespace without being an
ansible option: ansible-core declares no requirements-file setting, and
`ansible-galaxy` takes that path only as `-r/--role-file`. It is read anyway,
and it is not going away, because pipelines already set it; it is documented
here rather than in the table so that nobody expects `ansible-galaxy` to
honour it.
`GO_GALAXY_TOKEN` is the other name with no ansible counterpart, for the
separate reason described under
[Galaxy servers and authentication](#galaxy-servers-and-authentication).

Anything else in `ansible.cfg` is ignored. Within `[galaxy_server.<id>]` the
exceptions are deliberate and loud: `username`/`password` (Basic auth) and
`auth_url`/`client_id` (Keycloak/SSO) are refused as config errors naming the
key rather than ignored, because silently dropping a credential would send an
unauthenticated request to a private hub. See [Galaxy servers and
authentication](#galaxy-servers-and-authentication) for the full table, token
precedence, and TLS.

### Deliberate differences

- **A collection comes from one server, not from a union.** ansible queries
  every configured server and merges the results; go-galaxy walks
  `server_list` in order and the first server that has the collection owns it
  for the whole run. Merging means the same `namespace.name@version` can
  arrive from two servers with different bytes, with an arbitrary tie-break
  deciding which one you install.
- **A failing server stops the run instead of being skipped.** Only a 404
  means "this server does not have it, try the next". A 401/403, or a
  5xx/network failure that survives the retry budget, aborts and names the
  server. ansible swallows those and moves on, which turns a wrong token or a
  five-minute hub outage into an install from the public Galaxy - dependency
  confusion by accident.
- **`--timeout` is a no-progress budget, not a total-transfer cap.** It bounds
  the wait for response headers and the gap between two body reads, so a large
  download that keeps streaming is never cut off by it, however long it takes.
  ansible's own `--timeout` behaves the same way; what changed here is that
  go-galaxy used to apply it to the whole response as well. The gap it leaves -
  a server dribbling a few bytes into every idle window makes progress on every
  read and so never trips it - is closed by separate fixed ceilings on the whole
  acquisition, described under [install options](#install-options). A transfer
  that does stall reports `network read stalled` and exits `4`, or `5` when it
  fails one collection of an install; it is never reported as an interrupt.

Resolution itself is stricter than ansible's: a constraint set with no solution
is a failure with a proof, not a lenient pick. See [Exit codes](#exit-codes) for
what each failure class exits with.

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

### Binary

```bash
curl -sSLf -o /usr/local/bin/go-galaxy \
  https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64
chmod +x /usr/local/bin/go-galaxy
```

Substitute `linux-arm64`, `darwin-amd64` or `darwin-arm64` for another
platform. Each release also carries a `.tar.gz` per platform with the same
binary plus LICENSE and README. See [Verifying a
release](#verifying-a-release) before trusting either.

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

## Verifying a release

Every release is signed, catalogued and attested by the workflow that built
it. There is no public key to fetch and no key for anyone to lose: cosign
signs keylessly, so the identity in the certificate *is* the release workflow,
proved by a short-lived OIDC token from GitHub.

`checksums.txt` lists every asset by sha256, and it is what gets signed - so
verifying one signature and one hash covers whichever asset you actually
downloaded:

```bash
tag=v1.2.3
base="https://github.com/greeddj/go-galaxy/releases/download/$tag"
curl -sSLfO "$base/checksums.txt" -O "$base/checksums.txt.sigstore.json"

cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp \
    '^https://github\.com/greeddj/go-galaxy/\.github/workflows/release\.yml@refs/tags/'

sha256sum --ignore-missing -c checksums.txt   # or: shasum -a 256 --ignore-missing -c
```

Container images are signed the same way, with the signature stored in the
registry next to the image, so nothing needs downloading first:

```bash
cosign verify ghcr.io/greeddj/go-galaxy:latest \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp \
    '^https://github\.com/greeddj/go-galaxy/\.github/workflows/release\.yml@refs/tags/'
```

A signature says the file is the one that was published. Provenance says which
workflow, at which commit, produced it - a different question, recorded as a
GitHub build attestation for every asset:

```bash
gh attestation verify go-galaxy-linux-amd64 --repo greeddj/go-galaxy
```

Each asset also ships an SPDX SBOM next to it (`<asset>.sbom.json`), listing
the Go modules actually linked into that build, for scanning against a
vulnerability feed without unpacking anything.

macOS builds are **not** Apple-notarized, so Gatekeeper has nothing to check
them against - the signature and provenance above are what to verify instead.
A binary downloaded by a browser also arrives quarantined; the release ships
no packaging that clears that flag on your behalf.

## Usage

```bash
./dist/go-galaxy i -r requirements.yml -p ./collections
```

Clean unreachable collections:

```bash
./dist/go-galaxy c
```

### Commands

Running `go-galaxy` with no command runs `install`, so a bare invocation
performs a full install rather than printing help.

- `install` (`i`) - install collections from `requirements.yml`.
- `lock` (`l`) - resolve and write `requirements.lock.yml` for reproducible CI. Under `--frozen`, `lock` becomes a drift gate instead of a writer: it still resolves fresh (`lock` always does), but compares that fresh resolve against the lockfile already on disk and fails the run instead of overwriting the file when they differ, exiting with the lockfile exit code (`6`). The gate mirrors `lock`'s own resolution per the exact flags in effect - what it compares against is whatever `lock --<those flags>` would write - so `lock --frozen` alone reuses a cached resolve when `requirements.yml` is unchanged, and a version merely published upstream is not drift by itself: it gates the requirements-to-lockfile relationship, not upstream publication. Add `--refresh` (`lock --frozen --refresh`) to gate upstream publication too: `--refresh` makes the fresh resolve reach the live servers instead of reusing the cached one, so a newer version published upstream with `requirements.yml` unchanged now shows up as drift. A missing lockfile and one that exists but cannot be loaded each fail with their own distinct error rather than being reported as drift. Under `--dry-run`, `lock` diffs a fresh resolve against whatever lockfile is already on disk and reports what would change, without writing a lockfile; `--frozen` and `--dry-run` compose (both suppress the write, `--frozen` supplies the stricter verdict) - see [install options](#install-options) for the full `--dry-run` semantics.
- `warm` (`w`) - populate the artifact + extracted caches without installing (for CI image bake). Requires a cache: `--no-cache` is rejected as a usage error rather than downloading everything and discarding it. A warmed collection's extracted tree is protected from `cleanup` for 30 days after its last warm, so a machine that warms and then stops warming eventually reclaims the space. Under `--dry-run`, `warm` reports per collection whether it is already warm or would be warmed, downloads no artifact, and writes no warmed entry; it still rejects `--no-cache` as a usage error regardless of `--dry-run`, since `--no-cache` leaves warm nothing to do either way - see [install options](#install-options) for the full `--dry-run` semantics.
- `hash` (`h`) - print a deterministic cache key (`sha256:…`) for use as a CI cache key.
- `tree` (`t`) - print the resolved dependency tree from the lockfile; requires a lockfile and fails if one is absent.
- `explain` (`why`) - takes `<namespace.name>`; prints the locked version, source, and sha256, what requires it, and what it depends on, all read from the lockfile.
- `outdated` (`o`) - compare each lockfile entry against the latest version on its Galaxy server; requires network and is refused under `--offline`. It deliberately opens no cache backend, so it never takes the exclusive cache lock (it can run alongside an `install` or `warm` against the same cache) and every version it reports is a live answer rather than a cached one. That is also why `--no-cache`, `--refresh`, `--clear-cache`, `--cache-dir` and `--s3-bucket` have nothing to act on; `--no-deps` and `--download-path` likewise, since it resolves no dependency graph and writes nothing to the collections tree, and `--frozen` likewise, since the lockfile is already the only source of the locked side and the servers are always asked for the latest. Setting any of those (except the two path flags, which cannot be told apart from their defaults) prints one stderr warning naming them; the other `--s3-*` flags are not individually named, since none of them do anything for any command unless `--s3-bucket` is also set. It honors `--metrics-file`: the report's `collections` is the number of entries checked and `failures` is the number of lookups that failed, while `frozen` is always absent, since no `outdated` run ever honors that flag. A run in which any lookup failed exits with the network code (`4`). One failure shape is classified more specifically, and now at load rather than at lookup: a lockfile entry's `name` must be `<namespace>.<name>` with each half matching `^[a-z][a-z0-9_]*$` - the alphabet galaxy.ansible.com and Automation Hub themselves accept - and a lockfile carrying anything else is refused as invalid with the lockfile code (`6`) before a single request is made. Previously only the two-part split was checked, so a name carrying characters a URL cannot contain loaded fine and failed later while the request was being built, reported as a network failure (`4`) that no retry could repair.
- `cleanup` (`c`) - remove unused cached collections across projects.

### Global options

- `--help, -h`
- `--version, -v`

### install options

- `--verbose` - verbose output (`$GO_GALAXY_VERBOSE`)
- `--quiet, -q` (`$GO_GALAXY_QUIET`) - suppress progress and log lines; results, warnings and
  errors still print. Ignored when `--verbose` is also set.
- `--dry-run` (`$GO_GALAXY_DRY_RUN`) - report what `install`, `warm`, or `lock` would do, without
  downloading any artifact, creating any install tree or extracted tree, recording any install,
  writing any warmed entry, writing any lockfile, registering the project, honoring
  `--clear-cache`, or writing the metrics report. It still takes the exclusive cache lock. It
  updates the resolve-side metadata caches, but only when a persisted snapshot already existed for
  this cache; against a cache that was never saved before, the run saves nothing and prints a
  stderr warning that the caches it built are discarded - a preview must never leave behind a
  persisted-and-empty snapshot that a later `cleanup` would read as evidence that nothing is
  installed or warmed anywhere.
  For `install` and `warm`, each collection is reported as would install/would warm, already up to
  date/already warm, or would fail. A would-fail verdict covers, for both commands, the artifact
  not being cached while `--offline` forbids downloading it (exits with the install-failure code,
  `5`), or the cached artifact's own recorded digest disagreeing with a well-formed lockfile pin
  while `--offline` forbids refetching a replacement (exits with the dedicated integrity code,
  `7`); `install` alone adds a third cause, since only it writes an install tree: the collection's
  install directory sits under a namespace path a real install would refuse to write to - an
  escaping symlink, or a regular file blocking it - and extraction would fail the identical way
  (exits with `5`). A dangling namespace symlink is deliberately not a would-fail: a real install
  removes it and creates the directory fresh, so the preview reports the collection normally. Each
  cause exits with the code a real run hitting that same cause would. One asymmetry is deliberate,
  and it depends on the cache backend: the recorded-digest cause can fire where the real run still
  succeeds, because under a pin a real install re-hashes the tarball rather than trusting the
  recorded digest - so on the local cache backend, a cache whose recorded digest was altered while
  its bytes were left intact is refused by the preview and installed for real anyway. On the S3
  backend that same cache fails the real run too, because it re-checks the recorded digest against
  the freshly downloaded bytes before the pin is ever re-hashed - so there the preview's refusal
  matches what actually happens. The preview refuses it either way, because that cache is damaged
  either way. The dry-run banner is printed to stderr and survives `--quiet`, because the flag is
  env-sourced (`$GO_GALAXY_DRY_RUN`) and an org-wide CI environment block would otherwise turn
  every install, warm, or lock run into a silent no-op.
  Before any collection is even resolved, `install --dry-run` also checks whether
  `ansible_collections` itself is usable: a real directory, an in-root relative symlink to one, or
  an absent entry are all fine; an escaping or dangling symlink is refused with the install-failure
  code (`5`), and a regular file sitting there is refused unclassified (exit `1`) - matching a
  real, non-dry-run install exit-for-exit on every one of those shapes, and aborting the whole
  preview before resolution ever starts rather than reporting on any collection at all.
  For `install` and `warm`, a dry run still reports a cached artifact's presence, not its actual
  on-disk bytes: it does compare the artifact cache's own recorded digest against the lockfile pin
  (the integrity would-fail cause above), but a tarball whose bytes silently drift while that
  recorded digest is never updated cannot be detected without re-hashing it - a full object
  download on the S3 backend, the exact cost this preview exists to avoid. Under `--frozen
  --offline`, a collection in exactly that state is still reported cached - `Already warm`,
  `Would warm (artifact cached)`, or `Would install (artifact cached)` - even though the real run
  would fail closed with a checksum-mismatch error. `Up to date` is not affected, because a real
  install skips such a collection without ever opening its tarball. The run prints a one-time
  stderr warning whenever both flags are set together, naming this narrower residual; it is a
  disclosure, not a fix.
  For `lock`, a dry run builds the lockfile in memory from a fresh resolve, loads whatever
  lockfile is already on disk, and reports how the two differ instead of writing anything. A
  `Would change: server <from> -> <to>` line prints first when the file-level `server` field
  itself would change, followed by one line per added, updated, or removed collection - `Would
  add: <name>@<version>`, `Would update: <name> (<field> <from> -> <to>; ...)`, `Would remove:
  <name>@<version>` - and a trailing summary whose verdict is `lockfile would change` unless
  nothing at all would change, in which case it reads `lockfile is up to date`; a change to only
  the file-level `server` field flips this verdict even though every per-collection count stays
  zero, since that alone would still rewrite the file on a real run. A lockfile already on disk
  that cannot be loaded (a bad schema version, unparseable YAML, or any other read failure) is
  warned about on stderr and then treated the same as no lockfile at all - every collection
  reports as added - because a real `lock` run never reads that file, it only overwrites it, so
  the preview cannot fail on it either.
  **Breaking change:** before this release, `--dry-run` (and `$GO_GALAXY_DRY_RUN`) had no effect
  on `install` or `warm` - only `cleanup` implemented it - so `install --dry-run` performed a
  full, real install and `warm --dry-run` performed a full, real warm; `lock --dry-run` refused to
  run at all, exiting with the usage code (`2`). `install` and `warm` now install and warm
  nothing, and preview instead; `lock` now previews instead of refusing. A CI job that carried an
  ambient `$GO_GALAXY_DRY_RUN` and was really installing collections will now finish successfully
  with nothing installed; a bake job carrying the same ambient variable will now finish
  successfully with an empty cache, producing a green build and an empty image; a lock job
  carrying it will now finish successfully having left the lockfile untouched instead of exiting
  with a usage error. In every case the stderr banner above is the only signal, so a job that
  branches on the exit code alone will not notice. If a shared `$GO_GALAXY_DRY_RUN` CI environment
  variable is set, scope it to the jobs that actually want it, or unset it for `install`, `warm`,
  and `lock` jobs where it must not silently do nothing. `cleanup` implements its own `--dry-run`
  (see [cleanup options](#cleanup-options)). `hash`, `tree` and `explain` ignore the flag, since
  they have no product and write nothing for it to suppress. `outdated` writes no product either,
  but it does write one externally consumed report - the metrics file - so `--dry-run` suppresses
  that report and prints a stderr warning naming the path, and changes nothing else about the run.
- `--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`)
- `--server` (`$GO_GALAXY_SERVER`) - **Breaking change:** `$ANSIBLE_GALAXY_SERVER`
  is no longer read as a spelling of this flag. It now behaves as `[galaxy] server`
  does, which is what ansible itself does with it: a fallback consulted only when
  no `server_list` and no `--server` apply, rather than an override that collapses
  a configured `server_list` to one anonymous server. A pipeline that exported it
  to force a single server now gets `server_list` instead, and should set
  `$GO_GALAXY_SERVER` (or pass `--server`) to keep the old behavior.
- `--token` (`$GO_GALAXY_TOKEN`) - Galaxy API token for the single effective server;
  an error if a multi-entry `server_list` is configured, and setting it to the
  empty string clears a previously configured token (see
  [Galaxy servers and authentication](#galaxy-servers-and-authentication))
- `--timeout` (`$GO_GALAXY_SERVER_TIMEOUT`, `$GO_GALAXY_TIMEOUT`, `$ANSIBLE_GALAXY_SERVER_TIMEOUT`)
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
- `--download-path, -p` (`$GO_GALAXY_COLLECTIONS_PATH`, `$GO_GALAXY_DOWNLOAD_PATH`, `$ANSIBLE_COLLECTIONS_PATH`)
- `--requirements-file, -r` (`$GO_GALAXY_REQUIREMENTS_FILE`, `$ANSIBLE_GALAXY_REQUIREMENTS_FILE` -
  a go-galaxy extension, not an ansible option)
- `--ansible-config` (`$GO_GALAXY_ANSIBLE_CONFIG`, `$ANSIBLE_CONFIG`)
- `--workers` (`$GO_GALAXY_WORKERS`) - number of concurrent workers; unset means one per CPU. A
  non-positive value is a usage error and exits `2`.
- `--download-workers` (`$GO_GALAXY_DOWNLOAD_WORKERS`) - number of concurrent artifact downloads
  and cache presence probes, separate from `--workers`: `--workers` bounds extraction, which is
  CPU-bound (an install or warm worker unpacks a tree in the same goroutine that downloaded it),
  while `--download-workers` bounds downloads and cache probes, which are network-bound (a HEAD
  probe or a streamed GET into a temp file, never an extraction). Unset, it derives from the core
  count (4× per core) with a floor of 8 and a ceiling of 32, so a low-core CI runner still gets
  meaningful download concurrency and a high-core one does not oversubscribe the HTTP connection
  pool. A non-positive value falls back to that same default rather than erroring.
  **Breaking change:** before this release, the concurrency of artifact downloads and cache
  presence probes followed `--workers` (one per CPU by default), so a 4-core CI runner issued at
  most 4 concurrent requests to the configured Galaxy server. It now follows `--download-workers`'s
  own, larger default instead, so that same 4-core runner issues up to 16 concurrent requests. This
  matters to operators of rate-limited Automation Hub instances, or any Galaxy server enforcing a
  per-client request limit: set `--download-workers` (or `$GO_GALAXY_DOWNLOAD_WORKERS`) explicitly
  to keep the old, CPU-derived figure, or lower, if the new default triggers throttling.
- `--no-cache` (`$GO_GALAXY_NO_CACHE`)
- `--refresh` (`$GO_GALAXY_REFRESH`) - re-resolve against the configured Galaxy servers instead of reusing
  cached metadata or the previous resolution. It bypasses exactly the cached answers that name a collection
  without naming a version - which versions exist, which is highest, and which versions the last run picked
  for these requirements - never an answer that already names an exact version: that version's metadata,
  its dependency map, its artifact bytes, and its extracted tree are all still served from cache, so a
  refreshed re-resolve that lands on the same version downloads nothing new. `--offline` outranks
  `--refresh` - cached state is the only source of truth offline, so there is nothing left to re-resolve
  against - and the run warns once rather than silently dropping the flag. `--refresh` has no effect under
  `--frozen` on `install`/`warm`, since neither ever resolves against the network in the first place; on
  `lock --frozen` it does have an effect - see the `lock` command entry above.
- `--clear-cache` (`$GO_GALAXY_CLEAR_CACHE`)
- `--no-deps` (`$GO_GALAXY_NO_DEPS`)
- `--offline` (`$GO_GALAXY_OFFLINE`) - fail on any network access (cached state only)
- `--lock-file` (`$GO_GALAXY_LOCK_FILE`)
- `--frozen` (`$GO_GALAXY_FROZEN`) - the lockfile is law, enforced differently by each command it applies to. `install`/`warm` resolve FROM the lockfile instead of the network: no version listing, no metadata fetch, just the pinned entries, with each installed or cached artifact's SHA256 verified against its lockfile pin and the run aborted on any mismatch. `lock` cannot resolve from a file it is about to write, so it instead resolves fresh - exactly as an unfrozen `lock` run does, including reusing a cached resolve - and COMPARES the result against the lockfile already on disk, failing the run rather than overwriting the file on any difference; `lock --frozen` is therefore not a network-free path the way install/warm `--frozen` is. See the `lock` command entry above for the comparison's exact contract.
- `--metrics-file` (`$GO_GALAXY_METRICS_FILE`) - emit JSON run report

S3 cache options (if `--s3-bucket` is set, S3 backend is used):

- `--s3-bucket` (`$GO_GALAXY_S3_BUCKET`)
- `--s3-region` (`$GO_GALAXY_S3_REGION`)
- `--s3-prefix` (`$GO_GALAXY_S3_PREFIX`)
- `--s3-access-key` (`$GO_GALAXY_S3_ACCESS_KEY`, `$AWS_ACCESS_KEY_ID`)
- `--s3-secret-key` (`$GO_GALAXY_S3_SECRET_KEY`, `$AWS_SECRET_ACCESS_KEY`)
- `--s3-endpoint` (`$GO_GALAXY_S3_ENDPOINT`)
- `--s3-session-token` (`$GO_GALAXY_S3_SESSION_TOKEN`, `$AWS_SESSION_TOKEN`)
- `--s3-path-style-disabled` (`$GO_GALAXY_S3_PATH_STYLE_DISABLED`) - switch to virtual-hosted-style
  addressing (`<bucket>.<endpoint>/<key>`). Path style (`<endpoint>/<bucket>/<key>`) is the default,
  which is what the flag disables.

### cleanup options

- `--verbose` - verbose output (`$GO_GALAXY_VERBOSE`)
- `--quiet, -q` (`$GO_GALAXY_QUIET`) - suppress progress and log lines; results, warnings and
  errors still print. Ignored when `--verbose` is also set.
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
- `--s3-path-style-disabled` (`$GO_GALAXY_S3_PATH_STYLE_DISABLED`) - switch to virtual-hosted-style
  addressing (`<bucket>.<endpoint>/<key>`). Path style (`<endpoint>/<bucket>/<key>`) is the default,
  which is what the flag disables.

`cleanup` aborts with a non-zero exit and deletes nothing if a recorded project's `requirements.yml` fails to load for any reason other than the file no longer existing at all. A recorded requirements file that no longer exists at all is treated differently: it is a tolerated stale registry entry, reported with a single warning naming the project and contributing no reachability roots this run, rather than a load failure. A project whose `ansible_collections` entry does not resolve to a real directory inside its collections path - most commonly because that entry itself is a symlink escaping that path - is skipped for scanning instead, with its own warning naming the project: nothing under it is scanned or removed, and every other project's cleanup still proceeds unless some recorded project's `requirements.yml` fails to load for any reason other than the file no longer existing at all, which aborts the whole run for every project at once. A skipped project's `requirements.yml` is still resolved against every other recorded project's installed collections, though, so its roots can keep another project's on-disk copy alive even though nothing under the skipped project itself was scanned or removed this run; `--dry-run` still never previews a removal for the skipped project's own collections, since a real run could not perform one there either. Within a project that does get scanned, an individual collection whose `MANIFEST.json` is not a regular file - a symlink, a directory, or anything else in its place - is skipped with its own warning naming the path, while the rest of that project's collections are still scanned and cleaned up normally.

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

Prefer the environment variable over the flag. A token passed as `--token`
lands in this process's argv, where any local process can read it - on Linux
through `/proc/<pid>/cmdline`, and in a `ps` listing on most systems - for as
long as the run lasts. `GO_GALAXY_TOKEN` carries the same value without that
exposure. go-galaxy does not detect which route you used and will not warn:
this is guidance about how you invoke the tool, not a check it performs.

### Precedence

Highest wins:

1. An explicit `--server` (or `$GO_GALAXY_SERVER`) collapses everything to one
   server: if its value matches a configured `server_list` id exactly, that
   server's own token and `validate_certs` apply; otherwise the value is used
   verbatim as an anonymous URL, and `server_list` plays no further part - not
   even to validate it.
2. Otherwise a non-empty `server_list` wins, in list order.
3. Otherwise `[galaxy] server` from ansible.cfg, or `$ANSIBLE_GALAXY_SERVER`,
   which outranks that key whenever it is set but nothing above it.
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

Pass the two credential values through the environment rather than the command line.
`--s3-secret-key` and `--s3-session-token` land in this process's argv, where any local
process can read them for as long as the run lasts; `GO_GALAXY_S3_SECRET_KEY` /
`AWS_SECRET_ACCESS_KEY` and `GO_GALAXY_S3_SESSION_TOKEN` / `AWS_SESSION_TOKEN` carry the
same values without that exposure. This is guidance about how you invoke the tool, not a
check it performs - go-galaxy does not detect which route you used and will not warn.
`--s3-access-key` is deliberately not in this list: an AWS access key id travels in
cleartext inside every signed request's `Authorization` header by construction, so hiding
it from argv would prevent no disclosure.

**The endpoint must support conditional writes - both of them.** The distributed lock that
keeps concurrent runs off each other's cache is built on `If-None-Match: *` to take the
lock and `If-Match` against an object's ETag to take over one whose holder died, so an
endpoint providing either one only nominally cannot back it. Amazon S3 supports both;
an S3-compatible implementation may not, and versions predating conditional-write support
do not. `Open` proves it rather than assuming it: on every run it writes a throwaway probe
object and checks that a create-if-absent write is refused when the key exists, that reads
name an ETag, that a write conditioned on a stale ETag is refused, and that one
conditioned on the current ETag is accepted. A backend failing any of those exits `2` with
`cache backend cannot be used as configured` - no retry helps, and the remedy is a
different endpoint. The last check matters most for an implementation that refuses every
`If-Match` alike: nothing about it looks permissive, and without that check it would pass
here and instead leave a dead holder's lock unreclaimable, which every waiting run reads
as ordinary contention.

A run against the S3 backend distinguishes four ways the cache can fail to serve it. A
bucket that cannot be reached, or that answers a request with a failure of its own, exits
`4` - retry once the outage clears. A bucket that parses but cannot back the distributed
lock's guarantees (it does not enforce one of the two conditional writes), or an
`--s3-endpoint` that fails to parse, exits `2` - no retry helps; the configuration itself
has to change. A bucket whose lock this run does not acquire before its own wait ceiling
elapses exits `8` only when this run actually saw another acquirer holding that lock
during the wait; a wait that reached the bucket but never got that answer - an endpoint
that never replies, replies only with failures, or contradicts itself - exits `4` instead,
alongside the other unreachable-backend cases. And a lock this run does acquire and then
loses - a later heartbeat finds another acquirer's token on the lock object - exits `8`
too: the run stops there instead of finishing its writes against a cache it no longer has
to itself. See "Exit codes" below for the exact messages to grep for.

## Security / Trust model

- Every secret this tool accepts has an environment route, and that is the route to
  use. `--token`, `--s3-secret-key` and `--s3-session-token` all put their value in
  this process's argv, readable by any local process for the life of the run;
  `GO_GALAXY_TOKEN`, `GO_GALAXY_S3_SECRET_KEY` / `AWS_SECRET_ACCESS_KEY` and
  `GO_GALAXY_S3_SESSION_TOKEN` / `AWS_SESSION_TOKEN` do not. go-galaxy cannot tell the
  two routes apart and issues no warning: a value's source is not observable once
  urfave has resolved it, so a warning would have to fire on every run, including the
  environment-driven majority it exists to encourage.
- The shared S3 snapshot object and the project registry object are a trust boundary:
  go-galaxy serves cached Galaxy metadata (including a collection's download URL and
  sha256) from them without re-validating against the origin on every use, so anyone
  who can write to the bucket can influence what a run installs. Restrict bucket write
  access (a write-restricted ACL, or dedicated credentials) just as you would protect
  a local cache directory - the local Bolt snapshot is implicitly trusted for the same
  reason, since writing it already requires local filesystem access to the cache
  directory.
- On the shared S3 cache, the backend's exclusive lock is held for a whole run, so its
  hold time is proportional to the work that run requests: a large legitimate install
  holds it for as long as the install takes, and a run pointed at a slow or hostile
  Galaxy server - including one named by a requirements.yml `source:` that matches no
  configured server - can hold it far longer, while other runners sharing the bucket
  give up waiting and fail. A principal who can run against the shared bucket already
  holds bucket write credentials: a run against the S3 cache writes to it regardless of
  what it is asked to do - artifacts, the snapshot, the project registry - and the lock
  itself is taken by writing an object, so a read-only credential cannot run at all.
  Restricting bucket write access - the same guidance as the bullet above - is what
  bounds this too. Give untrusted runs (for example, CI jobs building from an untrusted
  branch or fork) their own bucket, or a prefix of their own together with a credential
  restricted to that prefix, or no S3 cache at all, rather than shared-bucket write
  credentials. `--s3-prefix` on its own is not a boundary: it selects which keys a run
  reads and writes, not which keys its credential may touch, so a run holding a
  bucket-wide credential can still write every other prefix - including the lock - no
  matter what the flag says.
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
- **Operator-facing output is sanitized before it reaches stdout or stderr.**
  Text this program did not generate itself - a Galaxy server's HTTP reason
  phrase, an S3 error body, a lockfile entry's `name`, a manifest, a
  filesystem path - can carry ANSI escape sequences or other control bytes a
  terminal or log processor would act on; every such byte is replaced with
  `U+FFFD` (newline and tab are kept) before printing. `outdated`'s own
  report is sanitized on the identical boundary as every other command's
  output: a hostile server's HTTP reason phrase, printed on its `Lookup
  failed:` line, and a lockfile entry's `name`, printed on every line that
  names it, are both neutralized before printing rather than reaching the
  terminal raw.

## Reproducible CI

Pin transitive collections with a lockfile, then drive CI from it:

```bash
# once, when you change requirements.yml:
go-galaxy lock             # writes requirements.lock.yml

# in CI:
go-galaxy install --frozen # install exactly the locked versions
```

A frozen install fails loudly if a cached or downloaded artifact does not match the
lockfile's recorded SHA256, so a poisoned cache or a mutated upstream artifact cannot
install silently; lockfiles with no recorded SHA (older lockfiles) are not pin-checked.

`--frozen` decides *what* gets installed and needs no cache to do it. `--offline`
is a separate, stronger promise: no network call at all, so an artifact that is
not already cached is not a download but a failure. Against a cold cache the run
exits `5` naming the collection it could not get. Add `--offline` only where the
cache is known to be populated - a base image you baked it into ([container image
bake](#container-image-bake) below), or a restored CI cache your job treats as
mandatory. A restored CI cache is not that by default: the first run after any
lockfile change misses by construction, because the key just changed.

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

      # --frozen, not --frozen --offline: restore-keys can hand this job a
      # cache from an older lockfile, and the run after a lockfile change gets
      # no hit at all. Offline would make either a failure instead of a
      # download; frozen alone still installs exactly what the lockfile pins.
      - name: Install collections (frozen)
        run: go-galaxy install --frozen -p ./collections
```

### GitLab CI

```yaml
stages: [install]

variables:
  GO_GALAXY_CACHE_DIR: "$CI_PROJECT_DIR/.cache/go-galaxy"

install_collections:
  stage: install
  # Not ghcr.io/greeddj/go-galaxy: that image is distroless and carries no
  # shell, and GitLab runs every job's script through one, so a job in it
  # cannot start at all. Drop the static binary into an ordinary image.
  image: alpine:3
  cache:
    # cache:key is expanded when the job is created, and the cache is restored
    # before before_script runs, so a key computed by a script step is always
    # too late: whatever it expands to is the same for every pipeline, which
    # means one shared cache entry rather than one per lockfile. cache:key:files
    # makes GitLab hash the lockfile itself - the same input `go-galaxy hash`
    # reads.
    key:
      files:
        - requirements.lock.yml
      prefix: go-galaxy
    paths:
      - .cache/go-galaxy
  before_script:
    - apk add --no-cache ca-certificates curl
    - curl -sSLf -o /usr/local/bin/go-galaxy https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64
    - chmod +x /usr/local/bin/go-galaxy
  script:
    # --frozen without --offline: a cache miss is normal here - the first
    # pipeline after a lockfile change gets one - and --offline would turn it
    # into a failed job instead of a download.
    - go-galaxy install --frozen -p ./collections
```

**Distributed runners.** GitLab's own `cache:` is per-runner unless the runner
is configured with a distributed cache, so with several runners each one
rebuilds its own copy. Pointing go-galaxy at its own S3 cache instead gives
every runner one shared artifact cache: set `GO_GALAXY_S3_BUCKET`,
`GO_GALAXY_S3_REGION` and `GO_GALAXY_S3_PREFIX` in `variables:`, and
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` as masked project variables,
which go-galaxy reads directly. Keep the `cache:` block alongside it: the
extracted-tree store stays local to `GO_GALAXY_CACHE_DIR` even with the S3
backend, so the job cache is what saves re-extracting every collection.

Two runtime consequences of a shared S3 cache are worth knowing before you
enable it. Jobs sharing one bucket serialize: a run holds the backend's
exclusive lock for its whole duration, so a `parallel:` matrix against one
bucket runs one job at a time, and a job that gives up waiting on another's
lock exits `8`. And a bucket is a trust boundary, not just storage: give jobs
that build untrusted branches or forks their own bucket, and see [Security /
Trust model](#security--trust-model) for why a prefix alone is not a boundary.

### Lockfile drift gate

Fail a pull request when `requirements.lock.yml` no longer matches
`requirements.yml` - a root added, removed, or repinned without regenerating
the lockfile. `lock --frozen` reads the lockfile as the thing to check rather
than as the answer, which is the opposite of what install/warm `--frozen` do -
it still resolves fresh, and only a warm resolve cache lets that stay off the
network - so this is a separate job from the install above, not a replacement
for it. Add
`--refresh` for a second, distinct gate on the same file: `lock --frozen`
alone only catches a `requirements.yml` change, since it reuses the cached
resolve; `lock --frozen --refresh` also catches a newer version simply
having been published upstream, since `--refresh` makes the comparison's
fresh resolve reach the live servers instead:

```yaml
name: lockfile-drift
on: [pull_request]

jobs:
  lockfile-drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Install go-galaxy
        run: |
          curl -sSL https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64 \
            -o /usr/local/bin/go-galaxy
          chmod +x /usr/local/bin/go-galaxy

      - name: Check requirements.lock.yml matches requirements.yml
        run: go-galaxy lock --frozen

      - name: Check requirements.lock.yml is not stale against upstream
        run: go-galaxy lock --frozen --refresh
```

A nonzero exit from either step (code `6`, the lockfile class - see the
exit-code table below) means the PR needs `go-galaxy lock` (optionally
`--refresh`, to pick up the newer upstream version too) run and its updated
`requirements.lock.yml` committed.

### Container image bake

Pre-warm caches in your CI base image so jobs only hardlink into place:

```dockerfile
FROM debian:stable-slim

# The published image is distroless: one static binary at /go-galaxy, and
# nothing else - no shell, no CA bundle. Copy the binary out of it, and bring
# your own certificates, or the first Galaxy request fails to verify its TLS
# certificate.
COPY --from=ghcr.io/greeddj/go-galaxy:latest /go-galaxy /usr/local/bin/go-galaxy
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Pin the cache somewhere that does not depend on who runs the job: the
# default is $HOME/.cache/go-galaxy, and the warm below runs as root while
# your jobs may not.
ENV GO_GALAXY_CACHE_DIR=/var/cache/go-galaxy

WORKDIR /src
COPY requirements.yml requirements.lock.yml ./
# A run needs the cache lock, so a job user that can only read the baked cache
# fails to start with `cache backend cannot be used as configured` (exit 2).
RUN go-galaxy warm --frozen && chmod -R a+rwX "$GO_GALAXY_CACHE_DIR"
```

Jobs built on that image are the case `--offline` is for, since the cache is
part of the image rather than something a key might miss:

```bash
go-galaxy install --frozen --offline -p ./collections
```

## Color

Status markers (`✔`, `✗`, `!`) are colored only when the stream they are
written to is a terminal, decided per stream: with `go-galaxy install >
install.log`, stdout gets plain text while stderr, still a terminal, keeps its
color. Redirecting both leaves the log free of escape sequences, so `grep '^✗'`
matches the lines it names.

Two environment variables override that check:

| Variable                        | Effect                                                     |
|---------------------------------|------------------------------------------------------------|
| `NO_COLOR`                      | Set to any non-empty value: never emit color.               |
| `CLICOLOR_FORCE` / `FORCE_COLOR`| Set to any non-empty value other than `0`: always emit color, terminal or not. |

`NO_COLOR` wins when both are set: it is an opt-out, and an opt-out another
variable can override is not one. The force variables exist for a CI that is
not a terminal but does render escape sequences in its log viewer. A value of
`0` for either force variable means "do not force" and falls through to the
terminal check rather than disabling color outright.

The spinner is a separate decision and is not affected by `NO_COLOR`: it is
drawn only when stdout is a terminal, and under `NO_COLOR` it still runs, just
without color.

## Exit codes

`go-galaxy` exits with a class-specific code instead of a flat `1`, so CI
pipelines can branch on failure type without parsing log output. The same
numbers and their one-phrase meanings are printed by `go-galaxy --help`; the
qualifications below are the part only this table carries:

| Code | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
|-----:|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------                                                                                                                         |
|    0 | Success                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
|    1 | Generic failure (does not match any class below)                                                                                                                                                                                                                                                                                                                                                                                                                  |
|    2 | Usage or configuration error (invalid flags, requirements, `ansible.cfg`, an unsupported collection source, an explicit namespace conflicting with a dotted collection name, an unsupported cache-snapshot schema version, an unreadable or unparseable project requirements file, or a cache backend that cannot be used as configured)                                                                                                                          |
|    3 | Dependency resolution failure (conflicts, missing candidates, cycle)                                                                                                                                                                                                                                                                                                                                                                                              |
|    4 | Network or Galaxy API failure (timeouts, stalled transfers, metadata and cache-state deadlines, a versions listing that exceeded its page ceiling, a response body that exceeded its size ceiling (an artifact, a metadata document, or a bucket listing), offline-mode violations, an unreachable cache backend, or an `outdated` run in which at least one latest-version lookup failed)                                                                        |
|    5 | Install-time failure (unsafe archive/symlink content, empty file, missing artifact cache)                                                                                                                                                                                                                                                                                                                                                                         |
|    6 | Lockfile error (missing, invalid, mismatched with requirements, or out of date under `lock --frozen`)                                                                                                                                                                                                                                                                                                                                                             |
|    7 | Artifact-integrity failure (content does not authenticate against its naming sha256, or the digest is malformed)                                                                                                                                                                                                                                                                                                                                                  |
|    8 | Cache contention (the cache lock is held elsewhere, the S3 lock's wait ceiling elapsed after this run observed another holder, or a lock this run did hold was taken away by another holder mid-run)                                                                                                                                                                                                                                                              |
|    9 | Persisted cache state is corrupt or oversized and must be discarded (a project registry that fails to decode, a state object that exceeds its size ceiling, or - local backend only - a Bolt snapshot file that fails one of its own corruption checks)                                                                                                                                                                                                           |
|  130 | Interrupted (a caught SIGINT, or the caller's own context canceled)                                                                                                                                                                                                                                                                                                                                                                                               |

Exit `6`'s "missing" half is uniform across every command that requires a
lockfile: `install --frozen`, `warm --frozen`, `lock --frozen`, `tree`,
`explain` and `outdated` all exit `6` when the lockfile they were told to read
is not there, rather than treating its absence as a usage error. `hash` is the
one deliberate exception and exits `0`: with no lockfile it falls back to
hashing `requirements.yml`, which is the documented behavior for repositories
that do not lock.

Exit `7` covers content that failed to authenticate against the sha256 that
named it - a lockfile pin, a Galaxy server's declared digest, a cache sidecar,
or the extracted store's content-address key - or a digest that was
structurally malformed. It is a stop-and-alert class: do not retry it
automatically. A retry cannot repair it, because the bytes or the digest are
wrong at the source, not transiently unavailable. It also outranks the network
class: a run that hits both an integrity failure and a network failure exits
`7`, not `4`.

Exit `8`'s lock-loss half outranks exit `7` in turn: a run that both lost the
cache lock mid-run and failed an integrity check exits `8`. Once another holder
is writing the same cache, this run's own checksum verdict is no longer
evidence about the artifact - it may simply be that other holder rewriting the
artifact underneath it - so the exclusivity failure is the actionable fact and
the mismatch is a symptom of it. Fix the contention first, then rerun; if the
integrity failure is real, the rerun reports it as exit `7` with nothing else
touching the cache.

A stalled or byte-dripped transfer is never reported as an interrupt, even
though the underlying mechanism that unblocks it is a context cancellation:
the tool distinguishes its own no-progress cancellation from a genuine SIGINT
or caller cancellation, and only the latter exits `130`. The same holds for
the metadata and cache-state ceilings above: grep the run's output for
`galaxy metadata fetch deadline exceeded` or `cache state object deadline
exceeded` to tell one of these deadlines apart from a genuine interrupt or
from any other network failure sharing exit code `4`.

Exit codes `2`, `4`, and `8` each fold in more than one cache-backend
condition too, distinguishable the same way - grep the run's output for the
message: `cache backend cannot be used as configured` (exit `2` - an
`--s3-endpoint` that fails to parse, or a bucket that does not enforce
create-if-absent or compare-and-swap, so the distributed lock cannot work),
`cache backend unavailable` (exit `4` - the backend could not be reached, or
answered with a failure that is not this program's own doing), `another
process holds the cache` (exit `8` - a local Bolt file open timed out against
another process's held lock, or the S3 lock's wait ceiling elapsed after this
run observed another acquirer holding it), `another instance is running`
(exit `8` - a second local run found the lock already held and refused to
start immediately), and `cache lock ownership was lost to another holder`
(exit `8` - this run acquired the lock and a later heartbeat found another
acquirer's token on it, so the work it had already done was not exclusive).

On the S3 backend, exit `8` reached through the wait means this run saw another
acquirer holding the cache lock at some point during the wait - not that the
backend was still healthy when the wait gave up, since one observation early in
the wait is enough even if the backend answers nothing at all for the rest of
it. A run that never got such an answer from the bucket in the first place -
one that accepts connections and never replies, replies only with failures, or
contradicts itself about whether the lock object exists - exits `4` with
`cache backend unavailable` instead. The `cache lock ownership was lost to
another holder` half has the opposite shape and is not covered by that
sentence: there was no wait at all, the lock was granted, and the positive
observation of another holder came afterward, from a heartbeat during the run.

Exit `9` means the persisted cache state itself - not this reader's ability
to interpret it - cannot be trusted by anyone and must be discarded before the
run can proceed: grep the run's output for `corrupt project registry`,
`cache state object exceeds the maximum allowed size`, or `corrupt snapshot
store` to tell which one fired. The last of the three is local-backend only:
it means the local cache directory's Bolt snapshot file itself failed one of
its own corruption checks, not merely that this run's own reader could not
make sense of it. The remedy is mechanical and safe to automate: delete the
offending object (or the whole cache directory / bucket prefix), or rerun
with `--clear-cache`, then rerun the command. This is deliberately distinct
from exit `2`: a snapshot a newer binary wrote in a schema this one cannot
safely interpret (`unsupported snapshot schema version`) exits `2` instead,
since the snapshot itself is not damaged, only unreadable by this particular
binary, and discarding it would destroy a shared cache other, newer runners
still depend on.

## Metrics

Pass `--metrics-file path/to/run.json` to install/warm/lock/outdated to emit a
JSON report suitable for CI dashboards:

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
run that failed to install some collections, a run whose snapshot save itself
failed, and a `lock --frozen` run that found drift - and is not written when
the run aborts earlier (unreadable `requirements.yml`, a resolution failure,
or a missing or unloadable lockfile). Its existence is therefore not a success
signal: gate automation on the process exit code (see the table above), never
on whether the metrics file exists or looks clean. This matters most for
`lock`: `failures` is always `0` in a `lock` report, so the report carries no
failure signal at all for that command, and a `lock` run whose snapshot save
failed, or whose `--frozen` gate found drift, still leaves a clean-looking
report next to a nonzero exit code. `frozen` is `true` exactly when the run
honored `--frozen`, for every command that reads the flag, `lock` included:
for `install`/`warm` that means resolving from the lockfile, and for `lock`
it means gating the fresh resolve against the lockfile instead of overwriting
it - not merely whether the flag was passed. `outdated` never honors
`--frozen`, so its report always omits `frozen`, whatever the flag or
`$GO_GALAXY_FROZEN` said.

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
