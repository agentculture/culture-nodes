"""Markdown catalog for ``culture-nodes explain <path>``.

Each entry is verbatim markdown. Keys are command-path tuples. The empty tuple
and ``("culture-nodes",)`` both resolve to the root entry.

Keep bodies self-contained: an agent reading one entry should get enough
context without chaining reads.
"""

from __future__ import annotations

_ROOT = """\
# culture-nodes

The Python side of Culture Nodes: a mesh-agent identity (`culture.yaml` +
`AGENTS.colleague.md`), the agent-first CLI contract (cited from the teken
`python-cli` reference), and a thin REST client over the Culture Nodes
control-plane API (`api/openapi/openapi.yaml`). No workflow-engine logic
lives in this package (spec decision c28) — every product verb sends one
HTTP request to the Go control-plane binary (`nodes serve`) and renders the
response.

## Identity verbs

- `culture-nodes whoami` — identity probe from `culture.yaml`.
- `culture-nodes learn` — structured self-teaching prompt.
- `culture-nodes explain <path>` — markdown docs for any noun/verb.
- `culture-nodes overview` — descriptive snapshot of the agent.
- `culture-nodes doctor` — check the agent-identity and API-reachability invariants.
- `culture-nodes cli overview` — describe the CLI surface.

## Product verbs (thin API clients)

- `culture-nodes workflow generate|generation-get|validate|publish|list|get`
- `culture-nodes run create|list|get|cancel|events|retag|grade`
- `culture-nodes node-runs list`
- `culture-nodes actors list|get|resume|dial-in`
- `culture-nodes ledger records|projection`
- `culture-nodes review create|commit`
- `culture-nodes human-tasks list|get|decide`
- `culture-nodes dispatch pending|show|confirm`

## API configuration

Every product verb resolves the API base URL as: `--api-url` flag, then the
`NODES_API_URL` environment variable, then `http://127.0.0.1:8080`.
`human-tasks decide` additionally resolves a bearer token: `--token`, then
the `NODES_HUMAN_DECISION_TOKEN` environment variable — never logged.
`actors resume` does the same against `NODES_ACTOR_REGISTRATION_TOKEN`.

## Exit-code policy

- `0` success
- `1` user-input error (also: `workflow validate` reporting an invalid document)
- `2` environment / setup error (also: the API is unreachable)
- `3+` reserved

## See also

- `culture-nodes explain whoami`
- `culture-nodes explain doctor`
- `culture-nodes explain workflow`
- `culture-nodes explain run`
- `culture-nodes explain node-runs`
- `culture-nodes explain actors`
- `culture-nodes explain ledger`
- `culture-nodes explain review`
- `culture-nodes explain human-tasks`
- `culture-nodes explain dispatch`
"""

_WHOAMI = """\
# culture-nodes whoami

Reports the agent's identity from `culture.yaml`: nick (`suffix`), backend,
served model, and the package version. Read-only.

## Usage

    culture-nodes whoami
    culture-nodes whoami --json
"""

_LEARN = """\
# culture-nodes learn

Prints a structured self-teaching prompt covering purpose, command map,
exit-code policy, `--json` support, and the `explain` pointer.

## Usage

    culture-nodes learn
    culture-nodes learn --json
"""

_EXPLAIN = """\
# culture-nodes explain <path>

Prints markdown documentation for any noun/verb path. Unlike `--help` (terse,
positional), `explain` is global and addressable by path.

## Usage

    culture-nodes explain culture-nodes
    culture-nodes explain whoami
    culture-nodes explain --json <path>
"""

_OVERVIEW = """\
# culture-nodes overview

Read-only descriptive snapshot of the agent: identity (from `culture.yaml`), the
verb surface, and the artifacts this repo carries (mesh identity files, the
vendored skill kit, the API this package fronts). Accepts an ignored `target`
so a stray path never hard-fails.

## Usage

    culture-nodes overview
    culture-nodes overview --json
"""

_DOCTOR = """\
# culture-nodes doctor

Checks the agent-identity invariants `steward doctor` verifies:
prompt-file-present and backend-consistency (`colleague` → `AGENTS.colleague.md`), a
skills-present check, and a `nodes_api_reachable` check (`GET /v1alpha1/healthz`
against the resolved API URL). Only `error`-severity checks (prompt-file-present,
backend-consistency) can flip the exit code to 1 — `nodes_api_reachable` and
`skills-present` are `warning`/`info` and never fail `doctor` on their own,
since the identity verbs work with no API running at all.

## Usage

    culture-nodes doctor
    culture-nodes doctor --json
    culture-nodes doctor --api-url http://localhost:9090
"""

