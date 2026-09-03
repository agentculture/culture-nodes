# Delivery Summary — web-ui-lift

plan: `web-ui-lift` · run: `partial` · date: `2026-09-03`
baseline: `devague summary skeleton`

## Intent

Lift the Culture Nodes web UI so that one graph language draws mesh, workflow
and run views; the mesh shows the relationships the records hold instead of a
hub-and-spoke, and becomes the cross-machine awareness surface issue #226
asked for; every published workflow is readable as a graph; the mesh never
replays history on arrival; and a workflow can be composed on canvas while the
author's YAML survives byte for byte. The plan executed is
`docs/plans/2026-09-03-web-ui-lift.md` (20 tasks, 8 waves) under the split plan
`docs/plans/2026-09-03-web-ui-lift-split-plan.md`, with culture-nodes itself as
the engine: every delegable task was one `assign` dispatch to a registered
actor (codex-thor, codex-orin, the spark developer lane), gated by the operator.
The acceptance reference was a browsable demo committed at
`docs/demos/web-ui-lift/culture-nodes-lifted.html`.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Server: tail-only stream mode with a snapshot marker
- `t2` — Web: the one shared stream opens tail-only and views reconcile on the snapshot marker
- `t3` — The one Culture node component on React Flow, in the mesh idiom
- `t4` — Design-token lint guard: no colour, font or radius literal outside culture-design
- `t5` — Worker presence: additive table written by the worker loop, names only
- `t6` — GET /v1alpha1/mesh: timed collector, probe cache with `observed_at`, machines keyed on self-reported hostname
- `t7` — Mesh rebuilt on React Flow from real relationships; MeshCanvas deleted
- `t8` — Design gallery: every published version as a graph, open its stored source
- `t9` — Navigation on the PRD §8.6 spine: eight links, one Runs page, redirects
- `t10` — Fluidity: no layout jump, one SegmentedToggle, mesh + active-graph screenshots
- `t11` — Acceptance reference: the demo page committed with a walkthrough map
- `t12` — Spike: yaml Document round-trip baseline and validate proof, decision record
- `t13` — Reopen the issue-12 won't-do: decision record, Record issue, Feature issue
- `t14` — workflow-document: surgical mutations on the author's YAML/JSON document
- `t15` — Design canvas: palette, connect, properties, diagnostics on nodes, verbatim Publish
- `t16` — Operator review: before/after screenshots posted to #270, h12-style
- `t17` — Walkthrough e2e: replay the demo's six checks against the built app
- `t18` — Live test: the wave-A build on the deployed control plane, against real rows
- `t19` — Validate-delivery pass: the agent runs the plan's behavioral tests and files evidence and deltas
- `t20` — Fixes cycle: close every divergence from the demo reference, then re-run the walkthrough and the live checks

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `?from=latest` mode with a `stream.snapshot` first frame and three server tests; merged 5636b82 (codex-thor, run 01M1M61CSNECPTHRYHNK3Y8HVR, graded 4; gate fixup: publish helper accepts the idempotent 200, openapi.json regenerated) |
| `t2` | delivered | the one shared EventSource opens `?from=latest`, `useSnapshotReconcile` in eight views, one-EventSource lint guard; merged 3cc526d (codex-thor, 01M1M733N7GE15NFH6W5YSXJGF, graded 4; gate fixups: Go package suffix, per-page fixture stream cursor) |
| `t3` | delivered | `culture-design/CultureNode.tsx` + `node.css`, adapters, zoom bands, lint pin; merged 9cbb5aa (codex-thor, 01M1M6KB23XND3XV54CMTTD9FT, graded 4) |
| `t4` | delivered | `tests/lint/webtokens_test.go`, 25-case fixture, exemption count pin; merged 848df08 (developer, 01M1M687NG5FFX5SPEDJ5BXTJR, graded 5) |
| `t5` | delivered | `migrations/0055_worker_presence.sql`, store + worker presence with names-only guard; merged daf4a99 (codex-orin, 01M1M61D4YMN2BEM2T8XF1WW4R, graded 5) |
| `t6` | delivered | `internal/mesh` collector, `GET /v1alpha1/mesh`, openapi; merged 11101d0 (codex-orin, 01M1M6KBDYQN2C13MVS6XS9ZGC, graded 5); the collector shipped with **no targets** — fixed in t20 (79ad328) |
| `t7` | delivered | Mesh on React Flow from real relationships, typed edges, MeshCanvas deleted; merged e0a41c6 (codex-thor, 01M1M84DEVRSE1GJ2QNJDCP4BD, graded 4; gate fixups: lint guards followed the deleted file, edge count 9→8) |
| `t8` | delivered | `routes/Design.tsx` gallery + Nodes + Active graphs, stored-source accessors, 19 e2e; merged d21a75a (developer, 01M1M7GY052QAGCG7KZFMGJZRC, graded 5; merge needed t2's reconcile ported into the moved panels) |
| `t9` | delivered | eight-link header, `/runs` with projection toggle, redirects that keep query strings, route walk; merged 81fdab8 (developer, 01M1M8XSGAJ4DPXJ3S8A04MJ2M, graded 5) |
| `t10` | delivered | `useElkLayout` ready + fade, `SegmentedToggle` in four views, no-jump spec, mesh/active-graph shots; merged d5337b4 (codex-thor, 01M1MBSTYK2NDXHFDGT3NVPCZ8, graded 4); the operator added the mesh refit and kind colours at the gate (b625ea8) |
| `t11` | delivered | `docs/demos/web-ui-lift/README.md` walkthrough map and before-state citations; 9d2d730 (operator lane) |
| `t12` | delivered | `docs/decisions/2026-09-03-design-canvas-spike.md`: byte-identical round-trips with `flowCollectionPadding: false`, mutation diff, validate replayed offline (0 errors), verdict **go**; merged ca237fe (codex-thor, 01M1M9024QHBQMWGKDPYA1K3Y9, graded 5) — rerouted from the developer lane to an idle codex-thor |
| `t13` | delivered | `docs/decisions/2026-09-03-reopen-graphical-editor.md`, Feature #287, Record #288 (closed on read), triage rows; 0a67099, b2f99d0 (operator lane) |
| `t14` | delivered | `web/src/domain/workflow-document.ts`, 27 tests, golden fixtures incl. anchors, flow-style edges and merge-key refusals; merged c89cee4 (developer, 01M1MBQE631QY0KP8J6SB5M4CP, graded 5) |
| `t15` | delivered | `routes/DesignCanvas.tsx` at `/design/canvas`: palette, drag, connect, keyboard path, per-kind fields, source pane, debounced validate with diagnostics by JSON path, verbatim Publish, no-semantic-change notice; merged 4ef97d1 (codex-thor, 01M1MDRQVZ6D373YVSHDANMN4C, graded 4; gate fixup: container height + fitView) |
| `t16` | partial | before/after shot sets captured (`/tmp/shots-before` from main, `/tmp/shots-after`, `/tmp/fix1-shots`, `/tmp/final-shots` from the branch) and the review comment drafted for the operator's signature; **the operator's verdict on #270 is not posted** (obligation o14 open) |
| `t17` | delivered | `web/e2e/walkthrough.spec.ts`: six checks in the demo's order named by spec id plus the outcome→step guard, 7/7; merged 3d67286 (developer, 01M1MG8RF6J5AKRR9JX75RBA1J, graded 5; first dispatch lost to a wedged bridge, #290) |
| `t18` | partial | pair deployed (c89cee4, then 4ef97d1, then 91cac26); `docs/audits/2026-09-03-web-ui-lift-live-a.md` with API-side rows filled and the collector/hostname findings; **browser rows are the operator's and are empty** |
| `t19` | delivered | 15 obligations, 17 evidence records (15 confirmed, e16/e17 proposed), 8 deltas (b1–b8) confirmed; `.devague/deliveries/web-ui-lift.json` committed 735aede |
| `t20` | partial | round 1 closed: collector targets + `NODES_HOSTNAME` (79ad328), node treatment to the reference (1241bd2), three probe classes + jira bridge hostname (78e7d36), canvas placement/drop/persistence/inserted-line highlight/status line and page layout (91cac26, b8d7c8a, operator lane); `docs/audits/2026-09-03-web-ui-lift-live-b.md` written; **not closed:** the jira bridge's hostname live (its `uv tool` copy needs the Jira-env redeploy), the bridge liveness defect (#290), the untouched tabs (handed to #291 by deviation d2) |

## Mid-work Decisions

- `d1` — Non-goal c20 relaxed for one bounded human-pages package — the operator judged the work-group pages "very messy still" against the demo; approved, then withdrawn:
- `d2` — the package under d1 withdrawn before any work merged; the untouched tabs go to Feature issue #291 with alignment guidance; c20 stands.
- Spike q2 decided **round-trip hand-written YAML** (not canvas-owned IR) — recorded on the frame; the spike then measured the stringify options that make it byte-identical.
- q4 (challenge pass): a comment-only edit publishes nothing; the canvas says so and offers Download (decision c42).
- Routing changes to the split plan, no deviation record: t12 ran on codex-thor (idle lane) instead of the developer lane; t17 ran on the developer lane (it can execute Playwright, codex hosts have Node 18) instead of codex-thor; the canvas polish of t20 ran in the operator lane after three dispatches were lost to wedged bridges.
- The pair is deployed as a pair: `deploy.sh` pauses the scheduler until both workers match the API revision, so every deploy is two runs from one pinned commit.
- Three scope-guard refusals and two sandbox refusals on the first dispatches were brief/environment defects, not actor defects (issues #286, #289); the actors that stopped honestly were graded 4.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|------------------------|-----------------|
| `t20` (`d1`) | The operator reviewed the after screenshots on 2026-09-03 and judged the work-group pages 'very messy still'; the demo the operator chose as the acceptance reference already renders these three pages, so the spec's non-goal now contradicts the reference the delivery is judged against. | acceptable |
| `t20` (`d2`) | Operator instruction 2026-09-03, on seeing the deviation ask: no build now; open a Feature issue explaining how to handle them so later work stays aligned with today's changes. | acceptable |
| `t6` | shipped with a collector that had no targets, so the mesh had no machines until the t20 fix; the task's Postgres-backed test could not run in the codex sandbox and the gap was only visible live | needs-follow-up |
| `t16` | the operator's verdict is not posted; the review draft exists | needs-follow-up |
| `t18` | the audit's browser rows need the operator's Access session; API rows are filled | needs-follow-up |
| `t20` | the jira bridge hostname fix is merged but not live (Jira-env redeploy is a hand-turn); bridge liveness (#290) blocked three dispatches | needs-follow-up |
| `t12`, `t17` | lane changes against the split plan (see Mid-work Decisions) | acceptable |

## Evidence

- tests: `go test ./...` on 3d67286 — 51 packages ok (ephemeral Postgres); `npx vitest run` on f0ac4a2 lineage — 765 passed; `npx playwright test` — 117 passed incl. `web/e2e/walkthrough.spec.ts` 7/7 and `web/e2e/canvas.spec.ts` 3/3; `go test ./tests/lint/` — ok (token guard, CultureNode import guard, EventSource guard)
- lint: `markdownlint-cli2` on every doc added — 0 errors; `python3 scripts/triage-report.py --check` — all open issues dispositioned
- commits: `origin/main..f0ac4a2` (83 commits, 124 files); merges `5636b82` `daf4a99` `848df08` `11101d0` `9cbb5aa` `3cc526d` `d21a75a` `e0a41c6` `81fdab8` `ca237fe` `c89cee4` `d5337b4` `4ef97d1` `3d67286`; fixes `79ad328` `1241bd2` `78e7d36` `91cac26` `b8d7c8a`
- live: `docs/audits/2026-09-03-web-ui-lift-live-a.md`, `docs/audits/2026-09-03-web-ui-lift-live-b.md` (pair at 91cac26)
- PRs / issues: #284 (spec + plan, merged), #270, #226, #12, #286, #287, #288, #289, #290, #291
- devague: frame `web-ui-lift` (c1–c42, h1–h33), `.devague/deliveries/web-ui-lift.json` (o1–o15, e1–e17, b1–b8, d1–d2)

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| The mesh no longer replays history on arrival | high | `internal/api/events_test.go` · `web/e2e/walkthrough.spec.ts › c30` · live: first frame `stream.snapshot` on the pair (audit B) |
| One Culture node draws Mesh, run, gallery, active graphs and canvas | high | `tests/lint/culturenode_test.go` · `web/src/culture-design/node.test.ts` · `walkthrough.spec.ts › c32` |
| The mesh draws real relationships and machines keyed on self-reported hostnames | high | `internal/mesh/collector_test.go` · `walkthrough.spec.ts › c34` · live: machines thor/orin/spark-f8a9 (audit B) |
| Worker presence carries key names only, with real hostnames | high | `internal/store/postgres/workerpresence_test.go` · live workers row (audit B) |
| Every published workflow is readable as a graph without a run | high | `web/e2e/design.spec.ts` · `walkthrough.spec.ts › c31` |
| Navigation is the eight-link spine with redirects | high | `web/src/components/Header.test.tsx` · `web/src/App.test.tsx` |
| The canvas round-trips hand-written YAML and publishes the document verbatim | high | `web/src/domain/workflow-document.test.ts` · `web/e2e/canvas.spec.ts` |
| A canvas-authored digest equals the CLI path's | medium | `walkthrough.spec.ts › c33` with the fixture's deterministic digest standing in for `nodes workflow publish` (evidence e4 at fidelity strength) |
| The canvas matches the demo's placement, drop point and inserted-line highlight | high | `web/e2e/canvas.spec.ts › a palette drop lands…` (commit 91cac26) |
| The jira bridge reports its hostname live | unverified | merged in 78e7d36; the deployed bridge copy predates it (audit B) |
| The lifted pages read as less intimidating to a person | unverified | obligation o14 — the operator's #270 verdict is not posted |
| The browser walkthrough passes on production | unverified | audit A and B browser rows are empty |

## Remaining Work / Follow-up

- `t16` — the operator posts the review verdict on #270 from the draft (`review-270-draft.md` in the session scratchpad; the per-page findings are in this summary's evidence). Owner: operator.
- `t18` / `t20` — the operator fills the browser rows of audits A and B with an Access session and writes the verdict lines. Owner: operator.
- `t20` — redeploy the jira bridge with `JIRA_SITE` and the Jira trio exported so its hostname reaches the mesh (`docs/operations/jira-service-account.md`). Owner: operator (hand-turn).
- #290 — bridge liveness: single-threaded servers stop answering after a control-plane redeploy while still serving the collector; needs a fix in the adapters, not this cycle. Owner: adapters.
- #291 — the untouched tabs (Your work, Inbox, Decisions, Ticket, Ledger-and-plan, Statistics, Ledger) follow the guidance issue, one package per page. Owner: next cycle.
- #286, #289 — scope-guard two-dot diff and deploy exit-0-on-refusal; both cost hand-turns tonight.
- Evidence e16/e17 (fixes-cycle) await the operator's confirm on the frame.
- Final PR `feat/web-ui-lift` → `main` (0.48.0) is the third human gate.
