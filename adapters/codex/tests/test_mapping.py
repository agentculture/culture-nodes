"""TaskResult -> actor-protocol mapping table (codex session -> PRD §13).
Mirrors `adapters/colleague`'s own `test_mapping.py` near-verbatim: the wire
protocol and the ok/error/incomplete vocabulary are identical, only the
underlying agent backend differs (see `codex_cli.py`).

The one property every test in this file ultimately serves: **incomplete is
never success.** A codex session with `status == "incomplete"` (no terminal
turn event — killed, crashed, or timed out) never becomes an HTTP 200 with
outcome "completed", or a `completed` callback event, unless the invocation
itself declared an `incomplete_outcome` domain outcome.
"""

from __future__ import annotations

from codex_bridge import mapping

CTX = mapping.InvocationContext(run_id="run_1", node_run_id="nr_1", attempt_id="att_1")


def _ok_result(**overrides):
    base = {
        "task_id": "019fe54f-8e7b-7940-943c-1728fd3a7c6b",
        "status": "ok",
        "summary": "did the thing",
        "changed_files": ["a.py"],
        "usage": {"input_tokens": 10, "output_tokens": 5},
        "error": None,
    }
    base.update(overrides)
    return base


def _error_result(**overrides):
    base = {
        "task_id": "019fe54f-cb2c-7780-9316-46ecb958a6ed",
        "status": "error",
        "summary": "",
        "changed_files": [],
        "usage": {"input_tokens": 1, "output_tokens": 0},
        "error": "engine raised: boom",
    }
    base.update(overrides)
    return base


def _incomplete_result(**overrides):
    """Shaped exactly like what `codex_cli.parse_session` returns for a
    killed/crashed session: no error, just no terminal event."""
    base = {
        "task_id": "019fe553-362a-7191-aa66-6c03191830b1",
        "status": "incomplete",
        "summary": "I'll run all six sequentially and wait for each to finish.",
        "changed_files": [],
        "usage": {},
        "error": None,
    }
    base.update(overrides)
    return base


# ---------------------------------------------------------------------------
# classify()
# ---------------------------------------------------------------------------


def test_ok_status_classifies_as_domain_success_with_default_outcome():
    c = mapping.classify(_ok_result(), CTX, default_success_outcome="completed")
    assert c.domain is True
    assert c.outcome == "completed"


def test_ok_status_honors_declared_success_outcome():
    ctx = mapping.InvocationContext(success_outcome="approved")
    c = mapping.classify(_ok_result(), ctx, default_success_outcome="completed")
    assert c.domain is True
    assert c.outcome == "approved"


def test_error_status_is_never_domain():
    c = mapping.classify(_error_result(), CTX, default_success_outcome="completed")
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION
    assert "boom" in c.message


def test_error_status_without_error_message_still_reports_execution_failure():
    c = mapping.classify(_error_result(error=None), CTX, default_success_outcome="completed")
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION


def test_incomplete_without_declared_outcome_is_execution_failure_never_success():
    c = mapping.classify(_incomplete_result(), CTX, default_success_outcome="completed")
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION
    assert "incomplete" in c.message
    assert c.outcome is None


def test_incomplete_with_declared_outcome_is_domain_but_never_completed():
    ctx = mapping.InvocationContext(incomplete_outcome="incomplete")
    c = mapping.classify(_incomplete_result(), ctx, default_success_outcome="completed")
    assert c.domain is True
    assert c.outcome == "incomplete"
    assert c.outcome != "completed"


def test_incomplete_declared_outcome_can_be_a_custom_name():
    ctx = mapping.InvocationContext(incomplete_outcome="needs_more_budget")
    c = mapping.classify(_incomplete_result(), ctx, default_success_outcome="completed")
    assert c.domain is True
    assert c.outcome == "needs_more_budget"


def test_unrecognised_status_is_defensively_an_execution_failure():
    c = mapping.classify({"status": "???"}, CTX, default_success_outcome="completed")
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION


