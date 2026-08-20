# How it works

This document describes the mechanism rather than the behavior. What go-galaxy
does is documented in the [CLI reference](cli.md) and the pages beside it; this
is how it does it, for anyone reading or changing the code.

## Layering

The import graph is acyclic and shallow. `cmd/go-galaxy` is wiring,
`internal/galaxy/*` is the work, `internal/cache/*` is persistence behind a
seam, and a few leaf packages hold cross-cutting concerns.

```
cmd/go-galaxy            main, signal handling, exit-code decision
  cliflags               the flag sets and their defaults
  commands               the urfave/cli command tree
  exitcode               sentinel errors and signals -> exit codes
  buildinfo              version string

internal/galaxy/collections   the resolve-download-verify-extract-record pipeline
internal/galaxy/cleanup       reachability and removal
internal/cache                the only factory choosing a concrete backend
  local                       BoltDB snapshot + JSON registry + flock
  s3                          gzipped-JSON objects, hand-rolled SigV4, distributed lock

internal/galaxy/cache         the Backend / ArtifactStore seam, decorators, cache policy
internal/galaxy/store         the persisted snapshot as an in-memory value
internal/galaxy/solver        the version solver: pure, no I/O
internal/galaxy/archive       tar.gz extraction with the refusal rules
internal/galaxy/extracted     the content-addressed tree store
internal/galaxy/manifest      MANIFEST.json / FILES.json, read-only
internal/galaxy/signature     OpenPGP verification, read-only
internal/galaxy/requirements  requirements.yml parsing
internal/galaxy/config        flags + ansible.cfg + environment -> one Config
internal/galaxy/lockfile      requirements.lock.yml
internal/galaxy/infra         the per-run DI container
internal/galaxy/fetch         the shared HTTP client and its per-origin policy
internal/galaxy/metrics       the run counters
internal/galaxy/helpers       sentinels, size caps, tuning constants, validation
internal/gzipstream           the only place a gzip reader opens over untrusted bytes
internal/galaxy/output        the Printer interface and its output tiers
internal/progress             the Printer implementation
internal/safeout              control-sequence stripping
```

Two facts about this graph are load-bearing rather than incidental.

**`internal/galaxy/solver` imports only `helpers`.** No config, no store, no
HTTP. That is the purity claim made structural: the solver cannot reach I/O
even by accident, and everything it learns arrives through its `Provider` seam.

**`internal/galaxy/helpers` sits at the bottom**, above only `internal/safeout`.
It owns the sentinel errors that `errors.Is` matches across every layer, the
size caps and tuning constants, and the validation predicates - `IsPathElement`,
`IsCollectionName`, `IsSHA256Hex`, `IsExactVersion`. A value's shape is judged
by one rule at the boundary it enters through, never re-derived downstream.

`internal/galaxy/infra` is the per-run container: the printer, the shared HTTP
client, the clock, the temp-dir accessor, the metrics counters, and four
test-only deadline overrides that are reachable through accessors so a nil
container or a non-positive override falls back structurally. Nothing wires
those overrides to a flag, an environment variable or an ansible.cfg key.
Extend `Infra` rather than adding a global or widening a signature.

## The version solver

`internal/galaxy/solver` is a PubGrub-style solver: it chooses one version per
package satisfying every declared constraint, or proves no such choice exists.
It is pure, in-memory and deterministic, and performs no I/O of its own.

### Units

- **Term** - "package in set" (positive) or "not (package in set)" (negative).
  Negating a term flips its sign; the set is never complemented in place.
- **Incompatibility** - a set of terms that cannot all hold. Normalized to at
  most one term per package, sorted by package name, with tautological terms
  and redundant positive root terms dropped - but only while more than one term
  remains, so a genuinely single-term incompatibility is never emptied.
- **Assignment** - a term recorded at a decision level, either a *decision* (a
  version chosen for a package) or a *derivation* (a term forced by an
  incompatibility, which records which one).
- **Partial solution** - the assignment list plus, per package, the signed
  conjunction of every term assigned to it so far. That accumulation is
  maintained incrementally from the first assignment on, and it is exact
  whether or not the package's published version list was ever fetched.

Signed conjunction follows four rules and nothing else: `P(a)&P(b) = P(a&b)`,
`P(a)&N(b) = P(a-b)`, `N(a)&P(b) = P(b-a)`, `N(a)&N(b) = N(a|b)`. Entailment is
the matching four cases, with one asymmetry worth knowing: a negative term never
entails a positive one.

