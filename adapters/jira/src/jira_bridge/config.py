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
    # Exact-match status names the transition_issue verb may move an issue to.
    # A LIST, not a single value, since task t11: the ticket now moves to
    # 'Pending' when culture-nodes raises a human decision and to 'Done' when
    # the work finishes, and one bridge serves both. Empty means the verb is
    # refused everywhere -- the allowlist is configured, never defaulted.
    transition_targets: tuple[str, ...] = ()
    # Exact-match project keys the create_issue verb may target. Empty means
    # creation is refused everywhere -- the allowlist is configured, never
    # defaulted (task t9).
    create_projects: tuple[str, ...] = ()
    # Exact-match issue type names create_issue may use. Defaults to the one
    # type the verb has always defaulted to; anything else is refused by name
    # (PR #208 review finding 1) -- widen deliberately per deployment.
    create_issue_types: tuple[str, ...] = ("Task",)
    # Read custody stays environment-only. None refuses every read.
    read_comment_limit: int | None = None

    @property
    def transition_target(self) -> str:
        """The single-target view this config had before task t11.

        Kept because a deployment that configured exactly one target should
        keep reading back exactly that string, and because the capabilities
        advertisement has published this name since the verb shipped. It is a
        property rather than a field, so it cannot drift from the list.
        """
        return self.transition_targets[0] if self.transition_targets else ""

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
        }.items():
            if name in env:
                values[field] = env[name]
        # Single-string compat: JIRA_TRANSITION_TARGET has always held one
        # status name, and a deployment that still sets one keeps working
        # unchanged. A comma separates several -- the same spelling
        # JIRA_CREATE_PROJECTS already uses, so an operator does not have to
        # learn a second list syntax for the same bridge.
        for name in ("JIRA_TRANSITION_TARGET", "JIRA_TRANSITION_TARGETS"):
            if name in env:
                values["transition_targets"] = env[name].split(",")
        if "JIRA_CREATE_PROJECTS" in env:
            values["create_projects"] = env["JIRA_CREATE_PROJECTS"].split(",")
        if "JIRA_CREATE_ISSUE_TYPES" in env:
            values["create_issue_types"] = env["JIRA_CREATE_ISSUE_TYPES"].split(",")
        if "JIRA_READ_COMMENT_LIMIT" in env:
            try:
                values["read_comment_limit"] = int(env["JIRA_READ_COMMENT_LIMIT"])
            except ValueError as exc:
                raise ConfigError("JIRA_READ_COMMENT_LIMIT must be an integer") from exc
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
        values["create_projects"] = tuple(item.strip() for item in raw_projects if item.strip())
        raw_targets = values.get("transition_targets", ())
        if not isinstance(raw_targets, (list, tuple)) or not all(
            isinstance(item, str) for item in raw_targets
        ):
            raise ConfigError("transition_targets must be a list of status names")
        values["transition_targets"] = tuple(item.strip() for item in raw_targets if item.strip())
        raw_types = values.get("create_issue_types", ("Task",))
        if not isinstance(raw_types, (list, tuple)) or not all(
            isinstance(item, str) for item in raw_types
        ):
            raise ConfigError("create_issue_types must be a list of issue type names")
        values["create_issue_types"] = tuple(
            item.strip() for item in raw_types if item.strip()
        ) or ("Task",)
        return cls(**values)