def test_missing_task_result_entirely_is_an_execution_failure():
    c = mapping.classify(None, CTX, default_success_outcome="completed")
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION
    assert "no parseable result" in c.message


# ---------------------------------------------------------------------------
# usage / output mapping
# ---------------------------------------------------------------------------


def test_usage_maps_input_and_output_tokens_cost_and_currency_are_null():
    usage = mapping.usage_from_task_result(_ok_result())
    assert usage == {"input_tokens": 10, "output_tokens": 5, "cost": None, "currency": None}


def test_usage_defaults_to_zero_when_absent():
    usage = mapping.usage_from_task_result({"status": "ok"})
    assert usage == {"input_tokens": 0, "output_tokens": 0, "cost": None, "currency": None}


def test_output_carries_summary_and_changed_files():
    output = mapping.output_from_task_result(_ok_result())
    assert output == {
        "summary": "did the thing",
        "changed_files": ["a.py"],
        "artifacts_path": None,
    }


# ---------------------------------------------------------------------------
# ledger_delta: propose-only
# ---------------------------------------------------------------------------


def test_claim_record_is_proposed_authority_agent_origin():
    record = mapping.claim_record(
        _ok_result(), CTX, actor_id="codex-bridge", created_at="2026-01-01T00:00:00+00:00"
    )
    assert record["record_type"] == "claim"
    assert record["authority"] == "proposed"
    assert record["origin"] == {"kind": "agent", "actor_id": "codex-bridge"}
    assert record["run_id"] == "run_1"
    assert record["node_run_id"] == "nr_1"
    assert record["attempt_id"] == "att_1"
    assert record["data"]["statement"] == "did the thing"
    assert record["data"]["codex_task_id"] == "019fe54f-8e7b-7940-943c-1728fd3a7c6b"


def test_claim_record_never_uses_confirmed_or_observed_or_derived():
    record = mapping.claim_record(
        _ok_result(), CTX, actor_id="x", created_at="2026-01-01T00:00:00+00:00"
    )
    assert record["authority"] not in ("confirmed", "observed", "derived", "rejected", "superseded")


def test_sync_response_ledger_delta_is_propose_only():
    response = mapping.sync_response(
        _ok_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="codex-bridge",
        created_at="2026-01-01T00:00:00+00:00",
    )
    records = response.body["ledger_delta"]["records"]
    assert len(records) == 1
    assert records[0]["authority"] == "proposed"


# ---------------------------------------------------------------------------
# sync_response(): the full status -> HTTP status/body table
# ---------------------------------------------------------------------------


