# Jira operating surface + flow store

> Jira is a peer operating surface for culture-nodes: the sweep replays issue history faithfully instead of sampling point-in-time state, a technically-failed pickup re-arms bounded by the control plane, and proven flows live in a browsable store users pull into their own control plane and extend on their own machine/db
> instruction: Verify from prod records with no session context: a two-comment reply emits two facts in order; a killed pickup re-mints bounded with a ledger-visible derived record then parks on a human; a flow proven here publishes into a second control plane and runs; the operator's board acts (create, answer, decide) each become engine facts with start/finish reports back on the ticket

## Audience

- The operator working from the Jira board as a peer surface, the culture-nodes agent fleet consuming ticket-derived facts, and users pulling proven flows from the store into their own control planes

## Before → After

- After: A ticket carries the whole loop: history-faithful facts (every transition and comment, in order), technically-failed pickups re-arm bounded, consumers report start and finish on the ticket, and the flows that ran it are pullable from a browsable store with their evidence

## Why it matters

- Point-in-time sampling loses human input (comment 10106 skipped live), silent non-consumption makes the board lie by omission (10109/10110 heard by nobody but a polling session), and proven flows are locked inside single deployments - today the loop cannot be operated from where the work is filed

## Requirements

- The sweep emits one fact per UNSEEN changelog entry and per unseen comment, watermarked by history position (changelog id / comment id) instead of comparing current status to last-recorded status — examples/pr-upkeep/sweep.py's fact-1/fact-2 blocks sample point-in-time state today, and Jira Cloud exposes expand=changelog on the read credential the sweep already holds (#193)
  - instruction: Extend sweep.py to read expand=changelog plus full comment pages; emit one fact per unseen changelog entry and per unseen comment, watermarked by history position (changelog id / comment id); acceptance replays: a between-polls round trip emits both transitions in order, a two-comment reply emits two facts, and a resolution-terminal transition (the SCRUM-2 Done case) emits before the issue leaves the JQL window
  - honesty: A To Do round trip completed entirely between two polls emits both transition facts in order, a two-comment reply emits two comment facts, and a resolution-terminal transition (the SCRUM-2 Done case) still emits - replayed from changelog/comment history positions, never sampled from current state
