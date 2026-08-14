from __future__ import annotations

import os
import time

import pytest

from claude_code_bridge import claude_cli
from claude_code_bridge.config import Config

from ._fakes import fake_claude_path


def _cfg(**overrides) -> Config:
    base = dict(claude_bin=fake_claude_path())
    base.update(overrides)
    return Config(**base)


# ---------------------------------------------------------------------------
# parse_version / parse_task_result / find_terminal_result / is_pid_alive
# ---------------------------------------------------------------------------


def test_parse_version_reads_leading_semver():
    assert claude_cli.parse_version("2.1.226 (Claude Code)") == (2, 1, 226)


def test_parse_version_none_on_garbage():
    assert claude_cli.parse_version("not a version") is None
    assert claude_cli.parse_version("") is None


def test_parse_task_result_reads_the_last_json_line():
    stdout = "\n\n" + '{"type": "result", "subtype": "success"}' + "\n"
    parsed = claude_cli.parse_task_result(stdout)
    assert parsed == {"type": "result", "subtype": "success"}


def test_parse_task_result_none_on_non_json_output():
    assert claude_cli.parse_task_result("not json at all") is None


def test_parse_task_result_none_on_empty_output():
    assert claude_cli.parse_task_result("") is None
    assert claude_cli.parse_task_result("\n\n\n") is None


def test_parse_task_result_none_when_last_line_is_a_json_array_not_object():
    assert claude_cli.parse_task_result("[1, 2, 3]") is None


def test_find_terminal_result_picks_the_result_typed_line():
    lines = [
        '{"type": "system", "subtype": "progress"}',
        "not json",
        '{"type": "result", "subtype": "success", "session_id": "s1"}',
    ]
    result = claude_cli.find_terminal_result(lines)
    assert result == {"type": "result", "subtype": "success", "session_id": "s1"}


def test_find_terminal_result_none_when_absent():
    assert claude_cli.find_terminal_result(['{"type": "system"}']) is None


def test_is_pid_alive_true_for_self():
    assert claude_cli.is_pid_alive(os.getpid()) is True


def test_is_pid_alive_false_for_bogus_pid():
    assert claude_cli.is_pid_alive(0) is False
    assert claude_cli.is_pid_alive(-1) is False


def test_is_pid_alive_false_for_a_pid_that_almost_certainly_does_not_exist():
    assert claude_cli.is_pid_alive(2**30) is False


# ---------------------------------------------------------------------------
# _common_argv() resume wiring (task t5, acceptance #1): a prior
# continuation_ref pins the exact `--resume <id>` argv; its absence leaves
# argv exactly as it was before t5 (cold start, no --resume at all).
# ---------------------------------------------------------------------------


def test_common_argv_adds_resume_flag_with_a_prior_continuation_ref():
    argv = claude_cli._common_argv(
        "keep going",
        output_format="json",
        permission_mode="bypassPermissions",
        role=None,
        max_steps=None,
        model=None,
        continuation_ref="sess-prior-1",
    )
    assert "--resume" in argv
    assert argv[argv.index("--resume") + 1] == "sess-prior-1"


def test_common_argv_omits_resume_without_a_prior_continuation_ref():
    argv = claude_cli._common_argv(
        "start fresh",
        output_format="json",
        permission_mode="bypassPermissions",
        role=None,
        max_steps=None,
        model=None,
    )
    assert "--resume" not in argv


def test_run_sync_passes_continuation_ref_through_to_resume_argv(monkeypatch, tmp_path):
    """End-to-end through run_sync's own argv assembly (not just
    _common_argv in isolation) — pins that the parameter actually reaches
    the subprocess boundary, closing the gap the seed left: `continuation_ref`
    was accepted as a keyword argument but never threaded into anything that
    called `_common_argv` with it from a real dispatch."""
    captured = {}
    real_popen = claude_cli.subprocess.Popen

    def spy_popen(argv, **kwargs):
        captured["argv"] = argv
        return real_popen(argv, **kwargs)

    monkeypatch.setattr(claude_cli.subprocess, "Popen", spy_popen)
    cfg = _cfg()
    claude_cli.run_sync(cfg, "resume me", str(tmp_path), continuation_ref="sess-resume-xyz")
    assert "--resume" in captured["argv"]
    assert captured["argv"][captured["argv"].index("--resume") + 1] == "sess-resume-xyz"


