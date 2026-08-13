# Build Plan — economy-discord-graphs

slug: `economy-discord-graphs` · status: `exported` · from frame: `economy-discord-graphs`

> Culture Nodes runs economically and legibly: runs post live updates to Discord as devex does; model actors keep warm sessions and measurable cache economics; dispatch is budget-aware so a build never starves the operator's own Claude window; bigger work packages route to codex actors by default; a human's real-world action (merging the PR) completes their task without a second submission; failed nodes preserve their changes; and the dashboard self-refreshes with a living Node Graphs tab

## Tasks

### t1 — Extend attempt usage: expand-only migration adding `usage_cached_input_tokens`, `usage_reasoning_tokens`, `usage_model`, `usage_thread_id`, `termination_reason`; actors.Usage + ToEngine + engine.Usage; insertAttemptSQL params; ADR amending PRD section 13.2

- covers: c2
- acceptance:
  - migration applies over N-1 rows with no backfill and a round-trip test persists and reads all five new fields as nullable
  - ADR committed citing the ADR-0008 precedent for protocol-field additions

### t2 — Expose extended usage: usage rollups, NodeRunUsages, and actor stats aggregate the new fields; openapi Usage component + API types; web usage domain computes and renders `cache_ratio` in Statistics

- depends on: t1
- covers: c2, h1
- acceptance:
  - run detail and actor stats responses include `cached_input_tokens` and a computable `cache_ratio`, rendered in the web Statistics view
  - web mergeUsage mirrors the server aggregation rules for the new fields (unit test)

### t3 — Bridge usage honesty (all three backends): emit `cached_input`, reasoning, model, thread or session id, and termination reason where the backend reports them; codex captures usage from failed and incomplete turns; truly absent usage emits NO block, never zeros; colleague cache fields stay null

- depends on: t1
- covers: c25, h16, h1
- acceptance:
  - codex fixture round-trips `cached_input_tokens` 9984 of 13880 into the completed payload
  - a codex turn.failed test shows either real usage or an absent block counted as `attempts_not_reported` — zero-filled usage is unrepresentable
  - colleague mapping test pins cache fields as null, not zero

### t4 — Engine continuation carriage: `continuation_ref` lands on InvocationRequest and CompletedPayload, persists per attempt, and dispatchActor populates the prior ref for the same session key

- depends on: t1
- covers: c3
- acceptance:
  - engine test: the next attempt's request carries the ref the prior completion returned; absent refs dispatch clean
  - async completion persists the ref on the attempt row

### t5 — Bridge session resume (all three backends): claude resumes with its captured session id, codex with its exec resume verb; the ref returns on sync AND async terminal payloads; `session_key` becomes a transport key excluded from Bound-inputs injection in all three servers; colleague honestly returns a null ref and an upstream feature request is filed

- depends on: t3, t4
- covers: c24, h15, h2
- acceptance:
  - per-bridge test pins the resume argv given a prior ref, and the async terminal payload carries `continuation_ref`
  - transport-key test proves `session_key` never appears in the prompt text
  - colleague bridge returns ref null and the upstream issue link is recorded

### t6 — Session-key serialization and bridge concurrency config: exactly one in-flight invocation per session key (concurrent same-key work queues or deliberately forks, recorded as such); in-flight caps join the shared bridge config surface

- depends on: t5
- covers: c44, h37
- acceptance:
  - two simultaneous same-key dispatches never interleave turns on one provider thread (bridge test)
  - concurrency config keys parse with sane defaults in all three bridges

### t7 — Stickiness A/B gate: ten representative tasks through fresh sessions vs one resumed thread; comparison artifact (uncached input, cached input, sessions, failures, wall time) recorded; stickiness stays opt-in until the artifact shows uncached-input reduction

- depends on: t2, t5
- covers: c42, h35, h2
- acceptance:
  - comparison artifact committed with the five measured columns and a verdict
  - default-on is config-gated on the artifact's verdict

