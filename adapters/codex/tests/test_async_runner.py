"""Unit tests for the async invocation lifecycle, against the REAL
subprocess boundary via the `fake_codex` fixture (conftest.py) — no
monkeypatching of `codex_cli` here, so these tests exercise the actual
Popen-owning, stdout-streaming, SIGTERM-cancelling code path.

The two tests that matter most for this task's acceptance criterion:
`test_crashed_session_delivers_a_failed_terminal_event_never_completed` and
`test_cancellation_terminates_the_owned_subprocess_directly` — the latter
proving cancellation on this bridge is a direct SIGTERM to a subprocess this
runner owns (no file-based control plane to write to, unlike
colleague-bridge's `flightfiles.write_stop`).
"""

from __future__ import annotations

from codex_bridge import mapping
from codex_bridge.async_runner import AsyncRunner
from codex_bridge.config import Config

from ._fakes import FakeCallbackReceiver


def _cfg(fake_codex, tmp_path, *, behavior, **overrides):
    kwargs = dict(
        codex_bin=str(fake_codex),
        codex_env={"FAKE_CODEX_BEHAVIOR": behavior},
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        heartbeat_after_seconds=1,
        poll_interval_seconds=0.05,
        async_wait_seconds=30.0,
        callback_retry_backoff_seconds=0.05,
    )
    kwargs.update(overrides)
    return Config(**kwargs)


def test_ok_session_delivers_accepted_then_completed(fake_codex, tmp_path):
    cfg = _cfg(fake_codex, tmp_path, behavior="ok")
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        ctx = mapping.InvocationContext(run_id="r1", node_run_id="nr1", attempt_id="a1")
        invocation_id = runner.start(
            instruction="say hi",
            repo=str(tmp_path),
            model=None,
            sandbox=None,
            ctx=ctx,
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
        )
        assert invocation_id

        accepted = receiver.wait_for_kind("accepted", timeout=10)
        assert accepted is not None
        assert accepted["payload"]["invocation_id"] == invocation_id

        completed = receiver.wait_for_kind("completed", timeout=10)
        assert completed is not None
        assert completed["sequence"] > accepted["sequence"]
        assert completed["payload"]["outcome"] == "completed"
        assert completed["payload"]["ledger_delta"]["records"][0]["authority"] == "proposed"
    finally:
        receiver.close()


def test_error_session_delivers_failed_terminal_event(fake_codex, tmp_path):
    cfg = _cfg(fake_codex, tmp_path, behavior="error")
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        ctx = mapping.InvocationContext()
        runner.start(
            instruction="say hi",
            repo=str(tmp_path),
            model=None,
            sandbox=None,
            ctx=ctx,
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
        )
        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=10)
        assert terminal is not None
        assert terminal["kind"] == "failed"
        assert terminal["payload"]["class"] == mapping.CLASS_EXECUTION
    finally:
        receiver.close()


def test_crashed_session_delivers_a_failed_terminal_event_never_completed(fake_codex, tmp_path):
    """The acceptance-criterion test at the async-runner layer: cancel a
    live invocation (mirroring a real crash/kill) and assert the terminal
    event this bridge delivers is ALWAYS 'failed', never 'completed' — the
    fake process mirrors real codex's own measured SIGTERM behavior (exit 0,
    no terminal turn event)."""
    cfg = _cfg(
        fake_codex,
        tmp_path,
        behavior="hang_then_clean_exit_zero_on_sigterm",
        async_wait_seconds=3600.0,
    )
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        ctx = mapping.InvocationContext()
        invocation_id = runner.start(
            instruction="do something slow",
            repo=str(tmp_path),
            model=None,
            sandbox=None,
            ctx=ctx,
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
        )
        accepted = receiver.wait_for_kind("accepted", timeout=10)
        assert accepted is not None

        # Give the fake process a moment to emit its non-terminal progress
        # events, then cancel — mirroring an operator/control-plane
        # cancellation of a hung run.
        progress = receiver.wait_for_kind("progress", timeout=10)
        assert progress is not None

        cancelled = runner.cancel(invocation_id)
        assert cancelled is True

        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=15)
        assert terminal is not None
        assert terminal["kind"] == "failed"
        assert terminal["kind"] != "completed"
    finally:
        receiver.close()


def test_cancellation_terminates_the_owned_subprocess_directly(fake_codex, tmp_path):
    cfg = _cfg(fake_codex, tmp_path, behavior="hang_then_clean_exit_zero_on_sigterm")
    runner = AsyncRunner(cfg)
    receiver = FakeCallbackReceiver()
    try:
        ctx = mapping.InvocationContext()
        invocation_id = runner.start(
            instruction="do something slow",
            repo=str(tmp_path),
            model=None,
            sandbox=None,
            ctx=ctx,
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
        )
        receiver.wait_for_kind("accepted", timeout=10)
        # Wait for real evidence the fake child has run far enough to have
        # installed its own SIGTERM handler (it does so before printing
        # anything) — "accepted" alone only proves Popen() returned, not
        # that the child has executed any Python yet.
        assert receiver.wait_for_kind("progress", timeout=10) is not None
        with runner._lock:
            proc = runner._invocations[invocation_id].proc
        assert proc.poll() is None  # still running

        assert runner.cancel(invocation_id) is True
        assert proc.wait(timeout=10) == 0  # the fake script catches SIGTERM and exits 0
    finally:
        receiver.close()


def test_cancel_unknown_invocation_returns_false_but_does_not_raise(fake_codex, tmp_path):
    cfg = _cfg(fake_codex, tmp_path, behavior="ok")
    runner = AsyncRunner(cfg)
    assert runner.cancel("no-such-invocation") is False
