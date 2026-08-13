# culture-nodes-human-inbox-bridge

A reference PRD §13 actor-protocol bridge for **humans as actors** (issue
38a, spec claim c11): an invocation does not dispatch a subprocess — it
parks as a durable inbox task until a person lists it and submits a result,
at which point the bridge delivers the terminal `completed` event through
the same authenticated callback path the agent bridges use.

The point the adapter proves: `actors.kind` needs **zero engine-side
branching** for humans. `kind` is free text with no CHECK, dispatch never
reads it, and `internal/actors/doc.go` names a human group as an intended
actor case — so a `kind=human` actor behind this bridge completes nodes
through the standard 202-plus-callback path with
`internal/actors/neutrality_test.go` untouched.

It mirrors `adapters/colleague` and `adapters/claude-code` in layout and
discipline: stdlib-only runtime dependencies (`dependencies = []`), the
same config / idempotency / callback-delivery shapes, and the same
`proposed`-only trust model.

## How it differs from the agent bridges

| Concern | Agent bridges (colleague / codex / claude-code) | This bridge |
|---|---|---|
| Execution | Spawns a backend subprocess | Nobody executes; a human answers later |
| Sync path | Yes, under a step threshold | Never — always 202 (§13.3) |
| Liveness | `heartbeat_after_seconds` > 0, poller heartbeats | `heartbeat_after_seconds: 0` — no promise; the worker treats 0 as an open-ended wait, so the parked task holds **no lease** |
| Callback timing | Seconds to minutes after accept | Possibly days later, across bridge restarts — so the pending task, the callback credentials, and the event-sequence counter are all persisted |
| `usage` block | Real token/cost figures passed through | **Omitted entirely.** Humans report no token usage; the protocol keeps an absent usage absent (`nil`, never fabricated zeros) |
| Ledger record | Proposed claim, `origin.kind: "agent"` | Proposed claim, `origin.kind: "human"` — via the ordinary append path a human proposes; confirmation stays a review transaction (PRD §10.4/§10.8) |
| Repo allowlist | Required (the bridge executes in repos) | None — the bridge touches no filesystem but its own state dir |

## Lifecycle

1. **Invocation** — the culture-nodes worker POSTs a §13.1 invocation. The
   bridge validates it (bearer auth, `Idempotency-Key`, protocol version,
   `input.instruction`, `callback.url` + `callback.token` — required,
   because an invocation with no callback could never be completed), writes
   the task durably to the state dir, and answers
   `202 {invocation_id, heartbeat_after_seconds: 0, supports_cancellation: true}`.
   The non-terminal `accepted` event (sequence 1) is delivered through the
   callback off-thread. Then nothing runs: no poller, no heartbeats.
2. **Park** — the task waits in the inbox. It survives restarts: everything
   needed to complete it later lives in one JSON file (mode `0600` — it
   carries the callback token).
