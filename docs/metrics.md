# Metrics

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
or a missing or unloadable lockfile). A `--dry-run` run is the one exception on
the other side: it reaches finalize and still writes nothing, because the report
carries no field that would distinguish a preview from a real run. All four
commands suppress it and print a stderr warning naming the path that was
skipped - see [--dry-run](cli.md#--dry-run). Its existence is therefore not a success
signal: gate automation on the process exit code (see
[Exit codes](exit-codes.md)), never
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

`offline` asks a different question than `frozen` does. It does not report
whether anything was honored during the run: `--offline` (or
`$GO_GALAXY_OFFLINE`) configures the HTTP transport for the whole command, so
the field simply mirrors the flag as configured, identically for
`install`/`warm`/`lock`/`outdated`. Like `frozen`, it carries `omitempty` and is
therefore absent from the JSON whenever it is false - which is why the example
above, an ordinary networked run, does not show it at all. A dashboard reading
these reports should treat a missing `offline` as false rather than as unknown.

`server`, `lockfile` and `lockfile_hash` carry `omitempty` too, and in practice
only the last of the three goes missing. `lockfile_hash` is the hash of the
lockfile at the resolved path, read fresh off disk, and it is omitted whenever
that file does not exist or fails to load - so an `install` or `warm` run in a
project that has never run `lock` emits a report with no `lockfile_hash` key at
all. That is not a signal about the run: read its absence as "there was no
lockfile to hash", never as a failure.

A collection built from a git source counts like any other artifact: its
fetch is a miss and the pack bytes written to disk for it count as
`bytes_downloaded` (the pack, not the artifact built from it, is what crossed
the wire), and a later run that installs it from the cache is a hit.

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
