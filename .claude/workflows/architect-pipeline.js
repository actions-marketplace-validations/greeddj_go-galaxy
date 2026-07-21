export const meta = {
  name: 'architect-pipeline',
  description: 'Architect-gated delivery pipeline for go-galaxy: the go-architect agent designs and gates, then developer -> tester -> security -> tech-writer execute, with the architect reviewing each return and signing off.',
  phases: [
    { title: 'Design', detail: 'architect reviews the task and approves/rejects', model: 'opus' },
    { title: 'Develop', detail: 'developer implements; architect reviews until accepted', model: 'sonnet' },
    { title: 'Test', detail: 'tester runs suites + coverage; architect reviews', model: 'sonnet' },
    { title: 'Security', detail: 'security audits; architect reviews', model: 'opus' },
    { title: 'Docs', detail: 'tech-writer documents the change', model: 'sonnet' },
    { title: 'Sign-off', detail: 'architect hands the result back to the main thread', model: 'opus' },
  ],
}

// This script is the orchestrator the agents cannot be (Claude Code subagents
// cannot call each other). It threads each agent's report to the next stage and
// lets the architect gate every transition. Run it with:
//   Workflow({ scriptPath: '.claude/workflows/architect-pipeline.js', args: { task: '<detailed task>' } })
// args may be a plain string or { task }.

const task =
  typeof args === 'string' ? args : args && args.task ? args.task : null

if (!task) {
  log('No task provided. Pass the task as args: a string, or { task: "..." }.')
  return { status: 'no-task' }
}

const MAX_REWORK = 3

const DESIGN_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['verdict', 'summary', 'instructions'],
  properties: {
    verdict: { type: 'string', enum: ['approved', 'rejected', 'needs-info'] },
    summary: { type: 'string', description: 'One paragraph: the decision and why.' },
    instructions: {
      type: 'string',
      description:
        'If approved: precise, numbered implementation instructions for the developer. If rejected: the blocking reasons. If needs-info: the questions for the main thread.',
    },
  },
}

const REVIEW_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['verdict', 'notes'],
  properties: {
    verdict: { type: 'string', enum: ['accept', 'rework'] },
    notes: {
      type: 'string',
      description:
        'If accept: a short rationale. If rework: precise notes for the developer on exactly what to change.',
    },
  },
}

// One review-and-maybe-rework round driven by the architect. Returns the final
// (possibly reworked) developer report, or null if rework was exhausted.
async function reviewAndRework(stageLabel, priorReport, originExpert) {
  const review = await agent(
    `Review the ${originExpert} report against the original task and go-galaxy invariants. Run gates if you need to. Accept only if the change is correct, idiomatic, efficient, complete, and properly tested; otherwise return precise rework notes for the developer.\n\nTASK:\n${task}\n\n${originExpert.toUpperCase()} REPORT:\n${priorReport}`,
    { agentType: 'go-architect', phase: stageLabel, schema: REVIEW_SCHEMA, label: `architect:review-${stageLabel.toLowerCase()}` },
  )
  if (!review || review.verdict === 'accept') return { accepted: true, review }

  log(`Architect requested rework after ${stageLabel}.`)
  const fix = await agent(
    `Apply this rework from the architect (raised after ${stageLabel}). Change only what is asked, then re-run the gates that cover it and report.\n\nTASK:\n${task}\n\nREWORK NOTES:\n${review.notes}\n\nCONTEXT (the report that triggered this):\n${priorReport}`,
    { agentType: 'go-developer', phase: 'Develop', label: `developer:fix-after-${stageLabel.toLowerCase()}` },
  )
  return { accepted: false, review, fix }
}

// 1. Design gate - the architect can stop the whole thing here.
phase('Design')
const design = await agent(
  `The main thread proposes this task for go-galaxy. Review it as the architect. Reject it if it is under-specified, architecturally wrong, adds an unjustified dependency, or is not worth its complexity. If it is sound, return precise implementation instructions for the developer.\n\nTASK:\n${task}`,
  { agentType: 'go-architect', phase: 'Design', schema: DESIGN_SCHEMA, label: 'architect:design' },
)
log(`Architect design verdict: ${design.verdict}`)
if (design.verdict !== 'approved') {
  return { status: design.verdict, architect: design }
}

