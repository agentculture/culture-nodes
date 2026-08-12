# Build Plan — operate-through-the-ui

slug: `operate-through-the-ui` · status: `exported` · from frame: `operate-through-the-ui`

> Operating culture-nodes happens through the UI: every run shows its name, purpose, and cost; every agent attempt carries measured workspace evidence an approver reviews in-page; workflows are visible, validated, and publishable from the browser; and completed work is graded per actor so the ledger answers which actor is better at what

## Tasks

### t1 — Persist §13.2 usage at the completion seam (sync + async callback paths) with an expand-only migration

- instruction: seam: engine CompleteAttempt (internal/engine/complete.go) + actors callback commit (internal/actors/callback.go); InvocationResult.Usage is internal/actors/protocol.go §13.2; add migration NNNN nullable (or sibling table) per ADR 0002; verify with the thor probe query from frame s23
- covers: c33, h25, c39, h31
- acceptance:
  - an expand-only migration adds nullable per-attempt usage storage; the N-1 binary runs against the migrated schema (migrate test)
  - both completion paths (worker sync, actors callback) persist the Usage block the bridge reported; a new run's attempts show `with_usage`>0 via the thor probe query; historical attempts stay null — no backfill, no fabricated zeros

### t2 — Aggregate usage attempt→node-run→run in the store and expose it on run detail + node-runs listing

- instruction: store rollup in internal/store/postgres (queries.sql + sqlc regen); expose via internal/api/runs.go runOut + noderuns.go; document in api/openapi/openapi.yaml; retry-burn = include failed attempts (h24)
- depends on: t1
- covers: c3, h2, h24
- acceptance:
  - run detail and node-runs listing carry usage rollups; a run's total equals the sum of ALL its attempts including failed/retried/cancelled (retry-burn test)
  - attempts that reported no usage are excluded from sums and surfaced as a not-reported count — never folded in as zero
  - openapi.yaml documents the new fields and tests/parity stays green

### t3 — Run metadata API: optional name/description/category on CreateRunRequest, category PATCH, derived name hint

- instruction: internal/api/runs.go createRunRequest {`workflow_digest`,input} today; add nullable runs columns via expand migration; PATCH /v1alpha1/runs/{id} category only (q4: name immutable); hint = truncated request/instruction field from input
- depends on: t2
- covers: c6, h3, c20, h13
- acceptance:
  - CreateRunRequest accepts optional name/description/category (nullable expand migration); existing clients keep working unchanged (contract test)
  - a run created without a name lists with a truncated hint derived from its input; category is retaggable via PATCH, name immutable (q4 decision)
  - openapi.yaml documents fields + PATCH; parity green

### t4 — CLI fronts: nodes run create --name/--category, usage/cost in run + node-runs output, nodes-op assign --category, explain catalog entries

- instruction: Python front: `culture_nodes`/cli/`_commands`/run.py + `node_runs.py` + explain/catalog.py (introspection tests enforce catalog); operator skill: .claude/skills/nodes-operator/scripts/nodes-op.sh assign; keep dependencies = \[\] (teken rubric gate)
- depends on: t2, t3
- covers: c21, h18
- acceptance:
  - Python nodes CLI creates runs with --name/--category and renders usage in run/node-runs views; explain catalog gains entries (introspection tests pass); zero third-party deps kept
  - nodes-op assign carries --category onto the run; tests/parity maps every new verb/param to a documented operation

### t5 — Web: render run names, derived hints, and token-first cost across RunsList/Board/Jobs/RunView/NodeDetailPanel

- instruction: web/src/components/RunCard,JobsTable,NodeDetailPanel + routes/RunsList,RunsBoard,JobsTimeline,RunView; tokens primary, currency secondary (c35); extend web/src/agent-state/store.ts
- depends on: t2, t3
- covers: c35, h27
- acceptance:
  - run cost renders token totals as the primary figure, currency only where an actor reported it, and an explicit 'not reported' state — no code path derives currency from tokens (grep-guard or unit test)
  - names/hints appear in list, board, jobs, and run views; agent-state carries the new fields

### t6 — Web: Statistics tab — jobs cost total, average, median over the listed window

- instruction: new route web/src/routes + header nav link (components/Header.tsx); consume t2 aggregation (client-side fold over listed runs is acceptable per frame s12); denominator statement per h9
- depends on: t2, t5
- covers: c13, h9, c36, h28
- acceptance:
  - totals/average/median are computed over exactly the run-set the view lists; runs with no reported usage are counted and shown as excluded, stating the denominator
  - the view registers in the agent-state node with stable ids and a webglass/e2e assertion covers it

### t7 — ADR: authoring slice — Phase-3 timing deviation + unauthenticated LAN-bound exposure (#6 as gate)

