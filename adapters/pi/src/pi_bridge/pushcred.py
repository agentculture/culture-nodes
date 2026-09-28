"""Read the current GitHub installation token at each subprocess dispatch."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Mapping


def fresh_environ(base: Mapping[str, str] | None = None) -> dict[str, str]:
    """Copy the environment and overlay the current token when readable."""
    env = dict(os.environ if base is None else base)
    path = Path(
        env.get(
            "CULTURE_NODES_GITHUB_TOKEN_FILE",
            os.environ.get("CULTURE_NODES_GITHUB_TOKEN_FILE", "~/.culture-nodes/github-token.env"),
        )
    ).expanduser()
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError):
        return env
    for line in lines:
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        name, value = line.split("=", 1)
        if name.strip() != "GITHUB_TOKEN_WORKER":
            continue
        value = value.strip().strip("\"'")
        if value:
            env["GITHUB_TOKEN_WORKER"] = value
            env["GH_TOKEN"] = value
    return env
