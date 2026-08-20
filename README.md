# go-galaxy

Fast Ansible Galaxy collections installer for CI.

> **Note:** This project was created in collaboration with the Claude Code.

CI pipelines often spend minutes downloading and unpacking Galaxy collections.
go-galaxy resolves, downloads and extracts them in parallel, hardlinks out of a
content-addressed cache on warm runs, and skips the network entirely under
`--frozen --offline`. With a lockfile and warm caches, installing 100
collections takes seconds rather than minutes - see
[Benchmarks](docs/benchmarks.md).

It is a drop-in for the collections subset of `ansible-galaxy`: the same
`requirements.yml`, the same `ansible.cfg` keys, the same `ANSIBLE_*`
environment variables. Where it deliberately behaves differently - one server
per collection rather than a union, a fail-closed 401/5xx, a resolver that
refuses an unsatisfiable constraint set instead of picking leniently - every
difference is written down in
[Compatibility with ansible-galaxy](docs/ansible-galaxy-compat.md).

## Scope

- Collections only (Galaxy API sources). Non-Galaxy sources (git/url/file/dir)
  are not supported, and `roles` entries are ignored with a warning.
- `requirements.yml` is either a mapping carrying a `collections` list or a
  bare top-level list of collection entries; anything else is refused.
- `ansible.cfg` is read for `[defaults] collections_path`, `[galaxy] server`,
  `[galaxy] server_list`, `[galaxy] cache_dir`, and `[galaxy_server.<id>]`
  sections (`url`, `token`, `validate_certs`). Everything else in that file is
  ignored or refused - see [Configuration](docs/configuration.md).
- A token you supply is never paired with a server address, or a relaxed TLS
  policy, that an `ansible.cfg` file chose rather than you. See
  [Galaxy servers and authentication](docs/servers-and-auth.md).

## Features

- Dependency resolution with snapshot reuse.
- API and tarball caches, local or shared over S3.
- Skip install if already extracted.
- Parallel downloads and extraction.
- Lockfile pinning every transitive collection to an exact version + SHA256.
- OpenPGP signature verification, in pure Go, with no `gpg` process.
- `cleanup` command to remove unreachable collections.

## Install

### Go

```bash
go install github.com/greeddj/go-galaxy/cmd/go-galaxy@latest
```

Binary is installed into `$(go env GOPATH)/bin` (usually `~/go/bin`).

### Binary

```bash
curl -sSLf -o /usr/local/bin/go-galaxy \
  https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64
chmod +x /usr/local/bin/go-galaxy
```

Substitute `linux-arm64`, `darwin-amd64` or `darwin-arm64` for another
platform. Each release also carries a `.tar.gz` per platform with the same
binary plus LICENSE and the documentation. See
[Verifying a release](docs/security.md#verifying-a-release) before trusting
either.

### Podman

```bash
podman run --rm -v "$PWD":/work -w /work ghcr.io/greeddj/go-galaxy:latest --help
```

### Build from source

```bash
go build -o ./dist/go-galaxy ./cmd/go-galaxy
```

## Usage

```bash
go-galaxy install -r requirements.yml -p ./collections
```

Running `go-galaxy` with no command runs `install`, so a bare invocation
performs a full install rather than printing help.

For reproducible CI, pin every transitive collection once and install from the
lockfile thereafter:

```bash
go-galaxy lock                 # writes requirements.lock.yml
go-galaxy install --frozen     # install exactly the locked versions
```

`go-galaxy` exits with a class-specific code rather than a flat `1`, so a
pipeline can branch on the failure type without parsing log output. See
[Exit codes](docs/exit-codes.md).

## Documentation

| Document | What it covers |
| :-- | :-- |
| [CLI reference](docs/cli.md) | Every command and option, `--dry-run`, output and color |
| [Configuration](docs/configuration.md) | `ansible.cfg` discovery and keys, the environment surface, `requirements.yml` |
| [Galaxy servers and authentication](docs/servers-and-auth.md) | `server_list`, tokens, precedence, TLS, refused configurations |
| [Signature verification](docs/signatures.md) | Keyrings, required counts, tolerated statuses, the manifest chain |
| [Compatibility with ansible-galaxy](docs/ansible-galaxy-compat.md) | Every deliberate difference, and what a migration runs into |
| [Caching](docs/caching.md) | The local cache, the shared S3 backend, `warm` and `cleanup` |
| [Reproducible CI](docs/ci.md) | GitHub Actions, GitLab CI, the lockfile drift gate, image bake |
| [Exit codes](docs/exit-codes.md) | The failure classes and what each one means |
| [Metrics](docs/metrics.md) | The JSON run report |
| [Security](docs/security.md) | Trust model, and verifying a release |
| [Benchmarks](docs/benchmarks.md) | Measurements against `ansible-galaxy`, and how to reproduce them |
| [How it works](docs/architecture.md) | The solver, the install pipeline, the caching model, the layering |
| [Development](docs/development.md) | Running the tests, the repository's own gates, lint, the benchmark harness |

## License

[MIT](LICENSE).
