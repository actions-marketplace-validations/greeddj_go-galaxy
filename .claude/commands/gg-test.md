---
description: Run go-galaxy tests with the race detector. Pass a package path or -run regex as arguments to scope the run; no argument runs the whole suite.
argument-hint: "[./internal/galaxy/<pkg>/... | -run TestName]"
allowed-tools: Bash(just test), Bash(go test *)
---

Run the tests for the scope the user gave: `$ARGUMENTS`

- If `$ARGUMENTS` is empty, run the full suite:

  !`just test`

- If `$ARGUMENTS` names a package and/or `-run` regex, run the focused race
  build instead (do this yourself with the arguments substituted in):
  `go test $ARGUMENTS -race -v`

Report which cases passed and failed; for each failure give the assertion, the
likely cause, and the next step. Keep the race detector on for focused runs.
