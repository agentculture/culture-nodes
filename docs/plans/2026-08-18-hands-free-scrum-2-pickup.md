# Build Plan — hands-free SCRUM-2 pickup

slug: `hands-free-scrum-2-pickup` · status: `exported` · from frame: `hands-free-scrum-2-pickup`

> A new Jira issue in SCRUM is picked up by the prod control plane hands-free: the sweep's transition fact triggers a published intake workflow, an actor acts on the issue through the engine, and the round trip is verifiable from prod run and ledger records alone — every blockage found on the way is fixed through the system itself

## Tasks

### t1 — Fix #188 in the claim path: a worker whose code-runner tuple is entirely absent must not claim code-node work items

- instruction: Start from cmd/nodes/worker.go's tuple preflight and internal/store/postgres/claiming.go / internal/worker's claim path. Write the failing test first: a worker with no CodeRunner, no RunnerService registry, and no Options.Runner must skip code-kind items and still claim agent/wait kinds. Do not touch the partial-tuple refusal. Keep the change in the claim/dispatch path only.
- covers: c5
- acceptance:
  - A new Go test pins that a worker configured with no code runner, no runner-service registry, and no custom Runner never claims a work item whose node kind is code — and still claims agent/wait/approval items
  - The existing partial-tuple refusal in cmd/nodes/worker.go is untouched (its test still passes); absent stays start-able, partial stays fatal
  - go vet and the full go test suite pass; no change outside the claim/dispatch path

### t2 — Widen adapters/jira with a narrow transition verb behind a bridge-enforced allowlist

- instruction: Work only in adapters/jira. Add the transition verb in its own module; config comes from env (project prefix, single target transition name) parsed at startup. Mirror mapping.py's parse/result shape and `post_comment`'s proposed-claim ledger record. Narrow tests/`test_no_transitions.py` to assert the transitions endpoint is reachable only from the new module. Write the decision record in docs/decisions/ naming custody per c19. The bridge stays zero-runtime-dependency (scripts/check-zero-runtime-deps.sh covers adapters/\*).
- covers: c13, h7
- acceptance:
  - `transition_issue` verb accepts only issues matching the configured project prefix and only the single configured target transition, both from deployment env — any other issue or target is refused at parse time with a policy error
  - tests/`test_no_transitions.py` is narrowed, not deleted: it now asserts the transitions endpoint is reachable ONLY from the new verb's module path and nowhere else in the package
  - A committed decision record (docs/decisions/) names the new custody: project prefix, single transition, env keys — per c19's bridge-enforced-allowlist decision
  - The verb's result proposes a claim ledger record exactly like `post_comment`'s (authority proposed, actor origin)

### t3 — Land #189 runner-side: the sweep's emitted count reaches durable, queryable run state without touching the emitter

- instruction: Change internal/runners/headspace (and internal/runners result plumbing if needed) only: capture the code process stdout into the existing `stdout_ref`/`result_payload_ref` artifact fields on the attempt, durable and readable via the public API (attempt artifacts route exists: POST/GET /v1alpha1/attempts/{id}/artifacts). Do NOT modify examples/pr-upkeep/sweep.py or sweep-cycle.workflow.yaml — h9 pins their diff to zero. Test with the fake headspace harness (internal/runners/headspace/testdata).
- covers: c8, h5, c6, h9
- acceptance:
  - The headspace in-process runner captures the code process's stdout as a durable artifact (`stdout_ref` or `result_payload_ref`) linked to the attempt, retrievable through the public API
  - A test proves a green run whose process printed {"emitted": N} is distinguishable from one that printed {"emitted": 0} by one API query
  - git diff shows zero changes to examples/pr-upkeep/sweep.py and zero changes to sweep-cycle.workflow.yaml's graph (c6/h9 boundary)

### t4 — Author + locally validate the jira-intake workflow (new `workflow_key`, staged guard, ceiling, kill-switch runbook)

