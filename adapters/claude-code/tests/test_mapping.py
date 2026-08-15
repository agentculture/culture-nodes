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
# classify() — capacity_exhausted (task t5, deviation d4): a provider quota
# / rate-limit / session-limit refusal is a distinct §13.5 class from an
# ordinary execution failure, so the capacity circuit breaker
# (internal/worker/breaker.go, task t9) can pause the actor instead of the
# node cascading into a string of failed attempts against a wall that has
# not moved (issue #48).
# ---------------------------------------------------------------------------


def test_rate_limit_error_text_classifies_as_capacity_exhausted():
    c = mapping.classify(
        _error_during_execution_result(
            result="API error: rate_limit_error: Number of request tokens has exceeded "
            "your per-minute rate limit"
        ),
        CTX,
        default_success_outcome="completed",
    )
    assert c.domain is False
    assert c.error_class == mapping.CLASS_CAPACITY_EXHAUSTED


def test_usage_limit_text_classifies_as_capacity_exhausted():
    c = mapping.classify(
        _error_during_execution_result(result="Claude AI usage limit reached for this session"),
        CTX,
        default_success_outcome="completed",
    )
    assert c.error_class == mapping.CLASS_CAPACITY_EXHAUSTED


def test_ordinary_failure_text_is_still_plain_execution_not_capacity_exhausted():
    """The negative half of the acceptance criterion: an unrelated failure
    must NOT be misclassified as capacity_exhausted just because something
    went wrong."""
    c = mapping.classify(
        _error_during_execution_result(result="tool call failed: file not found"),
        CTX,
        default_success_outcome="completed",
    )
    assert c.error_class == mapping.CLASS_EXECUTION
    assert c.error_class != mapping.CLASS_CAPACITY_EXHAUSTED


def test_capacity_exhausted_extracts_a_named_retry_after_delay():
    c = mapping.classify(
        _error_during_execution_result(
            result="rate_limit_error: too many requests, please retry after 120 seconds"
        ),
        CTX,
        default_success_outcome="completed",
    )
    assert c.error_class == mapping.CLASS_CAPACITY_EXHAUSTED
    assert c.retry_after_seconds == 120.0


def test_capacity_exhausted_without_a_named_delay_reports_none_not_zero():
    c = mapping.classify(
        _error_during_execution_result(result="quota exhausted for this billing period"),
        CTX,
        default_success_outcome="completed",
    )
    assert c.error_class == mapping.CLASS_CAPACITY_EXHAUSTED
    assert c.retry_after_seconds is None


