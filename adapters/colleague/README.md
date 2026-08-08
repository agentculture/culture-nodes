# culture-nodes-colleague-bridge

A **reference** implementation of the Culture Nodes actor protocol
(PRD §13, `internal/actors/protocol.go`) over `colleague work` subprocess
dispatch. It exists to prove the actor protocol against a real agent
backend, and to give the next adapter author seventy files of working code
to read instead of prose alone.

This package is deliberately **separate** from the `culture-nodes` Python
package: its own `pyproject.toml`, its own dependency group, its own test
suite. It is not installed as part of a culture-nodes deployment.

## Deployment model

```
   Culture Nodes control plane                 Agent host (this bridge)
   ┌─────────────────────────┐   HTTPS POST    ┌──────────────────────────┐
   │ engine dispatches an     │ ───────────────▶│ colleague-bridge          │
   │ attempt to an actor      │                 │  (this package)           │
   │ endpoint (internal/      │◀─────────────── │  --repo <allowlisted>     │
   │ actors.Client)           │   callbacks      │  shells out to `colleague │
   └─────────────────────────┘   (PRD §13.4)     │  work ...`               │
                                                  └──────────┬───────────────┘
                                                             │ subprocess
                                                             ▼
                                                  ┌──────────────────────────┐
                                                  │ colleague checkout +      │
                                                  │ its configured engine     │
                                                  │ (vLLM, mock, ...)         │
                                                  └──────────────────────────┘
```

The bridge runs **beside a `colleague` checkout, on the machine (or
container) that can run `colleague work`** — not inside the culture-nodes
control-plane process, and never through a shell/Docker socket the
control-plane process reaches into (PRD's headspace-cli boundary is a
*separate*, code-execution-specific runner; this bridge is itself an actor
the control plane invokes over the network, exactly like any other actor
adapter would be). An operator:

