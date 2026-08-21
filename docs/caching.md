# Caching

A run keeps four things between invocations: cached Galaxy API responses and
resolved dependency graphs (the snapshot), downloaded tarballs (the artifact
cache), unpacked collection trees (the extracted store), and a registry of the
projects that have run against this cache. What each is keyed by, and why the
artifact cache is scoped by server rather than by content, is described in
[How it works](architecture.md).

## The local cache

By default everything lives under `$HOME/.cache/go-galaxy`, relocatable with
`--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`). One run
holds it exclusively: a second run against the same directory fails fast with
`another instance is running` rather than interleaving writes. Installed files
are hardlinked out of the extracted store, so an installed collection and its
cache entry are one inode - which is what makes an install cheap and what makes
installed files read-only. See
[Differences a migration runs into](ansible-galaxy-compat.md#differences-a-migration-runs-into).

`go-galaxy hash` prints a deterministic `sha256:...` of the lockfile, or of
`requirements.yml` when no lockfile is present, for use as a CI cache key.
`go-galaxy warm` populates the caches without installing anything, and
`go-galaxy cleanup` removes cached collections no registered project reaches
any more - both are described in the [CLI reference](cli.md#commands), whose
[cleanup options](cli.md#cleanup-options) section carries the reachability
rules and the 30-day retention the extracted-cache sweep applies to warmed
entries.

Two caches must not be shared between principals holding different Galaxy
credentials; see [Security](security.md#security--trust-model) for why.

A collection built from a git source lives in the same caches under its own
key. The artifact key's scope is the source locator
`git+<url>#<subdir>@<commit>` rather than a server base, so a branch that
moves produces a new key and never overwrites the artifact built from its
previous commit - and, the other way round, the artifacts of superseded
commits stay in the artifact cache until `--clear-cache`, since nothing
references them and no sweep targets them. The snapshot records a git pin per
`(url, ref, subdir)`: the commit the ref resolved to and the collections that
commit held, which is what a rerun replays without contacting the remote. A
pin is keyed by the git line itself, so editing its URL, ref or subdir is a
new key; it is invalidated by `--refresh` (a commit ref is never
re-advertised, it is its own answer) and by `--clear-cache`, never by age and
never by an edit elsewhere in the requirements file. `--offline` replays a recorded pin or fails;
`--no-cache` fetches and builds once and hands the build straight to the
install phase without committing it. `warm` records a git collection under
`namespace.name@version` like any other, so two commits of a branch that did
not bump the collection's version share one warmed entry; the artifact cache
itself keeps both.

## S3 Cache (optional)

When `--s3-bucket` (or `GO_GALAXY_S3_BUCKET`) is set, go-galaxy uses S3 as the cache backend.
Artifacts and cache metadata are stored in S3; collections are still installed locally.

Both credentials are mandatory: setting the bucket without both an access key and a secret
key exits `2` with `s3 cache requires access/secret keys when GO_GALAXY_S3_BUCKET is set`.
Requests are signed by go-galaxy's own SigV4 implementation rather than by the AWS SDK, so
there is no credential chain behind those two values - no IAM role or instance profile, no
`~/.aws/credentials`, no `AWS_PROFILE`. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and
`AWS_SESSION_TOKEN` are read as alternative spellings of the [S3 cache
options](cli.md#install-options), and nothing else about an AWS environment is consulted.

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
`--s3-endpoint` that parses to no host, exits `2` - no retry helps; the configuration itself
has to change. An endpoint that is not a URL at all - an unclosed `[` in an IPv6 host, a
non-numeric port - fails earlier still, on the parse itself, and is reported unclassified
as exit `1` rather than joining this class. A bucket whose lock this run does not acquire
before its own wait ceiling elapses exits `8` only when this run actually saw another acquirer holding that lock
during the wait; a wait that reached the bucket but never got that answer - an endpoint
that never replies, replies only with failures, or contradicts itself - exits `4` instead,
alongside the other unreachable-backend cases. And a lock this run does acquire and then
loses - a later heartbeat finds another acquirer's token on the lock object - exits `8`
too: the run stops there instead of finishing its writes against a cache it no longer has
to itself. See [Exit codes](exit-codes.md) for the exact messages to grep for.