### Version sets

A version set is two independent sublines covering the whole version space,
quotiented by semver precedence so build metadata is invisible:

- the **release subline**, ordered by `(major, minor, patch)`, which has a total
  successor and so decomposes into half-open runs;
- the **prerelease subline**, ordered by full semver precedence, with
  metadata-free bounds.

The factoring is forced rather than chosen. The constraint grammar this project
speaks gates prereleases at the level of an AND group, which makes a constraint
set non-convex over a single order: "every release at or above 1.2.0" has no
interval form there, but is one run per subline here.

Each subline is kept canonical - sorted, nonempty, disjoint, non-abutting - so
structural equality *is* set equality, and the canonical byte encoding an
incompatibility is hashed by is injective. Intersection and complement preserve
canonicality directly; union is derived from them.

Two fields ride along beside the pieces. A singleton's original registry
spelling, which set equality deliberately ignores so it never affects set
identity, but which the exact-pin fast path and decision extraction do read. And
a display label for proof output, which is the one field never branched on for
any logic decision.

### Where prereleases are admitted

`semver.NewConstraint` stays the sole authority on what parses; only after it
accepts does an in-file mirror parser build the set. The mirror replicates the
vendored release's branch structure exactly, and a mirror failure on input the
authority accepted is reported as a solver bug rather than guessed around.

