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

## The preflight capability surface (issue #67, tasks t14/t15)

An actor may advertise, in its **registration** (`actors.capabilities`), the
host facts a dispatched task depends on. The control plane composes them —
verbatim — into the briefing an actor is handed before its first billable
turn:

```json
{
  "preflight": {
    "protocol_version": "1.0",
    "host": {
      "hostname": "build-host-1",
      "sandbox_modes": ["danger-full-access"],
      "sandbox_modes_unavailable": {
        "workspace-write": "unprivileged user namespaces are restricted on this host (apparmor_restrict_unprivileged_userns=1), so the sandbox helper this mode's confinement depends on cannot start here — requesting it does not fail, it silently loses every file write while shell commands keep running unconfined (#18/#63)"
      },
      "default_sandbox_mode": "workspace-write",
      "confinement": "nothing is confined on this host: codex enforces --sandbox with a bubblewrap helper backed by unprivileged user namespaces, which this kernel restricts",
      "commit_policy": "harvest: this bridge issues no commit of its own — a dispatched session's changes stay in the workspace for the operator to harvest",
      "writable_paths": ["/srv/work/checkout"]
    }
  }
}
```

`protocol_version` is the version this control plane speaks
(`internal/preflight.ProtocolVersion`). `host` is **deliberately open**: the
facts are backend-specific while the protocol is not, so the engine carries
the block unchanged, states who advertised it and when, and never
re-renders, supplements, or interprets a fact it did not measure.

### The agreed host keys

These are the keys the bridges agree to use where they apply. A bridge that
cannot measure one **omits it** rather than guessing — an absent key reads as
absence, a null or an empty string reads as a fact about the host.

| Key | Meaning |
| --- | --- |
| `hostname` | The host this bridge dispatches on. Always present. |
| `sandbox_modes` | The confinement modes a dispatch can **actually** get here, in the backend's own vocabulary. Omitted by a bridge that runs no session. |
| `sandbox_modes_unavailable` | Mode → why this host cannot deliver it. Omitted when empty. |
| `default_sandbox_mode` | What a dispatch that names no mode gets. |
| `confinement` | One sentence on what actually confines a session here — including "nothing", when that is the truth. |
| `commit_policy` | Whether the session commits, and where a dispatch's changes end up. Always present. |
| `writable_paths` | The paths a dispatch may write in. `[]` means nowhere, which is a fact rather than an absence. |

The facts must describe what the host **can do**, never what its
configuration asks for. That distinction is the whole reason this exists:
issues #18/#63 are `--sandbox workspace-write` requested on hosts whose
kernel restricted unprivileged user namespaces, so the confinement helper
could not start, every file write was silently lost, and shell commands kept
running unconfined. A surface that echoed the config would have advertised
`workspace-write` and been wrong in the one way that costs a whole session.
`sandbox_modes_unavailable` is where that measurement becomes visible.

### Where a bridge's surface comes from

Each reference bridge measures its own host and serves the result at an
optional route:

```bash
curl -sH "Authorization: Bearer $TOKEN" https://bridge.example/v1/capabilities
codex-bridge --print-capabilities        # same document, no server needed
```

`GET /v1/capabilities` (`internal/actors.CapabilitiesPath`) is **not** a PRD
§13 path and nothing in the engine's dispatch path calls it — the surface
reaches the control plane through the actor's *registration*. The route
exists so an operator writing that registration reads the facts off the host
that measured them instead of writing down what they believe about it, and
`--print-capabilities` covers the case where the actor is registered before
its bridge has ever started. An actor that serves neither is fully
conformant; `tests/conformance` skips the check rather than failing it.

Implementation-wise the split is enforced, not merely intended: the protocol,
the measurement helpers and the agreed key set live in ONE module
(`preflight.py`), byte-identical in every bridge, and the per-backend facts
live in that bridge's `capabilities.py`. `tests/lint/preflightsurface_test.go`
fails the build if the shared module diverges between bridges, if its
constants stop matching the Go control plane's, or if a bridge implements
half the surface.

The **protocol is engine-side, the facts are bridge-side**, and that split is
a recorded decision: a per-bridge protocol would be four implementations of
one contract, which is exactly the duplication that let one bug ship three
times in three lanes. A bridge's whole obligation is to advertise this block;
everything else — composing the briefing, holding the dispatch, the ledger
records, the confirm verb, the single-use windowed refusal — is the engine's.

Advertising the surface changes nothing on its own. The gate is **per-actor
and default-off**, turned on by the operator in the same registration's
`metadata`:

```json
{ "preflight_gate": { "enabled": true, "window_seconds": 900 } }
```

Enabling it for an actor that advertises no surface is refused **when the
actor is registered** (HTTP 400 from `POST /v1alpha1/actors`, an error from
`RegisterActor`, and a CHECK constraint for raw SQL — migration 0026), never
discovered later by a run that stalls against a gate nothing can satisfy.

Once enabled, a dispatch to that actor is held until the briefing is
acknowledged through `POST /v1alpha1/preflights/{id}/acknowledge` (or
`nodes dispatch confirm <id>`). The acknowledgement is a `proposed` record
by the acknowledging actor — an actor saying it understood is a claim, not
evidence, exactly like every other thing an actor reports. It is single-use
and windowed; a dispatch whose window closes unacknowledged is refused
before anything is invoked, routing under the `preflight_unacknowledged`
outcome.

## Not covered here

- Cost budgets and quota negotiation (PRD Phase 4).
- Workload identity beyond the per-attempt callback token; the invocation
  itself is authenticated the way `internal/actors.Client` documents.
