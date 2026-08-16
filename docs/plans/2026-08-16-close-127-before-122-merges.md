# Build Plan — close #127 before #122 merges

slug: `close-127-before-122-merges` · status: `exported` · from frame: `close-127-before-122-merges`

> Issue #127 closed out: PR #122 is green and merged into a clean main, the two intermittent tests are deterministic rather than re-run, and every item #127 listed is either done, decided by the owner, or a tracked successor with a named owner — nothing left as an unsorted handover blob.

## Tasks

### t1 — Run the falsification probe: shared vs per-package test database

- instruction: Probe script and logs are in the session scratchpad (probe.sh, `a_`\*/`b_`\*.log). Arm A points both a full 'go test ./...' and a concurrent '-count=5' focused run at ONE scratch postgres (docker nodes-probe-pg, port 55432); arm B leaves `NODES_TEST_DATABASE_URL` unset so pgtest starts a private container per package. Note the amplification honestly: CI runs one sweep, this runs two concurrent invocations, so a reproduction evidences the MECHANISM and is not a measurement of CI's rate.
- covers: c29, h21, c23, h16
- acceptance:
  - go test ./internal/queue/sqs/ ./internal/scheduler/ -count=5 is run TWICE: once with `NODES_TEST_DATABASE_URL` set to one scratch database while go test ./... runs concurrently against that same database, and once with the variable unset (per-package private containers)
  - At least one of the two named tests reproduces its failure under the shared database, and neither reproduces under private databases — or the opposite is observed and recorded as refuting c23/c24
  - The result is posted to #126 either way, including a negative result
  - The probe's own amplification is stated in the #126 write-up: arm A ran two concurrent go test invocations where CI runs one, so it evidences the mechanism and does not measure CI's failure rate

### t2 — Re-read both CI failure logs and confirm no third red hides behind the two

- instruction: Read-only. gh run view 31934728992 --log-failed (Go/build) and gh run view 31934729001 --log-failed (Tests/go). Do not stop at the first FAIL line — read to the end of each job.
- covers: c2, h1
- acceptance:
  - gh run view 31934728992 --log-failed and 31934729001 --log-failed are both read in full
  - A written statement records that TestSchedulerDeadlineFailsAParkedRunnerOperation and TestChaosDroppedSendRepairedByOutboxRelay are the only failures in their respective jobs, or names any third failure found

### t3 — Disposition the four Medium Qodo findings in writing

- instruction: Anchors: .claude/skills/nodes-operator/scripts/nodes-op.sh:121, internal/handover/git.go:134, deploy/prod/deploy.sh:43, internal/store/postgres/`inbound_transport.go`:84. Check the nodes-op.sh one against docs/skill-sources.md and CLAUDE.md FIRST — CLAUDE.md states nodes-operator is first-party and authored here, not vendored, so this finding is likely a false positive. Reply via the cicd skill's pr-reply.sh so the signature is appended automatically.
- covers: h4
- acceptance:
  - Each of the four Mediums (nodes-op.sh:121, handover/git.go:134, deploy.sh:43, `inbound_transport.go`:84) receives a written disposition: fixed, tracked as issue N, or rejected with a stated reason
  - The 'Vendored nodes-op.sh edited' finding is checked against docs/skill-sources.md and CLAUDE.md, and accepted or rejected on that evidence rather than assumed
  - No finding is batch-dismissed without a reason

### t4 — Fix the High Qodo finding: nil-wrapped dial-in error

- instruction: internal/actors/client.go:266. Write the failing test first — it must fail against unfixed code — then fix. This is the only High/Action-required finding on #122.
- covers: c8
- acceptance:
  - internal/actors/client.go:266 no longer returns a nil-wrapped error; the failure path returns a non-nil error carrying the dial-in cause
  - A Go test covers the previously nil-wrapped path and fails against the unfixed code

### t5 — Record the `SONAR_TOKEN` naming resolution; change nothing in-repo

- instruction: No repo edit. Evidence only: grep tracked files for `SONAR_CLOUD_SWEEP` (expect zero hits) and note `SONAR_TOKEN`'s real uses at examples/pr-upkeep/sweep.py:594 and .github/workflows/tests.yml:19.
- covers: c9, h5
- acceptance:
  - A fresh grep for `SONAR_CLOUD_SWEEP` across tracked files returns nothing, evidencing that no in-repo change is needed
  - The closure record states `SONAR_TOKEN` is the surviving name and the operator .env is the side that changes

### t6 — Open the post-merge successor issue and link it from #127