The gate itself is one rule: **an AND group in which no comparator's comparison
version carries a prerelease matches no prerelease version at all**, whatever
the individual runs would admit. A prerelease operand anywhere in the group -
including an exact pin on a prerelease - opens it for the whole group. That is
the mechanism behind both halves of the user-visible rule in
[Compatibility with ansible-galaxy](ansible-galaxy-compat.md#deliberate-differences):
stricter, in that a collection publishing only prereleases satisfies no plain
constraint; looser, in that `>=1.0.0-0` also admits `2.0.0-rc1`.

One shape is refused rather than approximated: a `!=` carrying both an x-range
patch and a prerelease operand excludes an infinite comb rather than a finite
union of runs, and admitting a comb piece kind would destroy closure under
complement.

### The loop

```
add the root incompatibility
loop:
    bail out if the iteration budget is exhausted (a solver defect, not a large input)
    check for cancellation
    unit-propagate from the package that just changed
    make a decision; if there is nothing left to decide, extract the result
```

**Propagation** walks each package's incompatibilities newest-first, since
conflict resolution tends to produce more general ones later. An incompatibility
with exactly one inconclusive term derives that term's negation; one that is
fully satisfied is a conflict and goes to resolution. Relating a term consults
only the partial solution's own accumulation - never a published version list -
so propagation and conflict resolution do zero I/O.

**Conflict resolution** finds the earliest assignment whose prefix satisfies the
conflicting incompatibility, and either backjumps (when that satisfier is a
decision, or when the previous satisfier sits at a different level) or merges
the incompatibility with the satisfier's own cause and repeats. The merged
incompatibility records both parents, which is what makes the derivation graph
reconstructible for the failure proof.

**Backtracking** truncates the assignment list and rebuilds every package's
bookkeeping by replaying the survivors. At Galaxy scale that full rebuild costs
microseconds, so no per-level snapshot machinery is kept. A package that loses
every assignment simply drops out: with exact sets there is nothing to preserve,
because two symbolically different constraints denoting the same set *are* the
same set.

**Deciding** picks a package by a frozen priority - exact pins first, then
highest conflict count, then, among packages whose version list is already
fetched, the fewest allowed candidates, then name order - with ties broken by
name ascending at every step. That third tier ranks only fetched packages, so a
pool in which none has been fetched falls straight through to name order.

Two fast paths keep the common case off the paginated version-list fetch
entirely: an exact pin decides without fetching a list at all, and an unfetched
package with a positive accumulation is probed with the registry's own reported
highest version and decided on it if it passes. Neither is free of the provider -
both still ask for the decided version's dependencies, and the probe costs a root
metadata resolve on top - but only when neither applies is the full version list
paged through.

### The Provider seam

```go
Highest(ctx, pkg)          (Version, bool, error)              // registry-reported highest, unchecked
Universe(ctx, pkg)         ([]Version, error)                  // all published versions
Dependencies(ctx, pkg, v)  (map[string]Constraint, error)      // validated fqdn -> constraint
```

An unknown package is an empty slice and a nil error, not an error. Key and
constraint validation happens inside `Dependencies`: a malformed dependency key
is a provider contract violation the core never guesses around. The production
implementation lives in `internal/galaxy/collections` and is what turns these
three questions into cached Galaxy API calls, recording as it goes which server
answered for each collection - that binding is what the resolved graph's
`source` is taken from.

`--no-deps` is a wrapper whose `Dependencies` always returns an empty map
without consulting the wrapped provider.

### Determinism

Every point where Go's map iteration order could leak into the result is
ordered explicitly: the propagation worklist pops the lexicographically smallest
package, candidate packages are sorted, dependency names are sorted,
incompatibility terms are sorted by package, and the published version list is
re-sorted by the core rather than trusted in the order the provider returned it.
That sort is semver precedence descending, tie-broken by the original string
descending, because single-level precedence is not a total order for
equal-precedence spellings such as `1.0.0` and `1.0.0+build` - and "the highest
version" has to be unambiguous.

There are no goroutines, no clock and no I/O in the core.

### The failure proof

A conflict renders as PubGrub's numbered explanation, built by walking the
derivation graph and emitting a line per node, with line numbers introduced for
nodes referenced more than once, plus one case where a partial-satisfier merge
forces a number early so a later line can refer back to it. Leaves phrase themselves by cause: a
dependency edge, "no version of X matches Y", or "X has no published versions".
When the terminal incompatibility is one that conflict resolution derived, the
final line is rewritten to begin `So,` and end `version solving failed.` A
terminal incompatibility that is external instead - a root requirement whose
constraint denotes the empty set, or a leaf reached through the clean-failure
path - renders as its own one-line description, with neither rewrite applied.

Hints are collected from the no-versions leaves, one per distinct package, and
this is where the prerelease gate becomes visible to an operator: a package
whose every published version is a prerelease is named as such, with an exact
pin or a `>=X.Y.Z-0` floor offered as the remedy. A package that publishes only
some prereleases draws its own, differently worded hint, since there the
excluded versions are a subset rather than everything.

## The install pipeline

`install`, `warm` and `lock` reach the cache backend through one funnel, which
opens the backend, takes its exclusive lock, and runs every piece of work under
the **holder context** that lock returned, judging the outcome against it.
`outdated` deliberately opens no backend at all, and therefore takes no lock and
serves no cached metadata.

That holder-context discipline is the property `internal/lockaudit` gates, over
two closed tables: one naming every function that takes the lock and then does
work under it - the funnel itself and cleanup's own equivalent - and one naming
the three collection commands that must reach the funnel rather than take a
lifecycle of their own. A run whose lock is taken away mid-flight stops rather
than continuing to install and persist without exclusivity. Granularity is one unit
of work per worker - an artifact already in hand still finishes extracting,
since neither the untar nor the store's rename is interruptible - but every
write to the shared cache ends with the context.

### Plan construction

Order is load-bearing:

1. Load `requirements.yml`; roles are warned about and ignored.
2. Build the verification context. This runs **ahead of the prefetcher**, so an
   unreadable keyring, or requirements declaring `signatures:` with none
   configured, fails the run before a single background download is scheduled.
3. Resolve - from the lockfile under `--frozen`, which touches no network at
   all, otherwise through the solver.
4. Fold the resolved set into a key-addressed map, rejecting an unsafe
   identifier, a non-exact version, or a duplicate key.
5. Check the post-condition that every requirements root came back resolved,
   which is what catches a solve silently dropping one.
6. Compute install levels by topological layering. This happens **before** the
   prefetcher starts, so a dependency cycle surfaces before any prefetch worker
   exists, and the level assignment can order the prefetch queue.
7. Start the prefetcher.

### Two pools, two resources

`--workers` bounds extraction, which is CPU-bound: an install or warm worker
unpacks a tree in the same goroutine that acquired it, and an install worker
that ends up acquiring an artifact itself does so inside that same bound.
`--download-workers` bounds the prefetcher instead: its background artifact
downloads and its cache-presence probe scan, both network-bound - a HEAD probe
or a streamed GET into a temp file, never an extraction. That is why
the download default is the larger of the two, and why it derives from the CPU
this process may use rather than from the node's core count.

### Prefetch and handoff

The prefetcher scans cache presence in parallel, then downloads ahead of the
install workers in install-level order. Its download workers never touch the
collections tree or the extracted store - their dependencies carry a nil root and
a nil extract store by construction - while the presence scan ahead of them does
stat the tree, through the same `os.Root`, to skip a collection that is already
installed. An install worker waits on its collection's key and takes
ownership of the temp file, so nothing is downloaded twice and nothing is
reclaimed twice. A prefetch failure is a warning, not a run failure: the worker
falls back to acquiring the artifact itself.

The prefetch pool is joined before the lock is released, structurally rather
than by convention - the join is deferred inside a callee of the function whose
own defers release the lock - so a late worker can never commit to the cache
after the run stopped holding it.

The prefetcher is disabled outright under `--dry-run`, `--no-cache`, or with no
artifact store, rather than by threading a flag into each worker.

### Acquiring an artifact

In priority order: a prefetched temp file wins; otherwise, an artifact that is
already cached, whose metadata the resolver did not push, and in a run that is
not verifying signatures, is served from cache with **no metadata request at
all**; otherwise metadata is fetched and the artifact downloaded.

That middle path is the metadata-free fast path, and the third condition is what
a verifying run gives up: a server's own signatures ride on the same
version-metadata document, so keeping the shortcut would let a server-signed
collection pass vacuously on a cache hit.

The digest a collection is judged against follows a trust ladder, and the rule
behind it is *validate what crossed a trust boundary, never what this process
just computed*: bytes this process hashed itself are authoritative; a lockfile
pin forces a fresh hash of the file rather than trusting any recorded value;
only then are a server's declared digest and a cache sidecar consulted, each
validated for shape first.

Downloading has two arms. With an extracted store, one pass writes the temp
file, hashes it, and feeds an extraction through a pipe simultaneously. Without
one, the bytes are written and hashed, then probed for tar.gz shape - a check
the streaming arm answers by construction, and one that keeps an error page from
occupying a shared cache slot when a server declares no digest.

### Verify, extract, record

Two questions in order, both before anything is written into the collections
tree: the pin proves these are the bytes the lockfile names, and the signature
proves who published them.

Extraction resets the destination through `os.Root` - a rooted remove followed
by a rooted directory creation - and then hands the per-entry untar that
just-created directory as a plain path, deliberately unrooted. Rooting each entry
write was measured at 1.5x to 2.3x the cost for no marginal coverage, and the
reason it buys none is the reset immediately beforehand: nothing can be
pre-planted inside a directory this run just created. What `os.Root` is load
bearing for is the ancestors - `ansible_collections`, the namespace and the name
directories - which it refuses to traverse if any of them is a symlink out of the
tree. The root is established at the download path itself rather than one level
lower, for the same reason the extracted store establishes its own at the cache
directory.

Extraction ends by writing the extract-done marker; recording then adds two
more, in this order: the `GALAXY.yml` sidecar in the version-scoped `.info`
directory, and the store entry.

### Bounded recovery

A cache-resident artifact that fails its digest check, its manifest chain, or
its extraction is evicted and refetched exactly once. "Once" is structural, not
a counter: eviction sets a force-download flag, and the next iteration's guard
returns on any failure instead of evicting again, so the loop runs at most twice
and never recurses.

Three classes are excluded because refetching cannot repair them: a
destination-side failure, where the fault is the tree rather than the artifact;
a signature source that was never obtained; and a verdict over the *set* of
signature blobs, which the same bytes would produce again. Eviction is also
skipped outright when the artifact never came from a cache hit, and under
`--offline`, where deleting the only local copy with nothing to refetch it from
would be pure data loss. Eviction removes the tarball and its sidecar only - never the extracted store, whose entries are
content-addressed and remain correct for every other project referencing them.

## The caching model

### Artifact cache: scoped by server, not by content

An artifact's key is a short fingerprint of the server base plus the escaped
filename, flat, with no directory nesting. Server scoping is the point: without
it, two servers publishing the same `namespace.name@version` would collide on
one slot and a cache hit would serve whichever landed first, indefinitely.

Content addressing was considered and rejected here for a mechanical reason: the
digest is not known until the download completes, and the cache-hit fast path
has to be answerable *before* any bytes are fetched.

### Extracted store: content-addressed, materialized by hardlink

Unpacked trees live under a per-digest directory and are materialized into a
collections tree by hardlink, with a copy fallback across devices. A readiness
marker is written last and carries a **version tag rather than a bare sentinel**:
the readiness check runs before the per-digest lock, so a tree unpacked by an
older binary would otherwise be trusted verbatim and hardlinked everywhere.
Bumping that tag forces every pre-existing tree to be rebuilt once.

Every path the store creates, renames or removes resolves through an `os.Root`
established at the **cache directory**, never one level lower: opening a root
follows a symlink while establishing it, so rooting at the store subdirectory
would adopt whatever that name pointed at.

Whether a digest is trusted or re-checked is carried explicitly, and the zero
value is the verifying one: a digest read back from a record makes the store
hash the file before extracting under it, while a digest this process computed
over exactly those bytes does not.

### Snapshot

One value holds the cached API responses, version lists, dependency maps,
install records, the dependency graph, the requirements spec, the last
resolution, and the warmed set. The local backend bucket-maps it into a single
BoltDB file - one atomic transaction rather than nine file writes - and the S3
backend marshals it as one gzipped JSON object.

Retention and redaction are applied at persist time, in the single copy path, so
both backends inherit one set of rules rather than each implementing its own.

The schema version is bumped for **any** change, including a purely additive
one, and the policy is drop-and-rebuild rather than field-level migration. The
reason is the shared cache: an older binary reading a newer snapshot must fail
loudly rather than silently ignore a bucket it does not know about.

A dirty flag records whether a run changed anything, so a run that only read can
skip the save entirely. Every write-locked method must set it, which
`internal/galaxy/store`'s own audit gates.

### Project registry

Keyed by project directory, recording the requirements file and collections path
of every project that has run against this cache. `cleanup` computes reachability
from it, which is why a registry that exists and fails to decode is an error
rather than an empty registry: reading it as empty would mean nothing is
reachable, and delete everything. A missing registry is simply empty.

`--dry-run` skips registering the project, since that is the only persistent,
non-cache, non-reconstructible write in the startup path, and it feeds a
destructive command.

### The extract-done marker

A file named for the artifact's digest, holding a format tag and a tally:
entry count, directory count, and total byte size. Not a hash. It catches any
file or directory added or removed, and any file whose size changed; it
deliberately misses an in-place edit that preserves the file's exact byte
length, which is pinned by a test so the guarantee is never mistaken for a
stronger one.

The digest is validated for shape before it is ever joined into a path, because
a leading `..` would otherwise fuse into the marker's constant prefix.

## The cache seam

`internal/galaxy/cache` declares two interfaces with deliberately different
concurrency contracts, and both implementations depend on the split rather than
merely tolerating it:

- **`Backend`** - state and locking. **Not** safe for concurrent use; its caller
  serializes every call, including the accessor that returns the artifact store.
- **`ArtifactStore`** - tarballs. Safe for concurrent use across *distinct keys*;
  concurrent commit and delete of the same key is undefined.

`Backend.Lock` returns the holder context described above. A backend whose lock
cannot be taken away returns the caller's context unchanged, so no caller needs
a per-backend branch - which is exactly what the local backend does, because
`flock(2)` holds while the process holds the descriptor and a contender fails
immediately rather than displacing the holder.

`internal/cache` is the only package that names a concrete backend. Both
backends classify their failures into the same small set of cache-backend
classes, so one operator mistake yields one exit code whichever backend was
configured.

## The lockfile

```yaml
server: https://galaxy.ansible.com
collections:
  - name: community.general
    version: "11.1.0"
    source: https://galaxy.ansible.com
    sha256: <hex>
    deps: [ansible.posix]
schema_version: 1
```

Written canonically - collections sorted by name, each dependency list sorted -
and atomically, so its hash is stable and `go-galaxy hash` is deterministic.

`server` is provenance rather than a source of truth: a source-less entry takes
its source from the consuming run's own configuration, not from this field. It
is still part of the file's identity, so a `server_list` reorder counts as drift
even when every pin is untouched.

Loading distinguishes exactly two outcomes - the file is not there, or it is
invalid - and three consumers depend on that dichotomy being exhaustive. An
entry's version must be exact: without that check, a lockfile carrying `"*"`
would make a `--frozen` install silently take the server's highest version.

`lock` is the command that manufactures the pin every later `--frozen` install
trusts, so it validates a server's declared digest for shape before writing it,
and validates each version before spending a metadata fetch on it - failing
closed under the whole-run exclusive lock rather than after buying work. An
empty digest is left alone, which keeps servers that publish no digests usable.
