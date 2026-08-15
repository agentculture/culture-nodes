"""Shared fixtures.

`fake_codex` backs the unit tests that need to exercise a REAL subprocess
boundary (argv construction, SIGTERM-on-timeout, EOF handling, live stdout
streaming) without the real `codex` CLI. Its four behaviors were grounded
against real codex-cli 0.144.6 output — see `codex_cli.py`'s module
docstring and README.md's "What a codex session's JSONL looks like" for the
un-trimmed transcripts each one is modeled on. In particular,
`hang_then_clean_exit_zero_on_sigterm` reproduces the exact, measured,
counter-intuitive case this whole task's acceptance criterion is about: a
SIGTERM'd real codex session exits 0 cleanly without ever emitting a
terminal turn event.

`codex_bin`/`scratch_repo` back ONLY the integration suite
(`test_integration_bridge.py`), which shells out to a REAL, authenticated
`codex exec` and skips (never fails) when codex is unavailable or not
logged in — mirroring `adapters/colleague`'s own conftest.py discipline, so
the unit suite (everything else) never depends on a real CLI being present.
"""

from __future__ import annotations

import shutil
import stat
import subprocess
from pathlib import Path

import pytest

FAKE_CODEX_SCRIPT = """#!/usr/bin/env python3
import json
import os
import signal
import sys
import time


def emit(obj):
    print(json.dumps(obj), flush=True)


behavior = os.environ.get("FAKE_CODEX_BEHAVIOR", "ok")

if behavior == "ok":
    emit({"type": "thread.started", "thread_id": "fake-thread-ok"})
    emit({"type": "turn.started"})
    msg_item = {"id": "item_0", "type": "agent_message", "text": "OK"}
    emit({"type": "item.completed", "item": msg_item})
    emit({"type": "turn.completed", "usage": {"input_tokens": 1, "output_tokens": 1}})
    sys.exit(0)
elif behavior == "error":
    emit({"type": "thread.started", "thread_id": "fake-thread-error"})
    emit({"type": "turn.started"})
    emit({"type": "turn.failed", "error": {"message": "fake failure"}})
    sys.exit(1)
elif behavior == "crash_before_any_output":
    sys.exit(1)
elif behavior == "hang_then_clean_exit_zero_on_sigterm":
    # Mirrors the REAL codex-cli 0.144.6 behavior this adapter was grounded
    # against: SIGTERM is caught and the process exits 0 cleanly, WITHOUT
    # ever emitting a terminal turn event.
    def _on_term(signum, frame):
        sys.exit(0)

    signal.signal(signal.SIGTERM, _on_term)
    emit({"type": "thread.started", "thread_id": "fake-thread-hang"})
    emit({"type": "turn.started"})
    emit(
        {
            "type": "item.started",
            "item": {
                "id": "item_1",
                "type": "command_execution",
                "command": "sleep 3",
                "aggregated_output": "",
                "exit_code": None,
                "status": "in_progress",
            },
        }
    )
    time.sleep(60)
elif behavior == "dirty_workspace_then_hang_clean_exit_zero_on_sigterm":
    # Task t13: the same measured SIGTERM behavior as the branch above, with
    # the one addition that makes the deadline case real -- this session
    # CHANGES THE WORKSPACE before it is stopped. codex is spawned with
    # cwd=repo (codex_cli.py), so writing here writes into the repository
    # the dispatch named, exactly as a real session's edits do. The file is
    # written BEFORE the first progress event, so a test that waits for
    # progress has already-observed evidence the work exists on disk rather
    # than a sleep.
    def _on_term(signum, frame):
        sys.exit(0)

    signal.signal(signal.SIGTERM, _on_term)
    with open("work-in-progress.txt", "w", encoding="utf-8") as fh:
        fh.write("half-finished work the session was stopped in the middle of\\n")
    emit({"type": "thread.started", "thread_id": "fake-thread-dirty-hang"})
    emit({"type": "turn.started"})
    emit(
        {
            "type": "item.started",
            "item": {
                "id": "item_1",
                "type": "command_execution",
                "command": "sleep 3",
                "aggregated_output": "",
                "exit_code": None,
                "status": "in_progress",
            },
        }
    )
    time.sleep(60)
"""


@pytest.fixture()
def fake_codex(tmp_path):
    script = tmp_path / "fake-codex"
    script.write_text(FAKE_CODEX_SCRIPT, encoding="utf-8")
    script.chmod(script.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return script


def _codex_bin() -> str | None:
    return shutil.which("codex")


def _codex_authenticated(bin_path: str) -> bool:
    try:
        proc = subprocess.run(
            [bin_path, "login", "status"], capture_output=True, text=True, timeout=15
        )
    except (OSError, subprocess.TimeoutExpired):
        return False
    return proc.returncode == 0 and "logged in" in (proc.stdout + proc.stderr).lower()


@pytest.fixture(scope="session")
def codex_bin() -> str:
    """The `codex` binary path, or a pytest.skip if it (or its auth) is
    unavailable — every integration test depends on this rather than
    duplicating the availability probe."""
    bin_path = _codex_bin()
    if bin_path is None:
        pytest.skip("codex is not on PATH; skipping integration tests")
    if not _codex_authenticated(bin_path):
        pytest.skip("codex is not logged in (`codex login status`); skipping integration tests")
    return bin_path


#: The scratch repo integration tests dispatch work into, alongside (not
#: reusing) colleague-bridge's own t26-repo scratchpad.
SCRATCH_REPO = Path(
    "/tmp/claude-1000/-home-spark-git-culture-nodes/8280e616-b882-4f04-92e8-eff3adc26e10"
    "/scratchpad/t13-codex-repo"
)


@pytest.fixture(scope="session")
def scratch_repo(codex_bin: str) -> Path:
    """The throwaway git repo integration tests dispatch work into.

    Created once per session (idempotent across pytest invocations too —
    the `.git` presence check skips re-init).
    """
    SCRATCH_REPO.mkdir(parents=True, exist_ok=True)
    if not (SCRATCH_REPO / ".git").is_dir():
        subprocess.run(["git", "init", "-q"], cwd=SCRATCH_REPO, check=True)
        subprocess.run(
            ["git", "config", "user.email", "t13-bridge-tests@example.com"],
            cwd=SCRATCH_REPO,
            check=True,
        )
        subprocess.run(
            ["git", "config", "user.name", "t13 bridge tests"], cwd=SCRATCH_REPO, check=True
        )
        (SCRATCH_REPO / "README.md").write_text(
            "# t13 codex-bridge scratch repo\n", encoding="utf-8"
        )
        subprocess.run(["git", "add", "README.md"], cwd=SCRATCH_REPO, check=True)
        subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=SCRATCH_REPO, check=True)
    return SCRATCH_REPO
