"""qwen_cli — the ACP client seam and the terminal-event classifier
(plan task t2 of qwen-bridge-acp).

Two layers, exactly like the codex sibling's test suite:

* `parse_session` in ISOLATION over the committed measured fixtures
  (tests/fixtures/acp/*.json, each with its own provenance field): the
  c16 terminal-shape table, the crash-case rule, the handshake gate and
  the mode policy as pure functions.
* `spawn` / `run_sync` against the committed fake ACP agent
  (tests/fixtures/acp/fake_acp_agent.py) - a stdlib-only stdio JSON-RPC
  replayer of the measured qwen 0.22.0 wire shapes, selected by
  FAKE_ACP_BEHAVIOR: proves the REAL subprocess boundary (Popen, pipes,
  SIGTERM, EOF, the driver child's argv) without a real qwen install or
  a live model endpoint. The fake replaces the qwen binary exactly the
  way the codex sibling's fake_codex replaces it (the fixture promoted
  to a committed, versioned artifact because its content IS the
  measured grounding).

The crash-case rule is mirrored from adapters/codex/tests/
test_codex_cli.py (the tests named after the codex originals): a qwen
process that exits 0 without a terminal session/prompt response is
incomplete, never ok - the exit code is not even an argument to
parse_session, so no adapter-specific exemption path exists.
"""

from __future__ import annotations

import json
import os
import stat
import sys
from pathlib import Path

import pytest

from qwen_bridge import mapping, qwen_cli
from qwen_bridge.config import Config

HERE = Path(__file__).resolve().parent
ACP_FIXTURES = HERE / "fixtures" / "acp"
FAKE_AGENT = ACP_FIXTURES / "fake_acp_agent.py"
SESSION_ID = "8c9f1b2e-4a6d-4e7f-9b3c-1d2e3f4a5b6c"  # the fixtures' fixed session
MEASURED_MODES = ("plan", "default", "auto-edit", "auto", "yolo")
MEASURED_MODEL = "unsloth/Qwen3.8-27B-NVFP4"

#: A repo path used by the PURE argv-shape test only; the live tests run
#: the fake agent with cwd = the test's own tmp_path (it must exist -
#: the driver Pops the grandchild there, the way a real dispatch Pops
#: qwen in the invocation's repo).
REPO = "/tmp/qwen-bridge-test-repo"


def _load(name: str) -> dict:
    with open(ACP_FIXTURES / name, encoding="utf-8") as fh:
        return json.load(fh)


def _firehose_final_text() -> str:
    """The firehose turn's final assistant text, straight from the
    fixture's non-thought agent_message_chunks (never hardcoded)."""
    updates = _load("firehose_72_updates.json")["updates"]
    return "".join(
        u["content"]["text"]
        for u in updates
        if u.get("sessionUpdate") == "agent_message_chunk" and isinstance(u.get("content"), dict)
    )


def _turn_stdout(turn_fixture: str, *, mode: str = "plan", with_handshake: bool = True) -> str:
    """Assemble the agent-to-client stdout the driver child would echo
    for one turn replayed from a committed fixture: the initialize and
    session/new responses, the post-set_mode current_mode_update echo
    the fake agent sends, the turn's session/update notifications (with
    the measured ext request injected mid-stream at the fixture's own
    position), and the terminal - or no terminal at all (crash)."""
    lines: list = []
    if with_handshake:
        lines.append(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "result": _load("initialize_measured.json")["response"]["result"],
            }
        )
        lines.append(
            {
                "jsonrpc": "2.0",
                "id": 2,
                "result": _load("session_new_measured.json")["response"]["result"],
            }
        )
        lines.append(
            {
                "jsonrpc": "2.0",
                "method": "session/update",
                "params": {
                    "sessionId": SESSION_ID,
                    "update": {"sessionUpdate": "current_mode_update", "currentModeId": mode},
                },
            }
        )
        lines.append({"jsonrpc": "2.0", "id": 3, "result": {}})
    turn = _load(turn_fixture)
    for i, update in enumerate(turn.get("updates") or [], start=1):
        lines.append(
            {
                "jsonrpc": "2.0",
                "method": "session/update",
                "params": {"sessionId": turn["session_id"], "update": update},
            }
        )
        if turn.get("ext_request") is not None and i == turn.get("ext_after"):
            lines.append(turn["ext_request"])
    terminal = turn.get("terminal")
    if turn.get("permission_request") is not None:
        lines.append(turn["permission_request"])
    if turn.get("permission_response") is not None:
        lines.append(turn["permission_response"])
    if terminal:
        line = {"jsonrpc": "2.0", "id": 4}
        line.update(terminal)
        lines.append(line)
    return "\n".join(json.dumps(item) for item in lines)


