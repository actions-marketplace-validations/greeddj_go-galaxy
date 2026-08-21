# Signature verification

`install` and `warm` can verify a collection's detached OpenPGP signatures
against a keyring before it is extracted. `lock` and `outdated` never verify
anything - neither of them registers the flags below at all, since neither
one writes anything into the collections tree for a signature to guard. It
is off by default and free when off: with no `--keyring` configured,
nothing is read, no HTTP client for signature sources is built, and nothing
is gathered.

## Turning it on

| Flag                                          | Environment                                | Ansible environment                             |
|:----------------------------------------------|:-------------------------------------------|:------------------------------------------------|
| `--keyring`                                   | `GO_GALAXY_KEYRING`                        | `ANSIBLE_GALAXY_GPG_KEYRING`                    |
| `--required-valid-signature-count`            | `GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT` | `ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT` |
| `--ignore-signature-status-code` (repeatable) | `GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE`   | `ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES`  |
| `--disable-gpg-verify`                        | `GO_GALAXY_DISABLE_GPG_VERIFY`             | `ANSIBLE_GALAXY_DISABLE_GPG_VERIFY`             |

`--keyring` names an OpenPGP keyring: armored or binary key material this
tool reads directly, never a GnuPG keybox (`.kbx`, the container a default
GnuPG installation writes) - a keybox is refused by name, with the
`gpg --export --armor` command that produces a keyring this tool can read
included in the refusal. This tool executes no `gpg` process; it verifies
signatures in pure Go.

**`ansible.cfg`'s four `[galaxy]` signature keys (`gpg_keyring`,
`required_valid_signature_count`, `ignore_signature_status_codes`,
`disable_gpg_verify`) are refused, but their `ANSIBLE_*` environment
variables listed above are honored.** A discovered `ansible.cfg` naming any
of the four is never read for its value: this program cannot establish
whether that file was authored by the operator or by the repository under
test, and a setting that can relax a verification check must not come from a
file whose author it cannot establish. `install`/`warm` warn about it once
per run, naming the file and the key names it carried - never their values,
since none were read; `lock`, `outdated`, `cleanup`, `hash`, `tree` and
`explain` never emit this warning, since none of them verify anything it
could apply to.

## Required count and the vacuous pass

`--required-valid-signature-count` (default `"1"`) accepts four spellings: a
bare non-negative count (`0`, `1`, `2`, ...), the literal `all`, or either
prefixed with `+`. A bare count or bare `all` is satisfied **vacuously** by an
empty gather - a collection offering no signatures at all still passes -
because neither spelling asks for a floor an empty set can fail. Only the `+`
marker closes that: `+N` additionally requires at least one signature to
have verified, and `+all` requires the same on top of every checked signature
verifying. An operator who needs a signature actually required writes `+1`
or higher.

What the number counts is **distinct signing keys**, not signature files: two
signatures made by the same key count once, so `2` asks for two independent
signers and the same signature supplied twice can never stand in for a second
one. A counted spelling, bare or `+`, also stops the walk at its Nth distinct
signer, so signatures past that point are never checked and cannot fail the
run. `all` is the spelling with no such cutoff - it is the one that checks
every gathered signature.

Two spellings are traps rather than choices. `+0` can never pass, under any
outcome: it is refused the moment nothing verifies (the strict clause) and
refused the moment something does (the equality clause), since there is no
strict form of "require zero" signatures. And `-1` - a spelling that means
"require every signature" in some ansible documentation - is refused by
name, naming `all` as the spelling to write instead; ansible's own grammar
accepts no negative number, so `-1` was never a working spelling to begin
with.

A run with verification on that gathers nothing to check for a collection -
because none was declared and the server offered none - still passes, under
any non-strict spelling including the default, but it is not silent about
it: it prints one warning naming the collection and the exact strict
spelling (`+1`, `+2`, ...) that would make that same pass fatal. A
collection whose every gathered signature failed with a status this run
tolerates draws the identical warning, but only where it actually passes:
under `all` or a bare `0`, never under the default or any stricter bare
count, where a tolerated failure still leaves the required count unmet and
the run fails instead. A cold API cache under `--offline` produces a
related but distinctly worded warning: when this run could not even learn
whether the collection carries signatures at all (no cached version
metadata, or the server failed to answer), the message says so explicitly
rather than reading like "this collection carries no signatures" - a run
that never learned whether a collection is signed has not learned that it
is unsigned either.

