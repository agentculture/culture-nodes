"""Non-secret GitHub bridge configuration."""

from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass
from pathlib import Path

_REPO = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
_FIELDS = {"actor_id", "host", "port", "auth_token", "repositories"}


class ConfigError(Exception):
    pass


@dataclass
class Config:
    actor_id: str = "github-bridge"
    host: str = "127.0.0.1"
    port: int = 8091
    auth_token: str | None = None
    repositories: tuple[str, ...] = ()

    @classmethod
    def load(cls, config_path: str | None = None, env: dict[str, str] | None = None) -> "Config":
        env = os.environ if env is None else env
        data: dict = {}
        path = config_path or env.get("GITHUB_BRIDGE_CONFIG")
        if path:
            try:
                data = json.loads(Path(path).read_text(encoding="utf-8"))
            except (OSError, ValueError) as exc:
                raise ConfigError(f"cannot read bridge config {path!r}: {exc}") from exc
            if not isinstance(data, dict):
                raise ConfigError("bridge config must be a JSON object")
            unknown = set(data) - _FIELDS
            if unknown:
                raise ConfigError(f"unknown bridge config key: {sorted(unknown)[0]!r}")
        values = dict(data)
        for name, field in {
            "GITHUB_BRIDGE_ACTOR_ID": "actor_id",
            "GITHUB_BRIDGE_HOST": "host",
            "GITHUB_BRIDGE_AUTH_TOKEN": "auth_token",
        }.items():
            if name in env:
                values[field] = env[name]
        if "GITHUB_BRIDGE_PORT" in env:
            try:
                values["port"] = int(env["GITHUB_BRIDGE_PORT"])
            except ValueError as exc:
                raise ConfigError("GITHUB_BRIDGE_PORT must be an integer") from exc
        if "GITHUB_REPOSITORIES" in env:
            values["repositories"] = env["GITHUB_REPOSITORIES"].split(",")
        repositories = values.get("repositories", ())
        if not isinstance(repositories, (list, tuple)) or not all(
            isinstance(repo, str) for repo in repositories
        ):
            raise ConfigError("repositories must be a list")
        values["repositories"] = tuple(repo.strip() for repo in repositories if repo.strip())
        if any(not _REPO.fullmatch(repo) for repo in values["repositories"]):
            raise ConfigError("repositories must contain owner/name pairs")
        return cls(**values)