@pytest.fixture()
def fake_acp_agent(tmp_path):
    """The committed fake ACP agent, executable. The fixture's content is
    the measured grounding, so it ships in the repo (unlike fake_codex,
    which is written per-test to tmp_path); the chmod is a self-heal for
    checkouts where the 100755 mode bit was lost (it is tracked)."""
    target = FAKE_AGENT
    target.chmod(target.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return target


def _side_dir(tmp_path: Path) -> Path:
    side = tmp_path / "fake-acp-side"
    side.mkdir(parents=True, exist_ok=True)
    return side


def _cfg(
    fake_acp_agent,
    tmp_path,
    *,
    behavior,
    mode: str | None = "plan",
    sync_timeout_seconds: float = 300.0,
) -> Config:
    """One Config whose driver will spawn the fake agent with the given
    behavior - the codex sibling's _cfg discipline (env selects the
    fake's behavior; the side dir is where the fake records what the
    driver actually sent)."""
    side = _side_dir(tmp_path)
    return Config(
        qwen_bin=str(fake_acp_agent),
        qwen_env={"FAKE_ACP_BEHAVIOR": behavior, "FAKE_ACP_STATE_DIR": str(side)},
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        sync_timeout_seconds=sync_timeout_seconds,
    )


def _driver_transcript(tmp_path: Path) -> list[dict]:
    """The driver child's local transcript file (c21 retention), parsed:
    exactly one file per run_sync, each line the {dir, ts, msg}
    envelope."""
    files = list((tmp_path / "state" / "acp-transcripts").glob("*.jsonl"))
    assert len(files) == 1
    return [
        json.loads(line)
        for line in files[0].read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def _c_to_a_methods(tmp_path: Path) -> list[str]:
    return [
        entry["msg"]["method"]
        for entry in _driver_transcript(tmp_path)
        if entry["dir"] == "c->a" and "method" in (entry["msg"] or {})
    ]


# ---------------------------------------------------------------------------
# parse_session in isolation: the c16 terminal-shape table (one passing
# test per measured shape)
# ---------------------------------------------------------------------------


def test_classifier_end_turn_terminal_is_ok():
    """Measured: the firehose turn ends {stopReason: end_turn,
    _meta.qwen.branchPoint} -> ok. The exit code is invisible here."""
    result = qwen_cli.parse_session(_turn_stdout("firehose_72_updates.json"))
    assert result["status"] == "ok"
    assert result["error"] is None
    assert result["termination_reason"] == "end_turn"
    assert result["task_id"] == SESSION_ID
    assert result["summary"] == _firehose_final_text()
    assert result["model"] == MEASURED_MODEL


def test_classifier_end_turn_with_failed_tool_is_ok_not_error():
    """Measured (the c16/h13 corner): a failed tool call still ends
    end_turn with error null - the failure rides in the transcript,
    never in the run status."""
    result = qwen_cli.parse_session(_turn_stdout("end_turn_with_failed_tool.json"))
    assert result["status"] == "ok"
    assert result["error"] is None
    tool_calls = result["acp_transcript"]["tool_calls"]
    assert len(tool_calls) == 1
    assert tool_calls[0]["status"] == "failed"
    assert "No such file or directory" in tool_calls[0]["output"]


def test_permission_cancelled_then_end_turn_is_permission_blocked_not_completed():
    result = qwen_cli.parse_session(_turn_stdout("permission_blocked.json"))
    assert result["status"] == "ok"
    assert result["outcome"] == "permission_blocked"
    classification = mapping.classify(
        result, mapping.InvocationContext(), default_success_outcome="completed"
    )
    assert classification.domain is True
    assert classification.outcome == "permission_blocked"
    response = mapping.sync_response(
        {**result, "summary": '{"outcome":"completed","output":{}}'},
        mapping.InvocationContext(),
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert response.body["outcome"] == "permission_blocked"


@pytest.mark.skipif(sys.platform == "win32", reason="subprocess Popen is required")
def test_driver_records_its_cancelled_permission_answer(fake_acp_agent, tmp_path):
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="permission-blocked")
    run = qwen_cli.run_sync(cfg, "needs a tool", str(tmp_path), mode="plan")
    assert run.task_result["outcome"] == "permission_blocked"
    answer = json.loads(
        (_side_dir(tmp_path) / "permission_answer.json").read_text(encoding="utf-8")
    )
    assert answer["result"]["outcome"]["outcome"] == "cancelled"


def test_classifier_cancelled_terminal_is_the_13_cancellation_outcome():
    """Measured: session/cancel (a notification, no id) is answered by
    the terminal {stopReason: cancelled} -> the 13.5/13.6 cancellation
    outcome: a terminal failure with termination_reason 'cancelled',
    never ok and never incomplete."""
    result = qwen_cli.parse_session(_turn_stdout("cancelled.json"))
    assert result["status"] == "error"
    assert result["termination_reason"] == "cancelled"
    assert "cancellation outcome" in result["error"]
    classification = mapping.classify(
        result, mapping.InvocationContext(), default_success_outcome="completed"
    )
    assert classification.domain is False
    assert classification.error_class == mapping.CLASS_EXECUTION
    response = mapping.sync_response(
        result,
        mapping.InvocationContext(),
        default_success_outcome="completed",
        actor_id="t2-test",
        created_at="2026-08-23T22:00:00Z",
    )
    # the durable cancellation's own reason rides the telemetry channel
    assert response.body["termination_reason"] == "cancelled"


def test_classifier_jsonrpc_error_terminal_is_error():
    """The model/API failure leg (SYNTHESIZED fixture per frame park v4;
    the first live occurrence must be diffed against it): a JSON-RPC
    error object on the session/prompt response -> error, naming the
    code and message."""
    result = qwen_cli.parse_session(_turn_stdout("prompt_error.json"))
    assert result["status"] == "error"
    assert "JSON-RPC error on the session/prompt response" in result["error"]
    assert "-32603" in result["error"]
    assert "HTTP 500" in result["error"]


def test_classifier_no_terminal_response_is_incomplete():
    """The agent died mid-turn: no terminal session/prompt response was
    ever seen -> incomplete, never ok."""
    result = qwen_cli.parse_session(_turn_stdout("crash.json"))
    assert result["status"] == "incomplete"
    # the partial stream is retained, downsampled, for the debugging:
    # the fixture's own 4 turn notifications + the driver's set_mode
    # current_mode_update echo
    assert result["acp_transcript"]["notifications"] == 5


# ---------------------------------------------------------------------------
# the crash-case rule, mirrored from test_codex_cli.py (the RULE, not the
# JSONL shape): an exit code of 0 is never, by itself, evidence of success
# ---------------------------------------------------------------------------


def test_parse_session_crashed_transcript_is_status_incomplete_never_ok():
    """The load-bearing test: exit_code is irrelevant to parse_session
    (it only ever sees stdout) - a transcript with no terminal
    session/prompt response is ALWAYS 'incomplete'."""
    result = qwen_cli.parse_session(_turn_stdout("crash.json"))
    assert result["status"] == "incomplete"
    assert result["status"] != "ok"
    assert result["task_id"] == SESSION_ID


def test_parse_session_exit_zero_without_terminal_event_is_incomplete_never_ok():
    """The exact regression the acceptance criterion names: a process
    that exited 0 is not, by itself, evidence of success. parse_session
    doesn't even take the exit code as an argument, by design, so there
    is no adapter-specific exemption path for 'well the exit code was
    0'."""
    result = qwen_cli.parse_session(_turn_stdout("crash.json"))
    assert result["status"] == "incomplete"


def test_parse_session_none_on_completely_empty_output():
    assert qwen_cli.parse_session("") is None
    assert qwen_cli.parse_session("\n\n\n") is None


def test_parse_session_none_on_non_json_garbage():
    assert qwen_cli.parse_session("not json at all, the agent died before writing anything") is None


def test_parse_session_skips_malformed_lines_without_raising():
    stdout = "not json\n" + _turn_stdout("firehose_72_updates.json")
    result = qwen_cli.parse_session(stdout)
    assert result["status"] == "ok"


@pytest.mark.skipif(sys.platform == "win32", reason="SIGTERM semantics are POSIX-specific")
def test_run_sync_crash_behavior_exits_zero_and_is_incomplete_never_ok(fake_acp_agent, tmp_path):
    """End-to-end proof through the REAL subprocess boundary: the fake
    agent mirrors the measured crash case - it exits 0 after a partial
    stream, never emitting a terminal - and run_sync's own parse of
    whatever was captured must report incomplete, never ok."""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="crash")
    result = qwen_cli.run_sync(cfg, "a long sequential task", str(tmp_path), mode="plan")
    assert result.exit_code == 0
    assert result.timed_out is False
    assert result.task_result is not None
    assert result.task_result["status"] == "incomplete"
    assert result.task_result["status"] != "ok"


@pytest.mark.skipif(sys.platform == "win32", reason="SIGTERM semantics are POSIX-specific")
def test_run_sync_dies_before_handshake_returns_none_task_result(fake_acp_agent, tmp_path):
    """The agent dies before answering initialize: the transcript is
    empty - no parseable result at all, classified no worse than that."""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="dies-before-handshake")
    result = qwen_cli.run_sync(cfg, "say hi", str(tmp_path), mode="plan")
    assert result.exit_code == 0
    assert result.task_result is None