### t8 — Capacity error class: section 13.5 gains `capacity_exhausted`; provider quota, rate, and session-limit failures classify to it; the class and Retry-After persist on the attempt instead of being discarded in-attempt

- covers: c4
- acceptance:
  - classification tests map provider-limit response shapes to `capacity_exhausted`
  - the attempt row carries the error class and any Retry-After value

### t9 — Circuit breaker: new mutable `actor_availability` table (actors stays append-only); dispatch-site check defers `work_items`.`available_at` instead of failing the attempt; pause emits an engine event; paused state renders on the actors API; manual clear via API and CLI

- depends on: t8, t4
- covers: c4, h3, c45, h38
- acceptance:
  - a forced provider-limit failure pauses the actor and zero further dispatches reach it until expiry (worker + store test)
  - the pause is visible on the actors surface with reason and until-when, and clearable without touching the database

### t10 — Pacing: durable dispatch-rate state (per actor and global) consulted at the dispatch site, honored identically across horizontally-scaled workers; reset-clock arithmetic spreads sessions across the remaining window

- depends on: t9
- covers: c5, h4, c43, h36
- acceptance:
  - two worker processes sharing one database jointly honor a declared rate (integration test)
  - a wave started mid-window schedules per remaining-window arithmetic (unit test)

### t11 — Budget contract: budget.`max_sessions` and `max_uncached_input` as declared workflow limits (schema, engine.Limits, run pinning); pre-invoke enforcement refuses unfundable dispatch as a routable domain outcome; counting charges cold session starts, not resumed warm turns

- depends on: t9
- covers: c6, h5
- acceptance:
  - a workflow declaring an unfundable wave is refused at dispatch as a domain outcome that routes an edge (worker test)
  - a warm workstream of N node turns consumes 1 from `max_sessions`, not N (engine test)

### t12 — Routing and granularity guidance: the split-plan lane and nodes-operator skill document work-package node sizing, a per-wave model-session-count declaration against the remaining window, and the codex-first default for big analysis and build packages

- covers: c28, h18
- acceptance:
  - assign-to-workforce and nodes-operator guidance updated; the split-plan template carries a session-count-per-wave declaration

### t13 — Webhook layer (Go port of devex's design): env resolve `CULTURE_NODES_WEBHOOK_URL` falling back to `DISCORD_WEBHOOK_URL`; minimal-metadata Discord embed (run id, workflow, state, actor, dashboard link only); one bounded 5s fail-open POST, no retries, no redirects; outcomes journaled without URL or payload; hermetic test pattern with a separate TEST env var

- covers: c16, h12, c33, h22, c40, h33
- acceptance:
  - payload-shaping tests prove no embed field derives from ledger records, node output, or workflow input
  - the live-network test gates on a separate TEST url env var and an autouse fixture unsets production vars for the whole suite

### t14 — Discord notifier daemon: out-of-process SSE consumer over the cross-run events feed with a durable last-event-id cursor and stable-id dedup; human-readable detail fetched via the read API; zero control-plane changes

- depends on: t13
- covers: c7, h6, c39, h32
- acceptance:
  - kill-and-restart mid-run posts exactly one Discord message per lifecycle event (restart test over the journal)
  - the daemon builds and runs without any control-plane code change

### t15 — Observable declaration: authoring convention binds an observe block (kind `github_pr_merged`, pr number) into the human-task node's input; example workflow updated; the tracker contract documented

- covers: c11, h8
- acceptance:
  - a declared observable round-trips into the bridge's stored `extra_input` untouched (bridge test)
  - the example workflow carries the declaration bound from a prior node's output

### t16 — Merge tracker beside the human-inbox bridge: stdlib-only poller holding `GITHUB_TOKEN`, watches declared observables, auto-submits ONLY the merged state through the existing submit surface as an observed-submission claim (`collection_method` `github_pr_merged`, merge commit named); closed-unmerged and undeclared tasks stay manual; manual submit still works on observed tasks

