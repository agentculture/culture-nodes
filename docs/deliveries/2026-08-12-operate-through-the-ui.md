# Delivery Summary — operate-through-the-ui

plan: `operate-through-the-ui` · run: `partial` · date: `2026-08-12`
baseline: `devague summary skeleton`

## Intent

Execute the operate-through-the-ui plan (PR #31, spec + challenge pass +
20-task plan) via assign-to-workforce: 19 tasks fanned out to isolated
worktrees across 8 dependency waves with TDD-gated merges, closing with a
live deployment to the thor+orin production pair and an end-to-end success
signal. The run is `partial` only at its final gate: every build task merged
and deployed, but t20's operator-facing verifications (the wide-display
design review, the in-browser ship-review demonstration, issue checkboxes)
remain with the human owner by design.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Persist §13.2 usage at the completion seam (sync + async callback
  paths) with an expand-only migration
- `t2` — Aggregate usage attempt→node-run→run in the store and expose it on
  run detail + node-runs listing
- `t3` — Run metadata API: optional name/description/category on
  CreateRunRequest, category PATCH, derived name hint
- `t4` — CLI fronts: nodes run create --name/--category, usage/cost in run +
  node-runs output, nodes-op assign --category, explain catalog entries
- `t5` — Web: render run names, derived hints, and token-first cost across
  RunsList/Board/Jobs/RunView/NodeDetailPanel
- `t6` — Web: Statistics tab — jobs cost total, average, median over the
  listed window