# ---------------------------------------------------------------------------
# the handshake gate (c19/h16): refuses BEFORE serving, with distinct
# messages
# ---------------------------------------------------------------------------


def test_handshake_accepts_the_measured_initialize():
    result = _load("initialize_measured.json")["response"]["result"]
    qwen_cli.validate_initialize(result)  # no refusal on the measured shape


def test_handshake_refuses_unsupported_protocol_version():
    """The unit test the acceptance criterion names: the measured
    initialize response with protocolVersion 2 (exactly what the fake
    agent's protocol-version-2 behavior replays)."""
    result = dict(_load("initialize_measured.json")["response"]["result"])
    result["protocolVersion"] = 2
    with pytest.raises(qwen_cli.AcpPolicyError, match="unsupported protocolVersion 2"):
        qwen_cli.validate_initialize(result)


def test_handshake_refuses_agent_info_version_mismatch():
    """A distinct refusal (not the protocolVersion one): the pinned
    agent is qwen-code 0.22.0; a disagreement fails closed."""
    result = json.loads(json.dumps(_load("initialize_measured.json")["response"]["result"]))
    result["agentInfo"]["version"] = "9.9.9"
    with pytest.raises(qwen_cli.AcpPolicyError, match="agentInfo mismatch"):
        qwen_cli.validate_initialize(result)


