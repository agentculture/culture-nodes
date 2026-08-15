# Build Plan — close-the-backlog

slug: `close-the-backlog` · status: `exported` · from frame: `close-the-backlog`

> Culture Nodes cleared its own tracker: all 43 open issues reached a recorded disposition — closed with evidence, closed with a reason, or scheduled into a named batch — the operator-lane loop closed so a cycle runs through the system instead of an operator's session, OpenTelemetry runs live against the production pair, and the triage that did it was dispatched, claimed and decided through Culture Nodes itself.

## Tasks

### t1 — Add a Go job to CI so the Go tree is tested somewhere that is not an operator's laptop (#115 step 1)

- instruction: Add a 'go' job to .github/workflows/tests.yml alongside test/lint/version-check: actions/setup-go pinned by SHA like the other actions, then go build ./... && go vet ./... && go test ./.... Note the repo has NO Go CI today — expect first-run failures that are real, not flaky, and fix or report them rather than relaxing the job. Prove the gate works by deleting an assertion in internal/engine and watching the check go red.
- covers: c125, h80
- acceptance:
  - '.github/workflows/tests.yml' runs go build ./... && go vet ./... && go test ./... on ubuntu-latest
  - deleting an assertion in internal/engine turns the check red, proving the job gates rather than merely existing

### t2 — Commit a regenerable triage table: every open issue, its bucket, its disposition, its evidence pointer

- instruction: Write scripts/triage-report.sh (or .py, stdlib only) that reads 'gh issue list --state open --json number' and joins it against a committed table under docs/triage/. Exit non-zero on any open issue with no disposition. Buckets from the spec: verify-then-close, operator-lane enablers, bug tail, finish work, owner decisions, large bets. Wire it into the lint job.
- covers: c6, h1, c29, h26, c43, h8, c40, h33
- acceptance:
  - a script joins gh issue list against the committed table and exits non-zero on any open issue with no disposition
  - running it twice on an unchanged tracker produces an identical file; moving one disposition changes exactly one row
  - the four after-state queries are committed alongside it so the state is re-derivable

### t3 — Template the closing comment so every closure is re-checkable by a stranger

- instruction: Define the closing-comment template and apply it to every issue this cycle closes: disposition, reason, and either a Culture Nodes run id or a test path plus the command that runs it. #59's closing comment is the worked example. Never use a bare 'gh issue close'.
- covers: c17, h24, c48, h13, c7, h2, c39, h32
- acceptance:
  - each closing comment states disposition, reason, and either a run id or a test path plus its command
  - a fresh reader (colleague review or another actor) reads three closed issues and reports whether the closure is checkable
  - no issue is closed with a bare 'gh issue close' — the comment is mandatory

### t4 — Account for the cycle honestly: issues opened versus closed, and every self-opened issue dispositioned

