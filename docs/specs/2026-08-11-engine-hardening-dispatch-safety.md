# engine-hardening-dispatch-safety

> Actor dispatch is safe to leave unattended: terminal-commit failures are visible in events and logs, a failed commit never permanently consumes the callback sequence, actor re-dispatch has a bounded retry budget that parks the node as failed with a recorded cause, and run cancellation reaps waiting and leased work items so a cancelled run can never dispatch again
> instruction: Verify by regression: reproduce both 2026-08-11 incidents as store/engine tests (pgtest pattern), then re-run the two-node smoke lane live and confirm a forced failure parks at attempt 3 with a visible cause instead of looping

## Audience

- The operator delegating billable agent work, both production workers, and anyone reading the events/ledger surface to understand why a node is not progressing

## Before → After

- Before: Measured 2026-08-11: a deterministic terminal-commit error is invisible (no log, no event), permanently blocks its attempt via the pre-advanced sequence ratchet, and the work item re-dispatches a fresh billable session every ~80s without bound; run cancellation leaves waiting/leased items dispatching (attempt 22 observed); ~25 sessions were burned across two incidents before manual DB surgery
- After: A terminal-commit failure is visible within one event and one log line, its redelivered terminal callback can still commit once the cause clears, a node run stops after 3 dispatch attempts parking as failed with a recorded cause, cancelling a run reaps its work items and SIGTERMs in-flight sessions, and no work item of a terminal run can ever be leased again

## Why it matters

- Every delegation surface (codex quota today, AWS Lambda credits next) sits on this dispatch loop — until failures are bounded and visible, any config mistake converts directly into unbounded spend, which is the single biggest blocker to leaving the system unattended

## Requirements

