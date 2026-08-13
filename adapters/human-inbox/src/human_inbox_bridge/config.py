"""Bridge configuration: env vars, with an optional small JSON file underneath.

Mirrors `adapters/claude-code/src/claude_code_bridge/config.py` field for
field where the concern is generic (HTTP surface, callback timing, state
dir); everything subprocess-shaped is absent because this bridge never
executes anything — a human does. Two human-specific defaults matter:

* `heartbeat_after_seconds` defaults to **0**: the §13.3 acceptance body's
  heartbeat figure is the actor's own liveness promise, and a human actor
  honestly makes none — `internal/worker/dispatch.go`'s `asyncDeadline`
  treats 0 as "no timer, the wait is genuinely open-ended", which is exactly
  how a task parked on a person must behave (no lease to lose).
* there is no repo allowlist: the bridge touches no filesystem but its own
  state dir.

Precedence: JSON config file (if present) sets the baseline; environment
variables (`HUMAN_INBOX_BRIDGE_*`) override individual fields on top of it.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

#: Env var naming the JSON config file to load (optional).
ENV_CONFIG_FILE = "HUMAN_INBOX_BRIDGE_CONFIG"

#: The `HUMAN_INBOX_BRIDGE_*` env vars this module recognises, and the
#: config field each overrides. One table so the precedence rule ("env wins
#: over file") is visibly total, not ad hoc per field.
_ENV_STRING_FIELDS = {
    "HUMAN_INBOX_BRIDGE_STATE_DIR": "state_dir",
    "HUMAN_INBOX_BRIDGE_AUTH_TOKEN": "auth_token",  # nosec B105 - a field NAME, not a credential
    "HUMAN_INBOX_BRIDGE_HOST": "host",
    "HUMAN_INBOX_BRIDGE_ACTOR_ID": "actor_id",
}
_ENV_INT_FIELDS = {
    "HUMAN_INBOX_BRIDGE_PORT": "port",
    "HUMAN_INBOX_BRIDGE_HEARTBEAT_AFTER_SECONDS": "heartbeat_after_seconds",
    "HUMAN_INBOX_BRIDGE_CALLBACK_MAX_RETRIES": "callback_max_retries",
}
_ENV_FLOAT_FIELDS = {
    "HUMAN_INBOX_BRIDGE_CALLBACK_TIMEOUT_SECONDS": "callback_timeout_seconds",
    "HUMAN_INBOX_BRIDGE_CALLBACK_RETRY_BACKOFF_SECONDS": "callback_retry_backoff_seconds",
}


class ConfigError(Exception):
    """Raised for a config file/env value the bridge cannot use."""


@dataclass
class Config:
    """Resolved bridge configuration. See module docstring for precedence."""

    # --- identity of the bridge as a ledger producer -----------------
    #: `origin.actor_id` on the proposed claim records this bridge emits.
    #: Operators typically set this to the registered actor key (e.g.
    #: `company/human-ops`).
    actor_id: str = "human-inbox-bridge"

    # --- outcome vocabulary ----------------------------------------------
    #: Kept for parity with the sibling bridges' config surface; the human's
    #: submission always names its outcome explicitly, so this is only a
    #: documentation default, never silently substituted.
    default_success_outcome: str = "completed"

    # --- HTTP surface ----------------------------------------------------
    host: str = "127.0.0.1"
    #: 8087: colleague holds 8085, codex and claude-code hold 8086 on their
    #: respective hosts — one bridge, one well-known port.
    port: int = 8087
    #: Bearer token required on `Authorization` — for BOTH the inbound
    #: invocation route and the human inbox surface (list/submit are a read
    #: of parked work and a mutation of a run; neither is anonymous). Empty
    #: means unauthenticated — legitimate only for a loopback deployment.
    auth_token: str | None = None

    # --- liveness promise -------------------------------------------------
    #: See module docstring: 0 = no heartbeat promise, open-ended park.
    heartbeat_after_seconds: int = 0

    # --- callback delivery ------------------------------------------------
    callback_timeout_seconds: float = 10.0
    callback_max_retries: int = 5
    callback_retry_backoff_seconds: float = 0.25

    # --- state ------------------------------------------------------------
    state_dir: str = ".human-inbox-bridge-state"

    @property
    def state_path(self) -> Path:
        return Path(self.state_dir)

    @classmethod
    def load(cls, config_path: str | None = None, env: dict[str, str] | None = None) -> "Config":
        """Build a Config from an optional JSON file, then env overrides.

        *config_path* wins over `HUMAN_INBOX_BRIDGE_CONFIG` when both are
        given; *env* defaults to `os.environ` and is only ever a parameter
        so tests can supply a clean map instead of mutating the real
        process environment.
        """
        env = os.environ if env is None else env
        data: dict = {}
        path = config_path or env.get(ENV_CONFIG_FILE)
        if path:
            data = _read_config_file(path)

        cfg = cls(**_coerce_file_fields(data))
        _apply_env_overrides(cfg, env)
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
#: the raw JSON value.
_FILE_FIELDS = {
    "actor_id": str,
    "default_success_outcome": str,
    "host": str,
    "port": int,
    "auth_token": str,
    "heartbeat_after_seconds": int,
    "callback_timeout_seconds": float,
    "callback_max_retries": int,
    "callback_retry_backoff_seconds": float,
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
