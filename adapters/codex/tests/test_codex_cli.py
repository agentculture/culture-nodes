"""Unit tests for the subprocess boundary: argv construction, and — the
load-bearing piece for this whole adapter — `parse_session()`'s JSONL ->
TaskResult-shaped classification.

The three JSONL fixtures below (`OK_TRANSCRIPT`, `ERROR_TRANSCRIPT`,
`CRASHED_TRANSCRIPT`) are not invented: they are lightly-trimmed copies of
real `codex exec --json` output captured while grounding this adapter
against codex-cli 0.144.6 (see `codex_cli.py`'s module docstring and the
README's "What a codex session's JSONL looks like" section for the full
un-trimmed transcripts and how each was produced). `CRASHED_TRANSCRIPT` in
particular is the exact shape a SIGTERM'd codex session left behind: process
exit code 0, "Done" from the shell's own job control, and NOT ONE terminal
`turn.completed`/`turn.failed` event. That is the concrete case behind this
task's acceptance criterion: an incomplete or crashed codex session must map
to failure, never success — no adapter-specific exemption, and in
particular exit_code == 0 is never, by itself, treated as proof of success.
"""

from __future__ import annotations

import json
import sys

import pytest
from codex_bridge import codex_cli, mapping
from codex_bridge.config import Config

OK_TRANSCRIPT = "\n".join(
    [
        json.dumps({"type": "thread.started", "thread_id": "019fe54f-8e7b-7940-943c-1728fd3a7c6b"}),
        json.dumps({"type": "turn.started"}),
        json.dumps(
            {
                "type": "item.completed",
                "item": {"id": "item_0", "type": "agent_message", "text": "OK"},
            }
        ),
        json.dumps(
            {
                "type": "turn.completed",
                "usage": {
                    "input_tokens": 13880,
                    "cached_input_tokens": 9984,
                    "output_tokens": 5,
                    "reasoning_output_tokens": 0,
                },
            }
        ),
    ]
)

ERROR_TRANSCRIPT = "\n".join(
    [
        json.dumps({"type": "thread.started", "thread_id": "019fe54f-cb2c-7780-9316-46ecb958a6ed"}),
        json.dumps(
            {
                "type": "item.completed",
                "item": {
                    "id": "item_0",
                    "type": "error",
                    "message": "Model metadata for `definitely-not-a-real-model-xyz` not found.",
                },
            }
        ),
        json.dumps({"type": "turn.started"}),
        json.dumps(
            {
                "type": "error",
                "message": '{"type":"error","status":400,"error":{"type":"invalid_request_error",'
                '"message":"The \'definitely-not-a-real-model-xyz\' model is not supported."}}',
            }
        ),
        json.dumps(
            {
                "type": "turn.failed",
                "error": {
                    "message": "The 'definitely-not-a-real-model-xyz' model is not supported "
                    "when using Codex with a ChatGPT account."
                },
            }
        ),
    ]
)

# The exact tail observed after sending SIGTERM to a real, running
# `codex exec --json` mid-turn: the process reported exit code 0 (bash job
# control even logged it as "Done", not "Terminated") and yet never reached
# a terminal turn event.
CRASHED_TRANSCRIPT = "\n".join(
    [
        json.dumps({"type": "thread.started", "thread_id": "019fe553-362a-7191-aa66-6c03191830b1"}),
        json.dumps({"type": "turn.started"}),
        json.dumps(
            {
                "type": "item.completed",
                "item": {
                    "id": "item_0",
                    "type": "agent_message",
                    "text": "I'll run all six sequentially and wait for each to finish.",
                },
            }
        ),
        json.dumps(
            {
                "type": "item.started",
                "item": {
                    "id": "item_1",
                    "type": "command_execution",
                    "command": "/bin/bash -lc 'sleep 3'",
                    "aggregated_output": "",
                    "exit_code": None,
                    "status": "in_progress",
                },
            }
        ),
    ]
)

FILE_CHANGE_TRANSCRIPT = "\n".join(
    [
        json.dumps({"type": "thread.started", "thread_id": "fake-file-change"}),
        json.dumps({"type": "turn.started"}),
        json.dumps(
            {
                "type": "item.completed",
                "item": {
                    "id": "item_1",
                    "type": "file_change",
                    "changes": [{"path": "/repo/probe.txt", "kind": "add"}],
                    "status": "completed",
                },
            }
        ),
        json.dumps(
            {
                "type": "item.completed",
                "item": {"id": "item_2", "type": "agent_message", "text": "Done."},
            }
        ),
        json.dumps({"type": "turn.completed", "usage": {"input_tokens": 1, "output_tokens": 1}}),
    ]
)


# ---------------------------------------------------------------------------
# _common_argv(): the exact `codex exec` invocation this bridge generates
# ---------------------------------------------------------------------------


def test_common_argv_minimal_shape():
    argv = codex_cli._common_argv("do the thing", "/repo", model=None, sandbox="workspace-write")
    assert argv == ["exec", "--json", "--sandbox", "workspace-write", "-C", "/repo", "do the thing"]