@pytest.mark.skipif(sys.platform == "win32", reason="subprocess Popen is required")
def test_run_sync_protocol_version_2_refuses_before_serving(fake_acp_agent, tmp_path):
    """Live: the refusal happens before any session is served - the
    driver's local transcript (c21 retention) shows initialize sent and
    NOTHING after it."""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="protocol-version-2")
    with pytest.raises(qwen_cli.QwenSeamRefusal) as excinfo:
        qwen_cli.run_sync(cfg, "say hi", str(tmp_path), mode="plan")
    assert "unsupported protocolVersion 2" in str(excinfo.value)
    assert "refusing to serve" in str(excinfo.value)
    methods = _c_to_a_methods(tmp_path)
    assert "initialize" in methods
    assert "session/new" not in methods
    assert "session/prompt" not in methods


@pytest.mark.skipif(sys.platform == "win32", reason="subprocess Popen is required")
def test_run_sync_agent_version_mismatch_refuses_before_serving(fake_acp_agent, tmp_path):
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="agent-version-mismatch")
    with pytest.raises(qwen_cli.QwenSeamRefusal) as excinfo:
        qwen_cli.run_sync(cfg, "say hi", str(tmp_path), mode="plan")
    assert "agentInfo mismatch" in str(excinfo.value)
    assert "9.9.9" in str(excinfo.value)
    methods = _c_to_a_methods(tmp_path)
    assert "session/new" not in methods


# ---------------------------------------------------------------------------
# c21: the 72-notification turn - the payload stays small, the full
# stream is retained locally
# ---------------------------------------------------------------------------


def test_parse_firehose_downsamples_the_72_notification_stream():
    """The measured ~72-notification firehose (dominated by thought
    chunks) is downsampled to its facts: the final text, the tool
    summaries with measured statuses, the usage totals, the ext
    methods, the branch point - never the raw stream."""
    turn = _load("firehose_72_updates.json")
    updates = turn["updates"]
    assert len(updates) == 72  # the fixture's own count, named by c21

    result = qwen_cli.parse_session(_turn_stdout("firehose_72_updates.json"))
    transcript = result["acp_transcript"]
    assert result["status"] == "ok"
    # the stream's session/update notifications: the fixture's own 72
    # turn notifications + the driver's set_mode current_mode_update echo
    assert transcript["notifications"] == 73
    assert transcript["thought_chunks"] == sum(
        1 for u in updates if u.get("sessionUpdate") == "agent_thought_chunk"
    )
    assert transcript["final_text"] == _firehose_final_text()
    tool_calls = transcript["tool_calls"]
    assert len(tool_calls) == 1
    assert tool_calls[0]["status"] == "completed"
    assert tool_calls[0]["output"] == "acp-probe-marker\n"
    assert transcript["usage_updates"] == {"used": 9842, "size": 32768}
    # context-window counters, NOT token counts: the mapping module's
    # 13.2 usage block stays absent
    assert result["usage"] == {}
    assert transcript["ext_methods_answered"] == ["craft/drainMidTurnQueue"]
    assert transcript["branch_point"] == turn["terminal"]["result"]["_meta"]["qwen.branchPoint"]


