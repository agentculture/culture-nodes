# culture-nodes

**Culture Nodes** is a durable, ledger-native workflow orchestrator for
agents, code, services, and people. Workflows are immutable graphs; agents
are triggered on their own machines through a provider-neutral protocol;
code runs through an external runner boundary; and every result lands in an
append-only work ledger where an agent's "done" is a claim, not a fact.

> **Every node has a contract. Every result has evidence.**

## See it running

![The Run view: the delivery-loop workflow as a live graph, its first node ready](docs/assets/run-view-light.png)

A run is a live graph: solid edges have been walked, dashed edges are still
possibilities — and the loops from `test.failed` and `verify.changes_required`
back to `build` are domain outcomes on the graph, never engine failures.

![The same run in dark mode, following the OS color scheme](docs/assets/run-view-dark.png)

Dark mode follows your OS, with the same design tokens the
[agentculture.org](https://agentculture.org) site ships — nothing here invents
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
  derive), transactional outbox, Postgres/SQS queue drivers, scheduler,
  worker.
- **Actor protocol** for external agents — colleague, claude, codex, or
  anything speaking HTTP/JSON — with a runnable conformance kit
  (`tests/conformance`) and a reference bridge (`adapters/colleague`).
- **Runner boundary** for code nodes: AWS Lambda adapter (registry-pinned,
  IAM-scoped, honest evidence) and a headspace-cli bridge for local dev. No
  Docker socket ever enters a control-plane container.
- **Web front** (`web/`): the read-only Run and Ledger views above, embedded
  into the Go binary.
- **Python CLI front** (`nodes` on PyPI): thin, zero-dependency client of the
  same API.

The full design lives in
[`docs/initial-design/culture-nodes-prd-spec.md`](docs/initial-design/culture-nodes-prd-spec.md);
what was built, with evidence, in
[`docs/acceptance.md`](docs/acceptance.md) and
[`docs/deliveries/`](docs/deliveries/).

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

This repo is also a Culture mesh agent: `culture.yaml`
(`suffix: culture-nodes`, `backend: colleague`) with the resident prompt file
`AGENTS.colleague.md`, and the vendored guildmaster/devague skill kit under
`.claude/skills/` (cite-don't-import — see
[`docs/skill-sources.md`](docs/skill-sources.md)).

## Contributing

See [`CLAUDE.md`](CLAUDE.md) for the working conventions: the design ground
rules distilled from the PRD, the version-bump-every-PR rule, the `cicd` PR
lane, and the vendored-skills policy.

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
