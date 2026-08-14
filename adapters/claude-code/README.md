# culture-nodes-claude-code-bridge

A **reference** implementation of the Culture Nodes actor protocol
(PRD §13, `internal/actors/protocol.go`) over headless `claude -p`
(Claude Code CLI "print mode") subprocess dispatch. It is the claude-code
sibling of [`adapters/colleague`](../colleague/README.md) — deliberately
mirroring that package's layout, module names, and discipline field for
field wherever the concern is protocol-level rather than backend-specific,
so a reader of one adapter can read the other with almost no relearning.

This package is deliberately **separate** from the `culture-nodes` Python
package: its own `pyproject.toml`, its own dependency group, its own test
suite. It is not installed as part of a culture-nodes deployment.

## Deployment model

```text
   Culture Nodes control plane                 Agent host (this bridge)
   ┌─────────────────────────┐   HTTPS POST    ┌──────────────────────────┐
   │ engine dispatches an     │ ───────────────▶│ claude-code-bridge        │
   │ attempt to an actor      │                 │  (this package)           │
   │ endpoint (internal/      │◀─────────────── │  --add-dir <allowlisted>  │
   │ actors.Client)           │   callbacks      │  shells out to `claude    │
   └─────────────────────────┘   (PRD §13.4)     │  -p ...`                  │
                                                  └──────────┬───────────────┘
                                                             │ subprocess
                                                             ▼
                                                  ┌──────────────────────────┐
                                                  │ claude (Claude Code CLI), │
                                                  │ authenticated against a   │
                                                  │ real Anthropic backend    │
                                                  └──────────────────────────┘
```

