# Delivery Summary — economy-discord-graphs

plan: `economy-discord-graphs` · run: `partial` · date: `2026-08-14`
baseline: `devague summary skeleton`

## Intent

> Culture Nodes runs economically and legibly: runs post live updates to
> Discord as devex does; model actors keep warm sessions and measurable cache
> economics; dispatch is budget-aware so a build never starves the operator's
> own Claude window; bigger work packages route to codex actors by default; a
> human's real-world action (merging the PR) completes their task without a
> second submission; failed nodes preserve their changes; and the dashboard
> self-refreshes with a living Node Graphs tab

Ten open issues (#41, #43, #45, #46, #47, #48, #49, #54, #56, plus the webhook
ask) were converged into one spec of 49 claims and 42 honesty conditions, then
a 35-task plan across 7 waves. This run executed that plan.

The run is **partial**: 34 of 35 tasks merged; `t33` is this artifact.

## Planned Work

Quoted verbatim from the `devague summary` skeleton (35 tasks, t1–t35). The
full verbatim list is in the skeleton and in
`docs/plans/2026-08-13-economy-discord-graphs.md`; it is not re-transcribed
here to avoid transcription drift. Task ids referenced below are that plan's.

## Actual Delivery

All 35 accounted for.

| Plan task | Status | What actually landed |
|---|---|---|
| `t1` | delivered | migration 0017, protocol/engine/store carriage, ADR-0009 |
| `t2` | delivered | rollups, actor stats, API `cache_ratio`, Statistics tile |
| `t3` | delivered | bridge usage honesty, no fabricated zeros |
| `t4` | delivered | `continuation_ref` carriage, migration 0018, ADR-0010 |
| `t5` | delivered | resume on claude + codex; colleague corrected later (see d1) |
| `t6` | delivered | one in-flight invocation per session key; fork, never silent |
| `t7` | delivered | A/B harness + artifact; **result negative** — see Drift |
| `t8` | delivered | §13.5 `capacity_exhausted`, non-retryable, Retry-After surfaced |
| `t9` | delivered | breaker, `actor_availability` (0020), dispatch-site deferral |
| `t10` | delivered | durable dispatch pacing, rate state (0022), reset-clock windows |
| `t11` | delivered | declared budget, routable `budget_exhausted`, sessions (0023) |
| `t12` | delivered | split-plan lane, session accounting, codex-first guidance |
| `t13` | delivered | `internal/notify` — devex webhook design ported to Go |
| `t14` | delivered | `nodes-notifier` daemon, SSE cursor, zero control-plane change |
| `t15` | delivered | observable-declaration authoring convention |
| `t16` | delivered | merge tracker beside the human-inbox bridge |
| `t17` | delivered | human-merges rule revised + control-plane credential gate |
| `t18` | delivered | Discord nudge transport for parked human tasks |
| `t19` | delivered | parallel-tokens design doc + engine test matrix |
| `t20` | delivered | split/join engine, per-token bounds, branch reap |
| `t21` | delivered | event-driven continuation, replay (0021) |
| `t22` | delivered | MapPlanShow/MapDeviations, import API + CLI, migration 0024 |
| `t23` | delivered | Implement-Plan dashboard view, origin distinction visible |
| `t24` | delivered | claim-chain verifier + newsletter instance — see Drift on proof scope |
| `t25` | delivered | preserve-on-failure plumbing commit, all three bridges |
| `t26` | delivered | preserve branch on attempt row (0025), API, run detail |
| `t27` | delivered | shared app-wide EventSource, runs-scoped events URL |
| `t28` | delivered | Node Graphs tab shell with sub-tabs |
| `t29` | delivered | cross-workflow node/graph catalog parser |
| `t30` | delivered | dashboard auto-refresh across views |
| `t31` | delivered | Active Graphs breathing halo + committed-event pulses |
| `t32` | delivered | headspace egress-allowlist watch documented |
| `t33` | **partial** | this artifact; the live-test pass is incomplete — see Remaining |
| `t34` | delivered | deploy-lane wiring for notifier + human-inbox (deviation d2) |
| `t35` | delivered | tracker runs unauthenticated; rate headroom (deviation d5) |