_CLI = """\
# culture-nodes cli

Noun group for CLI-surface introspection. `cli overview` describes the CLI
itself (distinct from the global `overview`, which describes the agent).

## Usage

    culture-nodes cli overview
    culture-nodes cli overview --json
"""

_WORKFLOW = """\
# culture-nodes workflow

Thin REST client over the workflows API (`api/openapi/openapi.yaml`,
`workflows` tag): generate, generation-get, validate, publish, list, get.
No engine logic lives here — every verb sends one HTTP request to the
Culture Nodes control-plane API (the Go `nodes serve` binary) and renders
the response.

## Usage

    culture-nodes workflow generate "DESCRIPTION" --actor-ref REF [--base-digest DIGEST]
    culture-nodes workflow generation-get <run-id>
    culture-nodes workflow validate <file.yaml|file.json>
    culture-nodes workflow publish <file.yaml|file.json>
    culture-nodes workflow list [--workflow-key KEY] [--limit N]
    culture-nodes workflow get <digest>

Every subcommand accepts `--json` (byte-exact passthrough of the API's JSON
response) and `--api-url` (default: `$NODES_API_URL`, else
`http://127.0.0.1:8080`).

## validate

Compiles the file server-side and reports every diagnostic. A document with
error diagnostics is a domain outcome (`valid: false`), not a technical
failure: diagnostics print to stdout with exit `1`, never an `error:`/
`hint:` stderr message.

## generate

Dispatches the one server-side generation workflow to a registered fleet
agent. The result stays `proposed` until its native approval node receives a
human decision and is never published by this verb. `generation-get` returns
the compiler diagnostics and, for an edit, the diff against `--base-digest`.

## publish

Compiles and stores the document as an immutable version addressed by its
content digest. Publishing identical content twice is idempotent (HTTP 200
with the existing version) rather than an error.
"""

_RUN = """\
# culture-nodes run

Thin REST client over the runs API (`api/openapi/openapi.yaml`,
`runs`/`events`/`grades` tags): create, list, get, cancel, events, retag,
grade. No engine logic lives here — every verb is one HTTP call to the
Culture Nodes control-plane API.

## Usage

    culture-nodes run create --workflow <digest> [--input <file>|--input -] \\
        [--name TEXT] [--description TEXT] [--category TEXT]
    culture-nodes run list [--state STATE] [--updated-since RFC3339] \\
        [--updated-until RFC3339] [--sort created_at|updated_at] [--limit N]
    culture-nodes run get <id>
    culture-nodes run cancel <id>
    culture-nodes run events <id> [--follow]
    culture-nodes run retag <id> --category TEXT
    culture-nodes run grade <id> --rating N --notes TEXT --actor EVALUATED_ID --as GRADING_ID \\
        [--node-run-ref REF] [--attempt-ref REF] [--category TEXT]

## create

`--name`/`--description`/`--category` are all optional (task t3). `--name`
and `--description` are set once, at creation, and immutable afterward —
there is no verb to change them. `--category` is the one field retaggable
later, via `run retag`.

## retag

The only field PATCH /v1alpha1/runs/{id} accepts is `category` (frame
decision q4) — `name`/`description` are immutable once a run is created.
Pass `--category ""` to clear a run's category.

## grade

Records an opinion — a 1-5 `--rating` plus free-text `--notes` (the
record's `rationale`) — evaluating `--actor` (`evaluated_actor_id`) on this
run, recorded as `--as` (`grading_actor_id`, issue #28 item 1). The API
looks up `--as`'s registered actor kind and decides origin/authority from
it: a human actor's grade lands `confirmed` immediately (it is the human's
own opinion, not a claim someone else must ratify); an agent actor's grade
lands `proposed`, exactly like any other agent-origin record, and reaches
`confirmed` only by later going through `nodes review create`/`commit`
against it. No actor may grade its own work — `--actor` equal to `--as` is
refused (HTTP 400, exit `1`) naming the ledger rule. A `--rating` outside
1-5 is refused by argparse itself before any request is sent.
`--node-run-ref`/`--attempt-ref` narrow the grade to one node run or
attempt; `--category` is an optional flat tag carried onto the grade record
itself (documentary — per-actor stats slice by the graded run's own
category, not this field).

## Rendering names, hints, and usage (text mode)

`create`, `get`, and `retag` render a run's display name when one was given
at creation. When no `name` was given, the API may supply `display_hint` — a
truncated, best-effort GUESS derived at read time from the run's own input,
never something an operator actually said — rendered as
`name: <hint> (derived)` so it is never mistaken for a real name. `list`
renders the same name-or-derived-hint per row.

`create`, `get`, and `retag` also render the run's §13.2 usage/cost rollup
(task t2) whenever the API includes a `usage` object: token totals and
attempt-reporting counts (`usage.attempts_reported`/
`usage.attempts_not_reported`) always render — they are required fields,
genuinely zero when no attempt reported anything, not fabricated;
`usage.cost`/`usage.cost_by_currency` render only when at least one attempt
in scope actually reported a cost. `list` does NOT include usage (the API
does not compute a per-row rollup for list responses); `node-runs list`
does (see `culture-nodes explain node-runs`).

## list

Newest first by `--sort` (default `created_at`). `--updated-since` /
`--updated-until` filter on `updated_at`; when either is set and `--sort` is
omitted, the API defaults `sort` to `updated_at` instead of `created_at`.

## events

Streams the run's committed events over Server-Sent Events. The API
(`internal/api/events.go`) closes the connection once a terminal run event
(`run.completed`, `run.failed`, `run.cancelled`, `run.bounded`) has been
sent, so `events` always follows until the stream ends — `--follow` is
accepted for symmetry with `tail -f` but is a no-op today. `--json` prints
one JSON object per event (`{id, event, data}`), one per line; text mode
prints one compact line per event.
"""