1. Provisions a host (or container image — see [Dockerfile](#dockerfile))
   with `colleague` installed and its engine reachable (a vLLM server, or
   `COLLEAGUE_ENGINE=mock` for a deterministic offline actor useful in CI).
2. Checks out (or mounts) the repositories this bridge is allowed to
   dispatch work into.
3. Configures the bridge (below) with that repo allowlist, starts it, and
   registers its base URL + a scoped workload token as an actor endpoint in
   Culture Nodes.

Culture Nodes reaches the bridge over the network exactly as it would reach
any other actor (`internal/actors.Client.Invoke`/`Cancel`); the bridge
reaches `colleague` as a local subprocess, never the reverse.

## Colleague contract pin + upgrade policy

Pinned against **colleague contract v1**
(`/home/spark/git/colleague/docs/contract.md`, read-only reference at the
time this bridge was built): `colleague work "<instruction>" --repo PATH
--json [--background] [--role r] [--mode m] [--max-steps N]`;
`TaskResult.status` `ok`|`error`|`incomplete` maps to exit codes `0`|`1`|`2`;
`--background` prints `{background, id, pid, log_dir, flight}` and detaches;
the flight control plane lives at `.colleague/flight/<task_id>.{feed.jsonl,
control.json}`; artifacts land at `.colleague/<task_id>.<slug>.json`.

The contract's own versioning policy (`docs/contract.md` "Versioning
policy") says a new *optional* artifact key is a minor bump and this bridge
only ever reads a handful of always-on top-level keys (`status`, `summary`,
`changed_files`, `usage`, `artifacts_path`, `error`, `task_id`) plus the
documented `--background` start payload and flight file shapes — so a minor
colleague release should never break this bridge. A **major** bump (removed/
renamed key, changed exit-code meaning) is exactly the kind of change that
would: re-read `docs/contract.md` at the new version and re-verify
`mapping.py`'s `classify()` before upgrading the pinned colleague version
this bridge is deployed against. There is no automated contract-drift test
here (that lives in colleague's own repo, `tests/test_contract_doc.py`) —
this bridge's own integration test (`tests/test_integration_bridge.py`)
running against a real `colleague work` invocation is the practical signal
that the pin still holds.

## Configuration

Env-first, with an optional small JSON file underneath (`config.py`'s own
docstring has the full precedence rule: file sets the baseline, then
`COLLEAGUE_BRIDGE_*` env vars override individual fields).

| Config file key | Env var | Default | Meaning |
|---|---|---|---|
| `repo_allowlist` | `COLLEAGUE_BRIDGE_REPO_ALLOWLIST` (`:`-joined) | `[]` | Absolute repo paths this bridge will dispatch into. A request naming any other `input.repo` is refused `403`. **Empty means the bridge accepts no repo** — the safe default. |
| `colleague_bin` | `COLLEAGUE_BRIDGE_COLLEAGUE_BIN` | `"colleague"` | Path/name of the colleague executable (resolved via `PATH` if bare). |
| `colleague_env` | — (file only) | `{}` | Extra env vars merged onto every colleague subprocess (`COLLEAGUE_ENGINE`, `COLLEAGUE_MODEL`, `COLLEAGUE_BASE_URL`, `COLLEAGUE_API_KEY`, `COLLEAGUE_LOBES_URL`, ...). Operator-supplied only — the bridge never invents a value here. |
| `open_pr` | `COLLEAGUE_BRIDGE_OPEN_PR` | `false` | Pass `--no-pr` unless true. A headless actor bridge defaults to local-commit-only. |
| `allow_dirty` | `COLLEAGUE_BRIDGE_ALLOW_DIRTY` | `false` | Forwarded as `--allow-dirty`. |
| `sync_max_steps` | `COLLEAGUE_BRIDGE_SYNC_MAX_STEPS` | `6` | Dispatch threshold: an expected step budget above this goes async. |
| `default_max_steps` | `COLLEAGUE_BRIDGE_DEFAULT_MAX_STEPS` | `6` | Assumed step budget when `input.max_steps` is absent, for the threshold comparison above. |
| `always_async` | `COLLEAGUE_BRIDGE_ALWAYS_ASYNC` | `false` | Force every invocation asynchronous, ignoring the threshold and `input.async`. |
| `default_success_outcome` | `COLLEAGUE_BRIDGE_DEFAULT_SUCCESS_OUTCOME` | `"completed"` | Domain outcome for `status: ok` when `input.success_outcome` is absent. |
| `actor_id` | `COLLEAGUE_BRIDGE_ACTOR_ID` | `"colleague-bridge"` | `origin.actor_id` on every proposed ledger record. |
| `host` | `COLLEAGUE_BRIDGE_HOST` | `"127.0.0.1"` | Bind address. |
| `port` | `COLLEAGUE_BRIDGE_PORT` | `8085` | Bind port. `0` picks a free port (useful for tests). |
| `auth_token` | `COLLEAGUE_BRIDGE_AUTH_TOKEN` | unset | Bearer token the bridge requires on `Authorization`. Unset means unauthenticated — only legitimate for a loopback/local deployment. |
| `heartbeat_after_seconds` | `COLLEAGUE_BRIDGE_HEARTBEAT_AFTER_SECONDS` | `20` | Advertised in the §13.3 acceptance, and the interval the poller sends a `heartbeat` callback absent other activity. |
| `poll_interval_seconds` | `COLLEAGUE_BRIDGE_POLL_INTERVAL_SECONDS` | `0.15` | How often the async poller checks the flight feed / background result. |
| `callback_timeout_seconds` | `COLLEAGUE_BRIDGE_CALLBACK_TIMEOUT_SECONDS` | `10.0` | Per-request timeout posting a callback event. |
| `callback_max_retries` | `COLLEAGUE_BRIDGE_CALLBACK_MAX_RETRIES` | `5` | Retries (with the SAME event id/sequence) on a non-2xx or unreachable callback delivery. |
| `callback_retry_backoff_seconds` | `COLLEAGUE_BRIDGE_CALLBACK_RETRY_BACKOFF_SECONDS` | `0.25` | Linear backoff step between callback retries. |
| `sync_timeout_seconds` | `COLLEAGUE_BRIDGE_SYNC_TIMEOUT_SECONDS` | `300.0` | Bounds one foreground `colleague work` call. On expiry: SIGTERM (never SIGKILL), then `408`. |
| `background_dispatch_timeout_seconds` | `COLLEAGUE_BRIDGE_BACKGROUND_DISPATCH_TIMEOUT_SECONDS` | `30.0` | Defensive ceiling on the (normally near-instant) `--background` parent call. |
| `async_wait_seconds` | `COLLEAGUE_BRIDGE_ASYNC_WAIT_SECONDS` | `3600.0` | Overall ceiling the poller waits for a detached run before reporting a timeout failure. |
| `state_dir` | `COLLEAGUE_BRIDGE_STATE_DIR` | `.colleague-bridge-state` | Where the idempotency replay store lives. |

Point the process at a config file with `COLLEAGUE_BRIDGE_CONFIG=/path/to/bridge.json`
or `colleague-bridge --config /path/to/bridge.json`. Example:

```json
{
  "repo_allowlist": ["/srv/repos/example-app"],
  "colleague_bin": "colleague",
  "colleague_env": {"COLLEAGUE_ENGINE": "vllm-openai", "COLLEAGUE_BASE_URL": "http://127.0.0.1:8000/v1"},
  "auth_token": "replace-me",
  "port": 8085
}
```

### Invocation input fields this bridge recognizes

The actor protocol's `input` is opaque JSON (node-contract-defined); this
bridge's own contract for it is:

| Field | Required | Meaning |
|---|---|---|
| `instruction` | yes | The goal/instruction passed to `colleague work`. `400` without it. |
| `repo` | yes | Absolute path; must be in `repo_allowlist` or the invocation is refused `403`. |
| `role` | no | Passed as `--role`. Validated against colleague's built-in roles (`explorer`, `planner`, `reviewer`, `validator`, `writer`) or a `.colleague/agents/<role>.md` override in the target repo; unknown role is `400`. |
| `max_steps` | no | Passed as `--max-steps`; also the "expected duration" signal for the sync/async threshold. |
| `mode` | no | Passed as `--mode` (`work`\|`plan`\|`explore`\|`review`). |
| `success_outcome` | no | Domain outcome reported for `status: ok` (default: `default_success_outcome`). |
| `incomplete_outcome` | no | Domain outcome reported for `status: incomplete`, **only if the node declares one here**. Absent: an incomplete run is reported as an execution failure, never as success. |
| `async` | no | Force sync (`false`) or async (`true`) dispatch, overriding the step-budget threshold. |

## Trust model: `proposed`-only

This bridge **never emits `confirmed`/`observed`/`derived`** ledger
authority. Every `ledger_delta.records[]` entry it produces is a single
`claim` record, `authority: "proposed"`, `origin.kind: "agent"`: colleague's
own summary of what it did is a **completion claim**, not verified evidence
(PRD §10.5, and this repo's own `CLAUDE.md` ledger-authority-model rule — no
actor promotes its own proposal). A human `confirm`/`reject`s it, a trusted
runner would have to directly measure a fact to write `observed`, and a
deterministic validator would have to compute one to write `derived` — none
of which this bridge is positioned to do on colleague's behalf. Whether
`status: ok` becomes the node's `completed` outcome or something else is a
**domain outcome** decision the invoking node's contract makes (via
`input.success_outcome`), never a technical/engine verdict this bridge
invents.

## Running it

```bash
uv run --project adapters/colleague colleague-bridge --config /path/to/bridge.json
# or, without installing the console script:
uv run --project adapters/colleague python -m colleague_bridge --config /path/to/bridge.json
```

## Tests

```bash
uv run --project adapters/colleague pytest
```

Unit tests need no `colleague` binary. The integration test
(`tests/test_integration_bridge.py`) shells out to a REAL `colleague work`
with `COLLEAGUE_ENGINE=mock` (colleague's deterministic offline engine) in a
throwaway scratch git repo; it skips (never fails) when `colleague` is not
on `PATH`, so the unit suite is always runnable without it.

### Running the PRD §13 conformance kit against this bridge

`scripts/run_conformance_kit.sh` starts the bridge against a scratch repo
with `COLLEAGUE_ENGINE=mock`, then runs culture-nodes's own
`tests/conformance` kit (`go test ./tests/conformance -args
-endpoint=http://127.0.0.1:<port> ...`) against it — the acceptance check
for this whole package. See the script for the exact flags; it requires `go`
and a working `colleague` install on `PATH`.

## Dockerfile

The provided `Dockerfile` builds an image with `colleague` installed via
`pipx` and this bridge as its entry point. **It ships separately from the
culture-nodes control-plane image** (PRD c24: the actor protocol is a
network boundary, not a shared deployment unit) — the control plane never
reaches into this container, and this container never reaches into the
control plane's. The image needs `colleague`'s configured engine (a vLLM
endpoint, typically) reachable from wherever it runs; `COLLEAGUE_ENGINE=mock`
is useful for a smoke-test deployment but is not a real backend. Repos the
bridge dispatches into must be mounted or checked out into the container
(the `repo_allowlist` paths are container-internal paths) — this Dockerfile
does not embed any specific repo, by design; that is a per-deployment
concern (a bind mount, an init container that clones, etc.), documented
here rather than baked into a generic reference image.