@pytest.mark.skipif(sys.platform == "win32", reason="subprocess Popen is required")
def test_run_sync_firehose_payload_under_64kb_and_full_stream_retained(fake_acp_agent, tmp_path):
    """The acceptance criterion, end-to-end: the 13.2 sync result
    payload for the 72-notification turn is <= 64 KB, while the FULL
    session/update stream is retained in the bridge's local transcript
    file."""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="ok")
    result = qwen_cli.run_sync(cfg, "echo acp-probe-marker", str(tmp_path), mode="plan")
    task_result = result.task_result
    assert task_result["status"] == "ok"

    response = mapping.sync_response(
        task_result,
        mapping.InvocationContext(run_id="run-t2"),
        default_success_outcome="completed",
        actor_id="t2-test",
        created_at="2026-08-23T22:00:00Z",
    )
    assert response.status_code == 200
    body = response.body
    blob = json.dumps(body, separators=(",", ":")).encode("utf-8")
    assert len(blob) <= 64 * 1024
    # the thought firehose is NOT on the wire: the final text is the
    # only transcript content in the payload
    assert body["output"]["summary"] == _firehose_final_text()
    assert "fragment 1" not in body["output"]["summary"]
    raw_updates = _load("firehose_72_updates.json")["updates"]
    assert len(json.dumps(raw_updates).encode("utf-8")) > len(blob)
    # the session's own id is the continuation_ref the bridge offers back
    assert body["continuation_ref"] == SESSION_ID

    # ...while the local transcript file retains the FULL stream
    assert result.transcript_path is not None
    entries = _driver_transcript(tmp_path)
    a_to_c_updates = [
        e
        for e in entries
        if e["dir"] == "a->c" and (e["msg"] or {}).get("method") == "session/update"
    ]
    # the 72 turn notifications, verbatim, + the fake agent's own
    # current_mode_update echo after set_mode
    assert len(a_to_c_updates) == 73
    fixture_updates = [e["msg"]["params"]["update"] for e in a_to_c_updates]
    for update in _load("firehose_72_updates.json")["updates"]:
        assert update in fixture_updates  # every measured notification retained
    a_to_c = [e["msg"] for e in entries if e["dir"] == "a->c"]
    assert any((m or {}).get("method") == "craft/drainMidTurnQueue" for m in a_to_c)
    assert any((m or {}).get("id") == 4 and "result" in (m or {}) for m in a_to_c)
    methods = _c_to_a_methods(tmp_path)
    assert methods[:4] == ["initialize", "session/new", "session/set_mode", "session/prompt"]


# ---------------------------------------------------------------------------
# the mode policy (c18/h15): set from the input/preflight policy at
# session creation, never a fallback to the measured default
# ---------------------------------------------------------------------------


def test_mode_policy_no_mode_never_falls_back_to_measured_default():
    """The measured scratch-session default is 'auto' - the bridge must
    refuse to serve a session it did not set."""
    with pytest.raises(qwen_cli.AcpPolicyError, match="no session mode given"):
        qwen_cli.resolve_acp_mode(None, list(MEASURED_MODES))
    with pytest.raises(qwen_cli.AcpPolicyError) as excinfo:
        qwen_cli.resolve_acp_mode("", list(MEASURED_MODES))
    assert "never falls back" in str(excinfo.value)


def test_mode_policy_accepts_yolo_only_when_agent_offers_it():
    assert qwen_cli.resolve_acp_mode("yolo", list(MEASURED_MODES)) == "yolo"
    with pytest.raises(qwen_cli.AcpPolicyError, match="does not offer mode 'yolo'"):
        qwen_cli.resolve_acp_mode("yolo", ["plan", "auto"])


def test_mode_policy_unoffered_mode_refuses():
    with pytest.raises(qwen_cli.AcpPolicyError, match="does not offer mode 'plan'"):
        qwen_cli.resolve_acp_mode("plan", ["auto"])


def test_mode_policy_valid_mode_is_chosen():
    assert qwen_cli.resolve_acp_mode("plan", list(MEASURED_MODES)) == "plan"
    assert qwen_cli.resolve_acp_mode("auto-edit", list(MEASURED_MODES)) == "auto-edit"


