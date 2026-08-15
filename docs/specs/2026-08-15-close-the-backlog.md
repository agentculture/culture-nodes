# close-the-backlog

> Culture Nodes cleared its own tracker: all 43 open issues reached a recorded disposition — closed with evidence, closed with a reason, or scheduled into a named batch — the operator-lane loop closed so a cycle runs through the system instead of an operator's session, OpenTelemetry runs live against the production pair, and the triage that did it was dispatched, claimed and decided through Culture Nodes itself.
> instruction: At cycle close, render the announcement's four numbers from queries: dispositioned count from the triage report, run-evidence count from closing comments, decided-claim count from the ledger, trace from the collector.

## Audience

- The repo owner operating culture-nodes, and culture-nodes itself as its own first customer. Secondary: any future reader who opens a closed issue and needs to know whether the closure was earned.
  - instruction: Have a fresh reader — a colleague-backend review or another actor — read three closing comments and report whether the closure is checkable.

## Before → After

- Before: 43 issues are open (#5 through #109, counted with gh issue list), and the tracker grows faster than it shrinks: the own-the-work-end-to-end cycle (#87) closed eleven and opened fourteen (#88-#94, #99-#104), all recorded in docs/deliveries/2026-08-15-own-the-work-end-to-end-STATE.md sections 11-12.
  - instruction: Snapshot 'gh issue list --state open --json number' at cycle start and at cycle close; report both counts and the delta in the delivery summary.
- Before: The last cycle's own delivery summary scores three of six success signals NOT MET (docs/deliveries/2026-08-15-own-the-work-end-to-end.md section 1): no two-carrier handoff, no observed cancel, and nine of nine ledger claims left proposed. Those three failures are literally issues #90, #21/#9-adjacent and #99 on this list.
  - instruction: Re-read section 1 of docs/deliveries/2026-08-15-own-the-work-end-to-end.md and map each NOT MET signal to the issue that would flip it; record the mapping in the triage table.
- Before: OpenTelemetry is no longer a stub: internal/telemetry ships an env-gated OTLP tracer+meter with a reviewable attribute allowlist, instrumenting exactly the three seams #5 names (engine complete.go, worker dispatch.go, actors callback.go), constructed in all three binaries (cmd/nodes/serve.go:147, worker.go:107, scheduler.go:52). What is missing is deployment: 'grep -rn OTEL deploy/' returns nothing, so `OTEL_EXPORTER_OTLP_ENDPOINT` is unset in production and telemetry.New returns NoOp on every process.
  - instruction: Confirm the gate by running the control plane once with `OTEL_EXPORTER_OTLP_ENDPOINT` unset and once set, comparing exported spans.
- Before: Fourteen operator steps run per work package in a human session (STATE section 11): checkout refresh, brief authoring, dispatch, poll, untruncated ledger read, ssh harvest, worktree stage, gate before and after, hand repair, commit, merge, worktree teardown, push. Each has an issue on this list; the row for 'decide the agent's proposed claim' reads 0 of 9.
  - instruction: Copy STATE section 11's fourteen rows into the triage artifact as a checklist; mark each automated-by-node or still-manual at cycle close.
- Before: headspace-cli 0.11.0 is both the version installed on thor and the latest published release on PyPI, and its CLI surface has no network flag at all — so #50's egress allowlist cannot arrive by upgrading. But headspace-cli is a first-party AgentCulture repo (agentculture/headspace-cli, checked out at /home/spark/git/headspace-cli, 12 open issues, none about egress), so this is a cross-repo feature we can ship, not an external blocker.
  - instruction: Re-run 'curl -s <https://pypi.org/pypi/headspace-cli/json>' at the point #50 is dispositioned and compare against 0.11.0.
- Before: The stated motivation for #59 is falsified by measurement: the entire control plane on thor uses 246MB (postgres 69, minio 95, api 32, worker 20, scheduler 15, notifier 14, backup 2) out of 122GB, with 56GB free — while model-gear's vLLM worker alone holds 16.2GB. Moving the control plane to AWS frees a quarter of a gigabyte. The mass-mesh motivation is untouched; the memory one should not be repeated.
  - instruction: Re-run free -h and docker stats on thor after the cutover; report the delta in the delivery summary.
- Before: Measured from thor, TCP connect latency is 19ms to il-central-1, 70ms to eu-central-1, and 160ms to us-east-1, where the existing SQS/Lambda lane lives. Local Postgres answers in under a millisecond. Region choice is therefore not a preference: it is an eight-fold difference on every database round trip the engine makes.
  - instruction: Re-run the three-region curl loop from thor at cutover.
- Before: Two owner-only prerequisites block provisioning, both measured: il-central-1 is not enabled on account 435593604218 (an sts call there returns InvalidClientTokenId, the opt-in-region signature), and deploy/aws/dev-operator-policy.json grants SQS, S3, ECR, Lambda, scoped IAM and STS but no RDS at all (rds:DescribeOrderableDBInstanceOptions is denied).
  - instruction: Verify with: aws sts get-caller-identity --region il-central-1 and aws rds describe-orderable-db-instance-options --region il-central-1.
- Before: Postgres data lives in a Docker named volume — `prod_pgdata`, mounted at /var/lib/docker/volumes/`prod_pgdata`/`_data` on thor, 96MB today — and its only backups are `pg_dump` -Fc files written every six hours to ~/.culture-nodes/backups on the same machine, seven retained, newest 2.3MB. Authoritative state and its entire backup history sit on one host's local disk. That, not memory, is the durability argument #59 actually has.
  - instruction: Re-check the volume and backup facts after cutover: docker volume inspect `prod_pgdata`, ls ~/.culture-nodes/backups, and confirm RDS automated backups are on with a stated retention.
- Before: The control-plane code is already database-location-agnostic: `NODES_DATABASE_URL` is a single connection URL read by api, migrate, scheduler and worker, and the Helm chart already documents bringing your own (postgresql.enabled false plus postgresql.external.url, values.yaml:155-160). It is already exercised in production — orin's worker connects to thor's Postgres over the LAN through the same variable. The gap is the compose profiles, which bundle a postgres service, inline sslmode=disable in four places, and point the backup loop at -h postgres.
  - instruction: Read deploy/helm/culture-nodes/values.yaml:155-160 for the pattern to mirror, and deploy/prod/compose.orin.yml:20 for the remote-database call site already in production.
- After: The tracker answers three questions without a human reconstructing them: what is closed and on what evidence, what is open and who owns it, and what the system did versus what a person did. Bucket A is closed on run evidence, the operator-lane loop runs the work instead of an operator's session, #5 exports live telemetry, and the four decision-shaped issues carry an owner's recorded answer.
  - instruction: Commit the four queries alongside the triage artifact so the after-state is re-derivable.

## Why it matters

- The backlog is not noise — it is the measured gap between what Culture Nodes claims and what it does. The last cycle scored three of its six success signals NOT MET and every one of those failures is an issue on this list. Closing them is the same work as making the product true, and leaving them is the same as letting the ledger lie by omission.
  - instruction: State the signal-to-issue mapping explicitly in the spec's own delivery summary.

## Requirements

- Every open issue reaches exactly one recorded disposition: closed as verified-done, closed as superseded by a parent issue, closed as won't-do with a stated reason, or kept open with a named batch and owner. 'Clean' means no unclassified issue, and the disposition is a comment on the issue, not a spreadsheet.
  - instruction: Verify with: gh issue list --state open --json number against a triage table committed under docs/; each of today's 43 numbers appears exactly once with a disposition.
  - honesty: Re-running the triage report against the live tracker returns zero issues without a disposition; if someone opens a new issue mid-cycle the report shows it as unclassified rather than silently passing.
- An issue closed as already-done cites executable evidence: a test that fails when the behaviour is removed, a live probe, or a run id plus its ledger record. A code reading is not evidence.
  - instruction: For each verified-done closure, the closing comment must name the test path, the probe command with its output, or the run id.
  - honesty: For every issue closed as already-done, deleting or stubbing the cited behaviour makes the cited test fail, or re-running the cited probe returns the pre-fix output. A closure whose evidence still passes with the feature removed is not evidence.
- \#5 closes on live telemetry, not on code: an OTLP collector runs in the production compose, `OTEL_EXPORTER_OTLP_ENDPOINT` is set for api, worker and scheduler, and one trace shows engine transition -> worker dispatch -> actor callback for a real run while two workers share one Postgres — the condition the issue itself names as the trigger.
  - instruction: Ship the collector configuration and the `OTEL_EXPORTER_OTLP_ENDPOINT` variable for api, worker and scheduler behind one switch, redeploy, run one dispatch, and paste the trace query output into #5. Which collector answers — a throwaway local one during the build, the AWS backend once #59 is decided — is a value, not a code change.
  - honesty: A trace query against the deployed collector returns one trace containing all three seam spans for a single run id, taken while docker ps shows two worker processes against one Postgres — and the same query returns nothing when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset, proving the export is what produced it.
- The verification wave runs through Culture Nodes: one read-only dispatch per issue-to-verify to a registered actor, its finding lands as a proposed ledger claim, and the operator records a decision on it — so the triage itself produces the first non-zero count in STATE section 11's decision row.
  - instruction: Dispatch bucket A read-only to codex-thor/codex-orin one package per actor, then record confirm or reject on each returned claim through the approval surface.
  - honesty: The count of ledger records with a decision, for runs created by this cycle, is greater than zero and equals the number of verification runs — checked by query, not by memory.
  - honesty: No more than two verification dispatches are in flight at any moment, checkable from run `created_at` and terminal timestamps overlapping by at most two.
- The operator-lane enablers land before the waves that would use them, in dependency order: untruncated ledger + run visibility (#92, #108), decision surface (#99), harvest node (#100), gate node (#101), repair loop (#102), brief composition (#103), checkout provisioning (#93), deploy node (#104).
  - instruction: Order the bootstrap work #92,#108 -> #99 -> #100 -> #101 -> #102 -> #103 -> #93 -> #104, and do not begin a downstream item before its upstream is merged.
  - honesty: Stage 2's exit is demonstrated by one package that goes from dispatch to merged with no operator shell command in the transcript. If the operator typed ssh or git apply, the gate has not been passed regardless of what shipped.
- The bug tail — #95 (hardcoded node.state), #105 (errored continue.while indistinguishable from false), #98 (scope guard matches instruction text), #21 (single-threaded codex bridge), #17 (swallowed scp failure) — are small independent fixes, each merged with a regression test that fails on the unfixed code, and each closeable without waiting for a batch.
  - instruction: For each of #95,#105,#98,#21,#17: write the failing test first, check out the parent commit to watch it fail, then fix. #95 and #105 share one fix in DecideContinuation.
  - honesty: Each bug-tail fix has a test that fails on the parent commit and passes on the fix, verified by checking out the parent and running it — not merely written alongside the fix.
- Issues that are decisions rather than work — #59 (AWS), #30 (S3 backup), #6 (OIDC), #50 (upstream headspace egress) — are closed by the repo owner making the decision, not by an agent choosing for them; the cycle's job is to state each decision's cost and dependencies and put it in front of them.
  - instruction: Write four briefs under docs/decisions/ (one per bucket-E issue), each with cost, dependencies, and the consequence of a no; #59's is written first.
  - honesty: Each bucket-E brief states what it costs, what it depends on, and what specifically closes if the owner says no; a brief whose 'no' branch leaves the issue in the same open state has not made a decision possible.
- The triage table is a committed, regenerable artifact under docs/ — issue number, bucket, disposition, evidence pointer — produced from gh issue list rather than transcribed by hand, so 'the tracker is clean' is re-checkable by running it again instead of believed.
  - instruction: Write scripts/triage-report.sh (or equivalent) that joins gh issue list against the committed disposition table and exits non-zero on any open issue with no disposition.
  - honesty: The triage artifact regenerates from the tracker: running its script twice on an unchanged tracker produces an identical file, and moving one issue's disposition changes exactly one row.
- Each stage has an exit gate that the next stage's start depends on: stage 1 (verification sweep) exits when every bucket-A issue has a decided ledger claim — confirmed or rejected, not proposed; stage 2 (operator-lane bootstrap) exits when a package can be harvested, gated and decided without an operator shell command; stage 3 exits when every remaining issue is closed or carries an owner.
  - instruction: Do not start stage 2 while any stage-1 claim is still proposed — that failure mode is exactly STATE section 11's 0-of-9 row repeating.
  - honesty: Stage 1's exit condition is queried, not asserted: a query over this cycle's runs returns zero claims in state proposed before any stage-2 dispatch is created.
- A verification dispatch has a fixed contract: the brief names the issue, the specific claim to test, and the evidence form that would settle it; the run returns evidence or the words 'cannot verify here' with the reason; it never returns an opinion about whether the issue feels done. The operator then records confirm or reject against the claim.
  - instruction: Template the brief once and reuse it for all thirteen bucket-A issues; a bespoke heredoc per issue is #103 repeating.
  - honesty: The same brief template produced every verification dispatch — checkable because the dispatched instructions differ only in the issue number, the claim, and the evidence form.
- A read-only verification run never fixes what it finds. An issue verified as not-done returns to the tracker with the evidence attached and is re-bucketed, because a fix costs capacity that the sweep did not budget and produces a diff no gate was scheduled to review.
  - instruction: Diff the verification stage's merge range with --name-only and assert every path is under docs/ or the triage artifact.
  - honesty: No commit in the verification sweep's stage touches source outside docs/ and the triage artifact; a diff of the stage's merge range proves it.
- \#5 splits into build and evidence: the collector configuration, the endpoint environment in the units, and the deploy are shippable now and do not wait on the AWS decision; only the closing live trace does. If the AWS decision slips, #5 sits at 'built, awaiting a backend' with that state recorded on the issue rather than looking untouched.
  - instruction: Land the config behind `OTEL_EXPORTER_OTLP_ENDPOINT` so pointing it at any collector — local or AWS — is a one-variable change.
  - honesty: Pointing the deployment at any OTLP endpoint requires changing one environment variable and nothing else — demonstrated by exporting to a throwaway local collector before the AWS backend exists.
- Every issue closed this cycle names, in its closing comment, either a Culture Nodes run id or a test path plus the command that runs it. A closing comment that says only 'done in the last batch' is not acceptable, because that is precisely the state that produced this backlog.
  - instruction: Template the closing comment: disposition, evidence pointer (run id or test path + command), and the bucket it came from.
  - honesty: Sampling any five closing comments from this cycle yields five that name a run id or a test path plus its command; a reader can re-run each one.
- \#50 is dispositioned as a cross-repo work item, not an external blocker: a feature issue is filed on agentculture/headspace-cli for the egress allowlist, and culture-nodes records its interim network:full posture as an explicit deviation naming that issue — so the fallback stops being an unexplained degradation.
  - instruction: Use the communicate skill to file the headspace-cli issue; it signs cross-repo posts as culture-nodes (Claude).
  - honesty: The headspace-cli issue exists and is referenced by number in the culture-nodes deviation record, so the interim network:full posture points at the work that will end it.
- Every dispatched package declares a model tier matched to its task kind, and the split plan states the tier per wave: mechanical and verification work — reading code, running a probe, reporting evidence — goes to the cheapest model that can do it; design, gate adjudication and judgment calls go to the strongest. A wave that runs everything on the top tier is a planning defect, not a safety margin.
  - instruction: In the split plan, add a per-wave line: concurrency (1-2), model tier per package, and the justification. Verify afterwards by reading `usage_model` off each attempt through the API.
  - honesty: Each wave's declared tier is checkable against what actually ran: the attempt records name the model, and a wave whose attempts all report the top tier while its split plan declared a cheaper one is a recorded miss, not a rounding error.
- Sweep's dedup state is durable and per source key — PR: head SHA plus newest comment timestamp; Jira issue: updated timestamp plus newest comment timestamp — surviving a sweep restart, a redeploy and a control-plane restart. A sweep that loses its watermark re-raises the whole world, which is the failure this split exists to prevent.
  - instruction: Store watermarks in Postgres beside the notifier cursor prior art; key them on repo+PR and on Jira issue key.
  - honesty: Killing the sweep between a raise and the next cycle, then restarting it, produces no duplicate event for anything it already reported — tested by restarting mid-sweep, not argued from the code.
- An event subscription may carry a condition evaluated over the event's payload, so one emitted finding reaches only the handlers that match it. The condition language is CEL, the same as every other predicate in the system (contracts, guards, continue.while) — not a second expression dialect.
  - instruction: Evaluate the condition inside the same delivery transaction that already matches subscriptions; record the non-match rather than dropping it.
  - honesty: An event that matches a handler's name but fails its condition creates no run and resumes nothing, and the event is still recorded — so a non-match is visible as a decision, not as silence.
- A workflow may declare an event trigger that CREATES a run, not merely resumes one parked on a wait node. That is what closes #71 (nothing wakes the flow when a new PR arrives) and the 'start the first run' half of #107; the existing until.signal wait covers only the resume case, which requires a run to already exist.
  - instruction: Extend the workflow schema with a trigger declaration, and match it at delivery time in DeliverSignalEvent.
  - honesty: Opening a PR causes a run to exist with no human action and no run parked in advance; removing the trigger declaration makes the same PR produce nothing.
- The RDS instance runs in il-central-1, not the us-east-1 the existing AWS lane uses. With compute staying on thor, the worker-to-Postgres link becomes the chattiest path in the system, and 160ms per round trip would multiply every engine transaction, every lease claim and every tick. The SQS and Lambda lanes may stay where they are; the database may not.
  - instruction: Enable il-central-1 on the account, then provision RDS there. Keep the us-east-1 SQS/Lambda resources as they are — a split-region deployment is correct here, not a mistake to tidy up.
  - honesty: Latency from thor to the provisioned RDS endpoint is measured and under 30ms; if it is not, the region choice is wrong and the cutover stops.
- Before any cutover, the engine's latency budget is measured against the real RDS endpoint, not assumed: the number of round trips in a completion transaction, a lease claim and one scheduler tick, multiplied by the measured RTT, compared against the lease and fencing timeouts already in the code. If a lease can expire inside one transaction's round trips, the cutover changes the timeouts before it changes the database.
  - instruction: Run the existing engine and worker suites against an il-central-1 RDS instance before pointing production at it; the suites already exercise leases and fencing.
  - honesty: A completion transaction's round-trip count times the measured RTT is smaller than the lease timeout with margin, stated as two numbers and their ratio; if it is not, the timeouts change first.
- The cutover has a proven rollback before it happens: the existing `pg_dump` loop in the compose backup service is restored into the RDS instance and the stack is run against it, so the migration path is exercised in the direction that matters before production depends on it. RESTORE.md is updated with whichever procedure actually worked.
  - instruction: Restore the newest ~/.culture-nodes/backups dump into the il-central-1 RDS instance, point a non-production stack at it, run the suites, then update docs/prod/RESTORE.md with what actually worked.
  - honesty: A restore from the current `pg_dump` into the RDS instance is completed and the stack runs against it, before production traffic points there. A rollback that has never been executed is not a rollback.
- The two owner prerequisites are cleared as an explicit, recorded step: il-central-1 enabled on the account, and deploy/aws/dev-operator-policy.json extended with the RDS actions the provisioning needs plus whatever the OTel backend requires. The policy is a checked-in artifact, so the extension is a reviewable diff, not a console click.
  - instruction: Run ./deploy/aws/preflight.py — it reports every prerequisite with who may fix it and the exact command. An account admin then runs 'bootstrap-operator.sh enable-region il-central-1' and 'bootstrap-operator.sh update-policy'. Re-run preflight until it exits 0.
  - honesty: The policy extension is a reviewed diff in deploy/aws/dev-operator-policy.json, and the same provisioning commands that failed with AccessDenied succeed afterwards.
  - honesty: The gate is an exit code, not a conversation: ./deploy/aws/preflight.py exits 0 before any provisioning command runs, and the same script is what proves it afterwards.
- Every deployment profile takes the database as one input and can run with no bundled database: compose gains the switch the Helm chart already has, sslmode becomes a variable rather than four inlined copies of sslmode=disable, and the backup service either follows the same connection URL or is explicitly disabled in favour of the provider's backups — never silently pointed at a container that is not there.
  - instruction: Add the toggle to deploy/compose/docker-compose.yml and deploy/prod/compose.thor.yml, and document it in both READMEs and .env.example.
  - honesty: A compose stack started with the bundled database disabled and an external URL supplied comes up healthy and passes the smoke script; a stack started with neither fails with a clear error rather than silently starting an empty bundled database.
- The RDS migration is executed as the first non-bundled deployment rather than as an AWS special case: the same switch a stranger would use to point at their own Neon or Cloud SQL instance is the switch our production stack uses to reach il-central-1. If the migration needs a code path a third-party install would not have, the portability requirement has been violated.
  - instruction: Perform the cutover by changing `NODES_DATABASE_URL` and disabling the bundled service — nothing else.
  - honesty: The diff that lands the RDS cutover touches deployment configuration and documentation only — no file under internal/ or cmd/ changes to make AWS work.

## Honesty conditions

- Every noun in the announcement is checkable after the fact: the disposition count, the run-evidence count, the decided-claim count and the trace all come from queries a reader can re-run, not from this document's own say-so.
- The claim that the tracker grows faster than it shrinks holds on re-count: closed-this-cycle minus opened-this-cycle is stated as a number at the end, and if it is negative the cycle says so rather than reporting only closures.
- Each of the three NOT MET signals maps to a named issue on this list, and closing that issue would actually flip the signal — checked by re-reading the signal's own wording, not by assuming the mapping.
- Removing `OTEL_EXPORTER_OTLP_ENDPOINT` returns every process to a no-op provider with no dialling and no goroutine, and setting it produces spans at a collector — both observed, so the env gate is what controls export.
- Each of the fourteen operator steps is either automated by a merged node or still listed as manual at the end of the cycle; no step disappears from the inventory without either a capability or an admission.
- The bootstrap batch's own transcript shows operator shell commands, and the delivery summary reports that as expected rather than claiming the loop ran itself.
- git diff over the cycle's merge range touches no file under .claude/skills that docs/skill-sources.md marks vendored.
- A test asserts that an agent-origin actor cannot promote its own record, and it fails if the authority check is stubbed out.
- pyproject.toml's dependencies list is still empty at the end of the cycle, and uv run nodes whoami works in an environment with no third-party packages installed.
- Every issue closed this cycle has a closing comment stating its disposition and reason; a closure with no comment is a gate failure, not an oversight.
- At least one verification run's claim is rejected rather than confirmed during stage 1 — evidence that the operator is actually weighing claims and not rubber-stamping them. If all thirteen come back confirmed, that is itself a signal to re-check the sample.
- The triage report exits non-zero when an issue has no disposition, demonstrated by removing one row and re-running it.
- Each of the ten closing comments names a run id that resolves to a real run in the control plane, checked by fetching each one.
- A query over this cycle's runs returns zero ledger records in state proposed at the point the cycle closes.
- The trace contains all three seam spans under one trace id, and the same query run before the collector was deployed returns nothing.
- The count of issues opened by this cycle that end it undispositioned is zero, stated as a number in the delivery summary.
- The next cycle starts without writing a -STATE.md, or writes one and the delivery summary records that this signal failed again — no third option.
- A reader who was not present can open any closed issue and tell from its comment alone whether the closure was earned; tested by having someone or something other than the author read three of them.
- All four after-state questions are answered by queries against the tracker and the control plane, with the queries themselves committed.
- The mapping from backlog item to product gap survives inspection: each of the three failed signals from the last cycle points at issues on this list, and closing them changes the signal's verdict.
- headspace-cli's published version is re-checked at the point #50 is dispositioned; if a release with a network flag has appeared, the disposition changes rather than shipping stale.
- Every Go-side closure this cycle cites a command that ran on spark, and none cites output from an agent host.
- The verification sweep's dispatches contain no writable-roots widening and no handover ref, checkable in the dispatch records themselves.
- No second events table and no second delivery path appear in the diff: git log -p over migrations shows the trigger work extending `signal_events`' delivery transaction rather than adding a parallel one.
- The 246MB figure is re-measured after the migration and the freed memory reported as a number, so the falsified premise is closed with evidence rather than left as an argument.
- The latency ladder is re-measured from thor at cutover time and recorded, because a route can change; a cutover that finds il-central-1 no longer near stops rather than proceeding on this measurement.
- Both prerequisites are verified by a command that succeeds — an sts call in il-central-1 returning an identity, and an rds describe call returning options — not by someone reporting that they clicked the console.
- The bridges-poll-outbound issue exists with a number before this cycle closes, so the deferral is a filed decision rather than a dropped thread.
- The backup and volume facts are restated after the migration: if RDS becomes authoritative, the compose backup loop is either removed or repurposed, and no stale six-hourly `pg_dump` keeps running against a database nothing reads.
- Pointing a fresh install at an external database requires editing configuration only: a clean checkout, one URL, no code change and no image rebuild — demonstrated by doing it against a throwaway database before the RDS cutover.
- The bundled-Postgres path still passes its smoke test after the migration, run on a machine that has no AWS credentials at all.

## Success signals

- Every number in today's 43-issue list appears in a committed triage table with a disposition, and gh issue list --state open returns only issues that name a batch and an owner.
  - instruction: Add scripts/triage-report.sh and wire it into the lint job so an undispositioned issue fails CI while the cycle is open.
- At least ten issues close citing a Culture Nodes run id in the closing comment — the verification came from the system, not from the operator reading code.
  - instruction: For each closing comment, fetch the named run id from the control plane and confirm it resolves.
- The ledger decision count for this cycle is non-zero: every proposed claim produced by this cycle's runs is confirmed or rejected before the cycle closes, inverting STATE section 11's 0-of-9 row.
  - instruction: Query the ledger for this cycle's run ids filtered to state=proposed before declaring the cycle closed.
- A single trace, queried from the deployed collector, shows engine transition commit, worker dispatch and actor callback for one production run while both workers are live against one Postgres — pasted into #5 as its closing evidence.
  - instruction: Query the collector for the run id of the first post-deploy dispatch and assert three seam spans under one trace id.
- This cycle's own findings are dispositioned before it closes: any issue it opens is either fixed inside the cycle or leaves it already assigned to a named batch — the failure mode of the last two cycles, measured rather than promised.
  - instruction: List issues opened during the cycle at close and check each appears in the triage table.
- The next cycle needs no -STATE.md companion file, because run visibility (#108) and decided claims (#99) let a reader reconstruct what happened from the runs — the same signal the last cycle scored NOT MET.
  - instruction: Do not create a -STATE.md for the next cycle; if a context compaction forces one, record that as this signal failing.

## Scope / boundaries

- The bootstrap batch is built the old way — operator harvest, operator gate, operator merge — because the capabilities that would automate it are exactly what it builds. Claiming otherwise would be the same overclaim the last cycle refused to make (STATE section 11).
  - instruction: Record operator shell commands in the stage-2 transcript and report them in the delivery summary rather than omitting them.
- Vendored skills under .claude/skills are cite-don't-import (docs/skill-sources.md); operator-surface fixes (#92, #108) land in the first-party nodes-operator scripts and in internal/api, never by editing a vendored script body.
  - instruction: Before merging any operator-surface change, run git diff --name-only against the vendored list in docs/skill-sources.md and refuse a match.
- The ledger authority model does not change: agents create proposed records only, nothing self-promotes (internal/ledger/authority.go, PRD section 10.4). #99 adds the affirmative half — a human or a deterministic validator recording a decision — and nothing else.
  - instruction: Keep internal/ledger/authority.go's self-promotion refusal test in the merge gate for every stage-2 package.
- The Python runtime package keeps zero third-party dependencies (CLAUDE.md, pyproject dependencies = \[\]); all telemetry work stays on the Go side and in deployment config.
  - instruction: Run the lint job's zero-dependency check each stage; it already fails if pyproject gains a runtime dependency.
- Closing an issue is a disposition, never a deletion: a won't-do closes with its reason recorded on the issue so the decision survives, and a superseded issue names the parent that absorbed it.
  - instruction: Close every issue with 'gh issue close --comment', never a bare close.
- Verification dispatched to a codex actor returns evidence for the operator to weigh; a codex run reporting 'already delivered' is a completion claim and has been wrong before (prior actor-quality record), so no issue closes on a bridge's say-so alone.
  - instruction: Read the full claim through the API (not the truncating operator surface) before deciding it; #92's fix removes that step.
- Go-side claims are gated on spark regardless of who verified them: neither agent host has a Go toolchain (STATE section 6), so a codex run's word on a Go behaviour is a pointer to check, never the evidence itself, until #96 makes routing toolchain-aware.
  - instruction: Run every Go-side verification command on spark and paste its output into the closing comment.
- The verification sweep dispatches read-only and stays read-only: no `writable_roots` widening, no .git widening, no handover ref. The write path is unproven (#18), and proving it is stage 2's job, not stage 1's.
  - instruction: Inspect each verification dispatch's recorded request for `writable_roots` or `handover_ref` and assert both are absent.
- The event seam already exists and is the one to build on: POST /v1alpha1/events appends an immutable signal event and resumes matching subscriptions inside one transaction, with PostgreSQL the single writer of waiting state and no completion committed outside a fenced claim. The sweep split adds emission, conditions and triggers to that surface — it does not introduce a second event path.
  - instruction: Review the diff against internal/api/signalevents.go and migrations/0016 before merging any trigger work.
- No AWS-specific assumption enters the control-plane code. RDS, S3, ECS and the OTel backend are deployment configuration; a self-hosted install with a bundled Postgres and MinIO must stay a first-class, tested target after the migration, not a legacy path that quietly rots.
  - instruction: Keep the bundled-Postgres compose smoke in the test matrix and run it on a credential-free machine after the migration.

## Non-goals

- Not zero open issues forever. New findings will keep arriving — this cycle will produce some itself — and #107 (upkeep runs itself) is the mechanism that turns future findings into runs rather than tracker rows.
- Not the AWS migration (#59). It is an infra and cost decision with #6 and #30 hanging off it, and deploy/aws plus ADRs 0003/0004/0006 already open that lane; this cycle sizes it and hands it to the owner.
- Not the fully autonomous cycle (#109). That issue is the measure of the whole program — the human doing exactly one thing, reviewing the PR — and it closes when a later cycle demonstrates it, not as a task inside this one.
- Not a rewrite of the devague chain into Culture Nodes (#89) as a precondition. The chain runs in this session as it always has; running it as a workflow is its own batch.
- Not bridges-poll-outbound. Inverting the actor dispatch protocol so bridges pull work is what would let agents live outside the LAN, and it changes the section 13 contract across all five bridges. It is recorded as the next architectural step and filed as its own issue, not folded into this decision.
  - instruction: File the issue with the section 13 contract change and the all-backends implication named explicitly.

## Assumptions

- A verify-first bucket exists and is large: #54 looks satisfied by adapters/human-inbox/src/`human_inbox_bridge`/tracker.py (`OBSERVATION_KIND` `github_pr_merged`, submits an observed result on merge), #62 by adapters/colleague/src/`colleague_bridge`/`colleague_cli.py`:71 (--continue `continuation_ref`, citing upstream #167), #61 by the live control plane's published pr-upkeep version 8 whose source names CI check runs as a third feed, #66 by internal/notifier/rundetail.go:160 fetching `workflow_key`, #67 by internal/worker/clarifygate.go, #28 item 1 by internal/api/grades.go + schemas/ledger/grade.schema.json, #8 by internal/api/`actorregister_test.go` + deploy/prod/register-actor.sh, #9 by the `testmain_test.go` files now present in api/engine/worker/notifier, and #48 by internal/worker/{budget,pacing,breaker}.go + scripts/`stickiness_ab.py`. Each is a candidate for closure on evidence, not a certainty.
- \#17's diagnosis may no longer hold: deploy/prod/deploy.sh was rewritten during the last batch and its fallback is now 'ssh build || { go build && ssh mkdir && scp && rm; }' under set -euo pipefail, where the group is the last command of the || list and its failure should abort. The issue is a verification task — reproduce the ETXTBSY case or close it — not an assumed bug.
- \#12 is partially self-closed already: its own checklist marks favicon and full-width layout done, leaving run cost, run names, a workflows view and in-UI authoring. Splitting it into what remains is cheaper than treating it as one open item.
- Fleet capacity is six registered actors — codex-thor and codex-orin on the two agent hosts, plus developer, planner, verifier and intake claude bridges on spark (nodes-op.sh actor table). The four claude bridges draw on the same subscription window as the operator's own session (CLAUDE.md session accounting, #48), so only the two codex actors add capacity that the operator's window does not already pay for.
- Go is still absent on the agent hosts (STATE section 6), so every Go-side claim — including all of #5's telemetry work — needs the operator's gate on spark until #96 makes the capability surface report toolchains and routing respects it.
- The codex write path remains unproven (CLAUDE.md, #18 open): shell exec works, but no dispatch has yet landed a patch under workspace-write with the .git widening measured in STATE section 5. Read-only verification dispatch is therefore safe to fan out now; write dispatch is a bet this cycle can test rather than assume.
- The sweep split collapses part of the backlog rather than adding to it: #71 becomes the trigger case, #107's 'start the first run' becomes the same trigger plus a schedule, #61 and #76 become two more emitters against one contract instead of two more vocabularies inside one sweep script, and #108's 'what is running' surface shrinks because a finding's path from raise to run becomes a queryable chain of events. The triage table must record that collapse rather than carrying five independent items.
- The #59 decision changes three other issues' dispositions: #30 closes on RDS automated backups plus S3 artifact durability rather than a new backup script; #6 becomes live work — the RDS and S3 access is exactly the workload-identity case OIDC was deferred for — rather than staying parked; and #5's collector gets an AWS home that thor reaches outbound, needing no inbound path.
  - instruction: Update #30, #6 and #5 with the consequence of this decision as soon as it is recorded, so their dispositions do not drift from it.

## Scope exploration

- `s1` — `internal/telemetry/{telemetry.go,attributes.go} + cmd/nodes/{serve.go:147,worker.go:107,scheduler.go:52}`: OTel is built, not stubbed: an env-gated OTLP tracer+meter with a reviewable attribute allowlist, instrumenting exactly the three seams #5 names, constructed in all three binaries. #5's remaining work is deployment and live evidence, not instrumentation.
  - seeds: `c4`, `c8`
- `s2` — `deploy/prod/ (compose.thor.yml, deploy.sh, *.service)`: grep -rn OTEL deploy/ returns nothing: no collector service, no `OTEL_EXPORTER_OTLP_ENDPOINT` in any unit or compose file, so telemetry.New returns NoOp on every production process and nothing is exported today.
  - seeds: `c4`, `c8`, `c32`
- `s3` — `docs/deliveries/2026-08-15-own-the-work-end-to-end-STATE.md section 11`: The fourteen-row inventory of what the operator did by hand per package, with an issue number against each row and a literal 0 of 9 in the 'decide the proposed claim' row — this list IS the operator-lane half of the backlog, and it is why the bootstrap batch cannot itself run through the system.
  - seeds: `c5`, `c10`, `c13`, `c31`
- `s4` — `docs/deliveries/2026-08-15-own-the-work-end-to-end.md section 1`: Three of six success signals scored NOT MET, and each maps to an open issue on this list (#90 handoff transport, cancel-observed, #99 undecided claims). Closing those issues means answering signals the last cycle deliberately refused to claim.
  - seeds: `c3`, `c34`
- `s5` — `GitHub tracker: gh issue list --state open (44 issues, #5 through #109)`: The backlog is not one kind of thing: roughly 15 verify-or-close candidates, 8 operator-lane enablers, 5 small bug fixes, 4 decision-shaped infra items, and 4 large product bets. A single 'close them all' plan would mis-size every one of those buckets.
  - seeds: `c2`, `c6`, `c29`
- `s6` — `.claude/skills/nodes-operator/{SKILL.md,scripts/nodes-op.sh}`: The operator surface already offers assign/run/ledger/grade/watch/cancel against six registered actors, so a read-only verification dispatch needs no new capability; nodes-op.sh is first-party (authored here, not vendored) so #92/#108 fixes may land in it directly.
  - seeds: `c9`, `c14`, `c26`
- `s7` — `internal/api/ (grades.go, humantasks.go, actorregister_test.go, reviews.go, noderuns.go)`: Several backlog items already have API surface: grades (#28 item 1), human tasks (#54), actor registration (#8). Their issues predate that code, which is why the first move on this bucket is verification rather than construction.
  - seeds: `c23`
- `s8` — `internal/ledger/authority.go + PRD section 10.4`: The refusal half of the authority model works — nothing self-promotes — and #99 must add only the affirmative half. Any design that lets an agent decide its own claim would break the invariant the last cycle proved holds.
  - seeds: `c15`, `c31`
- `s9` — `internal/engine/workflow.go (DecideContinuation) + internal/scheduler/scheduler.go (deadlineContinuationHolds)`: \#95 and #105 are two bugs in the same eight lines — a hardcoded node.state literal and an errored CEL evaluation returning the same zero decision as a false one — so they are one fix with two regression tests, not two batch items.
  - seeds: `c11`
- `s10` — `adapters/{claude-code,codex,colleague}/src/*/server.py`: \#98's scope guard is duplicated across three bridges by the all-backends rule, and #21's single-threaded BridgeHTTPServer is a documented deliberate choice in the codex and human-inbox module docstrings — so #21 is a design reversal to argue, not an oversight to patch.
  - seeds: `c11`
- `s11` — `adapters/human-inbox/src/human_inbox_bridge/tracker.py`: The tracker already polls GitHub, recognises `github_pr_merged`, and submits an observed result — which is exactly what #54 asks for. Its issue is a verification task with a live probe, not a build.
  - seeds: `c23`
- `s12` — `adapters/colleague/src/colleague_bridge/colleague_cli.py:62-72`: `continuation_ref` already appends --continue to the colleague argv, citing upstream #167 — the precise gap #62 filed. Verify against a live colleague run and close.
  - seeds: `c23`
- `s13` — `live control plane at 192.168.1.146:18080 (runs and workflows endpoints)`: The stack is up and working: the newest run is a developer-actor package on PR #106 Sonar findings, and the published pr-upkeep workflow is at version 8 with CI check runs already named in its source — evidence that #61 and parts of the upkeep backlog may already be satisfied in production.
  - seeds: `c23`, `c9`
- `s14` — `examples/pr-upkeep/ (workflow.yaml, sweep.py, fixtures/)`: Fixtures exist for GitHub check runs (#61) and Jira search (#76), so both feeds have at least partial implementations; what #71 and #107 add is a wake signal and a schedule, which are engine capabilities rather than sweep-vocabulary gaps.
  - seeds: `c23`, `c19`
- `s15` — `web/ (139 tracked files) + issue #12's own checklist`: \#12 self-reports favicon and full-width layout as done, leaving four UI items; a partial close plus a smaller successor issue is more honest than carrying it as one open item.
  - seeds: `c25`
- `s16` — `docs/skill-sources.md + .claude/skills/`: Vendored skills are cite-don't-import with an explicit no-edit rule on script bodies, so operator-surface work is confined to the first-party nodes-operator scripts and internal/api.
  - seeds: `c14`
- `s17` — `CLAUDE.md (zero-dep rule, dogfooding reflex, split-plan session accounting)`: Three repo rules constrain this cycle directly: the Python package takes no third-party deps, delegable work must go through nodes assign with an assessment afterwards, and every fan-out declares expected model-session count against one shared subscription window.
  - seeds: `c16`, `c26`, `c18`
- `s18` — `deploy/aws/ + docs/adr/{0003,0004,0006}`: The AWS lane is already opened by ADRs and a lambda-runner Dockerfile with IAM policies, so #59 is not greenfield — but it is still an owner's cost decision, and #6 (OIDC, deferred until a cloud lane exists) and #30 (S3 backup) both hang off whatever is decided.
  - seeds: `c20`, `c12`
- `s19` — `deploy/prod/deploy.sh lines 37-42`: The runner-binary fallback #17 reported was rewritten in the last batch; under set -euo pipefail the fallback group is the last command of the || list, so an scp failure should now abort. #17 needs reproduction against current HEAD before it is either fixed or closed.
  - seeds: `c24`
- `s20` — `internal/worker/{budget.go,pacing.go,breaker.go} + scripts/stickiness_ab.py + docs/adr/0011`: Pacing, the capacity breaker, session stickiness and an economic-budget ADR all exist, which covers most of #48's four asks; #97 then proposes to promote budgets to first-class records, so #48 should be verified and narrowed before #97 is planned on top of it.
  - seeds: `c23`
- `s21` — `docs/deliveries/2026-08-15-own-the-work-end-to-end-STATE.md sections 5-6 (fleet facts)`: Measured constraints that bound every dispatch this cycle: no Go on either agent host, exactly one exact-match allowlisted checkout per codex bridge (so one package per actor at a time), and .git writable only with an explicit per-dispatch widening.
  - seeds: `c27`, `c28`
- `s22` — `issue #59 promoted onto #5's critical path by the q4 decision`: Choosing an AWS-hosted collector means the user's stated first priority (#5) is now blocked on the infra decision they also asked to be given as a brief — so the #59 brief is the first artifact this cycle produces, not a later one.
  - seeds: `c35`, `c37`

## Decisions

- Bucket E (#6 OIDC, #30 S3 backup, #50 headspace egress allowlist, #59 AWS) reaches disposition by decision brief: one costed page per issue — options, dependencies, and what closes if the answer is no — put to the repo owner, who decides. An agent never chooses for them. #6 is in scope despite being omitted from the original list.
- The work splits into three stages against the shared subscription window: (1) a read-only verification sweep over bucket A dispatched through the fleet, (2) the operator-lane bootstrap batch built the old way, (3) later waves that use the new capability on the bug tail, the finish work and the product bets. One mega-batch is rejected.
- The OTel collector's backend is intended to be AWS-managed rather than a permanent fixture on thor, and the #59 decision brief is written first so that intent can be confirmed early — but #5 is not gated on it: the export path is one environment variable, so #5 closes on the first live trace from the deployed control plane regardless of which collector answers.
- The bucket-A verification sweep runs on codex-thor and codex-orin, read-only. Constraints carried from STATE section 6: exactly one exact-match allowlisted checkout per bridge, so one package per actor at a time, and neither host has a Go toolchain — every Go-side claim still needs the operator's gate on spark.
- Stage 2 opens with a single small write package dispatched to codex-thor to prove the write path (#18, unproven since the userns fix). If that package lands a patch, all stage-2 build work routes to codex actors and the operator's session is reserved for gates and merges; if it fails, stage 2 stops and is re-planned rather than absorbed into the operator's window.
- \#5 is built now and does not wait on #59: the collector configuration, endpoint environment and deploy ship behind a single `OTEL_EXPORTER_OTLP_ENDPOINT` variable. The #59 decision brief is the first artifact this cycle produces, so the backend answer arrives before the build needs it. #5 closes on the first live trace from the deployed control plane, wherever the collector ends up running.
- Concurrency is capped at one to two model tasks in flight at a time per subscription, counted across every lane that draws on the same window — the operator's own session, local subagents, and all bridge sessions. The thirteen bucket-A verifications therefore serialize deliberately; wall-clock is traded for not exhausting the window, which is what ended the attempts-evidence cycle mid-wave.
- Sweep is broken into two halves. Sweep itself becomes a pure finding-emitter: it discovers work, holds durable dedup state, and raises an event — nothing else. It carries no triage logic, no merge logic, and no opinion about what should happen next. Workflows with event handlers pick the events up, and the conditions on those handlers decide which workflow reacts to which finding.
- Sweep re-raises only on change, per source key: a GitHub PR raises again only if its head commit changed or a comment arrived since the last raise; a Jira issue only if its updated timestamp moved or a comment was added. Everything else it sees, it has already reported and stays silent about.
- \#59 is decided: authoritative state moves to AWS — RDS PostgreSQL replaces prod-postgres and S3 replaces MinIO — while api, worker, scheduler and notifier stay on thor and keep dispatching to the bridges over the LAN. No VPN, no change to the actor dispatch protocol. Bridges-poll-outbound, the configuration that would let agents live outside the LAN, becomes its own future issue rather than part of #59.
- Where the database lives is a deployment input, never a product decision. Anyone installing culture-nodes points it at their own hosted Postgres — RDS, Cloud SQL, Neon, Supabase, a shared cluster, or the bundled container — and the same applies to where the compute runs: thor, ECS, Kubernetes or a laptop. Our own AWS choice is one deployment's answer, not the system's.

## Hard questions

- risk: One package per codex actor at a time (exact-match allowlist) means thirteen verifications serialize into at least seven sequential rounds across two actors, each with a cold-session tax. The sweep may cost more wall-clock than doing it in the operator's window.
- risk: This cycle will itself produce findings. The last two cycles turned findings into issues faster than they closed them; if that repeats, the tracker ends larger than it started even though every listed issue was dispositioned.
- risk: The verify-first bucket is thirteen issues sized from reading code, not from running it. If half turn out to be partially done, stage 1 does not close thirteen issues — it re-buckets six of them into build work the plan did not budget.
- Stage 2 builds the operator-lane loop the old way, which is the same operator session the last cycle exhausted. What makes this attempt not run out of window at exactly the same point? (resolved: Codex-first, gated on a write-path probe: one small write package to codex-thor opens stage 2; success routes stage-2 build work to codex and reserves the operator window for gates and merges, failure stops and re-plans.)
- The AWS-hosted collector decision puts the user's stated first priority (#5) behind an infra decision that has not been made. Is #5 genuinely first, or is the AWS decision first? (resolved: Build #5 now behind one env var; write the #59 brief first. #5 closes on the first live trace, wherever the collector runs.)

## Open parks

- [unknown_nonblocking] Whether a repair loop (#102) can be bounded safely enough to run unattended is unknown: the last cycle's four gate failures were all cases where the actor could not run the tool that would have shown them, which a repair node on the same host also could not run.

## Resolved vagueness

- [unknown_blocking] Whether #50 can close at all depends on a headspace-cli release supporting egress allowlists; the pinned version in deploy/prod/deploy.sh is installed by uv tool with no version floor, so the current capability is unmeasured. — resolved: Measured, not unknown: headspace-cli 0.11.0 is both installed on thor and the latest PyPI release, with no network flag on its CLI surface. Since agentculture/headspace-cli is first-party, the egress allowlist becomes a cross-repo feature issue plus a recorded interim network:full deviation.