// 2. Develop + architect review loop until accepted or exhausted.
phase('Develop')
let devInstructions = design.instructions
let devReport = null
let developAccepted = false
for (let i = 0; i < MAX_REWORK; i++) {
  devReport = await agent(
    `Implement this task per the architect's instructions. Write the code, English doc comments, and unit tests. Run the gates that cover your change, fix what you find, then report.\n\nTASK:\n${task}\n\nARCHITECT INSTRUCTIONS / REWORK NOTES:\n${devInstructions}`,
    { agentType: 'go-developer', phase: 'Develop', label: `developer:impl-${i + 1}` },
  )
  const review = await agent(
    `Review the developer's implementation against the task and go-galaxy invariants. Run gates if needed. Accept only when it is correct, idiomatic, efficient, and complete; otherwise return precise rework notes.\n\nTASK:\n${task}\n\nDEVELOPER REPORT:\n${devReport}`,
    { agentType: 'go-architect', phase: 'Develop', schema: REVIEW_SCHEMA, label: `architect:review-dev-${i + 1}` },
  )
  if (!review || review.verdict === 'accept') { developAccepted = true; break }
  log(`Architect requested developer rework (round ${i + 1}).`)
  devInstructions = review.notes
}
if (!developAccepted) {
  return { status: 'develop-rework-exhausted', task, lastDevReport: devReport }
}

// 3. Test stage - tester runs, architect gates one rework round.
phase('Test')
const testReport = await agent(
  `Verify the implemented change. Run the relevant unit / integration tests with -race, audit coverage, and report which behaviors are covered, which fail, and which gaps need a test or a justified "not tested" note.\n\nTASK:\n${task}\n\nIMPLEMENTATION SUMMARY:\n${devReport}`,
  { agentType: 'go-tester', phase: 'Test', label: 'tester:run' },
)
const afterTest = await reviewAndRework('Test', testReport, 'tester')

// 4. Security stage - security audits, architect gates one rework round.
phase('Security')
const securityReport = await agent(
  `Audit the implemented change for security: untrusted-archive handling, network/API surface, host harm, concurrency-as-vulnerability, and reachable dependency CVEs (run govulncheck). Trace untrusted input end to end across layers, not just the changed file. Report findings by severity.\n\nTASK:\n${task}\n\nIMPLEMENTATION SUMMARY:\n${devReport}`,
  { agentType: 'go-security', phase: 'Security', label: 'security:audit' },
)
const afterSecurity = await reviewAndRework('Security', securityReport, 'security')

// 5. Docs stage - tech-writer documents and enforces English-only.
phase('Docs')
const docsReport = await agent(
  `The implementation, tests, and security audit are accepted. Audit comments and doc comments for correctness and presence, enforce English-only prose, and update README.md / CLAUDE.md / package docs to match the change. Do not change program logic.\n\nTASK:\n${task}\n\nIMPLEMENTATION SUMMARY:\n${devReport}`,
  { agentType: 'go-techwriter', phase: 'Docs', label: 'techwriter:docs' },
)

// 6. Sign-off - architect summarizes the whole delivery for the main thread.
phase('Sign-off')
const signoff = await agent(
  `Produce the final sign-off for the main thread. Confirm the task is delivered to the "it will not get better than this" bar, or list what still blocks it. Summarize the change, the test/coverage state, the security verdict, and the docs state.\n\nTASK:\n${task}\n\nDEVELOPER:\n${devReport}\n\nTESTER:\n${testReport}\n\nSECURITY:\n${securityReport}\n\nTECH-WRITER:\n${docsReport}`,
  { agentType: 'go-architect', phase: 'Sign-off', label: 'architect:sign-off' },
)

return {
  status: 'done',
  design,
  developer: devReport,
  tester: { report: testReport, review: afterTest.review },
  security: { report: securityReport, review: afterSecurity.review },
  techwriter: docsReport,
  signoff,
}
