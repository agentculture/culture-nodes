# Delivery Artifact — t24: generic decompose pipeline, proven on a non-code domain

task: `t24` (economy-discord-graphs) · covers: `c30` · honesty: `h19` · date: `2026-08-14`

## What t24 asked for

> The system should support splitting a document to claims (like devague
> does), then splitting for decisions/actions (connected to the claims).
> This method should be generic — code, design, document writing, newsletter
> (scope from web, break to claims, plan for articles, write docs, all with
> sources tracked along the way, and verified in the end).

Task t22 (merged) proved a first instance: importing a devague plan into the
ledger's claim/task/decision vocabulary without hardcoding code-repo
semantics. t24's job was to decide what of that already generalizes, add
only the surface that genuinely does not exist yet, and prove the whole
chain end to end on a domain with nothing to do with code — a newsletter.

## What already generalized (evidence, not assertion)

Before writing anything new, the substrate was read end to end
(`internal/ledger`, `internal/engine/ledgerdelta.go`, `internal/devague`,
`internal/compiler`, `examples/delivery-loop`, `examples/notify-message`).
Conclusion: **most of the pipeline already generalizes**; t24 needed one
small pure library, one CLI verb over an existing read endpoint, a
`sources` documentation property, and a second example workflow — no schema
migration, no new engine node kind, no worker wiring.

- **Node kinds are already domain-agnostic.** `kind: agent` and `kind: end`
  are the only kinds `examples/newsletter-decompose/workflow.yaml` needs,
  and they are the same two kinds `examples/delivery-loop` (a code build/test
  loop) and `examples/notify-message` (posting to Discord) already use.
  Nothing in `internal/engine` or `internal/compiler` branches on what a
  node's actor happens to do.
- **"Decisions/actions connected to claims" already exists.** A
  `ledger.Record`'s `ProvenanceRefs` is exactly this connection, and it is
  fully wired through `internal/engine/ledgerdelta.go`'s `prepareRecord`
  today. t22's own `MapPlanShow` (`internal/devague/plan_show.go`,
  `coveredClaimRefs`) already connects a task record to the claims it
  covers this exact way — t24 needed zero changes to make that connection
  usable outside devague.
- **"A verification node that asks a model produces `proposed`" already
  exists.** An ordinary agent node proposing an `evidence` record (as
  `examples/delivery-loop`'s `verify` node already does for `claim`/
  `result`/`question`) is the whole mechanism. No new engine surface.
