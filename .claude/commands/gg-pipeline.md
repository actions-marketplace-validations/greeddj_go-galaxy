---
description: Drive the architect-gated delivery pipeline (architect -> developer -> tester -> security -> tech-writer) for a non-trivial go-galaxy task.
argument-hint: "<detailed task description>"
---

Run the multi-agent delivery pipeline for this task:

$ARGUMENTS

## How it works

Claude Code subagents cannot call each other, so the **main thread** (you) is the
router. The `go-architect` agent is the brain: it designs, gates every stage, and
can reject a weak request outright. There are two ways to run the pipeline.

### Preferred: the orchestrated workflow

Call the `Workflow` tool with the saved script, passing the task as args. This
runs the full architect-gated loop deterministically and returns a structured
sign-off:

- script: `.claude/workflows/architect-pipeline.js`
- args: `{ "task": "<the full task above, with all detail and constraints>" }`

Before launching, make sure the task is specified well enough that the architect
will not bounce it back as `needs-info`: goal, scope, affected packages,
constraints, and the definition of done. If `$ARGUMENTS` is thin, ask the user
the missing questions first, then launch.

The workflow stops early and reports back if the architect returns `rejected` or
`needs-info` - relay that to the user verbatim; do not try to overrule the
architect.

### Manual fallback (no workflow)

Drive the same flow by hand through the Agent tool, one stage at a time:

1. `go-architect` - hand it the task. If `approved`, take its developer
   instructions; if `rejected`/`needs-info`, relay to the user and stop.
2. `go-developer` - implement per those instructions; collect its report.
3. `go-architect` - review the developer report; loop back to step 2 on `rework`.
4. `go-tester` - run suites + coverage; then `go-architect` reviews; loop to the
   developer on `rework`.
5. `go-security` - audit; then `go-architect` reviews; loop to the developer on
   `rework`.
6. `go-techwriter` - comments + docs + English-only pass.
7. `go-architect` - final sign-off; report the result to the user.

Use the quick gate commands `/gg-check`, `/gg-lint`, `/gg-test` and the
`go-galaxy-coverage` skill between stages as needed.
