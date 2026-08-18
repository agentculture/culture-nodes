# hands-free SCRUM-2 pickup

> A new Jira issue in SCRUM is picked up by the prod control plane hands-free: the sweep's transition fact triggers a published intake workflow, an actor acts on the issue through the engine, and the round trip is verifiable from prod run and ledger records alone — every blockage found on the way is fixed through the system itself
> instruction: Verify by creating one fresh unresolved SCRUM issue and then touching nothing: within one sweep interval a run must appear on thor (GET /v1alpha1/runs) whose trigger event names that issue; its ledger must show the intake actor's claims; the issue must show the comment and the transition

## Audience

- The operator (Ori) and the culture-nodes agent fleet — and, as reader, anyone auditing the prod ledger: the loop's acceptance is written for someone who trusts only prod records, not session claims. Issue #118 (the idea-to-shipped loop) is the downstream consumer of this joint

## Before → After

- Before: Today a new SCRUM issue produces nothing: the sweep reads it and emits pr-upkeep.jira.transitioned.to-do to a topic no published workflow subscribes to (#187); half of scheduled sweeps fail on which worker claims them (#188, 44 of the newest 200 runs); and a green sweep's record cannot say whether it emitted anything (#189). The apparent prior success was demo-stack evidence — the t17 run 404s on prod
- After: Creating an unresolved issue in the SCRUM project leads, within one sweep interval (~30 min) plus dispatch time, to: a prod run on thor triggered by the issue's transition fact, an intake comment posted on the issue by claude-intake through the engine, and the issue transitioned on the Jira board — with the entire trail (trigger event, run, ledger claims, actor attribution) readable from prod's v1alpha1 API

## Why it matters

- This is the joint that blocks #118 end-to-end: every piece either side of it is deployed and healthy, so publishing the subscriber turns an idle-but-running system into one that actually starts work from a human's issue. Equally it repairs evidential honesty — the gap hid behind demo-stack proof that read as prod proof

## Requirements

- A new published workflow on thor subscribes to the sweep's bare Jira transition events (pr-upkeep.jira.transitioned.to-do, guarded on event.payload.source == "jira") and routes the issue to a registered agent actor — issue #187's acceptance; nothing on prod subscribes to any pr-upkeep.jira.\* name today (read back live from GET /v1alpha1/workflows: only assign-\*, pr-upkeep v10 guarded to `github_pr`, pr-upkeep-sweep-cycle v1, cross-machine-proof, signal5-merge-as-action)
  - honesty: After publishing, the newest version of the intake workflow key on thor carries the trigger onEvent pr-upkeep.jira.transitioned.to-do with when event.payload.source == "jira" (read back from its normalized IR), and one sweep-emitted transition fact creates exactly one run — the subject key `jira:<site>:<id>:status` prevents a sibling run on the next unchanged sweep
- Issue #188 is fixed before hands-free is claimed: orin's worker either carries a complete `NODES_CODE_RUNNER_`{NAME,REVISION,`ACTOR_ID`} identity or stops claiming code-node work items it cannot run — a sweep that fails on a coin-flip of which worker claims first (44 failed of newest 200 runs) makes every downstream Jira trigger a 50% lottery
  - honesty: After the Go fix and orin redeploy, a worker whose code-runner tuple is ABSENT never claims a code-node work item (pinned by a test), and the scheduled sweep stops alternating: the next consecutive scheduled sweep runs are all green regardless of which worker polls first
- Acceptance is observed on prod, not claimed: a run on thor whose trigger event is SCRUM-2's transition fact, its ledger records showing the actor's proposed claims, and a Jira-side effect visible on the issue — demo-stack evidence is explicitly inadmissible (the t17 run 01M0A5QG2Q0EDG16BEFG9MG4TZ 404s on prod, which is how #187 hid). Verification uses the system's own read surfaces: GET /v1alpha1/runs?…, /runs/{id}/ledger, /events
  - honesty: Every run id, ledger record, and event cited as acceptance evidence resolves via GET against thor's API at verification time — any artifact that 404s on prod is inadmissible, whatever it proved elsewhere
- Issue #189 lands as part of the loop's verifiability: the emitted count sweep.py already prints reaches durable run state (the runner protocol already defines `stdout_ref` / `result_payload_ref` artifact fields), so 'did the sweep see SCRUM-2' becomes one query instead of an inspection
  - honesty: A green scheduled sweep run's durable prod record (run output, artifact, or evidence — whichever layer the plan picks) contains the emitted count, such that emitted:0 and emitted:N are distinguishable by one API query without inspecting worker logs
- Blockages are overcome through the system itself, per the operator's instruction: new capability lands as published workflows and engine dispatches (author -> nodes validate -> POST /v1alpha1/workflows -> trigger/run), delegable work routes to registered actors through assign or the new intake path, and any divergence from this frame is recorded as a first-class deviation — never a silent hand-fix that leaves no countable trail
  - honesty: Every fix this frame ships either landed through the system (authored workflow -> nodes validate -> POST /v1alpha1/workflows -> engine-created run) or, where it could not (Go code fix, bridge redeploy), is counted: a hand-turn issue or deviation record exists naming the manual step — zero silent hand-fixes
- The transition half of acceptance requires deliberately widening the Jira actor surface: adapters/jira today supports exactly one verb (`post_comment`) and tests/`test_no_transitions.py` PINS that no code path reaches a Jira transitions endpoint — an audited custody boundary, not an accident. Adding a narrow transition verb means changing that pinned test on purpose, with a decision record naming the new custody (which transitions, which project), never deleting the audit
  - honesty: After widening, the Jira actor supports exactly two verbs (`post_comment`, `transition_issue` or equivalent narrow transition); the audit test still exists and still pins every OTHER path to the transitions endpoint unreachable; a committed decision record names the new custody (which project, which transitions) — the audit is narrowed, never deleted
- The Jira actor must be DEPLOYED and REGISTERED on prod before the intake workflow can act on an issue: thor's live actor registry has no company/jira-comment at all (probed GET /v1alpha1/actors + /dial-in-presence, 2026-08-18T15:32Z) — the t17 round trip ran it on the demo stack only. deploy/prod/deploy.sh already carries the `deploy_jira` lane (installs adapters/jira, requires jira-bridge-jira.env + jira-bridge-auth.env on the host, and itself warns 'company/jira-comment is not registered; use register-actor.sh and re-deploy')
  - honesty: Before the intake workflow's first live run, GET /v1alpha1/actors on thor resolves a company/jira-comment registration whose endpoint answers its health/capabilities probe, and the jira-bridge systemd unit on the deploy host is active
- A kill switch ships with the subscriber: because only the NEWEST version of a `workflow_key` is a trigger candidate (internal/store/postgres/eventtriggers.go), publishing a version of the intake workflow with the trigger removed disables all future pickup in one POST — and in-flight runs are individually stoppable via POST /v1alpha1/runs/{id}/cancel. The spec's runbook states both, so 'hands-free' is reversible in minutes without touching the sweep or the schedule
  - honesty: The rollback is rehearsed or at minimum stated with exact commands in the delivery summary: which document to publish and which endpoint cancels a run — an operator who has never seen this system can stop pickup with it
- The intake trigger declares a cross-subject concurrency ceiling (maxConcurrentSubjectRuns, shipped in t16 — schemas/workflow/workflow.schema.json + internal/compiler/model.go): a first publish that meets N existing To Do issues, or a bulk issue creation, may start at most the declared number of concurrent intake runs — bounding claude-intake session burn on the shared subscription window (economy lane #48) by construction, not by hope
  - honesty: The published intake workflow's normalized IR carries an explicit maxConcurrentSubjectRuns value, and the chosen value is recorded with a one-line justification against the session window

## Honesty conditions

- Between the human creating the Jira issue and the intake comment + board transition appearing, NO human step occurs: no manual run creation, no manual event POST, no manual dispatch — the sweep schedule, the trigger, and the engine do it all, observed on prod
- The published intake workflow is a NEW `workflow_key`; jira-question-round-trip's trigger and guard are untouched by this frame's diff
- This frame's diff changes neither sweep.py's emit logic nor sweep-cycle.workflow.yaml's graph; every event keeps its name and source key
- The intake workflow declares finite limits (maxDuration, maxTransitions, maxVisitsPerNode) and every failure or exhaustion outcome is routed to an end or human node — nodes validate accepts no unrouted or unbounded path
- Every acceptance artifact is readable by a plain GET against thor's v1alpha1 API — no session context, no local demo stack, no operator memory required to audit the loop
- Measured live at least once end-to-end: issue created, then hands off — the comment and transition appear within one sweep interval plus dispatch time, and the run id is recorded in the delivery summary
- The before-state is the filed measurements of #187/#188/#189 (2026-08-18, thor) — each issue body cites its reproduction, and this frame's work is traceable to exactly those three defects plus the intake gap
- On completion, #187 closes citing prod evidence, and #118's open-gap list no longer contains 'no subscriber for Jira facts'
- Each success signal is individually checked and cited with prod ids (run id, ledger record ids, Jira comment id) in the delivery summary — a signal not checked is reported as unchecked, not assumed
- The delivery summary names spark as a required host for the intake leg (alongside thor and orin), and a spark-down dispatch produces a visibly failed/parked node run on prod, not a hung run with no record

## Success signals

- A fresh SCRUM issue (SCRUM-2 or a sibling created as the test) shows a system-authored intake comment AND a board transition it did not get from a human; a prod run id resolves on thor whose trigger event is that issue's transition fact and whose ledger carries the actor's proposed claims; scheduled sweeps stop failing on orin's claim lottery; and a green sweep's run record answers 'emitted?' in one query

## Scope / boundaries

- The existing jira-question-round-trip workflow is NOT the missing subscriber and must not be un-guarded: its trigger on pr-upkeep.jira.transitioned.needs-answer deliberately declines bare sweep transition events (guard requires event.payload.`question_id` and question, PR #180 review finding) — the new intake workflow is a separate document consuming the bare transition contract
- The sweep emitter and sweep-cycle workflow stay as-is: sweep-cycle.workflow.yaml declares the emitter pure (read credentials + event ingress only, no triage, no merge credential, boundary c5 of its own spec) and explicitly does not know its consumers — the fix is a new subscriber, never renaming Jira facts onto pr-upkeep.pr or teaching the emitter about listeners
- Hands-free never means unbounded: every automated leg keeps the system's existing ceilings — repair's 2-attempts-per-24h reaching a human, the round-trip's two-asks-then-approval-node shape, wait nodes that park on silence rather than guessing — and the ledger authority model holds (actors propose, humans confirm; an agent's done is a claim, not evidence)

## Non-goals

- No CI configuration changes (.github/ stays untouched — a repair dispatch may never modify CI, internal/repair rule), no PRD-scale control-plane features beyond the minimal #188/#189 fixes, and no attempt to close #18's codex write path inside this frame: if picking up SCRUM-2 requires landing a patch, the write leg routes to an actor with a proven write path or parks on a human, it does not silently extend scope

## Assumptions

- The bare transition event payload is sufficient for an intake node's input contract: sweep.py's `jira_work_items` emits {source: jira, id: `<SCRUM-N>`, project, severity, kind, title, status, `details_url`} and raises `pr-upkeep.jira.transitioned.<status-slug>` on source key `jira:<site>:<id>:status` with a status watermark, so an unchanged status is deduped by the control plane, not by the emitter
- Trigger event-name matching is EXACT (internal/engine/trigger.go: trigger.OnEvent != ev.Name, no wildcard) — #187's 'consumes pr-upkeep.jira.transitioned.\*' cannot be implemented literally; the intake workflow declares one onEvent entry per status slug it handles (at least transitioned.to-do). Run-stampede protection already exists: the control plane's watermark-equality dedup silences unchanged statuses, and one-active-run-per-subject (t15/c31) holds when the event carries a subject
- The prod hands-free loop has a hidden host dependency on SPARK: claude-intake is a push-transport actor (protocol http, endpoint 192.168.1.157:8086 — spark's LAN address, probed alive and auth-gated 2026-08-18) whose systemd unit and pinned workspace worktree (.worktrees.culture-nodes/owe-intake, currently checked out at db13388) live on the dev machine. A spark outage stalls the intake leg; the run then fails or parks along its routed edges (c11) rather than silently — accepted for now, the operator's machine is part of prod's actor fleet

## Scope exploration

- `s1` — `prod thor GET /v1alpha1/workflows (live read-back)`: 10 workflow keys published; no trigger consumes pr-upkeep.jira.\*; pr-upkeep v10's when-guard requires source == `github_pr`, double-locking Jira facts out
  - seeds: `c2`
- `s2` — `examples/jira-question-round-trip/workflow.yaml`: trigger guard has(event.payload.`question_id`) && has(event.payload.question) exists precisely to decline the sweep's bare transition events; publishing it unchanged would not fire for SCRUM-2
  - seeds: `c3`
- `s3` — `examples/pr-upkeep/sweep.py (jira_work_items, jira_transition_event_name, emit loop)`: payload shape and watermark semantics read from source: transition fact always attempted; comment fact is a different name on a different source key; self-echo comments raise nothing
  - seeds: `c4`
- `s4` — `deploy/prod/compose.orin.yml + cmd/nodes/worker.go preflight`: compose parameterizes the tuple from host .env (empty on orin); worker.go refuses a PARTIAL tuple but permits an ABSENT one, then the worker still claims code work — the preflight hole named in #188
  - seeds: `c5`
- `s5` — `examples/pr-upkeep/sweep-cycle.workflow.yaml (header + exit contract)`: emitter purity and 'consumed by OTHER published workflows' triggers; this graph neither knows nor cares which' read from the file header
  - seeds: `c6`
- `s6` — `prod thor /v1alpha1/version + /actors + api routes (internal/api/server.go)`: prod is serving revision 7d6a4a520f44 (build-flag stamped); actors registered include `claude_intake`/planner/developer/verifier, codex register entries, headspace runner, human ori; read surfaces for verification exist (runs, ledger, events SSE, node-runs)
  - seeds: `c7`
- `s7` — `internal/runners/result.go artifact fields`: `stdout_ref` and `result_payload_ref` exist in the runner result protocol; the sweep run record on prod carries neither, so the emitted count is measured then discarded at the boundary
  - seeds: `c8`
- `s8` — `.claude/skills/nodes-operator/SKILL.md (validate/publish/assign lane)`: the operator lane already covers author + server-side validate + publish + create-run + assign against the public v1alpha1 API; publishing to prod is the live-deploy step the user has now put in scope for this work
  - seeds: `c9`
- `s9` — `CLAUDE.md conventions (repair boundary, #18 write path unproven)`: a failing gate implicating .github/ always routes to a person; codex write path unproven until a workspace-write dispatch lands a patch
  - seeds: `c10`
- `s10` — `examples/jira-question-round-trip/workflow.yaml (bounds + needs-human) + internal/repair/doc.go per its citations`: both existing ceilings reach a human; maxVisitsPerNode structural bound is the mechanism, engine continuation counters are measured-inert for signal-parked shapes (Honest Gap 2)
  - seeds: `c11`
- `s11` — `internal/engine/trigger.go (TriggerEvent) + eventtriggers.go`: exact string match on event name; only the newest version of each `workflow_key` is a trigger candidate; per-subject advisory-locked single-active-run exists on the pickup path
  - seeds: `c12`
- `s12` — `adapters/jira (mapping.py verbs + tests/test_no_transitions.py)`: only verb is `post_comment`; an AST-level audit test asserts the transition endpoint is unreachable across the whole package — the user's chosen acceptance collides with this pinned boundary and must widen it deliberately
  - seeds: `c13`
- `s13` — `challenge pass / adjacent-systems lens: thor actor registry + deploy/prod/deploy.sh deploy_jira`: no Jira actor registered on prod; the deploy lane and register script exist and name the exact steps; the missing pieces are secrets on the host and one registration
  - seeds: `c20`
- `s14` — `challenge pass / reversibility lens: eventtriggers.go newest-version candidacy + runs cancel route`: rollback = republish without trigger (append-only, no deletion needed); per-run cancel exists; the sweep and schedule need no changes to stop pickup
  - seeds: `c21`
- `s15` — `challenge pass / concurrency+economy lens: workflow.schema.json maxConcurrentSubjectRuns + intake bridge always_async`: the ceiling exists as first-class trigger config; without it, publish-day fan-out equals the count of unresolved To Do issues, each a claude session
  - seeds: `c22`
- `s16` — `challenge pass / hidden-dependency lens: actor endpoint_ref + intake.json + owe-intake worktree`: intake bridge active on spark, workspace allowlist resolves, `always_async`; spark is a single point of failure for the intake leg and nothing on thor can dispatch intake without it
  - seeds: `c23`
- `s17` — `challenge pass / feedback-loop lens: adapters/jira mapping.py marked_text + sweep.py jira_comment_is_self_echo`: clean pass — the self-echo loop is closed by construction: every bridge-posted comment carries the culture-nodes:jira-actor marker and the sweep filters marked comments regardless of which Jira account posted them; no new claim needed
- `s18` — `challenge pass / observability lens: notify-discord actor + #189 requirement + run/ledger read surfaces`: clean pass — sweep observability is already c8's requirement; per-run visibility comes from existing read surfaces; notify-discord is registered for step notifications if the plan wants them; residual: nothing pages a human when a run parks on a failure edge, the human-inbox tracker covers approval nodes only
- `s19` — `challenge pass / overlooked-actors lens: dial-in presence full list`: clean pass with one note — company/developer shows disconnected (last seen ~6h), planner/verifier/operator-claude never dialled; none are on the intake path, but any plan task that routes work to them must re-probe presence first, not assume the t17-era topology

## Decisions

- Transition custody is bridge-enforced: project prefix + single target transition come from deployment configuration, refused at the bridge boundary, not trusted to workflow documents or actor instructions
- Rollout is staged (SCRUM-2-only guard first, then open) and To Do re-entry deliberately re-fires pickup as re-triage

## Hard questions

- Does the transition verb need its own guard against transitioning issues outside the SCRUM project or outside the to-do->in-progress edge — i.e. what stops a buggy or prompted actor from moving arbitrary board state? (resolved: Bridge-enforced allowlist: the Jira bridge itself refuses any transition outside the configured project prefix (SCRUM-) and the single configured target transition (-> In Progress), supplied as deployment config exactly like `jira_bot_account_id` — the same narrow-custody pattern that makes `post_comment` the only verb today. Workflow guards may add to this but the bridge is the enforcement point, so no actor prompt or sibling workflow can widen board-state custody.)

## Open parks

- [unknown_nonblocking] Which layer should durably record sweep.py's emitted count for #189 — the headspace runner uploading `stdout_ref`/`result_payload_ref` artifacts, an output binding in sweep-cycle's workflow document, or a new observed-evidence field — is undecided; the runner protocol already defines the artifact fields
- [unknown_nonblocking] SCRUM-2's existence and current Jira status slug are unverified from this machine (Jira read credentials live on the prod hosts, not spark); the exact transition event name it raises can be read back live from prod's event surfaces during a sweep firing before the intake trigger is finalized
- [unknown_nonblocking] Whether the Jira bridge's account can actually execute the To Do -> In Progress transition in the SCRUM project (permission, available transition id from the current status) is unverifiable from spark — no Jira credentials here; the first live dispatch settles it, and a refusal is a routed domain failure, not a silent stall
