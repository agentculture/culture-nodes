---
name: nodes-operator
description: >-
  Drive a running culture-nodes control plane: inspect runs/ledger/actors,
  author + validate + publish workflows, and assign real work to registered
  agent actors (codex-thor, codex-orin) through the normal engine dispatch
  path. Use when the user says "assign this to codex", "run it through
  nodes", "create a workflow for X", "what's running", "cancel that run",
  "check the ledger", or when you want to delegate a scoped task to the
  production actor fleet instead of doing it in-session. Works for ANY
  operator with the skill — Claude, the colleague backend, codex sessions,
  or a human at a shell — plain bash + curl + python3 against the public
  v1alpha1 HTTP API.
type: command
---

# nodes-operator — assign work through the culture-nodes system

The point of this skill: **anyone — this agent, another agent, a human — can
delegate a scoped task to the registered actor fleet** and get back a run id,
node outcomes, and proposed ledger claims, all through the engine's normal
dispatch path. No bypasses: everything lands in the same runs/board/jobs UI,
the same ledger, the same evidence trail.

## How to run

```bash
bash .claude/skills/nodes-operator/scripts/nodes-op.sh <verb> [args]
```

API resolution: `$NODES_API_URL` → `~/.culture-nodes/operator.env` →
`http://192.168.1.146:18080` (thor's LAN address; the web UI lives at the
same origin). Requires `bash`, `curl`, `python3` (+PyYAML for
`validate`/`publish`/`assign`; `jq` not required).

## The verbs

