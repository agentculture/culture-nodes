# Transport inversion: mixed mode

Status: proposed, decided before any bridge implementation changed (t23).

## Decision

Use **mixed mode**. The control plane accepts authenticated bridge dial-ins
while retaining the existing outbound `endpoint_ref` transport. Dispatch
prefers a currently dialled-in actor and falls back to outbound HTTP when no
inbound session is available. A bridge reconnects with its actor key (or a
host identity where explicitly configured); neither the connection protocol
nor PostgreSQL stores its IP address.

The fleet cannot cut over atomically: codex runs on thor and orin, while
claude-code, human-inbox, and notify run on spark. The five bridge packages
therefore gain the same dial-in client and configuration in one repository
change, but operators enable them one at a time. During the conversion an
enabled bridge and a legacy bridge are both valid dispatch targets.

PostgreSQL remains authoritative. A durable invocation mailbox records work,
responses, ownership, and expiry. Connection wakeups are disposable signals;
losing one only delays a poll. No connection's source address is persisted or
used as identity.

The rejected flag-day choice would have removed the fallback sooner and made
the steady-state code and operator surface smaller. Its cost is an outage-sized
failure domain: five services on three hosts would have to change together,
despite the stated inability to cut them over atomically, and rollback would
also be fleet-wide. Mixed mode instead pays temporary implementation and
operational complexity: two routing paths must be tested and watched until
t24 removes stored participant addresses.

## Authentication clock started by this change

Migration 0031 calls its single verifier record temporary only until the first
dial-in connection is accepted. This transport is that event. Before enabling
the first bridge, replace the simple record under issue #111 with the promised
per-actor authentication and authorization model, or explicitly accept and
time-bound that security debt. The admission path still applies t22's
rate-limit, lockout, and revocation checks before returning any work.

## Routing and reconnect semantics

Each bridge makes an authenticated, bounded long poll to the control plane.
The server claims at most one durable mailbox row for that actor and returns
the protocol request. The bridge executes its existing in-process invocation
handler and posts the protocol response against the mailbox id. A waiting
worker reads the durable response. Cancellation follows the same mailbox
shape; legacy cancellation remains outbound during mixed mode.

A dropped poll does not revoke a claim. The same bridge identity may reconnect
and resume it until its claim expires. This intentionally preserves the
existing idempotency key. A different worker may meanwhile believe it owns the
workflow attempt after a lease transition. Fencing tokens keep stale workflow
writes from committing, but the dial-in liveness signal is not consulted by
the lease model. Consequently duplicate execution or delayed completion is
possible in that window even though a stale final write is refused. This is an
**unresolved liveness risk**, not silently solved by transport inversion; the
operator should retain invocation/mailbox rows when investigating it.

## Rollback at 03:00

Rollback is one bridge at a time and does not require reverting code.

1. Stop enabling new inbound work for the affected actor in the control-plane
   routing configuration. Leave the API and workers running.
2. Inspect its durable mailbox for claimed or completed-but-uncollected rows.
   Do not delete these rows, attempt records, idempotency keys, fencing tokens,
   callback tokens, or ledger records. They are the state that must survive.
3. Allow an executing claim to finish, or wait for its claim expiry. If urgency
   requires stopping the bridge, record its mailbox and invocation ids first;
   the reconnect/expiry behavior above applies.
4. Confirm the actor's latest registration still has the pre-cutover
   `endpoint_ref` and its outbound bearer-token environment variable is
   present in every process that can dispatch or cancel it.
5. Restart only that bridge without its dial-in control-plane URL and dial-in
   credential. It returns to its existing listening service.
6. Dispatch a canary to that actor and check the worker event names the
   outbound transport and the registered endpoint revision. Then check the
   attempt and ledger; do not infer success merely from an HTTP 2xx.
7. Repeat for another actor only after the prior actor's canary finishes. A
   safe mid-rollback fleet has some bridges dialled in and some outbound; that
   is the normal mixed-mode state.

Rollback is complete when all five dial-in clients are disabled, no mailbox
claims remain live, and outbound canaries have reached codex on thor and orin
and claude-code, human-inbox, and notify on spark. Keep the mailbox rows through
the incident review.

## Shared and backend-specific implementation

The dial-in transport, authentication headers, retry/backoff, mailbox wire
messages, and configuration validation are shared behavior. Each backend must
start that client and hand a received request to its existing invocation
handler. That last call is necessarily package-specific because the five
servers own different mapping, idempotency, execution, and response code; it
is an adapter seam, not permission for a backend-specific transport dialect.

## Operator live demonstration

The repository session cannot open sockets. Run this ordered demonstration in
the operator lane after issue #111's replacement is deployed:

1. On thor, migrate the control-plane database and configure an inbound
   credential for the selected actor key. Configure the API/worker dial-in
   mailbox feature while leaving outbound routing enabled.
2. On orin, choose the codex actor used for the proof. Save its current
   registration revision and `endpoint_ref` for rollback, then register a new
   revision for the same actor key with **no `endpoint_ref`**. Query the newest
   row and save output showing `endpoint_ref IS NULL`; this is the proof that
   the control plane holds no address, not merely that an address was unused.
3. On orin, configure `CODEX_BRIDGE_CONTROL_PLANE_URL` with thor's API URL,
   `CODEX_BRIDGE_DIAL_TOKEN` with that actor's presented credential, and
   `CODEX_BRIDGE_ACTOR_KEY` with the exact registered key. Restart only the
   orin codex bridge. Leave thor's codex bridge unconverted and listening on
   its registered endpoint to demonstrate simultaneous transports.
4. From thor, run `uv run nodes run create <workflow-version-id>` for a
   one-node workflow whose `uses` names the orin actor key. Save the run id.
5. Run `uv run nodes run events <run-id>` and
   `uv run nodes run get <run-id>`. Criterion 2 is met only when the output
   ties the completed attempt to that actor key and records the inbound
   mailbox/transport id while step 2's query shows the newest actor row has no
   address. A merely accepted run, empty queue, or HTTP success is insufficient.
6. Dispatch a second canary to the still-legacy thor actor and save its event
   showing outbound transport. The paired outputs prove mixed mode: one
   converted bridge and one unconverted bridge were live simultaneously.
7. On failure, preserve the run, attempt, ledger, and mailbox ids and execute
   the rollback above. Restore the saved address only by registering a new
   actor revision; never mutate the append-only old row.
