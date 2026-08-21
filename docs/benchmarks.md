# Benchmarks

`go-galaxy` vs `ansible-galaxy` on three requirements files (1 / 10 / 100 root
collections, all without transitive deps for an apples-to-apples fetch+extract
comparison), measured on Linux with an xfs filesystem, which is what CI
overwhelmingly runs on.

Mean of 5 measured runs. The warm and frozen scenarios are preceded by one
priming run that is not measured. Both tools are invoked with `--no-deps`.

## Local cache

| Scenario             | 1 collection | 10 collections | 100 collections |
|:---------------------|-------------:|---------------:|----------------:|
| **cold cache**       |              |                |                 |
| ・ansible-galaxy      |       8.99 s |       152.28 s |        458.10 s |
| ・go-galaxy           |       2.93 s |         7.71 s |         21.67 s |
| ・**speedup**         |     **3.1x** |      **19.8x** |       **21.1x** |
| **warm cache**       |              |                |                 |
| ・ansible-galaxy      |       5.43 s |        36.67 s |        343.00 s |
| ・go-galaxy           |      0.206 s |        0.292 s |          1.04 s |
| ・**speedup**         |    **26.4x** |     **125.6x** |      **330.1x** |
| **frozen + offline** |              |                |                 |
| ・go-galaxy           |      0.217 s |        0.309 s |          1.13 s |

**Read the warm row knowing what each tool caches.** `ansible-galaxy` keeps
only an API response cache - a single `api.json` - and downloads every tarball
into a temporary directory it deletes afterwards. Its warm run therefore still
re-downloads all 100 collections, and saves only the metadata round trips.
`go-galaxy` caches the tarballs and the extracted trees, and hardlinks the
installed files out of that cache. The three-hundred-fold figure is the honest
measurement of two different designs, not of the same design done faster.

The cold rows are network-bound and correspondingly noisy: `ansible-galaxy` at
10 collections spread from 87 s to 229 s across five runs. The `go-galaxy` warm
and frozen rows are the tight ones, within a few percent of their mean, because
they touch no origin at all.

## Disk

| After           | ansible-galaxy | go-galaxy |
|:----------------|---------------:|----------:|
| 1 collection    |        26.3 MB |   30.6 MB |
| 10 collections  |        71.2 MB |   82.6 MB |
| 100 collections |       458.1 MB |  536.9 MB |

Cache plus installed collections, counted once. That qualification matters for
`go-galaxy`: its installed files are hardlinks into its own cache, one inode
under two names, so adding the two directories separately double-counts them.
At 100 collections the cache holds 532.6 MB and the install tree adds only
4.3 MB of genuinely new blocks - the directories, the `GALAXY.yml` sidecars and
the extract markers, none of which are hardlinked.

On a single project `ansible-galaxy` uses slightly less disk. The difference
appears from the second project or the second run onward: another project
wanting the same collections costs `ansible-galaxy` another 442 MB of real
bytes and `go-galaxy` about 4 MB of directory entries.

## Filesystem sensitivity

A `--frozen --offline` install creates about 66,000 filesystem objects for the
100-collection set - 50,792 files and symlinks hardlinked out of the extracted
store, plus 15,762 directories. That means the wall clock is dominated by the
cost of creating an inode, and that cost varies enormously between filesystems:

| 100 collections, frozen + offline | per object |  total |
|:----------------------------------|-----------:|-------:|
| Linux, xfs, 4 workers             |    17.1 us | 1.13 s |
| macOS, APFS, 12 workers           |     213 us | 14.0 s |

The same binary doing the same work is **12.5x slower on APFS**. Two practical
consequences. Numbers measured on a developer laptop do not describe what CI
will see, and this page is measured on Linux for that reason. And `--workers`
is worth tuning only where inode creation contends: on APFS the wall clock was
lowest at 4 workers and degraded past that, while on xfs the derived default
already performs well.

## Roles

`testing/bench.sh` also measures a roles install, in two scenarios over
`testing/requirements-roles.yml` - ten widely used Galaxy roles with no
dependencies between them, so a `--no-deps` run measures fetch plus extract
over one flat set, as the collection files do:

- `roles-cold` - both tools' caches and the roles directory wiped before each
  run: `ansible-galaxy role install --no-deps -r requirements-roles.yml -p
  <dir>` against `go-galaxy install --no-deps -r requirements-roles.yml
  --roles-path <dir>`.
- `roles-warm` - go-galaxy's caches primed once, only the roles directory
  wiped between runs.

No figures are published here yet; the numbers come from running the script,
which writes `roles-cold.md` and `roles-warm.md` into `dist/bench/` and
appends them to `summary.md`:

```bash
SCENARIOS="roles-cold roles-warm" testing/bench.sh
```

Read a warm result knowing what each tool caches, as with the collection rows
above: `ansible-galaxy` keeps no cache for a role at all and downloads the
GitHub archive of the tag on every run, while `go-galaxy` caches the artifact
it built from the tag and the extracted tree, and hardlinks the installed
files out of that cache. The two tools also fetch differently on a cold run -
`ansible-galaxy` the tarball GitHub serves, `go-galaxy` a pack of the tagged
commit through the git protocol - so a cold figure compares two transports,
not one transport done faster.

## Reproduce

`testing/bench.sh` is the full harness, including the S3 cache backend and a
peak-RSS pass; it needs `hyperfine`, a virtualenv with `ansible-core`, and
`docker compose -f testing/docker-compose.yaml up -d minio-svc` for the S3
scenarios. See [Development](development.md#the-benchmark-harness) for its
knobs and outputs.

```bash
brew install hyperfine
python3 -m venv .venv && .venv/bin/pip install ansible-core
go build -o ./dist/go-galaxy ./cmd/go-galaxy
testing/bench.sh                   # all sizes, all scenarios
SIZES=10 testing/bench.sh          # one file
SCENARIOS="warm" testing/bench.sh  # one scenario
SCENARIOS="roles-cold roles-warm" testing/bench.sh  # the role scenarios only
```

Budget about three hours for a full default run against the public Galaxy; the
100-collection `ansible-galaxy` scenarios are most of it. Every cache the
script wipes lives under `$TMPDIR`, never in `$HOME` and never inside the
repository. Point `$TMPDIR` at real storage: a `tmpfs` measures RAM rather than
disk, which for a workload that is mostly inode creation describes nothing.

## Conditions

- **Tools:** `ansible-galaxy [core 2.21.3]`, `go-galaxy` at commit `826c765`,
  built with go1.26.7.
- **Host:** Linux 6.12.0 x86_64, 4 CPUs, xfs. The macOS figures in the
  filesystem-sensitivity table above come from an Apple Silicon laptop with 12
  CPUs on APFS, and are there for contrast rather than as a second data point.
- **Both tools** run with `--no-deps`, so what is measured is fetch plus
  extract. The dependency resolvers differ too much for a shared number to mean
  anything: `requirements-100.yml` carries transitive constraint conflicts that
  `ansible-galaxy` resolves leniently and `go-galaxy` rejects strictly.
- **Cold cache:** both tools' caches and the install directory wiped before
  every run. Each tool gets a cache of its own, and `ansible-galaxy` is given
  `ANSIBLE_COLLECTIONS_PATH` and `ANSIBLE_LOCAL_TEMP` inside the same working
  tree, so neither tool sees the other's state and both do their temporary work
  on the same filesystem.
- **Warm cache:** caches primed once, only the install directory wiped between
  runs.
- **Frozen + offline:** `go-galaxy lock` once, then `go-galaxy install --frozen
  --offline` - zero network calls.