Beyond the plan, this run also shipped: the **notify actor** (#68), the
**dogfooding baseline**, five deploy-lane defect fixes, and a
`nodes doctor` sandbox check.

## Mid-work Decisions

- `d1` — colleague CAN resume (`work --continue`, upstream #167), superseding
  t5's null-ref fallback — *"operator correction during wave-1 execution;
  verified against the installed colleague CLI"*. **Initially shipped wrong**:
  the operator briefed t5 to build the superseded fallback anyway. Corrected
  in commit `c29f363` after the deviation was re-read.
- `d2` — the batch shipped two daemons with no deploy wiring; added `t34`.
- `d3` — Fable exhausted its credits mid-wave-1; remaining assignments ran on
  Opus. No scope change.
- `d4` — `capacity_exhausted` was unreachable from real providers; t5 picked
  up the bridge half.
- `d5` — run the tracker unauthenticated against the public repo; added `t35`.
- **Not covered by any record**: the four claude-code bridges on spark were
  running Aug-13 code with no resume support until restarted mid-run. Any
  measurement taken against them earlier would have compared two cold arms.
- **Not covered by any record**: an authorised `FORCE=1` rotation earlier in
  this cycle rewrote `prod.env` wholesale, silently destroying
  `NODES_ACTOR_CLAUDE_TOKEN`. Undetected for ~18 hours. See #69 item 1.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|---|---|---|
| `t5` (`d1`) | shipped the null-ref colleague fallback the approved deviation had already superseded; corrected in-cycle | acceptable |
| `t7` | plan asked for ten tasks per arm; ~20 real sessions were not affordable. Ran 6 per arm, twice, and labelled the N | acceptable |
| `t7` | **the A/B produced a negative result**: 0.0% uncached-input reduction, measured twice. Signal 2's second half is not met | needs-follow-up |
| `t24` | the newsletter proof's four nodes are answered by a scripted actor, not dispatched to a model actor; sources and engine are real | acceptable |
| `t33` (`d2`) | *"Success signal 1 … cannot be demonstrated on thor without it"* — resolved by t34 | acceptable |
| `t33` | three of eight signals are test-proven but not live-proven (3, 5, 6) | needs-follow-up |
| `t16` (`d5`) | shipped requiring a token the operator had chosen not to use; fixed by t35 | needs-follow-up |
| — | **0 of 35 tasks executed through culture-nodes** — the product proved itself, never built itself | needs-follow-up |

## Evidence

- commits: `31bf695..9f6af86` — 121 commits, 37 merges
- tests: root `uv run pytest -n auto -q` — **199 passed**
- tests: bridges — claude-code **200**, codex **183**, colleague **162**,
  notify **85**, human-inbox **130**
- tests: web — vitest **489**, playwright **79** (1 pre-existing flake in
  `jobs-timeline.spec.ts`, passes in isolation)
- lint: `go vet ./...` clean · `gofmt -l .` clean ·
  `black`/`isort`/`flake8` on `culture_nodes tests` clean
- live runs (thor control plane): `01KZZENVDWJ6T0ER7HT9DBT6TQ` (notify,
  completed), `01KZZGN203WBB3NZPYAKGTQMHT` (cross-machine, completed),
  `01KZYB3A41108SDWS623WXWGWY` (split/join, completed), `signal4-check`
  (completed)
- issues opened: #61–#69 · artifacts:
  `docs/deliveries/2026-08-14-dogfooding-baseline.md`,
  `docs/deliveries/2026-08-14-t7-stickiness-ab.md`

### The eight success signals, mapped to mechanical evidence

| # | Signal | Evidence | Verdict |
|---|---|---|---|
| 1 | run completion posts to Discord, no Discord code in the control plane | run `01KZZENVDWJ6T0ER7HT9DBT6TQ` → 204; `tests/lint/webhookisolation_test.go` scans 219 files, 0 violations; operator confirmed receipt | **met, live** |
| 2 | stats expose `cache_ratio`; a resumed session shows lower uncached input | `cache_ratio` shipped (t2, Statistics tile). Reduction **measured at 0.0%, twice** (t7 artifact) | **half met** — see below |
| 3 | a forced provider-limit failure pauses the actor, zero further dispatch | t9 engine tests + t5 bridge classification tests | **test-proven, not live** |
| 4 | `budget.maxSessions` refused at dispatch as a domain outcome | `signal4-check` workflow, completed live on thor | **met, live** |
| 5 | merging a live PR completes its human task, no manual submit | tracker running in production for the first time (fixed this cycle); no PR merged with an `observe` declaration yet | **not yet proven** |
| 6 | a failed node's changes reachable via a branch link | t25 plumbing tests (mutation-verified ×2), t26 store/API/web tests | **test-proven, not live** |
| 7 | every dashboard view reflects new server state without a refresh | t30 tests; operator observed the notify run appear in Active Graphs without refreshing | **met, live** |
| 8 | Node Graphs tab renders nodes/graphs/active-graphs with a breathing indicator, web gates green | t28/t29/t31; vitest 489, playwright 79, culture-design 4/4 | **met** |

Signal 2 is the one worth stating plainly: **the mechanism ships and works —
sessions genuinely resume, verified by `thread_id` collapsing to one across
the warm arm and by cache-growth in the flight feed — but the economic benefit
it was built for did not appear in the metric the honesty condition names.**
Stickiness therefore stays opt-in, which is exactly what h2 required. The
likely cause is a telemetry gap, not a design failure:
`cache_creation_input_tokens` is dropped by the wire protocol and that is
where the cold-vs-warm difference lives.

### Audience → surface map

| Audience | Surface shipped |
|---|---|
| Operators of the control plane | Discord notifier (t13/t14), `nodes doctor` incl. the new sandbox check, breaker resume API/CLI (t9), dispatch-rates surface (t10) |
| Operators reachable via Discord | notifier daemon (t14), nudge transport (t18), notify actor (#68) |
| Workflow authors | declared budget (t11), split/join (t20), event continuation (t21), observable declaration (t15), `require_delivery` routing (#68) |
| The actor fleet (claude/codex/colleague bridges, human inbox) | session resume (t5), session-key serialization (t6), `capacity_exhausted` (t8/d4), preserve-on-failure (t25) |
| Dashboard viewers | Node Graphs tab (t28/t29/t31), auto-refresh (t30), Implement-Plan view (t23), preserve branch link (t26) |
| Per-actor analytics consumers (#28) | **not served** — the attribution gap in the dogfooding baseline is the argument for #28 |

### After-state cross-map

| After-state clause | Requirement | Status |
|---|---|---|
| every run announces its lifecycle in Discord | t13/t14 | met |
| model actors resume warm sessions | t4/t5 | met |
| cache economics measurable per attempt | t1/t2 | met |
| dispatch paces itself through the session window | t10 | met |
| trips a capacity breaker instead of cascading | t9 + d4 | met (test-proven) |
| big packages route to codex by default | t12 | met (guidance only) |
| a human who merges a PR is done | t15/t16/t17/t35 | **not yet proven live** |
| a failed node leaves changes on a recorded, visible branch | t25/t26 | met (test-proven); "pushed" unproven — credentials unverified |
| the dashboard self-refreshes with breathing Active Graphs | t27–t31 | met |

### Dogfooding baseline

**0 of 35 plan tasks (0%) were executed through culture-nodes.** 61 runs and
108 attempts exist across the system's entire history; every workflow among
them is a proof, smoke test or demo. Full analysis, per-actor attempt
breakdown and the five causes are in
`docs/deliveries/2026-08-14-dogfooding-baseline.md`. Next-cycle target and the
six fixes are #69.

## Delivery Claims

| Claim | Confidence | Evidence |
|---|---|---|
| Run lifecycle reaches Discord with zero control-plane egress | high | run `01KZZENVDWJ6T0ER7HT9DBT6TQ` · `tests/lint/webhookisolation_test.go` |
| Split/join runs parallel tokens and joins them | high | run `01KZYB3A41108SDWS623WXWGWY` (5 tokens, 91s, join waited) |
| The control plane can drive actors on another machine | high | run `01KZZGN203WBB3NZPYAKGTQMHT`; 22 successful spark-actor attempts |
| Sessions genuinely resume on claude and codex | high | t7 artifact: `thread_id` 1 vs 6; flight-feed cache growth |
| Resuming reduces uncached input | **unverified — measured and NOT observed** | t7 artifact: 0.0%, two runs |
| A declared budget refuses an unfundable dispatch as a domain outcome | high | `signal4-check` run, completed |
| A capacity refusal pauses the actor | medium | t9 tests; no live provider-limit hit |
| A failed node's work survives on a branch without touching HEAD/index | high | t25 tests, mutation-verified on two bridges |
| A preserve branch is visible on the run detail page | medium | t26 store/API/web tests; not live-exercised |
| Merging a PR completes a human task with no manual submit | **unverified** | tracker only just started running; no observed merge yet |
| The dashboard reflects new state without a refresh | high | t30 tests + operator observation |
| The decompose pipeline generalizes beyond code | medium | t24: verifier reused over t22's fixture; newsletter nodes scripted, not model-dispatched |
| Bridges classify provider quota failures as `capacity_exhausted` | medium | t5 tests; text-matching heuristic, documented as best-effort |

## Remaining Work / Follow-up

- **`t33` live-test pass, partial** — signals 3, 5 and 6 are test-proven but
  not live-proven. Signal 5 is the cheapest to close and should be closed by
  this batch's own PR: park a human task with an
  `observe: {kind: github_pr_merged}` declaration on it, and the merge that
  lands this work completes the task. Owner: operator, at the PR gate.
- **Signal 2 / t7 follow-up** — add `cache_creation_input_tokens` to the wire
  protocol (ADR-0009 lineage: engine + migration + three bridge mappings).
  Likely the actual site of stickiness's economic benefit, invisible today.
  Stickiness stays opt-in until then.
- **Dogfooding** — ≥1 plan task end to end through culture-nodes next cycle,
  ledger records as the completion evidence (#69).
- **#69 items 1–4** — rotation must merge not replace; a post-deploy
  credential audit; bridges must not silently run stale code; a bridge should
  self-check its actor identity at boot.
- **#28** — per-actor analytics, so the dogfooding metric is queryable rather
  than remembered. Only 8 of 35 task commits carry attribution today.
- **Preserve push leg unverified** — bridge-host git credentials were never
  confirmed; local-only is the assumed common case and is deliberately never
  rendered as a link.
- **#62** — answered rather than implemented upstream: colleague already had
  `--continue`.
- **#63 / upstream codex** — `--sandbox workspace-write` degrades to
  writes-fail on all three hosts. Draft filed at
  `docs/upstream/codex-sandbox-silent-degradation.md` for the operator to send.
