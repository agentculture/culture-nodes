"""Measured host facts for the workspace-less Jira actor bridge."""

from __future__ import annotations

import os
import pwd
import sys
from typing import Any, Sequence

from jira_bridge import deployment, preflight
from jira_bridge.config import Config

_CONFINEMENT = (
    f"unix-user:{pwd.getpwuid(os.getuid()).pw_name}: "
    "no session: this bridge performs bounded Jira API requests in this process and starts no "
    "agent, subprocess or shell, so there is nothing to sandbox"
)
_COMMIT_POLICY = (
    "no workspace: this bridge writes no files and runs no git — there is nothing to commit, "
    "preserve or harvest. Its effects are allowlisted Jira API mutations"
)


def host_facts(
    cfg: Config,
    *,
    probes: Sequence[tuple[str, str]] = preflight.USERNS_SYSCTLS,
) -> dict[str, Any]:
    """Return facts this bridge can measure about the host it runs on."""
    del cfg, probes
    return preflight.host_block(
        hostname=preflight.hostname(),
        confinement=_CONFINEMENT,
        commit_policy=_COMMIT_POLICY,
        writable_paths=[],
        artifact_publish="not-applicable-no-workspace",
        deployment=deployment.deployment_facts(
            sys.modules[__package__], "culture-nodes-jira-bridge"
        ),
    )
