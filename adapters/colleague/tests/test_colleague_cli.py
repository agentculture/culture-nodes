from __future__ import annotations

import os

from colleague_bridge import colleague_cli


def test_role_is_known_accepts_builtins(tmp_path):
    for role in ("explorer", "planner", "reviewer", "validator", "writer"):
        assert colleague_cli.role_is_known(tmp_path, role) is True


def test_role_is_known_rejects_unknown_role_with_no_override(tmp_path):
    assert colleague_cli.role_is_known(tmp_path, "totally-made-up") is False


def test_role_is_known_accepts_a_repo_declared_override(tmp_path):
    agents_dir = tmp_path / ".colleague" / "agents"
    agents_dir.mkdir(parents=True)
    (agents_dir / "custom-role.md").write_text("# custom role\n")
    assert colleague_cli.role_is_known(tmp_path, "custom-role") is True


def test_parse_task_result_reads_the_last_json_line():
    stdout = "\n\n" + '{"status": "ok", "task_id": "abc"}' + "\n"
    parsed = colleague_cli._parse_task_result(stdout)
    assert parsed == {"status": "ok", "task_id": "abc"}


def test_parse_task_result_none_on_non_json_output():
    assert colleague_cli._parse_task_result("not json at all") is None


def test_parse_task_result_none_on_empty_output():
    assert colleague_cli._parse_task_result("") is None
    assert colleague_cli._parse_task_result("\n\n\n") is None


def test_parse_task_result_none_when_last_line_is_a_json_array_not_object():
    assert colleague_cli._parse_task_result("[1, 2, 3]") is None


def test_is_pid_alive_true_for_self():
    assert colleague_cli.is_pid_alive(os.getpid()) is True


def test_is_pid_alive_false_for_bogus_pid():
    # PID 0 and negative values are never real process ids.
    assert colleague_cli.is_pid_alive(0) is False
    assert colleague_cli.is_pid_alive(-1) is False


def test_is_pid_alive_false_for_a_pid_that_almost_certainly_does_not_exist():
    # A very large pid far past any real process table on a dev/test box.
    assert colleague_cli.is_pid_alive(2**30) is False


def test_read_background_result_none_when_log_absent(tmp_path):
    assert colleague_cli.read_background_result(tmp_path, "no-such-handle") is None


def test_read_background_result_parses_the_child_stdout_log(tmp_path):
    log_dir = tmp_path / ".colleague" / "background" / "abc123"
    log_dir.mkdir(parents=True)
    (log_dir / "stdout.log").write_text('{"status": "ok", "task_id": "abc123"}\n')
    result = colleague_cli.read_background_result(tmp_path, "abc123")
    assert result == {"status": "ok", "task_id": "abc123"}