The bridge runs **beside a `claude` install that is already authenticated**
(a Claude subscription's OAuth session, or `ANTHROPIC_API_KEY`) — not inside
the culture-nodes control-plane process, and never through a shell/Docker
socket the control-plane process reaches into. An operator:

1. Provisions a host (or container) with `claude` installed and
   authenticated.
2. Checks out (or mounts) the repositories this bridge is allowed to
   dispatch work into.
3. Configures the bridge (below) with that repo allowlist, starts it, and
   registers its base URL + a scoped workload token as an actor endpoint in
   Culture Nodes.

Culture Nodes reaches the bridge over the network exactly as it would reach
any other actor (`internal/actors.Client.Invoke`/`Cancel`); the bridge
reaches `claude` as a local subprocess, never the reverse.

## How headless claude is driven

Synchronous invocation (`claude_cli.run_sync`):

```text
claude -p "<instruction>" \
  --output-format json \
  --permission-mode <bypassPermissions|acceptEdits|...> \
  [--agent <role>] [--max-turns <max_steps>] [--model <model>]
```

run with `cwd=<the allowlisted repo>`, in the foreground, bounded by
`Config.sync_timeout_seconds`. `--output-format json` prints exactly one
`type: "result"` JSON object to stdout on exit (see `mapping.py`'s module
docstring for the pinned shape); the last non-blank line of stdout is parsed
as that object. A crashed session, a kill, or output that never parses as
JSON all read back as "no result" (`None`) — the same as an explicit
`is_error: true` — and are reported as an execution failure, never success.

Asynchronous invocation (`claude_cli.spawn_background`) swaps
`--output-format json` for `--output-format stream-json --verbose`, and this
bridge spawns it itself as a **detached** background process (`claude` has
no native `--background`/start-payload protocol the way colleague's own CLI
does), redirecting its stdout straight into a per-invocation feed file this
bridge's own `AsyncRunner` tails for progress — see `flightfiles.py`'s
module docstring for the full mechanics and how this differs from
colleague's native flight control plane.

## Session resume (task t5)

When a request carries a top-level `continuation_ref` (§13.1,
`internal/actors/protocol.go` — a sibling of `run_id`, NOT nested inside
`input`), this bridge resumes that prior session with `claude -p ...
--resume <continuation_ref>` on both the sync and async dispatch paths. On
a successful turn, claude's own reported `session_id` rides back as
`continuation_ref` in the §13.2 result body AND the §13.4 `completed`
callback event, so a later attempt against the same actor can resume it in
turn (the engine side that wires this end to end is task t4/ADR 0010).

`session_key` — the eventual workstream key spec claim c3 targets (ADR
0010 §4) — and `continuation_ref` are both **transport keys**: neither is
ever forwarded into the instruction text handed to claude, even if a
caller nests either inside `input` instead of sending `continuation_ref`
at the top level as the real wire shape does.

## Capacity refusals (task t5, deviation d4)

When claude's own failure text names a provider-side quota, rate-limit, or
session-limit refusal (matched against the Anthropic API's own error
vocabulary — `rate_limit_error`, `overloaded_error` — plus everyday
phrasing like "usage limit"/"quota"/"session limit"), this bridge
classifies the failure `capacity_exhausted` (§13.5) instead of plain
`execution`, and — when the text names a delay ("retry after N
seconds") — sets an HTTP `Retry-After` header on the synchronous failure
response (`internal/actors/client.go` reads the delay from exactly that
header, never the JSON body). The control-plane side of this — pausing
dispatch to the actor until the delay elapses — is the capacity circuit
breaker (`internal/worker/breaker.go`, task t9); this bridge's job is only
the honest classification.

`--permission-mode` matters specifically because headless dispatch has no
TTY to answer an interactive permission prompt: `Config.permission_mode`
(default `"bypassPermissions"`) must be a mode that never blocks on one.

## The claude CLI version gate

This bridge refuses to dispatch — 503, class `actor_unavailable`, honestly
naming both the detected and the required version — against a `claude`
binary older than `Config.min_claude_version` (default **2.1.220**), checked
via `claude_cli.ensure_supported_version` at the top of every dispatch path
(and again, fail-fast, at `claude-code-bridge` process startup).

**2.1.220** is the oldest version currently running across the fleet this
bridge was built against (thor `2.1.220`, orin `2.1.221`, dev `2.1.226` at
the time of writing) — the honest floor is "the oldest version this bridge
has actually been validated against", not a guessed feature-introduction
version this bridge cannot independently confirm. Moving the pin (in either
direction) is a config change (`min_claude_version`, or
`CLAUDE_CODE_BRIDGE_MIN_CLAUDE_VERSION`), never a silent behavior change on
upgrade.

## Configuration

Env-first, with an optional small JSON file underneath (`config.py`'s own
docstring has the full precedence rule).

| Config file key | Env var | Default | Meaning |
|---|---|---|---|
| `repo_allowlist` | `CLAUDE_CODE_BRIDGE_REPO_ALLOWLIST` (`:`-joined) | `[]` | Absolute repo paths this bridge will dispatch into. A request naming any other `input.repo` is refused `403`. **Empty means the bridge accepts no repo.** |
| `claude_bin` | `CLAUDE_CODE_BRIDGE_CLAUDE_BIN` | `"claude"` | Path/name of the claude executable (resolved via `PATH` if bare). |
| `claude_env` | — (file only) | `{}` | Extra env vars merged onto every claude subprocess (`ANTHROPIC_API_KEY`, ...). Operator-supplied only. |
| `permission_mode` | `CLAUDE_CODE_BRIDGE_PERMISSION_MODE` | `"bypassPermissions"` | Forwarded as `--permission-mode`. Must be a mode that never blocks on an interactive prompt. |
| `model` | `CLAUDE_CODE_BRIDGE_MODEL` | `""` (unset) | Default `--model`, when `input.model` is absent. |
| `min_claude_version` | `CLAUDE_CODE_BRIDGE_MIN_CLAUDE_VERSION` | `"2.1.220"` | The version gate — see above. |
| `sync_max_steps` | `CLAUDE_CODE_BRIDGE_SYNC_MAX_STEPS` | `6` | Dispatch threshold: an expected step budget above this goes async. |
| `default_max_steps` | `CLAUDE_CODE_BRIDGE_DEFAULT_MAX_STEPS` | `6` | Assumed step budget when `input.max_steps` is absent. |
| `always_async` | `CLAUDE_CODE_BRIDGE_ALWAYS_ASYNC` | `false` | Force every invocation asynchronous. |
| `default_success_outcome` | `CLAUDE_CODE_BRIDGE_DEFAULT_SUCCESS_OUTCOME` | `"completed"` | Domain outcome for a successful result when `input.success_outcome` is absent. |
| `actor_id` | `CLAUDE_CODE_BRIDGE_ACTOR_ID` | `"claude-code-bridge"` | `origin.actor_id` on every proposed ledger record. |
| `host` | `CLAUDE_CODE_BRIDGE_HOST` | `"127.0.0.1"` | Bind address. |
| `port` | `CLAUDE_CODE_BRIDGE_PORT` | `8086` | Bind port. `0` picks a free port (useful for tests). |
| `auth_token` | `CLAUDE_CODE_BRIDGE_AUTH_TOKEN` | unset | Bearer token the bridge requires on `Authorization`. Unset means unauthenticated — only legitimate for a loopback/local deployment. |
| `heartbeat_after_seconds` | `CLAUDE_CODE_BRIDGE_HEARTBEAT_AFTER_SECONDS` | `20` | Advertised in the §13.3 acceptance, and the interval the poller sends a `heartbeat` callback absent other activity. |
| `poll_interval_seconds` | `CLAUDE_CODE_BRIDGE_POLL_INTERVAL_SECONDS` | `0.15` | How often the async poller checks the flight feed. |
| `callback_timeout_seconds` | `CLAUDE_CODE_BRIDGE_CALLBACK_TIMEOUT_SECONDS` | `10.0` | Per-request timeout posting a callback event. |
| `callback_max_retries` | `CLAUDE_CODE_BRIDGE_CALLBACK_MAX_RETRIES` | `5` | Retries (with the SAME event id/sequence) on a non-2xx or unreachable callback delivery. |
| `callback_retry_backoff_seconds` | `CLAUDE_CODE_BRIDGE_CALLBACK_RETRY_BACKOFF_SECONDS` | `0.25` | Linear backoff step between callback retries. |
| `sync_timeout_seconds` | `CLAUDE_CODE_BRIDGE_SYNC_TIMEOUT_SECONDS` | `300.0` | Bounds one foreground `claude -p` call. On expiry: SIGTERM (never SIGKILL), then `408`. |
| `background_dispatch_timeout_seconds` | `CLAUDE_CODE_BRIDGE_BACKGROUND_DISPATCH_TIMEOUT_SECONDS` | `30.0` | Defensive ceiling on spawning the detached background process. |
| `async_wait_seconds` | `CLAUDE_CODE_BRIDGE_ASYNC_WAIT_SECONDS` | `3600.0` | Overall ceiling the poller waits for a detached run before reporting a timeout failure. |
| `state_dir` | `CLAUDE_CODE_BRIDGE_STATE_DIR` | `.claude-code-bridge-state` | Where the idempotency replay store and flight files live. |

Point the process at a config file with
`CLAUDE_CODE_BRIDGE_CONFIG=/path/to/bridge.json` or
`claude-code-bridge --config /path/to/bridge.json`. Example:

```json
{
  "repo_allowlist": ["/srv/repos/example-app"],
  "claude_bin": "claude",
  "permission_mode": "bypassPermissions",
  "auth_token": "replace-me",
  "port": 8086
}
```

### Invocation input fields this bridge recognizes

| Field | Required | Meaning |
|---|---|---|
| `instruction` | yes | The prompt passed to `claude -p`. `400` without it. |
| `repo` | yes | Absolute path; must be in `repo_allowlist` or the invocation is refused `403`. |
| `role` | no | Passed as `--agent`. Validated against `.claude/agents/<role>.md` in the target repo (claude ships no built-in role set the way colleague does); unknown role is `400`. |
| `max_steps` | no | Passed as `--max-turns`; also the "expected duration" signal for the sync/async threshold. |
| `model` | no | Passed as `--model` (falls back to `Config.model`). |
| `success_outcome` | no | Domain outcome reported on success (default: `default_success_outcome`). |
| `incomplete_outcome` | no | Domain outcome reported for `subtype: "error_max_turns"`, **only if the node declares one here**. Absent: reported as an execution failure, never as success. |
| `async` | no | Force sync (`false`) or async (`true`) dispatch, overriding the step-budget threshold. |

## Trust model: `proposed`-only

Identical stance to `adapters/colleague`: this bridge **never emits
`confirmed`/`observed`/`derived`** ledger authority. Every
`ledger_delta.records[]` entry is a single `claim` record, `authority:
"proposed"`, `origin.kind: "agent"` — claude's own final answer is a
**completion claim**, not verified evidence (PRD §10.5; this repo's own
CLAUDE.md ledger-authority-model rule — no actor promotes its own
proposal).

## Running it

```bash
uv run --project adapters/claude-code claude-code-bridge --config /path/to/bridge.json
# or, without installing the console script:
uv run --project adapters/claude-code python -m claude_code_bridge --config /path/to/bridge.json
```

## Tests

```bash
uv run --project adapters/claude-code pytest
```

Unlike colleague (whose `COLLEAGUE_ENGINE=mock` gives it a free, offline,
deterministic backend), the real `claude` CLI has no offline mock mode — it
always talks to a real, billed Anthropic backend. So this suite fakes the
`claude` subprocess itself (`tests/fake_claude.py`, a small stdlib-only
script standing in for the real binary — see its own docstring), the way
colleague's server-level unit tests monkeypatch `colleague_cli` directly.
Every test in this suite, including `test_integration_bridge.py`'s
real-subprocess-level tests, runs against that fake and needs no network
access, no API key, and costs nothing. A live smoke test against a real,
authenticated local `claude` install is optional and NOT part of this
suite — see `scripts/run_conformance_kit.sh`'s own note on why CI runs
against the fake too.

### Running the PRD §13 conformance kit against this bridge

`scripts/run_conformance_kit.sh` starts the bridge with `claude_bin`
pointed at `tests/fake_claude.py`, then runs culture-nodes's own
`tests/conformance` kit (`go test ./tests/conformance -args
-endpoint=http://127.0.0.1:<port> ...`) against it — the acceptance check
for this whole package, and what `.github/workflows/adapter-claude-code.yml`
runs in CI. See the script for the exact flags; it requires `go` and `uv`
only (no real `claude` install, no API key — see above).
