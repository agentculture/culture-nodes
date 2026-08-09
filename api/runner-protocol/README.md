# api/runner-protocol

The wire contract between Culture Nodes and a **runner service**: how an
operation is submitted, how its outcome is learned, and what a runner must
refuse. It is the code-execution counterpart of the actor protocol
(PRD §13.1–13.6, `internal/actors`), and it implements PRD §13.7's boundary —
*the control plane never executes code; it describes an operation, and a
runner executes it behind an enforced policy boundary.*

Two properties shape everything below:

- **Placement-unaware.** A workflow definition never says where its code runs.
  A runner service on this machine, on a machine down the hall, or in a cloud
  is one abstraction with three deployments; moving a code node between them
  changes a registry entry and nothing else.
- **Evidence, not assurance.** A result is trustworthy because the operation
  was typed, the policy enforced, the inputs immutable, and every observation
  attributed with declared completeness — not because a runner said it went
  well.

The Go declaration of this contract is `internal/runners/protocol.go`; the
identity that binds a node to a runner is `internal/runners/registry.go`. A
test (`internal/runners/protocol_test.go`) asserts every path, header and
version quoted here is the constant the code exports, so this document cannot
drift from the implementation without failing the build.

## The wire payloads are the schemas, verbatim

| Where | Payload | Schema |
| --- | --- | --- |
| Execute request body | Runner operation | `schemas/runner/operation.schema.json` |
| Terminal status `result` | Runner result | `schemas/runner/result.schema.json` |

These documents cross the wire **unchanged**. They are not wrapped in a
runner-specific envelope, not extended with transport fields, and not forked
per deployment. Both set `additionalProperties: false`, so there is no room in
the body for transport concerns — which is exactly why everything the
transport needs (idempotency key, protocol version, optional callback) rides
in headers instead. A runner that requires an extra body field has forked the
contract.

## Endpoints

A registered endpoint is a base URL; the protocol paths are appended to it, so
a runner may be mounted behind a gateway prefix.

| Method and path | Body | Success | Meaning |
| --- | --- | --- | --- |
| `POST /v1/operations` | operation document | `202` + acceptance | The runner has taken the operation. It has **not** answered it. |
| `GET /v1/operations/{operation_id}` | — | `200` + status | The authoritative completion path. |
| `POST /v1/operations/{operation_id}/cancel` | — | `202`/`204` | Optional, best-effort. |

### Execute

```http
POST /v1/operations HTTP/1.1
Authorization: Bearer <secret resolved from the registry's secret_ref>
Content-Type: application/json
Idempotency-Key: op_01JAV3QK2M0000000000000011
Nodes-Protocol-Version: 1.0
Nodes-Callback-Url: https://nodes.example/v1/runner-operations/op_01JAV.../events
Nodes-Callback-Token: <opaque bearer the runner echoes back>
Traceparent: 00-<trace-id>-<span-id>-01
```

The body is the operation document. Its `operation_id` **is** the idempotency
key — the schema says so — and `Idempotency-Key` restates it in the header so
a proxy or gateway can act on it without parsing the body. Re-submitting the
same key must return the acceptance the runner already issued; it must never
start the work a second time.

The response is `202 Accepted` and an acceptance:

```json
{
  "operation_id": "op_01JAV3QK2M0000000000000011",
  "poll_after_seconds": 5,
  "status_retention_seconds": 86400,
  "supports_cancellation": false,
  "supports_callback": true
}
```

- `operation_id` is required and must echo the submitted operation. Accepting
  a different id is a contract failure: the caller would poll a status it never
  dispatched.
- `poll_after_seconds` is the runner's requested minimum sampling interval.
  Advisory — cadence belongs to the runtime — but sampling faster than a runner
  asked for is load it said it did not want.
- `status_retention_seconds` is a promise about the completion path itself:
  how long the terminal status stays readable after the operation finishes.
  The protocol minimum is one hour; a shorter declared retention is refused at
  dispatch, because a runner that forgets an operation before it can be
  sampled has made its outcome unlearnable.
- `supports_cancellation` and `supports_callback` are declarations, not
  requirements. Both may be `false` for a fully conformant runner.