@pytest.mark.skipif(sys.platform == "win32", reason="subprocess Popen is required")
def test_driver_sets_requested_mode_never_the_default(fake_acp_agent, tmp_path):
    """Live: the fake agent records the exact set_mode params the
    driver sent (its sidecar set_mode.json) - the h15 assertion that the
    mode is the policy's, not the measured default 'auto' - and the
    agent's own current_mode_update echo lands in the seam facts."""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="ok", mode="auto-edit")
    result = qwen_cli.run_sync(cfg, "echo acp-probe-marker", str(tmp_path), mode="auto-edit")
    assert result.task_result["status"] == "ok"
    side = _side_dir(tmp_path)
    set_mode = json.loads((side / "set_mode.json").read_text(encoding="utf-8"))
    assert set_mode == {"sessionId": SESSION_ID, "modeId": "auto-edit"}
    assert set_mode["modeId"] != "auto"
    assert result.task_result["seam_facts"]["effective_mode"] == "auto-edit"


@pytest.mark.skipif(sys.platform == "win32", reason="subprocess Popen is required")
def test_run_sync_without_mode_refuses_and_never_calls_set_mode(fake_acp_agent, tmp_path):
    """No policy mode -> a distinct refusal BEFORE set_mode is ever
    sent: the fake agent's set_mode sidecar must not exist."""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="ok", mode=None)
    with pytest.raises(qwen_cli.QwenSeamRefusal) as excinfo:
        qwen_cli.run_sync(cfg, "say hi", str(tmp_path), mode=None)
    assert "no session mode given" in str(excinfo.value)
    side = _side_dir(tmp_path)
    assert not (side / "set_mode.json").exists()
    # the session itself was served (the refusal is the mode policy's,
    # after session/new) - but nothing was prompted
    methods = _c_to_a_methods(tmp_path)
    assert "session/new" in methods
    assert "session/set_mode" not in methods
    assert "session/prompt" not in methods


@pytest.mark.skipif(sys.platform == "win32", reason="subprocess Popen is required")
def test_continuation_ref_accepted_but_not_implemented_first_cut(fake_acp_agent, tmp_path):
    """Frame park v2: continuation_ref is accepted for the t1-ported
    core's API compatibility and cold-starts - the dispatch opens a
    FRESH session (the fake's own id, not the prior one's) and the
    transcript carries no session/load."""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="ok")
    result = qwen_cli.run_sync(
        cfg,
        "say hi",
        str(tmp_path),
        continuation_ref="prior-session-that-will-not-be-loaded",
        mode="plan",
    )
    assert result.task_result["task_id"] == SESSION_ID
    assert result.task_result["task_id"] != "prior-session-that-will-not-be-loaded"
    transcript_text = (
        result.transcript_path and Path(result.transcript_path).read_text(encoding="utf-8")
    ) or ""
    assert "session/load" not in transcript_text


# ---------------------------------------------------------------------------
# the binary probe + boot refusal (#113 contract leg, plan t3's h5):
# the probe never consults the non-interactive PATH
# ---------------------------------------------------------------------------


def _executable(path: Path) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return path


def test_missing_binary_raises_distinct_refusal(monkeypatch, tmp_path):
    """qwen absent from every probed install path -> QwenAgentMissingError
    (a SpawnError: server.py's 503 mapping), with the distinct message
    naming the probe paths - before any Popen, so no invoke is served."""
    monkeypatch.setenv("HOME", str(tmp_path))
    cfg = Config(qwen_bin="qwen", state_dir=str(tmp_path / "state"))
    with pytest.raises(qwen_cli.QwenAgentMissingError) as excinfo:
        qwen_cli.locate_qwen_bin(cfg)
    message = str(excinfo.value)
    assert "qwen-agent-missing" in message
    assert "refusing to serve invokes" in message
    assert str(tmp_path / ".local/lib/qwen-code/bin/qwen") in message
    assert str(tmp_path / ".local/bin/qwen") in message
    assert isinstance(excinfo.value, qwen_cli.SpawnError)


def test_missing_explicit_binary_raises_distinct_refusal(monkeypatch, tmp_path):
    """An explicit operator-configured path that is not a usable
    executable refuses too - with the path named."""
    monkeypatch.setenv("HOME", str(tmp_path))
    cfg = Config(qwen_bin=str(tmp_path / "elsewhere" / "qwen"), state_dir=str(tmp_path / "state"))
    with pytest.raises(qwen_cli.QwenAgentMissingError) as excinfo:
        qwen_cli.locate_qwen_bin(cfg)
    assert str(tmp_path / "elsewhere" / "qwen") in str(excinfo.value)


