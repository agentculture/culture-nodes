import os
import signal
import time
from pathlib import Path

from pi_bridge import async_runner, mapping, pi_cli
from pi_bridge.config import Config

HERE = Path(__file__).parent
FAKE = HERE / "fake_pi.py"
FIXTURES = HERE / "fixtures" / "pi"


def read(name):
    return (FIXTURES / name).read_text(encoding="utf-8")


def test_argv_is_documented_surface_only():
    cfg = Config(pi_bin="/opt/pi", provider="anthropic", model="claude-sonnet")
    assert pi_cli.build_argv(cfg, "do it") == [
        "/opt/pi",
        "--mode",
        "json",
        "-p",
        "do it",
        "--no-session",
        "-a",
        "--provider",
        "anthropic",
        "--model",
        "claude-sonnet",
    ]
    assert "--no-extensions" not in pi_cli.build_argv(cfg, "do it")


def test_input_model_overrides_configured_model():
    cfg = Config(model="configured")
    assert pi_cli.build_argv(cfg, "x", model="override")[-2:] == ["--model", "override"]


def test_message_end_usage_and_agent_end_are_authoritative():
    result = pi_cli.parse_session(read("success.jsonl"))
    assert result["status"] == "ok"
    assert result["usage"] == {"input_tokens": 11, "output_tokens": 7}
    assert result["model"] == "test/model"


def test_failed_tool_still_classifies_from_agent_end():
    result = pi_cli.parse_session(read("failed-tool.jsonl"))
    assert result["status"] == "ok"


def test_missing_agent_end_is_incomplete_never_ok():
    result = pi_cli.parse_session(read("incomplete.jsonl"))
    assert result["status"] == "incomplete"
    ctx = mapping.InvocationContext(run_id="w", node_run_id="n", attempt_id="a")
    assert not mapping.classify(result, ctx, default_success_outcome="completed").domain


def test_spawn_uses_repo_cwd_environment_and_new_process_group(tmp_path):
    cfg = Config(pi_bin=str(FAKE), pi_env={"FAKE_PI_FIXTURE": str(FIXTURES / "success.jsonl")})
    proc = pi_cli.spawn(cfg, "x", str(tmp_path))
    assert os.getpgid(proc.pid) == proc.pid
    stdout, _ = proc.communicate(timeout=5)
    assert pi_cli.parse_session(stdout)["status"] == "ok"


def test_cancel_kills_process_group_and_transcript_survives(tmp_path):
    pid_file = tmp_path / "child.pid"
    cfg = Config(
        pi_bin=str(FAKE),
        state_dir=str(tmp_path / "state"),
        sync_timeout_seconds=0.15,
        pi_env={
            "FAKE_PI_FIXTURE": str(FIXTURES / "incomplete.jsonl"),
            "FAKE_PI_SLEEP_CHILD": "1",
            "FAKE_PI_CHILD_PID_FILE": str(pid_file),
            "FAKE_PI_HANG": "1",
        },
    )
    result = pi_cli.run_sync(cfg, "x", str(tmp_path))
    assert result.timed_out
    assert result.task_result["status"] == "incomplete"
    transcripts = list((cfg.state_path / "pi-transcripts").glob("*.jsonl"))
    assert transcripts and transcripts[0].read_text(encoding="utf-8")
    child_pid = int(pid_file.read_text())
    for _ in range(50):
        try:
            os.kill(child_pid, 0)
        except ProcessLookupError:
            break
        time.sleep(0.02)
    else:
        # A reaped-or-zombie process is gone for execution purposes; Linux may
        # retain the proc entry until PID 1 reaps it.
        stat = Path(f"/proc/{child_pid}/stat").read_text().split()[2]
        assert stat == "Z"


