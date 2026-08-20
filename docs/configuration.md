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

## ansible.cfg

```ini
[defaults]
collections_path = ./collections

[galaxy]
server = https://galaxy.ansible.com
cache_dir = /home/ci/.cache/go-galaxy
```
