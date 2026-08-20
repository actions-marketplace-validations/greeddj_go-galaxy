# Security

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

Every build also ships an SPDX SBOM listing the Go modules actually linked into
it, for scanning against a vulnerability feed without unpacking anything. The
two published shapes name theirs differently, and the raw binary is the one
that surprises: an archive's SBOM sits next to it as `<asset>.sbom.json`
(`go-galaxy_<version>_Linux_x86_64.tar.gz.sbom.json`), while a raw per-platform
binary is renamed for the `releases/latest/download/` flow but its SBOM is not,
so the document for `go-galaxy-linux-amd64` is published as
`go-galaxy_<version>_linux_amd64.sbom.json` and `go-galaxy-linux-amd64.sbom.json`
does not exist.

macOS builds are **not** Apple-notarized, so Gatekeeper has nothing to check
them against - the signature and provenance above are what to verify instead.
A binary downloaded by a browser also arrives quarantined; the release ships
no packaging that clears that flag on your behalf.

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
- A URL a Galaxy server supplied is refused outright when it embeds a credential in its
  userinfo (`https://user:pass@objects.example/a.tar.gz`), at both boundaries such a URL
  enters through: an artifact's download URL, and the metadata references a version walk
  follows (`versions_url`, `highest_version.href`). Left alone, that credential would
  replace the token you configured - Go's HTTP client turns URL userinfo into a Basic
  `Authorization` header before go-galaxy's own transport ever sees the request, and the
  transport does not overwrite a header that is already set. The refusal never prints the
  credential: the message names the refused URL with its userinfo and query cut out, and
  the run exits `5`. The same two cuts are applied to every line go-galaxy prints about
  such a URL - the lines it logs on the download path, the ones it logs while resolving
  metadata, and the source it names in a signature-verification failure - and to a URL it
  writes down for you to read, which is what the `download_url` and `version_url` recorded
  in `GALAXY.yml` are. A URL go-galaxy stores in order to fetch it again keeps its query,
  since cutting it would break the fetch that query authenticates: the cached API
  responses in the snapshot are that case. The request itself always carries the whole
  URL, for the same reason. A `versions_url` malformed enough that Go's URL parser rejects
  it is never judged by that guard, and it is never printed either: go-galaxy drops Go's
  own parse error, which would name the value whole, and reports instead that the metadata
  URL could not be built into a request, naming no part of it. A request that fails at the
  transport - a refused connection, a DNS failure, a TLS error - is re-rendered over the
  same two cuts, because Go's own report of it masks a password but leaves a query the
  server declared; the failure itself is preserved underneath, so nothing about retries
  or exit codes changes. Where the request had been redirected, that re-render names the
  URL go-galaxy asked for rather than the hop that failed, so a presigned redirect target
  never reaches the log at all - at the cost of the message no longer saying which hop in
  the chain was unreachable.
- Pinned (`--frozen`) installs are already immune to a poisoned snapshot: for a
  lockfile-pinned collection, go-galaxy hashes the actually downloaded (or on-disk)
  bytes and compares them to the sha256 recorded in the in-repo lockfile, not to the
  cacheable metadata sha, so a poisoned download URL or sha causes the install to fail
  closed instead of installing attacker content. Use `--frozen` with a committed
  lockfile in CI as the robust mitigation against a compromised cache.
