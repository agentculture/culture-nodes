"""Bridge configuration: env vars, with an optional small JSON file underneath.

Mirrors `colleague_bridge/config.py` field for field — same precedence rule,
same safe-by-default allowlist stance — with the colleague-specific knobs
(`open_pr`, `allow_dirty`, `colleague_bin`) swapped for codex's own
(`codex_bin`, `default_sandbox`), since `codex exec` has no PR-opening or
dirty-worktree concept to configure.

The bridge runs on the machine that hosts a `codex` CLI install — it is a
deployment-time reference adapter, not part of the culture-nodes control
plane. Its configuration is deliberately small and readable in one place:

* a **repo path allowlist** — the bridge only dispatches into repos it was
  explicitly configured for; any other `input.repo` is refused with 403
  (never a silent `git clone` of whatever a caller names);
* the **codex binary** to shell out to, and any `codex_env` passthrough
  (e.g. `CODEX_HOME`) the operator wants every dispatch to carry;
* the **default sandbox mode** `codex exec --sandbox` uses when an
  invocation does not name one;
* the **sync/async dispatch threshold** (`sync_max_steps` / `always_async`)
  — a dispatch-timing decision only; codex has no `--max-steps` flag to
  forward it to (see README's "argv this bridge generates");
* a **state dir** for the idempotency store and per-invocation bookkeeping.

Precedence: JSON config file (if present) sets the baseline; environment
variables (`CODEX_BRIDGE_*`) override individual fields on top of it, so an
operator can ship one config file per repo checkout and still override a
single knob (e.g. the port) from the process supervisor without editing the
file. Passing neither is a valid, if useless, configuration — every field
has a documented default — except the repo allowlist, which is empty by
default (an unconfigured bridge accepts no repo, the safe failure mode).
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path

#: Env var naming the JSON config file to load (optional).
ENV_CONFIG_FILE = "CODEX_BRIDGE_CONFIG"

#: The `CODEX_BRIDGE_*` env vars this module recognises, and the config
#: field each overrides. Kept as one table so `Config.load`'s precedence
#: rule ("env wins over file") is visibly total, not ad hoc per field.
_ENV_STRING_FIELDS = {
    "CODEX_BRIDGE_CODEX_BIN": "codex_bin",
    "CODEX_BRIDGE_DEFAULT_SANDBOX": "default_sandbox",
    "CODEX_BRIDGE_STATE_DIR": "state_dir",
    # A field-name mapping table entry, not a credential.
    "CODEX_BRIDGE_AUTH_TOKEN": "auth_token",  # nosec B105
    "CODEX_BRIDGE_HOST": "host",
    "CODEX_BRIDGE_DEFAULT_SUCCESS_OUTCOME": "default_success_outcome",
    "CODEX_BRIDGE_ACTOR_ID": "actor_id",
    "CODEX_BRIDGE_PRESERVE_BRANCH_PREFIX": "preserve_branch_prefix",
    "CODEX_BRIDGE_PRESERVE_REMOTE": "preserve_remote",
}
_ENV_INT_FIELDS = {
    "CODEX_BRIDGE_PORT": "port",
    "CODEX_BRIDGE_SYNC_MAX_STEPS": "sync_max_steps",
    "CODEX_BRIDGE_DEFAULT_MAX_STEPS": "default_max_steps",
    "CODEX_BRIDGE_HEARTBEAT_AFTER_SECONDS": "heartbeat_after_seconds",
    "CODEX_BRIDGE_CALLBACK_MAX_RETRIES": "callback_max_retries",
    "CODEX_BRIDGE_MAX_INFLIGHT_PER_SESSION_KEY": "max_inflight_per_session_key",
}
_ENV_FLOAT_FIELDS = {
    "CODEX_BRIDGE_POLL_INTERVAL_SECONDS": "poll_interval_seconds",
    "CODEX_BRIDGE_CALLBACK_TIMEOUT_SECONDS": "callback_timeout_seconds",
    "CODEX_BRIDGE_CALLBACK_RETRY_BACKOFF_SECONDS": "callback_retry_backoff_seconds",
    "CODEX_BRIDGE_SYNC_TIMEOUT_SECONDS": "sync_timeout_seconds",
    "CODEX_BRIDGE_ASYNC_WAIT_SECONDS": "async_wait_seconds",
}
_ENV_BOOL_FIELDS = {
    "CODEX_BRIDGE_ALWAYS_ASYNC": "always_async",
    "CODEX_BRIDGE_SESSION_CONCURRENCY_ENABLED": "session_concurrency_enabled",
    "CODEX_BRIDGE_PRESERVE_ON_FAILURE": "preserve_on_failure",
    "CODEX_BRIDGE_PRESERVE_PUSH": "preserve_push",
}

#: `CODEX_BRIDGE_REPO_ALLOWLIST` is a `os.pathsep`-joined list of absolute
#: repo paths, matching how PATH-shaped env vars are conventionally written;
#: each entry is resolved (symlinks + `..` collapsed) at load time so a
#: later membership check is a plain string-equality test, never a fresh
#: filesystem walk per request.
ENV_REPO_ALLOWLIST = "CODEX_BRIDGE_REPO_ALLOWLIST"
ENV_REPO_ALLOWLIST_PREFIXES = "CODEX_BRIDGE_REPO_ALLOWLIST_PREFIXES"


class ConfigError(Exception):
    """Raised for a config file/env value the bridge cannot use."""


@dataclass
class Config:
    """Resolved bridge configuration. See module docstring for precedence."""

    # --- identity of the bridge as a ledger producer -----------------
    actor_id: str = "codex-bridge"

    # --- repo allowlist (the bridge only works repos it is configured
    # for) --------------------------------------------------------------
    repo_allowlist: tuple[str, ...] = ()
    repo_allowlist_prefixes: tuple[str, ...] = ()

    # --- codex dispatch --------------------------------------------------
    codex_bin: str = "codex"
    #: Extra env vars merged onto the subprocess environment (e.g.
    #: CODEX_HOME to point at a specific auth profile). Operator-supplied;
    #: the bridge never invents values here on its own initiative.
    codex_env: dict[str, str] = field(default_factory=dict)
    #: `codex exec --sandbox` value used when `input.sandbox` is absent.
    #: One of codex_cli.SANDBOX_MODES.
    default_sandbox: str = "workspace-write"

    # --- sync/async dispatch policy ------------------------------------
    #: A request whose expected step budget exceeds this threshold is
    #: dispatched asynchronously; at or under it, synchronously. An
    #: invocation's `input.async` (bool), when present, overrides the
    #: threshold decision outright. NOT forwarded to codex (no native
    #: step-budget flag) — dispatch-timing signal only.
    sync_max_steps: int = 6
    #: The step budget assumed for threshold comparison when
    #: `input.max_steps` is absent.
    default_max_steps: int = 6
    #: When set, every invocation is dispatched asynchronously regardless
    #: of the threshold or an `input.async` override — the "always-async"
    #: escape hatch the task names.
    always_async: bool = False

    # --- session-key concurrency (t6, c44/h37) --------------------------
    #: How many invocations may hold one `input.session_key`'s in-flight
    #: slot at once before a further concurrent arrival forks (dispatches
    #: cold, ignoring the `continuation_ref` it carried). See
    #: `session_registry.py`'s module docstring for the fork-vs-queue
    #: argument. 1 means "exactly one in-flight invocation per session
    #: key" — the acceptance criterion's own phrasing.
    max_inflight_per_session_key: int = 1
    #: Kill-switch back to t5's unserialized behaviour (every invocation
    #: dispatches with its `continuation_ref` as given, session_key
    #: collisions included) — for an operator who needs to rule this
    #: mechanism out while diagnosing something else.
    session_concurrency_enabled: bool = True

    # --- outcome vocabulary ---------------------------------------------
    #: Domain outcome used for a `status: ok` TaskResult when the
    #: invocation's `input.success_outcome` is absent.
    default_success_outcome: str = "completed"

    # --- preserve-on-failure (task t25, issue #49) ----------------------
    #: Commit-on-failure toggle: when a node's dispatch ends in a genuine
    #: technical failure (never a domain outcome), the bridge preserves the
    #: workspace's changes on a freshly minted branch via git plumbing (see
    #: `preserve.py`'s module docstring). Off means "never attempt it" —
    #: e.g. for a bridge host where preservation is deliberately unwanted.
    preserve_on_failure: bool = True
    #: Prefix for the code-minted preserve branch name.
    preserve_branch_prefix: str = "preserve/"
    #: Push-or-local: when True (the default), a preserve commit is pushed
    #: best-effort to `preserve_remote`; when the push fails or this is
    #: False, the commit stays local-only — an ordinary recorded outcome
    #: (task t25's own risk register: bridge-host push credentials for
    #: thor/orin are unverified), never an error.
    preserve_push: bool = True
    #: The remote a preserve branch is pushed to, when `preserve_push` is
    #: True.
    preserve_remote: str = "origin"

    # --- HTTP surface ----------------------------------------------------
    host: str = "127.0.0.1"
    #: Different default than colleague-bridge's 8085 so both can run on
    #: one host without colliding.
    port: int = 8086
    #: Bearer token the bridge requires on `Authorization`. Empty means
    #: unauthenticated — legitimate only for a local/loopback deployment
    #: (mirrors internal/actors.Endpoint's own docstring).
    auth_token: str | None = None

    # --- asynchronous dispatch timing -----------------------------------
    heartbeat_after_seconds: int = 20
    poll_interval_seconds: float = 0.15
    callback_timeout_seconds: float = 10.0
    callback_max_retries: int = 5
    callback_retry_backoff_seconds: float = 0.25

    # --- process bounds ----------------------------------------------------
    #: Bounds one SYNCHRONOUS `codex exec` subprocess. On expiry the bridge
    #: sends SIGTERM (never SIGKILL) and answers a timeout. codex responds
    #: to SIGTERM by exiting promptly WITHOUT a terminal turn.completed /
    #: turn.failed event — see codex_cli.py's module docstring for the
    #: grounding transcript — which is exactly why this bridge never trusts
    #: exit code alone.
    sync_timeout_seconds: float = 300.0
    #: Overall ceiling the async runner waits for a codex subprocess to
    #: finish before SIGTERM + reporting a timeout failure. Generous by
    #: default — background work is expected to run long.
    async_wait_seconds: float = 3600.0

    # --- state -------------------------------------------------------------
    state_dir: str = ".codex-bridge-state"

    @property
    def state_path(self) -> Path:
        return Path(self.state_dir)

    def repo_allowed(self, repo: str) -> bool:
        """True for an exact entry or a strict child of a scoped prefix."""
        try:
            resolved = str(Path(repo).expanduser().resolve())
        except OSError:
            return False
        if resolved in self.repo_allowlist:
            return True
        candidate = Path(resolved)
        return any(
            candidate != Path(root) and candidate.is_relative_to(root)
            for root in self.repo_allowlist_prefixes
        )

    @classmethod
    def load(cls, config_path: str | None = None, env: dict[str, str] | None = None) -> "Config":
        """Build a Config from an optional JSON file, then env overrides.

        *config_path* wins over `CODEX_BRIDGE_CONFIG` when both are given
        (an explicit caller argument is the most specific source); *env*
        defaults to `os.environ` and is only ever a parameter so tests can
        supply a clean map instead of mutating the real process
        environment.
        """
        env = os.environ if env is None else env
        data: dict = {}
        path = config_path or env.get(ENV_CONFIG_FILE)
        if path:
            data = _read_config_file(path)

        cfg = cls(**_coerce_file_fields(data))
        _apply_env_overrides(cfg, env)
        _normalize_allowlist(cfg)
        return cfg


def _read_config_file(path: str) -> dict:
    try:
        raw = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise ConfigError(f"cannot read bridge config file {path!r}: {exc}") from exc
    try:
        data = json.loads(raw)
    except ValueError as exc:  # json.JSONDecodeError is a ValueError
        raise ConfigError(f"bridge config file {path!r} is not valid JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise ConfigError(f"bridge config file {path!r} must contain a JSON object")
    return data


#: Field names a config file may set, each mapped to a coercion applied to
#: the raw JSON value (so a file-supplied "8086" string integer field still
#: works the same as a real JSON number would).
_FILE_FIELDS = {
    "actor_id": str,
    "repo_allowlist": lambda v: tuple(str(x) for x in v),
    "repo_allowlist_prefixes": lambda v: tuple(str(x) for x in v),
    "codex_bin": str,
    "codex_env": lambda v: {str(k): str(x) for k, x in dict(v).items()},
    "default_sandbox": str,
    "sync_max_steps": int,
    "default_max_steps": int,
    "always_async": bool,
    "max_inflight_per_session_key": int,
    "session_concurrency_enabled": bool,
    "default_success_outcome": str,
    "preserve_on_failure": bool,
    "preserve_branch_prefix": str,
    "preserve_push": bool,
    "preserve_remote": str,
    "host": str,
    "port": int,
    "auth_token": str,
    "heartbeat_after_seconds": int,
    "poll_interval_seconds": float,
    "callback_timeout_seconds": float,
    "callback_max_retries": int,
    "callback_retry_backoff_seconds": float,
    "sync_timeout_seconds": float,
    "async_wait_seconds": float,
    "state_dir": str,
}


def _coerce_file_fields(data: dict) -> dict:
    out = {}
    for key, value in data.items():
        if key not in _FILE_FIELDS:
            raise ConfigError(f"unknown bridge config key: {key!r}")
        try:
            out[key] = _FILE_FIELDS[key](value)
        except (TypeError, ValueError) as exc:
            raise ConfigError(f"bridge config key {key!r} has an invalid value: {exc}") from exc
    return out


def _apply_env_overrides(cfg: Config, env: dict[str, str]) -> None:
    for name, field_name in _ENV_STRING_FIELDS.items():
        if name in env:
            setattr(cfg, field_name, env[name])
    for name, field_name in _ENV_INT_FIELDS.items():
        if name in env:
            setattr(cfg, field_name, _parse_int(name, env[name]))
    for name, field_name in _ENV_FLOAT_FIELDS.items():
        if name in env:
            setattr(cfg, field_name, _parse_float(name, env[name]))
    for name, field_name in _ENV_BOOL_FIELDS.items():
        if name in env:
            setattr(cfg, field_name, _parse_bool(name, env[name]))
    if ENV_REPO_ALLOWLIST in env:
        raw = env[ENV_REPO_ALLOWLIST]
        cfg.repo_allowlist = tuple(p for p in raw.split(os.pathsep) if p.strip())
    if ENV_REPO_ALLOWLIST_PREFIXES in env:
        raw = env[ENV_REPO_ALLOWLIST_PREFIXES]
        cfg.repo_allowlist_prefixes = tuple(p for p in raw.split(os.pathsep) if p.strip())


def _normalize_allowlist(cfg: Config) -> None:
    resolved = []
    for entry in cfg.repo_allowlist:
        try:
            resolved.append(str(Path(entry).expanduser().resolve()))
        except OSError as exc:
            raise ConfigError(
                f"repo allowlist entry {entry!r} could not be resolved: {exc}"
            ) from exc
    cfg.repo_allowlist = tuple(resolved)
    prefixes: list[str] = []
    for entry in cfg.repo_allowlist_prefixes:
        try:
            prefixes.append(str(Path(entry).expanduser().resolve()))
        except OSError as exc:
            raise ConfigError(f"cannot resolve repo allowlist prefix {entry!r}: {exc}") from exc
    cfg.repo_allowlist_prefixes = tuple(prefixes)


def _parse_int(name: str, raw: str) -> int:
    try:
        return int(raw)
    except ValueError as exc:
        raise ConfigError(f"{name}={raw!r} is not an integer") from exc


def _parse_float(name: str, raw: str) -> float:
    try:
        return float(raw)
    except ValueError as exc:
        raise ConfigError(f"{name}={raw!r} is not a number") from exc


def _parse_bool(name: str, raw: str) -> bool:
    lowered = raw.strip().lower()
    if lowered in ("1", "true", "yes", "on"):
        return True
    if lowered in ("0", "false", "no", "off", ""):
        return False
    raise ConfigError(f"{name}={raw!r} is not a boolean (1/0, true/false, yes/no, on/off)")
