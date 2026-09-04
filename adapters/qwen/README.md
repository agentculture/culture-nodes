# qwen-bridge

A culture-nodes actor bridge for [qwen-code](https://github.com/QwenLM/qwen-code),
driven over ACP (Agent Client Protocol) on stdio. Same actor protocol as the
`codex`, `claude-code`, `colleague` and `jira` siblings: `POST /v1/invocations`,
`POST /v1/invocations/<id>/cancel`, `GET /v1/capabilities`, `GET /healthz`.

The bridge was brought up as a real registered actor on 2026-08-27 and
executed a live session end to end. Zero runtime dependencies, stdlib only,
like every adapter here.

The account of how it got here, and the five defects found doing it, is
[`docs/deliveries/2026-08-27-qwen-bridge-first-dispatch.md`](../../docs/deliveries/2026-08-27-qwen-bridge-first-dispatch.md).

## Trust model — read this first

The bridge's own capability document says it plainly:

> `confinement: qwen-code runs its own tools in-process as the bridge user`
> (measured 2026-08-23: no fs/terminal client requests, no sandbox helper)

qwen-code executes its tools inside its own process. The production bridge runs
as the dedicated Unix account `culture-qwen`, with no sudo access and no Docker
group membership. **That Unix-user account is the confinement boundary.** ACP
session modes are approval policies, not additional confinement; consequently
`yolo` widens no OS authority and is admitted. See the account design in
[`docs/deviations/2026-08-29-agents-as-os-users.md`](../../docs/deviations/2026-08-29-agents-as-os-users.md).

What that leaves as the actual boundary:

- **The exact-match repo allowlist** (`repo_allowlist` / `repo_allowlist_prefixes`).
  It governs which checkout a dispatch *resolves to*. It does not stop the
  process touching anything else the user can reach.
- **The `culture-qwen` account's privileges.** Its lack of sudo and Docker-group
  access is the boundary around every mode.

Practically: give `culture-qwen` a dedicated worktree and only the credentials
and paths intended for Qwen dispatches.

## Why it was parked

`acp/transport.py` **fails closed** on `session/request_permission` — a bridge
with no human attached must not invent consent. The mode decision is now:

| Mode | Unattended policy |
|---|---|
| `plan` | no — analysis only by definition |
| `default` | permission requests are cancelled |
| `auto-edit` | edits are approved by the agent policy; other permission requests are cancelled |
| `auto` | agent-policy approvals apply; other permission requests are cancelled |
| `yolo` | admitted; the account is the boundary |

Two reporting rules are unconditional in every mode: a bridge-cancelled
permission followed by `end_turn` reports `permission_blocked`, and a
`workspace-write` dispatch with no measured changed files reports `no_changes`.
Neither reports `completed`.

Historical note: **#228 was the thing to answer**; this change answers it by
admitting `yolo` under the engine account and adding those two distinct outcomes.

One input to that decision, recorded so it is not re-derived: **h15's live check
is satisfied.** Run `01M11NNKGNR8PMG995C2JPQ1G9` showed a fresh `session/new`
returning `yolo` among `availableModes`, `session/set_mode` round-tripping with
a confirming `mode-update` echo, and `auto` demonstrably behaving as its
description claims. That evidence established that admitting `yolo` would not
widen confinement; the dedicated account decision made that admission policy.

## The ACP seam

One `qwen --acp` child per invocation, spoken to over stdio JSON-RPC by a driver
child (`python -m qwen_bridge.qwen_cli`), whose argv is assembled in exactly one
place (`acp/dispatch.py::_driver_argv`).

```text
initialize        handshake gate (c19/h16) — refuses before serving
session/new       returns sessionId, availableModes, model identity
session/set_mode  the mode policy (h15) — set from input, NEVER the agent default
session/prompt    the turn; session/update events stream back as progress
```

Two properties worth knowing:

- **The mode is never defaulted.** `acp/gate.py::resolve_acp_mode` refuses a
  missing, out-of-vocabulary, or agent-unoffered mode rather than picking one.
  A session running in a mode nobody chose would be a silently-different grant.
  `input.mode` is therefore **required** — a dispatch without it gets a 400.
- **A refusal is not an execution failure.** The driver writes
  `qwen-acp-refusal: <detail>` to stderr and exits `wire.REFUSAL_EXIT_CODE`.
  Both dispatch paths parse it via the shared `dispatch.refusal_detail` and
  report `actor_rejected_input`. (They did not always: on the async path the
  message used to be discarded — #225.)

Transcripts of every session land in `<state_dir>/acp-transcripts/<id>.jsonl`,
one JSON object per line with `dir` (`c->a` / `a->c`), `ts`, and `msg`. They are
the first thing to read when a dispatch behaves oddly.

## Invocation input

| Field | Required | Notes |
|---|---|---|
| `instruction` | yes | the prompt |
| `repo` | yes | must match the allowlist exactly |
| `mode` | **yes** | `plan` \| `default` \| `auto-edit` \| `auto` \| `yolo`. Never defaulted |
| `sandbox` | no | `read-only` \| `workspace-write`. Not a kernel boundary here |
| `handover` | no | create + push a git ref. Requires `sandbox: workspace-write` |
| `model` | no | overrides the configured model |
| `max_steps` | no | **dispatch-timing only** — qwen has no `--max-steps` to forward |
| `async` | no | force the async path; `always_async` config does it globally |
| `success_outcome` | no | the outcome name reported on a clean turn |
| `session_key`, `continuation_ref` | no | resume; `continuation_ref` is accepted but not implemented |

## Build and run

```bash
cd adapters/qwen
uv sync
uv run pytest -q                    # 354 tests, no network, no qwen needed

# What this host would advertise at registration — needs a real qwen install,
# because it measures one with a scratch session:
uv run qwen-bridge --config ~/.config/culture-nodes-bridges/qwen-developer.json \
    --print-capabilities
```

Minimal config (`~/.config/culture-nodes-bridges/qwen-developer.json`, mode 600):

```json
{
  "actor_id": "actor_register_…",
  "port": 8092,
  "host": "0.0.0.0",
  "auth_token": "…",
  "repo_allowlist": ["/home/spark/git/.worktrees.culture-nodes/qwen-dev"],
  "repo_identities": {
    "agentculture/culture-nodes": "/home/spark/git/.worktrees.culture-nodes/qwen-dev"
  },
  "config_repo": "agentculture/culture-nodes",
  "always_async": true,
  "state_dir": "/home/spark/.local/state/culture-nodes-bridges/qwen-developer",
  "default_sandbox": "workspace-write"
}
```

Every field is also settable as `QWEN_BRIDGE_*` (see `config.py`).

## Registering it as an actor — the four-place ritual

This is the part that is easy to get wrong, and the reason a registration can
report success against a configuration that cannot work (**#222**). A new actor
token must be declared in **four** places, by hand, and nothing checks that they
agree:

1. **`prod.env` on every host running a worker** — the value.
2. **`deploy/prod/compose.thor.yml`** — the name, in **both** the `api` and
   `worker` environment blocks. There is no `env_file:`, so `prod.env` reaches
   compose, not the container.
3. **`deploy/prod/compose.orin.yml`** — the name again. **orin runs a second
   worker on the same namespace**, so whichever polls first claims the item; a
   token on only one host makes auth a coin flip (**#224**).
4. **`deploy/prod/audit-credentials.sh`'s `audit_classification()`** — or the
   audit warns the key is unclassified and fails the host as incomplete.

Then:

```bash
# on the control-plane host
deploy/prod/register-actor.sh company/qwen-developer http://<ip>:8092 \
    NODES_ACTOR_QWEN_TOKEN --metadata repository_identity=agentculture/culture-nodes

# recreate the services so they carry the new env
docker compose --env-file ~/.culture-nodes/prod.env \
    -f deploy/prod/compose.thor.yml up -d api worker

# verify — from the OPERATOR machine, not the host (it sshes to the host itself)
bash deploy/prod/audit-credentials.sh thor
bash deploy/prod/audit-credentials.sh orin
```

The audit compares `prod.env` against **that host's own compose file**, so it
cannot see a key the compose file never declared. It will not catch step 2 or 3
being skipped. Nothing currently compares the two compose files to each other —
that is **#226**.

Finally, add the actor to `nodes-op.sh`'s table so `assign` can reach it, and
dispatch:

```bash
bash .claude/skills/nodes-operator/scripts/nodes-op.sh assign qwen-developer \
    "…instruction…" --sandbox workspace-write --mode auto --yes
```

## Systemd

```ini
[Unit]
Description=Culture Nodes qwen bridge (qwen-developer)
After=network-online.target

[Service]
Environment=QWEN_BRIDGE_CONFIG=%h/.config/culture-nodes-bridges/qwen-developer.json
WorkingDirectory=/home/spark/git/culture-nodes/adapters/qwen
ExecStart=/home/spark/.local/bin/uv run qwen-bridge
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

This is an **editable** install: it serves the working tree, so it cannot go
stale but can be serving uncommitted code. `--print-capabilities` reports that
as `revision_is_dirty` in its `deployment` block. Check it before trusting a
probe.

## Worktree reaping

`--reap-plan <repo>` prints what the worktree reaper would reclaim, read-only,
with the exact `git worktree remove` it would run. `--reap-perform` acts on it;
`--force` is never passed, so a dirty worktree is retained rather than
destroyed. Standalone it has no session registry, so add `--reap-assume-idle`
to state positively that no live session is held.

## Known gaps

| Issue | What |
|---|---|
| **#228** | answered: `yolo` admitted under `culture-qwen`; permission-blocked and no-change turns report distinct outcomes |
| #222 | `register-actor.sh` validates no `auth_token_env` against the compose files |
| #224 | two workers, two credential sets |
| #226 | no mesh-wide view of what each machine is serving |
| #120 | the async handover path has still never created a ref in any bridge |
| #214 | this plan's t5–t8; t6 (Dockerfile + CI + conformance kit) and t8 are still unstarted |
