---
description: Run the full go-galaxy static-analysis gate (just check) and summarize results.
allowed-tools: Bash(just check), Bash(go vet *), Bash(go tool staticcheck *), Bash(go tool govulncheck *), Bash(go tool fieldalignment *)
---

Run the project's static-analysis gate and report the outcome.

Output of `just check` (vet, staticcheck, govulncheck, fieldalignment; it
mutates nothing and does not re-sync `vendor/`):

!`just check`

Summarize: which analyzers passed, any findings with file:line, and the single
next action. If a gate failed, do not move on - propose the minimal fix. A
failure reading `inconsistent vendoring` is not an analyzer finding: it means
`vendor/` no longer matches `go.mod`, and the fix is `just deps`.