- instruction: docs/adr/0007-\*.md; record BOTH the PRD §8.6/Phase-3 timing deviation and the unauthenticated LAN-bound write surface with issue #6 as the exposure gate (frame c37/h29)
- covers: c9, h17, c37, h29
- acceptance:
  - the ADR merges BEFORE the authoring slice ships and records both the PRD §8.6/Phase-3 timing deviation and the unauthenticated write-surface acceptance (LAN-bound, #6 gates wider exposure)

### t8 — Web: Workflows view — published workflows, versions/digests, owner, recent runs

- instruction: web-only: GET /v1alpha1/workflows + /workflows/{digest} exist (internal/api/workflows.go); route + App.tsx + agent-state; recent-runs via runs list filtered by workflow digest
- covers: c7, h4
- acceptance:
  - a /workflows route lists published workflows with versions/digests/owner and each workflow's recent runs, using only existing endpoints (zero API change proven by parity inventory diff)
  - agent-state registration + e2e coverage against the fixture API

### t9 — Web: authoring slice — paste/upload YAML → validate → diagnostics → read-only graph preview → publish

- instruction: POST /v1alpha1/workflows/validate returns workflowValidationOut diagnostics; graph preview reuses web/src/domain/graph.ts + hooks/useElkLayout + WorkflowNode; digest-identity e2e vs CLI publish
- depends on: t7, t8
- covers: c8, h5
- acceptance:
  - invalid YAML renders the compiler diagnostics verbatim and publishes nothing; valid YAML publishes a digest byte-identical to a CLI publish of the same source (e2e proves both)
  - a read-only graph preview (existing graph components) renders before publish; agent-state registration included

### t10 — Bridges: measured workspace facts (HEAD before/after, status, changed files, diffstat, branch) in claude-code, codex, AND colleague

- instruction: adapters/{claude-code,codex,colleague}/src/\*/mapping.py currently hardcode `changed_files`:\[\]; measure via git in the bridge subprocess around the session (HEAD before/after, status --porcelain, diffstat, branch); run each adapter's conformance kit
- covers: c10, h6
- acceptance:
  - each bridge populates the measured fields from git itself around the session; a unit test proves they are process-measured, never copied from model output
  - measured-by-bridge fields are structurally distinct from model-claimed content in the result mapping; all three bridges ship it (all-backends rule) and each conformance kit asserts it

### t11 — Web: render measured evidence — changed files, diffstat, per-attempt cost — in NodeDetailPanel and the run view

- instruction: web/src/components/NodeDetailPanel.tsx evidence section (currently renders 'No observed evidence'); fixtures in web/src/fixtures; pair with run view evidence section
- depends on: t10, t5
- covers: c11, h7
- acceptance:
  - with a fixture carrying measured fields, the approver reads changed files + diffstat + attempt cost in the ship-review pause entirely in-page (e2e); the empty state still renders honestly when no measured facts exist

### t12 — Standard `post_run` workspace-snapshot hook producing observed evidence for sync actors

- instruction: internal/worker/hooks.go buildHookEvidence/appendHookEvidence is the seam; ship the snapshot as a standard headspace-run operation; keep refuseAsyncPostRun intact (q2 decision: async = bridge-measured only)
- covers: c15, h10
- acceptance:
  - the hook snapshots changed files, diff digest, and artifact refs through the runner boundary; the worker appends it as observed evidence — a test proves it never rides the agent's own ledger delta
  - async agent nodes declaring `post_run` are still refused exactly as today (regression test on refuseAsyncPostRun)

### t13 — Independent LLM review node pattern + example workflow (different backend reviews the measured diff)

- instruction: examples/ + docs; prior art: examples/self-hosting-loop/workflow.yaml verify node (same-backend today); bind measured diff fields from t10 into the review node input contract
- depends on: t10
- covers: c17, h11
- acceptance:
  - the example workflow dispatches the review to a backend different from the builder; its input binds the measured diff + task instruction, not the builder's summary
  - the review's structured claim (findings, verdict) lands proposed — a test proves §10.4 holds and nothing auto-confirms

### t14 — Ledger: grade record type — schema, authority rules, self-grade refusal, supersedes

- instruction: schemas/ledger/record.schema.json if/then dispatch + new grade.schema.json + schemas/embed.go; authority rules internal/ledger/authority.go; self-grade refusal beside §10.4 checks; migrations only if a table is needed (prefer ledger records)
- covers: c18, h12, c38, h30
- acceptance:
  - grade lands additively in the schema registry (record.schema.json dispatch + contracts validate); agent-origin grades land proposed, human grades land human-authority, and grades are never observed/derived (authority tests)
  - a grade whose grading actor equals the evaluated actor is refused with a structured error (API-level test); corrections append with supersedes

### t15 — Actors API family: GET /v1alpha1/actors, /actors/{id}, /actors/{id}/stats with per-category slices

- instruction: new internal/api/actors.go + openapi paths; stats SQL over runs/attempts/ledger + t2 usage rollup; replaces nodes-op.sh actors ssh+psql verb; CLI verb in `culture_nodes`/cli/`_commands`
- depends on: t3, t14
- covers: c1
- acceptance:
  - the actors read surface exists (list + detail); stats aggregates runs by outcome, claims proposed/confirmed/rejected, attempts per completion, duration percentiles, and usage/cost — sliced per category with uncategorized as its own bucket
  - openapi.yaml + parity green; a CLI verb reads the same surface (replacing nodes-op's ssh+psql actors verb)

### t16 — Grade API + operator verb: POST grade, `nodes-op grade <run-id> --rating N --notes`

- instruction: grade endpoint under /v1alpha1 (runs/{id}/grades or actors-scoped — pick in ADR-lite comment); nodes-op.sh grade verb; integration test: agent proposes via bridge path, human confirms via review surface
- depends on: t14, t15
- covers: c18, h12
- acceptance:
  - an operator grades a completed run against its actor through the public API; the grade flows through the normal authority model end-to-end (agent proposes, human confirms — integration test)
  - nodes-op grade works from any operator shell; openapi + parity green

### t17 — Cross-run events surface scoped to active runs (the mesh view's data source)

- instruction: extend internal/api/events.go: cross-run poll over the same events table WHERE `run_id` IN (active); SSE or long-poll; risk r1 containment (active-runs scope, coarse cadence) documented in the handler comment
- depends on: t16
- covers: c34, h26
- acceptance:
  - one endpoint serves committed events across all displayed/active runs — a test proves the mesh consumer needs no per-run SSE fan-out
  - scoping and poll cadence are load-bounded by design (active-runs filter, coarse interval documented); openapi + parity green

### t18 — Web: live-mesh overview — actors and runs as a breathing graph, pulses from real committed events

- instruction: reference: /home/spark/git/katvan/site-astro/src/components/MeshIsland.svelte (canvas idiom, reduced-motion single frame); palette from web/src/culture-design/tokens.css; nodes=t15 actors + active runs, pulses=t17 events; operator design review gate before close (h16)
- depends on: t17, t15
- covers: c23, h14, c25, h16, c36, h28
- acceptance:
  - nodes come from the actors API and live runs, edges from dispatch paths, particles correspond one-to-one to committed events (no canned data — e2e against fixture events)
  - prefers-reduced-motion renders a dignified static frame; agent-state registration + stable ids
  - the stunning bar: a dedicated motion/design pass (culture-design palette, breathing, glow) reviewed by the operator on the real wide display before the task closes

### t19 — OpenTelemetry: instrument engine transitions, runner dispatch, actor callbacks behind env-gated OTLP export

- instruction: internal/telemetry package; go.opentelemetry.io/otel + otlptracegrpc; wrap engine transition commit, worker dispatch, actors callback; attribute allowlist as a named const reviewed in PR; exporter env-gated (no collector = no-op)
- covers: c24, h15, c40, h32
- acceptance:
  - go.opentelemetry.io instrumentation emits traces/metrics for the three seams; with no collector configured the control plane runs unchanged (test with exporter unset)
  - span/metric attributes come from an explicit reviewable allowlist — ids, enum states, counts, durations; no attribute carries run input, instruction text, or ledger payloads (allowlist test)

### t20 — Closeout: deploy to thor, verify every headline live, exercise the success signal, tick issue checkboxes

- instruction: deploy/prod/deploy.sh thor (watch issue #17 scp pitfall); verify per h1/h8/h20/h23 on <http://192.168.1.146:18080> with real runs; tick #12/#13/#28/#5 checkboxes only after live demo; then /summarize-delivery
- depends on: t4, t6, t9, t11, t12, t13, t16, t18, t19
- covers: c1, h1, c12, h8, c26, h19, c27, h20, c28, h21, c29, h22, c30, h23
- acceptance:
  - each headline capability is demonstrated on the live thor UI with real runs (not fixtures) before its issue checkbox is ticked: cost visible, evidence in-page, workflows publishable, grades recorded
  - the success signal runs on production: one ship-review approval decided entirely in-page (diff, cost, evidence — no ssh) AND one assigned run graded per actor through the API
  - the board fills the operator's real wide display (h8) — verified by the operator, and the before/after narrative in the delivery summary cites the operating evidence it came from

## Risks

- [unknown_nonblocking] fleet-wide event stream load: active-runs scoping + coarse poll cadence are the containment; if real traffic outgrows them, server-side fan-in needs a design pass (task t17)
- [follow_up] extending `post_run` hook evidence to async-answering agents via the callback path — recorded follow-up (frame park), its own issue when the evidence family lands
- [follow_up] cost LIMIT option (capping spend) deferred to its own issue per the operator's decision (frame c14)
- [follow_up] a pricing table for deriving currency from tokens is a possible follow-up — this cycle never estimates prices (c35)
- [unknown_nonblocking] codex fleet is analysis-only until #18 (bwrap): dogfooding assignments during this plan route reviews/audits to codex-thor/orin, shell/write tasks stay local
- [unknown_nonblocking] openapi.yaml + tests/parity inventory is a shared merge surface across t2/t3/t15/t16/t17 — sequenced by explicit deps rather than parallel merges
