# culture-nodes

**Culture Nodes** is a durable, ledger-native workflow orchestrator for
agents, code, services, and people. Workflows are immutable graphs; agents
are triggered on their own machines through a provider-neutral protocol;
code runs through an external runner boundary; and every result lands in an
append-only work ledger where an agent's "done" is a claim, not a fact.

> **Every node has a contract. Every result has evidence.**

**Drive it from Jira** — the loop with no shell in it: move a ticket and
culture-nodes picks it up, works it, asks its questions on the ticket, and
comes back for the decisions only a person can make.
[`docs/drive-from-jira.md`](docs/drive-from-jira.md) is the whole thing
written for a reader who never opens a terminal — which trigger starts which
flow, what each system comment means, and how to answer one.

## See it running

![The Run view: the delivery-loop workflow as a live graph, its first node ready](docs/assets/run-view-light.png)

A run is a live graph: solid edges have been walked, dashed edges are still
possibilities — and the loops from `test.failed` and `verify.changes_required`
back to `build` are domain outcomes on the graph, never engine failures.

![The same run in dark mode, following the OS color scheme](docs/assets/run-view-dark.png)

Dark mode follows your OS, with the same design tokens the
the AgentCulture site ships — nothing here invents
a sibling aesthetic.

![The node detail panel: contract digest, owner, attempts, ledger delta](docs/assets/node-detail.png)

Click any node (or press Enter on it — the whole canvas is keyboard-operable)
to see what it really is: its pinned contract digest, its owner, every
attempt, and the ledger records it appended.

![The Ledger view: records with authority chips, projections picker](docs/assets/ledger-view.png)

The work ledger is the run's truth: every record carries its authority —
`proposed` renders dashed, `confirmed`/`observed`/`derived` render solid —
so an unverified completion claim *looks* unverified.

![The runs list: one line per run — state, workflow digest, created](docs/assets/runs-list.png)

Everything the UI shows comes from the same `/v1alpha1` API the CLI uses, so
anything you can see here you can script there.

<!--
  placeholder: docs/assets/board.png — runs board, cards on state columns
  (queued/running/waiting/completed/failed) — landing this cycle (task t14)
-->
<!--
  placeholder: docs/assets/jobs-timeline.png — cross-run jobs timeline with
  the updated_since/updated_until time-range filter — landing this cycle
  (task t15)
-->

A **runs board** (cards on state columns) and a **jobs timeline**
(cross-run node-run history with a time-range filter) are landing later
this cycle — screenshots go here once they ship.

## Quickstart

The complete local system in one command (API + scheduler + worker +
PostgreSQL + MinIO, UI embedded):

```bash
cd deploy/compose
cp .env.example .env       # dev-only defaults; required — no password ships in the compose file
docker compose up --build  # UI + API on http://localhost:8080
```

Publish the reference workflow and start a run from the Python CLI front:

```bash
export NODES_API_URL=http://localhost:8080
uv run nodes workflow publish examples/delivery-loop/workflow.yaml
uv run nodes run create --workflow <digest> --input examples/delivery-loop/input.json
uv run nodes run events <id>   # follow the live event stream
```

That's the whole product path — no AgentCulture-mesh dependency anywhere in
it. `nodes` (the Python CLI) has zero third-party dependencies and is a pure
REST client; the compose stack above is Postgres, MinIO, and the Go control
plane. Agent and code nodes execute through two open, provider-neutral
contracts, and you are not limited to the reference implementations this
repo ships:

- **Actor protocol** ([`api/actor-protocol`](api/actor-protocol/README.md)) —
  how an `agent` node's attempt reaches an external process and how that
  process reports back. Three conformant reference bridges exist —
  [`adapters/colleague`](adapters/colleague/README.md),
  [`adapters/claude-code`](adapters/claude-code/README.md),
  [`adapters/codex`](adapters/codex/README.md) — but anything that passes
  the runnable conformance kit (`tests/conformance`) is just as valid a
  fourth, in any language, over any agent backend.
