# Galaxy servers and authentication

Beyond a single `[galaxy] server`, go-galaxy understands ansible's multi-server
configuration surface, so a fleet of CI jobs can share one `ansible.cfg` with a
private Automation Hub and the public Galaxy both configured.

## server_list and per-server sections

`[galaxy] server_list` (or `ANSIBLE_GALAXY_SERVER_LIST`, which wins outright
whenever it is set at all, even to an empty string) is a comma-separated list of
server ids. An id must match `^[A-Za-z0-9_-]+$` - a `.` would make the
`[galaxy_server.<id>]` section grammar ambiguous, and anything else is unsafe to
fold into an `ANSIBLE_GALAXY_SERVER_<ID>_*` variable name - and no two ids may
be equal or differ only in case, since both spellings would fold onto the same
environment-variable prefix. Either one is a hard config-load error. Each id
gets its own `[galaxy_server.<id>]` section:

```ini
[galaxy]
server_list = automation_hub, release_galaxy

[galaxy_server.automation_hub]
url = https://hub.example.internal/api/galaxy

[galaxy_server.release_galaxy]
url = https://galaxy.ansible.com
```

```bash
export ANSIBLE_GALAXY_SERVER_AUTOMATION_HUB_URL=https://hub.example.internal/api/galaxy
export ANSIBLE_GALAXY_SERVER_AUTOMATION_HUB_TOKEN=xxxxxxxxxxxxxxxx
go-galaxy install
```