def test_sync_response_ok_is_200_with_outcome_and_output():
    r = mapping.sync_response(
        _ok_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert r.status_code == 200
    assert r.body["outcome"] == "completed"
    assert r.body["output"]["summary"] == "did the thing"
    assert r.body["continuation_ref"] is None
    assert r.body["artifact_refs"] == []


def test_sync_response_error_is_execution_failure_not_200():
    r = mapping.sync_response(
        _error_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert r.status_code != 200
    assert r.body["class"] == mapping.CLASS_EXECUTION
    assert "outcome" not in r.body


def test_sync_response_incomplete_without_declaration_is_never_200_completed():
    r = mapping.sync_response(
        _incomplete_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.status_code != 200
    assert r.body.get("outcome") != "completed"


def test_sync_response_crashed_session_is_never_200_completed():
    """The literal acceptance-criterion test at the mapping layer: a
    crashed/killed codex session (parse_session's 'incomplete' with no
    declared incomplete_outcome) never becomes a 200 'completed' — no
    adapter-specific exemption."""
    crashed = _incomplete_result(summary="", changed_files=[], usage={})
    r = mapping.sync_response(
        crashed, CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert r.status_code != 200
    assert r.body.get("outcome") != "completed"
    assert r.body["class"] == mapping.CLASS_EXECUTION


def test_sync_response_incomplete_with_declaration_is_200_with_declared_outcome():
    ctx = mapping.InvocationContext(incomplete_outcome="incomplete")
    r = mapping.sync_response(
        _incomplete_result(),
        ctx,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.status_code == 200
    assert r.body["outcome"] == "incomplete"


def test_sync_response_timeout_is_408_regardless_of_task_result():
    r = mapping.sync_response(
        _ok_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        timed_out=True,
    )
    assert r.status_code == 408
    assert r.body["class"] == mapping.CLASS_TIMEOUT


def test_sync_response_missing_result_is_execution_failure():
    r = mapping.sync_response(
        None, CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert r.status_code == 500
    assert r.body["class"] == mapping.CLASS_EXECUTION


# ---------------------------------------------------------------------------
# terminal_event(): the async callback equivalent of sync_response()
# ---------------------------------------------------------------------------


def test_terminal_event_ok_is_completed_kind():
    ev = mapping.terminal_event(
        _ok_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert ev.kind == "completed"
    assert ev.payload["outcome"] == "completed"
    assert ev.payload["ledger_delta"]["records"][0]["authority"] == "proposed"


def test_terminal_event_error_is_failed_kind_with_execution_class():
    ev = mapping.terminal_event(
        _error_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert ev.kind == "failed"
    assert ev.payload["class"] == mapping.CLASS_EXECUTION


def test_terminal_event_crashed_session_is_failed_never_completed():
    ev = mapping.terminal_event(
        _incomplete_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert ev.kind != "completed"


def test_terminal_event_incomplete_with_declaration_is_completed_with_declared_outcome():
    ctx = mapping.InvocationContext(incomplete_outcome="incomplete")
    ev = mapping.terminal_event(
        _incomplete_result(),
        ctx,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "completed"
    assert ev.payload["outcome"] == "incomplete"


def test_terminal_event_timeout_is_failed_with_timeout_class():
    ev = mapping.terminal_event(
        None,
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        timed_out=True,
        detail="gave up waiting",
    )
    assert ev.kind == "failed"
    assert ev.payload["class"] == mapping.CLASS_TIMEOUT
    assert ev.payload["detail"] == "gave up waiting"


def test_terminal_event_missing_result_without_timeout_is_execution_failure():
    ev = mapping.terminal_event(
        None, CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert ev.kind == "failed"
    assert ev.payload["class"] == mapping.CLASS_EXECUTION


# ---------------------------------------------------------------------------
# workspace_measured (task t10): passed through untouched, structurally
# distinct from `output`'s model-claimed changed_files, and honestly
# defaulted when a caller supplies none.
# ---------------------------------------------------------------------------

_REAL_MEASUREMENT = {
    "measured": True,
    "repo": "/tmp/some-repo",
    "reason": None,
    "branch": "main",
    "head_before": "aaaa",
    "head_after": "bbbb",
    "status_porcelain": " M b.py\n",
    "changed_files": ["b.py"],
    "diffstat": " b.py | 1 +\n",
}


def test_sync_response_passes_through_a_supplied_workspace_measurement_untouched():
    r = mapping.sync_response(
        _ok_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        workspace_measured=_REAL_MEASUREMENT,
    )
    assert r.body["workspace_measured"] == _REAL_MEASUREMENT
    # Structurally distinct from the model-claimed output block: codex's
    # own self-reported changed_files ("a.py") differs from what the bridge
    # actually measured against git ("b.py") — never conflated.
    assert r.body["output"]["changed_files"] == ["a.py"]
    assert r.body["workspace_measured"]["changed_files"] == ["b.py"]


def test_sync_response_defaults_workspace_measured_to_an_honest_unmeasured_shape():
    r = mapping.sync_response(
        _ok_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    wm = r.body["workspace_measured"]
    assert wm["measured"] is False
    assert wm["reason"]
    assert wm["changed_files"] == []


def test_sync_response_carries_workspace_measured_even_on_failure():
    r = mapping.sync_response(
        _error_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        workspace_measured=_REAL_MEASUREMENT,
    )
    assert r.status_code != 200
    assert r.body["workspace_measured"] == _REAL_MEASUREMENT


def test_sync_response_carries_workspace_measured_on_timeout():
    r = mapping.sync_response(
        None,
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        timed_out=True,
        workspace_measured=_REAL_MEASUREMENT,
    )
    assert r.status_code == 408
    assert r.body["workspace_measured"] == _REAL_MEASUREMENT


def test_terminal_event_passes_through_a_supplied_workspace_measurement_untouched():
    ev = mapping.terminal_event(
        _ok_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        workspace_measured=_REAL_MEASUREMENT,
    )
    assert ev.payload["workspace_measured"] == _REAL_MEASUREMENT


def test_terminal_event_defaults_workspace_measured_to_an_honest_unmeasured_shape():
    ev = mapping.terminal_event(
        _ok_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    wm = ev.payload["workspace_measured"]
    assert wm["measured"] is False
    assert wm["reason"]


def test_terminal_event_carries_workspace_measured_on_failure_and_timeout():
    failed = mapping.terminal_event(
        _error_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        workspace_measured=_REAL_MEASUREMENT,
    )
    assert failed.payload["workspace_measured"] == _REAL_MEASUREMENT

    timed_out = mapping.terminal_event(
        None,
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        timed_out=True,
        workspace_measured=_REAL_MEASUREMENT,
    )
    assert timed_out.payload["workspace_measured"] == _REAL_MEASUREMENT


# ---------------------------------------------------------------------------
# usage on failure (issue #32, task t4): a failed session still burned real
# tokens. When a parseable terminal result exists, its API-reported usage
# rides the failure body/payload; the result-less crash and timeout branches
# stay usage-less — no key at all, never fabricated zeros.
# ---------------------------------------------------------------------------


def test_sync_response_failure_carries_usage_from_task_result():
    r = mapping.sync_response(
        _error_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.status_code == 500
    assert r.body["usage"] == {
        "input_tokens": 1,
        "output_tokens": 0,
        "cost": None,
        "currency": None,
    }


def test_sync_response_undeclared_incomplete_failure_carries_usage():
    r = mapping.sync_response(
        _incomplete_result(usage={"input_tokens": 900, "output_tokens": 400}),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.status_code == 500
    assert r.body["usage"] == {
        "input_tokens": 900,
        "output_tokens": 400,
        "cost": None,
        "currency": None,
    }


def test_sync_response_crash_emits_no_usage_key():
    r = mapping.sync_response(
        None, CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert r.status_code == 500
    assert "usage" not in r.body


def test_sync_response_timeout_emits_no_usage_key():
    r = mapping.sync_response(
        None,
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        timed_out=True,
    )
    assert r.status_code == 408
    assert "usage" not in r.body


def test_terminal_event_failed_carries_usage_from_task_result():
    ev = mapping.terminal_event(
        _error_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert ev.payload["usage"] == {
        "input_tokens": 1,
        "output_tokens": 0,
        "cost": None,
        "currency": None,
    }


def test_terminal_event_undeclared_incomplete_failure_carries_usage():
    ev = mapping.terminal_event(
        _incomplete_result(usage={"input_tokens": 900, "output_tokens": 400}),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert ev.payload["usage"] == {
        "input_tokens": 900,
        "output_tokens": 400,
        "cost": None,
        "currency": None,
    }


def test_terminal_event_crash_emits_no_usage_key():
    ev = mapping.terminal_event(
        None, CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert ev.kind == "failed"
    assert "usage" not in ev.payload


def test_terminal_event_timeout_emits_no_usage_key():
    ev = mapping.terminal_event(
        None,
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        timed_out=True,
    )
    assert ev.kind == "failed"
    assert "usage" not in ev.payload