_LEDGER = """\
# culture-nodes ledger

Thin REST client over the ledger read API (`api/openapi/openapi.yaml`,
`ledger` tag): records, projection. No engine or projection logic lives
here — the PRD §10.9 standard projections are computed server-side.

## Usage

    culture-nodes ledger records <run-id>
    culture-nodes ledger projection <run-id> <name> [--subject SUBJECT]

`<name>` is one of: `current_scope`, `confirmed_claims`,
`open_assumptions_and_questions`, `ready_tasks`, `active_tasks`,
`verification_queue`, `decision_history`, `evidence_for_subject`,
`delivery_summary`. `--subject` is required by, and used only by,
`evidence_for_subject`.
"""

_NODE_RUNS = """\
# culture-nodes node-runs

Thin REST client over the cross-run node-runs listing
(`api/openapi/openapi.yaml`, `node-runs` tag) — the "jobs timeline" (task
t11): every node run in the namespace, newest first by `updated_at`, unlike
the `node_runs` nested under one run's Run-view payload
(`culture-nodes run get`). No engine logic lives here.

## Usage

    culture-nodes node-runs list [--updated-since RFC3339] [--updated-until RFC3339] \\
        [--cursor CURSOR] [--limit N]

Pagination is a keyset cursor, not an offset: pass a response's
`next_cursor` back as `--cursor` to fetch the next page. Its absence in the
response means there is no further page.

## Usage rendering (text mode)

Every row always carries a §13.2 usage/cost rollup (task t2) — even a
present-but-empty one for a node run with zero attempts — rendered indented
beneath the row: token totals and attempt-reporting counts always render
(required fields, honest zeros); `cost`/`cost_by_currency` render only when
at least one attempt in scope reported a cost. See
`culture-nodes explain run` for the same rendering on run-level usage.
"""

