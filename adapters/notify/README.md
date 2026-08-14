# culture-nodes-notify-bridge

An actor-protocol bridge that posts a workflow-declared message to a
Discord webhook (issue [#68](https://github.com/agentculture/culture-nodes/issues/68)).
A `notify` node reaches it as an ordinary `kind: agent` node — this bridge
runs OUTSIDE the culture-nodes deployment, and the control-plane process
never sees the webhook URL.

## Why an actor, not a node kind

The PRD boundary rule is that **the control-plane process gains no
Discord egress**. That is why the run-lifecycle notifier
(`internal/notify` / `cmd/nodes-notifier`, tasks t13/t14) is an
out-of-process daemon consuming the SSE feed instead of a package inside
the API, and why `internal/events/doc.go` keeps content out of events
entirely. A `kind: notify` node the worker executed directly would put a
webhook POST inside the control plane and undo that. An actor is the
shape the system already has for "something outside the deployment does a
real-world thing on the engine's behalf" — `adapters/human-inbox` is the
precedent this bridge follows in layout and discipline.

## The three Discord lanes, kept distinct

| lane | what it answers | when it fires | ledger |
|---|---|---|---|
| `cmd/nodes-notifier` (t14) | "what is the system doing?" | run lifecycle, unattended | none — it observes |
| **this bridge** (issue #68) | "tell this person this thing" | a declared step in a workflow | a record per send |
| `scripts/notify-discord.sh` (where present) | an operator pinging the channel by hand | ad hoc | none |

The notifier answers a monitoring question and must never be the
mechanism a workflow depends on for delivery. This bridge is the one a
plan can declare and a run can be gated on.

## Ported, not reinvented

`internal/notify` is the Go port of devex's proven webhook design
(itself documented in that package's `doc.go`); this bridge ports the
same transport rules again in Python rather than inventing a third set
(`webhook.py`):

* the webhook URL is env-only (`CULTURE_NODES_WEBHOOK_URL`, falling back
  to `DISCORD_WEBHOOK_URL`), resolved fresh on every send and **never**
  stored, logged, or written to a config file — see "Trust model" below;
* one bounded 5-second POST, no retries;
* redirects are never followed — a 3xx response is treated exactly like
  any other non-2xx status;
* every failure mode collapses to the same `FAILED` result, so the
  fail-open default never needs a cause-specific branch.

The **payload shape** deliberately does NOT mirror `internal/notify`'s
fixed five-field run-lifecycle envelope. That envelope exists to keep
ledger records, node output, and workflow input out of an *unattended*
notification. This bridge's message *is* exactly what the workflow node's
`input` asked to be sent — the workflow author is the trust boundary
here, the same way any node's `input` is author-controlled.

## Invocation contract

A `notify` node's `input`:

```jsonc
{
  "content": "optional top-level message text",
  "title": "optional embed title",
  "description": "optional embed description / message body",
  "fields": [
    {"name": "Run", "value": "run_1", "inline": true}
  ],
  "require_delivery": false
}
```

At least one of `content`, `title`, `description` must be a non-blank
string, or the bridge refuses the invocation as `actor_rejected_input`.
When the resolved webhook URL is a Discord webhook endpoint
(`webhook.is_discord_url`), the message is shaped as a Discord embed;
otherwise a generic flat-JSON object is posted so any other webhook
receiver can read it.

## Fail-open by default; `require_delivery` is the declared exception

* **Default** (`require_delivery` absent or `false`): the node always
  reports the `sent` domain outcome — a webhook outage, a non-2xx
  response, or no webhook configured at all never fails the node or stalls
  the run. `output.delivered` and `output.status_code` still carry the
  real result, for a workflow that wants to look without gating on it.
* **`require_delivery: true`**: anything other than a 2xx (including "no
  webhook configured") reports the `delivery_failed` domain outcome
  instead of `sent` — a **routable** outcome (an edge, not an engine
  failure), the same shape `budget_exhausted` took in task t11. A
  notification that is the *point* of the node (an approval request, a
  page) authors this so a fallback edge can run.

```yaml
notify:
  kind: agent
  ownerRef: team/platform-ai
  uses: actor://company/notify-discord@sha256:<digest>
  input:
    bindings:
      title: /run/input/title
      description: /nodes/build/output/summary
      require_delivery: /run/input/notify_is_gate   # or a literal `true`
  contract:
    outcomes:
      sent:
        schema: {type: object}
      delivery_failed:
        schema: {type: object}
```

## Trust model: `proposed`, status codes only

The bridge emits exactly **one** ledger record per invocation: a `claim`
with `authority: "proposed"` and `origin: {kind: "agent", actor_id: <cfg>}`.
This is a `proposed` claim, never `observed` — the bridge is not a trusted
measuring runner, and a 2xx from Discord is evidence the request was
*accepted*, not that a human read it (PRD §10.4).

The record's `data` block is deliberately narrow — **only status codes**:

```json
{
  "statement": "posted a Discord notification (status 204)",
  "kind": "notify-send",
  "outcome": "sent",
  "delivered": true,
  "status_code": 204
}
```

The webhook URL **never** appears in this record, in the node's `output`,
in any HTTP response this bridge writes, in a config file, in argv, or in
a log line. `mapping.py`'s module docstring and `tests/test_mapping.py`
(`test_ledger_record_and_result_never_carry_the_webhook_url_or_message_body`)
hold this as a structural guarantee, not just a convention: the URL is
resolved directly by `server.py` from `webhook.resolve_webhook()` into a
local variable that `mapping.py` never receives at all.

## Routes

* `POST /v1/invocations` — PRD §13.1. **Always answers 200** (§13.2) with
  a synchronous `InvocationResult` — never 202. `webhook.post` is a single
  call bounded to 5 seconds, so every invocation completes well inside a
  synchronous response; there is no callback surface at all.
* `POST /v1/invocations/<id>/cancel` / `DELETE /v1/invocations/<id>` —
  PRD §13.6. Always answers `202 {"status": "cancel-requested"}`. Every
  invocation is already finished by the time its response reaches the
  caller, so there is never anything outstanding to cancel; answering
  success unconditionally (the sibling bridges' own convention) means a
  generic actor client never has to special-case this bridge.
* `GET /healthz` — operational convenience, no protocol meaning, the only
  unauthenticated route.

## Configuration

Precedence: JSON config file (`--config` or `NOTIFY_BRIDGE_CONFIG`) sets
the baseline; `NOTIFY_BRIDGE_*` env vars override individual fields on
top of it. **The webhook URL is not a config field** — see "Trust model".

| Config file key | Env var | Default | Meaning |
|---|---|---|---|
| `actor_id` | `NOTIFY_BRIDGE_ACTOR_ID` | `notify-bridge` | `origin.actor_id` on emitted claim records; typically the registered actor key |
| `host` | `NOTIFY_BRIDGE_HOST` | `127.0.0.1` | Bind host |
| `port` | `NOTIFY_BRIDGE_PORT` | `8088` | Bind port (human-inbox holds 8087, colleague 8085, codex/claude-code 8086) |
| `auth_token` | `NOTIFY_BRIDGE_AUTH_TOKEN` | unset | Bearer token for the invocation route. Startup refuses a non-loopback bind without one (override: `NOTIFY_BRIDGE_ALLOW_UNAUTHENTICATED=1`) |
| `state_dir` | `NOTIFY_BRIDGE_STATE_DIR` | `.notify-bridge-state` | Durable Idempotency-Key replay store only — this bridge holds no other state |

| Webhook env var (never a config field) | Default | Meaning |
|---|---|---|
| `CULTURE_NODES_WEBHOOK_URL` | unset | The webhook URL, tried first |
| `DISCORD_WEBHOOK_URL` | unset | Fallback, tried when the primary is unset or blank |

With neither webhook env var set, every send resolves to `PostResult.DISABLED`
— the default (`require_delivery` absent) still reports `sent`; the run
stays green with no webhook configured at all.

## Registering the actor

Following `deploy/prod/register-actor.sh`'s pattern (same as
`adapters/human-inbox`'s README describes for `kind='human'`), register
this bridge with `kind='agent'`:

```bash
PSQL="docker compose -f deploy/prod/compose.yaml exec -T postgres psql -U nodes -d nodes -tA"
NAMESPACE_ID=$($PSQL -c "SELECT id FROM namespaces ORDER BY created_at LIMIT 1")

$PSQL -c "INSERT INTO actors
  (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata)
  VALUES
  ('actor_register_$(date +%s%N)_$$',
   '$NAMESPACE_ID',
   'company/notify-discord',
   1,
   'agent',
   'http',
   'http://192.168.1.157:8088',
   '{\"auth_token_env\": \"NOTIFY_DISCORD_BRIDGE_TOKEN\"}'::jsonb)"
```

`endpoint_ref` must use a numeric IPv4 host (worker containers do not
inherit the host's `/etc/hosts`). `metadata.auth_token_env` names the env
var the worker reads the bridge credential from at dispatch time — never
the token value itself.

## Running it

```bash
uv sync
export CULTURE_NODES_WEBHOOK_URL=...   # or DISCORD_WEBHOOK_URL; unset = disabled, fail-open
export NOTIFY_BRIDGE_AUTH_TOKEN=...
uv run notify-bridge serve
# or, without installing the console script:
uv run python -m notify_bridge serve
```

## Tests

```bash
uv sync
uv run pytest
```

`tests/conftest.py` unsets both webhook env vars before every test (the
Python equivalent of `internal/notify/testmain_test.go`'s `TestMain`), so
the suite never depends on — or leaks into — the real environment; tests
that need a webhook point `CULTURE_NODES_WEBHOOK_URL` at a local fake
HTTP server (`tests/test_server_unit.py`'s `_FakeDiscord`), never a real
Discord endpoint.

Coverage highlights, by acceptance bullet (issue #68):

* *an example workflow sends a message through the actor and the run
  records it* — `test_successful_send_is_synchronous_and_records_the_claim`
  (server) proves a 200 synchronous result carrying a `proposed` ledger
  claim record with the delivery status.
* *the webhook URL appears in no config file, no argv, and no log line* —
  `test_config.py`'s webhook-surface tests prove `Config` has no such
  field/env var/file key at all; `test_no_response_ever_carries_the_
  webhook_url` and `test_mapping.py`'s
  `test_ledger_record_and_result_never_carry_the_webhook_url_or_message_body`
  prove neither the HTTP response nor the ledger record ever contains it.
* *a CI gate proves no webhook egress inside `internal/` or `cmd/` outside
  the notifier* — `tests/lint/webhookisolation_test.go` in the Go module
  root (mirrors `tests/lint/github_isolation_test.go`'s pattern).
* *`require_delivery: true` routes a failed send as a domain outcome; the
  default does not* — `test_require_delivery_true_routes_delivery_failed`
  vs. `test_webhook_outage_default_settings_stays_sent` /
  `test_webhook_5xx_default_settings_stays_sent`.
* *a webhook outage with the default settings leaves the run green* —
  the same two default-settings tests: the node answers `200` with
  outcome `sent`, never a failure.
