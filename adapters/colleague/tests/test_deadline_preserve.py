"""Task t13, colleague's half: a session stopped by the control plane's
DEADLINE still lands its work on a preserve branch.

t13 named only the codex and claude-code bridges, but colleague carries the
same cancel -> measure -> preserve chain, and CLAUDE.md's all-backends rule
is explicit that a capability proven in one bridge and unproven in another
is a bug rather than a scope boundary. Its cancel is also the WEAKEST of the
three, which is the argument for pinning it rather than assuming it:

* codex owns its subprocess and SIGTERMs it directly;
* claude-code's poller reads a stop file and sends the SIGTERM itself;
* colleague only WRITES the stop file. Nothing in the bridge ever signals
  the session -- the detached colleague process is expected to notice the
  request and stop on its own.

So the fake session here polls the control file and exits when it sees the
stop, which is exactly the cooperation colleague's real background loop
provides. Everything after that is production code: the poller observes the
pid gone, measures the real workspace, classifies the terminal event as
`failed`, and runs `preserve.preserve_on_failure` against a real git
repository.

The assertion is a ref in the repository, not the bridge's own `preserve`
payload -- a payload is the bridge's claim about what it did (PRD §10.4),
and #78's `preserve_branch: None` is precisely a case where believing the
claim would have been wrong.
"""

from __future__ import annotations

import os
import signal
import stat
import subprocess
import time
from pathlib import Path

from colleague_bridge import colleague_cli, flightfiles, mapping, workspace
from colleague_bridge.async_runner import AsyncRunner
from colleague_bridge.config import Config

from ._fakes import FakeCallbackReceiver

#: A stand-in for a DETACHED colleague session that has already changed the
#: workspace and is still working, cooperating with the stop request the way
#: a real one does.
#:
#: It double-forks on purpose: in production the session is detached, so the
#: bridge is not its parent and `is_pid_alive` flips to False the moment it
#: exits. A plain child of the test process would linger as a zombie after
#: exiting -- `os.kill(pid, 0)` succeeds on a zombie -- and the poller would
#: wait forever for a process that had already died.
DIRTY_SESSION_SCRIPT = """#!/usr/bin/env python3
import json
import os
import sys
import time

control_path, pid_path, write_work = sys.argv[1], sys.argv[2], sys.argv[3] == "dirty"

if os.fork() > 0:
    sys.exit(0)
os.setsid()
if write_work:
    with open("work-in-progress.txt", "w", encoding="utf-8") as fh:
        fh.write("half-finished work the session was stopped in the middle of\\n")
with open(pid_path, "w", encoding="utf-8") as fh:
    fh.write(str(os.getpid()))

deadline = time.time() + 600
while time.time() < deadline:
    try:
        with open(control_path, encoding="utf-8") as fh:
            if json.load(fh).get("stop") is True:
                sys.exit(0)
    except (OSError, ValueError):
        pass
    time.sleep(0.05)
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
    # colleague's flight files live INSIDE the repo
    # (`<repo>/.colleague/flight/`), so without this the bridge's own
    # bookkeeping would register as a workspace change and the
    # nothing-to-preserve case below could never be reached. Ignoring it is
    # what a repository colleague actually works in does.
    (repo / ".gitignore").write_text(".colleague/\n", encoding="utf-8")
    _git(repo, "add", "README.md", ".gitignore")
    _git(repo, "commit", "-q", "-m", "init")
    return repo


def _cfg(tmp_path: Path, repo: Path) -> Config:
    return Config(
        repo_allowlist=(str(repo),),
        heartbeat_after_seconds=1,
        poll_interval_seconds=0.05,
        # Far beyond any test's patience: the bridge's own wait bound must
        # not be what ends this session. Only the cancel does, which is what
        # makes this the deadline path rather than a bridge timeout.
        async_wait_seconds=3600.0,
        callback_retry_backoff_seconds=0.05,
        preserve_push=False,
    )


def _wait_for(predicate, timeout: float, what: str) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.05)
    raise AssertionError(f"{what} did not happen within {timeout}s")


def _spawn_session(tmp_path: Path, repo: Path, handle_id: str, *, dirty: bool) -> int:
    path = tmp_path / "detached-session.py"
    path.write_text(DIRTY_SESSION_SCRIPT, encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    pidfile = tmp_path / f"{handle_id}.pid"
    control = flightfiles.control_path(repo, handle_id)
    control.parent.mkdir(parents=True, exist_ok=True)
    # cwd=repo mirrors how the bridge spawns colleague, so the session's
    # edits land in the repository the dispatch named.
    subprocess.run(  # noqa: S603 - a test's own fake session
        [str(path), str(control), str(pidfile), "dirty" if dirty else "clean"],
        cwd=repo,
        check=True,
    )
    _wait_for(pidfile.exists, timeout=10, what="the detached session reporting its pid")
    return int(pidfile.read_text(encoding="utf-8").strip())


def _reap(pid: int) -> None:
    """Nothing else owns the detached session, so nothing else would clean
    it up if the test failed before the cancel."""
    try:
        os.kill(pid, signal.SIGKILL)
    except OSError:
        pass


def _start(runner: AsyncRunner, cfg: Config, repo: Path, handle_id: str, pid: int, receiver, ctx):
    handle = workspace.begin(str(repo))
    assert handle.available, handle.reason
    runner.start(
        start=colleague_cli.BackgroundStart(
            handle_id=handle_id, pid=pid, log_dir=str(repo), flight=None
        ),
        repo=str(repo),
        ctx=ctx,
        callback_url=receiver.url,
        callback_token="tok",
        heartbeat_after_seconds=cfg.heartbeat_after_seconds,
        workspace_handle=handle,
    )


def test_deadline_cancel_of_a_dirty_session_leaves_a_preserve_ref(tmp_path):
    repo = _repo_with_one_commit(tmp_path)
    cfg = _cfg(tmp_path, repo)
    handle_id = "col_deadline_dirty"
    pid = _spawn_session(tmp_path, repo, handle_id, dirty=True)
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        _start(
            runner,
            cfg,
            repo,
            handle_id,
            pid,
            receiver,
            mapping.InvocationContext(run_id="r-deadline", node_run_id="nr-1", attempt_id="a-1"),
        )
        assert receiver.wait_for_kind("accepted", timeout=10) is not None
        _wait_for(
            lambda: (repo / "work-in-progress.txt").exists(),
            timeout=10,
            what="the session writing into the repository",
        )

        # The deadline fires: internal/scheduler issues Cancel and the
        # bridge writes the cooperative stop the session is watching for.
        assert runner.cancel(handle_id) is True

        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=30)
        assert terminal is not None
        assert terminal["kind"] == "failed"

        preserve_block = terminal["payload"]["preserve"]
        assert preserve_block["attempted"] is True, preserve_block.get("reason")
        assert preserve_block["committed"] is True, preserve_block.get("reason")
        branch = preserve_block["branch"]
        assert branch

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

    As on the other two bridges, this stops at the terminal event:
    `internal/actors`' `Preserve.ToEngine` carries only a COMMITTED branch
    onto the attempt row, so `attempted:false` plus its reason is not
    persisted -- t13's third acceptance bullet, not implemented here.
    """
    repo = _repo_with_one_commit(tmp_path)
    cfg = _cfg(tmp_path, repo)
    handle_id = "col_deadline_clean"
    pid = _spawn_session(tmp_path, repo, handle_id, dirty=False)
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        _start(
            runner,
            cfg,
            repo,
            handle_id,
            pid,
            receiver,
            mapping.InvocationContext(run_id="r-deadline", node_run_id="nr-2", attempt_id="a-2"),
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