- Terminal-commit errors become observable: when commitTerminal returns a non-refusal error (internal/actors/callback.go — ResumeWaitingWork, Engine.CompleteAttempt, CloseInvocation error returns), the failure is recorded as an event on the attempt/node-run aggregate via deps.record AND logged server-side — today the error rides only the HTTP response to the bridge and prod-api-1 logs nothing (measured live, issue #16)
  - honesty: A commitTerminal error leaves exactly one recorded event on the attempt's aggregate (kind naming the failed commit and the error class) and one server log line — verified by unit test asserting deps.record and by grepping the API container log in the live check
- A failed terminal commit never permanently consumes the sequence: handleClaimed advances the ratchet (AdvanceCallbackSequence) BEFORE terminal processing, and the error path releases only the event-id claim — the same-id/same-sequence redelivery §13.4 mandates is then rejected out-of-order forever (measured live: every retry of `evt_8` refused against last=8). Fix by advancing after successful terminal processing, or accepting an equal sequence for an event id whose claim was released
  - honesty: After a terminal commit fails, redelivering the SAME event id and sequence commits successfully once the underlying cause is fixed — no operator surgery, no new invocation; the §13.4 monotonic-sequence contract for accepted streams is unchanged (conformance kit green)
- Actor re-dispatch gets a bounded retry budget: `work_items`.attempt already counts dispatches (reached 22 live) but nothing caps it — the claim/reclaim path and worker dispatch must park the node run as failed with a recorded cause once the budget is exhausted, instead of looping a billable session per lease cycle (issue #16)
  - honesty: The 4th dispatch attempt of a node run never happens: the item parks after 3 with technical status failed and the exhaustion cause in the attempt result; a workflow with a failure edge routes it, one without ends the run failed — never a hang
- Run cancellation reaps waiting and leased work items: internal/api/runs.go cancelRun cancels only 'ready' rows by design, documenting the assumption that a leased item's completion no-ops — but the late/superseded path releases the item back to retry instead of completing it, so a cancelled run's actor node re-dispatched every ~80s until a manual UPDATE (measured live, issue #19); cancelRun already holds the run advisory lock, so extending its UPDATE to waiting/leased rows is race-safe
  - honesty: After cancelRun commits, zero work items of that run are in a leasable state, and every pending invocation received a best-effort actors Cancel (SIGTERM at the bridge) — verified by test asserting item states + a recorded cancel-request event per invocation
- Defense in depth independent of cancel: the work-item claim/reclaim path refuses to lease an item whose run is already terminal (runs.status join), so no future terminal-run path can leak a dispatching zombie — the cancel fix alone would not have covered a run that reached 'failed' with an in-flight item (also measured live: re-dispatches continued after the smoke run failed)
  - honesty: The claim/reclaim SQL refuses items whose run status is terminal regardless of how the run got there — covered for cancelled AND failed runs (the second live incident's shape)
- Cancel propagation needs the API service to reach bridges: cancelRun runs in the api container, which today carries NO actor token envs (compose.thor.yml grants `NODES_ACTOR_`\* to the worker service only — probed live) — the api service env gains both codex token vars (and future actor tokens), and the api process reuses the worker's DBRegistry resolution to build authenticated Cancel calls
  - honesty: A live cancel of a run with an in-flight codex session results in the bridge logging the cancel and the session terminating early — proving the api container's tokens and registry wiring actually work, not just compile
- The API gains a logging facility: internal/api/server.go contains zero log calls today (probed — prod-api-1 logs only its startup line), so c2's 'one server log line per terminal-commit failure' requires introducing a logger to the API/callback path, not just adding one call
  - honesty: Every 5xx the API returns carries a corresponding log line with the error chain — verified for the callback path by test and for the live deployment by inspecting prod-api-1 logs during the acceptance check
- Budget exhaustion also best-effort Cancels the in-flight invocation (same actors.Client.Cancel lane as cancelRun) — parking the node while a session still runs would otherwise let it finish into a discarded late callback, spending quota on a result nobody can use
  - honesty: The budget-exhaustion test asserts a Cancel was issued for the pending invocation alongside the parking completion

## Honesty conditions

- Both live incident shapes are reproduced as failing tests before the fixes and pass after — the FK-driven commit failure commits on redelivery, and the cancelled-run zombie can no longer lease
- adapters/\*\* and tests/conformance are byte-unchanged; the conformance kit runs green against an unmodified bridge after the engine changes
- No new UPDATE/DELETE statements touch actors, `ledger_records`, or attempts; retry-budget parking flows through the existing CompleteAttempt/transition vocabulary
- internal/queue/sqs is byte-unchanged this cycle; if the claim contract changes shape, a follow-up lands on issue #7 naming the divergence
- New store tests are namespace-scoped following the cleaner existing pgtest suites, adding no new cross-contamination of the kind issue #9 tracks
- The failure surfaces land where each audience already looks: events (UI/API consumers), server logs (operator), attempt result records (ledger readers)
- The before-state is the recorded 2026-08-11 forensics (issues #16/#19 and delivery doc evidence), not reconstruction
- Each after-state clause maps to at least one success-signal regression test — no clause ships untested
- The budget applies to actor dispatch generically — the same path a future Lambda-backed actor or runner-service dispatch retries through — not a codex-specific patch
- The four success-signal scenarios exist as named tests that fail on pre-fix code — verified by running them against a pre-fix checkout at review time
- The regression test for the failed-commit incident asserts the work item's state sequence explicitly (waiting -> leased-by-resume -> not looping after the fix), proving the mechanism, not just the outcome
- A store test proves a 'waiting' item's attempt counter is unchanged across ReclaimExpired sweeps and lease-duration passage while waiting — only a genuine re-claim increments it

## Success signals

- Regression tests reproduce both live incidents and pass: (1) a terminal commit that fails once commits successfully on redelivery after the cause clears, emitting a recorded failure event; (2) a node run whose dispatches fail 3 times parks as failed with the exhaustion cause in its attempt record; (3) cancelRun on a run with waiting+leased items leaves zero leasable items and issues actor Cancel for pending invocations; (4) the claim path refuses items of terminal runs

## Scope / boundaries

- The actor protocol wire contract is unchanged: bridges (adapters/\*) are not modified, callback event shapes/sequencing rules the bridges implement stay as §13.4 specifies, and the tests/conformance kit must stay green unmodified — the fixes are engine-side ingest/scheduling semantics only
- The ledger authority model and append-only records are untouched: no UPDATE/DELETE paths are added to actors, `ledger_records`, or attempts; parking an exhausted node run flows through the existing engine completion/transition vocabulary (technical status, not a new state)
- The SQS queue driver (internal/queue/sqs, fake-tested, deferred AWS lane #7) is out of scope: claim-semantics changes land in the postgres driver/store path production uses; if the claim contract itself changes shape, the SQS driver gets a tracked follow-up on #7, not a silent parallel edit

## Assumptions

- Store-level tests for these paths follow the existing pgtest pattern (internal/store/postgres testmain) and need a live Postgres; issue #9's namespace cross-contamination applies, so new suites must be namespace-scoped like the better existing ones
- `work_items`.attempt increments only on genuine claim/dispatch (the claim SQL), never while an item sits healthily in 'waiting' — ReclaimExpired touches only expired 'leased' rows — so budget-3 cannot kill a healthy long-running session that heartbeats through its callbacks

## Scope exploration

- `s1` — `internal/actors/callback.go (HandleCallback ladder, handleClaimed, commitTerminal, late)`: the exact ingest order that ratchets-then-fails is here (step 4 advance precedes terminal processing; error path releases only the id claim); commitTerminal's three error returns are the invisible-failure sites; the late path closes the invocation but never completes the work item
  - seeds: `c2`, `c3`, `c5`
- `s2` — `internal/store/postgres/async.go (ClaimCallbackEvent/ReleaseCallbackEvent/AdvanceCallbackSequence, idempotency_keys callbackScope)`: event-id claims live in `idempotency_keys` namespaced per attempt; the sequence ratchet is a separate column on `actor_invocations` — accepting an equal sequence for a released id, or advancing post-processing, are both implementable here without wire changes
  - seeds: `c3`
- `s3` — `internal/api/runs.go cancelRun`: single-transaction cancel under the run advisory lock; its own comment documents leaving leased items alone on the assumption the fenced completion no-ops — the assumption the live zombie disproved for the re-dispatch side; extending the `work_items` UPDATE to waiting/leased is race-safe under the same lock
  - seeds: `c5`
- `s4` — `internal/worker (dispatch.go StartAsyncWait, worker.go deadlineFor, complete) + work_items schema`: `work_items`.attempt counts dispatches with no cap; the claim/reclaim SQL is the site for both the run-terminal guard and the budget; fencing tuple flows from the claim so parking-on-exhaustion composes with existing completion
  - seeds: `c4`, `c6`
- `s5` — `live forensics 2026-08-11 (runs 01KZS4EYR5/01KZS5TJE0, actor_invocations attempts 20-22, events out-of-order rows)`: both failure loops measured in production: deterministic FK error looped ~12 sessions pre-cancel; cancellation left the item cycling to attempt 22 post-cancel; re-dispatches also continued after a DIFFERENT run reached failed — grounding c2-c6
  - seeds: `c2`, `c3`, `c4`, `c5`, `c6`
- `s6` — `internal/queue/sqs + tests/conformance`: SQS driver is fake-tested and deferred (#7) — explicitly fenced out; the actor-protocol conformance kit pins the wire contract the fixes must not move
  - seeds: `c7`, `c9`
- `s7` — `challenge pass / adjacent-systems lens: internal/worker/runnerasync.go + store runnerasync (runner protocol ingest)`: the runner protocol's callback ingest is a separate path where polling is authoritative (phase-2 d-decision) — a failed runner-callback commit is recovered by the poll loop, so the actor-side ratchet trap has no direct twin; verify during implementation, recorded here as examined-with-caveat
- `s8` — `challenge pass / failure-mode lens: internal/store/postgres/claiming.go ReclaimExpired + commitTerminal resume ordering`: resume-before-complete plus lease expiry is the exact loop motor: ReclaimExpired flips only expired 'leased' rows, and the failed commit leaves the item exactly there — seeded c16 and grounds c4's budget placement in the claim path
  - seeds: `c16`, `c20`
- `s9` — `challenge pass / operations lens: compose.thor.yml api service env (probed live)`: api container has no actor token envs — cancel propagation from cancelRun cannot authenticate to bridges without the compose change; seeded c17
  - seeds: `c17`
- `s10` — `challenge pass / observability lens: internal/api/server.go logging`: zero log statements exist in the API server — the invisible-failure problem is broader than the callback path; c18 introduces the facility
  - seeds: `c18`
- `s11` — `challenge pass / lifecycle lens: budget exhaustion vs in-flight sessions`: parking at budget while a session runs would discard its eventual result as late — seeded the symmetric best-effort Cancel (c19)
  - seeds: `c19`

## Decisions

- The zombie re-dispatch cadence is mechanically explained: commitTerminal resumes the work item (ResumeWaitingWork re-leases it) BEFORE CompleteAttempt; on commit failure the item stays leased-but-incomplete, the 1m lease expires, ReclaimExpired (claiming.go) flips it ready, and the loop repeats — the c3/c4 fixes must account for this exact sequence (e.g. failed commit un-resumes or the budget catches the reclaim)

## Open parks

- [unknown_nonblocking] the 180s attempt timeout observed in run 01KZS5TJE0ZR7JZEARKCM96KHS (zero callbacks ingested for the node's own attempt while the zombie item competed) is not fully explained — deadlineFor uses node.Timeout (15m in that workflow) or DefaultTimeout; diagnosing needs #16's observability landed first, then a reproduction
- [unknown_nonblocking] whether the runner-protocol path needs an equivalent retry budget (runner operations are compute, not billed per-invocation like agent sessions) — decide when the runner path is next touched, not in this cycle