def test_binary_probe_never_consults_path(monkeypatch, tmp_path):
    """The measured thor/orin fact: qwen is absent from the
    non-interactive ssh PATH there - so a PATH entry must not even be
    LOOKED AT. A qwen executable on PATH does not satisfy the probe."""
    bin_dir = tmp_path / "on-path"
    _executable(bin_dir / "qwen")
    empty_home = tmp_path / "empty-home"
    empty_home.mkdir()
    monkeypatch.setenv("HOME", str(empty_home))
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ.get('PATH', '')}")
    cfg = Config(qwen_bin="qwen", state_dir=str(tmp_path / "state"))
    with pytest.raises(qwen_cli.QwenAgentMissingError):
        qwen_cli.locate_qwen_bin(cfg)


def test_binary_probe_first_install_path(monkeypatch, tmp_path):
    monkeypatch.setenv("HOME", str(tmp_path))
    first = _executable(tmp_path / ".local/lib/qwen-code/bin/qwen")
    cfg = Config(qwen_bin="qwen", state_dir=str(tmp_path / "state"))
    assert qwen_cli.locate_qwen_bin(cfg) == str(first)


def test_binary_probe_second_install_path(monkeypatch, tmp_path):
    monkeypatch.setenv("HOME", str(tmp_path))
    second = _executable(tmp_path / ".local/bin/qwen")
    cfg = Config(qwen_bin="qwen", state_dir=str(tmp_path / "state"))
    assert qwen_cli.locate_qwen_bin(cfg) == str(second)


def test_explicit_configured_bin_is_honored(fake_acp_agent, tmp_path):
    """A configured path containing a separator is honored as-is (the
    operator chose it): the fake agent itself doubles as the binary."""
    cfg = Config(qwen_bin=str(fake_acp_agent), state_dir=str(tmp_path / "state"))
    assert qwen_cli.locate_qwen_bin(cfg) == str(fake_acp_agent)


# ---------------------------------------------------------------------------
# the lenient unknown-ext answer (the measured craft/drainMidTurnQueue)
# ---------------------------------------------------------------------------


@pytest.mark.skipif(sys.platform == "win32", reason="subprocess Popen is required")
def test_unknown_ext_answered_leniently_and_turn_completes(fake_acp_agent, tmp_path):
    """The agent-to-client craft/drainMidTurnQueue request (id 0) is
    answered leniently - the exact response line the fixture records -
    and the turn completes ok. (The fake agent BLOCKS on the answer
    mid-turn, so a wrong or missing answer would hang to the timeout:
    completing is itself the proof.)"""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="ok")
    result = qwen_cli.run_sync(cfg, "echo acp-probe-marker", str(tmp_path), mode="plan")
    assert result.exit_code == 0
    assert result.timed_out is False
    assert result.task_result["status"] == "ok"
    side = _side_dir(tmp_path)
    answer = json.loads((side / "ext_answer.json").read_text(encoding="utf-8"))
    assert answer == _load("craft_drain_mid_turn_queue.json")["response"]
    assert answer["id"] == 0
    assert answer["result"] == {}
    assert result.task_result["acp_transcript"]["ext_methods_answered"] == [
        "craft/drainMidTurnQueue"
    ]


# ---------------------------------------------------------------------------
# the bridge's own timeout: SIGTERM (never SIGKILL) and the cooperative
# session/cancel the driver attempts in response
# ---------------------------------------------------------------------------


@pytest.mark.skipif(sys.platform == "win32", reason="SIGTERM semantics are POSIX-specific")
def test_run_sync_timeout_sigterm_cooperative_cancel_is_cancellation_outcome(
    fake_acp_agent, tmp_path
):
    """End-to-end through the REAL subprocess boundary: the bridge's
    own deadline fires while the turn is in flight, SIGTERM reaches the
    driver child (never SIGKILL), the driver sends the measured
    session/cancel notification, and the cancelled terminal that
    arrives in the grace window classifies as the 13.5/13.6
    cancellation outcome - terminal, never ok, never incomplete."""
    cfg = _cfg(fake_acp_agent, tmp_path, behavior="cancelled", sync_timeout_seconds=2.0)
    result = qwen_cli.run_sync(cfg, "write the numbers 1 through 500", str(tmp_path), mode="plan")
    assert result.timed_out is True
    assert result.task_result is not None
    assert result.task_result["status"] == "error"
    assert result.task_result["termination_reason"] == "cancelled"
    assert "cancellation outcome" in result.task_result["error"]


# ---------------------------------------------------------------------------
# the driver argv (the ONE place it is assembled - the codex sibling's
# _common_argv discipline)
# ---------------------------------------------------------------------------


