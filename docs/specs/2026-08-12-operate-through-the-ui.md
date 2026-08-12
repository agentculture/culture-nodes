# operate-through-the-ui

> Operating culture-nodes happens through the UI: every run shows its name, purpose, and cost; every agent attempt carries measured workspace evidence an approver reviews in-page; workflows are visible, validated, and publishable from the browser; and completed work is graded per actor so the ledger answers which actor is better at what
> instruction: after each delivery slice, verify its headline on <http://192.168.1.146:18080> with real runs, not fixtures

## Audience

- the human operator running the thor/orin fleet day-to-day (Ori), plus every mesh operator driving culture-nodes through nodes-operator — and the approvers who decide ship-review pauses from the evidence on screen

## Before → After

- Before: operating today means run ids with no names, cost invisible anywhere, `changed_files` always empty so approvers ssh to the workspace to read diffs out-of-band, no workflows view, authoring only via CLI, no per-actor quality picture beyond ad-hoc /remember notes, and a telemetry stub
- After: the operator runs the fleet from the browser: every run shows a name, its cost, and measured evidence of what each node actually changed; workflows are listed, validated, and published in-UI; a stats tab totals job cost; a living mesh view shows the fleet breathing; completed work is graded per actor and the ledger answers which actor is better at what; engine internals are observable via OTel traces

## Why it matters

- the product's brand line is 'every node has a contract, every result has evidence' — but the operating surface can't show the evidence, the cost, or the actor comparison; closing that gap is what makes dogfooding through nodes-operator honest rather than ceremonial

## Requirements

- cost visibility is aggregation + exposure, not new measurement: all three bridges (claude-code, codex, colleague mapping.py) already map backend usage into the §13.2 Usage block (input/output tokens, cost/currency pointers, internal/actors/protocol.go); aggregate attempt -> node run -> run, expose on run detail + node-runs listing, render in RunView, NodeDetailPanel, and the jobs timeline (PRD §8.6 lists cost/usage among close-zoom view fields)
  - instruction: aggregate usage attempt->node-run->run in the store layer, expose on run detail + node-runs listing, render in RunView/NodeDetailPanel/jobs timeline; test the no-usage-reported rendering path
  - honesty: a run's rendered cost equals the sum of its attempts' §13.2 usage, and attempts that reported no usage render as 'not reported' — never silently as zero
  - honesty: a run's cost includes every attempt — failed, retried, and cancelled included: money spent is money spent, and hiding failed-attempt burn would misstate exactly the retry-burn signal #28's aggregates need
- run name/description: createRunRequest today is exactly {`workflow_digest`, input} (internal/api/runs.go) — add optional display name/description at POST /v1alpha1/runs, carry it through list/board/jobs/run views, add CLI parity (nodes run create --name), and derive a truncated hint from the run input when absent
  - instruction: add optional name/description to CreateRunRequest + runOut + list queries, nodes run create --name, and the input-derived fallback hint; update openapi.yaml (parity gate)
  - honesty: name/description are optional and additive — a run created without them still lists with a usable hint derived from its input, and existing clients keep working unchanged
- workflows view is web-only work: the API family is already complete (validate/publish/list/get in internal/api/workflows.go, paths in api/openapi/openapi.yaml) but web/src/App.tsx routes only runs/board/jobs/run-detail/ledger — add a /workflows route listing published workflows, versions/digests, owner, and recent runs
  - instruction: add /workflows route: published workflows, versions/digests, owner, recent runs per workflow; e2e test against the fixture API
  - honesty: the workflows view is read-only composition over the existing endpoints — shipping it requires zero API change
- in-UI authoring first slice: paste/upload workflow YAML -> POST /v1alpha1/workflows/validate (already returns compiler diagnostics via workflowValidationOut) -> render diagnostics -> publish, plus a read-only graph preview reusing the existing graph components (web/src/domain/graph.ts, WorkflowNode, useElkLayout)
  - instruction: authoring slice: paste/upload YAML -> POST /workflows/validate -> render diagnostics -> read-only graph preview (existing graph components) -> publish; write the ADR against PRD §8.6/Phase-3 first
  - honesty: publishing from the UI produces the same content digest as publishing the same YAML via CLI, and invalid YAML renders the compiler diagnostics without publishing anything
