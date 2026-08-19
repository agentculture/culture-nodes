"""Non-secret bridge config; Jira credentials are intentionally absent."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

ENV_CONFIG_FILE = "JIRA_BRIDGE_CONFIG"
_FILE_FIELDS = {
    "actor_id",
    "host",
    "port",
    "auth_token",
    "state_dir",
    "jira_site",
    "create_projects",
    "create_issue_types",
}


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
    transition_project_prefix: str = ""
    transition_target: str = ""
    # Exact-match project keys the create_issue verb may target. Empty means
    # creation is refused everywhere -- the allowlist is configured, never
    # defaulted (task t9).
    create_projects: tuple[str, ...] = ()
    # Exact-match issue type names create_issue may use. Defaults to the one
    # type the verb has always defaulted to; anything else is refused by name
    # (PR #208 review finding 1) -- widen deliberately per deployment.
    create_issue_types: tuple[str, ...] = ("Task",)

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
            "JIRA_TRANSITION_PROJECT_PREFIX": "transition_project_prefix",
            "JIRA_TRANSITION_TARGET": "transition_target",
        }.items():
            if name in env:
                values[field] = env[name]
        if "JIRA_CREATE_PROJECTS" in env:
            values["create_projects"] = env["JIRA_CREATE_PROJECTS"].split(",")
        if "JIRA_CREATE_ISSUE_TYPES" in env:
            values["create_issue_types"] = env["JIRA_CREATE_ISSUE_TYPES"].split(",")
        if "JIRA_BRIDGE_PORT" in env:
            try:
                values["port"] = int(env["JIRA_BRIDGE_PORT"])
            except ValueError as exc:
                raise ConfigError("JIRA_BRIDGE_PORT must be an integer") from exc
        raw_projects = values.get("create_projects", ())
        if not isinstance(raw_projects, (list, tuple)) or not all(
            isinstance(item, str) for item in raw_projects
        ):
            raise ConfigError("create_projects must be a list of project keys")
        values["create_projects"] = tuple(
            item.strip() for item in raw_projects if item.strip()
        )
        raw_types = values.get("create_issue_types", ("Task",))
        if not isinstance(raw_types, (list, tuple)) or not all(
            isinstance(item, str) for item in raw_types
        ):
            raise ConfigError("create_issue_types must be a list of issue type names")
        values["create_issue_types"] = tuple(
            item.strip() for item in raw_types if item.strip()
        ) or ("Task",)
        return cls(**values)
