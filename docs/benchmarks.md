# Benchmarks

`go-galaxy` vs `ansible-galaxy` on three requirements files (1 / 10 / 100 root
collections, all without transitive deps for an apples-to-apples fetch+extract
comparison). Numbers are mean ± σ from `hyperfine`, 5 measured runs with 1
warmup where applicable.

> **Read the σ, not just the mean.** The 100-collection cold figure for
> `ansible-galaxy` varies by a factor of two between runs (min 496 s, max
> 1019 s), so its speedup carries a correspondingly wide interval. The
> `go-galaxy` figures are far tighter, but see [Conditions](#conditions) below
> for what these particular numbers were measured on.

## Local cache

| Scenario             |     1 collection |    10 collections |   100 collections |
|:---------------------|-----------------:|------------------:|------------------:|
| **cold cache**       |                  |                   |                   |
| ・ansible-galaxy      |   16.39 ± 6.22 s |    54.10 ± 5.48 s | 667.19 ± 216.73 s |
| ・go-galaxy           |    5.86 ± 0.42 s |     6.91 ± 1.84 s |    32.83 ± 6.43 s |
| ・**speedup**         | **2.80 ± 1.08×** |  **7.83 ± 2.23×** | **20.32 ± 7.71×** |
| **warm cache**       |                  |                   |                   |
| ・ansible-galaxy      |    5.06 ± 1.51 s |    28.84 ± 4.79 s |  448.94 ± 87.54 s |
| ・go-galaxy           |    1.16 ± 0.01 s |     2.45 ± 0.29 s |    14.07 ± 0.23 s |
| ・**speedup**         | **4.37 ± 1.31×** | **11.77 ± 2.40×** | **31.91 ± 6.24×** |
| **frozen + offline** |                  |                   |                   |
| ・go-galaxy           |    1.19 ± 0.03 s |     2.41 ± 0.19 s |    14.04 ± 0.30 s |

## S3 cache backend

`go-galaxy` only - `ansible-galaxy` has no equivalent. `s3-warm` is the shape
the S3 backend exists for: a fresh runner with an empty local cache against a
warm shared bucket. The extracted-tree store stays local either way, so a warm
bucket alone still costs an extraction, which is why `s3-warm` sits above the
local `warm` row rather than matching it.

| Scenario   |  1 collection | 10 collections | 100 collections |
|:-----------|--------------:|---------------:|----------------:|
| ・s3-cold   | 5.08 ± 1.03 s | 11.73 ± 1.61 s |  31.94 ± 3.20 s |
| ・s3-warm   | 1.55 ± 0.04 s |  2.99 ± 0.02 s |  17.62 ± 0.25 s |
| ・s3-frozen | 1.56 ± 0.05 s |  3.01 ± 0.04 s |  17.52 ± 0.31 s |

`s3-frozen` is deliberately not `--offline`: reaching the bucket is a network
call, so `--offline` refuses the S3 backend outright at its opening bucket HEAD.

## Memory and bytes

Measured in a separate single-run pass, 100 collections: peak RSS from
`/usr/bin/time`, bytes from `go-galaxy`'s own `--metrics-file`, so the byte
column reads `n/a` for `ansible-galaxy`, which has no metrics report.

| Scenario                  | Peak RSS (MiB) | Bytes downloaded |
|:--------------------------|---------------:|-----------------:|
| ansible-galaxy, cold      |          808.4 |              n/a |
| go-galaxy, cold           |          164.7 |         53.6 MiB |
| ansible-galaxy, warm      |        1,104.1 |              n/a |
| go-galaxy, warm           |           92.5 |                0 |
| go-galaxy, frozen+offline |           93.7 |                0 |
| go-galaxy, s3 warm        |          192.7 |                0 |

The memory gap widens rather than narrows with the collection count, and
`ansible-galaxy` peaks *higher* on a warm cache than on a cold one.

## Why it scales

The speedup grows with the number of collections - `go-galaxy` parallelizes
downloads and cache presence probes across `--download-workers` (network-bound,
and always the larger default of the two) and extractions across `--workers`
workers (CPU-bound, sized from the CPU this process is permitted to use rather
than from the node's core count), uses hard links from a content-addressable
cache on warm runs, and skips the network entirely under `--frozen --offline`.
With a lockfile and warm caches, installing 100 collections takes ~14 s instead
of ~7 minutes.

## Reproduce

```bash
brew install hyperfine
python3 -m venv .venv && .venv/bin/pip install ansible-core
go build -o ./dist/go-galaxy ./cmd/go-galaxy
docker compose -f testing/docker-compose.yaml up -d minio-svc  # for the s3-* scenarios
testing/bench.sh                   # all sizes, all scenarios
SIZES=10 testing/bench.sh          # one file
SCENARIOS="warm" testing/bench.sh  # one scenario
RUNS=10 testing/bench.sh           # more measured runs than the default 5
```

The defaults are `RUNS=5`, `WARMUP=1`, `SIZES="1 10 100"` and
`SCENARIOS="cold warm frozen s3-cold s3-warm s3-frozen"` - so a bare
`testing/bench.sh` benchmarks the S3 cache backend as well as the local one,
which is what the `minio-svc` line above is for. The S3 scenarios are skipped
with a warning, rather than failing the run, when `$S3_ENDPOINT` (default
`http://127.0.0.1:9000`) does not answer, so the local scenarios still run on a
machine with no container runtime. Every cache the script wipes lives under
`$TMPDIR`, never in `$HOME` and never inside the repository, so benchmarking
does not touch the caches you actually use.

Budget about three hours for a full default run; the 100-collection
`ansible-galaxy` scenarios are most of it.

Each size writes three kinds of file under `dist/bench/`:
`<scenario>-<N>.md` is hyperfine's own table, `resources-<N>.md` adds a
`Peak RSS (MiB)` and a `Bytes downloaded` column measured in a separate
single-run pass, and `summary.md` concatenates every table produced.

## Conditions

- **Tools:** `ansible-galaxy [core 2.20.5]`, `go-galaxy` at commit `6608e79`
  (344 commits after `v1.0.2`), `hyperfine 1.20.0`.
- **Hardware:** single Apple Silicon laptop, `Darwin 25.6.0 arm64`, on home
  Wi-Fi. Cold-cache numbers are network-bound; warm and frozen are CPU/IO-bound.
- **The host was not idle during this run**, so the warm and frozen figures in
  particular should be read as an upper bound rather than as this tool's best.
  A quiescent machine is what the numbers above want and did not get.
- **Both tools** invoked with `--no-deps` so the benchmark measures fetch +
  extract. The dep-resolution paths in the two tools differ; in particular
  `requirements-100.yml` has transitive constraint conflicts that
  `ansible-galaxy` resolves leniently and `go-galaxy` rejects strictly, which is
  a separate comparison.
- **Cold cache:** both tools' cache directories and the install dir wiped
  before each run. The script points each tool at a cache of its own under
  `$TMPDIR` rather than at its default in `$HOME`, and sets
  `ANSIBLE_COLLECTIONS_PATH=$TARGET` so `ansible-galaxy` does not see anything
  pre-installed in `~/.ansible/collections`.
- **Warm cache:** caches primed once, only the install dir wiped between runs.
- **Frozen + offline:** `go-galaxy lock` once, then
  `go-galaxy install --frozen --offline` - zero network calls.