| Verb | What it does |
|------|--------------|
| `status` | healthz + run-state counts |
| `running` | every running run with its metadata/input and current node/attempt detail |
| `workflows` | published workflows (key, digest) |
| `runs [N]` / `run <id>` | newest runs / one run's nodes, outcomes, attempts |
| `ledger <id>` | a run's ledger records (authority, origin actor, data) |
| `tasks` | pending human tasks |
| `validate <f.yaml>` / `publish <f.yaml>` | server-side compile check / publish, printing the content digest |
| `create <digest> <input.json> [--category C] --yes` | create a run from a published digest |
| `watch <id>` | poll to terminal, then print outcomes + ledger |
| `cancel <id>` | cancel: reaps work items, best-effort Cancels in-flight sessions |
| `grade <run-id> --rating N --notes "..." [--actor ID] [--as ID] [--category C]` | grade a run against an actor (1-5 rating + rationale, issue #28 item 1) |
| `assign <actor> "instruction" [opts] --yes` | the headline: one-node workflow → publish → run → watch |
| `actors` | registered actor rows, read over the API (no `ssh`) |

## `assign` — delegate one task to one actor

```bash
bash .claude/skills/nodes-operator/scripts/nodes-op.sh assign codex-thor \
  "Read deploy/prod/README.md and report any statement that no longer matches the repo tree. Make no changes." \
  --yes
```

Renders `templates/assign.workflow.yaml` (single `task` node → `finish`,
modelled byte-for-byte on the proven `examples/codex-smoke-pair` shape) for
the chosen actor, publishes it (idempotent — identical renders return the
same digest), creates the run with the instruction as input, and watches it
to terminal. Options: `--sandbox read-only|workspace-write` (default
read-only), `--timeout` (default 15m — always explicit: a node timeout is
the recovery story for a bridge restart), `--retries` (default 1 — a
billable session is investigated, not auto-retried), `--outcome`,
`--category` (optional flat tag on the created run, e.g. `review`, `audit`
— retaggable later via `nodes run retag <id> --category C`), `--devague-write`
(the package writes `.devague/`: developer lane only, in its custody
checkout — refused for every other actor before anything is billed; see
`docs/operations/spec-chain-lane.md`), `--no-watch`.

Actors map to a checkout owned by that bridge's dedicated Unix account, not
to the operator's own checkout: `codex-thor` and `codex-orin` each run as
`culture-codex` on their host, both checking out
`/home/culture-codex/git/culture-nodes-agent`; `developer`, `planner`,
`verifier`, `intake` run as `culture-claude` on spark, each in its own clone
under `/home/culture-claude/git/culture-nodes-<role>`; `qwen-developer` runs
as `culture-qwen` on spark, in `/home/culture-qwen/git/culture-nodes-qwen-developer`.
New actors: register with `deploy/prod/register-actor.sh`, then extend the
actor table in `scripts/nodes-op.sh` (one case line) with a checkout under
that bridge's account.

## `grade` — record an opinion on a run's actor

```bash
bash .claude/skills/nodes-operator/scripts/nodes-op.sh grade run_01J... \
  --rating 4 --notes "read the files it was asked to, correct diagnosis, one stale detail" \
  --actor codex-thor-actor-id
```

Appends a `grade` ledger record (`POST /v1alpha1/runs/{id}/grades`) evaluating
`--actor` on the named run. `--as` (the grading actor) defaults to the first
registered `kind=human` actor; `--actor` defaults to the run's most recently
attempted actor when it can be read cheaply off the run itself — both can be
passed explicitly, and the verb refuses rather than guessing when neither a
default nor an explicit value is available. The API looks up `--as`'s
registered kind and decides origin/authority from it: a human actor's grade
lands `confirmed` immediately; an agent actor's grade lands `proposed` and
reaches `confirmed` only through the ordinary review surface. No actor may
grade its own work.

This is the mechanism behind CLAUDE.md's "nodes dogfooding reflex" —
grade every assigned run against its actor so the comparative record of
which actor is better at what accrues as first-class ledger evidence, not
only as an eidetic `/remember` note.

## What every operator must know

- **Billable + guarded.** `assign`/`create` dispatch real agent sessions
  (ChatGPT quota today). They refuse without `--yes` / `NODES_OP_YES=1`.
- **Sandbox reality (issues #18 / #63) and the account model.** codex
  sessions on thor/orin **can exec shell commands**, including in a
  `read-only` sandbox. The blocker was the host AppArmor userns restriction,
  not codex: with `kernel.apparmor_restrict_unprivileged_userns=0` applied
  and persisted on all three hosts, run `01M00AM5NME6TZ1PXDG4A454HE` executed
  `git log`, `git status`, `pwd` and `ls` and returned real output. Every
  bridge now runs as its own dedicated, unprivileged Unix account
  (`culture-codex` on thor/orin, `culture-claude` and `culture-qwen` on
  spark — `deploy/prod/lanes/unix-user.sh`) rather than the operator's login
  user, so a `--sandbox workspace-write` dispatch is confined by account
  ownership, not by a sandbox flag: the account cannot write anything it
  does not own, and it cannot read the operator's own checkout at all. The
  `read-only` probe above was pre-account. The account model was proven
  live on 2026-08-29/30 (#243 t10): as `culture-codex`, runs
  `01M17E36Z7G8A60WP6TA9QB0AP` (orin) and `01M17E3ZC1MBQ6DZRKY93T8PWE`
  (thor) fetched from github, ran black/isort/flake8 through `uvx`, passed
  29 loopback tests and were denied on the login user's home, `sudo` and
  the docker socket; runs `01M17NJ8W7B6NQ2RKT6B89SF94` / `01M17NK19NY09G55J7AJWYW7XT`
  did the same under `workspace-write` with network from the account's own
  codex config. As `culture-claude`, run `01M17P15BJ28XN57N7P7Q77Y48` did
  all of that AND handed its change over as
  `refs/culture-nodes/01M17P15BJ28XN57N7P7Q77Y48/...` authored by the
  account, fetched by the operator as `culture-claude@localhost`. Assign
  reads, analysis and writes freely; a `--handover` dispatch is how a write
  reaches you (see "Harvesting a dispatched checkout").
- **Results are claims, not evidence.** A session's report lands as a
  `proposed` ledger claim attributed to its registered actor. Confirming it
  is a human's job; treat the summary exactly as you'd treat a colleague's
  "done" — worth reading, not worth asserting onward unverified.
- **Retry semantics.** The engine parks a node as `failed` after 3 dispatch
  attempts per work item; workflow-declared `--retries` compose on top.
  `cancel` reaps immediately and SIGTERMs in-flight sessions (~30ms
  observed).
- Everything is inspectable afterward: web UI at the API origin, this
  skill's `run`/`ledger` verbs, or the Python `nodes` CLI
  (`uv run nodes run list`) which speaks the same API.

## Harvesting a dispatched checkout

A dispatch writes into the *bridge's* account checkout, not the operator's.
To read or copy what an actor left behind, log in as that account rather
than sudo-ing into its home: `ssh culture-<engine>@<host>` (`culture-codex`
on thor/orin, `culture-claude` or `culture-qwen` on spark — for spark use
`ssh culture-<engine>@localhost` since the operator is already on that
host). Never `sudo -u` into the account or read its files as the operator's
login user — the account's checkout is deliberately unreadable to any other
account, the harvest path goes through the account's own login, the same
way a dispatch does.

## Split-plan lane guidance and session accounting (issue #48)

When building an implementation split plan (via `/spec-to-plan` and
`/assign-to-workforce`), account for session economics across the dispatch
landscape. The operator's interactive Claude, local subagents, and all bridge
sessions share ONE subscription window — not independent capacity pools.

### One meter, many lanes

- **Operator main loop** (lowest cost): prompt cache warm, marginal session
  cost lowest; right place for small/mechanical steps and merge gates.
- **Local subagents**: cold-ish start but same-process context handoff, no
  HTTP overhead.
- **Bridge sessions** (full cold tax): each codex exec / claude -p / colleague
  work invocation pays a complete cold-session cost (repo discovery, plan
  reading, zero conversational history) plus engine dispatch overhead. Worth
  it only when ledger attribution, isolation, or cross-machine execution
  justifies it.

### Node granularity and routing

- **Work-package model nodes**: amortize bootstrap into one persistent warm
  session with many ledgered sub-actions, never one cold session per small
  plan task. This cycle's wave-0 averaged ~25k output tokens per task on top
  of repeated cold bootstrap — a shared workstream session would have paid
  that bootstrap once.
- **Deterministic/code nodes**: stay microscopic — these belong in the
  operator lane or as part of a larger model workstream.
- **Codex-first routing** for big analysis and build packages: the cold-session
  tax is large enough to amortize over significant work; reserve the operator's
  Claude window for operator-lane work and human merge gates.

### Split-plan template

Before any fan-out, declare expected model-session count per wave against the
remaining subscription window (windows reset on a fixed clock; the operator should
know the reset time and remaining capacity before planning). Copy this template
into the plan's waves section and fill it in:

```yaml
# Wave W: <description>
# - Tasks: t1, t2, t3, ...
# - Model sessions (remaining window): N (justify per lane: operator X sessions,
#   codex-bridge Y sessions for big packages P1, P2, ...)
# - Non-billable: deterministic nodes, human tasks
```

Example:

```yaml
# Wave 0: Core design pass and scope exploration
# - Tasks: t1 (scope), t2 (think), t3 (challenge), t4 (spec-to-plan)
# - Model sessions (remaining window: 4h): 1 (operator lane, local design refinement)
# - Non-billable: challenge pass findings, spec review
#
# Wave 1: Phase-0 vertical slice implementation
# - Tasks: t5–t11 (usage telemetry, session stickiness, breaker, pacing, budget)
# - Model sessions (remaining window: 3.5h): 3 (codex-thor for usage + stickiness,
#   codex-orin for breaker logic + pacing; operator: PR review gates only)
# - Non-billable: tests, documentation
```

Record deviations (issue #48 comment item 5): if a wave exhausts budget mid-execution
or stays significantly under-budget, emit a deviation record with the observed session
count and reason (longer node output, unforeseen complexity, faster execution). These
records feed the next build's planning accuracy and the economics analysis (issue #28
per-actor analytics).

## Provenance

First-party to **culture-nodes** — authored here (2026-08-12 cycle), not
vendored; steward may broadcast it to the mesh later. Cite, don't import:
downstream repos copy it. See `docs/skill-sources.md`.