- depends on: t15
- covers: c12, h9, c34, h23, c47, h40, c14, h10
- acceptance:
  - a merged PR completes its task with the observed-submission claim naming the merge commit (tracker test on recorded fixtures)
  - a PR closed without merging does not auto-complete
  - manual submission on an observed task succeeds unchanged

### t17 — Human-merges doc revision and credential gate: rewrite the pr-upkeep README's human-merges section to define approved as the observed merge; add a gate proving the control-plane process holds no GitHub credential and never calls the GitHub API

- depends on: t16
- covers: c13, h25
- acceptance:
  - the README section is rewritten to name observed merges
  - a grep gate over control-plane sources finds no GitHub credential or api.github.com caller

### t18 — Nudge transport: one discord-bot-cli thread per parked human attempt (create + mention), tracker-owned cadence with its own throttle, reply relay via channel-messages polling; severity color, username, and timestamp embed vocabulary lifted from steward's discord-notify

- depends on: t16
- acceptance:
  - a parked human attempt produces one thread and one nudge post (fixture test)
  - a reply posted in-thread is relayed to the run as a progress update

### t19 — Parallel-tokens design pass (issue 43 in full): multi-edge transition plans, join-barrier node-run state, per-token bounds, signal replay and catch-up, and the mid-execution emission surface — an ADR-grade design doc with the engine test matrix

- covers: c49
- acceptance:
  - design doc committed enumerating state transitions, bound semantics, and the test matrix
  - an independent review (ask-colleague) is recorded against the doc

### t20 — Split/join engine implementation: maxParallelTokens honored, multi-edge splits, join barriers, per-token bounds enforced

- depends on: t19
- covers: c49, h42
- acceptance:
  - a workflow declaring maxParallelTokens above 1 runs concurrent tokens through a split and reconverges at a join with bounds enforced (engine tests)

### t21 — Event-driven continuation: any-node event pickup, non-blocking mid-execution emission, and replay/catch-up closing the migration-0016 event-then-subscription gap

- depends on: t20
- covers: c49, h42
- acceptance:
  - an event emitted BEFORE its subscription still resumes the late subscriber (replay test)
  - a node emits an event mid-execution and keeps working (emission test)

### t22 — Plan ingestion: MapPlanShow (per-task status and real dependency edges) and MapDeviations (origin user or llm) join internal/devague; an import API and CLI verb; durable plan, wave, and task state

- depends on: t10
- covers: c10, h7, c15, h11
- acceptance:
  - an imported plan round-trips with per-task status and real dep edges — not the lossy waves view (round-trip test)
  - deviations import carrying their origin; malformed plans are refused with a hint

### t23 — Implement-Plan dashboard view: plan, waves, and task status over the ledger-projection substrate; deviations render with origin user vs llm visibly distinguished using the AuthorityChip vocabulary

- depends on: t22
- covers: c10, h7, c15, h11
- acceptance:
  - the plan view renders tasks, waves, and deviations from a fixture with the origin distinction visible (vitest + e2e)

### t24 — Generic decompose pipeline: document to claims (with sources) to connected decisions and actions to end verification, as engine surfaces; proven end-to-end on a non-code domain — a newsletter run from web scope through claims and an article plan to written, verified output with sources tracked throughout

- depends on: t21, t22
- covers: c30, h19
- acceptance:
  - the non-code demo run completes with a source on every claim and a verification node at the end, recorded as a delivery artifact

### t25 — Preserve-on-failure (bridge-side, all three backends): a plumbing commit — write the tree, commit-tree with the failure reason, update-ref the preserve branch — that never touches HEAD, the index, or the working tree; push best-effort; the failure payload records pushed-vs-local-only

- depends on: t6
- covers: c26, h17, c41, h34
- acceptance:
  - after a preserve commit, git status, HEAD, and the index are byte-identical to the pre-preserve state (test)
  - the preserve commit message carries the failure reason; a failed push leaves the local commit and records local-only

### t26 — Preserve branch surfacing: branch name minted by code, persisted on the attempt row (migration), returned by the API, rendered as a link on the run detail page

