"""Task t13, claude-code's half: a session stopped by the control plane's
DEADLINE still lands its work on a preserve branch.

The all-backends rule (CLAUDE.md) is why this file mirrors
`adapters/codex/tests/test_deadline_preserve.py` rather than assuming the
codex proof carries. The two bridges cancel differently and that difference
is exactly what could break here: codex owns its subprocess and SIGTERMs it
directly, while this bridge's session is DETACHED — `cancel()` only writes a
`flightfiles` stop request, and the poller loop in `async_runner.py` is the
single place that ever turns one into a real signal. A preserve path proven
on the direct-SIGTERM bridge says nothing about the file-mediated one.

Everything downstream of `AsyncRunner.cancel` here is production code: the
poller reads the stop file, SIGTERMs a real process, observes it gone,
measures the real workspace, classifies the terminal event as `failed`, and
runs `preserve.preserve_on_failure` against a real git repository.

The subprocess is a plain python script rather than `fake_claude.py`,
because what this test needs from it is not claude's output shape (there is
none — a cancelled session never produces a `type: "result"` record, which
is the whole reason the terminal event is a failure) but a real pid that
edits the repository and then stays alive until it is signalled.
"""

from __future__ import annotations

import os
import signal
import stat
import subprocess
import time
from pathlib import Path

from claude_code_bridge import claude_cli, mapping, workspace
from claude_code_bridge.async_runner import AsyncRunner
from claude_code_bridge.config import Config

from ._fakes import FakeCallbackReceiver, fake_claude_path

#: A stand-in for a DETACHED claude session that has already changed the
#: workspace and is still working.
#:
#: It double-forks on purpose. In production `claude_cli.spawn_background`
#: detaches the session, so the bridge is not its parent and never reaps it;
#: `is_pid_alive` therefore flips to False the moment the process really
#: exits. A plain child of the test process would instead linger as a zombie
#: after SIGTERM -- `os.kill(pid, 0)` succeeds on a zombie -- and the poller
#: would wait forever for a process that had already died. Double-forking
#: reproduces the production parentage rather than working around the
#: bridge.
#:
#: The workspace write happens before the pid file, so a test that waits for
#: the pid file already knows the work exists on disk.
DIRTY_SESSION_SCRIPT = """#!/usr/bin/env python3
import os
import signal
import sys
import time

if os.fork() > 0:
    sys.exit(0)
os.setsid()
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
with open("work-in-progress.txt", "w", encoding="utf-8") as fh:
    fh.write("half-finished work the session was stopped in the middle of\\n")
with open(sys.argv[1], "w", encoding="utf-8") as fh:
    fh.write(str(os.getpid()))
time.sleep(600)
"""

#: The same detached session, leaving the workspace untouched.
CLEAN_SESSION_SCRIPT = """#!/usr/bin/env python3
import os
import signal
import sys
import time

if os.fork() > 0:
    sys.exit(0)
os.setsid()
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
with open(sys.argv[1], "w", encoding="utf-8") as fh:
    fh.write(str(os.getpid()))
time.sleep(600)
"""


def _git(repo: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["git", *args], cwd=repo, check=True, capture_output=True, text=True)


def _repo_with_one_commit(tmp_path: Path) -> Path:
    repo = tmp_path / "repo"
    repo.mkdir()
    _git(repo, "init", "-q")
    _git(repo, "config", "user.email", "t13@example.com")
    _git(repo, "config", "user.name", "t13")
    (repo / "README.md").write_text("# deadline scratch\n", encoding="utf-8")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-q", "-m", "init")
    return repo


def _cfg(tmp_path: Path, repo: Path) -> Config:
    return Config(
        claude_bin=fake_claude_path(),
        repo_allowlist=(str(repo),),
        state_dir=str(tmp_path / "state"),
        heartbeat_after_seconds=1,
        poll_interval_seconds=0.05,
        # Far beyond any test's patience: the bridge's own wait bound must
        # not be what ends this session. Only the cancel does, which is what
        # makes this the deadline path rather than a bridge timeout.
        async_wait_seconds=3600.0,
        callback_retry_backoff_seconds=0.05,
        preserve_push=False,
    )