def test_common_argv_includes_model_when_given():
    argv = codex_cli._common_argv("do the thing", "/repo", model="gpt-5-codex", sandbox="read-only")
    assert argv == [
        "exec",
        "--json",
        "--sandbox",
        "read-only",
        "-C",
        "/repo",
        "-m",
        "gpt-5-codex",
        "do the thing",
    ]


def test_common_argv_instruction_is_always_the_trailing_positional():
    argv = codex_cli._common_argv(
        "last arg please", "/repo", model="m", sandbox="danger-full-access"
    )
    assert argv[-1] == "last arg please"


def test_sandbox_modes_are_the_three_codex_declares():
    assert codex_cli.SANDBOX_MODES == frozenset(
        {"read-only", "workspace-write", "danger-full-access"}
    )


# ---------------------------------------------------------------------------
# parse_session(): JSONL transcript -> TaskResult-shaped dict
# ---------------------------------------------------------------------------


def test_parse_session_ok_transcript_is_status_ok():
    result = codex_cli.parse_session(OK_TRANSCRIPT)
    assert result["status"] == "ok"
    assert result["summary"] == "OK"
    assert result["task_id"] == "019fe54f-8e7b-7940-943c-1728fd3a7c6b"
    assert result["usage"]["input_tokens"] == 13880
    assert result["usage"]["cached_input_tokens"] == 9984
    assert result["usage"]["output_tokens"] == 5
    assert result["usage"]["reasoning_output_tokens"] == 0
    assert result["error"] is None


def test_completed_fixture_round_trips_exact_cache_counts_to_protocol_payload():
    result = codex_cli.parse_session(OK_TRANSCRIPT)
    response = mapping.sync_response(
        result,
        mapping.InvocationContext(),
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )

    assert response.body["usage"]["input_tokens"] == 13880
    assert response.body["usage"]["cached_input_tokens"] == 9984


def test_parse_session_error_transcript_is_status_error():
    result = codex_cli.parse_session(ERROR_TRANSCRIPT)
    assert result["status"] == "error"
    assert "not supported" in result["error"]
    assert result["task_id"] == "019fe54f-cb2c-7780-9316-46ecb958a6ed"


def test_failed_fixture_without_usage_round_trips_no_usage_block():
    result = codex_cli.parse_session(ERROR_TRANSCRIPT)
    response = mapping.sync_response(
        result,
        mapping.InvocationContext(),
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )

    assert response.status_code == 500
    assert "usage" not in response.body


def test_parse_session_failed_turn_captures_reported_usage_and_reason():
    transcript = "\n".join(
        [
            json.dumps({"type": "thread.started", "thread_id": "thread-failed"}),
            json.dumps(
                {
                    "type": "turn.failed",
                    "reason": "context_window_exceeded",
                    "usage": {
                        "input_tokens": 21,
                        "cached_input_tokens": 13,
                        "output_tokens": 8,
                        "reasoning_output_tokens": 5,
                    },
                    "error": {"message": "context window exceeded"},
                }
            ),
        ]
    )

    result = codex_cli.parse_session(transcript)

    assert result["status"] == "error"
    assert result["usage"] == {
        "input_tokens": 21,
        "cached_input_tokens": 13,
        "output_tokens": 8,
        "reasoning_output_tokens": 5,
    }
    assert result["termination_reason"] == "context_window_exceeded"


def test_parse_session_captures_model_only_when_stream_reports_it():
    transcript = "\n".join(
        [
            json.dumps({"type": "thread.started", "thread_id": "thread-model"}),
            json.dumps({"type": "turn.started", "model": "gpt-5.6-codex"}),
            json.dumps(
                {
                    "type": "turn.completed",
                    "usage": {"input_tokens": 2, "output_tokens": 1},
                }
            ),
        ]
    )

    result = codex_cli.parse_session(transcript)

    assert result["model"] == "gpt-5.6-codex"


def test_parse_session_incomplete_transcript_retains_interim_reported_usage():
    transcript = "\n".join(
        [
            json.dumps({"type": "thread.started", "thread_id": "thread-incomplete"}),
            json.dumps(
                {
                    "type": "item.completed",
                    "usage": {"input_tokens": 34, "output_tokens": 3},
                    "item": {"type": "agent_message", "text": "still working"},
                }
            ),
        ]
    )

    result = codex_cli.parse_session(transcript)

    assert result["status"] == "incomplete"
    assert result["usage"] == {"input_tokens": 34, "output_tokens": 3}


def test_parse_session_crashed_transcript_is_status_incomplete_never_ok():
    """The load-bearing test: exit_code is irrelevant to parse_session (it
    only ever sees stdout) — a transcript with no terminal turn event is
    ALWAYS 'incomplete', matching the real SIGTERM'd session this fixture
    was captured from."""
    result = codex_cli.parse_session(CRASHED_TRANSCRIPT)
    assert result["status"] == "incomplete"
    assert result["status"] != "ok"
    assert result["task_id"] == "019fe553-362a-7191-aa66-6c03191830b1"