3. **Submission** — a human lists pending tasks and submits
   `{outcome, output?, note?}`. The bridge builds the `completed` payload
   (the submitted `output` object passes through verbatim as the node's
   output; the note becomes the claim record's statement), reserves the
   next persisted sequence number, and delivers the event with retries
   through the standard authenticated callback path. Only an accepted
   delivery marks the task completed — a failed delivery answers 502 and
   leaves the task pending so the submission is never silently lost.
4. **Cancellation** (PRD §13.6) — `POST /v1/invocations/<id>/cancel` (or
   `DELETE /v1/invocations/<id>`) marks a pending task cancelled and always
   answers 202: cancellation is durable in Culture Nodes and best-effort at
   the actor.

## Routes

Protocol surface (what the culture-nodes worker talks to):

* `POST /v1/invocations` — §13.1; always answers 202.
* `POST /v1/invocations/<id>/cancel` — §13.6.
* `DELETE /v1/invocations/<id>` — alias for the same cancellation.
* `GET /healthz` — operational convenience; the only unauthenticated route.

Human surface (same server, same bearer token):

* `GET /inbox/tasks?status=pending` — list tasks; callback credentials are
  redacted from every listing.
* `POST /inbox/tasks/<id>/submit` — body `{outcome, output?, note?}`.
  `outcome` is required and never defaulted: a person who did not say what
  happened has not answered. `output` must be a JSON object when present
  (it is bound into the node's contract-shaped output).

## The human surface from a shell

```bash
export HUMAN_INBOX_BRIDGE_AUTH_TOKEN=...   # picked up by list/submit too

human-inbox-bridge list
# hit_1f60d2b8a9c4e7aa  run=run_42  2026-08-13T09:00:00+00:00
#   approve the release

human-inbox-bridge submit hit_1f60d2b8a9c4e7aa \
  --outcome approved \
  --output '{"verdict": "ship"}' \
  --note "checked the changelog and the diff by hand"
```

Or with plain curl, the same endpoints:

```bash
curl -H "Authorization: Bearer $HUMAN_INBOX_BRIDGE_AUTH_TOKEN" \
  http://127.0.0.1:8087/inbox/tasks?status=pending

curl -X POST -H "Authorization: Bearer $HUMAN_INBOX_BRIDGE_AUTH_TOKEN" \
  -d '{"outcome": "approved", "output": {"verdict": "ship"}}' \
  http://127.0.0.1:8087/inbox/tasks/hit_1f60d2b8a9c4e7aa/submit
```

## Configuration

Precedence: JSON config file (`--config` or `HUMAN_INBOX_BRIDGE_CONFIG`)
sets the baseline; `HUMAN_INBOX_BRIDGE_*` env vars override individual
fields on top of it.

| Config file key | Env var | Default | Meaning |
|---|---|---|---|
| `actor_id` | `HUMAN_INBOX_BRIDGE_ACTOR_ID` | `human-inbox-bridge` | `origin.actor_id` on emitted claim records; typically the registered actor key |
| `host` | `HUMAN_INBOX_BRIDGE_HOST` | `127.0.0.1` | Bind host |
| `port` | `HUMAN_INBOX_BRIDGE_PORT` | `8087` | Bind port (colleague holds 8085, codex/claude-code 8086) |
| `auth_token` | `HUMAN_INBOX_BRIDGE_AUTH_TOKEN` | unset | Bearer token for the invocation route AND the inbox surface. Startup refuses a non-loopback bind without one (override: `HUMAN_INBOX_BRIDGE_ALLOW_UNAUTHENTICATED=1`) |
| `heartbeat_after_seconds` | `HUMAN_INBOX_BRIDGE_HEARTBEAT_AFTER_SECONDS` | `0` | The §13.3 liveness promise. Leave at 0 for humans: no heartbeat is ever sent, and 0 tells the worker the wait is open-ended |
| `callback_timeout_seconds` | `HUMAN_INBOX_BRIDGE_CALLBACK_TIMEOUT_SECONDS` | `10.0` | Per-delivery HTTP timeout |
| `callback_max_retries` | `HUMAN_INBOX_BRIDGE_CALLBACK_MAX_RETRIES` | `5` | Redeliveries of the same event (same id, same sequence) |
| `callback_retry_backoff_seconds` | `HUMAN_INBOX_BRIDGE_CALLBACK_RETRY_BACKOFF_SECONDS` | `0.25` | Linear backoff base |
| `state_dir` | `HUMAN_INBOX_BRIDGE_STATE_DIR` | `.human-inbox-bridge-state` | Durable tasks + idempotency replays |
| `default_success_outcome` | — | `completed` | Documentation default only; submissions always name their outcome explicitly |

## Registering a `kind=human` actor

`deploy/prod/register-actor.sh` registers agent actors with a hard-coded
`kind='agent'`. A human actor follows the **same append-only revision
semantics** — actor rows are never updated in place; a change is a new
revision — with `kind='human'` and this bridge as the endpoint. Extending
the script's own INSERT pattern (same columns, same metadata contract):

```bash
# On the control-plane host, same psql lane as register-actor.sh:
PSQL="docker compose -f deploy/prod/compose.yaml exec -T postgres psql -U nodes -d nodes -tA"

NAMESPACE_ID=$($PSQL -c "SELECT id FROM namespaces ORDER BY created_at LIMIT 1")

$PSQL -c "INSERT INTO actors
  (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata)
  VALUES
  ('actor_register_$(date +%s%N)_$$',
   '$NAMESPACE_ID',
   'company/human-ops',
   1,
   'human',
   'http',
   'http://192.168.1.157:8087',
   '{\"auth_token_env\": \"HUMAN_OPS_BRIDGE_TOKEN\"}'::jsonb)"
```

Notes, mirroring the script's own rules:

* `endpoint_ref` must use a numeric IPv4 host (worker containers do not
  inherit the host's `/etc/hosts`).
* `metadata.auth_token_env` is the **name** of the env var the worker reads
  the bridge credential from at dispatch time — never the token value
  itself. Export that env var (set to this bridge's `auth_token`) in the
  worker's environment.
* Re-registering an existing `actor_key` appends `revision + 1`; read the
  newest revision first, exactly as the script does. (t13 adds an
  authenticated `POST /v1alpha1/actors` API as the non-raw-SQL lane;
  prefer it once it exists.)

A workflow node then names the actor as usual — dispatch reads the
endpoint, not the kind, which is the whole point.

## Trust model: `proposed`-only, no usage, no evidence

The bridge emits exactly one ledger record per completion: a `claim` with
`authority: "proposed"` and `origin: {kind: "human", actor_id: <cfg>}`.
A submission is the human's completion claim, not verified evidence — the
run's approval surface is where anything gets confirmed (PRD §10.4/§10.8:
confirmation and rejection are review transactions, and no actor promotes
its own proposal). No `usage` block is ever attached: omit, never
fabricate.

## Running it

```bash
uv sync
uv run human-inbox-bridge serve --config bridge.json
# or, without installing the console script:
uv run python -m human_inbox_bridge serve
```

## Tests

```bash
uv sync
uv run pytest
```

The server tests drive real HTTP over loopback against a real state dir
and a stub §13.4 callback receiver, covering the t12 acceptance set: the
202 park with a durable pending task and no lease-holding behavior, the
submission completing the invocation through the standard authenticated
callback (envelope asserted event by event), and pending tasks surviving a
bridge restart (the server is torn down and rebuilt over the same state
dir mid-test).
