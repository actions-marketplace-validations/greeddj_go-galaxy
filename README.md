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

The speedup grows with the number of collections — `go-galaxy` parallelizes
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
  galaxy` resolves leniently and `go-galaxy` rejects strictly — that's a
  separate comparison).
- **Cold cache:** `~/.ansible/galaxy_cache`, `~/.cache/go-galaxy` and the
  install dir wiped before each run. `ANSIBLE_COLLECTIONS_PATH=$TARGET` so
  `ansible-galaxy` doesn't see anything pre-installed in `~/.ansible/collections`.
- **Warm cache:** caches primed once, only the install dir wiped between runs.
- **Frozen + offline:** `go-galaxy lock` once, then `go-galaxy install --frozen
  --offline` — zero network calls.
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
  - `[galaxy] cache_dir`

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

- `install` (`i`) — install collections from `requirements.yml`.
- `lock` (`l`) — resolve and write `requirements.lock.yml` for reproducible CI.
- `warm` (`w`) — populate the artifact + extracted caches without installing (for CI image bake).
- `hash` (`h`) — print a deterministic cache key (`sha256:…`) for use as a CI cache key.
- `cleanup` (`c`) — remove unused cached collections across projects.

### Global options

- `--help, -h`
- `--version, -v`

### install options

- `--verbose` — verbose output (`$GO_GALAXY_VERBOSE`)
- `--quiet, -q` — quiet mode (`$GO_GALAXY_QUIET`)
- `--dry-run`
- `--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`)
- `--server` (`$GO_GALAXY_SERVER`, `$ANSIBLE_GALAXY_SERVER`)
- `--timeout` (`$GO_GALAXY_SERVER_TIMEOUT`, `$ANSIBLE_GALAXY_SERVER_TIMEOUT`)
- `--download-path, -p` (`$GO_GALAXY_COLLECTIONS_PATH`, `$ANSIBLE_COLLECTIONS_PATH`)
- `--requirements-file, -r` (`$GO_GALAXY_REQUIREMENTS_FILE`, `$ANSIBLE_GALAXY_REQUIREMENTS_FILE`)
- `--ansible-config` (`$GO_GALAXY_ANSIBLE_CONFIG`, `$ANSIBLE_CONFIG`)
- `--workers` (`$GO_GALAXY_WORKERS`)
- `--no-cache` (`$GO_GALAXY_NO_CACHE`)
- `--refresh` (`$GO_GALAXY_REFRESH`)
- `--clear-cache` (`$GO_GALAXY_CLEAR_CACHE`)
- `--no-deps` (`$GO_GALAXY_NO_DEPS`)
- `--offline` (`$GO_GALAXY_OFFLINE`) — fail on any network access (cached state only)
- `--lock-file` (`$GO_GALAXY_LOCK_FILE`)
- `--frozen` (`$GO_GALAXY_FROZEN`) - require a lockfile and verify each installed or cached artifact's SHA256 against its lockfile pin, aborting the run on any mismatch
- `--metrics-file` (`$GO_GALAXY_METRICS_FILE`) — emit JSON run report

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

- `--verbose` — verbose output (`$GO_GALAXY_VERBOSE`)
- `--quiet, -q` — quiet mode (`$GO_GALAXY_QUIET`)
- `--dry-run`
- `--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`)
- `--s3-bucket` (`$GO_GALAXY_S3_BUCKET`)
- `--s3-region` (`$GO_GALAXY_S3_REGION`)
- `--s3-prefix` (`$GO_GALAXY_S3_PREFIX`)
- `--s3-access-key` (`$GO_GALAXY_S3_ACCESS_KEY`, `$AWS_ACCESS_KEY_ID`)
- `--s3-secret-key` (`$GO_GALAXY_S3_SECRET_KEY`, `$AWS_SECRET_ACCESS_KEY`)
- `--s3-endpoint` (`$GO_GALAXY_S3_ENDPOINT`)
- `--s3-session-token` (`$GO_GALAXY_S3_SESSION_TOKEN`, `$AWS_SESSION_TOKEN`)
- `--s3-path-style-disabled` (`$GO_GALAXY_S3_PATH_STYLE_DISABLED`)

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

## Notes

- Non-Galaxy sources (git/url/file/dir) are not supported.
- `roles` in requirements.yml are ignored.

## S3 Cache (optional)

When `--s3-bucket` (or `GO_GALAXY_S3_BUCKET`) is set, go-galaxy uses S3 as the cache backend.
Artifacts and cache metadata are stored in S3; collections are still installed locally.

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
when no lockfile is present) — perfect as a CI cache key.

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

| Code | Meaning                                                                           |
|-----:|-----------------------------------------------------------------------------------|
|    0 | Success                                                                           |
|    1 | Generic failure (does not match any class below)                                  |
|    2 | Usage or configuration error (invalid flags, requirements, or `ansible.cfg`)      |
|    3 | Dependency resolution failure (conflicts, missing candidates, cycle)              |
|    4 | Network or Galaxy API failure (timeouts, offline-mode violations)                 |
|    5 | Install or artifact-integrity failure (checksum mismatch, unsafe archive/symlink) |
|    6 | Lockfile error (missing, invalid, or mismatched with requirements)                |
|  130 | Interrupted (SIGINT)                                                              |

## Metrics

Pass `--metrics-file path/to/run.json` to install/warm/lock to emit a JSON report
suitable for CI dashboards:

```json
{
  "started_at":   "2026-04-28T10:00:00Z",
  "finished_at":  "2026-04-28T10:00:08Z",
  "command":      "install",
  "server":       "https://galaxy.ansible.com",
  "lockfile":     "requirements.lock.yml",
  "lockfile_hash":"<sha256-hex>",
  "duration_ns":  8123456789,
  "collections":  17,
  "failures":     0,
  "frozen":       true
}
```