- instruction: At cycle close, snapshot 'gh issue list --state open --json number' and compare against the cycle-start count recorded in the spec (43 at start, 31 after #59 closed and six were opened). Report opened, closed and delta as numbers in the delivery summary, including a negative delta if that is what happened.
- covers: c2, h16, c33, h30, c1, h15
- acceptance:
  - the delivery summary states opened-count, closed-count and the delta as numbers, reporting a negative delta plainly if that is what happened
  - the count of issues opened by this cycle that end it undispositioned is zero, stated as a number
  - the announcement's four numbers are each rendered from a query a reader can re-run

### t5 — Write the stage-1 lane split down before any dispatch: which issue's evidence comes from spark, which from an agent host

- instruction: Split bucket A by what its evidence needs, not by convenience: Go/shell verifications (#8, #9, #13, #17, #28, #48) go to spark; Python-side ones (#54, #62, #61, #10, #98) go to a spark claude bridge too, since probes 01M03374VAKH0KHN0GDZ466NP4 and 01M0342X60F3NY8MH150G48AZ6 proved no agent host can execute a suite under read-only. Commit the split before the first dispatch.
- depends on: t2
- covers: c94, h64, c111, h74, c50, h36
- acceptance:
  - every bucket-A issue appears in exactly one lane, and the split is committed before the first dispatch
  - no stage-1 verification produces a test result from an agent host; every executable-evidence artifact names a command that ran on spark
  - an issue that turns out to need the other lane is recorded as a miss, not silently re-routed

### t6 — Template the verification dispatch: one brief shape, read-only posture, evidence-or-cannot-verify contract

- instruction: Write ONE brief template and reuse it for every verification. Variables: issue number, the specific claim under test, the evidence form that would settle it. Fixed text: read-only posture, 'return evidence or the words cannot verify here with the reason', 'never an opinion about whether it feels done'. A bespoke heredoc per issue is #103 repeating.
- depends on: t5
- covers: c45, h10, c51, h37, c122, h78
- acceptance:
  - the dispatched instructions differ only in issue number, claim under test, and required evidence form
  - a run that cannot produce evidence returns 'cannot verify here' with the reason, never an opinion
  - every verification dispatch record contains no writable-roots widening and no handover ref; a workspace-write verification dispatch is a gate failure

### t7 — Run the bucket-A sweep through Culture Nodes and decide every claim it produces

- instruction: Dispatch bucket A through nodes-op.sh assign, at most two per account concurrently. After EVERY run: read run + full ledger via the API (not the truncating operator surface), weigh the claim as a completion claim rather than evidence, record confirm or reject, and grade the actor. Expect at least one rejection; if all thirteen confirm, re-check the sample.
- depends on: t6
- covers: c9, h4, h39, c18, h25, c30, h27, c31, h28, c46, h11, c55, h38
- acceptance:
  - at most two verification dispatches are in flight at any moment, checkable from run timestamps
  - each wave's declared model tier matches what actually ran, read back from `usage_model` on the attempts
  - at least one verification claim is rejected rather than confirmed
  - at least ten issues close citing a run id that resolves in the control plane
  - no commit in this stage touches source outside docs/ and the triage artifact, proven by a diff of the stage's merge range

### t8 — Make each stage's exit gate a query, not an assertion

- instruction: Before creating any stage-2 dispatch, run a query over this cycle's run ids filtered to ledger state=proposed and require zero rows. Do the same at each stage boundary. The gate is the query's output, not anyone's recollection.
- depends on: t7
- covers: c44, h9, c10, h5
- acceptance:
  - a query over this cycle's runs returns zero claims in state proposed before any stage-2 dispatch is created
  - stage 2's exit is demonstrated by one package going dispatch-to-merged with no operator shell command in the transcript
  - the operator-lane enablers land in the recorded dependency order

### t9 — Git becomes the handover medium: the agent pushes to refs/culture-nodes/RUN-ID, fenced server-side (#90)

- instruction: Deliver the push credential and prove its fence: a GitHub ruleset restricting the worker identity to refs/culture-nodes/\*, then extend scripts/verify-token-scope.sh to ATTEMPT an out-of-namespace push and require refusal. A fence that has not been tested against is worse than none, because it will be trusted. Build dispatches use workspace-write plus the .git widening from deviation d6; verification dispatches stay read-only.
- covers: c119, h76, c123, h79
- acceptance:
  - a push to any ref outside refs/culture-nodes/\* is REFUSED by the remote, demonstrated by attempting one
  - scripts/verify-token-scope.sh is extended to attempt an out-of-namespace push and require refusal — a false positive here is worse than no fence
  - build packages dispatch workspace-write with the measured .git widening; verification packages stay read-only

### t10 — Record a pushed ref as OBSERVED evidence the control plane measured, not as the agent's claim (#13)

- instruction: When a run hands over a ref, have the control plane fetch it and record what it measured — ref name, commit sha, changed paths — as a ledger record with authority 'observed'. If no fetchable ref exists, write no observed record at all; the agent's summary is not a substitute. This is #13's answer.
- depends on: t9
- covers: c120, h77
- acceptance:
  - the ledger record for a pushed ref carries authority 'observed' and cites the ref and its commit sha
  - a run whose agent claims success without a fetchable ref produces no observed record at all
  - the control plane fetches the ref and records what it measured, never what the agent reported

### t11 — The merge gate consumes CI's verdict as derived evidence instead of an operator reading a green tick (#101)

- instruction: Make the merge gate a node whose verdict is derived evidence: consume the CI conclusion (the sweep already reads check runs per #61) and record it. The gate fetches refs/culture-nodes/RUN-ID instead of harvesting an ssh diff. Keep internal/ledger/authority.go's self-promotion refusal test in the gate for every package.
- depends on: t1, t10
- covers: c15, h22
- acceptance:
  - a test asserts an agent-origin actor cannot promote its own record, and it fails if the authority check is stubbed
  - the gate fetches refs/culture-nodes/RUN-ID rather than harvesting an ssh diff

### t12 — Fix the bug tail: #95 and #105 (one fix, two tests), #98, #21, #17

- instruction: \#95 and #105 are ONE fix in internal/engine/workflow.go DecideContinuation plus internal/scheduler/scheduler.go deadlineContinuationHolds — a hardcoded node.state literal and an errored CEL eval returning the same zero decision as a false one — with a regression test for each symptom. #98 is the scope guard matching instruction text, duplicated across three bridges by the all-backends rule. #21 is a deliberate design choice to argue, not an oversight to patch. #17 needs reproduction against current HEAD before it is fixed or closed.
- covers: c11, h6
- acceptance:
  - each fix carries a test that fails on the parent commit and passes on the fix, verified by checking the parent out
  - \#95 and #105 land as one change in DecideContinuation with a test for each symptom

### t13 — Deploy an OTLP collector and close #5 on a live trace

- instruction: Add an OTLP collector to deploy/prod/compose.thor.yml and set `OTEL_EXPORTER_OTLP_ENDPOINT` for api, worker and scheduler. internal/telemetry already instruments the three seams #5 names and is env-gated, so this is deployment, not instrumentation. #5's trigger condition already holds: prod-worker-1 runs on both thor and orin against thor's prod-postgres-1.
- covers: c8, h3, c47, h12, c32, h29, c4, h18
- acceptance:
  - one trace from the deployed collector contains all three seam spans under a single trace id for a real run
  - the same query returns nothing when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset, proving the export is what produced it
  - pointing the deployment at any collector is a one-variable change, demonstrated against a throwaway local one

### t14 — Ship the database's backups off its own host, and prove the restore (#30)

- instruction: Extend the compose backup service to push each `pg_dump` to s3://culture-nodes-\*/backups/ using the ArtifactBucket grant the base operator policy already holds. Set a public-access block and SSE, and state the retention beside the existing seven-dump local rotation. Then FETCH a dump back from S3, restore it into a scratch Postgres and run the stack against it — before closing #30.
- covers: c97, h66, c102, h69, c73, h52, c69, h49
- acceptance:
  - the backup bucket refuses a public read and encrypts at rest, both checked by a command whose output is pasted into #30
  - a dump is fetched back FROM object storage, restored into a scratch Postgres, and the stack runs against it BEFORE #30 closes
  - the volume and backup facts are restated afterwards, and no stale six-hourly dump keeps running against a database nothing reads

### t15 — Make the database a deployment input in every profile, not just in the Helm chart (#112)

- instruction: Mirror the Helm chart's pattern (values.yaml:155-160) in deploy/compose and deploy/prod: a bundled-postgres toggle plus an external URL. Make sslmode a variable rather than four inlined copies of sslmode=disable. Point the backup loop at the same URL or disable it explicitly. Prove it by pointing a clean checkout at a throwaway external database with configuration edits only.
- covers: c76, h54, c74, h53, c77, h55, c78, h56
- acceptance:
  - a compose stack started with the bundled database disabled and an external URL supplied comes up healthy and passes the smoke script
  - a stack started with neither fails with a clear error rather than silently starting an empty bundled database
  - sslmode is a variable, not four inlined copies; the backup service follows the same URL or is explicitly disabled
  - pointing a fresh install at an external database is a configuration edit only — no code change, no image rebuild

### t16 — Close out the AWS lane honestly: opt-in RDS, the measurements that decided it, and what binds only if #112 chooses rds

- instruction: Decide and record: either revert RDS from dev-operator-policy.json or state in the delivery summary why the grant is kept — silence is the failure mode. Restate the 246MB measurement and the region latency ladder with the commands that produced them. Record that c67/c68/c70's RDS requirements bind only if #112 selects the rds target.
- covers: c91, h63, c66, h46, c70, h50, h57, c64, h44, c65, h45, c67, h47, c68, h48, c80, h58
- acceptance:
  - deploy/aws/dev-operator-policy.json no longer grants RDS, or the delivery summary says why the grant is kept — silence is the failure
  - the 246MB control-plane figure and the region latency ladder are restated with their measurement commands
  - c67/c68/c70's RDS requirements are recorded as binding only if #112 selects the rds target, since no cutover is happening
  - ./deploy/aws/preflight.py exits 0 before any provisioning command runs, and the same script proves it afterwards

### t17 — Split sweep into a pure emitter with durable watermarks, and let conditioned event handlers react

- instruction: Split examples/pr-upkeep/sweep.py into a pure emitter: it discovers findings, holds durable per-source-key watermarks in Postgres (PR: head SHA plus newest comment timestamp; Jira issue: updated timestamp plus newest comment), and raises an event. No triage logic, no merge logic. Add a CEL condition to event subscriptions and a workflow-level trigger that CREATES a run. Build on POST /v1alpha1/events and `signal_events` (migrations/0016) — extend that delivery transaction, do not add a second event path. Prior art for the watermark: internal/notifier/cursor.go.
- covers: c58, h40, c59, h41, c60, h42, c61, h43
- acceptance:
  - killing the sweep between a raise and the next cycle, then restarting it, produces no duplicate event for anything already reported
  - an event matching a handler's name but failing its condition creates no run and resumes nothing, and the event is still recorded
  - opening a PR causes a run to exist with no human action and no run parked in advance; removing the trigger makes the same PR produce nothing
  - no second events table and no second delivery path appear in the diff — the trigger extends `signal_events`' delivery transaction

### t18 — Disposition #50 as cross-repo work, not an external wall

- instruction: File the egress-allowlist feature issue on agentculture/headspace-cli using the communicate skill, then reference it by number in a culture-nodes deviation record covering the interim network:full posture. Re-check 'curl -s <https://pypi.org/pypi/headspace-cli/json>' at disposition time — 0.11.0 was both installed and latest when measured.
- covers: c42, h35, c49, h14
- acceptance:
  - a feature issue exists on agentculture/headspace-cli for the egress allowlist and is referenced by number in the culture-nodes deviation record
  - headspace-cli's published version is re-checked at the point #50 is dispositioned, and the disposition changes if a release with a network flag has appeared

### t19 — Make the capability surface report toolchains, using the three probes as its test cases (#96)

- instruction: Make the capability surface report toolchains, using the three probe runs as its test cases: thor's snap-packaged uv versus orin's standalone one, and Go absent on both. A surface reporting 'uv: present' would have been true on both hosts and useless on both — report what can actually EXECUTE under the dispatch posture, not what is on disk.
- covers: c109, h73, c115, h75
- acceptance:
  - the surface distinguishes thor's snap-packaged uv from orin's standalone one — a bare 'uv: present' would have been true on both hosts and useless on both
  - the probe findings are re-checked after any toolchain change or codex-cli bump; the three run ids are the baseline

### t20 — Hold the two standing repo boundaries through the cycle: vendored skills and the zero-dependency runtime

- instruction: Two standing guards, both cheap: git diff --name-only over the cycle's merge range must touch nothing docs/skill-sources.md marks vendored, and pyproject.toml's dependencies list must still be empty with 'uv run nodes whoami' working in a bare environment. Wire both into the lint job so they fail CI rather than being remembered.
- covers: c14, h21, c16, h23
- acceptance:
  - git diff over the cycle's merge range touches no file docs/skill-sources.md marks vendored
  - pyproject.toml's dependencies list is still empty and 'uv run nodes whoami' works with no third-party packages installed

### t21 — Land the simple credential form with its expiry wired in, and file what replaces it (#111)

- instruction: Fill the empty internal/auth package. Store a HASH of the per-actor secret, never the secret — internal/worker/registry.go:153 already proves the pattern by storing `auth_token_env`, a variable name, which is why a `pg_dump` carries no credentials today and #30 ships those dumps to S3. Key the record by actor key or host name, never by IP (thor's is not static). Put the expiry in a comment at the definition site naming #111 and the dial-in event.
- covers: c105, h71, c106, h72, c96, h70
- acceptance:
  - a `pg_dump` of the credential table yields nothing an attacker could present — hashes or env-var names only, checked against a real dump
  - the record is keyed by actor key or host name, never by IP address
  - the expiry is a comment at the credential's definition site naming #111 and the dial-in event, not a date
  - grep over the schema and a dump for a credential column returns nothing

### t22 — Give the dial-in path rate limiting, lockout and revocation before it accepts a connection

- instruction: Before the dial-in path accepts a connection: rate limiting, lockout on repeated failure, and revocation. Build on internal/actors/token.go's HMAC signer and constant-time verify. A credential with no revocation path cannot be rotated under compromise, and each check needs a test that fails when the check is removed.
- depends on: t21
- covers: c98, h67
- acceptance:
  - a worker presenting a wrong credential repeatedly is locked out, proven by a test that fails when the check is removed
  - a revoked actor credential stops working on the next dial, proven the same way

### t23 — Invert the transport: bridges dial in, with mixed mode decided before the first bridge changes

- instruction: Decide mixed-mode versus flag day BEFORE the first bridge changes — five bridges across thor, orin and spark cannot cut over atomically. If mixed, the control plane keeps its outbound dial path alive while accepting inbound dials. Demonstrate one dispatch reaching a bridge the control plane holds no address for. All five bridges change together per the all-backends rule.
- depends on: t22
- covers: c99, h68
- acceptance:
  - either both transports are alive simultaneously and one bridge is converted while another is not, demonstrated live, or the plan states the flag day with its rollback
  - one dispatch reaches a bridge the control plane holds no address for

### t24 — Retire `endpoint_ref` under the approved ADR 0002 bypass

- instruction: Drop `endpoint_ref` from actors (migrations/0001:46) and endpoint from `runner_invocations` under the human-approved ADR 0002 bypass. Cite the bypass IN the migration file with its reason: expand-contract protects a rolling fleet, and this deployment is two workers and one API restarted together by deploy.sh. Never bypass the policy silently.
- depends on: t23
- covers: c95, h65
- acceptance:
  - the migration that drops `endpoint_ref` cites the human-approved bypass in the migration file itself — the policy is overridden in writing, never silently
  - the bypass rationale is recorded: expand-contract protects a rolling fleet, and this is two workers and one API restarted together

### t25 — Audit the codebase against the connection principle and record what still violates it

- instruction: Grep the codebase and deployment config for stored participant addresses and report every one: actors.`endpoint_ref`, `runner_invocations`.endpoint, the bridge URLs in nodes-op.sh, web/src/api/types.ts:417. Say which the inversion removes and which remain, rather than declaring the principle satisfied.
- covers: c84, h59
- acceptance:
  - no configuration file, database column or dispatch record holds a participant's IP address
  - a grep over deployment config returns only the LAN bridge endpoints the inversion is scheduled to remove

### t26 — Record that the bootstrap was built the old way, rather than claiming the loop ran itself

- instruction: Copy STATE section 11's fourteen operator-step rows into the delivery summary as a checklist and mark each automated-by-a-merged-node or still-manual. Report the stage-2 transcript's operator shell commands rather than omitting them — the bootstrap IS built the old way and claiming otherwise would be the overclaim the last cycle refused to make.
- covers: c13, h20, c5, h19
- acceptance:
  - the stage-2 transcript's operator shell commands are reported in the delivery summary rather than omitted
  - each of the fourteen STATE-section-11 operator steps is marked automated-by-a-merged-node or still-manual at cycle close; none disappears without either

### t27 — Put the remaining owner decisions in front of the owner as costed briefs

- instruction: Write one costed page per remaining owner decision under docs/decisions/: options, cost, dependencies, and what specifically closes if the answer is no. #59's closing comment is the model. A brief whose 'no' branch leaves the issue in the same open state has not made a decision possible.
- covers: c12, h7
- acceptance:
  - each brief states cost, dependencies, and what specifically closes if the answer is no
  - a brief whose 'no' branch leaves the issue in the same open state has not made a decision possible and is rewritten

### t28 — Close the loop on why this mattered, and on the signal that judges the next cycle

- instruction: Map each of the last cycle's three NOT MET signals to the issue that would flip it, and state the mapping in this cycle's delivery summary. At the end, either start the next cycle without a -STATE.md or record that this signal failed again — those are the only two honest outcomes.
- covers: c41, h34, c3, h17, c34, h31
- acceptance:
  - each of the last cycle's three NOT MET signals maps to a named issue here, and closing it would actually flip the signal
  - the next cycle starts without writing a -STATE.md, or one is written and the delivery summary records the signal failing again — no third option

### t29 — Make a run explain itself: untruncated ledger claims and a real run-status surface (#92, #108)

- instruction: Remove the truncation in the nodes-operator ledger output (first-party script, safe to edit) and give internal/api a run-status surface that answers 'what is running and what is it doing' without hand-written curl. An unknown filter must be REJECTED, not silently ignored. Never edit a vendored skill script body.
- covers: c10, h5
- acceptance:
  - the operator surface prints a claim in full; the truncation that hid the qualifying half is gone
  - 'what is running right now, and what is it doing' is answerable without hand-written curl, and an unknown filter is rejected rather than silently ignored
  - the fixes land in the first-party nodes-operator scripts and internal/api, never by editing a vendored script body

### t30 — Build the affirmative half of the authority model: a surface that decides a proposed claim (#99)

- instruction: Build the affirmative half of PRD section 10.4: a surface that records a human or derived decision against a proposed claim, as its own immutable ledger record naming who decided. The refusal half already works — nothing self-promotes — and must keep working: the agent-origin self-promotion test stays green.
- depends on: t29
- covers: c10
- acceptance:
  - a proposed claim can be confirmed or rejected through the product, and the decision is itself a ledger record naming who decided
  - nothing self-promotes: an agent-origin actor still cannot decide its own claim, and the test proving it fails if the check is stubbed

### t31 — Compose the work-package brief from the plan and the capability surface instead of a heredoc (#103, #93)

- instruction: Generate the work-package brief from the plan task (summary, acceptance criteria, instruction, coverage targets) plus the target actor's capability surface. devague plan waves already emits the dependency graph. The agent pulls its own checkout, so no operator ssh precedes a dispatch.
- depends on: t9, t30
- covers: c10
- acceptance:
  - a brief is generated from the plan task plus the target actor's capability surface; a hand-written heredoc is not the input
  - the agent prepares its own checkout by pulling, so no operator ssh precedes a dispatch

### t32 — Close the gate-repair-gate loop and make the deployed revision a recorded fact (#102, #104)

- instruction: Route a gate failure to a bounded repair attempt instead of the operator's session, and STATE the bound. Honest constraint: last cycle's four gate failures were all cases where the actor could not run the tool that would have shown them — a repair node on the same host cannot either, so this may legitimately stop at 'routed, not unattended'. Separately, make deploy record which revision it shipped.
- depends on: t11
- covers: c10
- acceptance:
  - a gate failure routes to a bounded repair attempt rather than to the operator's session, and the bound is stated
  - a deploy records which revision it shipped, so a live test can prove which code it tested

### t33 — Make upkeep run itself: a schedule that starts runs, and findings routed by actor affinity (#107)

- instruction: Give the engine a schedule that starts runs on a cadence, and route findings to a developer node by declared actor affinity, recording the affinity on the run so the comparative record can use it. With t17's trigger plus this schedule, demonstrate a finding reaching a run with no operator action — end to end, not argued. Restarting the control plane must neither double-start nor lose the schedule.
- depends on: t17
- covers: c60
- acceptance:
  - a run starts on a declared cadence with no human action, and disabling the schedule stops it starting
  - restarting the control plane neither double-starts a scheduled run nor silently loses the schedule
  - a finding routes to a developer node by declared actor affinity, and the affinity is recorded on the run so the comparative record can use it
  - with the trigger from t17 plus this schedule, a finding reaches a run with no operator action — demonstrated end to end, not argued

## Risks

- [unknown_nonblocking] The repair loop (#102) may not be boundable safely enough to run unattended: the last cycle's four gate failures were all cases where the actor could not run the tool that would have shown them, and a repair node on the same host cannot run it either. Task t32 may have to stop at 'routed, not unattended'. (task t32)
- [unknown_nonblocking] A dialing worker may hold a claim across a dropped-and-reconnected connection while another worker believes it holds it. Fencing tokens keep the WRITE safe; they say nothing about liveness, and the inversion introduces a liveness signal the lease model does not consult. (task t23)
- [unknown_nonblocking] Nothing requires a closed-as-verified issue to be REOPENED when its cited evidence later fails. Disposition-not-deletion (c17) protects the record; it does not protect against a wrong closure becoming permanent. (task t3)
- [unknown_nonblocking] Thirty-two tasks against a concurrency cap of one-to-two model sessions is a long wall-clock, and the cap is deliberate. The plan must be paced across multiple subscription windows rather than assumed to fit in one; a wave that declares more sessions than the remaining window holds is a planning defect.
- [unknown_nonblocking] This cycle will open issues of its own — it has already opened six. If that rate continues, the tracker ends larger than it started even though every listed issue was dispositioned. #107 (upkeep runs itself) is the mechanism that would turn future findings into runs instead of rows, and it is NOT in this plan. (task t4)
- [unknown_nonblocking] SUPERSEDES the earlier risk that #107 is absent: it is now task t33, in scope by the owner's decision. The residual risk is narrower — #107 makes findings become runs, but nothing yet guarantees those runs are DECIDED, so the 0-of-9 undecided-claim failure could reappear at a higher volume. (task t33)
