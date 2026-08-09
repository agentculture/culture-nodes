# api/actor-protocol

The wire contract between Culture Nodes and an **actor**: how the engine
hands an `agent` (or `action.http`) node's attempt to an external process,
how that process reports back, and what it may claim. It is the
agent-execution counterpart of the [runner protocol](../runner-protocol/README.md)
(PRD §13.1–13.6) — this document covers the network boundary an *agent*
crosses; `api/runner-protocol` covers the one *code* crosses.

Two properties shape everything below, mirroring the runner protocol's own:

- **Provider-neutral.** Nothing in the protocol names a model vendor or an
  agent product. An `agent` node's contract is satisfied by any process that
  speaks this HTTP/JSON surface — colleague, claude-code, codex, a hand-rolled
  script, a human-in-the-loop bridge. Three conformant, open-source
  implementations exist today (below); a fourth is exactly as welcome.
- **A completion is a claim, not a fact.** Everything an actor reports lands
  in the work ledger as `authority: "proposed"` (PRD §10.4, this repo's
  ledger-authority-model rule). No actor promotes its own proposal to
  `confirmed`/`observed`/`derived` — a human, a trusted runner's direct
  measurement, or a deterministic validator does that, never the actor that
  made the claim.

The Go declaration of this contract is `internal/actors/protocol.go` — an
adapter author reads that file (not this stub) for exact field names and
JSON casing, because it is asserted to mirror PRD §13.1–13.4 field for field.
`tests/conformance` is the runnable acceptance kit: any implementation that
passes it, using the kit unmodified, is a conformant actor.

## Shape, in brief

| Step | Path | Meaning |
| --- | --- | --- |
| Invoke | `POST /v1/invocations` | The engine hands the actor one attempt: workflow/node refs, pinned contract digest, input, a callback URL + short-lived token. |
| Synchronous answer | `200` + `InvocationResult` | The actor finished inline: a domain outcome, output, and an optional proposed `ledger_delta`. |
| Asynchronous answer | `202` + `AsyncAccepted` | The actor took the work and will report later — the worker parks it (`waiting_external`) rather than holding a lease or a connection for the duration. |
| Callback events | `POST` to the URL the invocation carried | `accepted` / `heartbeat` / `progress` / `artifact` while running, `completed` / `failed` / `blocked` once terminal — authenticated by the attempt-scoped token the invocation handed the actor. |

An actor picks sync or async per invocation; the engine does not guess up
front which one is coming (PRD §9.5's actor manifest declares what an actor
*supports*, not what a given call *will* do). An incomplete or crashed actor
session must never map to a reported success — every reference
implementation is tested against exactly that failure mode.

## Reference implementations

Three sibling adapters, each its own package with its own tests, none
installed as part of a culture-nodes deployment:

| Adapter | Drives | README |
| --- | --- | --- |
| `adapters/colleague` | `colleague work` subprocess dispatch | [`adapters/colleague/README.md`](../../adapters/colleague/README.md) |
| `adapters/claude-code` | headless `claude -p` subprocess dispatch | [`adapters/claude-code/README.md`](../../adapters/claude-code/README.md) |
| `adapters/codex` | headless `codex exec` subprocess dispatch | [`adapters/codex/README.md`](../../adapters/codex/README.md) |

All three run beside their agent backend on a host or container the control
plane never reaches into, and are themselves invoked exactly like any other
actor over the network (`internal/actors.Client.Invoke`/`Cancel`). Each
passes `tests/conformance` unmodified, is `proposed`-only in the ledger
sense above, and is deliberately readable end to end as a template for a
fourth adapter over a different backend.

## Registering an actor

There is no actor-registration HTTP endpoint yet (PRD §26 open question) —
an actor becomes reachable by inserting a row naming its base URL into the
`actors` table (`internal/worker/registry.go`'s `DBRegistry`), and a
workflow node's `uses:` reference binds to it by that row's id. See
`deploy/compose/README.md`'s colleague-bridge section for a worked local
example.

## Not covered here

- Cost budgets and quota negotiation (PRD Phase 4).
- Workload identity beyond the per-attempt callback token; the invocation
  itself is authenticated the way `internal/actors.Client` documents.