- `t7` — ADR: authoring slice — Phase-3 timing deviation + unauthenticated
  LAN-bound exposure (#6 as gate)
- `t8` — Web: Workflows view — published workflows, versions/digests, owner,
  recent runs
- `t9` — Web: authoring slice — paste/upload YAML → validate → diagnostics →
  read-only graph preview → publish
- `t10` — Bridges: measured workspace facts (HEAD before/after, status,
  changed files, diffstat, branch) in claude-code, codex, AND colleague
- `t11` — Web: render measured evidence — changed files, diffstat,
  per-attempt cost — in NodeDetailPanel and the run view
- `t12` — Standard `post_run` workspace-snapshot hook producing observed
  evidence for sync actors
- `t13` — Independent LLM review node pattern + example workflow (different
  backend reviews the measured diff)
- `t14` — Ledger: grade record type — schema, authority rules, self-grade
  refusal, supersedes
- `t15` — Actors API family: GET /v1alpha1/actors, /actors/{id},
  /actors/{id}/stats with per-category slices
- `t16` — Grade API + operator verb: POST grade,
  `nodes-op grade <run-id> --rating N --notes`
- `t17` — Cross-run events surface scoped to active runs (the mesh view's
  data source)
- `t18` — Web: live-mesh overview — actors and runs as a breathing graph,
  pulses from real committed events
- `t19` — OpenTelemetry: instrument engine transitions, runner dispatch,
  actor callbacks behind env-gated OTLP export
- `t20` — Closeout: deploy to thor, verify every headline live, exercise the
  success signal, tick issue checkboxes

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | migration 0012 + engine/store/callback/worker seams; usage proven persisted in production (15750/489 tokens on run `01KZW27WYX0FG2SSQABAC85YM9`) |
| `t2` | delivered | `usage_rollup.go` + run detail / node-runs exposure; retry-burn and no-cross-currency rules test-locked |
| `t3` | delivered | migration 0013, PATCH retag, `display_hint` never presented as a given name (rendered live) |
| `t4` | delivered | Python CLI verbs + explain catalog + `nodes-op assign --category`; teken 26/26, parity green |
| `t5` | delivered | names/hints/category chips + token-first cost across all five web surfaces |
| `t6` | delivered | `/stats` route: window totals, avg/median per run, stated denominator with excluded-count |
| `t7` | delivered | `docs/adr/0007-authoring-slice.md`, merged before t9 shipped |
| `t8` | delivered | `/workflows` route over existing endpoints, zero API change |
| `t9` | delivered | paste/upload → verbatim diagnostics → ELK preview → byte-identical publish |
| `t10` | delivered | identical `workspace_measured` block in all three bridges, measured from git, honest degradation |
| `t11` | delivered | workspace evidence + per-attempt cost render in NodeDetailPanel/run view (fixture-faithful; see drift on diffstat) |
| `t12` | delivered | snapshot evidence through the runner boundary as observed records; async refusal regression-locked |
| `t13` | delivered | `examples/independent-review/` validates 0/0; binding gap recorded (d2, #33, #34) |
| `t14` | delivered | `grade.schema.json` + authority matrix + self-grade refusal + supersedes |
| `t15` | delivered | actors list/detail/stats (GROUPING SETS, per-category buckets); live: grade + category buckets confirmed — see d3 for the attribution gap it exposed |
| `t16` | delivered | `POST /v1alpha1/runs/{id}/grades` + CLI/skill verbs; first production grade committed |
| `t17` | delivered | `GET /v1alpha1/events` cross-run SSE, bounded polls, honest ULID-cursor resume, migration 0014 |
| `t18` | delivered | `/mesh` canvas (MeshIsland idioms, live SSE pulses, reduced-motion frame); 320 vitest / 67 playwright at merge — operator design review pending (t20) |
| `t19` | delivered | env-gated OTLP on four seams with structurally enforced attribute allowlist |
| `t20` | partial | deployed to thor AND orin (migrations 0012–0015 applied); live-verified: favicon, actors API, SSE stream, run usage, category, grade, claim review loop, all three new routes served; d3 attribution fix built+deployed same day. Remaining: operator visual pass (h8 wide board, h16 mesh), in-browser ship-review demonstration, issue checkboxes, and the failed 4th dogfood run (below) |

## Mid-work Decisions

- `d1` — async failed attempts report no usage: the failed-event payload
  carries no §13.2 Usage field, so h24 holds only where usage was reportable
  — discovered during t1; the wire protocol only defines Usage on
  completed/blocked results; extending it touches all three bridges
  (issue #32)
- `d2` — the independent-review pattern cannot bind t10's measured workspace
  facts: no protocol field, no binding surface — t13 binds the closest honest
  substitutes and documents the gap (issues #33, #34)
- `d3` — per-actor attribution gap found at live verification: only code
  nodes ever set `attempts.actor_id`; fixed same-day for completed sync+async
  attempts (DBRegistry.ActorRowID + migration 0015); failed attempts remain
  unattributed (issue #40)
- t18 derives actor↔run edges from the node-runs listing instead of event
  fields (the engine never emits actor refs in events) — live data either
  way; no deviation record, documented in `web/src/domain/mesh.ts`
- t15 reads "runs by outcome" as status+outcome kept separate (the repo's
  domain-outcome ≠ technical-status rule) and "claims" as all
  ledger records by origin actor — judgment calls flagged at merge, accepted
- Qodo review fixes folded mid-run: atomic run-creation metadata
  (`engine.WithRunMetadata` — closed a duplicate-run retry window) and
  `--no-ext-diff --no-textconv` hardening in all three bridges; one Qodo
  finding declined as the spec's recorded decision (human-direct grades)
- Colleague review fix folded mid-run: mid-session workspace loss now
  degrades to the honest unmeasured shape (all three bridges)

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t1` (`d1`) | failed-event payload carries no Usage field; h24 narrowed to "where reportable" | needs-follow-up |
| `t13` (`d2`) | measured diff unbindable today; honest substitutes shipped, gap documented | needs-follow-up |
| `t20` (`d3`) | attribution gap invisible to fixtures, found live; completed-attempt fix shipped, failed-attempt half deferred | needs-follow-up |
| `t11` | no shipped producer writes a `diffstat` into ledger evidence (bridge diffstat never reaches the engine — same family as d2/#33); renderer supports it defensively, fixture stays shape-faithful | acceptable |
| `t20` | 4th dogfood run (`01KZW2XDR7YD2GER787QZ0K67M`) failed: operator deployed orin while its bridge served the run, orphaning the session; engine timed it out and parked cleanly | acceptable |
| `t20` | operator-facing verifications (wide-display h8/h16 review, in-browser ship-review, checkboxes) not performable by the agent; handed to the human owner with this artifact | acceptable |

## Evidence

- Go: `go test ./internal/... ./tests/parity/...` — pass at every one of the
  19 merges and after the d3 fix (pgtest ephemeral Postgres)
- Web: 320 vitest / 67 playwright at t18's merge (final web state); 292/…
  after t11; every merge gated
- Python: 132 pytest, ~95% coverage; teken `cli doctor --strict` 26/26;
  black/isort/flake8/bandit clean; adapters 152/129/121 (colleague's 4
  failures pre-existing env, baseline-diffed)
- CI: PR #35 checks green post-wave-6 (lint, test ×2, conformance,
  SonarCloud, GitGuardian, version-check; webglass/kind-smoke pending at
  last check)
- commits: `55797be..778621e` (48 commits on `spec/operate-through-the-ui`)
- PRs: #31 (spec+plan, merged), #35 (delivery, open — the final gate)
- issues filed this run: #32, #33, #34, #40; evidence comments on #18
- live probes (thor, 2026-08-13): `/favicon.svg` `image/svg+xml`; actors API
  8 actors; `GET /v1alpha1/events` SSE 200; run
  `01KZW27WYX0FG2SSQABAC85YM9` usage `15750/489`, category `review`,
  `attempts_reported: 1`; grade `ledger_01KZW2B18TMX5VNBKWQAEBM2FG`
  (human-confirmed, rating 4); stats buckets `['', 'review']`, grades
  `confirmed: {count: 1, mean_rating: 4}`; routes `/mesh` `/stats`
  `/workflows` all 200 from the embedded bundle

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| usage is measured, persisted, aggregated, and rendered token-first end-to-end | high | live run `01KZW27WYX0FG2SSQABAC85YM9` · migration 0012 · `internal/store/postgres/usage_rollup.go` |
| runs carry names/categories with honest derived hints; category retag works | high | migration 0013 · live category `review` · PATCH tests in `internal/api/runmetadata_test.go` |
| the grading loop runs on production through the authority model | high | grade `ledger_01KZW2B18TMX5VNBKWQAEBM2FG` · claim confirm at ledger_version 3 · `internal/api/grades_test.go` |
| per-actor stats aggregate grades and categories live; usage attribution fixed for completed attempts | high | live stats payload · migration 0015 · `TestCallbackCompletedAttributesActor` |
| workflows are visible and publishable from the browser with byte-identical digests | medium | `web/e2e/authoring.spec.ts` (fixture-proven) — not yet exercised against production in a browser |
| the live-mesh view renders live topology with real event pulses | medium | 6 mesh e2e specs · `/mesh` served live — operator's h16 design review pending |
| OTel traces/metrics export when a collector is configured | medium | `internal/telemetry/telemetry_test.go` (in-memory sinks) — no live collector exists to demonstrate against |
| the ship-review pause is decidable entirely in the browser (diff, cost, evidence) | unverified | evidence rendering is fixture-proven (`web/e2e/run-view.spec.ts`) but no production run has yet carried workspace evidence through it |
| the board fills the operator's wide screen | unverified | v0.11.1 CSS + full-width layout deployed — awaiting the operator's own display (h8) |

## Remaining Work / Follow-up

- `t20` operator half: the h16 mesh design review and h8 wide-board check on
  the real display; the in-browser ship-review walkthrough; then tick #12
  items 3–6, #13, #28, #5 checkboxes per h20 — owner: operator
- #32 — extend the failed event with optional usage (all three bridges) so
  h24 covers failed async attempts
- #33 / #34 — carry measured workspace facts into the engine/bindings; fix
  the empty-subject evidence projection (both reference workflows' verify
  nodes currently receive empty evidence)
- #40 — attribute failed attempts (failAttempt's call sites) so per-actor
  retry burn counts what defines it
- frame park: extending post_run hook execution to async actors (callback
  path) — file when the evidence family lands
- cost-limit option (frame c14) and a token→currency pricing table — both
  deliberately deferred, not yet filed as issues
- PR #35 review and merge — human gate 3; this artifact is its review map