# ---------------------------------------------------------------------------
# role_is_known
# ---------------------------------------------------------------------------


def test_role_is_known_rejects_unknown_role_with_no_override(tmp_path):
    assert claude_cli.role_is_known(tmp_path, "totally-made-up") is False


def test_role_is_known_accepts_a_repo_declared_override(tmp_path):
    agents_dir = tmp_path / ".claude" / "agents"
    agents_dir.mkdir(parents=True)
    (agents_dir / "custom-role.md").write_text("# custom role\n")
    assert claude_cli.role_is_known(tmp_path, "custom-role") is True


# ---------------------------------------------------------------------------
# version gate — acceptance criterion #3: dispatch below the pinned minimum
# is refused with an honest error naming BOTH versions.
# ---------------------------------------------------------------------------


def test_probe_version_reads_the_fake_binary(monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    cfg = _cfg()
    raw = claude_cli.probe_version(cfg)
    assert raw.startswith("2.1.226")


def test_probe_version_raises_when_binary_is_missing():
    cfg = _cfg(claude_bin="/no/such/claude-binary-xyz")
    with pytest.raises(claude_cli.ClaudeVersionProbeError):
        claude_cli.probe_version(cfg)


def test_ensure_supported_version_passes_when_at_minimum(monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.220")
    cfg = _cfg(min_claude_version="2.1.220")
    claude_cli.ensure_supported_version(cfg)  # must not raise


def test_ensure_supported_version_passes_when_above_minimum(monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    cfg = _cfg(min_claude_version="2.1.220")
    claude_cli.ensure_supported_version(cfg)  # must not raise


def test_dispatch_below_pinned_minimum_is_refused_naming_both_versions(monkeypatch):
    """The task's own named acceptance test."""
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.219")
    cfg = _cfg(min_claude_version="2.1.220")
    with pytest.raises(claude_cli.UnsupportedClaudeVersionError) as excinfo:
        claude_cli.ensure_supported_version(cfg)
    err = excinfo.value
    assert err.detected.startswith("2.1.219")
    assert err.minimum == "2.1.220"
    # The error is "honest": both the version the bridge found and the
    # version it requires are named in the message text an operator reads.
    assert "2.1.219" in str(err)
    assert "2.1.220" in str(err)


def test_dispatch_far_below_pinned_minimum_is_refused():
    cfg = _cfg(min_claude_version="2.1.220")
    with pytest.MonkeyPatch.context() as mp:
        mp.setenv("FAKE_CLAUDE_VERSION", "1.9.999")
        with pytest.raises(claude_cli.UnsupportedClaudeVersionError):
            claude_cli.ensure_supported_version(cfg)


def test_version_that_cannot_be_parsed_fails_closed(monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "not-a-version")
    cfg = _cfg(min_claude_version="2.1.220")
    with pytest.raises(claude_cli.ClaudeVersionProbeError):
        claude_cli.ensure_supported_version(cfg)


def test_run_sync_refuses_dispatch_below_pinned_minimum_without_ever_invoking_print(
    monkeypatch, tmp_path
):
    """The version gate must be enforced BEFORE the actual `-p` dispatch —
    a too-old CLI must never get as far as running the invocation."""
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "1.0.0")
    cfg = _cfg(min_claude_version="2.1.220")
    with pytest.raises(claude_cli.UnsupportedClaudeVersionError):
        claude_cli.run_sync(cfg, "do something", str(tmp_path))


def test_spawn_background_refuses_dispatch_below_pinned_minimum(monkeypatch, tmp_path):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "1.0.0")
    cfg = _cfg(min_claude_version="2.1.220", state_dir=str(tmp_path / "state"))
    with pytest.raises(claude_cli.UnsupportedClaudeVersionError):
        claude_cli.spawn_background(cfg, "do something", str(tmp_path))


# ---------------------------------------------------------------------------
# run_sync() against the fake subprocess
# ---------------------------------------------------------------------------


def test_run_sync_success(monkeypatch, tmp_path):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_SUBTYPE", "success")
    monkeypatch.setenv("FAKE_CLAUDE_RESULT_TEXT", "hello world")
    cfg = _cfg()
    result = claude_cli.run_sync(cfg, "say hello", str(tmp_path))
    assert result.timed_out is False
    assert result.exit_code == 0
    assert result.task_result["subtype"] == "success"
    assert result.task_result["result"] == "hello world"


def test_run_sync_crashed_session_never_success(monkeypatch, tmp_path):
    """Acceptance criterion #2 exercised at the subprocess-dispatch layer:
    a crashed claude session must map to a missing (None) task_result, never
    to a fabricated success."""
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_CRASH", "1")
    cfg = _cfg()
    result = claude_cli.run_sync(cfg, "say hello", str(tmp_path))
    assert result.exit_code != 0
    assert result.task_result is None


def test_run_sync_incomplete_session_never_success(monkeypatch, tmp_path):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_SUBTYPE", "error_max_turns")
    monkeypatch.setenv("FAKE_CLAUDE_IS_ERROR", "1")
    cfg = _cfg()
    result = claude_cli.run_sync(cfg, "say hello", str(tmp_path))
    assert result.task_result["subtype"] == "error_max_turns"
    assert result.task_result["is_error"] is True


def test_run_sync_sigterms_a_hung_process_on_timeout(monkeypatch, tmp_path):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_HANG", "1")
    cfg = _cfg(sync_timeout_seconds=0.3)
    start = time.monotonic()
    result = claude_cli.run_sync(cfg, "hang forever", str(tmp_path))
    elapsed = time.monotonic() - start
    assert result.timed_out is True
    assert result.task_result is None
    # SIGTERM is the default disposition for the fake script (it does not
    # ignore it unless FAKE_CLAUDE_IGNORE_SIGTERM=1), so the process exits
    # promptly rather than this test waiting out the full grace period.
    assert elapsed < 15


# ---------------------------------------------------------------------------
# spawn_background() against the fake subprocess
# ---------------------------------------------------------------------------


def test_spawn_background_returns_a_handle_and_writes_a_feed_file(monkeypatch, tmp_path):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_STREAM_DELAY", "0.01")
    cfg = _cfg(state_dir=str(tmp_path / "state"))
    start = claude_cli.spawn_background(cfg, "do something", str(tmp_path))
    assert start.handle_id.startswith("cc_")
    assert start.pid > 0

    deadline = time.monotonic() + 10
    result = None
    while time.monotonic() < deadline:
        result = claude_cli.read_background_result(cfg, start.handle_id)
        if result is not None:
            break
        time.sleep(0.05)
    assert result is not None
    assert result["type"] == "result"
    assert result["subtype"] == "success"

    # The subprocess is detached (start_new_session=True); by the time it
    # finished writing its terminal result, it has already exited.
    deadline = time.monotonic() + 5
    while claude_cli.is_pid_alive(start.pid) and time.monotonic() < deadline:
        time.sleep(0.05)
    assert claude_cli.is_pid_alive(start.pid) is False


def test_spawn_background_dispatch_error_when_binary_missing(tmp_path):
    cfg = _cfg(
        claude_bin="/no/such/claude-binary-xyz",
        state_dir=str(tmp_path / "state"),
        min_claude_version="0.0.0",
    )
    # With min_claude_version 0.0.0, ensure_supported_version's own probe
    # will itself fail closed with ClaudeVersionProbeError before
    # spawn_background gets anywhere near Popen — assert that, since a
    # missing binary can never pass the version gate either.
    with pytest.raises(claude_cli.ClaudeVersionProbeError):
        claude_cli.spawn_background(cfg, "do something", str(tmp_path))


def test_read_background_result_none_when_feed_absent(tmp_path):
    cfg = _cfg(state_dir=str(tmp_path / "state"))
    assert claude_cli.read_background_result(cfg, "no-such-handle") is None
