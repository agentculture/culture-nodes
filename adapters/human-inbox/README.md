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
* `POST /inbox/tasks/<id>/submit` — body `{outcome, output?, note?}` for a
  manual submission. The sibling merge tracker adds a validated
  `observed: {collection_method, merge_commit}` marker; manual clients do
  not need to send it.
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

## Authoring the node: human-timescale deadlines

A human answers in days, not minutes — and every time bound a node gets is
**authored configuration**, never actor-kind special-casing (issue 38 /
spec claim c28; `internal/actors/neutrality_test.go` enforces the absence
of branching). Three settings carry the whole pattern:

* **`policy.timeout`** is the async wait bound: it becomes the parked
  attempt's durable deadline timer, verbatim and unclamped (the 900s cap
  is a code-node runner limit and never touches agent nodes). Durations
  are Go literals topping out at the hour unit, so a week is `168h`.
  Without it, this bridge's `heartbeat_after_seconds: 0` makes the wait
  genuinely open-ended — author the bound you mean.
* **`policy.retry.maxAttempts: 1`** — a human is asked once. A timeout or
  failure routes the node's declared edge instead of automatically
  re-asking. (The worker's separate dispatch budget counts *dispatches*,
  not elapsed time: a parked task consumes exactly the one dispatch that
  started it, however long the person takes.)
* **`spec.limits.maxDuration`** must contain the park: the run-level
  wall-clock bound is checked when the late callback resumes the run, and
  the `1h` default would end a five-day run right there. Two weeks is
  `336h`.

```yaml
spec:
  limits:
    maxDuration: 336h          # the run outlives the longest human wait

  nodes:
    review:
      kind: agent              # an ordinary node; the ACTOR is human
      ownerRef: team/platform-ai
      uses: actor://company/human-ops@sha256:<digest>
      input:
        from: /run/input
      contract:
        outcomes:
          completed:
            schema: {type: object}
      policy:
        timeout: 168h          # a week to answer, as a durable deadline
        retry:
          maxAttempts: 1       # ask once; route edges, don't re-ask
```

`internal/worker/humanpace_test.go` proves the pattern end to end with a
simulated clock: the `168h` timeout lands unclamped in the deadline timer
and the §13.1 invocation, five simulated days of sweeps and worker ticks
leave the park untouched (no timeout, no retry, dispatch budget still at
one), and the day-five callback completes the run normally.

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

## Observable-declaration convention (t15 / c11 / h8)

A workflow node that targets a `kind=human` actor through this bridge can
declare an **observable** in its input: any non-`instruction` key round-trips
verbatim into the bridge's stored `extra_input` (server.py line 369:
`extra_input={k: v for k, v in raw_input.items() if k != "instruction"}`),
where an external tracker reads it to watch for real-world completion.

```yaml
input:
  bindings:
    instruction: /run/input/merge_instruction
    prNumber: /nodes/fix/output
    observe:
      kind: github_pr_merged
      pr: /nodes/fix/output/pr_number   # or a literal: pr: 42
```

The tracker contract:

* **Auto-submit on merge only.** When the tracker observes the declared
  observable reaching its target state (e.g. a `github_pr_merged` event
  for the given PR), it calls the bridge's existing submit surface
  (`POST /inbox/tasks/<id>/submit`) with the observed outcome — no human
  intervention needed.
* **Manual submit always remains.** A person can still submit the task
  through the inbox surface at any time; the tracker's auto-submission
  does not remove or block the manual path.
* **Only declared observables are watched.** Tasks with no `observe` key
  behave exactly as today — purely manual.

The `observe` value is a free-form object; the tracker interprets the
`kind` field to select the right external check. Two kinds are supported:
`github_pr_merged` (t16, merge-as-action) and `github_pr_reply` (issue #71,
the pr-upkeep decision node). Both accept `repo: owner/name`; when absent,
the tracker uses its configured default repository.

`github_pr_merged`: a task-level `input.success_outcome` is used when
present; otherwise this observation kind reports its unambiguous `merged`
outcome.

`github_pr_reply`: three outcomes, all optionally renamed per-task via
`answered_outcome`/`merged_outcome`/`dropped_outcome` in the `observe`
block (defaulting to those three literal names) — a qualifying reply
submits the answered outcome, the PR being merged submits the merged
outcome, and the PR closing unmerged submits the dropped outcome. See
"The github_pr_reply observation kind" below for the full contract and its
rate-budget arithmetic.