def test_driver_argv_shape():
    argv = qwen_cli._driver_argv(
        "/opt/qwen/bin/qwen",
        "do the thing",
        "/repo/one",
        model="some-model",
        sandbox="read-only",
        mode="plan",
        state_dir="/state/dir",
        run_id="run-id-123",
        qwen_env={"QWEN_HOME": "/profiles/p1"},
    )
    assert argv == [
        sys.executable,
        "-m",
        "qwen_bridge.qwen_cli",
        "--qwen-bin",
        "/opt/qwen/bin/qwen",
        "--cwd",
        "/repo/one",
        "--instruction",
        "do the thing",
        "--mode",
        "plan",
        "--state-dir",
        "/state/dir",
        "--run-id",
        "run-id-123",
        "--model",
        "some-model",
        "--sandbox",
        "read-only",
        "--qwen-env",
        json.dumps({"QWEN_HOME": "/profiles/p1"}),
    ]


def test_driver_argv_shape_without_optional_args():
    argv = qwen_cli._driver_argv(
        "/opt/qwen/bin/qwen",
        "do the thing",
        "/repo/one",
        model=None,
        sandbox=None,
        mode=None,
        state_dir="/state/dir",
        run_id="run-id-123",
        qwen_env={},
    )
    assert argv == [
        sys.executable,
        "-m",
        "qwen_bridge.qwen_cli",
        "--qwen-bin",
        "/opt/qwen/bin/qwen",
        "--cwd",
        "/repo/one",
        "--instruction",
        "do the thing",
        "--mode",
        "",
        "--state-dir",
        "/state/dir",
        "--run-id",
        "run-id-123",
    ]


# ---------------------------------------------------------------------------
# the seam facts: the plain-data contract t3's capability surface
# consumes by value
# ---------------------------------------------------------------------------


def test_seam_facts_shape_for_t3():
    """t3's capabilities.py (written in parallel) consumes EXACTLY this
    shape as plain data - a JSON-serializable dict, every field measured
    from the agent's own responses, never assumed."""
    result = qwen_cli.parse_session(_turn_stdout("firehose_72_updates.json", mode="plan"))
    seam = result["seam_facts"]
    assert set(seam.keys()) == {
        "protocol_version",
        "agent_info",
        "auth_methods",
        "modes_available",
        "effective_mode",
        "session_id",
        "current_model_id",
    }
    assert seam["protocol_version"] == 1
    assert seam["agent_info"] == {"name": "qwen-code", "title": "Qwen Code", "version": "0.22.0"}
    assert [m["id"] for m in seam["auth_methods"]] == ["openai"]
    assert seam["auth_methods"][0]["_meta"]["args"] == ["--auth-type=openai"]
    assert list(seam["modes_available"]) == ["plan", "default", "auto-edit", "auto"]
    assert seam["effective_mode"] == "plan"
    assert seam["session_id"] == SESSION_ID
    assert seam["current_model_id"] == MEASURED_MODEL
    json.dumps(seam)  # plain data: no dataclass, no non-serializable value


def test_seam_facts_dataclass_round_trip():
    facts = qwen_cli.SeamFacts(
        protocol_version=1,
        agent_info={"name": "qwen-code", "version": "0.22.0"},
        auth_methods=(),
        modes_available=MEASURED_MODES,
        effective_mode="plan",
        session_id=SESSION_ID,
        current_model_id=MEASURED_MODEL,
    )
    dumped = facts.to_dict()
    assert dumped["protocol_version"] == 1
    assert list(dumped["modes_available"]) == list(MEASURED_MODES)


def test_seam_facts_absent_from_an_unserved_turn():
    """dies-before-handshake: no response was ever measured, so no seam
    fact may be invented."""
    assert qwen_cli.parse_session("") is None
    result = qwen_cli.parse_session(_turn_stdout("crash.json", with_handshake=False))
    assert result is not None
    assert result["seam_facts"]["protocol_version"] is None
    assert result["seam_facts"]["session_id"] is None
    assert result["seam_facts"]["modes_available"] == ()


# ---------------------------------------------------------------------------
# the dispatch-surface constants the t1-ported core reads
# ---------------------------------------------------------------------------


def test_sandbox_modes_match_the_dispatch_surface():
    assert qwen_cli.SANDBOX_MODES == frozenset(
        {"read-only", "workspace-write", "danger-full-access"}
    )
    assert qwen_cli.SANDBOX_WORKSPACE_WRITE == "workspace-write"


def test_git_writable_override_is_an_honest_first_cut_noop():
    """The qwen ACP seam has no bwrap-style .git carve-out (see the
    function's docstring): the override is the empty string, never an
    invented flag."""
    assert qwen_cli.git_writable_override(REPO) == ""