- **Signature verification (see [Signature verification](signatures.md#signature-verification))
  trusts the configured keyring a priori; it binds neither a key to a namespace nor
  ships any publisher's key.** No key is bound to any namespace: any key in the
  keyring may vouch for any collection. What IS bound is the collection identity
  inside the signed document itself - once a signature verifies, the manifest's
  declared namespace, name and version are checked against the collection this run
  actually resolved, so a key this run trusts cannot vouch for one collection while
  a different one gets installed under its name.
- **The default signature policy passes when nothing was gathered to check - but
  it is not silent about it.** A bare count (or bare `all`) is satisfied
  vacuously by an empty signature list; go-galaxy warns about it anyway, once per
  affected collection, naming the exact strict spelling (`+1`, `+2`, ...) that
  would make that same pass fatal. Bare `all` alone does not close the vacuous
  pass either - only the `+` marker does.
- **Tolerating a failure status is likewise not a silent defusal, with two
  precise exceptions.** When a tolerated status defuses every gathered
  signature and nothing is left verified, that is the same vacuous pass above
  and is warned the same way - but only under `all` or a bare `0`, the two
  spellings where that shape actually passes; under the default or any
  stricter bare count, a tolerated failure still leaves the required count
  unmet and the run fails instead. What genuinely is silent: the one-line
  announcement a verifying run prints names the keyring and the required count,
  never the tolerated-status list, and the six status codes this tool accepts
  but can never actually produce (it holds no secret key material and runs no
  `gpg` process) are tolerated with no warning of their own.
- **An already-installed collection is skipped without being re-verified - and
  the run reports it.** `install`'s ordinary skip logic for an already-settled
  collection runs ahead of signature verification, so the first run with
  `--keyring` turned on over an existing workspace verifies nothing; it says so
  once, on the result tier, naming how many collections were left unverified.
  `warm` carries no equivalent skip and re-verifies every collection it
  touches, cache hits included.
- **Nothing about a verification verdict is ever persisted - to the snapshot,
  the extract marker, the lockfile, or `GALAXY.yml` - which is the trust model
  working as intended rather than a gap.** This project's threat model already
  treats the cache as attacker-writable, so a cached "already verified" is
  exactly the kind of assertion signature verification exists to refuse to
  take on faith.
- **Revocation is only as fresh as the keyring file on disk.** There is no
  keyserver lookup and no network revocation check; a key revoked upstream
  after the keyring was last updated on disk still verifies.
- **A signature covers file content, not archive metadata.** The manifest
  chain check (see [Signature verification](signatures.md#signature-verification)) records
  a name-to-content-sha256 mapping; file modes, ownership, modification times,
  and the tar stream's own framing are all outside what a signature can be
  said to cover, as is any archive entry of a type the chain check does not
  record (only regular files, symlinks and hardlinks are).
- **A `requirements.yml` `signatures:` entry drives an outbound request this
  run would not otherwise make, and what one outcome discloses is broader than
  reachable-or-not.** No destination class is refused - loopback, link-local
  (including a cloud metadata service's own address), and unique-local
  addresses are all fetched exactly like any other - and a failed connection
  attempt's own message can include the resolved address and address family,
  the port, the connect error, and, on a TLS mismatch, the certificate's own
  list of valid names; redirects are followed too. This is a disclosed
  reachability oracle for whoever can edit the repository's
  `requirements.yml`, not a closed one, on the same grounds go-galaxy already
  accepts for a discovered `ansible.cfg`'s own `[galaxy] server`/`server_list`/
  `url` values.
- **A `file://` signature source reads a repository-chosen local path, and a
  failed read collapses into one message - but a read that succeeds can still
  disclose more than that message would suggest.** An absent path, an
  unreadable one, a non-regular file, and one too large to read all render
  identically; a file that opens and reads, though, is then handed to the same
  verification walk as any other signature, whose own failure messages can
  render the file's remaining byte count and other content-derived details.
  This is disclosed rather than bounded.
- **Converting a GnuPG keybox to a keyring this tool can read is the
  operator's own step**, not something go-galaxy does automatically - a
  `.kbx` file is refused by name, with the export command to run instead.
- **Under `--offline` with a cold API cache, a verifying run can learn nothing
  about whether a collection is signed at all - and it says so distinctly.** A
  collection installed this way is not silently reported as unsigned; the
  vacuous-pass warning it prints names the metadata as unavailable rather than
  reading like "no signatures found".
- **`--frozen` and `--no-deps` normally skip a per-collection metadata request
  for an already-cached artifact; a verifying run cannot.** A server's own
  signatures live in that same version-metadata document, so turning on
  `--keyring` under either flag reintroduces a metadata request per collection
  that would otherwise have been skipped.
- **Signature verification covers the artifact a collection resolved to,
  never the dependency graph that led there.** A collection's dependencies are
  always taken from the Galaxy API's own dependency map for the resolved
  version, never from the signed manifest's own `dependencies` field.
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