### Running the GitHub merge tracker

The tracker is a separate stdlib-only process beside the bridge. It reads
only `pending` task files from the same durable state directory, calls
`GET /repos/{repo}/pulls/{number}` anonymously for public repositories or
with `GITHUB_TOKEN` when one is present, and talks back only to the bridge's
authenticated submit surface. It calls the Culture Nodes control plane
exactly once, at startup, and only to read
`GET /v1alpha1/actors` for the identity check below — never during a poll
cycle, and never with a callback credential. `merged: true` plus a non-empty
`merge_commit_sha` is the sole auto-submit state; `closed` with
`merged: false`, malformed or unsupported declarations, and undeclared tasks
stay manual.

```bash
# Optional: export GITHUB_TOKEN=... for private repositories or higher cadence
export HUMAN_INBOX_BRIDGE_AUTH_TOKEN=...       # submit auth to the sibling bridge
export HUMAN_INBOX_BRIDGE_STATE_DIR=.human-inbox-bridge-state
export HUMAN_INBOX_BRIDGE_ACTOR_ID=company/human-ops        # the actor this bridge serves
export HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL=http://192.168.1.5:18080
export HUMAN_INBOX_TRACKER_DEFAULT_REPO=agentculture/culture-nodes

uv run python -m human_inbox_bridge.tracker
# operational/test probe: run exactly one bounded cycle
uv run python -m human_inbox_bridge.tracker --once
```

| Env var | Default | Meaning |
|---|---|---|
| `GITHUB_TOKEN` | unset | Optional GitHub bearer token; unset selects anonymous public-repository polling (60 requests/hour), present selects authenticated polling (5,000 requests/hour) |
| `HUMAN_INBOX_TRACKER_STATE_DIR` | bridge config's `state_dir` | Durable bridge state directory to scan read-only |
| `HUMAN_INBOX_TRACKER_BRIDGE_URL` | loopback + bridge config's `port` | Sibling bridge base URL |
| `HUMAN_INBOX_BRIDGE_AUTH_TOKEN` | bridge config's `auth_token` | Bearer token for the bridge submit surface |
| `HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL` | unset | Control-plane base URL for the startup identity check below. Unset **disables the check** and logs a warning naming what is then unguarded |
| `HUMAN_INBOX_TRACKER_DEFAULT_REPO` | unset | Fallback GitHub `owner/repository` when `observe.repo` is absent |
| `HUMAN_INBOX_TRACKER_POLL_SECONDS` | `60` | Requested delay between cycles; clamped to the active lane's minimum safe cadence (60 seconds anonymous, 0.72 seconds authenticated) |
| `HUMAN_INBOX_TRACKER_GITHUB_REQUEST_BUDGET` | `50` | Requested maximum unique PR GETs per cycle (`0` disables GitHub requests); clamped so `budget × 3600 / poll_seconds` cannot exceed the active lane's hourly ceiling |
| `HUMAN_INBOX_TRACKER_HTTP_TIMEOUT_SECONDS` | `30` | Timeout for each GitHub GET and bridge POST |
| `HUMAN_INBOX_TRACKER_REPLY_IGNORED_LOGINS` | unset | Comma-separated extra GitHub logins the `github_pr_reply` "which reply counts" rule ignores, ALWAYS unioned with the built-in default (`qodo-code-review[bot]`) — an operator cannot accidentally re-admit it |

At the defaults, anonymous mode makes at most one request per cycle and
rotates that request fairly across distinct watched PRs. Authenticated mode
makes at most 50 per cycle. A GitHub rate-limit response backs off to the
reported reset time and retries on the next cycle; a non-rate-limit `403` is
logged separately as a permission problem.

#### Startup identity check: this bridge must be the actor's bridge

