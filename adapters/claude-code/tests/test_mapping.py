"""claude `type: "result"` -> actor-protocol mapping table.

The one property every test in this file ultimately serves — the task's own
acceptance criterion #2, "incomplete-never-success": an incomplete OR
crashed claude session must map to failure, never success, unless the
invocation itself declared an `incomplete_outcome` domain outcome (and even
then it is never silently folded into the DEFAULT success outcome).
"""

from __future__ import annotations

from claude_code_bridge import mapping

CTX = mapping.InvocationContext(run_id="run_1", node_run_id="nr_1", attempt_id="att_1")


def _success_result(**overrides):
    base = {
        "type": "result",
        "subtype": "success",
        "is_error": False,
        "num_turns": 3,
        "session_id": "sess-abc",
        "stop_reason": None,
        "total_cost_usd": 0.0123,
        "usage": {"input_tokens": 100, "output_tokens": 40},
        "result": "did the thing",
    }
    base.update(overrides)
    return base


def _error_during_execution_result(**overrides):
    base = {
        "type": "result",
        "subtype": "error_during_execution",
        "is_error": True,
        "num_turns": 1,
        "session_id": "sess-err",
        "stop_reason": None,
        "total_cost_usd": 0.0001,
        "usage": {"input_tokens": 5, "output_tokens": 0},
        "result": None,
    }
    base.update(overrides)
    return base


def _incomplete_result(**overrides):
    base = {
        "type": "result",
        "subtype": "error_max_turns",
        "is_error": True,
        "num_turns": 20,
        "session_id": "sess-partial",
        "stop_reason": None,
        "total_cost_usd": 0.05,
        "usage": {"input_tokens": 900, "output_tokens": 400},
        "result": "got halfway",
    }
    base.update(overrides)
    return base


# ---------------------------------------------------------------------------
# classify() — "incomplete is never success"
# ---------------------------------------------------------------------------


def test_success_subtype_classifies_as_domain_success_with_default_outcome():
    c = mapping.classify(_success_result(), CTX, default_success_outcome="completed")
    assert c.domain is True
    assert c.outcome == "completed"


def test_success_subtype_honors_declared_success_outcome():
    ctx = mapping.InvocationContext(success_outcome="approved")
    c = mapping.classify(_success_result(), ctx, default_success_outcome="completed")
    assert c.domain is True
    assert c.outcome == "approved"


def test_error_during_execution_is_never_domain():
    c = mapping.classify(_error_during_execution_result(), CTX, default_success_outcome="completed")
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION


def test_incomplete_never_success_without_declared_outcome():
    """The task's own named acceptance test: subtype=error_max_turns must
    never become a domain success unless the invocation declared an
    incomplete_outcome."""
    c = mapping.classify(_incomplete_result(), CTX, default_success_outcome="completed")
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION
    assert "incomplete" in c.message.lower() or "error_max_turns" in c.message
    assert c.outcome is None
    assert c.outcome != "completed"


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


def test_unrecognised_subtype_is_defensively_an_execution_failure():
    c = mapping.classify(
        {"type": "result", "subtype": "???", "is_error": False},
        CTX,
        default_success_outcome="completed",
    )
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION


def test_is_error_true_with_subtype_success_is_still_not_domain():
    """A shape the CLI should never produce, but this module never trusts
    is_error and subtype to agree without checking both."""
    c = mapping.classify(
        {"type": "result", "subtype": "success", "is_error": True},
        CTX,
        default_success_outcome="completed",
    )
    assert c.domain is False


def test_crashed_session_never_success():
    """The task's other named acceptance property: a crashed/killed claude
    session that produced no parseable result at all must never be read as
    success."""
    c = mapping.classify(None, CTX, default_success_outcome="completed")
    assert c.domain is False
    assert c.error_class == mapping.CLASS_EXECUTION
    assert "no parseable result" in c.message


# ---------------------------------------------------------------------------
# usage / output mapping
# ---------------------------------------------------------------------------


def test_usage_maps_tokens_and_passes_through_real_cost():
    usage = mapping.usage_from_result(_success_result())
    assert usage == {"input_tokens": 100, "output_tokens": 40, "cost": 0.0123, "currency": "USD"}


def test_usage_defaults_to_zero_and_null_cost_when_absent():
    usage = mapping.usage_from_result({"type": "result"})
    assert usage == {"input_tokens": 0, "output_tokens": 0, "cost": None, "currency": None}


def test_output_carries_result_text_as_summary():
    output = mapping.output_from_result(_success_result())
    assert output == {"summary": "did the thing", "changed_files": [], "artifacts_path": None}