There is **no** synchronous variant. A `200` carrying a result is not a faster
path, it is a protocol violation: see *Asynchronous only* below.

### Status

```http
GET /v1/operations/op_01JAV3QK2M0000000000000011 HTTP/1.1
Authorization: Bearer <secret resolved from the registry's secret_ref>
Nodes-Protocol-Version: 1.0
```

While the operation is in flight:

```json
{ "operation_id": "op_01JAV3QK2M0000000000000011", "state": "running" }
```

Once it has finished:

```json
{
  "operation_id": "op_01JAV3QK2M0000000000000011",
  "state": "completed",
  "result": { "…": "the runner result document, verbatim" }
}
```

The envelope is deliberately this thin — an operation id, a state, and the
result document once there is one. It makes no claim of its own; everything a
caller may conclude about the execution comes from the embedded result and its
per-observation honesty declarations.

| `state` | Terminal | `result` |
| --- | --- | --- |
| `accepted` | no | absent |
| `running` | no | absent |
| `completed`, `failed`, `timed_out`, `cancelled`, `rejected` | yes | **required** |

The five terminal states are exactly the result schema's `state` enum, so
`state` and `result.state` are comparable — and must agree. An envelope that
disagrees with its own evidence is a contract failure, as is a terminal status
with no result document, or a non-terminal status that carries one.

### Cancel (optional)

Cancellation is durable in the control plane before this call is made, so a
runner that answers `404` or `405` changes nothing about the run: the runtime
records the cancellation either way and stops caring about the operation's
outcome. A runner that implements the path answers `202`/`204` and reports
`cancelled` on its next status.

## Asynchronous only

Dispatch returns `202` and the connection closes. Completion is learned by
**status sampling**: the runtime polls `GET /v1/operations/{operation_id}`
every few seconds until the state is terminal.

This is not a stylistic preference about HTTP. An operation may run for ten
minutes on another machine. A worker that held a connection open for its
duration would hold a lease and a goroutine with it, and the runtime's cost
would then scale with *how long work takes* rather than with *how many runners
exist*. Tracking distributed runners is cheap; hosting them is what bloats a
pod. So:

- The worker parks the work item as `waiting_external` after a `202`, holding
  no lease, no transaction, and no per-operation connection or goroutine.
- Sampling load scales with runners × interval, never with operation duration.
- A worker killed mid-operation strands nothing: the surviving worker's
  sampler picks the parked item up after lease handoff, and the operation
  keeps running in its own process throughout.