Before its first cycle, the tracker resolves `HUMAN_INBOX_BRIDGE_ACTOR_ID`
against `GET /v1alpha1/actors` on `HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL`,
takes that actor_key's **newest revision** (actor identity is append-only —
an endpoint move is a new row, never an update), and compares its
`endpoint_ref` against `HUMAN_INBOX_TRACKER_BRIDGE_URL`. On a mismatch it
prints both endpoints and exits non-zero (issue #72):

```text
error: this tracker submits to http://127.0.0.1:8087, but actor
'company/human-ops' (revision 2) is registered at http://192.168.1.157:8090
— a different bridge. Refusing to start: ...
```

Why a refusal rather than a warning: the bridge's idempotency store is
**per-bridge and file-based** (one JSON file per key under `state_dir`), so
it can only deduplicate submissions that pass through the same bridge
process's state directory. Two bridges serving one actor never see each
other's replays, which makes "one logical human inbox" a deployment
convention — and this check the only mechanism that can notice the
convention has been broken.

Comparison rules, and what each one deliberately does not excuse:

* **Host and port, not the URL string.** A tracker on the actor's own host
  addresses the bridge as `http://127.0.0.1:8090` while the actor row names
  `http://192.168.1.157:8090`. Those are the same bridge, so the check
  resolves whether the registered address is one this machine itself answers
  on rather than comparing text.
* **A matching port on another host is still a mismatch** — that is exactly
  the split this guards against.
* **A different port on the same host is a mismatch** — two bridge
  processes, two idempotency stores, and only one of them is the actor's.
* **Failure to resolve is a refusal.** An unreachable control plane, an
  actor_key with no registration, and an unusable `endpoint_ref` all exit
  non-zero: an unverified identity is not a verified one. The systemd unit
  restarts, so a control plane that is merely restarting costs retries, not
  an unguarded window.

Leaving `HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL` unset skips the check and
logs a warning naming the bridge, the actor, and the fact that nothing is
then guarding against a split deployment.

An automatic submit uses the task success outcome and a note naming the
merge commit, plus this explicit marker:

```json
{
  "observed": {
    "collection_method": "github_pr_merged",
    "merge_commit": "9f64f1bc75353f4b2e6b232f5668e338168b794e"
  }
}
```

The server validates that exact marker shape. Mapping then emits a
`data.kind: "observed-submission"` claim carrying the collection method
and merge commit. The record remains `authority: "proposed"` and retains
the bridge actor origin; this attribution does not claim runner-observed
authority. A submission without the marker follows the original
`human-submission` mapping unchanged, even when its task has an `observe`
declaration.

### The github_pr_reply observation kind (issue #71)

`examples/pr-upkeep`'s decision node (`human-answers-review`) parks on
`observe: {kind: github_pr_reply, pr: ...}` — the SAME park/observe/
auto-submit shape `github_pr_merged` uses, generalized to a PR THREAD
rather than only a PR's merge state, with three possible auto-submitted
outcomes instead of one.

**Which reply counts.** GitHub's own `since` query parameter on
`GET /repos/{repo}/issues/{pr}/comments` already scopes every fetch to
comments posted strictly after the task's OWN `created_at` (the moment the
question was parked) — a comment from before the question was asked can
never qualify, full stop. The only remaining filter is authorship: a
comment counts when its author is not one of the flow's own automated
identities (`DEFAULT_REPLY_IGNORED_LOGINS`, extended via
`HUMAN_INBOX_TRACKER_REPLY_IGNORED_LOGINS`). No content marker (no
"approve:" prefix or similar) is required — the question was JUST posted
on this specific PR, so the next human comment on the thread IS the answer
in context. This is a deliberate choice over a marker convention: a marker
makes a person's ordinary reply invisible to the tracker unless they
remember the exact incantation, whereas freshness + authorship already
rules out the "resumes on an unrelated thanks" failure mode the reply
observable has to avoid — an unrelated aside would need a non-bot author,
posted strictly after the question, on this exact PR, which in practice
only the person actually answering does.

**Terminal states release the wait.** Before checking for a reply, the
tracker checks the PR's own state (the SAME `GET /repos/{repo}/pulls/{pr}`
call `github_pr_merged` uses): `merged: true` submits the merged outcome
immediately (the strongest possible answer — the human merged instead of
replying) and `state: closed` (unmerged) submits the dropped outcome
(the run must not wait forever on a dead PR). Both are ONE-request checks
that short-circuit before ever calling the comments endpoint.

