import os
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