def test_parse_session_exit_zero_without_terminal_event_is_incomplete_never_ok():
    """The exact regression this task's acceptance criterion names: a
    process that exited 0 is not, by itself, evidence of success. This test
    asserts the classifier never special-cases exit code — parse_session
    doesn't even take one as an argument, by design, so there is no
    adapter-specific exemption path for "well the exit code was 0"."""
    result = codex_cli.parse_session(CRASHED_TRANSCRIPT)
    assert result["status"] == "incomplete"


def test_parse_session_none_on_completely_empty_output():
    assert codex_cli.parse_session("") is None
    assert codex_cli.parse_session("\n\n\n") is None


def test_parse_session_none_on_non_json_garbage():
    assert codex_cli.parse_session("not json at all, codex crashed before writing anything") is None


def test_parse_session_skips_malformed_lines_without_raising():
    stdout = "not json\n" + OK_TRANSCRIPT
    result = codex_cli.parse_session(stdout)
    assert result["status"] == "ok"


def test_parse_session_collects_changed_files_from_completed_file_change_items():
    result = codex_cli.parse_session(FILE_CHANGE_TRANSCRIPT)
    assert result["status"] == "ok"
    assert result["changed_files"] == ["/repo/probe.txt"]


def test_parse_session_does_not_collect_failed_file_change_items():
    transcript = "\n".join(
        [
            json.dumps({"type": "thread.started", "thread_id": "t"}),
            json.dumps(
                {
                    "type": "item.completed",
                    "item": {
                        "id": "item_1",
                        "type": "file_change",
                        "changes": [{"path": "/repo/rejected.txt", "kind": "add"}],
                        "status": "failed",
                    },
                }
            ),
            json.dumps({"type": "turn.completed", "usage": {}}),
        ]
    )
    result = codex_cli.parse_session(transcript)
    assert result["changed_files"] == []


def test_parse_session_summary_is_the_last_agent_message():
    transcript = "\n".join(
        [
            json.dumps({"type": "thread.started", "thread_id": "t"}),
            json.dumps(
                {
                    "type": "item.completed",
                    "item": {"id": "i0", "type": "agent_message", "text": "first"},
                }
            ),
            json.dumps(
                {
                    "type": "item.completed",
                    "item": {"id": "i1", "type": "agent_message", "text": "final"},
                }
            ),
            json.dumps({"type": "turn.completed", "usage": {}}),
        ]
    )
    result = codex_cli.parse_session(transcript)
    assert result["summary"] == "final"


# ---------------------------------------------------------------------------
# run_sync() against a FAKE codex-shaped executable (the `fake_codex`
# fixture lives in conftest.py, shared with test_async_runner.py and
# test_server_unit.py): proves the real subprocess.Popen /
# SIGTERM-on-timeout code path (not just parse_session in isolation)
# without needing the real codex CLI — always runs in CI.
# ---------------------------------------------------------------------------


def _cfg(fake_codex, tmp_path, *, behavior, sync_timeout_seconds=300.0):
    return Config(
        codex_bin=str(fake_codex),
        codex_env={"FAKE_CODEX_BEHAVIOR": behavior},
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        sync_timeout_seconds=sync_timeout_seconds,
    )


def test_run_sync_ok_behavior_returns_ok_task_result(fake_codex, tmp_path):
    cfg = _cfg(fake_codex, tmp_path, behavior="ok")
    result = codex_cli.run_sync(cfg, "say hi", str(tmp_path))
    assert result.timed_out is False
    assert result.exit_code == 0
    assert result.task_result["status"] == "ok"


def test_run_sync_error_behavior_returns_error_task_result(fake_codex, tmp_path):
    cfg = _cfg(fake_codex, tmp_path, behavior="error")
    result = codex_cli.run_sync(cfg, "say hi", str(tmp_path))
    assert result.exit_code == 1
    assert result.task_result["status"] == "error"


def test_run_sync_crash_before_any_output_returns_none_task_result(fake_codex, tmp_path):
    cfg = _cfg(fake_codex, tmp_path, behavior="crash_before_any_output")
    result = codex_cli.run_sync(cfg, "say hi", str(tmp_path))
    assert result.exit_code == 1
    assert result.task_result is None


@pytest.mark.skipif(sys.platform == "win32", reason="SIGTERM semantics are POSIX-specific")
def test_run_sync_timeout_sends_sigterm_and_the_incomplete_session_is_never_ok(
    fake_codex, tmp_path
):
    """End-to-end proof through the REAL subprocess boundary: the bridge's
    own timeout fires, SIGTERM is sent (never SIGKILL), the fake process
    mirrors real codex by exiting 0 without a terminal event, and
    run_sync's own parse of whatever was captured must never report 'ok'."""
    cfg = _cfg(
        fake_codex,
        tmp_path,
        behavior="hang_then_clean_exit_zero_on_sigterm",
        sync_timeout_seconds=1.0,
    )
    result = codex_cli.run_sync(cfg, "say hi", str(tmp_path))
    assert result.timed_out is True
    # Whatever partial transcript was captured before SIGTERM never reached
    # a terminal event, so even if a task_result was parsed at all, it must
    # never be "ok".
    if result.task_result is not None:
        assert result.task_result["status"] != "ok"