- instruction: Author examples/jira-intake/workflow.yaml modeled on examples/jira-question-round-trip/workflow.yaml's conventions (header documents deployment config; honest-gap comments where mechanisms are inert). Trigger: onEvent pr-upkeep.jira.transitioned.to-do, when source=="jira" && id=="SCRUM-2" (staged), maxConcurrentSubjectRuns declared+justified. Nodes: intake (claude-intake actor, ledger propose \[claim\], instruction literal telling it to read the issue via `details_url` payload fields and draft an intake comment) -> post-comment (jira actor `post_comment` binding comment from intake output) -> transition (new verb) -> named ends; route every failure outcome; finite limits. Kill-switch runbook in header with exact commands. Validate with uv run nodes validate (or the built Go CLI) locally.
- covers: c2, c3, h8, c11, h10, c21, c22
- acceptance:
  - examples/jira-intake/workflow.yaml triggers on onEvent pr-upkeep.jira.transitioned.to-do with when guard requiring event.payload.source == "jira" AND (staged) event.payload.id == "SCRUM-2"; maxConcurrentSubjectRuns is declared with a one-line justification comment
  - Graph: intake agent node (uses claude-intake, ledger propose \[claim\]) -> post-comment (jira actor `post_comment`) -> transition (jira actor transition verb) -> named end nodes; every failure outcome routes to an end or human node; finite maxDuration/maxTransitions/maxVisitsPerNode
  - nodes validate accepts the document; the file header carries the kill-switch runbook (republish-without-trigger command + runs cancel endpoint) and cites c21
  - jira-question-round-trip's file is untouched by the branch diff (h8); the `workflow_key` is new (c3)

### t5 — Deploy the widened Jira bridge to thor and register company/jira-comment on prod

- instruction: Operator leg on thor via ssh: run deploy/prod/install-secrets.sh for the jira bridge env files (operator supplies credentials interactively — never commit them), then deploy/prod/deploy.sh's `deploy_jira` lane, then deploy/prod/register-actor.sh for company/jira-comment. Verify h16 with GET /v1alpha1/actors + the bridge's capabilities probe. File/annotate the hand-turn issue as you go, per convention.
- depends on: t2
- covers: c20, h16
- acceptance:
  - jira-bridge-jira.env / jira-bridge-auth.env exist on thor (operator supplies credentials), deploy.sh's `deploy_jira` lane installs the widened adapter, and the systemd unit is active
  - GET /v1alpha1/actors on thor resolves company/jira-comment and its endpoint answers its capabilities/health probe (h16)
  - Every manual step is counted: one hand-turn issue (or comment on an existing one) names the secrets install, the registration, and the deploy

### t6 — Redeploy both prod workers with the t1+t3 binary and observe the sweep lottery end

