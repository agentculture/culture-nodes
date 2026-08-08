"""Shared fixtures. Unit tests need none of this — only the integration
suite (`test_integration_bridge.py`) touches a real `colleague` binary."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

#: The scratch repo the task's own acceptance instructions name explicitly.
#: Reused (not recreated) across test runs — colleague's own worktree
#: isolation means each work item still lands on its own `colleague/<id>`
#: branch, so repeated runs against the same checkout are safe.
SCRATCH_REPO = Path(
    "/tmp/claude-1000/-home-spark-git-culture-nodes/840c4ac5-c169-4960-b064-4a70a81a3eb3"
    "/scratchpad/t26-repo"
)

#: A deliberately dead loopback address. Some hosts (including the one this
#: bridge was developed on) carry a machine-wide `~/.colleague/config.json`
#: `{"lobes": "..."}` pointer to a senses/cortex gateway that is not always
#: reachable, which makes colleague spend several seconds per invocation
#: retrying a health probe before giving up. This bridge's own actor
#: dispatches are one-shot and headless (no interactive senses/cortex front
#: door involved), so the test suite explicitly disarms it via
#: `COLLEAGUE_LOBES_URL` for speed and determinism — a production deployment
#: with a real lobes gateway would instead point `colleague_env` at it (see
#: README.md's config reference).
_DISARM_LOBES_ENV = {"COLLEAGUE_LOBES_URL": "http://127.0.0.1:1/v1"}


def _colleague_bin() -> str | None:
    return shutil.which("colleague")


def _mock_engine_reachable(bin_path: str) -> bool:
    env = dict(os.environ)
    env["COLLEAGUE_ENGINE"] = "mock"
    env.update(_DISARM_LOBES_ENV)
    try:
        proc = subprocess.run(
            [bin_path, "whoami", "--json"],
            capture_output=True,
            text=True,
            timeout=15,
            env=env,
        )
    except (OSError, subprocess.TimeoutExpired):
        return False
    if proc.returncode != 0:
        return False
    try:
        payload = json.loads(proc.stdout.strip().splitlines()[-1])
    except (ValueError, IndexError):
        return False
    return payload.get("work_engine") == "mock"


@pytest.fixture(scope="session")
def colleague_bin() -> str:
    """The `colleague` binary path, or a pytest.skip if it (or its mock
    engine) is unavailable — every integration test depends on this rather
    than duplicating the availability probe."""
    bin_path = _colleague_bin()
    if bin_path is None:
        pytest.skip("colleague is not on PATH; skipping integration tests")
    if not _mock_engine_reachable(bin_path):
        pytest.skip("colleague whoami --json with COLLEAGUE_ENGINE=mock did not report the mock engine")
    return bin_path


@pytest.fixture(scope="session")
def scratch_repo(colleague_bin: str) -> Path:
    """The throwaway git repo integration tests dispatch work into.

    Created once per session (idempotent across pytest invocations too —
    the `.git` presence check skips re-init) at the exact path the task's
    own acceptance instructions name.
    """
    SCRATCH_REPO.mkdir(parents=True, exist_ok=True)
    if not (SCRATCH_REPO / ".git").is_dir():
        subprocess.run(["git", "init", "-q"], cwd=SCRATCH_REPO, check=True)
        subprocess.run(["git", "config", "user.email", "t26-bridge-tests@example.com"], cwd=SCRATCH_REPO, check=True)
        subprocess.run(["git", "config", "user.name", "t26 bridge tests"], cwd=SCRATCH_REPO, check=True)
        (SCRATCH_REPO / "README.md").write_text("# t26 colleague-bridge scratch repo\n", encoding="utf-8")
        subprocess.run(["git", "add", "README.md"], cwd=SCRATCH_REPO, check=True)
        subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=SCRATCH_REPO, check=True)
    return SCRATCH_REPO


@pytest.fixture()
def colleague_env() -> dict[str, str]:
    """The env overrides every integration-test colleague dispatch needs:
    the deterministic offline engine, and the lobes gateway disarmed (see
    `_DISARM_LOBES_ENV` above)."""
    env = {"COLLEAGUE_ENGINE": "mock"}
    env.update(_DISARM_LOBES_ENV)
    return env