- instruction: Open this issue BEFORE closing #127 — c6's ordering is load-bearing. Carry: redeploy thor from main, redeploy both codex bridges (copied installs, stale silently per #120), delete ~/nodes-predeploy-20260816T064724Z.dump once stable. State the squash consequence explicitly.
- covers: c6, h10, c26, h19
- acceptance:
  - A new issue exists carrying the thor redeploy from main, both codex-bridge redeploys, and the pre-deploy dump deletion
  - It states the squash-merge consequence explicitly: 431528d will NOT be an ancestor of main, so prod is verified by GET /v1alpha1/version reporting main HEAD sha after redeploy, never by asking whether main contains 431528d
  - It is linked from #127 before #127 is closed

### t7 — Make the scheduler deadline test wait on the transition it asserts

- instruction: internal/scheduler/`runnerdeadline_test.go`. The race is real and located: scheduler.go:658 CompleteAttempt commits the node run, scheduler.go:694 closeWait commits the runner operation, in SEPARATE transactions. Do not widen a timeout — wait on the runner-operation transition itself. Prove by construction with a temporary sleep between the two commits.
- depends on: t1
- covers: c3, h2, c14, h11
- acceptance:
  - internal/scheduler/`runnerdeadline_test.go` no longer asserts the runner-operation state immediately after waiting only on the node run; it waits explicitly, with a bounded deadline, for RunnerOperationCompleted
  - Proven by construction: with a temporary sleep inserted between CompleteAttempt (scheduler.go:658) and closeWait (scheduler.go:694), the OLD assertion fails on every run and the NEW one still passes; the sleep is then removed
  - A comment above the test names the two-transaction root cause and cites #126
  - No t.Skip, no retry loop, and no reduction in -count is introduced

### t8 — Make the chaos relay test robust to foreign sweepers

- instruction: internal/queue/sqs/`chaos_test.go`. Do NOT add a namespace filter to internal/events/relay.go — deployment-wide draining is intended (resolved question q1). If t1 confirms the shared-database cause, the honest fix may be isolation rather than a drain loop; let t1's result choose.
- depends on: t1
- covers: c4, h3, c24, h17
- acceptance:
  - internal/queue/sqs/`chaos_test.go` no longer depends on the repaired row appearing in a single Receive(ctx,10,0) page
  - Proven by construction: seeding pending outbox rows in a second namespace before the repair run makes the OLD assertion fail deterministically and the NEW one pass
  - The contamination is demonstrated concretely at least once — a foreign relay observed marking this test's outbox row published while its own Receive returns nothing
  - internal/events/relay.go is NOT given a namespace filter; deployment-wide draining stays intended behaviour per resolved question q1
  - A comment above the test names the shared-database root cause and cites #126

### t9 — Match CI test-database isolation to local isolation

- instruction: GATED ON t1. If the probe refutes c23/c24, close this task as not-needed and record the evidence — do not implement it anyway. If confirmed: .github/workflows/tests.yml:93 is where the single shared URL is set; internal/store/postgres/pgtest/pgtest.go:50-62 is the per-package fallback. Keep the 'database-backed tests actually ran' guard passing — isolation must not turn Postgres tests into skips.
- depends on: t1
- covers: c23, h16
- acceptance:
  - Applied ONLY if t1's probe confirms the shared-database cause; if the probe refutes it, this task is closed as not-needed with the evidence recorded
  - Each package gets its own database or schema in CI, so that pgtest's per-package isolation and CI isolation agree
  - The database-backed-tests-actually-ran guard in tests.yml still passes — isolation must not turn Postgres-backed tests into skips
  - Both previously flaking tests pass under the full parallel sweep after the change
  - The fix is verified against the FULL class the probe surfaced, not just the two tests #126 names: TestChaosDroppedSendRepairedByOutboxRelay, TestSchedulerDeadlineTimeoutIsNotRetriedIntoASecondSession, TestSchedulerStandbyTakesOverWhenActiveLosesItsConnection, TestSchedulerTickIsBoundedByNoUnreachableDeadlineBridge, TestSchedulerDeadlinePausesWhenDeclaredContinuationHolds, and internal/engine's TestExhaustedRetriesFailTheRun
  - Re-running the probe's arm A (one shared database) after the fix produces zero failures across the class, matching arm B's clean baseline

### t10 — Gate the diff against retries, skips and count reductions

- instruction: Read-only grep over the changed test files for t.Skip, retry loops, and -count reductions. #126 states plainly that quietly adding a retry is not acceptable; this task is the enforcement of that.
- depends on: t7, t8, t9
- covers: c5, h9
- acceptance:
  - Grepping the changed test files shows no t.Skip, no retry loop, and no reduction in -count
  - If any such construct is present, the change is rejected rather than explained