def test_second_timeout_never_sigkills_and_maps_to_timeout_class(tmp_path):
    """A pi process that ignores SIGTERM survives — the family never SIGKILLs.

    Mirrors claude_cli.py / codex_cli.py: the second ``communicate`` after
    ``terminate_group`` also raises ``TimeoutExpired`` when the child ignores
    SIGTERM, and ``run_sync`` must report the timeout honestly (empty
    stdout, explanatory stderr, ``timed_out=True``) rather than escalating to
    SIGKILL. The handler then maps that onto ``mapping.CLASS_TIMEOUT``.
    """
    pid_file = tmp_path / "fake.pid"
    cfg = Config(
        pi_bin=str(FAKE),
        state_dir=str(tmp_path / "state"),
        sync_timeout_seconds=0.15,
        pi_env={
            "FAKE_PI_FIXTURE": str(FIXTURES / "incomplete.jsonl"),
            "FAKE_PI_HANG": "1",
            "FAKE_PI_IGNORE_SIGTERM": "1",
            "FAKE_PI_CHILD_PID_FILE": str(pid_file),
        },
    )
    try:
        result = pi_cli.run_sync(cfg, "x", str(tmp_path))

        assert result.timed_out
        assert result.stdout == ""
        assert "did not exit after SIGTERM" in result.stderr

        # The pid file is written before the fake replays its fixture, so by
        # the time run_sync has given up waiting the fake has long since
        # started and recorded itself.
        fake_pid = int(pid_file.read_text())
        os.kill(fake_pid, 0)  # raises ProcessLookupError if the fake died

        ctx = mapping.InvocationContext(run_id="w", node_run_id="n", attempt_id="a")
        response = mapping.sync_response(
            result.task_result,
            ctx,
            default_success_outcome="completed",
            actor_id="pi",
            created_at="2026-09-05T00:00:00Z",
            timed_out=result.timed_out,
        )
        assert response.body["class"] == mapping.CLASS_TIMEOUT
    finally:
        try:
            fake_pid = int(pid_file.read_text())
        except (FileNotFoundError, ValueError):
            pass
        else:
            try:
                os.killpg(fake_pid, signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                try:
                    os.kill(fake_pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass


def test_usage_maps_to_actor_contract():
    usage = mapping.usage_from_task_result(pi_cli.parse_session(read("success.jsonl")))
    assert usage["input_tokens"] == 11 and usage["output_tokens"] == 7
    assert usage["model"] == "test/model"


def test_async_dispatch_cancel_is_cancelled_kills_group_and_keeps_transcript(tmp_path, monkeypatch):
    pid_file = tmp_path / "child.pid"
    events = []

    class Emitter:
        def __init__(self, *args, **kwargs):
            pass

        def send(self, kind, payload):
            events.append((kind, payload))
            return True

    monkeypatch.setattr(async_runner, "CallbackEmitter", Emitter)
    cfg = Config(
        pi_bin=str(FAKE),
        state_dir=str(tmp_path / "state"),
        async_wait_seconds=5,
        poll_interval_seconds=0.01,
        preserve_on_failure=False,
        pi_env={
            "FAKE_PI_FIXTURE": str(FIXTURES / "incomplete.jsonl"),
            "FAKE_PI_SLEEP_CHILD": "1",
            "FAKE_PI_CHILD_PID_FILE": str(pid_file),
            "FAKE_PI_HANG": "1",
        },
    )
    runner = async_runner.AsyncRunner(cfg)
    invocation_id = runner.start(
        instruction="x",
        repo=str(tmp_path),
        model=None,
        sandbox="workspace-write",
        mode=None,
        ctx=mapping.InvocationContext(run_id="r"),
        callback_url="http://unused",
        callback_token="unused",
        heartbeat_after_seconds=1,
    )
    for _ in range(100):
        if pid_file.exists() and list((cfg.state_path / "pi-transcripts").glob("*.jsonl")):
            break
        time.sleep(0.01)
    assert runner.cancel(invocation_id)
    for _ in range(200):
        terminal = [item for item in events if item[0] == "failed"]
        if terminal:
            break
        time.sleep(0.01)
    assert terminal[0][1]["termination_reason"] == "cancelled"
    transcript = cfg.state_path / "pi-transcripts" / f"{invocation_id}.jsonl"
    assert transcript.exists()
    child_pid = int(pid_file.read_text())
    try:
        os.kill(child_pid, 0)
    except ProcessLookupError:
        pass
    else:
        assert Path(f"/proc/{child_pid}/stat").read_text().split()[2] == "Z"


def test_parse_session_puts_the_visible_answer_in_summary_not_thinking():
    # #299: a clean pi turn whose answer lives in a text content block must
    # surface that text as `summary` (so the completed outcome and the
    # proposed claim carry it); thinking blocks are excluded.
    from pi_bridge import pi_cli

    stdout = "\n".join(
        [
            '{"type":"message_end","message":{"model":"m","usage":{"input":5,"output":3},'
            '"content":[{"type":"thinking","thinking":"let me think"},'
            '{"type":"text","text":"the answer"}]}}',
            '{"type":"agent_end","messages":[{"role":"assistant","content":['
            '{"type":"thinking","thinking":"let me think"},{"type":"text","text":"the answer"}]}]}',
        ]
    )
    result = pi_cli.parse_session(stdout)
    assert result is not None
    assert result["status"] == "ok"
    assert result["summary"] == "the answer"
    assert "let me think" not in result["summary"]
