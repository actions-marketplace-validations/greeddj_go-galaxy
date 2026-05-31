---
name: go-galaxy-build
description: Build go-galaxy binaries and OCI images via Justfile — host build, Linux amd64 build, container image. Use when the user asks to build a binary, produce a release artifact in dist/, or build a container image. Do NOT use for tests (see go-galaxy-test) or lint (see go-galaxy-check).
---

# go-galaxy — Build & OCI

## When to use

- "собери бинарь" / "build binary" / "release artifact"
- "собери под Linux" / "linux amd64"
- "собери образ" / "build image" / "OCI"

## Commands

| Intent | Command |
|---|---|
| Host build | `just build` |
| Linux amd64 build | `just build_linux` |
| OCI image (default podman, tag local) | `just oci` |
| OCI with explicit args | `just oci executor=podman tag=local` |
| OCI with docker | `just oci executor=docker tag=v1.2.3` |

## Outputs

- `just build` → `dist/go-galaxy` (host OS/arch)
- `just build_linux` → `dist/go-galaxy` (overwrites — built with `GOOS=linux GOARCH=amd64`)
- `just oci` → chains `build_linux`, then runs `<executor> build -f Dockerfile`. Image: `go-galaxy:<tag>`.

## Build flags

Both build targets:

- `CGO_ENABLED=0`
- `-trimpath`
- `-ldflags="-s -w -X main.Version=<git-tag-or-branch> -X main.Commit=<short-sha> -X main.Date=<UTC-RFC3339> -X main.BuiltBy=just"`

`Version` resolves to the latest git tag, falling back to current branch name if no tag exists. `Commit` is `git rev-parse --short HEAD`.

## Pre-build chain

- `just build` depends on **`check lint test`** — every host build runs the full quality + test suite first. `just check` itself depends on `just deps` (mutates `vendor/`, `go.mod`, `go.sum`).
- `just build_linux` depends on **`check`** only (no lint, no tests) — still mutating via `deps`.
- `just oci` depends on `build_linux`.

If the user wants a build *without* dep mutation, run `go build` directly with the same ldflags, or warn before proceeding.

## OCI gotchas

- Requires a container runtime (`podman` default, `docker` works as alternative — pass via `executor=`).
- `Dockerfile` is `FROM gcr.io/distroless/static-debian13:nonroot` and copies `dist/go-galaxy` to `/go-galaxy`. Build it once via `build_linux` first; the image is host-arch-agnostic only because the binary inside is linux/amd64.
- The same `dist/go-galaxy` path is reused by both `build` and `build_linux`. Don't intermix: a host build will be overwritten by a Linux build.

## Workflow

1. Confirm whether the user wants vendor/check/test mutation; if not, run `go build` manually.
2. Run the appropriate `just` target.
3. Verify artifact: `ls -lh dist/`.
4. For OCI: confirm image with `<executor> images | grep go-galaxy`.
