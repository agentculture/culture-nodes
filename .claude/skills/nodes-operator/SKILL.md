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
| `workflows` | published workflows (key, digest) |
| `runs [N]` / `run <id>` | newest runs / one run's nodes, outcomes, attempts |
| `ledger <id>` | a run's ledger records (authority, origin actor, data) |
| `tasks` | pending human tasks |
| `validate <f.yaml>` / `publish <f.yaml>` | server-side compile check / publish, printing the content digest |
| `create <digest> <input.json> --yes` | create a run from a published digest |
| `watch <id>` | poll to terminal, then print outcomes + ledger |
| `cancel <id>` | cancel: reaps work items, best-effort Cancels in-flight sessions |
| `assign <actor> "instruction" [opts] --yes` | the headline: one-node workflow → publish → run → watch |
| `actors` | registered actor rows (the one verb needing `ssh thor`) |

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
`--no-watch`.

Actors: `codex-thor`, `codex-orin` — each maps to its host's allowlisted
checkout (`/home/<host>/git/culture-nodes-agent`). New actors: register with
`deploy/prod/register-actor.sh`, then extend the actor table in
`scripts/nodes-op.sh` (one case line).

## What every operator must know

- **Billable + guarded.** `assign`/`create` dispatch real agent sessions
  (ChatGPT quota today). They refuse without `--yes` / `NODES_OP_YES=1`.
- **Sandbox reality (issue #18).** codex sessions on thor/orin currently
  cannot exec shell commands in ANY sandbox mode (bwrap userns limits) —
  they read files and reason. Assign analysis/reading tasks; don't expect
  `git` output until #18 is fixed.
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

## Provenance

First-party to **culture-nodes** — authored here (2026-08-12 cycle), not
vendored; steward may broadcast it to the mesh later. Cite, don't import:
downstream repos copy it. See `docs/skill-sources.md`.
