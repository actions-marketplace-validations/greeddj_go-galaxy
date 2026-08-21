# Configuration

## What go-galaxy reads

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

`ANSIBLE_CONFIG` is the one row that is not a setting's environment override:
it names the file the other rows are read from, and it is a discovery
candidate rather than a strict source. A path in it that does not exist is
skipped in favor of the next candidate, whereas `--ansible-config` and
`$GO_GALAXY_ANSIBLE_CONFIG` name a file that has to be there - see
[install options](cli.md#install-options) for the two behaviors side by side.

One variable go-galaxy reads is deliberately absent from that table.
`ANSIBLE_GALAXY_REQUIREMENTS_FILE` sits in ansible's namespace without being an
ansible option: ansible-core declares no requirements-file setting, and
`ansible-galaxy` takes that path only as `-r/--role-file`. It is read anyway,
and it is not going away, because pipelines already set it; it is documented
here rather than in the table so that nobody expects `ansible-galaxy` to
honour it.
`GO_GALAXY_TOKEN` is the other name with no ansible counterpart, for the
separate reason described under
[Galaxy servers and authentication](servers-and-auth.md#galaxy-servers-and-authentication).

A git collection source adds a small environment surface of its own, none of
it with an ansible counterpart: `GO_GALAXY_GIT_CREDENTIALS` and the
`GO_GALAXY_GIT_<ID>_*` variables bind a credential to a repository host (see
[Git sources and credentials](servers-and-auth.md#git-sources-and-credentials)),
and three variables go-galaxy reads rather than defines decide what an ssh
repository is reached with: `SSH_AUTH_SOCK` names the agent used when no key
is bound, `SSH_KNOWN_HOSTS` names the known_hosts file (`~/.ssh/known_hosts`
and `/etc/ssh/ssh_known_hosts` otherwise), and `SSL_CERT_FILE`/`SSL_CERT_DIR`
supply a private CA for an https repository exactly as they do for a Galaxy
server. `~/.ssh/config` is not read.

Anything else in `ansible.cfg` is ignored. Within `[galaxy_server.<id>]` the
exceptions are deliberate and loud: `username`/`password` (Basic auth) and
`auth_url`/`client_id` (Keycloak/SSO) are refused as config errors naming the
key rather than ignored, because silently dropping a credential would send an
unauthenticated request to a private hub. A token is also never paired with a
`[galaxy] server` or `[galaxy_server.<id>] url` this file supplied, and
never paired with a server whose certificate verification a
`[galaxy_server.<id>] validate_certs` key in this file disabled: for
`[galaxy] server` the destination refusal is unconditional, since there is
no `[galaxy] token` to satisfy it and no `[galaxy] validate_certs` key to
relax in the first place, while for a `[galaxy_server.<id>]` section either
refusal lifts only when that same section also supplied the token - see
[Galaxy servers and authentication](servers-and-auth.md#galaxy-servers-and-authentication) for
the full table, token precedence, and TLS.

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

A collection can also come from a git repository, in every spelling
`ansible-galaxy` accepts:

```yaml
---
collections:
  # auto-detected by the git+ or git@ prefix; version defaults to HEAD
  - git+https://github.com/acme/app.git
  - git@github.com:acme/app.git
  # explicit type with a ref: a branch, a tag, or a full 40-hex commit
  - name: https://github.com/acme/app.git
    type: git
    version: v1.2.0
  # ansible's combined form: "#<subdir>" selects a directory inside the
  # repository, ",<ref>" the ref, in that order, and the ref after the comma
  # wins over a version: key
  - git+https://github.com/acme/mono.git#collections/app,main
  # a repository holding one collection you name explicitly
  - name: acme.app
    type: git
    source: ssh://git@git.example.internal/acme/mono.git#collections
```

`version:` is the git ref to check out - a branch, a tag, a qualified
`refs/heads/...` or `refs/tags/...`, or a full forty-digit commit; an
abbreviated commit is refused. The collection's own version, namespace and
name come from its `galaxy.yml`, and the version there has to be an exact
`MAJOR.MINOR.PATCH`. A repository whose root carries no `galaxy.yml` is
searched one level down: every immediate child directory with a `galaxy.yml`
(or a built tree's `MANIFEST.json`) is a collection, and all of them are
installed unless the entry names one. A `signatures:` key is not accepted on a
git entry - the artifact is built here and nobody has signed it - and a
credential in the URL is refused; credentials are bound through the
environment (see
[Git sources and credentials](servers-and-auth.md#git-sources-and-credentials)).
The `url`, `file` and `dir` types stay unsupported. See
[Compatibility with ansible-galaxy](ansible-galaxy-compat.md) for what differs
from `ansible-galaxy` on a git source.

## ansible.cfg

```ini
[defaults]
collections_path = ./collections

[galaxy]
server = https://galaxy.ansible.com
cache_dir = /home/ci/.cache/go-galaxy
```