- depends on: t25, t22
- covers: c32, h21
- acceptance:
  - a failed run's detail page links the preserve branch; the DB row carries the branch name (store + web test)

### t27 — Shared event stream in the web app: one app-wide EventSource provider that views subscribe to and detach from without reconnecting; the runs-scoping query parameter exposed in the API client

- covers: c48, h41
- acceptance:
  - opening every view holds exactly one SSE connection per tab (fixture assertion)
  - subscribe and detach cycles never reconnect the stream (unit test)

### t28 — Node Graphs tab shell: replace the Workflows tab with Node Graphs; sub-tabs Nodes, Node Graphs, Active Graphs on the aria-pressed segmented pattern with URL-param state; the authoring entry stays reachable; Header and e2e pins updated

- covers: c19, h13
- acceptance:
  - the nav renders Node Graphs with three working sub-tabs and /workflows/new stays reachable
  - Header tests and the workflows e2e spec are updated and green

### t29 — Cross-workflow graph catalog: a pure domain parser derives node definitions and graph groupings from published workflow IRs client-side — no new API surface

- acceptance:
  - catalog derivation is unit-tested from the workflows fixture: distinct node definitions and per-workflow graph groupings
  - no new HTTP endpoint is introduced

### t30 — Auto-refresh across views: generalize the reload-trigger idiom over the shared event stream to Runs, Board, Jobs, Inbox, Statistics, Ledger, and the Node Graphs sub-tabs; load conventions preserved — AbortController on every fetch, ready means initial-load-settled, stale-while-revalidate

- depends on: t27, t28
- covers: c22, h14
- acceptance:
  - each view reflects a new server event without a browser refresh (e2e with the SSE fixture)
  - no view nulls a rendered list back to loading during a refetch

### t31 — Active Graphs aliveness: a faint breathing halo on graphs with active tokens and visible periodic-action pulses driven by real SSE events; reduced-motion renders one static frame; the agent-state mirror stays complete; the culture-design check passes

- depends on: t27, t28, t29
- covers: c31, h20, c21, h26
- acceptance:
  - the halo renders only for graphs whose runs hold active tokens (fixture test); every pulse traces to a committed event
  - reduced-motion renders a static frame; check-culture-design and the webglass gates stay green

### t32 — Headspace allowlist watch: keep the no-proxy boundary; the sweep's declared egress-allowlist intent stays documented in the workflow; when headspace ships the allowlist, restore it on the sweep and pin a runner conformance fixture

- covers: c29, h27
- acceptance:
  - no proxy code exists in the control plane; the sweep workflow documents the declared intent and the tracked upstream follow-up

### t33 — Delivery verification: map each of the eight success signals to mechanical evidence; produce the audience-to-surface map and the after-state cross-map; cite the recorded live-build pain (deviation d1, issues 47 and 48) in the delivery summary

- depends on: t7, t11, t12, t14, t16, t17, t18, t21, t24, t26, t30, t31, t32
- covers: c1, h24, c35, h28, c36, h29, c37, h30, c38, h31
- acceptance:
  - the delivery summary maps every signal to its evidence, every audience to a shipped surface, and every after-state clause to a requirement — orphans cut or demoted

## Risks

- [unknown_nonblocking] Bridge-host push credentials unverified: the preserve push leg assumes thor and orin service environments hold a writable git credential — never checked; local-commit fallback carries until ops confirms (task t25)
- [unknown_nonblocking] Daemon placement and secret custody: where the notifier and merge tracker run (thor beside the API vs spark) is undecided; webhook URL and bot token live in an interactive shell today (task t14)
- [unknown_nonblocking] Full split/join is the largest engine change since phase 0; the design pass may surface schema or state-model needs beyond the current groundwork (task t20)
- [unknown_nonblocking] Colleague upstream resume support has no committed timeline; stickiness ships two-backends-real, one-backend-honest until it lands (task t5)
- [unknown_nonblocking] GitHub API rate limits bound the merge tracker's polling cadence across many parked tasks; cadence needs a budget before the tracker scales (task t16)
