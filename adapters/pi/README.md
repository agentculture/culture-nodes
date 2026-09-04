# pi-bridge

A stdlib-only Culture Nodes actor-protocol bridge for
[`@earendil-works/pi-coding-agent`](https://github.com/badlogic/pi-mono), driven
as one non-interactive turn with `pi --mode json -p <instruction> --no-session`.
It streams JSONL, uses `message_end` for the final assistant message and usage,
and requires `agent_end` before claiming success. There is no ACP package.

## Trust model

The account is the boundary. Pi has no sandbox and no tool-approval prompt;
its tools run as the process user. `input.sandbox` describes dispatch policy
but does not add confinement. Run the bridge under a dedicated Unix account
whose files, credentials, processes, and network access may safely be exposed
to the agent. The exact repository allowlist selects the working directory; it
does not prevent pi from reaching other resources available to that account.
Project-local `.pi` resources retain their normal pi behavior. The bridge does
not pass `--no-extensions`, `--no-skills`, or `--no-prompt-templates` by
default. It does pass `-a`, explicitly trusting project-local `.pi` resources
for that allowlisted repository and that run.

Cancellation sends SIGTERM to the complete process group, including tool
children. Every invocation is tee'd while parsed to
`<state_dir>/pi-transcripts/<invocation-id>.jsonl`.

## Invocation input

`instruction` and `repo` are required. `repo` must resolve through
`repo_allowlist`, `repo_allowlist_prefixes`, or `repo_identities`. Optional
fields are `sandbox` (`read-only` or `workspace-write`), `handover` (requires
workspace-write), `model` (overrides config), `max_steps` (dispatch timing
only; never forwarded to pi), `async`, and `success_outcome`. The shared
bridge core also accepts session metadata, but pi always runs with
`--no-session` and does not resume it.

## Configuration

The JSON config supports `pi_bin`, `pi_env`, `provider`, `model`, `state_dir`,
`repo_allowlist`, `repo_allowlist_prefixes`, `repo_identities`, `config_repo`,
`always_async`, and `default_sandbox`, plus the shared HTTP, callback,
preservation, and timing settings in `config.py`. Environment overrides use
the `PI_BRIDGE_` prefix; notably `PI_BRIDGE_PI_BIN`, `PI_BRIDGE_PROVIDER`, and
`PI_BRIDGE_MODEL`. `pi_bin` is executed directly, so its `#!/usr/bin/env node`
resolves Node using PATH from the inherited environment plus `pi_env`.

```json
{
  "actor_id": "actor_register_…",
  "host": "127.0.0.1",
  "port": 8093,
  "pi_bin": "/home/pi-agent/.local/bin/pi",
  "pi_env": {"PATH": "/home/pi-agent/.local/bin:/usr/bin"},
  "provider": "anthropic",
  "model": "claude-sonnet-4-5",
  "state_dir": "/home/pi-agent/.local/state/culture-nodes-pi",
  "repo_allowlist": ["/home/pi-agent/work/culture-nodes"],
  "repo_identities": {"agentculture/culture-nodes": "/home/pi-agent/work/culture-nodes"},
  "config_repo": "/home/pi-agent/work/culture-nodes",
  "always_async": true,
  "default_sandbox": "workspace-write"
}
```

## Run and systemd

```bash
uv sync
uv run pytest -q
uv run pi-bridge --config ~/.config/culture-nodes-bridges/pi.json
```

```ini
[Unit]
Description=Culture Nodes pi bridge
After=network-online.target

[Service]
Environment=PI_BRIDGE_CONFIG=%h/.config/culture-nodes-bridges/pi.json
WorkingDirectory=/home/pi-agent/src/culture-nodes/adapters/pi
ExecStart=/home/pi-agent/.local/bin/uv run pi-bridge
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```