def test_sync_response_capacity_exhausted_surfaces_retry_after_on_the_response_not_the_body():
    """internal/actors/client.go reads Retry-After from the HTTP header
    only — server.py is the one that turns this into the actual header
    (test_server_unit.py covers that HTTP-level wiring); this test pins the
    contract at the mapping layer: the delay rides the SyncResponse object,
    and the JSON body's key set is untouched (matching the exact-set pin in
    test_sync_response_failure_carries_usage_from_terminal_result)."""
    r = mapping.sync_response(
        _error_during_execution_result(
            result="rate_limit_error: retry after 30 seconds", session_id="sess-cap"
        ),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.status_code == 500
    assert r.body["class"] == mapping.CLASS_CAPACITY_EXHAUSTED
    assert r.retry_after_seconds == 30.0
    assert "retry_after_seconds" not in r.body


def test_terminal_event_capacity_exhausted_is_failed_kind_with_the_class():
    ev = mapping.terminal_event(
        _error_during_execution_result(result="session limit reached, try again later"),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert ev.payload["class"] == mapping.CLASS_CAPACITY_EXHAUSTED
    # No new wire key: FailedPayload (protocol.go) has no retry_after field.
    assert "retry_after_seconds" not in ev.payload


# ---------------------------------------------------------------------------
# usage / output mapping
# ---------------------------------------------------------------------------


def test_usage_maps_cache_model_session_and_passes_through_real_cost():
    usage = mapping.usage_from_result(
        _success_result(
            model="claude-opus-4-1",
            usage={
                "input_tokens": 100,
                "cache_read_input_tokens": 70,
                "cache_creation_input_tokens": 20,
                "output_tokens": 40,
            },
        )
    )
    assert usage == {
        "input_tokens": 100,
        "output_tokens": 40,
        "cost": 0.0123,
        "currency": "USD",
        "cached_input_tokens": 70,
        "model": "claude-opus-4-1",
        "thread_id": "sess-abc",
    }
    assert usage["cached_input_tokens"] != 90


def test_usage_defaults_to_zero_and_null_cost_when_absent():
    usage = mapping.usage_from_result({"type": "result"})
    # Counts default to zero and cost stays null, but `model` is PRESENT and
    # explicit rather than absent. A result carrying no model at all is the
    # #77 case: an omitted key and a key nobody wrote are the same null
    # downstream, so the bridge says which one this is.
    assert usage == {
        "input_tokens": 0,
        "output_tokens": 0,
        "cost": None,
        "currency": None,
        "model": mapping.MODEL_NOT_REPORTED,
    }


def test_usage_maps_single_model_from_model_usage_when_no_direct_model_field():
    usage = mapping.usage_from_result(
        _success_result(modelUsage={"claude-sonnet-4-5": {"inputTokens": 100}})
    )
    assert usage["model"] == "claude-sonnet-4-5"


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
    # t5: claude's own captured session_id IS the continuation_ref the
    # bridge offers back (ADR 0010 §2/§13.2) — a hardcoded None here would
    # be the exact bug t5 fixed (session resume was captured but never
    # actually returned on the sync body).
    assert r.body["continuation_ref"] == "sess-abc"
    assert r.body["artifact_refs"] == []


def test_sync_response_continuation_ref_is_none_when_claude_reported_no_session_id():
    r = mapping.sync_response(
        _success_result(session_id=None),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.body["continuation_ref"] is None


def test_sync_response_maps_claude_stop_reason_beside_usage():
    r = mapping.sync_response(
        _success_result(stop_reason="end_turn"),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.body["termination_reason"] == "end_turn"


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


def test_terminal_event_completed_payload_carries_continuation_ref():
    """Acceptance: 'the async terminal payload carries continuation_ref' —
    ADR 0010 §2 extended CompletedPayload with exactly this field because
    the asynchronous path is the one long sessions actually take; the seed
    this task inherited only ever wired this onto the SYNC body."""
    ev = mapping.terminal_event(
        _success_result(session_id="sess-async-1"),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "completed"
    assert ev.payload["continuation_ref"] == "sess-async-1"


def test_terminal_event_failed_payload_has_no_continuation_ref_key():
    """ADR 0010 §2: FailedPayload deliberately gains no continuation_ref —
    a bridge reporting a failed turn is the least reliable position from
    which to claim a resumable conversation exists."""
    ev = mapping.terminal_event(
        _error_during_execution_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert "continuation_ref" not in ev.payload


def test_terminal_completion_maps_claude_stop_reason_beside_usage():
    ev = mapping.terminal_event(
        _success_result(stop_reason="end_turn"),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "completed"
    assert ev.payload["termination_reason"] == "end_turn"


def test_terminal_failure_maps_claude_stop_reason_beside_usage():
    ev = mapping.terminal_event(
        _error_during_execution_result(stop_reason="tool_error"),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert ev.payload["termination_reason"] == "tool_error"


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


# ---------------------------------------------------------------------------
# usage on failure (issue #32, task t4): a failed session still burned real
# tokens. When a parseable terminal result exists, its API-reported usage
# rides the failure body/payload; the result-less crash and timeout branches
# stay usage-less — no key at all, never fabricated zeros.
# ---------------------------------------------------------------------------


def test_sync_response_failure_carries_usage_from_terminal_result():
    r = mapping.sync_response(
        _error_during_execution_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.status_code == 500
    # The full 500-body key set is a wire contract (issue #32, task t5): the
    # control plane's actors client parses exactly this shape out of a non-2xx
    # response (usageFromErrorBody in internal/actors/client.go), so a renamed
    # or nested key would silently cost failed attempts their accounting.
    assert set(r.body) == {"error", "class", "workspace_measured", "usage"}
    assert r.body["usage"] == {
        "input_tokens": 5,
        "output_tokens": 0,
        "cost": 0.0001,
        "currency": "USD",
        "thread_id": "sess-err",
        "model": "unknown:claude-code-session-did-not-report",
    }


def test_sync_response_undeclared_incomplete_failure_carries_usage():
    r = mapping.sync_response(
        _incomplete_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert r.status_code == 500
    assert r.body["usage"] == {
        "input_tokens": 900,
        "output_tokens": 400,
        "cost": 0.05,
        "currency": "USD",
        "thread_id": "sess-partial",
        "model": "unknown:claude-code-session-did-not-report",
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


def test_terminal_event_failed_carries_usage_from_terminal_result():
    ev = mapping.terminal_event(
        _error_during_execution_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert ev.payload["usage"] == {
        "input_tokens": 5,
        "output_tokens": 0,
        "cost": 0.0001,
        "currency": "USD",
        "thread_id": "sess-err",
        "model": "unknown:claude-code-session-did-not-report",
    }


def test_terminal_event_undeclared_incomplete_failure_carries_usage():
    ev = mapping.terminal_event(
        _incomplete_result(),
        CTX,
        default_success_outcome="completed",
        actor_id="a",
        created_at="now",
    )
    assert ev.kind == "failed"
    assert ev.payload["usage"] == {
        "input_tokens": 900,
        "output_tokens": 400,
        "cost": 0.05,
        "currency": "USD",
        "thread_id": "sess-partial",
        "model": "unknown:claude-code-session-did-not-report",
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


def test_a_multi_model_session_reports_an_explicit_unknown_not_an_omission():
    """The live case that reopened #77 after t14/t15 shipped.

    `claude -p` reports `modelUsage` as a map, and a session that ran subagents
    has more than one entry — a main model plus whatever served the subagents.
    Naming any single one would be a guess, so the bridge used to omit `model`
    entirely. Downstream that is a null, and a null was indistinguishable from
    a field nobody wrote, which is the whole of #77. Five consecutive live
    attempts across four claude bridges all landed usage_model NULL this way,
    after the batch believed the issue closed.
    """
    usage = mapping.usage_from_result(
        {
            "type": "result",
            "usage": {"input_tokens": 10, "output_tokens": 20},
            "modelUsage": {"claude-opus-4": {}, "claude-haiku-4": {}},
        }
    )
    assert usage["model"] == mapping.MODEL_NOT_REPORTED
    assert usage["input_tokens"] == 10

    # One entry is unambiguous, so the real name still wins.
    named = mapping.usage_from_result({"type": "result", "modelUsage": {"claude-opus-4": {}}})
    assert named["model"] == "claude-opus-4"


def test_the_two_unknown_sentinels_say_different_things():
    """`did-not-report` is a gap in ONE attempt; the colleague/notify
    `cannot-report` sentinels are a permanent property of those backends.
    Collapsing them would hide a regression here behind a limitation there —
    a claude bridge that silently stopped reporting models would read exactly
    like a backend that never could."""
    assert mapping.MODEL_NOT_REPORTED.startswith("unknown:")
    assert "cannot-report" not in mapping.MODEL_NOT_REPORTED
    assert "did-not-report" in mapping.MODEL_NOT_REPORTED