# ---------------------------------------------------------------------------
# ledger_delta: propose-only
# ---------------------------------------------------------------------------


def test_claim_record_is_proposed_authority_agent_origin():
    record = mapping.claim_record(
        _success_result(),
        CTX,
        actor_id="claude-code-bridge",
        created_at="2026-01-01T00:00:00+00:00",
    )
    assert record["record_type"] == "claim"
    assert record["authority"] == "proposed"
    assert record["origin"] == {"kind": "agent", "actor_id": "claude-code-bridge"}
    assert record["run_id"] == "run_1"
    assert record["node_run_id"] == "nr_1"
    assert record["attempt_id"] == "att_1"
    assert record["data"]["statement"] == "did the thing"
    assert record["data"]["claude_session_id"] == "sess-abc"


def test_claim_record_never_uses_confirmed_or_observed_or_derived():
    record = mapping.claim_record(
        _success_result(), CTX, actor_id="x", created_at="2026-01-01T00:00:00+00:00"
    )
    assert record["authority"] not in ("confirmed", "observed", "derived", "rejected", "superseded")


def test_sync_response_ledger_delta_is_propose_only():
    response = mapping.sync_response(
        _success_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="claude-code-bridge",
        created_at="2026-01-01T00:00:00+00:00",
    )
    records = response.body["ledger_delta"]["records"]
    assert len(records) == 1
    assert records[0]["authority"] == "proposed"


# ---------------------------------------------------------------------------
# sync_response(): the full subtype -> HTTP status/body table
# ---------------------------------------------------------------------------


def test_sync_response_success_is_200_with_outcome_and_output():
    r = mapping.sync_response(
        _success_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert r.status_code == 200
    assert r.body["outcome"] == "completed"
    assert r.body["output"]["summary"] == "did the thing"
    assert r.body["continuation_ref"] is None
    assert r.body["artifact_refs"] == []


def test_sync_response_error_is_execution_failure_not_200():
    r = mapping.sync_response(
        _error_during_execution_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
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


def test_sync_response_timeout_is_408_regardless_of_result():
    r = mapping.sync_response(
        _success_result(),
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


def test_terminal_event_success_is_completed_kind():
    ev = mapping.terminal_event(
        _success_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    assert ev.kind == "completed"
    assert ev.payload["outcome"] == "completed"
    assert ev.payload["ledger_delta"]["records"][0]["authority"] == "proposed"


def test_terminal_event_error_is_failed_kind_with_execution_class():
    ev = mapping.terminal_event(
        _error_during_execution_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert ev.payload["class"] == mapping.CLASS_EXECUTION


def test_terminal_event_incomplete_without_declaration_is_failed_never_completed():
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


def test_terminal_event_crashed_session_is_failed_never_completed():
    """The __pid_gone__ path (async_runner.py) hands classify() a None
    result the same way a crashed sync dispatch does — must still never be
    "completed"."""
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
    "status_porcelain": " M README.md\n",
    "changed_files": ["README.md"],
    "diffstat": " README.md | 1 +\n",
}


def test_sync_response_passes_through_a_supplied_workspace_measurement_untouched():
    r = mapping.sync_response(
        _success_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        workspace_measured=_REAL_MEASUREMENT,
    )
    assert r.body["workspace_measured"] == _REAL_MEASUREMENT
    # Structurally distinct from the model-claimed output block: claude
    # never reports changed_files of its own, but the bridge measured one.
    assert r.body["output"]["changed_files"] == []
    assert r.body["workspace_measured"]["changed_files"] == ["README.md"]


def test_sync_response_defaults_workspace_measured_to_an_honest_unmeasured_shape():
    r = mapping.sync_response(
        _success_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    wm = r.body["workspace_measured"]
    assert wm["measured"] is False
    assert wm["reason"]
    assert wm["changed_files"] == []


def test_sync_response_carries_workspace_measured_even_on_failure():
    """A crashed/errored session may still have left workspace changes —
    the measurement is never conditioned on the session's own outcome."""
    r = mapping.sync_response(
        _error_during_execution_result(),
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
        _success_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
        workspace_measured=_REAL_MEASUREMENT,
    )
    assert ev.payload["workspace_measured"] == _REAL_MEASUREMENT


def test_terminal_event_defaults_workspace_measured_to_an_honest_unmeasured_shape():
    ev = mapping.terminal_event(
        _success_result(), CTX, default_success_outcome="completed", actor_id="a", created_at="now"
    )
    wm = ev.payload["workspace_measured"]
    assert wm["measured"] is False
    assert wm["reason"]


def test_terminal_event_carries_workspace_measured_on_failure_and_timeout():
    failed = mapping.terminal_event(
        _error_during_execution_result(),
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