_ACTORS = """\
# culture-nodes actors

Thin REST client over the actors API (`api/openapi/openapi.yaml`, `actors`
tag): list, get, resume. No engine logic lives here — the capacity circuit
breaker's state is computed and cleared server-side.

## Usage

    culture-nodes actors list [--paused-only]
    culture-nodes actors get <actor-id>
    culture-nodes actors resume <actor-id> [--cleared-by WHO] [--token TOKEN]

`list` renders every registered actor row — every revision, because actor
identity is append-only (a capability or endpoint change is a new row, never
an update).

## The capacity circuit breaker

When a dispatch to an actor fails with the `capacity_exhausted` §13.5 error
class — a provider quota, per-session limit, or rate-window exhaustion the
bridge declared in its own error body — the worker marks that ACTOR
unavailable until a deadline and DEFERS work addressed to it: the work item
goes back to `ready` with its availability pushed forward, never failed. One
provider limit must not become a cascade of failed billable sessions.

That state renders as the `availability` block on `get` and `list`:

- `availability.paused` — whether the pause is in force right now.
- `availability.paused_until` — when dispatch resumes on its own.
- `availability.reason` — the §13.5 class that tripped it.
- `availability.retry_after_seconds` — the provider's own Retry-After, when
  it named one. Absent means it named none; it is never rendered as `0`.
- `availability.tripped_by_run_id` / `tripped_by_attempt_id` — which
  dispatch discovered the exhaustion.
- `availability.cleared_at` / `cleared_by` — present only when an operator
  ended the pause early, so an expiry and a human clear stay
  distinguishable.

An actor that has never been paused carries NO availability block at all —
distinct from one whose pause lapsed, which renders with `paused: no`.

## resume

`resume` ends a pause early, for the operator who knows better than the
automatic classification (the quota reset, the bridge was misreporting, the
limit did not apply to this project). The pause is keyed by `actor_key`, so
resuming any revision resumes the identity.

Unlike `list` and `get` — and like `human-tasks decide` — `resume` requires
a bearer token: the `NODES_ACTOR_REGISTRATION_TOKEN` environment variable,
or `--token`. Neither is ever printed, logged, or included in `--json`
output. It is deliberately the SAME secret actor registration uses:
registration grants an endpoint the standing to be dispatched real work, and
clearing a pause restores exactly that standing.

Resuming an actor that is not paused succeeds (exit `0`) rather than
erroring: the intent — "this actor should be dispatchable" — is already
satisfied. Work items already deferred become claimable again within the
worker's deferral horizon (minutes), not instantaneously.

## dial-in — which bridges are connected right now

    culture-nodes actors dial-in [--absent-only]

Every bridge dials OUT to the control plane and the control plane records no
address for it (issue #121). That removes the decayed-address failure, but
not the QUESTION the address answered: "is this bridge reachable?" becomes
"is this bridge dialled in right now?", and `dial-in` is where that is
answered.

It is a READ, never a probe. There is no address to probe — presence is a
fact PostgreSQL already holds, written by the bridge's own poll of
`POST /v1alpha1/inbound/poll`. Nothing is dispatched, so asking costs no
billable session and cannot fail a run.

Each actor renders in one of three states, and the three are deliberately
not two:

- `connected` — polled within the window, printed in the header. That window
  is the SAME one dispatch resolution uses when it decides whether an actor
  is reachable through the durable mailbox, so this view can never say
  connected while a dispatch says otherwise.
- `DISCONNECTED` — this bridge HAS dialled in before and stopped. The
  last-seen instant and how long ago it was are printed with it, because
  *when* it dropped is the whole answer.
- `NEVER DIALLED` — no presence record at all. A configuration or deployment
  fact, not an outage, and it carries no last-seen instant rather than a
  fabricated one.

Where the credential explains the absence, the line says so — `REVOKED`,
`LOCKED OUT until …`, `credential not control-plane issued`, or `no
credential record`. A locked-out bridge is dialling and being refused, which
is the opposite situation from one that is not dialling at all, and the two
need different remedies. No verifier material is ever rendered.

`--absent-only` drops the connected rows, which is the shape of the question
an operator actually has. The summary counts stay, so `connected` against
the total is a one-line fleet check — the check the address-retirement
cutover makes before disabling the outbound fallback.
"""

_REVIEW = """\
# culture-nodes review

Thin REST client over the human review transactions API
(`api/openapi/openapi.yaml`, `reviews` tag): create, commit. No ledger
authority logic lives here — confirm/reject decisions are applied
server-side, all-or-nothing, under the PRD §10.8 review protocol.

## Usage

    culture-nodes review create <run-id> --records id1,id2 --ledger-version N
    culture-nodes review commit <review-id> --confirm id1 --reject id2 --ledger-version N

`commit` refuses (HTTP 409, exit `1`) when the ledger has moved since the
review was created, a target record was superseded, or the review was
already committed — the API's own remediation names the fix (re-read the
current ledger version and submit a new review request).
"""

