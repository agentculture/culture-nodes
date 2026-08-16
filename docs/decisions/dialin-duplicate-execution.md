# Dial-in duplicate execution: carried by measurement

Status: decided 2026-08-16 (plan task t20, cycle
`upkeep-dispatch-stable-addresses-gate-as-node`). Disposition: **INSTRUMENT**.

## The risk

`docs/decisions/transport-inversion.md` names this and explicitly declines to
solve it:

> A dropped poll does not revoke a claim. The same bridge identity may
> reconnect and resume it until its claim expires. […] A different worker may
> meanwhile believe it owns the workflow attempt after a lease transition.
> Fencing tokens keep stale workflow writes from committing, but the dial-in
> liveness signal is not consulted by the lease model. Consequently duplicate
> execution or delayed completion is possible in that window even though a
> stale final write is refused. This is an **unresolved liveness risk**, not
> silently solved by transport inversion.

It mattered enough to decide now because this cycle converts the whole fleet to
dial-in and disables the outbound fallback. That makes the window the **steady
state** rather than a transitional exposure — and nobody has ever measured how
often it opens.

## The window, concretely

Traced by `codex-thor` in run `01M05ZH6AW6TFDSESF3B0GKD3A` (graded 5), with
commit-qualified references:

1. Worker A holds a workflow lease and invokes the actor through dial-in. Actor
   resolution selects dial-in from a presence observation no older than 30
   seconds (`internal/worker/registry.go`); `Client.Invoke` then waits in
   `InvokeInbound` (`internal/actors/client.go`).
2. `InvokeInbound` inserts one durable mailbox row keyed by namespace and
   attempt id, then polls until that row carries a completed response
   (`internal/store/postgres/inbound_transport.go`; uniqueness from
   `migrations/0033_inbound_transport.sql`).
3. A bridge poll claims that row for 90 seconds. **The claim sets `claimed_at`
   and `claim_until` but carries no bridge-instance identity and has no
   relationship to the workflow lease.** That independence is the whole defect.
4. The bridge forwards the request to its local invocation handler with the
   mailbox attempt id as `Idempotency-Key` (`dialin.py`). The poll connection or
   the completion POST is then lost.
5. If Worker A's lease heartbeat stops, the lease expires, reclaim returns the
   work item to `ready` (`internal/store/postgres/claiming.go`), and Worker B
   claims it with a higher fencing token and attempt number.
6. Worker B dispatches again — new attempt id, therefore a **second mailbox
   row**. The first claim may still be executing, may be redelivered after its
   90-second claim expires, or may return late. The mailbox claim clock and the
   workflow lease clock are independent.

A bad close is one of: both deliveries execute; the old delivery completes first
and Worker A's completion is refused as stale; or the newer delivery completes
authoritatively while the old execution continues uselessly.

The stale write **is** refused — completion requires the current work id, worker
id, fencing token and attempt. That protects orchestration state. It does not
protect external execution cost.

## Is it billable? Yes — sometimes, and we cannot currently tell which

- Codex's async path calls `codex_cli.spawn` **before** the durable idempotency
  response is written (`async_runner.py`, `server.py`). There is a crash window
  between the two.
- The bridge's file-backed idempotency store normally prevents a repeat of the
  *same key* — but Worker B's replacement dispatch gets a **new attempt id**, so
  it presents a different key even though it is a replacement execution of the
  same work.
- Different bridge instances sharing one actor identity do not share that local
  file store.
- The control plane charges a cold session immediately before invocation
  (`internal/worker/dispatch.go`), so the replacement dispatch is counted
  economically too.

There is already a coarse outer bound — one work item is dispatched at most
three times — but it does not measure *this* risk and can multiply across
workflow-declared retries, which create new work items.

## The decision

**Instrument the overlap. Change neither lease nor liveness behaviour.**

Bounding it was rejected because there is no honest number to bound it at: no
production evidence exists about frequency, so any ceiling would be invented.
Accepting it time-bounded was rejected because a deadline without a measurement
just re-raises the same undecidable question later. Instrumenting produces the
evidence the other two dispositions need.

### What gets recorded

On every inbound mailbox claim, detect transactionally (beside the claim update
in `Store.ClaimInbound`) whether another **claimed, incomplete** mailbox row
exists for the same `node_run_id` under a different `attempt_id`. Append a
durable event:

```text
dev.culture.nodes.dialin.execution_overlap
```

carrying `namespace_id`, `actor_key`, `run_id`, `node_run_id`, `work_id`,
`new_mailbox_id`, `new_attempt_id`, `new_work_attempt`, `new_fencing_token`,
`prior_mailbox_id`, `prior_attempt_id`, `prior_claimed_at`, `prior_claim_until`,
`detected_at`, plus `bridge_instance_id` and `provider_invocation_id`.

### The field that keeps this honest

`new_session_started = true | false | unknown`.

The bridge reports `true` immediately after its provider spawn actually begins
and `false` when its local idempotency store replays a response. **Until that
acknowledgement exists, the control plane must record `unknown` — it must never
infer billing from mailbox pickup.** Counting a pickup as a billed session would
manufacture a number, which is the failure this whole cycle is about.

Two counters, kept separate:

- `culture_nodes_dialin_execution_overlaps_total`
- `culture_nodes_dialin_duplicate_sessions_total` — incremented **only** when
  `new_session_started = true`

Metrics are labelled by namespace and actor key only; every id stays in the
durable event, to keep cardinality bounded.

### Correlation, not authority

`work_id` and `fencing_token` travel as mailbox **correlation metadata**. They
are not actor-controlled workflow authority, and nothing in the claim path may
treat them as such.

## What this record does not claim

It does not claim transport inversion, idempotency keys, or fencing solves the
lease/liveness interaction. They do not. This record is how the risk is
*carried*, and it stays open until production evidence establishes the overlap
frequency and the confirmed billable-duplicate rate.

Disabling the outbound fallback does not close it either — dial-in becomes the
steady state, which is precisely why the measurement is needed.

**The instrumentation described here is decided, not implemented.** Implementing
it is tracked separately; this file is the disposition t20 required, and a
disposition written down is what "silence is not an outcome" meant.
