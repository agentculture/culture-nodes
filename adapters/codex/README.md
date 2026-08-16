# culture-nodes-codex-bridge

A **reference** implementation of the Culture Nodes actor protocol
(PRD §13, `internal/actors/protocol.go`) over headless `codex exec`
subprocess dispatch. It mirrors `adapters/colleague`'s own layout and
discipline field for field, so an operator who has read one has effectively
read both, and exists to prove the actor protocol against a second, real
agent backend — not to be the production actor host for every deployment.

This package is deliberately **separate** from the `culture-nodes` Python
package and from `adapters/colleague`: its own `pyproject.toml`, its own
dependency group, its own test suite. It is not installed as part of a
culture-nodes deployment.

## Deployment model

```text
   Culture Nodes control plane                 Agent host (this bridge)
   ┌─────────────────────────┐   HTTPS POST    ┌──────────────────────────┐
   │ engine dispatches an     │ ───────────────▶│ codex-bridge               │
   │ attempt to an actor      │                 │  (this package)           │
   │ endpoint (internal/      │◀─────────────── │  --repo <allowlisted>     │
   │ actors.Client)           │   callbacks      │  shells out to `codex     │
   └─────────────────────────┘   (PRD §13.4)     │  exec ...`                │
                                                  └──────────┬───────────────┘
                                                             │ subprocess
                                                             ▼
                                                  ┌──────────────────────────┐
                                                  │ codex CLI (codex-cli),    │
                                                  │ authenticated against a   │
                                                  │ ChatGPT/API account        │
                                                  └──────────────────────────┘
```

The bridge runs **beside a `codex` CLI install, on the machine (or
container) that can run `codex exec`** — not inside the culture-nodes
control-plane process, and never through a shell/Docker socket the
control-plane process reaches into (PRD's headspace-cli boundary is a
*separate*, code-execution-specific runner; this bridge is itself an actor
the control plane invokes over the network, exactly like `colleague-bridge`
or any other actor adapter would be). An operator:

1. Provisions a host (or container image) with `codex` installed and
   authenticated (`codex login`; this bridge never manages credentials
   itself — it inherits whatever `$CODEX_HOME`/ChatGPT-or-API auth the host
   process has).
2. Checks out (or mounts) the repositories this bridge is allowed to
   dispatch work into. Each must already be a git checkout — `codex exec`
   itself requires one (it refuses to run outside a git repo unless told
   `--skip-git-repo-check`, which this bridge never passes).
3. Configures the bridge (below) with that repo allowlist, starts it, and
   registers its base URL + a scoped workload token as an actor endpoint in
   Culture Nodes.

Culture Nodes reaches the bridge over the network exactly as it would reach
any other actor (`internal/actors.Client.Invoke`/`Cancel`); the bridge
reaches `codex` as a local subprocess, never the reverse.

## Codex contract pin + the exact argv this bridge generates