_HUMAN_TASKS = """\
# culture-nodes human-tasks

Thin REST client over the human tasks API (`api/openapi/openapi.yaml`,
`human-tasks` tag): list, get, decide. No approval-node logic lives here —
outcome validation against `allowed_outcomes`, ledger-version guarding, and
edge routing all happen server-side (PRD §9.9).

## Usage

    culture-nodes human-tasks list [--status pending|decided] [--limit N]
    culture-nodes human-tasks get <id>
    culture-nodes human-tasks decide <id> --outcome OUTCOME \\
        --decider-actor-id ACTOR --expected-ledger-version N \\
        [--note TEXT] [--record-ids id1,id2] [--token TOKEN]

## decide

Unlike every other verb in this CLI, `decide` requires a bearer token: the
`NODES_HUMAN_DECISION_TOKEN` environment variable, or `--token`. Neither is
ever printed, logged, or included in `--json` output. A missing token is a
structured `error:`/`hint:` failure (exit `1`) naming the env var, never a
token value. The API itself refuses an unauthenticated or wrongly
authenticated decision with HTTP 401 before the body is even read; a second
decision on an already-decided task is refused with 409.

`--note` fills the decision's `response` payload as `{"note": TEXT}`.
`--record-ids` names other ledger records the decider is confirming
alongside this decision (ordinarily empty).
"""


_DISPATCH = """\
# culture-nodes dispatch

Thin REST client over the clarify-then-commit gate (`api/openapi/openapi.yaml`,
`preflights` tag): pending, show, confirm. The gate briefs a dispatched actor
BEFORE its first billable turn — the operating facts its task depends on, as
that actor's own bridge advertised them — and holds the dispatch until the
briefing is acknowledged (issue #67).

## Usage

    culture-nodes dispatch pending [--actor-key KEY] [--limit N]
    culture-nodes dispatch show <preflight-id>
    culture-nodes dispatch confirm <preflight-id> --actor-id ACTOR [--note TEXT]

## The three verbs are one sequence

`pending` answers "is anything waiting on me". `show` prints the briefing
itself — host capabilities, the task declaration, the expected terminal
shape, and what does not proceed until it is acknowledged. `confirm` is the
second, separate action that commits the dispatch, recording the actor's
*proposed* claim to have read it. A confirm without a show is a keystroke,
not an acknowledgement.

## What confirm records, and what it does not

The acknowledgement is a `proposed` ledger record naming the briefing by id
AND content digest. It is a claim that the actor was told, never evidence
that it understood — no actor promotes its own claim (PRD §10.4).

It is **single-use** (the next dispatch of that node needs its own briefing)
and **windowed** (default 15 minutes). Confirming an already-answered, spent
or expired briefing is refused with HTTP 409, and a dispatch whose window
closes unacknowledged is refused rather than sent — routing under the
`preflight_unacknowledged` outcome, which a workflow author may declare an
edge from.

## Gate configuration

The gate is per-actor and default-off. It is enabled on the actor's
registration (`metadata.preflight_gate`), and enabling it for an actor whose
registration advertises no `capabilities.preflight` surface is refused when
the actor is registered — not discovered later by a run that stalls.
"""