**Rate-budget arithmetic.** `github_pr_reply` shares ONE GitHub request
budget with `github_pr_merged` — `github_request_budget` is not raised and
`poll_seconds` is not shortened for this kind. Anonymous-lane worst case
(no `GITHUB_TOKEN`, `HUMAN_INBOX_TRACKER_RATE_UTILIZATION` at its default
0.5): `TrackerConfig.__post_init__`'s clamp yields `poll_seconds=120`,
`github_request_budget=1` — one GitHub request every 120 seconds, 30
requests/hour against the 60/hour anonymous ceiling, and that arithmetic
does not change no matter how many `(kind, repo, pr)` groups — merge OR
reply — are pending; a new kind only adds entrants to the SAME
round-robin queue sharing that one request per cycle. What DOES change is
detection latency: a reply-kind group's full check (terminal-state GET,
then a comments GET when the PR is still open) costs up to TWO of those
per-cycle budget units versus a merge-kind group's one, so at budget=1 an
open reply-kind group needs at least two cycles (up to ~240s) to complete
one full pass. `MergeTracker._check_reply_group` degrades by SKIPPING the
comments call rather than partially spending past the budget when only one
unit remains — the group just waits for the next cycle. Reply-kind groups
are checked BEFORE merge-kind groups each cycle (a human is actively
blocked on a reply-kind group, whereas a merge-kind group's human can act
at their own pace) — this reprioritises the same fixed budget, it does not
grow it. `tests/test_tracker.py`'s
`test_reply_groups_are_checked_before_merge_groups_share_the_same_budget`
pins this ordering, and the `test_reply_group_does_not_spend_a_second_
request_when_budget_is_one` /
`test_qualifying_reply_completes_with_answered_outcome` pair pin the
1-vs-2-request costs.

An automatic submit for this kind carries a matching `observed` marker —
`{"collection_method": "github_pr_reply", "reference": "<comment URL>"}`
for a reply, `{"collection_method": "github_pr_merged", "merge_commit":
"<sha>"}` for a merge (reusing the SAME collection method and field
`github_pr_merged` already uses), or `{"collection_method":
"github_pr_closed", "reference": "<PR URL>"}` for an unmerged close. The
server's `mapping.py` validates each collection method against its own
required-field set (`merge_commit` for `github_pr_merged`, `reference` for
`github_pr_reply`/`github_pr_closed`) — adding a new collection method is
a one-line addition to that map, not a bespoke validation branch.

## Nudge transport

When a human task sits in the inbox for too long, the tracker can **nudge**
the person through a Discord channel instead of relying on the manual inbox
surface.  Nudging is **opt-in**: it requires all four `DISCORD_NUDGE_*`
environment variables to be set; without them the tracker behaves exactly as
before.

### How it works

The tracker runs a nudge cycle after every merge-observation cycle.  For
each pending task that has no nudge state yet, it calls
`nudge.first_nudge()` which creates a **thread per task** in the configured
Discord channel.  Subsequent cycles check the cadence:

* **First nudge** — creates a new thread with the task instruction and
  persists `thread_id`, `last_nudge_at`, `last_seen_message_id`, and
  `escalation_level` in the task's `nudge_state`.
* **Cadence nudge** — when `nudge_interval_seconds` has elapsed since the
  last nudge, a follow-up message is posted in the existing thread.
* **Escalation** — when `nudge_escalation_after_seconds` has elapsed since
  the first nudge, the tracker escalates (levels 0 → 1 → 2), posting a
  higher-priority message in the same thread.
* **Reply polling** — after nudging, the tracker polls for new replies in
  each thread.  A reply is relayed through the bridge's submit surface,
  completing the task.

The nudge cycle is **idempotent**: it never sends duplicate nudges for the
same thread, and it never blocks the main merge observation path.  If the
nudge module is unavailable (import error), the tracker silently skips
nudging.

### Thread-per-task model

Each pending task gets its own Discord thread.  The thread is identified by
`thread_id` stored in the task's `nudge_state` dict (persisted in the
task's JSON file).  This means:

* A person can reply in the thread and the tracker picks up the reply.
* Multiple tasks never share a thread — no cross-talk.
* A restart resumes from the persisted `thread_id`; no new threads are
  created for tasks that already have one.

### Configuration

| Env var | Default | Meaning |
|---|---|---|
| `DISCORD_NUDGE_CHANNEL_ID` | unset | Discord channel to post nudges in (required for nudging) |
| `DISCORD_NUDGE_BOT_TOKEN` | unset | Discord bot token (required for nudging) |
| `DISCORD_NUDGE_INTERVAL_SECONDS` | `300` | Seconds between cadence nudges on the same thread |
| `DISCORD_NUDGE_GLOBAL_THROTTLE_SECONDS` | `10` | Minimum wall-clock gap between any two nudge sends |
| `DISCORD_NUDGE_ESCALATION_AFTER_SECONDS` | `600` | Seconds after first nudge before escalation kicks in |

All five must be present for nudging to be enabled.  The channel ID and bot
token are the gate: if either is empty, the tracker treats nudging as
disabled and skips the nudge cycle entirely.

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