- **Runner protocol** ([`api/runner-protocol`](api/runner-protocol/README.md)) —
  how a `code` node's operation reaches a runner service.
  [`cmd/nodes-runner`](cmd/nodes-runner) is the reference implementation
  (headspace-cli behind the contract); anything that passes
  `tests/runnerconformance` is just as valid a replacement.

Registering an actor or a runner is a row in a table today (no HTTP
registration endpoint yet — PRD §26 open question); see
[`deploy/compose/README.md`](deploy/compose/README.md#the-code-runner-boundary)
for the worked local example.

For Kubernetes, the Helm chart deploys the same system with a migration Job,
probes, and worker `replicas: 2` by default (multi-pod safety — leases and
fencing — is built in):

```bash
helm install nodes deploy/helm/culture-nodes
```

> Phase 1 runs **authless behind a private network** — deploy only on a
> private cluster/VPC. See the chart's NOTES and
> [`docs/guide.md`](docs/guide.md) for the full tour, including dev mode and
> the external-agent story.

## What's in the box

- **Go control plane** (`cmd/nodes`, one binary): compiler + `nodes validate`,
  a durable engine (fenced claiming, bounded loops, restart survival), the
  work ledger (agents propose; humans confirm; runners observe; validators
  derive), an approval surface for human-in-the-loop nodes (below),
  transactional outbox, Postgres/SQS queue drivers, scheduler, worker.
- **Actor protocol** ([`api/actor-protocol`](api/actor-protocol/README.md))
  for external agents — provider-neutral HTTP/JSON, a runnable conformance
  kit (`tests/conformance`), and three reference bridges:
  [`adapters/colleague`](adapters/colleague/README.md),
  [`adapters/claude-code`](adapters/claude-code/README.md),
  [`adapters/codex`](adapters/codex/README.md).
- **Runner protocol** ([`api/runner-protocol`](api/runner-protocol/README.md))
  for code nodes: placement-unaware (the same workflow digest runs against a
  runner anywhere by changing one registry entry), polling-authoritative,
  callbacks optional. [`cmd/nodes-runner`](cmd/nodes-runner) is the
  reference runner service (headspace-cli behind the contract, mandatory
  bearer auth); the AWS Lambda adapter (`internal/runners/lambda`,
  registry-pinned, IAM-scoped, honest evidence) is the cloud-native
  alternative. No Docker socket, and no execution of any code, ever enters a
  control-plane container.
- **Web front** (`web/`): the read-only Runs list, Run view, and Ledger view
  above, embedded into the Go binary. A runs board and a cross-run jobs
  timeline are landing later this cycle.
- **Python CLI front** (`nodes` on PyPI): thin, zero-dependency client of the
  same API.

The full design lives in
[`docs/initial-design/culture-nodes-prd-spec.md`](docs/initial-design/culture-nodes-prd-spec.md);
what was built, with evidence, in
[`docs/acceptance.md`](docs/acceptance.md) and
[`docs/deliveries/`](docs/deliveries/).

## Approvals: a human in the loop

An `approval` node pauses a run without ever creating a work item: the
engine writes one `human_tasks` row (decision schema, approver role/group,
deadline, context/artifact refs, allowed outcomes — PRD §9.9) inside the
same transaction that creates the node run, and the run holds no worker
lease and no open database transaction while it waits
(`internal/engine/humantask.go`, `internal/worker/doc.go`). A human answers
through the API, never through the worker:

```bash
curl http://localhost:8080/v1alpha1/human-tasks              # pending + decided, this namespace
curl http://localhost:8080/v1alpha1/human-tasks/<id>          # one task's context

curl -X POST http://localhost:8080/v1alpha1/human-tasks/<id>/decision \
  -H "Authorization: Bearer $NODES_HUMAN_DECISION_TOKEN_SECRET" \
  -H 'content-type: application/json' \
  -d '{"outcome": "approved", "decider_actor_id": "ori", "expected_ledger_version": 4}'
```

The decision endpoint is one of three writes in this API that require a
bearer token (`NODES_HUMAN_DECISION_TOKEN_SECRET` on `nodes serve`; the
other two are the review routes below) — every other Phase-1 endpoint is
authless behind the private network above, but a decision here writes a
human-authority review into the ledger and resumes the run on whoever's
behalf the token vouches for. The commit is atomic and stale-guarded: a
decision against a ledger version the run has since moved past is refused,
never silently applied. The shipped
[`examples/delivery-loop`](examples/delivery-loop) reference workflow does
not include an approval node yet (see its header comment); the engine, API,
and worker plumbing above are real and tested independently of that
fixture.

## Deciding a claim: the affirmative half of the authority model

An agent may only create `proposed` records, and no actor promotes its own
proposal (PRD §10.4). That refusal is one half of the model; the other half
is somebody actually deciding. A decision is recorded as its own immutable
`review` record — human origin naming the reviewer, `confirmed` or
`rejected` authority, `subject_ref` naming the record decided, and the
stated reason in its payload.

```bash
# What is awaiting a decision, across every run (or one, with ?run_id=).
curl http://localhost:8080/v1alpha1/pending-decisions

# Open a review over the records, at the ledger version you read them at.
curl -X POST http://localhost:8080/v1alpha1/runs/<run-id>/reviews \
  -H "Authorization: Bearer $NODES_HUMAN_DECISION_TOKEN_SECRET" \
  -H 'content-type: application/json' \
  -d '{"record_ids": ["rec_..."], "ledger_version": 7, "reviewer_actor_id": "actor_..."}'

# Decide it. `rationale` is required, and it is recorded on each decision.
curl -X POST http://localhost:8080/v1alpha1/reviews/<review-id>/commit \
  -H "Authorization: Bearer $NODES_HUMAN_DECISION_TOKEN_SECRET" \
  -H 'content-type: application/json' \
  -d '{"decisions": {"rec_...": "confirm"}, "expected_ledger_version": 7,
       "rationale": "re-ran the suite on spark and read the output"}'
```

Three things this surface refuses to be casual about:

- **Who decided is checked, not asserted.** `reviewer_actor_id` is resolved
  against the actor registry and must be registered `human`. Without that
  check the human origin stamped on a review record would be a value the
  ledger asserts on the caller's behalf, and an agent could decide its own
  claim by naming itself (rule `reviewer_must_be_human`).
- **Why is required.** A confirmation with no stated reason cannot be told
  apart from an unread one.
- **Nothing is rewritten.** Records are immutable, so a confirmed claim
  still reads `authority: proposed` forever with a review record pointing at
  it. That is why "what is still undecided" is `GET /pending-decisions` — a
  join — and not a filter on authority.

The same thing from a browser: the **Decisions** view (`/decisions`) lists
every undecided record with its payload in full and records the verdict,
reviewer and rationale. `scripts/decide-claims.py` is the terminal version,
and `scripts/ledger-gate.py` is the stage gate that fails while anything is
still undecided.

### Collecting a handover, and gating a merge on a real suite

A run whose session handed work over left it on a git ref named
`refs/culture-nodes/<run-id>/<node-run-id>-<attempt-id>-<UTC>-<short-sha>`,
on whatever machine that session ran on. `scripts/collect-handover.py` turns
the **run id alone** into a reviewable diff:

```bash
scripts/collect-handover.py <run-id>            # fetch and show what changed
scripts/collect-handover.py <run-id> --json
```

It asks the control plane which actor ran the run, resolves that actor's host
from the registry, fetches the run's refs by wildcard into `refs/handover/`
(no branch is touched, nothing is checked out), and reports each ref, its
commit, and the paths it changed. The remote is **the control plane's
configuration, never the run's own report** — a session that could point the
fetch at a repository it prepared would make the measurement real and the
subject forged — and only `refs/culture-nodes/` is ever fetched. Configure it
per actor with `metadata.handover_remote` at registration, or fleet-wide with
`NODES_HANDOVER_REMOTE_TEMPLATE` (`{host}` is substituted from the actor's
registered `endpoint_ref`). Neither present is a refusal, not a guess.

**No ref is an ambiguous state, and is reported as one.** Either the session
handed over nothing, or the bridge on that host cannot hand over at all
(issue #120: bridges deployed before the ref-minting code create no ref even
on success). The script exits non-zero and names both, because guessing here
is how a lost handover becomes "the agent did nothing".

`--gate` then runs a suite against the collected commit and records what
happened:

```bash
scripts/collect-handover.py <run-id> --gate --suite 'go test ./...' -- go test ./...
```

The suite runs in a detached worktree at that exact commit, and the result is
appended through `POST /v1alpha1/runs/{id}/suite-verdicts` as a **`derived`**
record from the named validator — because a test suite *is* a deterministic
validator (PRD §10.4), where an operator reading a green tick is not evidence
of anything. The record names the suite, the exit code, and the commit sha,
and the sha is read back from the worktree the suite actually ran in rather
than assumed. A verdict that does not name what it tested is not evidence, so
`commit_sha` must be a full 40-hex id, an absent `exit_code` is refused rather
than defaulted to a pass, and a verdict naming a commit other than the one the
control plane measured as this run's handover is refused outright.

## Example topology: one machine, or a small production split

The runner protocol's placement-unaware model above is what makes this
possible with zero workflow changes: local dev runs everything — Postgres,
API, scheduler, worker, and a runner — on one machine. A small production
split looks like the shared control plane and its Postgres on one machine,
and a second machine running just a worker (and its own runner service)
pointed at the first machine's database; nothing about the workflow
definition or its compiled contract differs, only registry/deployment
config does (the runner-protocol doc's own worked example uses exactly this
shape — an endpoint named `runner.thor.internal`).

This repo's own development follows that shape as a concrete example —
`spark` for local dev, a `thor` + `orin` pair sharing one Postgres on `thor`
for production — but the machine names are illustrative, not a product
requirement: any names, any machine count, any cloud or bare metal work the
same way. Per-machine compose profiles for that split are landing later
this cycle.

## CLI

The Python front's product verbs (`workflow`, `run`, `ledger`, `review`) are
thin API clients; the identity verbs below work offline:

| Verb | What it does |
|------|--------------|
| `nodes whoami` | Report this agent's nick, version, backend, and model from `culture.yaml`. |
| `nodes learn` | Print a structured self-teaching prompt. |
| `nodes explain <path>` | Markdown docs for any noun/verb path. |
| `nodes overview` | Read-only descriptive snapshot. |
| `nodes doctor` | Identity invariants + API reachability. |
| `nodes cli overview` | Describe the CLI surface itself. |

Every command supports `--json`. Results go to stdout, errors/diagnostics to
stderr (never mixed). Exit codes: `0` success, `1` user error, `2` environment
error, `3+` reserved. The Go binary carries the same contract for its
`serve` / `scheduler` / `worker` / `all` / `migrate` / `validate` modes.

## Mesh identity

Separately from the product path above (which has no mesh dependency), this
*repository* is also a Culture mesh agent — it develops itself using the
same AgentCulture tooling its maintainers use elsewhere: `culture.yaml`
(`suffix: culture-nodes`, `backend: colleague`) with the resident prompt file
`AGENTS.colleague.md`, and the vendored guildmaster/devague skill kit under
`.claude/skills/` (cite-don't-import — see
[`docs/skill-sources.md`](docs/skill-sources.md)). Running Culture Nodes
yourself needs none of this.

## Contributing

See [`CLAUDE.md`](CLAUDE.md) for the working conventions: the design ground
rules distilled from the PRD, the version-bump-every-PR rule, the `cicd` PR
lane, and the vendored-skills policy.

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