## Ignored status codes

`--ignore-signature-status-code` (repeatable) names a gpg status code this
run tolerates as a non-fatal signature failure - `BADSIG`, `NO_PUBKEY`, and
so on. The vocabulary is closed and three-way: eight codes are ones a
verdict from this tool can actually carry (`BADSIG`, `ERRSIG`, `NO_PUBKEY`,
`EXPKEYSIG`, `REVKEYSIG`, `EXPSIG`, `NODATA`, `BADARMOR`); two more
(`KEYEXPIRED`, `KEYREVOKED`) are gpg's other name for two of those eight and
are accepted as synonyms, so configuring either spelling covers both; and six
more (`MISSING_PASSPHRASE`, `BAD_PASSPHRASE`, `NO_SECKEY`, `UNEXPECTED`,
`ERROR`, `FAILURE`) are accepted and inert, since this tool holds no secret
key material and runs no `gpg` process to report on itself. A value outside
all sixteen is refused, naming the accepted list. What the one-line
announcement a verifying run prints at startup does **not** name is which
codes are ignored - only the keyring path and the required count - and the
six inert codes are accepted with no warning of their own, since tolerating
a failure that cannot occur changes nothing either way.

## `signatures:` in requirements.yml

A collection entry may declare `signatures:` as a single source string or a
list of them:

```yaml
collections:
  - name: community.general
    version: "11.1.0"
    signatures:
      - https://galaxy.ansible.com/community/general/signatures/foo.asc
      - file:///etc/pki/collections/community-general.asc
```

Each source has to be one of: an absolute `http` or `https` URL, or a
`file://` URL naming an absolute local path with an empty authority or
`localhost` only (`file:///etc/...` or `file://localhost/etc/...`). Every
source is validated where the file is read, before any request is made, as a
usage error naming the file rather than a per-collection worker failure: a
value that names nothing fetchable (no scheme, an unsupported scheme, an
opaque value, an `http`/`https` URL naming no host, or a `file` URL naming
another host or a relative path), one embedding a credential in its userinfo
(`https://user:pass@host/sig.asc`), a `signatures:` value that is neither a
string nor a list of strings, or more than 64 sources declared for one
collection, each fail the load. A source's own query string is still sent on
the request - it may be a presigned capability the source needs to be
fetchable at all - but it is never persisted: it is cut from every source
before the resolved requirements spec reaches the store, so an `install
--frozen` run, which never rebuilds that spec, does not silently keep
re-persisting an old, uncut entry.

Declaring `signatures:` with no keyring configured is a hard error (exit
`2`) naming the first such collection - the verdict earned by a requirements
file that asks for verification with nothing configured to verify against.
`--disable-gpg-verify` changes that: verification explicitly switched off is
not treated as a missing keyring, so the run proceeds, but it says so loudly
in up to two places on the same run - once when a keyring is configured and
the switch disables it anyway, and once more (only if some requirements root
actually declares `signatures:`) naming the first collection whose declared
sources will not be checked.

## What a verifying run does differently

- **The pin, then the signature, both before extraction.** For a
  lockfile-pinned collection, its recorded sha256 is checked first, then its
  signatures - only once both hold does anything get written into the
  collections tree, so a collection this run cannot attribute is never
  extracted where a playbook would find it.
- **A verifying run cannot take the metadata-free cache-hit fast path.**
  Normally, an already-cached artifact with no metadata pushed by the
  resolver installs with no per-collection metadata request at all. A
  server's own signatures ride on that same version-metadata document, so a
  verifying run gives up that shortcut: a collection signed only by its
  server would otherwise pass vacuously on a cache hit and only get checked
  on a miss. Under `--frozen` or `--no-deps`, where nothing else would have
  fetched that document, turning on `--keyring` means paying one metadata
  request per collection that did not exist before.
- **Nothing about a verification verdict is written anywhere** - not the
  snapshot, not the extract marker, not the lockfile, not `GALAXY.yml`. This
  project's trust model already treats the cache as attacker-writable, so a
  cached "already verified" is exactly the assertion this feature exists to
  refuse to take on faith. The consequence: an already-installed collection
  is skipped without being re-verified, the same gate that makes `install`'s
  "Up to date" line unaffected by verification at all, and the run says so
  once, on the result tier, naming how many collections were skipped and
  therefore left unverified this run - the answer to "I turned `--keyring`
  on over an existing workspace and nothing seemed to happen." `warm`
  carries no such gate: it re-verifies every collection it touches, cache
  hits included, so "Already warm" does not carry the same caveat "Up to
  date" does.