def _spawn_session(tmp_path: Path, repo: Path, script: str) -> int:
    """Start the detached fake session and return the pid the bridge would
    have been handed, once that process has really started."""
    path = tmp_path / "detached-session.py"
    path.write_text(script, encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    pidfile = tmp_path / "session.pid"
    # cwd=repo is how the real bridge spawns claude (claude_cli.py), so the
    # session's edits land in the repository the dispatch named.
    launcher = subprocess.run(  # noqa: S603 - a test's own fake session
        [str(path), str(pidfile)], cwd=repo, check=True
    )
    assert launcher.returncode == 0
    _wait_for(pidfile.exists, timeout=10, what="the detached session reporting its pid")
    return int(pidfile.read_text(encoding="utf-8").strip())


def _wait_for(predicate, timeout: float, what: str) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.05)
    raise AssertionError(f"{what} did not happen within {timeout}s")


def _reap(pid: int) -> None:
    """Make sure the detached session is gone even if the test failed before
    the cancel: nothing else owns it, so nothing else would clean it up."""
    try:
        os.kill(pid, signal.SIGKILL)
    except OSError:
        pass


def test_deadline_cancel_of_a_dirty_session_leaves_a_preserve_ref(tmp_path):
    repo = _repo_with_one_commit(tmp_path)
    cfg = _cfg(tmp_path, repo)
    pid = _spawn_session(tmp_path, repo, DIRTY_SESSION_SCRIPT)
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        handle_id = "cc_deadline_dirty"
        # The snapshot server.py takes right before spawning claude; the
        # runner measures against it once the session ends.
        handle = workspace.begin(str(repo))
        assert handle.available, handle.reason

        runner.start(
            start=claude_cli.BackgroundStart(
                handle_id=handle_id, pid=pid, log_path=str(tmp_path / "unused.log")
            ),
            ctx=mapping.InvocationContext(
                run_id="r-deadline", node_run_id="nr-1", attempt_id="a-1"
            ),
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
            workspace_handle=handle,
        )
        assert receiver.wait_for_kind("accepted", timeout=10) is not None
        _wait_for(
            lambda: (repo / "work-in-progress.txt").exists(),
            timeout=10,
            what="the session writing into the repository",
        )

        # The deadline fires. internal/scheduler issues Cancel, the bridge's
        # HTTP layer hands it to exactly this call, and the poller turns the
        # stop request into a real SIGTERM.
        assert runner.cancel(handle_id) is True

        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=30)
        assert terminal is not None
        assert terminal["kind"] == "failed"

        preserve_block = terminal["payload"]["preserve"]
        assert preserve_block["attempted"] is True, preserve_block.get("reason")
        assert preserve_block["committed"] is True, preserve_block.get("reason")
        branch = preserve_block["branch"]
        assert branch

        # The fact, not the bridge's claim (PRD §10.4): the branch it named
        # is a real ref, and the interrupted work is reachable from it.
        ref = _git(repo, "rev-parse", "--verify", f"refs/heads/{branch}").stdout.strip()
        assert ref == preserve_block["commit"]
        assert "work-in-progress.txt" in _git(repo, "show", "--stat", ref).stdout
        assert "half-finished work" in _git(repo, "show", f"{ref}:work-in-progress.txt").stdout
    finally:
        receiver.close()
        _reap(pid)


def test_deadline_cancel_of_a_clean_session_reports_why_it_preserved_nothing(tmp_path):
    """When there is genuinely nothing to preserve the bridge says so, with
    a reason a reader can tell from a preserve that failed.

    As on the codex side, this stops at the terminal event:
    `internal/actors`' `Preserve.ToEngine` carries only a COMMITTED branch
    onto the attempt row, so `attempted:false` plus its reason is not
    persisted. That is task t13's third acceptance bullet and is not
    implemented -- see the delivery note.
    """
    repo = _repo_with_one_commit(tmp_path)
    cfg = _cfg(tmp_path, repo)
    pid = _spawn_session(tmp_path, repo, CLEAN_SESSION_SCRIPT)
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        handle_id = "cc_deadline_clean"
        handle = workspace.begin(str(repo))
        assert handle.available, handle.reason

        runner.start(
            start=claude_cli.BackgroundStart(
                handle_id=handle_id, pid=pid, log_path=str(tmp_path / "unused.log")
            ),
            ctx=mapping.InvocationContext(
                run_id="r-deadline", node_run_id="nr-2", attempt_id="a-2"
            ),
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
            workspace_handle=handle,
        )
        assert receiver.wait_for_kind("accepted", timeout=10) is not None

        assert runner.cancel(handle_id) is True

        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=30)
        assert terminal is not None
        assert terminal["kind"] == "failed"

        preserve_block = terminal["payload"]["preserve"]
        assert preserve_block["attempted"] is False
        assert preserve_block["branch"] is None
        assert preserve_block["reason"]
        assert "nothing to preserve" in preserve_block["reason"]
    finally:
        receiver.close()
        _reap(pid)