- bridge-measured workspace facts on ALL THREE bridges (all-backends rule): today claude-code's mapping.py hardcodes `changed_files`: \[\] (honestly documented); the bridge subprocess measures HEAD before/after, git status --porcelain, changed-file list, diffstat, new branch — attached to the actor result as measured-by-process fields clearly distinguished from model-claimed content, still proposed authority
  - instruction: extend the actor-result mapping in claude-code, codex, AND colleague bridges with bridge-measured workspace fields; conformance-kit tests assert the fields exist and are process-measured
  - honesty: measured fields are populated by the bridge process from git itself (HEAD before/after, status, diffstat) — never copied from model output — and are rendered distinctly from model-claimed content; all three bridges ship it or it's a bug (all-backends rule)
- render the evidence: NodeDetailPanel already filters and renders observed evidence records from the ledger delta ('No observed evidence is attached' empty state) — changed files, diffstat, and per-attempt cost slot into that existing section plus the run view, so the approver reviews the diff in the ship-review pause instead of ssh-ing to the workspace
  - instruction: render measured workspace facts + cost in NodeDetailPanel's existing evidence section and the run view; e2e test with a fixture carrying measured fields
  - honesty: an approver in the ship-review pause reads changed files, diffstat, and per-attempt cost in NodeDetailPanel without ssh-ing to the workspace
- the runs board must actually fill a wide screen in live operation — the operator reports the board still renders narrow despite v0.11.1's full-width CSS (.view-rail max-width:none, .runs-`board__column` flex:1 1 15rem); acceptance is verified on the real wide display, not in the stylesheet
  - instruction: operator loads /board on the wide screen after the 2026-08-12 redeploy; if still narrow, debug the residual constraint in the served bundle
  - honesty: verified on the operator's real wide display against the deployed thor build — not in the stylesheet
- a Statistics view tab in the web UI: aggregate job/run cost as total and average (and mean/median) over the listed window — consumes the same attempt->node-run->run usage aggregation as run-detail cost (c3) and slots beside runs/board/jobs in the header nav
  - instruction: add a Statistics tab beside runs/board/jobs: jobs cost total, average and median over the listed window, consuming the c3 aggregation; cost-limit option deferred to a follow-up issue (c14)
  - honesty: the stats tab states its denominator: totals/averages are computed over exactly the window and run-set the view lists, and runs with no reported usage are counted and shown as excluded rather than folded in as zero
- observed workspace-diff evidence via the hook seam: a standard `post_run` code hook snapshots the workspace (changed files, diff digest, artifact refs) and the worker appends it as observed evidence through the runner boundary — the machinery exists (internal/worker/hooks.go buildHookEvidence/appendHookEvidence, per-observation measured/complete flags)
  - instruction: ship a standard `post_run` workspace-snapshot hook (changed files, diff digest, artifact refs) for sync actors; async coverage per the q2 decision
  - honesty: hook-produced workspace evidence lands as observed authority appended by the worker through the runner boundary — never folded into the agent's own ledger delta, which can never carry observe