- **The ledger authority split (agent proposes / human confirms via review /
  runner observes with a manifest / engine-or-validator derives) is already
  fully generic** — enforced once, centrally, in
  `internal/ledger/authority.go`, independent of what domain produced the
  record. `internal/engine/ledgerdelta.go`'s `prepareRecord` confirms a node
  completion can only ever land `proposed` or `observed` authority — a
  `derived` verdict is architecturally a deterministic, out-of-band
  computation over already-committed records (task t18's
  `internal/worker/successsignal.go` is the existing precedent: a
  validator-origin, derived review record computed after the fact, never
  inside a node's own completion). t24's `internal/ledger.VerifyClaimChain`
  is exactly that shape of computation — see below.

## What genuinely needed a new surface

1. **`schemas/ledger/claim.schema.json`: a documented `sources` property.**
   No claim anywhere in this codebase had a place to name where its content
   came from. The property is optional, documentary, and additive — the
   `data` object already has no `additionalProperties: false`, so this is
   not a breaking or migration-requiring change (confirmed against
   `internal/contracts/validate_test.go`, which does not enumerate claim
   properties). **No migration 0025 was needed** — nothing here touches the
   database schema.
2. **`internal/ledger/chainverify.go` — `VerifyClaimChain`.** A pure,
   domain-agnostic function: every live claim sourced, every live
   decision/task motivated by at least one live claim through its own
   `ProvenanceRefs`. This is the "verification checks the chain" engine
   surface h19 asks for. It is proven generic in two ways, not one:
   - `internal/ledger/chainverify_test.go` exercises it directly against
     hand-built records (the newsletter shape).
   - `internal/devague/chainverify_generalizes_test.go` runs it against
     `MapPlanShow`'s real, unmodified output over the existing
     `testdata/plan-show.json` fixture (t22's own fixture) — proving the
     SAME function, with zero new code path, correctly reads t22's
     `coveredClaimRefs` connections. It also proves the honest negative: a
     devague-imported claim has no `sources`, so the sourcing half fails —
     which is the correct, non-fabricated answer, not a bug.
3. **`nodes chain-verify` (`cmd/nodes/chainverify.go`).** Exposes
   `VerifyClaimChain` as an operable verb rather than leaving it
   Go-only: `GET /v1alpha1/runs/{id}/ledger` (an existing, unmodified
   read-only endpoint — no new API route) followed by a local computation,
   the same "fetch, then compute locally" shape `nodes validate` already
   uses for `internal/compiler`. It never appends to the ledger itself —
   the same "pure function in, `Append` is a caller's decision" discipline
   `internal/devague`'s own package doc states.
4. **`examples/newsletter-decompose/workflow.yaml`** — the non-code proof
   instance: `scope` (web-sourced claims) → `plan` (decisions connected to
   claims) → `write` (drafts) → `verify` (asks-a-model evidence, `proposed`).

## Which acceptance bullet each test proves

- t24's acceptance — *"the non-code demo run completes with a source on
  every claim and a verification node at the end, recorded as a delivery
  artifact"* — is proven by
  `tests/e2e/TestNewsletterDecomposeRunsEndToEndWithRealSourcedContent`
  (this document is the delivery artifact it is recorded in).
- h19 — *"the decompose pipeline runs end-to-end on at least one non-code
  domain ... with sources tracked at every step and a verification pass at
  the end"* — the same test asserts `ledger.VerifyClaimChain` reports
  `Passed: true` over the run's real, appended ledger records, that every
  claim's payload contains its real source URL, and that the verify node's
  evidence record is `origin: agent, authority: proposed` (never
  fabricated `derived`/`observed`).
- Genericity of the connection mechanism —
  `internal/devague/chainverify_generalizes_test.go`
  (`TestVerifyClaimChainComposesWithMapPlanShowUnchanged`).
- The pure verification function's contract (sourced/unsourced claims,
  motivated/unmotivated decisions, task records, superseded records, the
  vacuous empty case) — `internal/ledger/chainverify_test.go`.
- The CLI verb wired against a real API server and real PostgreSQL, both
  the passing and failing verdicts, naming exactly which claim is unsourced
  — `cmd/nodes/chainverify_test.go`
  (`TestChainVerifyEndToEndAgainstTestServer`).
- The example workflow is a real, valid, deterministically-compiling
  Culture Nodes workflow — `tests/e2e/TestNewsletterDecomposeWorkflowCompilesCleanlyAndDeterministically`.

## Exactly what ran live and what did not

**Ran live:**

- The web scoping. Three real claims were gathered via two live web
  searches (August 2026 AI-agent-orchestration news and EU AI Act
  enforcement), each with a real, distinct source URL:
  - Amazon renaming Bedrock Agents to "Bedrock Agents Classic" — <https://aiagentstore.ai/ai-agent-news/this-week>
  - The EU AI Act's August 2 2026 high-risk enforcement date and the
    Digital Omnibus multi-agent liability clarification — <https://the-agent-report.com/2026/06/eu-ai-act-agent-regulation/>
  - Article 14's human-oversight/stop-button requirement — <https://artificialintelligenceact.eu/article/14/>
- The full engine run: `examples/newsletter-decompose/workflow.yaml`
  published and started through the real HTTP API
  (`internal/api`), driven by the real `internal/engine` and a real
  `internal/worker`, against a real, ephemerally-provisioned PostgreSQL
  (`internal/store/postgres/pgtest`) — the same fidelity
  `tests/e2e`'s existing `TestPhase1VerticalSlice` proves for the
  code-repo reference workflow. This is not a mock of the engine; it is
  the engine.
- The chain-verification computation, both ways: as a Go-level assertion
  inside the e2e test, and separately as the actual `nodes chain-verify`
  CLI verb driven end to end against a second real API server + real
  PostgreSQL in `cmd/nodes/chainverify_test.go`, including the failing
  case (an unsourced claim correctly flips the verdict and is named).

**Not live — clearly fixture-driven, and why:**

- The four workflow nodes (`scope`, `plan`, `write`, `verify`) are answered
  by a small scripted HTTP actor
  (`tests/e2e/newsletterdecompose_test.go`'s `newsletterAgents`), not by a
  dispatched call to a registered model actor (`codex-thor`/`codex-orin`)
  on the deployed control plane at `http://thor:18080`. The actor's own
  business judgement — which claims to scope, which article to plan per
  claim — is scripted with the REAL content gathered above; only the
  "was an LLM actually prompted to do this reasoning, right now, inside this
  run" question is not live. This mirrors the existing precedent exactly:
  `examples/delivery-loop`'s own acceptance test
  (`tests/e2e/slice_test.go`) scripts its four agents' judgement the same
  way — a scripted actor answering the real §13 actor protocol is this
  codebase's established bar for "the pipeline is genuinely exercised",
  not a shortcut invented for this task.
- Nothing was dispatched against the deployed control plane at
  `http://thor:18080`. Standing up a new registered actor capable of
  correctly emitting the ledger-delta shape this workflow's four nodes
  expect (proposing claims with `sources`, decisions with matching
  `provenance_refs`) was judged a heavier, riskier lift than the value it
  would add on top of the real-engine-real-worker-real-Postgres proof
  above, and out of proportion for a task whose repo rules already forbid
  touching `adapters/`. This is recorded here as a deliberate scope
  boundary, not an oversight.

## Assumptions

- "Sources" is left loose (`url`, `title`, `retrieved_at`, `note`, all
  optional) rather than schema-required, matching every other Phase-0
  ledger payload property in this codebase — an author states what they
  have, `VerifyClaimChain` reports what is honestly missing.
- `VerifyClaimChain` checks structural connection (a decision/task's
  `ProvenanceRefs` names a live claim), not semantic relevance (whether the
  connection makes sense) — the same boundary `runners.EvaluateAcceptance`
  draws between "mechanically checkable" and "someone's judgement call".

## Left undone, and why

- No worker-side automatic `derived`-authority chain check (the
  `internal/worker/successsignal.go` shape, generalized) was built. The
  newsletter's own verification node satisfies the acceptance bullet as an
  agent-origin, `proposed` check; a deterministic, `derived` twin is
  possible future work (`nodes chain-verify`'s own doc comment names the
  shape a caller wiring its result through a real `Append`/review
  transaction would use) but was judged out of scope for proving the
  pipeline generalizes — CLAUDE.md's own ledger authority model text is
  satisfied by the fact that BOTH shapes (asks-a-model → proposed,
  computes-a-check → derived) are reachable with the surfaces this task
  ships, not by this task building both end-to-end for one demo.
- No live dispatch against `codex-thor`/`codex-orin` on the deployed
  `http://thor:18080` control plane (see "not live" above).
