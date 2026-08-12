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

- `culture-nodes workflow validate|publish|list|get`
- `culture-nodes run create|list|get|cancel|events|retag|grade`
- `culture-nodes node-runs list`
- `culture-nodes ledger records|projection`
- `culture-nodes review create|commit`
- `culture-nodes human-tasks list|get|decide`

## API configuration

Every product verb resolves the API base URL as: `--api-url` flag, then the
`NODES_API_URL` environment variable, then `http://127.0.0.1:8080`.
`human-tasks decide` additionally resolves a bearer token: `--token`, then
the `NODES_HUMAN_DECISION_TOKEN` environment variable — never logged.

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
- `culture-nodes explain ledger`
- `culture-nodes explain review`
- `culture-nodes explain human-tasks`
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
`workflows` tag): validate, publish, list, get. No engine logic lives here —
every verb sends one HTTP request to the Culture Nodes control-plane API
(the Go `nodes serve` binary) and renders the response.

## Usage

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
}
