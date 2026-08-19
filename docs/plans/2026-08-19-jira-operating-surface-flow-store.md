# Build Plan — Jira operating surface + flow store

slug: `jira-operating-surface-flow-store` · status: `exported` · from frame: `jira-operating-surface-flow-store`

> Jira is a peer operating surface for culture-nodes: the sweep replays issue history faithfully instead of sampling point-in-time state, a technically-failed pickup re-arms bounded by the control plane, and proven flows live in a browsable store users pull into their own control plane and extend on their own machine/db

## Tasks

### t1 — Sweep emits history-position facts: one fact per unseen changelog entry and per unseen comment, in order, watermarked by changelog id / comment id

- instruction: Work in examples/pr-upkeep/sweep.py: replace the fact-1/fact-2 point-in-time blocks (~lines 920-980) with history replay from expand=changelog (paginated, monotonic ids) and the comment list. Watermark = last consumed changelog id + comment id per issue. Emit facts in history order. Keep the emitter pure (c4): read creds + event-ingress token only. Fixtures: record real SCRUM-3 changelog/comment JSON via the thor ssh custody pattern before coding.
- covers: c2, h1
- acceptance:
  - A To Do round trip completed entirely between two polls emits both transition facts in order, and a two-comment reply emits two comment facts — unit-tested against recorded Jira changelog/comment fixtures
  - Facts carry history-position watermarks (changelog id / comment id); the fact-1/fact-2 point-in-time status-comparison blocks in examples/pr-upkeep/sweep.py are removed, not bypassed

### t2 — Discovery keeps a resolved issue eligible until its terminal history is consumed: bounded recently-resolved lookback in `fetch_jira_issues` JQL

- instruction: `fetch_jira_issues` (sweep.py ~line 638): add a bounded recently-resolved lookback to the JQL (e.g. resolution IS EMPTY OR resolved >= -Xd where X covers several sweep intervals). The control plane's watermark-equality dedup makes re-reads idempotent — assert that in the test. Reproduce the SCRUM-2 case: resolved between two polls, terminal transition fact still emitted.
- depends on: t1
- covers: c2
- acceptance:
  - An issue resolved between two polls still yields its terminal transition fact (the SCRUM-2 Done case reproduced as a test); re-reads inside the lookback window emit nothing new thanks to watermark-equality dedup (idempotence asserted)

### t3 — Watermark cutover migration: seed every known issue's `signal_event_watermarks` row at its current history head so the first history-aware pass replays nothing

- instruction: Control-plane side: a migration over `signal_event_watermarks` translating status-value/comment-timestamp rows to history-position rows seeded at each issue's CURRENT history head. Replay test uses recorded prod watermark rows (capture them read-only via the API/artifacts first). Coordinate with r4: this ships in the same deploy unit as t1/t2.
- depends on: t1
- covers: c14, h14
- acceptance:
  - A replay test over recorded prod watermark rows proves the first post-deploy history-aware pass emits zero facts for history that predates the cutover
  - The migration is one deploy unit with the history-aware emitter — no window where old watermark semantics meet the new reader

### t4 — Transition self-echo discipline: changelog entries carry an author, and the system's own board moves never become trigger-firing facts

