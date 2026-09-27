"""Measured host facts for the workspace-less GitHub bridge."""

from __future__ import annotations

import os
import pwd
import sys
from typing import Any

from github_bridge import deployment, preflight

from .config import Config


def host_facts(cfg: Config) -> dict[str, Any]:
    del cfg
    return preflight.host_block(
        hostname=preflight.hostname(),
        confinement=(
            f"unix-user:{pwd.getpwuid(os.getuid()).pw_name}: "
            "bounded GitHub API requests; no subprocess or shell"
        ),
        commit_policy="no workspace: this bridge writes no files and runs no git",
        writable_paths=[],
        artifact_publish="not-applicable-no-workspace",
        deployment=deployment.deployment_facts(
            sys.modules[__package__], "culture-nodes-github-bridge"
        ),
    )