- instruction: Redeploy thor and orin workers from the merged t1+t3 revision via deploy/prod/deploy.sh; confirm each worker's stamped revision. Leave orin's code-runner env absent — that is the point. Then watch GET /v1alpha1/runs for the next 4+ scheduled pr-upkeep-sweep-cycle runs: all green regardless of claimer. Note r2's waiting-not-failing caveat in the observation. Count the hand-turn.
- depends on: t1, t3
- covers: h3
- acceptance:
  - thor and orin workers run the new revision (read back from each worker's stamped revision); orin's worker restarted with its code-runner env still absent
  - The next 4+ consecutive scheduled sweep runs are green regardless of which worker claimed them, read from GET /v1alpha1/runs (h3)
  - The redeploy hand-turn is counted in an issue

### t7 — Publish the intake workflow to prod through the system and read it back

- instruction: From the merged tree: nodes validate examples/jira-intake/workflow.yaml, then POST /v1alpha1/workflows to thor (nodes-operator skill's publish verb). GET the digest back and assert the normalized IR carries the trigger with both guards and the ceiling value. Cite this publish in the through-the-system trail (c9). No side-channel steps.
- depends on: t4, t5
- covers: c2, h2, c9, h18
- acceptance:
  - nodes validate then POST /v1alpha1/workflows against thor succeeds; the response digest matches a local re-render
  - GET of the published digest's normalized IR shows the trigger (event name + both guards) and the maxConcurrentSubjectRuns value with recorded justification (h2, h18)
  - The publish is the system lane end-to-end: no side channel, and the step is cited in the through-the-system trail (c9)

### t8 — Live hands-free measurement on SCRUM-2: create/reset the issue, touch nothing, observe the loop

- instruction: Ensure SCRUM-2 exists, unresolved, in To Do (create or ask the operator to reset it) — then hands off. Watch runs/events on thor across the next sweep firing. Collect: run id, trigger event, ledger record ids (intake claim, `comment_posted`, transition), the Jira comment id + marker, the board transition, and the sweep run's emitted-count query. Every id must GET-resolve on thor at write-up time; report any 404 instead of citing it. r1: a transition refusal is a routed domain failure — report it faithfully and stop for adjudication.
- depends on: t6, t7
- covers: c1, h1, c7, h4, c14, h11, c15, h12, c18
- acceptance:
  - SCRUM-2 sits unresolved in To Do at T0 with hands off thereafter; within one sweep interval plus dispatch, GET /v1alpha1/runs on thor shows a run whose trigger event is SCRUM-2's transition fact (h1, c15, h12)
  - The run's ledger carries the intake actor's proposed claim, the jira actor's `comment_posted` and transition claims; SCRUM-2 shows the marker-stamped comment and the In Progress transition
  - Every cited artifact resolves by plain GET against thor at verification time — run id, ledger record ids, event; anything that 404s is reported, not cited (h4, h11); the delivery evidence also names spark as a required host per the confirmed spark-dependency assumption
  - The green sweep run that emitted the fact answers 'emitted?' via the one-query surface from t3 (h5 live)

### t9 — Open the rollout: drop the staged guard per decision c24

- instruction: Republish examples/jira-intake/workflow.yaml with the SCRUM-2 guard removed (source==jira guard and ceiling stay), read back the IR, then observe the next sweep: report exactly how many To Do issues fired and how the ceiling bounded them. Any surprise routes through /deviate before proceeding.
- depends on: t8
- covers: c2
- acceptance:
  - A second publish removes the SCRUM-2-only guard, keeping the source==jira guard and the ceiling; read-back IR confirms
  - The next sweep's behavior is observed and reported honestly: how many To Do issues fired, bounded by maxConcurrentSubjectRuns — surprises routed via /deviate, not smoothed over

### t10 — Delivery summary + dispositions: close the loop with counted evidence

- instruction: Write docs/deliveries/ per the summarize-delivery skill: planned-vs-actual per task, every hand-turn with its issue number, kill-switch runbook verbatim, spark named as required host. Close #187 with scripts/close-issue.sh --artifact <delivery doc> citing the prod run id; disposition #188/#189; update #118. Check each c18 signal individually — unchecked signals are reported unchecked.
- depends on: t9
- covers: c9, h6, c16, h13, c17, h14, c18, h15, h17, c21
- acceptance:
  - docs/deliveries/ summary records planned-vs-actual, every hand-turn counted with its issue number (h6), the kill-switch runbook commands verbatim (h17), spark named as a required host
  - \#187 closed citing the prod run id; #188/#189 dispositioned with their fix evidence; #118's gap list updated to drop 'no subscriber' (h14); before-state traceability cites the three bug bodies (h13)
  - Each success signal from c18 is individually checked and cited with prod ids, or reported unchecked (h15)

## Risks

- [unknown_nonblocking] Whether the Jira bridge account can execute To Do -> In Progress in SCRUM (permission + transition id) is unverifiable until the first live dispatch (frame park v3); a refusal is a routed domain failure surfacing in t8 (task t8)
- [unknown_nonblocking] After t1, code-node items on a fleet with NO configured code runner wait unclaimed instead of failing — if thor's worker is down, sweeps queue silently until it returns; the plan accepts this (waiting beats false-failing) but t6's observation window must note it (task t6)
- [unknown_nonblocking] Actor presence on prod is stale-prone (developer disconnected, planner/verifier never dialled at challenge time): any dispatch beyond claude-intake and the jira actor must re-probe /v1alpha1/dial-in-presence at execution time, not assume the t17-era topology
- [unknown_nonblocking] Jira credentials for the bridge env files on thor must be supplied by the operator at t5 — they exist nowhere in-repo by design; t5 stalls without them (task t5)
