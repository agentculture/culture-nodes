# AGENTS.md

Guidance for a codex session dispatched into this checkout by the
culture-nodes codex-bridge. This is the culture-nodes agent workspace on a
production host (thor/orin), not a local dev clone.

## Sandbox and scope

- Sessions default to a **read-only** sandbox. Write access is granted only
  when the dispatched task explicitly requests `workspace-write` — don't
  assume it.
- Never `git commit` or `git push` from a session. Any diff you produce is
  harvested by the human operator afterward; leave changes uncommitted in
  the working tree.
- A result you report ("done", "tests pass", "fixed") is a **completion
  claim**, not verified evidence — see the ledger authority rules under
  "Repository invariants" below. State
  what you did and how you checked it; let the operator confirm.

## Control-plane access

The control plane runs on thor. Point client calls at it:

```bash
export NODES_API_URL=http://thor:18080
```

This checkout carries the Python CLI front (`culture_nodes/`, zero
third-party dependencies) — run it with `uv run nodes <verb>`. It is a thin
REST client over `/v1alpha1` and reads `NODES_API_URL` automatically. Verbs
for the read surfaces you'll want most:

```bash
uv run nodes run list [--state STATE] [--updated-since T] [--updated-until T]
uv run nodes run get <run-id>                 # run, tokens, node runs, attempts
uv run nodes run events <run-id>               # follow the run's event stream
uv run nodes node-runs list                    # cross-run "jobs timeline"
uv run nodes ledger records <run-id>            # a run's ledger records
uv run nodes ledger projection <run-id> <name>  # a standard ledger projection
uv run nodes human-tasks list [--status pending|decided]
uv run nodes human-tasks get <task-id>
```

`human-tasks decide` exists too but requires a bearer token
(`NODES_HUMAN_DECISION_TOKEN` or `--token`) — that's the operator's call,
not a session's.

Note: the Go control-plane binary (`nodes serve` / `scheduler` /
`worker`) runs inside the compose containers, not on the host PATH. The
host-installed `~/.local/bin/nodes` is the Python query client (uv tool);
it is the one with list/get verbs for runs, node runs, ledger, and human
tasks.

## Repository invariants

- Never edit anything under `.claude/skills/**` — it's vendored verbatim
  from sibling repos (see `docs/skill-sources.md`); modify the source repo
  instead.
- Every PR bumps the version in `pyproject.toml` (CI's `version-check` job
  blocks merge otherwise) — a dispatched task that opens or updates a PR
  needs a version bump in the diff.
- The work ledger's authority model: agents (including this session) may
  only produce `proposed` records. Only a human `confirm`s or `reject`s;
  only a trusted runner emits `observed` evidence for what it directly
  measured. Don't describe your own output as confirmed or verified.
