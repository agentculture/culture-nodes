# Build Plan — jira-driven idea-to-shipped loop

slug: `jira-driven-idea-to-shipped-loop` · status: `exported` · from frame: `jira-driven-idea-to-shipped-loop`

> A Jira issue moves to the right state and, with no operator shell command in the transcript, a PR appears against a feature branch whose every constituent task was dispatched, gated, merged and decided by the system - the idea-to-shipped loop runs as Culture Nodes flows and the combining step is a node instead of the operator

## Tasks

### t1 — Harvest node (#100): a code-node program that fetches the actor's handed-over ref through the internal/handover seam into the integration worktree - no operator ssh anywhere on the path

- covers: c3, h3
- acceptance:
  - Fetches a refs/culture-nodes/ ref from a control-plane-configured remote and stages it into a worktree, unit-tested against a fixture remote
  - Killed mid-run, the fetched work is preserved and recoverable from the run alone - no silent drop
  - No ssh invocation and no operator credential appears in the program or its tests

### t2 — Candidate staging + .github containment: stage the harvested package onto a candidate of the feature branch; a merge conflict lands its own domain outcome edge; a diff touching .github/ routes to a human regardless of verdict

- depends on: t1
- covers: c35, h20
- acceptance:
  - Conflicting package lands the conflict domain outcome, not an engine failure - fixture-tested
  - A fixture package touching .github/ parks at a human gate before merge; a green verdict cannot bypass it

### t3 — Gate the combination: point scripts/merge-gate.py at the candidate tree, aggregate via POST /runs/{id}/gate-reports; the verdict that authorizes a merge is measured on feature-branch + package, never the package alone

- depends on: t2
- covers: c27, h12, h2
- acceptance:
  - A wave-2 package green on its own branch but red in combination lands `changes_required` measured on the combination - a test constructs exactly that
  - The verdict is a derived record queryable from the ledger; a gate that could not measure lands `measurement_incomplete` (go-test all-skips trap covered by an explicit test)

### t4 — Merge execution with named credential custody: the merge node performs the --no-ff merge and pushes the feature branch from the #90 seam; the workflow/deploy docs name the host, checkout and credential

- depends on: t3
- covers: c33, h18
- acceptance:
  - The push succeeds from the loop's own custody with no operator credential dance
  - The credential appears in no committed artifact, argv, or log line - verified by test and by grep over the diff

### t5 — The combining-loop workflow: a committed yaml stringing harvest -> stage -> gate -> merge -> decide -> wave release, compiling with 0 errors on nodes validate, driven by hand-emitted events, reusing repair routing and pending-decisions as-is

- depends on: t4
- covers: c2
- acceptance:
  - nodes validate accepts the graph; every domain outcome (conflict, `changes_required`, `measurement_incomplete`, `gates_passed`) has an edge
  - A finished task's claim is surfaced on pending-decisions by the loop, and the next wave releases only once dependencies are satisfied

### t6 — Wave release consults pacing: dispatches released by the loop go through the shipped session-window arithmetic (internal/pacing) so a burst queues rather than overdraws the window

- depends on: t5
- covers: c34, h19
- acceptance:
  - A burst of releases beyond the window's remaining sessions queues the excess; the pacing decision is visible per dispatch
  - The window is never overdrawn in the burst test

### t7 — Self-verifying steps: each loop step records a measured post-condition as its evidence (ref present, worker started, actor reachable) instead of trusting exit 0

- depends on: t5
- covers: c9, h7
- acceptance:
  - At least one test drives a step whose command exits 0 while its verification fails, and the step reports failure
  - Each step's post-condition is readable from the run as evidence, not narrated

### t8 — Stage-1 demonstration: one real package goes dispatch -> gated -> merged -> decided through the combining loop, driven by hand-emitted events, with zero operator shell commands in the transcript

- depends on: t6, t7
- covers: c20, h27
- acceptance:
  - The demonstrating session's transcript shows no operator shell command between the hand-emitted event and the merged, decided package
  - A reader given only the run can reconstruct what merged, on what verdict, and who decided the claim

### t9 — Sweep evolution: distinct per-state-transition event names, and the self-echo filter (skip comments authored by the system's own Jira account when computing the resume event)

- covers: c8, h6, c28, h13
- acceptance:
  - A trigger subscribed to transition events never receives comment events - fixture-pinned
  - A sweep pass over an issue whose newest comment is the system's own emits nothing, while the watermark still advances

### t10 — The Jira comment actor: a new adapters bridge in the notify layout with exactly one verb (post a comment on a named issue), holding the only Jira write credential, with no transition code path

- covers: c29, h14, c5, h10
- acceptance:
  - An audit test asserts no code path reaches a transition endpoint
  - The sweep's runtime environment carries only the event-ingress token; the actor's credential appears in no control-plane config or committed artifact
  - The bridge follows the adapters/notify layout and registers as an ordinary actor

### t11 — The question round trip: a leg's question posts as a Jira comment via the actor, the flow parks on until.signal, and the answer's sweep event resumes it; watermark advance and event append stay one transaction; the resume event names the originating question

- depends on: t9, t10
- covers: c4, h4
- acceptance:
  - A restart between sweep passes cannot re-emit or skip an answer - transaction test
  - The resume event payload names the originating question id, and the consumer treats the dispatch as a continuation