- On TECHNICAL failure of a trigger-created run, the control plane re-mints a run from the same delivered event after backoff, bounded the way internal/repair bounds repairs (N attempts per window, then a human task), with a ledger-visible derived record naming the original event and attempt count (#194; measured live: run 01M0B4K8JSY3QRQJ3M7D2WTZDC consumed its fact, failed in 8s, and nothing re-armed)
  - instruction: Implement at the control-plane trigger seam (internal/engine, bounding pattern from internal/repair): on TECHNICAL failure of a trigger-created run, re-mint from the stored delivered event after backoff, at most N attempts per window, then park on a human task; the ledger carries a derived record naming the original event id and attempt count
  - honesty: A technically-failed trigger-created run re-mints after backoff at most N times per window with a ledger-visible derived record naming the original event and attempt count, then parks on a human; a domain-outcome failure never re-mints
- A store entry is graph PLUS evidence: the proving prod run ids, the deviations recorded against it, and the actors it needs — browsable, pullable into a user's own control plane without hand-editing digests, and coexisting with the user's local additions (#192 acceptance sketch; 'every result has evidence' applies to the catalog too)
  - instruction: Design and ship the registry API: export/import of a flow as graph + evidence manifest (proving run ids, deviations recorded against it, required actors); acceptance: pull a flow proven on thor into a second control plane and publish it without hand-editing digests, alongside locally-added flows
  - honesty: A flow pulled from the store carries its proving evidence (run ids, deviations recorded against it, required actors) and publishes into a foreign control plane without hand-editing digests; local additions coexist with pulled flows
- Operating the system via Jira reaches parity with the operator lane only when human comments on a ticket have a consumer: engine-originated questions round-trip live (t17, SCRUM-1), but a human's answer to an OPERATOR question only emits pr-upkeep.jira.comment with no subscriber, and ticket CREATION has no verb in any lane — the system bridge is comment+transition only behind the c19 allowlist, and this scope's SCRUM-3 needed an ad-hoc widening of the jira-status ssh custody pattern
  - instruction: Ship the board-parity consumers per #197's acceptance: a subscriber that lands human answers to operator questions durably without session polling, an operator-lane create/comment verb with declared custody, and operator-vs-bridge identity separation on the board
  - honesty: An operator can create work, answer questions back and forth, and continue into planning and task assignment from the board, with every such act becoming an engine fact - no operator session polling as a hidden dependency (the bar set on SCRUM-3 comment 10106)
- Jira-originated work reports back to its ticket: an engine-driven start update when a consumer picks the fact up (run id + workflow + trigger event id) and a finish update with the terminal outcome, through the narrow jira bridge, never the emitter (#198; operator direction 2026-08-19)
  - instruction: Engine-driven start and finish reports through the narrow jira bridge for every run minted (or subject-attached) from a source==jira fact, per #198's acceptance: start names run id + workflow + trigger event id, finish names the terminal outcome, both land even when the run starts and finishes between two sweep passes
  - honesty: A run minted from a ticket-derived fact posts a start update (run id, workflow, trigger event id) and a finish update (terminal outcome) through the narrow jira bridge, even when the run starts and finishes between two sweep passes
- Watermark cutover, not just new emission: migrating `signal_event_watermarks` from status-value/comment-timestamp to history-position must seed each known issue's watermark at its CURRENT history head, so the first history-aware pass re-emits nothing already consumed - a naive deploy would replay old transitions and mint spurious (billable) intake runs
  - honesty: The first sweep pass after deploy on a board with prior watermarks emits zero facts for history that predates the cutover, proven by a replay test over recorded prod watermark rows
- Transition facts need the self-echo discipline comments already have: changelog entries carry an author, and the system's own board moves (the intake flow's To Do -> In Progress, changelog entry 10180 on SCRUM-3) must not become trigger-firing facts, or a flow subscribing to transitioned.in-progress fires on its own move
  - honesty: A transition performed by the bridge account (or marker-correlated to a system verb) emits either no fact or a fact flagged as self-originated that exact-match triggers can exclude; the intake flow's own move demonstrably re-fires nothing
- Re-mints pass through the same EnqueueWork seam as triggered and manual runs, so the one-active-run-per-subject ceiling and pacing/breaker gates bound them identically - a re-mint never doubles an active subject and never bypasses the session economy gates
  - honesty: A postgres-backed test shows a re-mint deferred while a subject run is active and admitted after it terminates, through the identical inbound path (no re-mint-only side door)
- Store portability is an actor-mapping problem, not only a digest problem: a pulled flow pins actor://...@sha256 and runner:// ids that do not exist on the importing plane; the store entry's evidence manifest declares required actor/runner capabilities and the import binds them to local registrations without hand-editing the graph
  - honesty: A flow exported from thor imports into a second control plane whose actors have different ids/digests, and runs after a declared mapping step - the graph document itself is byte-identical before and after

## Honesty conditions

- All three legs ship in one plan (the q2 decision) and each is proven on the live prod lane, not in a demo namespace: history-faithful emission on a real ticket, a bounded re-mint on a real technical failure, a store pull between two real control planes
- No re-mint, retry, or consumer-awareness logic lands in sweep.py or the sweep-cycle graph - the sweep's diff for this cycle is emission-only
- The re-mint trigger keys off technical failure exclusively; a domain outcome (`changes_required` and kin) in the test matrix never produces a re-mint record
- The jira-intake v4 loop remains live as demonstrated: SCRUM-3 was picked up hands-free within one sweep interval of creation, acknowledged with a clarifying question, and moved to In Progress (run 01M0D1TE3YZJAAB0DGH0ZQ6QG9)
- Each named audience can run its leg with no other role's tooling: the operator from the board, the fleet from facts, store users from the registry API
- The end-to-end demonstration runs ticket-to-store with zero operator shell commands, verifiable from prod records
- Each cited failure has a regression proof: the 10106 skip caught by history replay, unheard board acts caught by consumed-fact checks, deployment-locked flows caught by a cross-plane pull test
- Every listed signal is measured from prod records alone - no session context or operator memory needed to audit
- Each number in the signal is read from prod records alone - sweep stdout artifacts, run and ledger rows, store API responses - by an auditor with no session context, and the cycle demonstration publishes the measured values beside the record ids they came from

## Success signals

- A human answers on a ticket and the engine consumes it with no operator session polling; a two-comment reply emits two facts; a killed pickup re-arms and the ticket shows the attempt; a flow proven on one control plane runs on another after a store pull - all verifiable from prod records alone
- Measurable: a board act becomes an engine fact within one sweep interval (<=5 min) with ZERO session-polling consumptions across a full cycle; a two-comment reply emits exactly two facts in order; a technically-failed pickup re-mints and parks on a human after exactly its bounded attempt count; one flow proven on thor runs on a second plane after one pull

## Scope / boundaries

- The re-arm lives in the control plane ONLY: the sweep stays a pure emitter — read credentials plus the event-ingress token, no triage, blind to its consumers (sweep-cycle header boundary c5; #194's 'where the fix must not go')
  - instruction: Enforce by a pinning test that sweep.py gains no consumer-side awareness (no event reads, no run queries, no retry logic); the re-mint diff touches only control-plane packages
- A run that fails with a DOMAIN outcome never re-mints: `changes_required` and its kin are answers that stand, not engine failures (PRD domain-outcome-vs-technical-status ground rule; #194 acceptance)
  - instruction: Gate re-mint eligibility on the technical-vs-domain distinction; the test matrix includes a domain-outcome failure asserting zero re-mint records exist for that run

## Assumptions

- The live jira-intake v4 loop is the demonstrated base of Jira-as-operating-surface: pickup open to every transitioned.to-do, Claude drafts acknowledge+question, the bridge posts it and moves the board to In Progress — exercised end-to-end today by SCRUM-3, created 2026-08-19 through the operator lane specifically as this scope's probe
- Jira accepts comments on resolved/closed issues under the default workflow, so #198 finish reports can land after a ticket is Done; if a workflow property forbids it, the finish report needs a declared fallback (reopen is NOT it)

## Scope exploration

- `s1` — `examples/pr-upkeep/sweep.py (fact emission + watermark blocks, lines ~920-980)`: fact 1 is always-attempted current status deduped by control-plane watermark equality; fact 2 is NEWEST comment only — a To Do round trip inside one 5-minute poll interval emits nothing, and older unseen comments in the interval are skipped (measured live 2026-08-18, recall: hands-free-loop-live-debug-ladder item 7)
  - seeds: `c2`
- `s2` — `internal/engine/trigger.go + internal/repair (bounding pattern)`: trigger matching is exact per onEvent entry and the engine already holds both sides of the joint — delivered event, minted run, terminal status; internal/repair's 2-attempts-per-24h-window-then-human is the committed bounding shape #194 reuses (CLAUDE.md t32/#102)
  - seeds: `c3`
- `s3` — `examples/pr-upkeep/sweep-cycle.workflow.yaml header (emitter custody)`: the emitter's blindness to consumers is a confirmed custody boundary; what it emits is consumed by other published workflows' triggers and this graph neither knows nor cares which — re-mint logic in the sweep would break c5
  - seeds: `c4`
- `s4` — `docs/initial-design PRD ground rule (domain outcome != technical status) via CLAUDE.md + issue #194 acceptance sketch`: re-mint eligibility must key off the technical/domain distinction the PRD already mandates; #194's acceptance explicitly pins 'a run that fails with a DOMAIN outcome never re-mints'
  - seeds: `c5`
- `s5` — `examples/*/workflow.yaml (17 compiling flows) + GET /v1alpha1/workflows (per-deployment registry)`: the two halves of a store both exist and nothing bridges them: examples/ carries working flows discoverable only by reading the tree with provenance only in comments; the workflows registry is content-addressed but private to one deployment — no import/export with evidence, no browsing surface, no user-local extension story (#192 body, verified against prod: jira-intake v1-4, pr-upkeep v7-10 published)
  - seeds: `c6`
- `s6` — `examples/jira-intake/workflow.yaml v4 + prod state (schedules, actors, SCRUM-3)`: prod has a 5-minute sweep schedule enabled (pr-upkeep-sweep-5m; the 30m one disabled), jira-intake v4 published, claude-intake bridge running on spark (192.168.1.157:8086) — a fresh SCRUM ticket in To Do emits on first sighting because no watermark row exists, so creation alone triggers pickup
  - seeds: `c7`
- `s7` — `examples/jira-question-round-trip + adapters/jira (c19 allowlist) + .claude/skills/jira-status + the SCRUM-3 creation probe`: the round trip resumes ONLY on `originating_question_id` correlation (marker-stamped engine comments); bare comment events have no subscriber; the operator credential on thor can create issues (proved by SCRUM-3) but no skill/bridge verb exposes create or free-form comment — the 'layer between human and ticket' is today a session polling the board
  - seeds: `c8`
- `s8` — `internal/api/signalevents.go (EventDeliveryOut) + prod-api-1/prod-worker-1 logs`: the delivery verdict (resumed/`picked_up`-with-refusals/triggered/duplicate) is returned synchronously to the EMITTER and discarded - the sweep stdout keeps only the count; prod api and worker containers log NOTHING in the delivery window, so a fact consumed by nobody leaves no readable trace anywhere, and even a consumed fact's linkage (run.created `trigger_event_id`) lives only on the operator SSE stream, never the ticket
  - seeds: `c9`
- `s9` — `challenge pass / cheap probe: SCRUM-3?expand=changelog on the sweep credential (thor)`: changelog works per-issue on the existing read pair: paginated (startAt/maxResults/total), monotonic entry ids, newest-first, non-status items included (assignee), and the system's own transition present as entry 10180 - seeded the cutover and transition-self-echo requirements
  - seeds: `c14`, `c15`
- `s10` — `challenge pass / adjacent-systems lens: examples/jira-question-round-trip (decide_reply.py + trigger guard)`: the round trip assumes the NEWEST comment is the answer (payload.answer from `_newest_comment`); per-unseen-comment emission changes that cardinality - the consumer must correlate by `originating_question_id` per emitted comment, not by newest-wins; no claim seeded, flagged for the plan to carry as a task constraint
- `s11` — `challenge pass / concurrency lens: t15 subject dedup + t6 EnqueueWork seam`: triggered and manual runs already share the inbound seam with pacing/breaker; re-mint is a third entrant and must use the same door
  - seeds: `c16`
- `s12` — `challenge pass / security lens: registry API between planes + evidence manifests`: no authn/authz story exists between control planes and evidence carries run ids/ledger extracts; routed as q6 rather than a guessed requirement
- `s13` — `challenge pass / reversibility lens: sweep rollback`: the sweep is URL+SHA256-pinned at dispatch (t16); rolling back history-aware emission is a redeploy of the prior digest plus watermark compatibility both directions - the cutover requirement c14 must state the downgrade half too; clean otherwise
- `s14` — `live lane observation: sweep passes 15:46-16:16Z vs 16:21Z+`: the self-echo marker matches by SUBSTRING on the newest comment: operator comment 10117 merely QUOTING the marker string suppressed SCRUM-3's comment fact for 30+ minutes (emitted dropped 2->1) until the human's unmarked 10118 displaced it (2 again) - live evidence that string-convention identity is fragile, feeding the q7 second-account decision
  - seeds: `c15`

## Open parks

- [unknown_nonblocking] Whether Jira changelog pagination and rate limits allow full-history replay per sweep pass at the 5-minute cadence, or the history watermark needs a bounded lookback window
- [unknown_nonblocking] Whether Jira changelog pagination (startAt/maxResults per issue) stays cheap at 5-minute cadence across a project with long-history issues - the probe proved shape and credential, not cost at scale