- independent LLM review step: a review-node workflow pattern where a DIFFERENT backend reviews the change-set (codex reviewing claude or vice versa — ask-colleague's diversity principle as a node); input = measured diff from the evidence layers + task instruction, output = structured review claim landing proposed like any agent claim — §10.4 stands, only the human confirms (issue #13 item 3 notes today's self-hosting-loop verify node reviews through the same backend that built)
  - instruction: add a review-node pattern + example workflow where codex reviews claude's change-set (or vice versa) from the measured diff; structured review claim (findings, verdict) lands proposed
  - honesty: the reviewing backend is provably different from the building backend, its input is the measured diff (not the builder's self-report), and its verdict lands proposed per §10.4 — the review step never auto-confirms anything
- grading records: a first-class evaluation (rating + rationale) of a completed run/attempt recorded against the evaluated actor, flowing through the normal authority model — an agent-authored grade lands proposed and a human confirms; a human grading directly is human-authority; a grade is an opinion record and must never be conflated with observed/derived evidence (issue #28 item 1 honesty constraint)
  - instruction: add the grade record (shape per q3 decision) + API + `nodes-op grade <run-id> --rating N --notes`, recorded against actor and run through the normal authority model
  - honesty: a grade is an opinion record: it never lands as observed/derived, an agent-authored grade lands proposed, only a human confirms — and a grade is never conflated with claim confirm/reject
- task categories: an optional flat tag at assignment (nodes-op assign --category review|audit|explore|implement|docs) carried onto the run — the same createRunRequest change as the run name (c6), deliberately a cheap-to-change tag and not a taxonomy, so per-actor aggregates can slice better-at-WHAT (issue #28 item 3)
  - instruction: carry --category on nodes-op assign onto the run via the same CreateRunRequest change as name/description; wire into per-actor aggregates
  - honesty: category is an optional flat tag: absent category never blocks run creation, the tag is cheap to change, and aggregates slice uncategorized as its own bucket
- a live-mesh overview view in the UX: nodes, lines, and pulses traveling between them, in the spirit of culture.dev's MeshIsland (katvan site-astro/src/components/MeshIsland.svelte — Canvas 2D breathing graph, message particles along edges, prefers-reduced-motion renders one static frame) — for culture-nodes the graph is live topology (actors, runs, control plane) rather than build-time JSON, feeding off run events
  - instruction: new overview route composing the SSE events surface with a canvas mesh in MeshIsland's spirit: nodes=actors/control-plane, edges=dispatch paths, particles=live run/event traffic
  - honesty: the mesh view renders live control-plane state (registered actors, runs, dispatch traffic) — not canned data — pulses correspond to real events, and prefers-reduced-motion gets a dignified static frame
- OpenTelemetry beyond the stub (issue #5): traces and metrics for engine transitions, runner dispatch, and actor callbacks — internal/telemetry is a doc-comment-only stub today; prior art is ../culture, which carries opentelemetry api/sdk/otlp-grpc-exporter deps; the Go control plane takes the go.opentelemetry.io dependency family (the zero-third-party-deps rule binds the PYTHON package, not the Go module); issue #5's own becomes-worthwhile trigger — two workers against one Postgres on thor+orin — is the live topology now
  - instruction: take the go.opentelemetry.io dependency family in the Go module, instrument engine/dispatch/callback seams behind env-gated OTLP export; mirror culture's dependency discipline
  - honesty: traces and metrics for engine transitions, runner dispatch, and actor callbacks export via OTLP when a collector is configured — and the control plane runs unchanged when none is
- the live-mesh visual's quality bar is STUNNING — a signature, futuristic, alive-feeling showpiece (wholesome, not clinical): the MeshIsland reference sets the floor (breathing motion, traffic pulses), not the ceiling; this view is a deliberate design investment, the product's face, and acceptance is the operator's wow on the real screen — while still honoring prefers-reduced-motion with a dignified static frame
  - instruction: treat the mesh view as a design investment: motion design pass (breathing, pulses, glow), palette from culture-design tokens, review with the operator on the wide display before calling it done
  - honesty: the bar is the operator's judgment on the real screen: the view reads as alive, futuristic, and wholesome — MeshIsland is the floor, not the ceiling — and the reduced-motion frame still looks intentional
- cost persistence comes FIRST: the §13.2 Usage block the bridges emit is dropped at the completion seam today — no Usage consumer exists in engine/worker/actors non-test code, `actor_invocations` has no usage column, and a live probe of thor prod found 0 of 25 attempts carrying usage in result — so the cost family starts with persisting usage per attempt at completion (expand-only migration), and every pre-existing attempt renders 'not reported' (h2)
  - instruction: persist the §13.2 Usage block at the completion seam (sync and callback paths) via an expand-only migration; then aggregate per c3
  - honesty: after the persistence change, re-running the thor probe shows new attempts carrying the usage their bridge reported (`with_usage` > 0), while historical attempts render 'not reported' — no backfill, no fabricated zeros
- the live-mesh view needs a fleet-wide event source: GET /v1alpha1/runs/{id}/events is per-run only (pollEvents filters WHERE `run_id`), so the mesh view requires either a new cross-run events stream endpoint or coarse polling of the runs list — plus the c19 actors API for its actor nodes; 'composing existing SSE' as scoped was optimistic
  - instruction: add a cross-run events surface (or server-side fan-in endpoint) scoped to active runs; mesh nodes come from the c19 actors API
  - honesty: the mesh view's pulses come from committed events across all displayed runs without opening one SSE connection per run — the data source is a deliberate new surface, not an N+1 hack
- cost figures are token-first and never estimated: Usage.Cost/Currency are nullable by §13.2 design ('an actor that does not price its work says so with null'), codex/colleague report tokens only and the bridges never estimate — so stats and run views total tokens always, show currency cost only where actors reported it, and the control plane derives no prices from token counts this cycle (a pricing table is a possible follow-up issue)
  - instruction: stats tab and run views: token totals as the primary figure, currency cost as a secondary figure where present; 'not reported' otherwise
  - honesty: no code path in the control plane or web derives a currency amount from token counts — currency renders only when an actor reported it, token totals render always
- every new view (workflows, statistics, mesh, authoring) extends the agent-state store and carries stable ids/data-attributes — the machine-readable #agent-state node is how agents and the webglass CI job assert what the page shows; a view that exists only in pixels breaks the repo's agent-operable-UI contract and h19
  - instruction: extend AgentState with route-specific fields for workflows/stats/mesh/authoring; every assertable element carries a stable id or data-attribute
  - honesty: the webglass CI job (or an e2e test) asserts each new view through the #agent-state node — a view unregistered in agent-state fails CI, not review
- no actor grades its own run: the grade record names the graded actor AND the grading origin, and the API refuses a grade whose grading actor equals the evaluated actor — the self-promotion rule (§10.4 'no actor promotes its own proposal') extended to opinion records; grade corrections append with supersedes like every ledger record
  - instruction: enforce in the grade append path beside the existing §10.4 authority checks; test both the agent-grades-agent allowed path and the self-grade refusal
  - honesty: an API-level test proves a grade whose grading actor equals the evaluated actor is refused with a structured error
- every schema change in this family is expand-only per ADR 0002: nullable name/category columns on runs, a nullable usage column (or sibling table) for attempts, and the grade record type introduced additively — each migration tolerates the N-1 binary still running during the thor/orin rolling deploy window
  - instruction: runs.name/runs.category nullable columns, attempt usage as nullable column or sibling table, grade record type added to the schema registry additively
  - honesty: every migration in this family is reviewed against the ADR 0002 checklist: nullable or default-bearing additions only, and the N-1 binary keeps running against the expanded schema
- OTel spans and metrics carry identifiers, states, and durations — run/node/attempt ids, outcome kinds, queue depths — never run input, instruction text, or ledger payload content; observability must not become a second, unaudited copy of the work ledger
  - instruction: instrument engine transitions, dispatch, and callbacks with the allowlist; no attribute takes run input, instruction text, or ledger record data
  - honesty: span and metric attributes are drawn from an explicit allowlist of ids, enum states, counts, and durations — a reviewer can read the allowlist; payload content has no path in

## Honesty conditions

- every headline capability is verifiable on the live thor UI by the operator — run cost visible, evidence reviewable in-page, workflows publishable from the browser, grades recorded per actor
- no canvas/drag editing ships this cycle, and the ADR recording the early authoring slice against PRD Phase-3 scope is merged before the slice itself
- tests/parity passes on every PR in this family — any new endpoint, parameter, or verb missing from openapi.yaml or a CLI front fails CI visibly
- every delivered surface is usable by a human in the browser AND by an agent operator through the API/CLI — neither audience is served a degraded half
- each after-state capability is demonstrated on the live thor UI with a real run before its issue checkbox is ticked
- the before-state pains are cited from operating evidence (issues #12/#13/#28, run 01KZJYNC884FJHZ46XA4TW0MMF) — none is invented to pad the story
- the brand-line claim is honored in the product: what the UI shows as evidence is measured or observed provenance, never a completion claim restyled
- the success signal is exercised end-to-end on production (thor), not simulated in a fixture
- the authoring-slice ADR names the unauthenticated write surface and the LAN-bound acceptance explicitly — the exposure is a recorded decision, not an accident

## Success signals

- a ship-review approval decided entirely in-page (diff, cost, and evidence read in the UI, no ssh), plus one assigned run graded per actor through the API — both on the live thor UI

## Scope / boundaries

- the full graphical/canvas workflow editor stays out — PRD Phase 3 'Authoring and reuse' owns it; the paste-validate-preview-publish slice needs an ADR recording the deviation-in-timing against PRD §8.6 Design view / Phase-3 scope, per issue #12 item 6
- every new API surface lands in api/openapi/openapi.yaml and maps from all three fronts — tests/parity/`parity_test.go` enumerates Go CLI verbs, Python CLI verbs, and web-client operations and asserts each maps to a documented operation; new endpoints (name/category params, stats, actors, workflows-view queries) that skip the spec fail parity visibly
- the in-UI authoring slice adds one-click publish + run-creation to a control plane that has NO authentication today (issue #6 open) — the slice ships without adding auth, which is acceptable only while the API stays LAN-bound on the thor/orin network; issue #6 is the explicit gate before any wider exposure, and the ADR for the authoring slice must say so
  - instruction: write the exposure section into the same ADR that records the Phase-3 timing deviation; #6 is cited as the gate

## Non-goals

- cost enforcement (budgets, limits, refusal on overrun) stays parked — PRD Phase 4 'Rich execution' owns cost and budget enforcement; this scope observes and renders cost only, exactly as issue #12 item 3 states
- the cost limit option (capping spend) is deferred to its own follow-up issue — consistent with the existing enforcement non-goal (c5, PRD Phase 4); this cycle renders statistics only
- the AWS lane stays untouched: the Lambda runner, SQS driver, and deploy/aws are runner-side surfaces orthogonal to this UI/evidence/grading scope; production remains the thor/orin compose pair — AWS access matters to this cycle only if the user decides to host the control plane there, which nothing here requires

## Assumptions

- issue #12 items 1-2 already shipped in v0.11.1 (PR #27): web/public/favicon.svg carries the ADR-0001 AgentCulture mark and the layout moved to full-width .view-rail with stacked board columns, .table-scroll tables, and a collapsible nav — the remaining #12 scope is items 3-6 (cost, run names, workflows view, authoring)
- attempts persist actor results as opaque JSONB (Result \[\]byte in internal/store/postgres/sqlcgen/models.go) — usage aggregation needs SQL JSON extraction over that column or a migration extracting usage into queryable columns; no schema change has been decided yet
- the async `post_run` gap blocks #13 item 2 for the real fleet: worker refuses `post_run` on agent nodes whose actor answers async (refuseAsyncPostRun — the callback path has no IR/runner awareness), and the production claude/codex bridges run `ALWAYS_ASYNC` — so hook-snapshot evidence cannot cover production attempts without extending hook execution into the callback path, a cross-cutting change hooks.go explicitly documents as past-slice
- per-actor aggregates imply introducing the actors API family first: api/openapi/openapi.yaml has NO /v1alpha1/actors paths at all — actors are registered via raw DB rows (issue #8) and even nodes-op's 'actors' verb resorts to ssh+psql on thor; GET /v1alpha1/actors/{id}/stats (runs by outcome, claims proposed/confirmed/rejected, attempts per completion, duration percentiles, usage/cost) is bigger than a stats endpoint — it stands up actor read surface where none exists
- statistics over live-updating runs are eventually-consistent snapshots: the stats tab reads committed rows at query time with no transactional freeze, which is honest for an observation surface — a number can be stale by one refresh, never wrong about what was committed

## Scope exploration

- `s1` — `web/ + CHANGELOG.md 0.11.1 + issue #12`: favicon and full-width/responsive layout landed in v0.11.1; issue #12's open scope is items 3-6
  - seeds: `c2`
- `s2` — `adapters/*/src/*/mapping.py + internal/actors/protocol.go + PRD §8.6`: usage is already measured and mapped to §13.2 on every actor result; only aggregation, API exposure, and rendering are missing
  - seeds: `c3`
- `s3` — `internal/store/postgres/sqlcgen/models.go + migrations/`: attempt results are opaque JSONB; per-run cost rollup requires either `json_extract`-style queries or an extraction migration
  - seeds: `c4`
- `s4` — `PRD roadmap (Phase 4 §'cost and budget enforcement') + issue #12 item 3`: observation-only is the deliberate boundary; enforcement is a later PRD phase
  - seeds: `c5`
- `s5` — `internal/api/runs.go (createRunRequest/handleCreateRun)`: run creation accepts only `workflow_digest`+input; name/description is a straightforward additive field carried into runOut and list queries
  - seeds: `c6`
- `s6` — `internal/api/workflows.go + web/src/App.tsx routes`: GET /v1alpha1/workflows and /workflows/{digest} exist with no web view over them; no API change needed for issue #12 item 5
  - seeds: `c7`
- `s7` — `internal/api/workflows.go (handleValidateWorkflow) + web/src/domain/graph.ts + web/src/hooks/useElkLayout.ts`: validate-then-publish plumbing and graph rendering both exist; the authoring slice is composition, not new engine surface
  - seeds: `c8`
- `s8` — `PRD §8.6 'Design' view + Phase 3 roadmap`: PRD assigns compose-and-validate authoring to Phase 3; shipping a thin slice early is an explicit ADR-recorded decision, not silent drift
  - seeds: `c9`
- `s9` — `adapters/claude-code/src/claude_code_bridge/mapping.py (+ codex, colleague siblings)`: `changed_files` is always empty by construction; the model's summary is the only account of work — bridge-side measurement is the cheap first evidence layer (issue #13 item 1)
  - seeds: `c10`
- `s10` — `web/src/components/NodeDetailPanel.tsx evidence section`: the evidence rendering seam exists and is empty today precisely because bridges report no measured facts; #13 item 4 is filling a built slot
  - seeds: `c11`
- `s11` — `web/src/styles/app.css (.view-rail + .runs-board__columns) vs live thor UI`: the shipped CSS looks correct for wide screens, but the operator's live experience contradicts it — either thor serves a pre-v0.11.1 build or a residual width constraint survives; must be verified against the deployed build
  - seeds: `c12`
- `s12` — `web/src/App.tsx routes + internal/api/noderuns.go (no usage exposure today)`: a stats tab is pure consumption of the c3/c4 aggregation surface; no stats endpoint exists — either the API grows an aggregate query or the view folds client-side over listed runs
  - seeds: `c13`
- `s13` — `internal/worker/hooks.go (t14 hook seam)`: hook-appended observed evidence is built and tested — but see the async constraint recorded next; the seam works today only for synchronously-answering actors
  - seeds: `c15`
- `s14` — `internal/worker/hooks.go (refuseAsyncPostRun + package doc) + phase2 memory (bridges ALWAYS_ASYNC)`: `post_run` and the production bridges are mutually exclusive today; #13 item 2 needs a decision, recorded as a question
  - seeds: `c16`
- `s15` — `issue #13 item 3 + examples/self-hosting-loop/workflow.yaml (per the issue's own account)`: this is a workflow-pattern + example change once evidence layers 1/2 give it honest input; no engine change — ordering dependency on c10/c15
  - seeds: `c17`
- `s16` — `internal/ledger authority model + issue #28 item 1`: the authority model already expresses exactly the proposed-vs-human split a grade needs; the open shape decision is the record type, recorded as a question
  - seeds: `c18`
- `s17` — `api/openapi/openapi.yaml paths + .claude/skills/nodes-operator/scripts/nodes-op.sh actors verb`: no actors HTTP surface exists; #28 item 2 depends on it and on the c3/c4 usage aggregation
  - seeds: `c19`
- `s18` — `.claude/skills/nodes-operator/scripts/nodes-op.sh assign + internal/api/runs.go`: category rides the same additive run-creation field family as name/description; pairing them in one API change avoids two migrations
  - seeds: `c20`
- `s19` — `tests/parity/parity_test.go`: the parity harness is the drift gate for every surface this scope adds; CLI parity for new verbs is enforced, not optional
  - seeds: `c21`
- `s20` — `deploy/aws + ADR 0006 + cmd/nodes-runner-lambda`: no file in the AWS lane is touched by cost/name/workflows-view/authoring/evidence/grading work
  - seeds: `c22`
- `s21` — `/home/spark/git/katvan/site-astro/src/components/MeshIsland.svelte + web/src/hooks/useRunEvents.ts + GET /v1alpha1/runs/{id}/events`: the reference implementation is a self-contained Svelte canvas island with build-time data; culture-nodes already has an SSE events surface and a run canvas whose pulse is deliberately its only pure animation (app.css) — a live mesh view is a new route composing those, honoring reduced-motion like the reference
  - seeds: `c23`
- `s22` — `internal/telemetry/doc.go + issue #5 + /home/spark/git/culture/pyproject.toml otel deps`: telemetry is a stub with a parked-until-production trigger that has since fired; culture's OTel dependency set is the in-house prior art to mirror in Go
  - seeds: `c24`
- `s23` — `challenge pass / adjacent-systems lens: engine CompleteAttempt seam + thor prod attempts table (probe: 25 total, 25 with result, 0 with usage, 0 with cost)`: bridges measure usage but persistence discards it — c3's 'only aggregation is missing' was wrong; a persistence step precedes aggregation
  - seeds: `c33`
- `s24` — `challenge pass / adjacent-systems lens: internal/api/events.go pollEvents (per-run WHERE clause)`: no fleet-wide event stream exists; the mesh view's data source is a new API surface, dependent on the actors API family
  - seeds: `c34`
- `s25` — `challenge pass / unstated-assumptions lens: internal/actors/protocol.go Usage (nullable Cost/Currency) + codex mapping.py 'never estimates'`: most fleet attempts will carry tokens without currency; 'cost total/average' must define its unit honestly or the stats tab misleads
  - seeds: `c35`
- `s26` — `challenge pass / adjacent-systems lens: web/src/agent-state/store.ts + .github/workflows/web.yml webglass job`: agent-state registration is a hidden per-view dependency the spec never named
  - seeds: `c36`
- `s27` — `challenge pass / security lens: internal/api/server.go (no authn middleware) + issue #6`: authoring widens the write surface of an unauthenticated API; recorded as a boundary with #6 as the exposure gate rather than silently inherited
  - seeds: `c37`
- `s28` — `challenge pass / overlooked-actors lens: internal/ledger/authority.go origin rules + envelope supersedes`: the frame's grading claims never addressed self-grading; the authority model has the analog rule to extend
  - seeds: `c38`
- `s29` — `challenge pass / migration lens: docs/adr/0002-migration-policy.md expand-contract + N-1 rule`: three schema touches (runs, attempts, ledger record set) all fit the expand shape; none needs a contract migration this cycle
  - seeds: `c39`
- `s30` — `challenge pass / observability lens: internal/telemetry stub + PRD §10 ledger-authority model`: span-content hygiene was unstated; the ledger is the audited record, telemetry must stay metadata-only
  - seeds: `c40`
- `s31` — `challenge pass / concurrency lens: two workers + scheduler writing one Postgres while stats read it`: snapshot semantics suffice for observation-only stats; no locking or materialized rollup needed this cycle
  - seeds: `c41`
- `s32` — `challenge pass / reversibility lens: migrations/ + ADR 0002 rollback posture`: clean pass: every planned change is additive, so binary rollback to N-1 tolerates the expanded schema; no downgrade migration needed
- `s33` — `challenge pass / lifecycle lens: compiler owners.go ownerRef requirement vs UI publish`: clean pass: workflow ownership comes from the YAML itself (err-missing-owner is a compile diagnostic), so UI publishing introduces no ownerless path
- `s34` — `challenge pass / distributed-state lens: thor/orin one-at-a-time deploy vs new record type`: workers consume work items, not ledger grade records — a grade written while orin still runs N-1 is never read by the old binary; residual risk bounded by ADR 0002 expand rule

## Decisions

- q2 decided: async agent attempts carry bridge-measured workspace facts this cycle; hook-observed evidence is sync-only; callback-path hook execution is a recorded follow-up, not silent scope
- q3 decided: grade is a new first-class ledger `record_type`, not a review extension — truth-of-a-claim and quality-of-work remain different statements
- q4 decided: retag allowed (category PATCH surface), rename immutable at creation this cycle

## Open parks

- [unknown_nonblocking] how the three issues split into delivery slices (quick web-only wins vs API+store cost family vs evidence layers vs actors/grading family) is a plan-stage call, not a frame blocker
- [unknown_nonblocking] fleet-wide event stream load: a mesh view polling events for many runs could get heavy — containment (active-runs scoping, coarse poll interval, server-side fan-in) is a plan-stage design risk, not a spec change
- [follow_up] extend `post_run` hook execution to async-answering agents via the callback path (cross-cutting per hooks.go) — file as its own issue when the evidence family lands