### t12 — Session identity through the gap: the asking node's `session_key`/`continuation_ref` ride the parked state into the resume dispatch; warm resume when the provider session survives, otherwise a cold session with a recorded ForkEvent and the re-briefed question

- depends on: t11
- covers: c16, h22, c30, h15
- acceptance:
  - A prompt answer resumes with the original `continuation_ref` (bridge registry shows no ForkEvent)
  - An answer after forced session loss produces a cold session WITH a ForkEvent and a re-brief containing question + answer - neither path silent

### t13 — Claim decisions round-trip through Jira: the loop posts a pending decision as a comment naming the record id and accepted verbs; a conservative parser commits a review only on an exact verb+id reply, citing the Jira comment; anything else re-asks

- depends on: t11
- covers: c38, h23
- acceptance:
  - An ambiguous or partial reply commits nothing and produces a re-ask - fixture-tested
  - An exact-match reply commits a review record through the ledger's review route, and the record names the Jira comment it transcribed

### t14 — The bounded question loop: continue.while with a declared bound and backoff on re-asks; exhaustion routes to a human node via onExhausted, mirroring the repair bound's shape

- depends on: t11
- covers: c32, h17
- acceptance:
  - An unresolved question exhausts its bound and lands on a human node; the run shows the bound spent and the routing record
  - nodes validate refuses the workflow if the onExhausted outcome is unrouted

### t15 — One active run per issue: measure the trigger layer's dedup semantics, then implement the guard (engine-side if it exists, workflow-side if not) so a second event on an in-flight issue lands on the existing run

- depends on: t9
- covers: c31, h16
- acceptance:
  - Two events on one issue during a single flight yield exactly one active run in the run list
  - The second event's effect is visible on the existing run, not a sibling

### t16 — Concurrency policy (#166): configurable max simultaneous Jira-driven runs, per-machine limits (one ticket per machine), and tag-pinned items routed to their host - composing pacing and the placement registry, with per-issue dedup as the floor

- depends on: t15
- covers: c36, h21
- acceptance:
  - Two items proceed on two machines while a third queues, with max and per-machine limits read from configuration
  - A tag-pinned item queues for its machine rather than spilling elsewhere
  - Per-issue dedup still holds underneath - the t15 test keeps passing

### t17 — Stage-2 live proof on the seeded backlog: a real Jira state transition starts a run under its transition-specific event name, and the question round trip is shown live in both resume modes (warm, and forced-loss fork)

- depends on: t12, t13, t14, t15
- covers: c22, h29, c21, h28
- acceptance:
  - A real transition on the seeded backlog starts a run whose triggering event name is transition-specific, read from the run's event stream
  - Both resume paths are demonstrated live on a real issue: warm `continuation_ref` reuse, and a ForkEvent-carrying cold resume with the re-briefed question

### t18 — The spec chain as a graph (#89): deterministic devague moves as code nodes operating on the frame-as-artifact, judgement as agent nodes, human-inbox approval gates; outputs map through internal/devague into the run ledger

- covers: c6, h5, c11, h11
- acceptance:
  - A committed workflow expresses scope -> think -> challenge and compiles with 0 errors
  - One real frame produced through it is structurally indistinguishable from a hand-run one; deterministic moves land as derived records
  - An authority audit of the run shows every agent-origin record proposed and confirmations reachable only through the review transaction

### t19 — Registration residue (#8), take-or-defer: attempt runner-services live reload without a worker restart and namespace discovery on the API; whatever is not taken is deferred by a recorded scoping decision, not silence

- covers: c14, h8
- acceptance:
  - Either a runner-service change takes effect without a worker restart (test), or a recorded decision defers it with the reason
  - Existing actor-registration surfaces are reused unmodified - no parallel registration path appears

### t20 — PR wiring and the end-to-end demonstration: spec-PR and feature-branch-PR nodes (gated on all waves merged), the existing sweep picking the PR up, and one full cycle Jira -> PR with zero operator shell commands; the delivery record cites STATE.md s11 and the 2026-08-17 hand-turn counts as the before-state

- depends on: t8, t17, t18
- covers: c1, h1, c17, h24, c18, h25, c19, h26
- acceptance:
  - A reader given only the run reconstructs the cycle; every human act in it was a Jira reply
  - The transcript between the Jira state change and the PR contains zero operator shell commands
  - The operator merged nothing by hand; the fleet executed every dispatch; the committed workflows compile as loadable examples

## Risks

- [unknown_nonblocking] Engine trigger-dedup semantics unmeasured: t15 starts with a measurement; the guard's home (engine vs workflow) follows the finding (task t15)
- [unknown_nonblocking] Whether a signal-wait can also declare a timeout (until signal + duration together) is unsettled - decides the reminder/escalation shape on unanswered questions (task t14)
- [unknown_nonblocking] Jira Cloud behavior under real latency, permissions and rate limits has never been live-proven; the backlog was empty for every probe. Live proof depends on the user seeding SCRUM (decision c26) - external precondition (task t17)
- [unknown_nonblocking] Jira priority names vs the unified `_SEVERITY_RANK`: whether #106 joined the vocabulary or kept a Jira-local ordering - affects mixed-source triage priority (task t9)
- [unknown_nonblocking] development-loop's remaining gaps (provisioner wiring, worktree-event vocabulary) were not re-measured; t1/t5 must re-verify which still hold at HEAD before reusing that skeleton (task t1)
