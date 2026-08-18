"""Non-secret bridge config; Jira credentials are intentionally absent."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

ENV_CONFIG_FILE = "JIRA_BRIDGE_CONFIG"
_FILE_FIELDS = {"actor_id", "host", "port", "auth_token", "state_dir", "jira_site"}


class ConfigError(Exception):
    pass


@dataclass
class Config:
    actor_id: str = "jira-bridge"
    host: str = "127.0.0.1"
    port: int = 8089
    auth_token: str | None = None
    state_dir: str = ".jira-bridge-state"
    jira_site: str = ""

    @classmethod
    def load(cls, config_path: str | None = None, env: dict[str, str] | None = None) -> "Config":
        env = os.environ if env is None else env
        data: dict = {}
        path = config_path or env.get(ENV_CONFIG_FILE)
        if path:
            try:
                data = json.loads(Path(path).read_text(encoding="utf-8"))
            except (OSError, ValueError) as exc:
                raise ConfigError(f"cannot read bridge config {path!r}: {exc}") from exc
            if not isinstance(data, dict):
                raise ConfigError("bridge config must be a JSON object")
            unknown = set(data) - _FILE_FIELDS
            if unknown:
                raise ConfigError(f"unknown bridge config key: {sorted(unknown)[0]!r}")
        values = dict(data)
        for name, field in {
            "JIRA_BRIDGE_ACTOR_ID": "actor_id",
            "JIRA_BRIDGE_HOST": "host",
            "JIRA_BRIDGE_AUTH_TOKEN": "auth_token",
            "JIRA_BRIDGE_STATE_DIR": "state_dir",
            "JIRA_SITE": "jira_site",
        }.items():
            if name in env:
                values[field] = env[name]
        if "JIRA_BRIDGE_PORT" in env:
            try:
                values["port"] = int(env["JIRA_BRIDGE_PORT"])
            except ValueError as exc:
                raise ConfigError("JIRA_BRIDGE_PORT must be an integer") from exc
        return cls(**values)