_JIRA_SERVICE_ACCOUNT = """\
# culture-nodes jira-token

The runbook for the Jira SERVICE ACCOUNT token (`culture-nodes`,
accountId `712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615`), kept as a verb so
the recovery path no longer lives in one operator's head (issue #273). The
long form is `docs/operations/jira-service-account.md`. The token is never
written to a plaintext file on spark: it lives hidden in `grant`, the
per-user secrets manager, as `JIRA_SERVICE_ACCOUNT_TOKEN`.

## Usage

    culture-nodes jira-token mint
    culture-nodes jira-token seal
    culture-nodes jira-token verify
    culture-nodes jira-token install

## mint — where the token comes from

A service-account token is minted ONLY in the Atlassian admin UI
(admin.atlassian.com -> Directory -> Service accounts -> culture-nodes ->
API tokens -> Create). No API mints one, so this CLI cannot either: `mint`
prints that path, the gateway base, the accountId to expect, and how the
token is sealed (`seal`) and consumed
(`grant run --inject JIRA_API_TOKEN=JIRA_SERVICE_ACCOUNT_TOKEN -- <cmd>`).
It reads nothing.

## seal — store the token hidden in grant

Reads the token once — `getpass` without echo on a TTY, one line of stdin
otherwise (`printf %s "$TOKEN" | culture-nodes jira-token seal`) — and
runs `grant set JIRA_SERVICE_ACCOUNT_TOKEN - --hidden` with the token on
stdin, never in an argv. A hidden grant secret can only be consumed through
`grant run --inject`; `grant get`/`grant env` refuse it and `grant show`
prints metadata only. An empty token is exit `1`; `grant` missing from
PATH, or a non-zero `grant set`, is exit `2` (grant's stderr is quoted
with the token scrubbed). Re-sealing overwrites, which is how rotation
works.

## verify — the one call that proves the pair

Reads `JIRA_API_TOKEN` from the environment — the `grant run --inject`
path — and calls `GET $JIRA_API_BASE/rest/api/3/myself` with Basic auth.
`JIRA_ACCOUNT_EMAIL` and `JIRA_API_BASE` are not secrets and default to
the service account and the gateway base. Without a token: a TTY is
prompted with `getpass`; a non-TTY is exit `2` with the `grant run` hint.
On 200 it prints `accountId: <id>` (`--json`: `account_id`, `email`,
`api_base`) and exits `0`. A 401/403 or a network failure is a structured
`error:`/`hint:` failure with exit `2`; a non-https base is exit `1`. The
token value is never printed, not even in an error.

The trap the hint names: a service-account token authenticates only at the
API gateway base `https://api.atlassian.com/ex/jira/<cloudId>`. The site
URL `https://agentculture.atlassian.net` answers 401 for it.

## install — the hand-turn sequence, printed not run

Prints the ordered operator steps that land a verified pair on thor and
orin, none of which sources a file: `seal` (once), `verify` under
`grant run --inject`, `install-secrets.sh` under the same wrapper with the
non-secret email and base exported (runner-secrets.env on both hosts), the
`pgrep` pre-check and `deploy.sh thor` (`deploy_jira` merges the base into
the bridge env and restarts jira-bridge; the pair in that file is a hand
edit on thor), then the `runner-env-write.sh` re-grant with
`jira_bot_account_id` on every repository entry and a runner restart on
each host. Rotation is: mint a new token, revoke the old one in the admin
UI, `seal` again (it overwrites), repeat steps 2-5.
"""


ENTRIES: dict[tuple[str, ...], str] = {
    (): _ROOT,
    ("culture-nodes",): _ROOT,
    ("nodes",): _ROOT,
    ("whoami",): _WHOAMI,
    ("learn",): _LEARN,
    ("explain",): _EXPLAIN,
    ("overview",): _OVERVIEW,
    ("doctor",): _DOCTOR,
    ("cli",): _CLI,
    ("cli", "overview"): _CLI,
    ("workflow",): _WORKFLOW,
    ("workflow", "generate"): _WORKFLOW,
    ("workflow", "generation-get"): _WORKFLOW,
    ("workflow", "validate"): _WORKFLOW,
    ("workflow", "publish"): _WORKFLOW,
    ("workflow", "list"): _WORKFLOW,
    ("workflow", "get"): _WORKFLOW,
    ("run",): _RUN,
    ("run", "create"): _RUN,
    ("run", "list"): _RUN,
    ("run", "get"): _RUN,
    ("run", "cancel"): _RUN,
    ("run", "events"): _RUN,
    ("run", "retag"): _RUN,
    ("run", "grade"): _RUN,
    ("node-runs",): _NODE_RUNS,
    ("node-runs", "list"): _NODE_RUNS,
    ("actors",): _ACTORS,
    ("actors", "list"): _ACTORS,
    ("actors", "get"): _ACTORS,
    ("actors", "resume"): _ACTORS,
    ("actors", "dial-in"): _ACTORS,
    ("ledger",): _LEDGER,
    ("ledger", "records"): _LEDGER,
    ("ledger", "projection"): _LEDGER,
    ("review",): _REVIEW,
    ("review", "create"): _REVIEW,
    ("review", "commit"): _REVIEW,
    ("human-tasks",): _HUMAN_TASKS,
    ("human-tasks", "list"): _HUMAN_TASKS,
    ("human-tasks", "get"): _HUMAN_TASKS,
    ("human-tasks", "decide"): _HUMAN_TASKS,
    ("dispatch",): _DISPATCH,
    ("dispatch", "pending"): _DISPATCH,
    ("dispatch", "show"): _DISPATCH,
    ("dispatch", "confirm"): _DISPATCH,
    ("jira-token",): _JIRA_SERVICE_ACCOUNT,
    ("jira-token", "mint"): _JIRA_SERVICE_ACCOUNT,
    ("jira-token", "seal"): _JIRA_SERVICE_ACCOUNT,
    ("jira-token", "verify"): _JIRA_SERVICE_ACCOUNT,
    ("jira-token", "install"): _JIRA_SERVICE_ACCOUNT,
}
