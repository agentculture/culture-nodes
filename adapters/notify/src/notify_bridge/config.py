"""Bridge configuration: env vars, with an optional small JSON file
underneath. Mirrors `adapters/human-inbox/src/human_inbox_bridge/config.py`
field for field where the concern is generic (HTTP surface, state dir);
everything callback-shaped is absent because this bridge never calls
back -- every invocation is answered synchronously, within the webhook
transport's own 5-second bound (`webhook.POST_TIMEOUT_SECONDS`), so there
is no async park and no callback delivery surface to configure.

The webhook URL is DELIBERATELY ABSENT from this dataclass and from every
env var this module recognises. It is resolved directly from
`CULTURE_NODES_WEBHOOK_URL` / `DISCORD_WEBHOOK_URL` at POST time by
`webhook.resolve_webhook()`, never accepted as a constructor argument,
never read from a config file, and never logged -- see `webhook.py`'s
module docstring for why.

Precedence: JSON config file (if present) sets the baseline; environment
variables (`NOTIFY_BRIDGE_*`) override individual fields on top of it.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

#: Env var naming the JSON config file to load (optional).
ENV_CONFIG_FILE = "NOTIFY_BRIDGE_CONFIG"

#: The `NOTIFY_BRIDGE_*` env vars this module recognises, and the config
#: field each overrides. One table so the precedence rule ("env wins over
#: file") is visibly total, not ad hoc per field. Note the deliberate
#: absence of anything webhook-URL-shaped -- see module docstring.
_ENV_STRING_FIELDS = {
    "NOTIFY_BRIDGE_STATE_DIR": "state_dir",
    "NOTIFY_BRIDGE_AUTH_TOKEN": "auth_token",  # nosec B105 - a field NAME, not a credential
    "NOTIFY_BRIDGE_HOST": "host",
    "NOTIFY_BRIDGE_ACTOR_ID": "actor_id",
}
_ENV_INT_FIELDS = {
    "NOTIFY_BRIDGE_PORT": "port",
}


class ConfigError(Exception):
    """Raised for a config file/env value the bridge cannot use."""


@dataclass
class Config:
    """Resolved bridge configuration. See module docstring for precedence."""

    # --- identity of the bridge as a ledger producer -----------------
    #: `origin.actor_id` on the proposed claim records this bridge emits.
    #: Operators typically set this to the registered actor key (e.g.
    #: `company/notify-discord`).
    actor_id: str = "notify-bridge"

    # --- HTTP surface ----------------------------------------------------
    host: str = "127.0.0.1"
    #: 8088: human-inbox holds 8087, colleague 8085, codex/claude-code
    #: 8086 -- one bridge, one well-known port.
    port: int = 8088
    #: Bearer token required on `Authorization` for the invocation route.
    #: Empty means unauthenticated -- legitimate only for a loopback
    #: deployment (see `server._refuse_unauthenticated_exposure`).
    auth_token: str | None = None

    # --- state (idempotency replay only; this bridge holds no other durable
    # state -- every invocation is answered synchronously, nothing parks) --
    state_dir: str = ".notify-bridge-state"

    @property
    def state_path(self) -> Path:
        return Path(self.state_dir)

    @classmethod
    def load(cls, config_path: str | None = None, env: dict[str, str] | None = None) -> "Config":
        """Build a Config from an optional JSON file, then env overrides.

        *config_path* wins over `NOTIFY_BRIDGE_CONFIG` when both are given;
        *env* defaults to `os.environ` and is only ever a parameter so
        tests can supply a clean map instead of mutating the real process
        environment.
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
#: the raw JSON value. `webhook_url` (or anything URL-shaped) is
#: deliberately absent -- see module docstring.
_FILE_FIELDS = {
    "actor_id": str,
    "host": str,
    "port": int,
    "auth_token": str,
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


def _parse_int(name: str, raw: str) -> int:
    try:
        return int(raw)
    except ValueError as exc:
        raise ConfigError(f"{name}={raw!r} is not an integer") from exc