- Completion ingest is idempotent. Two samples racing, or a sample racing a
  callback, both resolve to the same terminal status; the fencing discipline
  that guards actor callbacks (attempt-scoped tokens, re-lease under the
  attempt's fencing token) makes the duplicate a no-op.

The runner's obligations follow from that:

1. Answer `202` **quickly**. A dispatch POST that takes minutes is a
   malfunction, not a long job.
2. Hold per-operation status durably enough to answer the status endpoint for
   at least the retention declared in the acceptance.
3. Never let an operation's status *disappear* before that retention elapses.
   A `404` on the status of an operation the runtime dispatched is not a
   completion and is never read as one — it is a dispatch error, and the
   parked attempt fails on its `waiting_external` deadline instead.

## The completion callback is strictly optional

A runner that never calls back is fully conformant. Polling is the runtime's
responsibility and the system is correct on polling alone; a callback only
tightens completion latency. **No deployment is required to provide a callback
route**, and no acceptance is required to declare `supports_callback: true`.

When the runtime offers one, it sends two headers on the execute request:

| Header | Meaning |
| --- | --- |
| `Nodes-Callback-Url` | Where the runner may POST a completion notification. |
| `Nodes-Callback-Token` | The bearer token the runner must present on that POST. |

A runner that honours them POSTs, once the operation is terminal:

```json
{ "operation_id": "op_01JAV3QK2M0000000000000011", "state": "completed" }
```

The notification carries **no result**, and that is the design. It is a hint:
on receiving one the runtime samples the status endpoint it dispatched to,
over the connection it authenticated, and learns the outcome there. Nothing is
ever committed on a callback's word, so a forged or replayed notification
costs at most one extra status read. The callback endpoint still refuses a
request whose bearer token does not match the one it issued for that
operation — an unauthenticated notification is refused with `401`, as the
actor callback ingress does.

Retries are the runner's business and are unnecessary: a callback that never
arrives, or arrives three times, changes nothing except when the runtime
happens to sample.

## Caller authentication is mandatory

**Every** request to a runner service — execute *and* status, and cancel where
implemented — carries `Authorization: Bearer <secret>`. Serving an
unauthenticated request is not a relaxed deployment posture; a runner service
accepting operations over the network is a remote-code-execution surface, and
an unauthenticated one executes code for anyone who can reach it.

| Condition | Response |
| --- | --- |
| No credential, or one the runner cannot parse | `401` |
| A valid credential that is not permitted this operation | `403` |

Implementations MUST refuse both cases. There is no loopback exemption and no
"local deployment" exemption: the runner conformance kit's auth case is run
against the reference deployment, and it fails a service that answers an
unauthenticated execute or status request with anything other than `401`/`403`.

Further requirements on implementations:

- Compare credentials in constant time.
- Never log the secret, and never echo it in an error body.
- Resolve the secret from the deployment's secret source at dispatch time. The
  registry stores a `secret_ref` — the *name* of a credential — never the
  material.
- Prefer TLS. The registry refuses a plaintext `http` endpoint to a
  non-loopback host unless the operator sets `AllowInsecureTransport` on the
  identity, because presenting a bearer secret over plaintext to another
  machine is the secret leaking. The opt-in exists so that accepting the risk
  on a trusted LAN is a deliberate, greppable act rather than an accident.

Workload identity (OIDC/mTLS) is parked to a later cycle; secret-based auth is
this cycle's boundary.

## The policy boundary: no shell wrapper

The operation schema's own words bind every implementation:

> A wrapper that checks policy and then hands an **unrestricted shell** to
> another process does not satisfy that boundary.

Concretely, a conformant runner:

- Executes `command.argv` as an argument array. Never a shell string, never a
  concatenation, never `sh -c`. If an operation genuinely needs a shell it sets
  `command.requires_shell`, and policy may reject it — that is what the field
  is for.
- Enforces `policy` **inside** dispatch: timeout, network mode, memory, pids,
  disk, allowed output paths. A field it cannot enforce is a reason to refuse
  the operation (`400`, `rejected_input`, naming the field and the cap), never
  a reason to run under a different limit silently.
- Honours the safe defaults the operation declares: network disabled, image
  pinned by digest, non-root user, read-only container root, copy-on-write
  workspace, no host Docker socket.
- Reports what it measured. A runner that cannot observe a fact says so with
  `{"measured": false, "complete": false}` and a `note`; it never fabricates
  the value, and never omits the declaration to imply completeness. Text the
  executed process printed is process-reported content, never an observation
  about that process's own behaviour.

## Errors: a dispatch failure is never a result

The honest-evidence contract `internal/runners.Runner` defines in-process is
preserved verbatim over HTTP. A `Result` exists only when an execution
actually happened and the runner can honestly describe it — *including* when
the executed process failed, timed out, or exited nonzero, which are results,
not dispatch errors. Everything else is a `*DispatchError`: no result is
recorded, because none was measured.

**A transport, auth, or refusal failure is a dispatch error —
never a fabricated failed result.** The runtime does not synthesize a
`Result` it was not given, and a runner must not synthesize one either.

| HTTP | Error kind | Retryable | Notes |
| --- | --- | --- | --- |
| `400`, `422` | `rejected_input` | no | The runner refused the operation document or a policy it cannot enforce. |
| `401`, `403` | `auth_or_policy` | no | Credential missing, wrong, or not permitted. |
| `404` (status) | `runner_unavailable` | yes | The runner forgot an operation it accepted. Resampled until the attempt's deadline — never read as a completion. |
| `409` | `rejected_input` | no | Same `operation_id`, different document. |
| `413` | `rejected_input` | no | Payload over the transport limit. The remedy is an artifact ref, never truncation. |
| `429` | `rate_limited` | yes | Honour `Retry-After`. |
| `5xx` | `runner_unavailable` | yes | |
| connection failure, timeout | `retryable_transport` | yes | |
| unparseable body, envelope disagreeing with its result | `contract_failure` | no | The runtime learned nothing it can record. |

The kinds are the result schema's `error.kind` enum, so a failure classified
here and a failure reported inside a result speak one vocabulary.

## How a node reaches a runner: the registry identity

`internal/runners.FunctionRegistry` is the allowlist and the single
enforcement point. It holds two identity forms in one namespace:

| Form | Fields | Dispatched over |
| --- | --- | --- |
| `FunctionIdentity` | ARN + pinned image digest | The platform's function-invoke API (the legacy Lambda adapter) |
| `ServiceIdentity` | endpoint + pinned image digest + `secret_ref` | This protocol |

A `ServiceIdentity` is refused at registration if it names a wildcard
endpoint, is not an absolute `http`/`https` URL, embeds credentials in the
URL, carries a query string or fragment, is plaintext to a non-loopback host
without `AllowInsecureTransport`, carries an unpinned or malformed digest, or
names no secret reference. Registration is the enforcement point: a check
skipped there is a check that happens nowhere.

```text
deliver-change/run-tests → https://runner.thor.internal:8443
                           sha256:0604fdb7…  (pinned execution image)
                           runner/thor/execute-token  (secret_ref)
```

Two consequences worth stating out loud:

- **Placement is a registry fact.** Running the same workflow digest against a
  runner on `spark` and a runner on `thor` differs by one endpoint string. The
  workflow does not change, the operation does not change, the pinned digest
  does not change.
- **A runner service grants no cloud access.** Service identities never appear
  in `FunctionRegistry.ARNs()` and so can never widen the worker's IAM policy;
  `Endpoints()` is their separate, credential-free counterpart for egress
  review.

## Conformance checklist

What a runner-conformance kit (the code-execution sibling of
`tests/conformance`) checks against a live service:

| Check | Why it matters |
| --- | --- |
| An unauthenticated execute is refused `401`/`403` | A runner that serves it executes code for anyone who can reach it. |
| An unauthenticated **status** read is refused too | Status leaks what ran, where, and with what digests. |
| Dispatch answers `202` with an acceptance echoing the operation id | Without it there is nothing to poll. |
| A repeated `Idempotency-Key` does not start the work twice | At-least-once delivery must not become at-least-twice execution. |
| Status is answerable immediately after acceptance | Polling starts before the work does. |
| A terminal status carries a schema-valid result whose `state` matches the envelope | The envelope must not disagree with its own evidence. |
| The terminal status stays readable for the declared retention | Otherwise completion is unlearnable. |
| A shell-string command is refused, `requires_shell` is honoured | The policy boundary. |
| A policy the runner cannot enforce is refused, not ignored | A silently different limit is worse than a refusal. |
| An operation whose digest does not match the registered pin is refused | Pinning that is not checked is not pinning. |
| Completion works with **no** callback configured | Polling alone must be sufficient. |

## Not covered here

- Workload identity (OIDC, mTLS) — parked; secret-based auth is this cycle's
  boundary.
- Log streaming. Logs are artifacts referenced by the result, not a channel.
- Batch or multiplexed submission. One operation, one request.
- Cost budgets and quota negotiation (PRD Phase 4).

## Decisions recorded here

Choices this document had to make that the spec left open, recorded rather
than absorbed:

- **The callback rides in headers, not the operation body.** The operation
  schema is closed (`additionalProperties: false`) and crosses the wire
  verbatim, so a callback URL cannot live in it without forking the schema.
- **The callback is a notification, not a result delivery.** It carries no
  result document; the runtime re-reads status over the connection it
  authenticated. This makes "callbacks only tighten latency" literally true
  and leaves no path for a forged completion.
- **The registered digest is the execution-environment pin** — the digest an
  operation's `execution.image_digest` must match — mirroring the function
  form. A runner service's own build revision remains `runner_revision` on the
  operation and in the result's `environment`.
- **Plaintext HTTP is refused to non-loopback hosts unless opted in.** The
  spec required mandatory auth but did not decide transport; refusing by
  default with an explicit `AllowInsecureTransport` escape keeps a LAN
  deployment possible without making it silent.
- **Auth has no loopback exemption**, unlike transport: the contract says
  authentication is mandatory, and a local exemption is the one every remote
  deployment inherits by copy-paste.
