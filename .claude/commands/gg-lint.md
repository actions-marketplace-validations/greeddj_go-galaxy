---
description: Run golangci-lint over the go-galaxy module (just lint) and summarize findings.
allowed-tools: Bash(just lint), Bash(golangci-lint *)
---

Run the linter and report the outcome.

Output of `just lint`:

!`just lint`

Summarize the findings grouped by linter, each with file:line, and propose the
minimal fix for each. Remember the depguard allowlist: a new third-party import
needs an entry in `.golangci.yml` under
`linters.settings.depguard.rules.main.allow`.