The `_URL` export here is not redundant with the identical `url` the section
above already names: a token this run supplies is refused against a server
URL this run read out of the ansible.cfg file instead of from the operator -
see [--token](#--token) below for why. Naming the same address again through
the environment moves it onto the operator's own channel, which is what lets
the token export above resolve at all.

Every collection is resolved independently against `automation_hub` first, falling
back to `release_galaxy` only if the private hub doesn't have it - so one install
can legitimately draw some collections from the private hub and the rest from the
public Galaxy. go-galaxy also auto-discovers the API root under `url`, trying
`/api/v3` (galaxy.ansible.com's shape) and then the bare `/v3` a Galaxy NG /
Automation Hub deployment mounts directly under its own base path, so the same
`url` works for either shape without an extra option.

`[galaxy_server.<id>]` keys, and what go-galaxy does with them:

| Key                     | Support                                                                                                                                                |
|:----------------------|:-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `url`                   | Supported, required. A token is never sent to a `url` supplied by this key unless this same section also supplies the token - see [--token](#--token). |
| `token`                 | Supported. Authorizes only itself against this section's own `url`; it does not authorize an operator-supplied token overriding it.                    |
| `validate_certs`        | Supported (see TLS below). A token is never sent over a connection this key disabled verification for unless this same section also supplies the token - see [--token](#--token). |
| `api_version`           | Accepted only as `v3` (a no-op; this tool always speaks the v3 API); any other value is a config-load error.                                           |
| `username`, `password`  | Hard config-load error naming the key: this is ansible's Basic auth, which this tool does not implement.                                               |
| `auth_url`, `client_id` | Hard config-load error naming the key: this is ansible's Keycloak/SSO token exchange, which this tool does not implement.                              |
| anything else           | Warned about and ignored.                                                                                                                              |

Basic auth and Keycloak/SSO are refused outright rather than silently sending an
unauthenticated request and surfacing a confusing 401 later - the error names the
offending key so you know to configure a plain API token instead.

Every key also has a per-id environment override:
`ANSIBLE_GALAXY_SERVER_<ID>_URL`, `_TOKEN`, `_VALIDATE_CERTS`, where `<ID>` is
the id **upper-cased** and otherwise untranslated - a `-` stays a `-`, so an id
written `my-hub` is read from `ANSIBLE_GALAXY_SERVER_MY-HUB_URL`, and one
written `myHub` from `ANSIBLE_GALAXY_SERVER_MYHUB_URL`. That is ansible's own
rule, which composes the same name from the id upper-cased. An id with no
`[galaxy_server.<id>]` section at all builds purely from its env overrides,
which is the common shape in containerized CI where you'd rather not template
an ansible.cfg for a secret.

## --token

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

The pairing rule below is documented here, but it is not scoped to this
flag. It applies to every token you supplied yourself, which includes a
per-server `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN` inside a multi-entry
`server_list` - a shape `--token` itself cannot reach, since there it is a
hard error for the separate reason above. Every configured server is judged
on its own, and the first one that offends fails the whole config load
naming that server, so a `server_list` entry other than the first is refused
just as squarely. Where the rest of this section says "the single effective
server", read "the server that token is configured for" whenever the token
in your hands is a per-server `_TOKEN`.

**`--token` / `GO_GALAXY_TOKEN` is refused, not merely overridden, when the
single effective server's URL came from the ansible.cfg file rather than
from you, and refused the same way when that server's certificate
verification was disabled by the file rather than by you.** A repository
can commit an `ansible.cfg` naming any address it likes - a bare `[galaxy]
server` line is enough, no `server_list` and no `[galaxy_server.<id>]`
section required - and a token you export the ordinary CI way must never
follow an address you did not yourself supply, nor cross a connection whose
verification you did not yourself disable. Two remedies both move the URL
onto your own channel instead of the file's, and neither needs a new
switch: export `ANSIBLE_GALAXY_SERVER` (or, for a `server_list` entry, that
id's own `ANSIBLE_GALAXY_SERVER_<ID>_URL`) naming the identical address, or
pass `--server`/`$GO_GALAXY_SERVER` the address itself. Naming the
`server_list` id through `--server=<id>` does not help: an id match only
selects which section applies, and that section's own `url` is still
file-sourced either way. The TLS-policy refusal has its own analogous
remedy instead - `ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS`, naming the
identical value already in the section - since moving the *address* onto
your own channel does nothing for a *TLS policy* the file separately
disabled for it. When one section supplies both `url` and `validate_certs`,
the two remedies are not alternatives: you need both, and you meet them one
at a time, since the destination refusal is reported first and the TLS one
only on the rerun after you have fixed it. A section's own `token` key does
not open either door: overriding it with `--token`/`GO_GALAXY_TOKEN` while the `url` or the
`validate_certs` is still file-sourced is refused the same way, because
whether a section also declares a decoy `token` is a choice made by
whoever authored that file, not by you - the same pairing rule the
`[galaxy_server.<id>]` key table above states for `url`, `token`, and
`validate_certs`.

A `server_list` id containing a `-` makes the `ANSIBLE_GALAXY_SERVER_<ID>_*`
remedy above unsettable by a plain shell `export`, since `-` is not a legal
character in a POSIX shell variable name. Four routes remain: `env
'ANSIBLE_GALAXY_SERVER_MY-ID_VALIDATE_CERTS=no' go-galaxy ...` (`env`
accepts a name a shell `export` cannot); a container's own `-e` flag, which
carries the same exemption; renaming the id in `server_list` to use `_`
instead of `-`; or `--server=<url>` naming the address directly, which
discards the `[galaxy_server.<id>]` section entirely - its own `token` and
`validate_certs` go with it, so that route is a different configuration
rather than a workaround for this one.

Prefer the environment variable over the flag. A token passed as `--token`
lands in this process's argv, where any local process can read it - on Linux
through `/proc/<pid>/cmdline`, and in a `ps` listing on most systems - for as
long as the run lasts. `GO_GALAXY_TOKEN` carries the same value without that
exposure. go-galaxy does not detect which route you used and will not warn:
this is guidance about how you invoke the tool, not a check it performs.

## Precedence

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

## Server selection at resolve time

Resolving a collection against `server_list` deviates from ansible in two
deliberate ways:

- **First match wins, not a union.** For an unpinned collection, go-galaxy walks
  the effective server list in order and installs from the first server that
  has it. Unlike ansible, which unions results across every configured server,
  go-galaxy never merges: a collection published on more than one server always
  comes from the earliest one that has it.
- **Fail closed, not fall through.** Only a 404 across all of one server's API
  root candidates means "this server doesn't have it, try the next one". A
  401/403 response, or a `429`/`500`/`502`/`503`/`504` that survives the retry
  budget, aborts the whole run naming the server that failed and exits with the
  network exit code (`4`) - it never silently advances to the next server. Any
  other failure aborts the run just as squarely and exits `4` too, but reports
  the underlying error rather than the named-server phrasing: another 5xx such
  as `501`, or a transport-level failure such as a refused connection, a DNS
  failure or a TLS handshake error, none of which is retried at all. A wrong
  token or a brief outage on your private hub must never quietly redirect an
  install to the public Galaxy instead.

A `requirements.yml` collection's `source:` pins it to one server for the whole
run: an exact `server_list` id match, or a URL matching a configured server's
network origin (so `source: https://hub.example.internal/content/published/`
still gets that server's own token and TLS policy, even though the path differs
from the configured `url`).

## TLS: validate_certs

`validate_certs = false` really disables certificate verification for that
server - but only for that one server's own network origin, never globally and
never for a download host on a different origin. The run warns loudly about
it, even in quiet mode (twice, if that server also carries a token, since the
token would then cross a connection this run cannot authenticate).

Prefer trusting a self-signed hub's CA over disabling verification at all:
point `SSL_CERT_FILE` or `SSL_CERT_DIR` at it and leave `validate_certs`
unset. That is the remedy that needs no `validate_certs` key at all, so
nothing below ever applies to it.

When `validate_certs = false` genuinely has to stay, and the same server
also carries a token you supply yourself (`--token`, `GO_GALAXY_TOKEN`, or
that server's own `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN`), the pairing is
refused unless the `validate_certs` key is *also* sourced from your own
environment rather than the file: export
`ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS` naming the identical value. See
[Rejected as configuration errors](#rejected-as-configuration-errors) below
and [Galaxy servers and authentication](#galaxy-servers-and-authentication)
for the exact rule and its remedies.

## Rejected as configuration errors

These are refused before any request is made, exiting with the usage exit code
(`2`) - the operator has to fix `ansible.cfg`, an environment variable, or
`requirements.yml`, not retry:

- A server URL, or a `requirements.yml` `source:`, with embedded userinfo
  (`https://user:pass@hub/`).
- A token configured for a plaintext (`http://`) origin that isn't loopback.
- Two configured servers that share a network origin but disagree on their
  token or their `validate_certs`.
- A `server_list` id outside `^[A-Za-z0-9_-]+$`, or two ids that are equal or
  differ only in case - both spellings would fold onto one
  `ANSIBLE_GALAXY_SERVER_<ID>_*` prefix.
- An operator-supplied token (`--token`, `GO_GALAXY_TOKEN`, or that
  server's own `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN`) paired with a server URL
  this run read out of `[galaxy] server` or a `[galaxy_server.<id>] url`,
  unless that same section also supplied the token itself.
- The identical operator-supplied token paired with a server whose
  certificate verification a `[galaxy_server.<id>] validate_certs` key
  disabled, unless that same section also supplied the token itself. See
  [TLS: validate_certs](#tls-validate_certs) above for the remedy.

By contrast, an auth failure (401/403) or an unavailable server exits with the
network exit code (`4`) instead, since that's a runtime condition to retry or
investigate, not a configuration mistake.
