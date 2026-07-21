---
description: Run the full go-galaxy static-analysis gate (just check) and summarize results.
allowed-tools: Bash(just check), Bash(go vet *), Bash(go tool staticcheck *), Bash(go tool govulncheck *), Bash(go tool fieldalignment *)
---

Run the project's static-analysis gate and report the outcome.

Output of `just check` (vet, staticcheck, govulncheck, fieldalignment; this also
re-syncs `vendor/` via `just deps`):

!`just check`

Summarize: which analyzers passed, any findings with file:line, and the single
next action. If a gate failed, do not move on - propose the minimal fix. If you
only need analysis without `vendor/` churn, note that the direct
`go tool ...` equivalents (see the `go-galaxy-check` skill) skip `just deps`.