Pinned against **codex-cli 0.144.6** (`codex exec --help`, and this
bridge's own grounding runs against a real, authenticated `codex exec
--json` — see `src/codex_bridge/codex_cli.py` module docstring for the raw
JSONL this bridge observed and built its classifier against):

```text
codex exec --json --sandbox <read-only|workspace-write|danger-full-access> -C <repo> [-m <model>] "<instruction>"
```

* `exec` — non-interactive mode (`codex exec [OPTIONS] [PROMPT]`).
* `--json` — print events to stdout as JSONL (`{"type": ...}` per line);
  this bridge never uses the interactive TUI.
* `--sandbox` — always explicit, always one of codex's three declared
  values (`Config.default_sandbox`, default `workspace-write`, or
  `input.sandbox` when the invocation names one). This bridge never passes
  `--dangerously-bypass-approvals-and-sandbox`.
* `-C <repo>` — "use this directory as the working root"; paired with
  `cwd=repo` on the subprocess itself, belt-and-suspenders, the same way
  `adapters/colleague` pairs `--repo PATH` with `cwd=repo`.
* `-m <model>` — only when `input.model` is given; codex's own model
  catalog changes over time and this bridge does not validate it — an
  unknown model is codex's own `turn.failed` to report (see below), which
  this bridge maps like any other execution failure, not a bridge-level
  400.
* The instruction is the trailing positional `PROMPT`. `stdin` is always
  `DEVNULL` on the subprocess, so codex's own "stdin is appended as a
  `<stdin>` block" behavior never fires unexpectedly.

There is no `--max-steps` equivalent: codex exec has no CLI flag to bound
its own step/turn count (unlike colleague's `--max-steps`). `input.max_steps`
is accepted and still drives the sync/async dispatch **threshold**
(`Config.sync_max_steps`, same decision colleague-bridge makes) but is never
forwarded to `codex`. Duration is instead bounded purely by
`sync_timeout_seconds`/`async_wait_seconds` + a cooperative SIGTERM, exactly
like colleague-bridge's own timeout fallback (see below).

## Session resume (task t5)

When a request carries a top-level `continuation_ref` (§13.1,
`internal/actors/protocol.go` — a sibling of `run_id`, NOT nested inside
`input`), this bridge resumes that prior session with codex's own
**separate subcommand**, `codex exec resume <continuation_ref> --json
[-m <model>] "<instruction>"` — verified against `codex exec resume
--help` on codex-cli 0.147.0. `resume`'s flag surface is narrower than
plain `exec`'s: it accepts neither `-C`/`--cd` nor `-s`/`--sandbox` (a
resumed session already knows its working directory and sandbox policy
from when it first started), so a resumed dispatch's argv is not simply
`exec` with `resume` inserted. On a successful turn, codex's own captured
thread id (`thread.started`'s `thread_id`) rides back as `continuation_ref`
in both the §13.2 result body and the §13.4 `completed` event.

`session_key` and `continuation_ref` are both **transport keys**: neither
is ever forwarded into the instruction text handed to codex.

## Capacity refusals (task t5, deviation d4)

When codex's own `turn.failed` error text (or a standalone `error` event's
message) names a provider-side quota, rate-limit, or session-limit
refusal, this bridge classifies the failure `capacity_exhausted` (§13.5)
instead of plain `execution`, and sets an HTTP `Retry-After` header on the
synchronous failure response when the text names a delay. See
`adapters/claude-code/README.md`'s identical section for the full
rationale — this bridge's own `_CAPACITY_SIGNALS` list is deliberately
kept in step with that one.

## What a codex session's JSONL looks like (grounding evidence)

This bridge's classification rules were built against real output from an
authenticated `codex exec --json` run on the machine this adapter was
developed on, not guessed from documentation alone. Three shapes matter:

**A normal completion** (`exit 0`):

```jsonl
{"type":"thread.started","thread_id":"019fe54f-..."}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"OK"}}
{"type":"turn.completed","usage":{"input_tokens":13880,"cached_input_tokens":9984,"output_tokens":5,"reasoning_output_tokens":0}}
```

**A rejected/failed turn** (`exit 1`, e.g. an unrecognised model):

```jsonl
{"type":"thread.started","thread_id":"019fe54f-..."}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Model metadata for `...` not found. ..."}}
{"type":"turn.started"}
{"type":"error","message":"{\"type\":\"error\",\"status\":400,...}"}
{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"status\":400,...}"}}
```

**A killed/crashed session — the load-bearing case for this task's
acceptance criterion** (SIGTERM delivered mid-turn; observed exit code
**0**, job control even reported the process as cleanly `Done`):

```jsonl
{"type":"thread.started","thread_id":"019fe553-..."}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'll run all six sequentially..."}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'sleep 3'","aggregated_output":"","exit_code":null,"status":"in_progress"}}
```

— stream ends there. **No `turn.completed`, no `turn.failed`.** This is the
concrete, measured proof behind this bridge's central rule: `exit_code == 0`
is *not* sufficient evidence of success. Only the presence of an explicit
`turn.completed` event (with no `turn.failed` before it) counts as a
completed turn; anything else — a `turn.failed` event, or a subprocess that
exited (for any reason, any exit code, including a clean-looking `0`)
without ever reaching a terminal turn event — is classified `incomplete`,
which `mapping.classify()` (mirroring colleague-bridge's own module,
verbatim in spirit) reports as an **execution failure**, never success,
unless the invocation itself declared an `incomplete_outcome` domain
outcome. No adapter-specific exemption exists for this rule; see
`tests/test_codex_cli.py::test_parse_session_exit_zero_without_terminal_event_is_incomplete_never_ok`
and the crash-mapping tests in `tests/test_server_unit.py` /
`tests/test_mapping.py`.

`codex_cli.parse_session()` is the module that turns a full captured JSONL
transcript into a `{"status": "ok"|"error"|"incomplete", ...}` dict shaped
exactly like colleague's own `TaskResult` (`status`, `summary`,
`changed_files`, `usage`, `task_id`, `error`) — the one deliberate seam that
lets `mapping.py` be almost a verbatim port of colleague-bridge's own
module: the wire protocol this bridge speaks to Culture Nodes, and the
`ok`/`error`/`incomplete` vocabulary it classifies against, are identical;
only how that three-way status gets derived from the underlying agent
backend differs (colleague: read `TaskResult.status` off the last stdout
line; codex: scan every JSONL line for a terminal `turn.completed` /
`turn.failed` event, defaulting to `incomplete` when neither ever arrives).

`changed_files` is collected from codex's own `item.completed` events where
`item.type == "file_change"` and `item.status == "completed"` — a
best-effort, self-reported list (codex may also change files via a shell
tool call it does not also report as a `file_change` item), exactly as
honest and exactly as unverified as colleague's own `changed_files` field.
Neither bridge's output is evidence; both are `proposed` claims (see "Trust
model" below).

## Configuration

Env-first, with an optional small JSON file underneath — same precedence
rule as `adapters/colleague`'s `config.py`: file sets the baseline,
`CODEX_BRIDGE_*` env vars override individual fields.

| Config file key | Env var | Default | Meaning |
|---|---|---|---|
| `repo_allowlist` | `CODEX_BRIDGE_REPO_ALLOWLIST` (`:`-joined) | `[]` | Absolute repo paths this bridge will dispatch into. A request naming any other `input.repo` is refused `403`. **Empty means the bridge accepts no repo** — the safe default. |
| `repo_identities` | `CODEX_BRIDGE_REPO_IDENTITIES` (`:`-joined `name=path`) | `{}` | Maps a **repository identity** (the name the control plane sends from the actor's `metadata.repository_identity`) to a checkout on this host. Only needed when a checkout directory is not named after the repository it holds — otherwise the name is inferred from `repo_allowlist`. A declaration says *which* repository, never that the bridge may touch it: the resolved path still has to pass `repo_allowlist`. See [Repository identity](#repository-identity-issue-125). |
| `codex_bin` | `CODEX_BRIDGE_CODEX_BIN` | `"codex"` | Path/name of the codex executable (resolved via `PATH` if bare). |
| `codex_env` | — (file only) | `{}` | Extra env vars merged onto every codex subprocess (e.g. `CODEX_HOME` to point at a specific auth profile). Operator-supplied only — the bridge never invents a value here. |
| `default_sandbox` | `CODEX_BRIDGE_DEFAULT_SANDBOX` | `"workspace-write"` | The `--sandbox` value used when `input.sandbox` is absent. One of `read-only`, `workspace-write`, `danger-full-access`. |
| `sync_max_steps` | `CODEX_BRIDGE_SYNC_MAX_STEPS` | `6` | Dispatch threshold: an expected step budget above this goes async. Not forwarded to codex (no native flag — see above). |
| `default_max_steps` | `CODEX_BRIDGE_DEFAULT_MAX_STEPS` | `6` | Assumed step budget when `input.max_steps` is absent, for the threshold comparison above. |
| `always_async` | `CODEX_BRIDGE_ALWAYS_ASYNC` | `false` | Force every invocation asynchronous, ignoring the threshold and `input.async`. |
| `default_success_outcome` | `CODEX_BRIDGE_DEFAULT_SUCCESS_OUTCOME` | `"completed"` | Domain outcome for `status: ok` when `input.success_outcome` is absent. |
| `actor_id` | `CODEX_BRIDGE_ACTOR_ID` | `"codex-bridge"` | `origin.actor_id` on every proposed ledger record. |
| `host` | `CODEX_BRIDGE_HOST` | `"127.0.0.1"` | Bind address. |
| `port` | `CODEX_BRIDGE_PORT` | `8086` | Bind port (colleague-bridge's default is `8085`; different by default so both can run on one host without colliding). `0` picks a free port (useful for tests). |
| `auth_token` | `CODEX_BRIDGE_AUTH_TOKEN` | unset | Bearer token the bridge requires on `Authorization`. Unset means unauthenticated — only legitimate for a loopback/local deployment. |
| `heartbeat_after_seconds` | `CODEX_BRIDGE_HEARTBEAT_AFTER_SECONDS` | `20` | Advertised in the §13.3 acceptance, and the interval the poller sends a `heartbeat` callback absent other activity. |
| `poll_interval_seconds` | `CODEX_BRIDGE_POLL_INTERVAL_SECONDS` | `0.15` | Granularity of the async runner's wait-for-output-or-heartbeat loop over the live codex subprocess's stdout pipe. |
| `callback_timeout_seconds` | `CODEX_BRIDGE_CALLBACK_TIMEOUT_SECONDS` | `10.0` | Per-request timeout posting a callback event. |
| `callback_max_retries` | `CODEX_BRIDGE_CALLBACK_MAX_RETRIES` | `5` | Retries (with the SAME event id/sequence) on a non-2xx or unreachable callback delivery. |
| `callback_retry_backoff_seconds` | `CODEX_BRIDGE_CALLBACK_RETRY_BACKOFF_SECONDS` | `0.25` | Linear backoff step between callback retries. |
| `sync_timeout_seconds` | `CODEX_BRIDGE_SYNC_TIMEOUT_SECONDS` | `300.0` | Bounds one foreground `codex exec` call. On expiry: SIGTERM (never SIGKILL), then a timeout response. |
| `async_wait_seconds` | `CODEX_BRIDGE_ASYNC_WAIT_SECONDS` | `3600.0` | Overall ceiling the async runner waits for a codex subprocess to finish before SIGTERM + reporting a timeout failure. |
| `state_dir` | `CODEX_BRIDGE_STATE_DIR` | `.codex-bridge-state` | Where the idempotency replay store lives. |

Point the process at a config file with `CODEX_BRIDGE_CONFIG=/path/to/bridge.json`
or `codex-bridge --config /path/to/bridge.json`. Example:

```json
{
  "repo_allowlist": ["/srv/repos/example-app"],
  "codex_bin": "codex",
  "codex_env": {"CODEX_HOME": "/srv/codex-bridge/.codex"},
  "default_sandbox": "workspace-write",
  "auth_token": "replace-me",
  "port": 8086
}
```

### Invocation input fields this bridge recognizes

The actor protocol's `input` is opaque JSON (node-contract-defined); this
bridge's own contract for it is:

| Field | Required | Meaning |
|---|---|---|
| `instruction` | yes | The prompt passed to `codex exec` as its trailing positional `PROMPT`. `400` without it. |
| `repo` | no | Absolute path; must be in `repo_allowlist` or the invocation is refused `403`. Must already be a git checkout — this bridge never passes `--skip-git-repo-check`. Optional since issue #125: when absent, the checkout is resolved from `repository_identity`, and only then from a single-entry `repo_allowlist`. |
| `repository_identity` | no | Set by the **control plane**, from the actor's registration, and never by a workflow author or an event payload. Resolved to a checkout on this host when `repo` is absent. A name that reaches two permitted checkouts, or none, is a named `400` (`repository_identity_ambiguous` / `repository_identity_unknown`) with a hint — never a silently-picked first match. |
| `model` | no | Passed as `-m`. Not validated against a fixed catalog (codex's own model list changes); an unrecognised model is codex's own `turn.failed`, mapped like any other execution failure. |
| `sandbox` | no | Passed as `--sandbox`; must be one of `read-only`, `workspace-write`, `danger-full-access` or the invocation is refused `400`. Defaults to `Config.default_sandbox`. |
| `max_steps` | no | **Not** forwarded to codex (no native flag) — only the sync/async dispatch threshold signal, matching colleague-bridge's own field name and semantics for that one purpose. |
| `success_outcome` | no | Domain outcome reported for `status: ok` (default: `default_success_outcome`). |
| `incomplete_outcome` | no | Domain outcome reported for `status: incomplete`, **only if the node declares one here**. Absent: an incomplete or crashed run is reported as an execution failure, never as success. |
| `async` | no | Force sync (`false`) or async (`true`) dispatch, overriding the step-budget threshold. |

Unlike colleague-bridge, there is no `role`/`mode` field: codex exec has no
persona/mode concept to map onto, so this bridge does not invent one.

## Repository identity (issue #125)

A trigger-created run's `input` *is* the event payload, and a payload carries
no checkout path. The control plane therefore sends the actor's registered
repository **identity** — a name, from `actor.metadata.repository_identity`
and from nowhere else — and this bridge maps it to a directory, because a
path chosen on the control-plane host need not exist here.

The mapping is tried in two steps:

1. **Declared** — `repo_identities[identity]`, the operator's own statement.
2. **Inferred** — the identity's repository segment
   (`agentculture/culture-nodes` → `culture-nodes`) matched against the final
   component of every path this bridge may work in: each `repo_allowlist`
   entry, and `<root>/<name>` for each `repo_allowlist_prefixes` root that
   actually holds such a directory.

Then the survivor is validated against `repo_allowlist` like any other repo.
Ambiguity and misses are named, hinted refusals, mirroring how
`only_allowed_repo()` already fails closed — and `repo_allowlist` goes back to
being a pure **permission** surface, free to hold as many entries as the host
needs.

An explicit `input.repo` still wins, and an actor registered with no identity
dispatches exactly as before: the single-allowlist-entry inference is
unchanged.

## Preflight capability surface (issue #67)

This bridge measures the host it dispatches on and serves the result at
`GET /v1/capabilities` (authenticated, like the invocation route), or prints
the same document without a server:

```bash
uv run codex-bridge --print-capabilities
curl -sH "Authorization: Bearer $CODEX_BRIDGE_AUTH_TOKEN" http://127.0.0.1:8086/v1/capabilities
```

The document is exactly what an actor registration carries in
`capabilities`. What codex contributes to it:

| Fact | Where it comes from |
|---|---|
| `sandbox_modes` | Which of `read-only` / `workspace-write` / `danger-full-access` this host can **actually** enforce |
| `sandbox_modes_unavailable` | Modes it cannot, and why — measured from the userns sysctls, not read from config |
| `default_sandbox_mode` | `default_sandbox` (what a dispatch that names no sandbox gets) |
| `confinement` | Whether the bubblewrap helper codex's confinement rests on can start here at all |
| `commit_policy` | The `preserve_on_failure` / `preserve_push` / `preserve_remote` policy in force |
| `writable_paths` | `repo_allowlist` — `[]` means this bridge writes nowhere |
| `dispatch_grants` | What each `--sandbox` mode actually grants a session — writes, egress, the ability to start a nested confinement helper (issue #96) |
| `toolchains` | `uv`, `go`, `gh` and `codex` itself: where each is, how it was packaged, what version it reports, and **which modes can actually run it** |

The measurement is the point. Issues #18/#63: `--sandbox workspace-write`
was requested on three hosts whose kernel restricted unprivileged user
namespaces, the confinement helper could not start, every `apply_patch`
failed and shell commands kept running unconfined. A surface that echoed
`default_sandbox` would have advertised `workspace-write` and been wrong.
This one reports it under `sandbox_modes_unavailable` with the sysctl that
says so.

The same reasoning produced `toolchains` (issue #96). A surface reporting
`uv: present` was true on thor and on orin and useless on both: thor's uv is
a snap whose own snap-confine cannot start inside codex's sandbox (run
`01M03374VAKH0KHN0GDZ466NP4`), orin's is a standalone binary that dies
initialising a cache under a read-only `$HOME` (run
`01M0342X60F3NY8MH150G48AZ6`), and neither ran a test suite. `gh` is sharper
still: `gh auth status` over ssh on thor reports logged in while a dispatch
on that host reaches neither api.github.com nor pypi.org (run
`01M039NZ2TZYFG68YZT93A6DC7`) — a true fact about the host, a false one
about the dispatch. The baseline those findings are pinned to, and the
re-check that notices a codex-cli bump invalidating them, are
`docs/baselines/2026-08-15-agent-host-toolchains.md` and
`scripts/toolchain-baseline.sh`.

The protocol is engine-side and the facts are bridge-side. The shared,
byte-identical `preflight.py` in every bridge holds the protocol, the agreed
key set and the measurement helpers; `capabilities.py` holds only what THIS
backend measures. `tests/lint/preflightsurface_test.go` fails the build if
those two ever swap roles. The full contract is
[`api/actor-protocol/README.md`](../../api/actor-protocol/README.md); the
engine half (the gate, the derived briefing, the acknowledgement, `nodes
dispatch confirm`) is `internal/preflight`.

Advertising changes nothing on its own: the gate that reads the surface is
per-actor and default-off, and this bridge dispatches exactly as it did
before whether or not an operator ever registers the block.

## Trust model: `proposed`-only

This bridge **never emits `confirmed`/`observed`/`derived`** ledger
authority — identical stance to `adapters/colleague`. Every
`ledger_delta.records[]` entry it produces is a single `claim` record,
`authority: "proposed"`, `origin.kind: "agent"`: codex's own final message is
a **completion claim**, not verified evidence (PRD §10.5, and this repo's
own `CLAUDE.md` ledger-authority-model rule — no actor promotes its own
proposal). This bridge's own grounding runs made the honesty of that stance
concrete: in a broken-sandbox environment, codex reported `turn.completed`
(`exit 0`, a clean "I did it" agent message) while its own attempted direct
file write had in fact failed and it had silently fallen back to a shell
tool to get the same result — a real, observed case of "the agent said it
succeeded" needing independent confirmation before anyone treats it as
verified. A human `confirm`/`reject`s it, a trusted runner would have to
directly measure a fact to write `observed`, and a deterministic validator
would have to compute one to write `derived` — none of which this bridge is
positioned to do on codex's behalf. Whether `status: ok` becomes the node's
`completed` outcome or something else is a **domain outcome** decision the
invoking node's contract makes (via `input.success_outcome`), never a
technical/engine verdict this bridge invents.

## Running it

```bash
uv run --project adapters/codex codex-bridge --config /path/to/bridge.json
# or, without installing the console script:
uv run --project adapters/codex python -m codex_bridge --config /path/to/bridge.json
```

## Tests

```bash
uv run --project adapters/codex pytest
```

Unit tests need no `codex` binary at all — `codex_cli.parse_session()` (the
JSONL-to-TaskResult classifier) is tested against literal transcript
fixtures, and `server.py`'s request-handling ladder is tested with
`codex_cli.run_sync`/`codex_cli.spawn` monkeypatched out, exactly mirroring
`adapters/colleague`'s own unit-test discipline. The one exception —
`tests/test_codex_cli.py`'s `TestRunSyncAgainstAFakeExecutable` — spawns a
**fake** `codex`-shaped executable (a tiny script this test suite writes to
a temp dir and points `Config.codex_bin` at) to exercise the real
`subprocess.Popen`/SIGTERM-on-timeout code path without needing the real
CLI, so that path is not left untested-in-CI the way colleague-bridge's
own `run_sync` timeout handling only gets covered when a real `colleague`
binary happens to be on `PATH`.

The integration test (`tests/test_integration_bridge.py`) shells out to a
REAL, authenticated `codex exec` in a throwaway scratch git repo; it skips
(never fails) when `codex` is not on `PATH` or not logged in (`codex login
status`), so the unit suite is always runnable without it, and CI never
depends on it (see "For CI" below).

### Running the PRD §13 conformance kit against this bridge

`scripts/run_conformance_kit.sh` starts the bridge against a scratch repo,
then runs culture-nodes's own `tests/conformance` kit (`go test
./tests/conformance -args -endpoint=http://127.0.0.1:<port> ...`) against
it — the acceptance check for this whole package, run **unmodified**
(the same kit `adapters/colleague` runs itself against). See the script for
the exact flags; it requires `go` and a real, authenticated `codex` install
on `PATH` — unlike colleague's `COLLEAGUE_ENGINE=mock`, codex has no offline
deterministic engine, so running this script dispatches real (billable)
codex invocations. It is a local/manual verification tool, not a CI step.

## For CI

`.github/workflows/adapter-codex.yml` runs this package's own `uv sync` +
`uv run pytest` (fakes only, no real `codex` — see "Tests" above) plus
black/isort/flake8/bandit against `adapters/codex/src` and
`adapters/codex/tests` using the root project's lint configuration (the
same tools `tests.yml`'s `lint` job runs against `culture_nodes`;
`adapters/colleague` has no dedicated lint config of its own either — this
mirrors that same minimal footprint). It is a workflow file dedicated to
this adapter, added without touching any existing workflow file.