### t11 — Verify the fixes under the contention condition that produced the failures

- instruction: Run in the contention condition, not isolation. Serialize this against t12 despite the same wave — both saturate the machine and share the test database, and running them together would manufacture the very contention being measured.
- depends on: t7, t8, t9
- covers: c18, h15
- acceptance:
  - go test ./internal/scheduler/ ./internal/queue/sqs/ -count=5 passes while a full go test ./... sweep runs concurrently against the same database
  - The run is done in the contention condition, not in isolation, and the command plus result are recorded
  - The contention run covers the wider class the probe surfaced, not only internal/scheduler and internal/queue/sqs — internal/engine is included

### t12 — Run the full local gate and bump the version

- instruction: Run CLAUDE.md's commands verbatim, including BOTH adapter lint styles: codex from the repo root (root config applies) and claude-code from inside its own directory (adapter config applies). Skipping the second is what took #122 red on three lint jobs (#123). Confirm the version number with the owner before bumping — the merge publishes it to real PyPI irreversibly.
- depends on: t4, t7, t8, t9
- covers: c13, h7, c25, h18
- acceptance:
  - uv run pytest -n auto passes
  - The four root lint commands pass: black, isort, flake8, bandit
  - BOTH adapter lint invocation styles pass exactly as CLAUDE.md spells them out — root-config codex paths AND the adapter-dir claude-code form
  - uv run teken cli doctor . --strict passes
  - The version in pyproject.toml is bumped, and the number is confirmed with the owner as the intended public PyPI release, since merging publishes it irreversibly
  - The CI result after push matches the local result

### t13 — Open or update an issue for every hand-turn in this session

- instruction: Per CLAUDE.md's every-operator-action rule. Cross-reference #123/#124/#127 rather than duplicating them. Cite the issue in each commit message so the trail runs both ways.
- depends on: t12
- covers: c12, h6
- acceptance:
  - Every hand-typed fix in this session has a new issue or a comment on an existing one recording that an operator typed it
  - Every commit message names the issue it serves
  - The two prod.env edits and the deploy already recorded in #123/#124/#127 are cross-referenced rather than duplicated

### t14 — Squash-merge #122 on a green run that was never re-run

- instruction: SQUASH merge per the recorded decision. Do NOT re-run a red job to get green — if a re-run happens, write it into the closure comment. Confirm the PyPI publish succeeded after the merge.
- depends on: t10, t11, t12, t13
- covers: c15, h12, c17, h14
- acceptance:
  - gh pr view 122 shows MERGED, merged via SQUASH per the recorded decision
  - The CI run the merge relied on is green at attempt=1 — no job was re-run to get there
  - If any job IS re-run during this work, that fact is written into the closure comment rather than silently repeated
  - The PyPI publish triggered by the merge is observed to succeed, at the confirmed version

### t15 — Close #127 against the successor issue

- instruction: Read #127's current body first. The closure must state plainly which items were NOT done and why — three of them are structurally post-merge and now live on the successor issue. Do not imply the checklist cleared.
- depends on: t14, t6, t3, t5, t2
- covers: c1, h8, c16, h13
- acceptance:
  - \#127's body as it stands today is read first, confirming it mixes done facts, blocked reconciliation, unowned findings and owner-only decisions
  - A closure comment records: what was done, the `SONAR_TOKEN` resolution, the four Qodo dispositions, and that the 21 deviation records moved to their own tracked issue without gating the merge
  - \#127 is closed on GitHub with the successor issue linked from it
  - The closure states plainly which #127 items were NOT done and why, rather than implying the checklist cleared

## Risks

- [unknown_blocking] The probe (t1) may refute c23/c24. If so, t9 is dropped and t7/t8 stand alone as test-local fixes — the plan branches on t1's result and must not be executed as if the shared-database cause were already established. (task t1)
- [unknown_nonblocking] Whether any reconciliation sweep closes a runner operation orphaned by a crash between CompleteAttempt and closeWait is unverified (c27). Making the test wait longer makes the test honest but does not answer this. (task t7)
- [unknown_nonblocking] Concurrent Store.Migrate across ~20 packages on one CI database (c28) was noted but not examined in depth; it may produce further intermittents that t9 would incidentally fix or incidentally hide. (task t9)
- [unknown_nonblocking] A green CI run after the fix may reflect luck rather than a fixed root cause. t11's contention run is the mitigation, not a proof. (task t11)
- [follow_up] Merging publishes to real PyPI irreversibly (c25). There is no rollback for a published version — only a subsequent release. (task t14)