- **`--offline` warns about a signature source it cannot reach, once per
  run.** A `file://` source works offline, since it reads local state, but
  an `http`/`https` one does not; if some requirements root declares a
  network source, the run warns once, naming the first such root, rather
  than failing outright - a `file://` source listed first that verifies on
  its own means a later network source in the same list is never actually
  reached.
- **A dry run validates the setup and verifies nothing.** `install --dry-run`
  and `warm --dry-run` with a keyring configured print a caveat alongside the
  rest of the preview: the setup (keyring, required count) is echoed, but no
  signature source is fetched and no artifact's signatures are checked, so
  the preview's would-fail count never reflects a signature verdict.
- **The combined candidate set - declared sources plus whatever the server
  offers - is capped at 64 per collection**, with the declared sources
  always taking priority. If the combined set exceeds the cap, the run warns
  naming how many were dropped from the end. This is a different cap from
  the load-time one above: that one bounds what one requirements entry may
  *declare*, this one bounds what the *gather* actually reads once the
  server's own offer is added on top.
- **A failed verdict's own message renders at most 8 per-signature causes**,
  plus a footer naming which distinct failure statuses were left out. This
  bounds only the message: every cause stays reachable through `errors.Is`
  for exit-code purposes, and the cap is not a claim about how many
  signatures were checked - it is the count of distinct failure statuses a
  verdict from this tool can carry, so a message at the cap can still show
  every shape the vocabulary has.
- **A refused collection can still leave its artifact cached and its
  extracted tree populated**, since both are populated before the signature
  verdict is reached. This is the largest of a few disclosed residuals: the
  extracted store is content-addressed, so this is harmless there (an entry
  is only ever reachable by naming the sha of the bytes it holds), but the
  artifact cache slot is named by server and filename rather than by
  content, so a rejected artifact can occupy a name a later run's cache
  lookup serves without contacting the origin again - the same run repeating
  the check reaches the same refusal, and `--frozen` re-hashes against the
  lockfile pin regardless. A signature *verdict* on its own (the signatures
  in hand did not satisfy the policy) does not evict the cached artifact,
  since refetching would only re-read the same signatures again; a broken
  manifest chain, a missing `MANIFEST.json`, or a signature that vouches for
  a different collection all do evict and retry once.

A collection built from a git source carries no signature and can carry none:
its `MANIFEST.json` is written here, at build time, and nobody has signed it.
A verifying run therefore reports every git collection as the vacuous pass
described above, with the same warning, and a strict `+N` spelling fails it -
which is the honest answer, since the run was told to require a signature it
cannot have. Its attribution rests instead on the identity the builder read
from `galaxy.yml` being the one the resolve asked for, and on the manifest
chain check below, which the builder runs on every artifact it produces.

## Manifest chain check

Once at least one signature verifies, the artifact is checked against
`MANIFEST.json` in both directions: every file, symlink and hardlink entry
the archive carries must be listed in `FILES.json` (a directory entry is
exempt from this side, since the check only ever records regular files,
symlinks and hardlinks even though extraction still creates it), and every
file `FILES.json` lists must hash to the digest it declares.
`MANIFEST.json` and `FILES.json` are both allowed to go unlisted by
`FILES.json` itself - a listing naming the documents that name it is a
convention, not a guarantee - but the exemption is asymmetric: a
`FILES.json` row naming itself with `ftype: file` is simply skipped, while a
`MANIFEST.json` row listed with a digest is checked like any other file and
can only ever fail, because the manifest's own bytes carry the listing's
digest, so no listing can name the manifest's digest without predicting
bytes that depend on it.

The signed manifest's declared `namespace`, `name` and `version` are also
checked against the collection this run actually resolved - a signature
that verifies but names a different collection (a downgrade, a substitution)
fails the same way a manifest whose identity cannot be read unambiguously
does (two conflicting spellings of the same JSON key, for instance). See
[Exit codes](exit-codes.md#exit-codes) for how both classify.