- instruction: sweep.py: changelog entries carry author accountId. Suppress (or flag self-originated) entries authored by the bridge account id — same discipline `jira_comment_is_self_echo` applies to comments, but author-id based, NOT marker-substring (s14 showed substring matching is fragile). Replay SCRUM-3 entry 10180 in the test. r1 (second Jira account) may sharpen identity later — key off configured `bot_account_id` now.
- depends on: t2
- covers: c15, h15
- acceptance:
  - A transition performed by the bridge account (or marker-correlated to a system verb) emits either no fact or a fact flagged self-originated that exact-match triggers exclude; a test replaying SCRUM-3 changelog entry 10180 (the intake flow's own To Do -> In Progress) asserts nothing re-fires

### t5 — Control-plane re-mint of technically-failed trigger-created runs: backoff, N-attempts-per-window bound, derived ledger record naming the original event and attempt count, then park on a human

- instruction: Go control plane, model on internal/repair's bounds (N per window then human task). New code lives beside the trigger/EnqueueWork seam, NOT in the sweep. Re-mint = new run from the same delivered event after backoff; derived ledger record names original event id + attempt count. Domain outcomes (`changes_required` and kin) are answers, never re-mint triggers — table-test the outcome taxonomy. Measured baseline: run 01M0B4K8JSY3QRQJ3M7D2WTZDC failed in 8s, nothing re-armed.
- covers: c3, h2, c5, h9, c4, h8
- acceptance:
  - PG-backed test: a technical failure re-mints after backoff at most N times per window, each re-mint carrying a derived record with the original event id and attempt count, then parks on a human task (mirrors internal/repair bounds)
  - Domain outcomes (`changes_required` and kin) in the test matrix never produce a re-mint record
  - The cycle's diff to examples/pr-upkeep/sweep.py and the sweep-cycle graph is emission-only: no re-mint, retry, or consumer-awareness logic lands sweep-side (asserted at review; the boundary is the diff scope)

### t6 — Re-mints enter the same EnqueueWork seam as triggered and manual runs: subject ceiling, pacing, and breaker gates bound them identically

- instruction: Route t5's re-mints through EnqueueWork itself — no parallel insert path. PG-backed test: subject with active run defers the re-mint, admits after terminal. Coordinate with the #202 trigger-seam failure breaker (r5): one breaker design at this seam covers both watermark-churn mints and re-mints.
- depends on: t5
- covers: c16, h16
- acceptance:
  - PG-backed test shows a re-mint deferred while a subject run is active and admitted after that run terminates, through the identical inbound path — no re-mint-only side door exists in the code (asserted by the test entering via EnqueueWork)

### t7 — Store entry model and private registry API: a catalog entry is graph digest PLUS evidence manifest (proving prod run ids, deviations recorded against it, required actor/runner capabilities), full fidelity, internal server

- instruction: Go control plane + API: store entry = graph digest + evidence manifest (proving run ids, deviation record refs, required actor/runner capabilities). Internal/private server, full fidelity (q6 decision on SCRUM-3 comment 10118). Registry API and repos stay config (q1). Include the collision test: local flows and pulled flows coexist.
- covers: c6
- acceptance:
  - The registry lists entries with graph digest and evidence manifest; an entry created from a proven prod flow carries its run ids and deviation records verbatim (full fidelity per the q6 decision); the server is internal/private
  - Local additions coexist with pulled entries — pulling never overwrites or shadows a locally-authored flow (collision test)

### t8 — Store pull with actor mapping: the import binds the entry's declared actor/runner capability requirements to local registrations; the graph document stays byte-identical

- instruction: Import verb: reads the entry's capability requirements, binds actor://...@sha256 and runner:// ids to local registrations via an explicit mapping step (declared, not inferred). The graph document must hash-compare byte-identical pre/post import (c17/h17) — mapping lives in bindings, never in the graph. r3: the second plane for the live proof is unscoped setup; the task's own test can use two schemas/instances locally.
- depends on: t7
- covers: c17, h17, h3
- acceptance:
  - A flow exported from one control plane imports into a second whose actor/runner ids and digests differ; after a declared mapping step it publishes and runs; the graph document is byte-identical before and after (hash-compared) and no digest was hand-edited
  - The pulled entry carries its proving evidence (run ids, deviations, required actors) into the importing plane's catalog view

### t9 — Board-parity consumers: a human's bare comment on a tracked ticket has a consumer, operator questions round-trip on the board, and ticket creation gets a lane verb

- instruction: Three legs: (1) a subscriber class for bare human comments on tracked tickets (the four dead SCRUM-3 go-signals); (2) a ticket-creation verb in the allowlisted bridge lane (jira adapter has comment+transition today); (3) generalize the t17 marked-question resume past intake. Self-echo via t4's discipline so the consumer never fires on bridge-authored comments.
- covers: c8
- acceptance:
  - A human's bare comment (not an engine-question answer) on a tracked ticket mints a consumer run within one sweep interval — the four dead go-signals from SCRUM-3 (10106/10107/10109/10110) have a subscriber class
  - Ticket creation is available through a lane verb behind the allowlist — no ad-hoc ssh custody widening (the SCRUM-3 gap)
  - An operator question posted to a ticket resumes its flow on the human's marked answer (the t17 mechanism, generalized past intake)

### t10 — Board-driven planning continuation: the spec-chain leg is reachable from the ticket — frame decisions land as marked questions, the human's reply transacts exactly the stated decision, plan and assignment continue from the board

- instruction: Wire examples/spec-chain/workflow.yaml (22 nodes, compiles, unpublished) to ticket facts. Frame decisions -> marked ticket questions; human reply -> transact exactly the stated decision (devague confirm only on correlated human answer — never inferred, the devague contract). r2: declare the .devague/ custody lane before dispatching this; if undeclared by dispatch time, park and route to the operator.
- depends on: t9
- covers: h4
- acceptance:
  - From the board alone an operator creates work, answers questions back and forth, and continues into planning and task assignment; every act becomes a consumed engine fact — zero operator-session polling anywhere in the chain (the bar from SCRUM-3 comment 10106)
  - devague confirmations transact only in response to a human's on-ticket answer correlated to the asking comment — never inferred; frame-state custody runs through a declared checkout lane, not silent operator-tree mutation

### t11 — Start/finish ticket reports: a run minted from a ticket-derived fact posts an engine-driven start update (run id, workflow, trigger event id) and a finish update (terminal outcome) through the narrow jira bridge, never the emitter

- instruction: Engine lifecycle hooks -> transactional outbox -> narrow jira bridge: start update on fact pickup (run id, workflow, trigger event id), finish update on terminal outcome. Never the emitter (c4). EVERY node on the posting path declares ledger.propose \[claim\] — the #202 trap killed 4 runs on exactly this (mapping.py:516/:593 attaches claim records unconditionally).
- depends on: t6
- covers: c9, h5
- acceptance:
  - Both updates post through the narrow jira bridge even when the run starts and finishes between two sweep passes (sub-interval run in the test matrix); the sweep's credentials and code are untouched by this path
  - The start update lands when the consumer picks the fact up, not when the sweep next notices — engine-driven, verified by timestamp ordering against the sweep schedule

### t12 — Regression-proof suite for the cited live failures: every failure named in the spec's why-it-matters maps to a named test

- instruction: One named regression test per cited failure: comment-10106 skip (history replay), unheard board acts (consumed-fact check), cutover replay (t3's test counts). Deliverable includes the failure->test mapping table for the delivery record.
- depends on: t4, t9
- covers: c12, h12
- acceptance:
  - The comment-10106 skip is caught by a history-replay regression test; an unheard board act is caught by a consumed-fact check; the deployment-lock/cutover hazard is caught by the t3 replay test — each cited failure names its test in the plan's delivery record

### t13 — Live prod proof cycle, ticket-to-store, measured from prod records alone: the whole announcement demonstrated on the real lane with zero operator shell commands

- instruction: Operator-lane task, not a dispatch: run the live cycle on prod, zero shell commands in the demonstrated path, then audit purely from prod records (sweep stdout artifacts via GET /v1alpha1/attempts/<id>/artifacts/stdout, run/ledger rows, store API). Numbers to hit are c19's: fact <=5min, two comments -> exactly two facts in order, bounded re-arm visible on ticket, one pull runs on second plane.
- depends on: t3, t6, t8, t10, t11, t12
- covers: c1, h7, c10, h10, c11, h11, c13, h13, c19, h18
- acceptance:
  - On the live prod lane: a board act becomes an engine fact within one sweep interval (<=5 min) with ZERO session-polling consumptions across the full cycle; a two-comment reply emits exactly two facts in order; a killed pickup re-arms bounded and the ticket shows the attempt; a flow proven on thor runs on a second plane after one pull
  - Every number is read from prod records alone — sweep stdout artifacts, run and ledger rows, store API responses — by an auditor with no session context; the demonstration involves zero operator shell commands
  - Each audience runs its leg with no other role's tooling: operator from the board, fleet from facts, store users from the registry API

## Risks

- [unknown_nonblocking] The q7 second/per-agent Jira account mechanism is undecided in detail (API keys vs platform users vs per-machine accounts): author-identity self-echo (t4) and the bridge's own identity depend on it; the marker-correlation fallback exists but was live-demonstrated fragile (s14 substring suppression) (task t4)
- [unknown_nonblocking] Frame-state custody for board-driven planning (t10): the codex write path is proven for repo commits (#18) but no declared lane exists for mutating .devague/ working-tree state from an engine-driven run — the custody story must be declared before the spec-chain leg dispatches, and may need its own design decision (task t10)
- [unknown_nonblocking] The second control plane for the store-pull proof (t8, t13) does not exist yet as a deployment: spark's dev plane is the natural candidate, but standing one up with its own postgres and actor registrations is unscoped setup work that gates the h17/c19 proof legs (task t8)
- [unknown_nonblocking] Deploy sequencing for the cutover: t3's migration and t1/t2's history-aware emitter must land as one deploy unit on thor — the sweep is a uv-tool-installed copy that goes stale silently (the #104/#120 lesson), so a partial deploy replays history into spurious billable intake runs (task t3)
- [follow_up] Bug #202's trigger-seam failure breaker overlaps t6's breaker/gate work: the fix-node ledger rejection is a separate bug-tail PR, but the per-subject failure breaker should be designed once at the EnqueueWork seam, not built twice — coordinate the #202 fix with t6 before either dispatches (task t6)
