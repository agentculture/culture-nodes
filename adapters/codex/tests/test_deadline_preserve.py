"""Task t13: a session stopped by the control plane's DEADLINE still lands
its work on a preserve branch.

Why this file exists at all, given `test_async_runner.py` already has a
preserve test and a cancellation test. Those two pin different halves and
neither pins the case #78 was actually filed about:

* `test_error_session_preserves_workspace_changes_on_failure` preserves, but
  the session ENDS BY ITSELF (a `turn.failed` event) and the workspace was
  dirtied by the test, not by the session.
* `test_crashed_session_delivers_a_failed_terminal_event_never_completed`
  cancels a live session, but against a CLEAN workspace, so a preserve that
  silently did nothing would have passed it unchanged.

The deadline path is the composition: a session that is still working, that
has already changed the repository, is stopped from outside by a cancel it
did not ask for. That is what `internal/scheduler` now sends when a node
run's deadline expires (task t9) -- POST /invocations/{id}/cancel, which the
server hands straight to `AsyncRunner.cancel`. Everything downstream of that
call in this test is production code: a real subprocess gets a real SIGTERM,
the runner measures the real workspace afterwards, and
`preserve.preserve_on_failure` runs against a real git repository.

The assertion is deliberately a REF IN THE REPOSITORY, not the bridge's own
`preserve` payload. A payload is the bridge's claim about what it did (PRD
§10.4); `git rev-parse` on the branch it names is the fact. #78's
`preserve_branch: None` is precisely a case where the claim would have been
believed and the ref never existed.
"""

from __future__ import annotations

import subprocess
import time
from pathlib import Path

from codex_bridge import mapping
from codex_bridge.async_runner import AsyncRunner
from codex_bridge.config import Config

from ._fakes import FakeCallbackReceiver


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


def _cfg(fake_codex: Path, tmp_path: Path, repo: Path, **overrides) -> Config:
    kwargs = dict(
        codex_bin=str(fake_codex),
        codex_env={"FAKE_CODEX_BEHAVIOR": "dirty_workspace_then_hang_clean_exit_zero_on_sigterm"},
        repo_allowlist=(str(repo),),
        state_dir=str(tmp_path / "state"),
        heartbeat_after_seconds=1,
        poll_interval_seconds=0.05,
        # Deliberately far beyond any test's patience: the bridge's OWN wait
        # bound must not be what ends this session. The only thing that stops
        # it is the cancel, which is what makes this the deadline path rather
        # than a bridge timeout wearing its clothes.
        async_wait_seconds=3600.0,
        callback_retry_backoff_seconds=0.05,
        preserve_push=False,
    )
    kwargs.update(overrides)
    return Config(**kwargs)


def _wait_for_dirty_workspace(repo: Path, timeout: float = 10.0) -> None:
    """Block until the session has actually written into the repository.

    Cancelling before the session has changed anything would test a clean
    workspace by accident -- and a clean workspace is the one case where
    preserve correctly does nothing, so the test would pass while proving
    the opposite of what it claims.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status = subprocess.run(
            ["git", "status", "--porcelain"], cwd=repo, capture_output=True, text=True
        ).stdout
        if status.strip():
            return
        time.sleep(0.05)
    raise AssertionError(f"the session never dirtied {repo} within {timeout}s")


def test_deadline_cancel_of_a_dirty_session_leaves_a_preserve_ref(fake_codex, tmp_path):
    repo = _repo_with_one_commit(tmp_path)
    cfg = _cfg(fake_codex, tmp_path, repo)
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        ctx = mapping.InvocationContext(run_id="r-deadline", node_run_id="nr-1", attempt_id="a-1")
        invocation_id = runner.start(
            instruction="start something long",
            repo=str(repo),
            model=None,
            sandbox=None,
            ctx=ctx,
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
        )
        assert receiver.wait_for_kind("accepted", timeout=10) is not None
        _wait_for_dirty_workspace(repo)

        # The deadline fires. internal/scheduler resolves the pending
        # invocation, resolves the endpoint, and issues Cancel; the bridge's
        # HTTP layer hands it to exactly this call.
        assert runner.cancel(invocation_id) is True

        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=20)
        assert terminal is not None
        # A cancelled session is a technical failure, never a domain outcome
        # -- and only the failed branch routes through preserve at all.
        assert terminal["kind"] == "failed"

        preserve_block = terminal["payload"]["preserve"]
        assert preserve_block["attempted"] is True, preserve_block.get("reason")
        assert preserve_block["committed"] is True, preserve_block.get("reason")
        branch = preserve_block["branch"]
        assert branch

        # The fact, not the claim: the branch the bridge named is a real ref
        # in this repository, and the work the session was stopped in the
        # middle of is reachable from it.
        ref = _git(repo, "rev-parse", "--verify", f"refs/heads/{branch}").stdout.strip()
        assert ref == preserve_block["commit"]
        shown = _git(repo, "show", "--stat", ref).stdout
        assert "work-in-progress.txt" in shown
        blob = _git(repo, "show", f"{ref}:work-in-progress.txt").stdout
        assert "half-finished work" in blob
    finally:
        receiver.close()


def test_deadline_cancel_of_a_clean_session_reports_why_it_preserved_nothing(fake_codex, tmp_path):
    """The other half of honesty: when there is genuinely nothing to
    preserve, the bridge says so with a reason rather than reporting an
    absent branch that a reader cannot tell from a preserve that failed.

    Note what this does NOT yet prove. `attempted:false` plus its reason
    reaches the terminal event and stops there: `internal/actors`'
    `Preserve.ToEngine` only carries a COMMITTED branch onto the attempt
    row, so the attempts table still shows all-NULL preserve columns for
    this case. Persisting the honest negative is task t13's third
    acceptance bullet and is not implemented -- see the delivery note.
    """
    repo = _repo_with_one_commit(tmp_path)
    cfg = _cfg(
        fake_codex,
        tmp_path,
        repo,
        codex_env={"FAKE_CODEX_BEHAVIOR": "hang_then_clean_exit_zero_on_sigterm"},
    )
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        ctx = mapping.InvocationContext(run_id="r-deadline", node_run_id="nr-2", attempt_id="a-2")
        invocation_id = runner.start(
            instruction="start something long",
            repo=str(repo),
            model=None,
            sandbox=None,
            ctx=ctx,
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
        )
        assert receiver.wait_for_kind("accepted", timeout=10) is not None
        assert receiver.wait_for_kind("progress", timeout=10) is not None

        assert runner.cancel(invocation_id) is True

        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=20)
        assert terminal is not None
        assert terminal["kind"] == "failed"

        preserve_block = terminal["payload"]["preserve"]
        assert preserve_block["attempted"] is False
        assert preserve_block["branch"] is None
        assert preserve_block["reason"]
        assert "nothing to preserve" in preserve_block["reason"]
    finally:
        receiver.close()
