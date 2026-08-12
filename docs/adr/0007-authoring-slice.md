# ADR 0007: The in-UI authoring slice — Phase-3 timing deviation and unauthenticated LAN-bound exposure

- Status: accepted
- Date: 2026-08-12
- Task: t7 (operate-through-the-ui plan,
  `docs/plans/2026-08-12-operate-through-the-ui.md`)
- Issues: #6 (open — Phase-2 follow-up: OIDC / workload authentication),
  #12 item 6 (in-UI authoring)
- Frame: `docs/specs/2026-08-12-operate-through-the-ui.md`, claims c8/c9/c37,
  honesty conditions h17/h29

## Status note

This ADR merges **before** the authoring slice it governs ships. The slice
itself — paste/upload YAML, server-side validate with compiler diagnostics,
read-only graph preview, publish — is task t9 in the same plan, scheduled in
the next dependency wave (`depends on: t7, t8`). Per the frame's own honesty
condition, "the ADR recording the early authoring slice against PRD
Phase-3 scope is merged before the slice itself" — this document is that
gate, not a retrospective account of code already shipped.

## Context

`docs/initial-design/culture-nodes-prd-spec.md` §8.6 "Primary views" lists
**Design** — "Compose and validate workflow definitions" — as one of Culture
Nodes' primary views, and the PRD's roadmap (§23, "Phase 3 — Authoring and
reuse") assigns the full authoring surface to Phase 3: graphical editor,
compiler diagnostics on canvas, ledger/provenance graph and review inbox,
reusable node and subworkflow packages, immutable version publishing,
approval inbox, policy administration, and OCI artifact packaging and
signatures. §8.7 "First visual slice" explicitly defers this: "Do not wait
for the complete drag-and-drop editor before proving alignment and runtime
truth" — but the slice it describes there is a **read-only** live graph Run
view, not authoring.

Two things changed since the PRD was written that motivate shipping a
narrower authoring slice now, ahead of Phase 3:

1. The engine-side plumbing the slice needs already exists and needs no new
   API surface: `POST /v1alpha1/workflows/validate` already returns compiler
   diagnostics via `workflowValidationOut` (`internal/api/workflows.go`),
   and the graph-rendering components the preview needs already exist
   (`web/src/domain/graph.ts`, `WorkflowNode`, `useElkLayout`). Authoring in
   this narrow sense is composition over what is already built, not new
   engine work.
2. Operating the fleet through the CLI only, with no way to see or publish a
   workflow from the browser, is a felt operating gap today (issue #12 item
   6) — the same gap the rest of this plan (run names, cost, evidence,
   workflows view) closes for every other operating surface.

## Decision 1 — timing: ship a thin authoring slice now, ahead of PRD Phase 3

We ship an early, deliberately thin authoring slice this cycle:

- paste or upload workflow YAML in the browser;
- server-side validation against the existing
  `POST /v1alpha1/workflows/validate` endpoint, rendering the compiler
  diagnostics verbatim;
- a **read-only** graph preview, reusing the existing graph components,
  before publish;
- publish, producing a content digest byte-identical to publishing the same
  YAML via the CLI.

This is a **deviation in timing**, not in scope-boundary: it does not pull
forward any of the capabilities the PRD's Phase 3 list actually names.
Explicitly out of scope for this slice, and staying in Phase 3 where the
PRD puts them:

- the full graphical/canvas editor (drag-and-drop node placement, in-canvas
  editing);
- compiler diagnostics rendered on canvas (this slice renders diagnostics as
  text/list output, not inline on the graph);
- a ledger/provenance graph and review inbox;
- reusable node and subworkflow packages;
- an approval inbox and policy administration;
- OCI artifact packaging and signatures.

No canvas or drag-and-drop editing ships this cycle. The slice's contract
with itself: invalid YAML renders the compiler diagnostics and publishes
nothing; valid YAML publishes a digest identical to the CLI publish path for
the same source. Recording this here, rather than letting the slice land
silently ahead of its PRD phase, is the point of this decision — per this
repo's CLAUDE.md: "Record deviations from the PRD explicitly (ADR / devague
deviation record), don't drift silently."

## Decision 2 — exposure: the authoring slice ships on an unauthenticated control plane, and that is a recorded, LAN-bound acceptance

**Stated plainly: the control plane has no authentication today.**
`internal/api/server.go` wires no authn middleware in front of the
`/v1alpha1/*` handlers. Issue #6 ("Phase-2 follow-up: OIDC / workload
authentication") is open and tracks this gap; it was deliberately parked out
of the self-hosted phase-2 cycle (frame park v4, resolved question q5) until
a cloud lane exists, and the thor+orin production deployment runs on
secret-based auth scoped to the runner/actor callback path only (HMAC
attempt tokens, Bearer callbacks) — not on the human-facing HTTP API the
authoring slice extends.

Before this slice, the unauthenticated surface was read-mostly plus
run-creation from a known workflow digest. The authoring slice adds two new
one-click, unauthenticated write actions to that surface:

- **publish** — any browser that can reach the API can publish a new
  immutable workflow version;
- **run creation from a freshly-published digest** — the same unauthenticated
  path that already exists for known digests now composes with an
  unauthenticated publish, so an unauthenticated caller can both introduce
  new workflow content and cause it to execute.

This is an intentional widening of the write surface, accepted under one
explicit condition: **acceptance is LAN-bound**. The control plane is
reachable only on the thor/orin production network (the fleet's private
LAN), not exposed to the public internet or to any untrusted network. This
ADR does not claim the exposure is safe in general — it claims it is
acceptable *because* the network boundary substitutes for the missing
authentication, and only for as long as that boundary holds.

**Issue #6 is the recorded gate.** Any move to expose the control plane
beyond the thor/orin LAN — a public endpoint, a cloud deployment, access
from an untrusted network, or any other wider exposure — requires issue #6
(or its successor) to close first. This ADR does not resolve #6; it depends
on #6 staying open-but-tracked as the condition under which the authoring
slice's exposure is accepted, and closes/expires that acceptance the moment
issue #6's scope is realized (a wider deployment target) without #6 itself
being addressed.

## Consequences

- Task t9 (the authoring slice) may proceed once this ADR merges; it may not
  merge ahead of this ADR per the frame's honesty condition.
- The authoring slice's acceptance criteria carry an explicit environmental
  precondition (LAN-bound reachability) that is not enforced by any code in
  this repo — it is an operational/network boundary the operator maintains,
  the same way thor+orin's production topology is maintained today. A future
  reader of this ADR should treat "LAN-bound" as a deployment fact to verify,
  not a guarantee the software provides.
- If issue #6 lands (authentication is added), the LAN-bound condition this
  ADR records becomes obsolete and may be relaxed in a follow-up ADR; until
  then, deploying the control plane anywhere reachable outside the thor/orin
  LAN is out of scope for this decision and should not be treated as covered
  by it.
- This ADR does not change the AWS lane (ADR 0006): nothing here implies or
  requires exposing the control plane via the AWS surface, and ADR 0006's
  own scope statement — production remains the thor/orin compose pair —
  stands unmodified.
