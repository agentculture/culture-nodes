"""Mapping-layer tests: input validation, the fail-open/require_delivery
outcome ladder (issue #68's core contract), and the "only status codes"
ledger record -- proven here by asserting no field of the built record or
result carries a webhook URL or the message body that was sent."""

from __future__ import annotations

import json

from notify_bridge import mapping
from notify_bridge.webhook import PostResult


def test_usage_names_an_explicit_non_null_unknown_model():
    usage = mapping.usage_for_backend()
    assert usage["model"] == "unknown:notify-backend-cannot-report"
    assert usage["model"] is not None


CTX = mapping.InvocationContext(run_id="run_1", node_run_id="nr_1", attempt_id="att_1")
SECRET_URL = "https://discord.com/api/webhooks/999/super-secret-token-value"


# -- parse_message ------------------------------------------------------------


def test_parse_message_requires_at_least_one_text_field():
    parsed, error = mapping.parse_message({})
    assert parsed is None
    assert "content" in error and "title" in error and "description" in error


def test_parse_message_accepts_content_only():
    parsed, error = mapping.parse_message({"content": "hello"})
    assert error is None
    assert parsed.message.content == "hello"
    assert parsed.require_delivery is False


def test_parse_message_reads_require_delivery():
    parsed, error = mapping.parse_message({"content": "hi", "require_delivery": True})
    assert error is None
    assert parsed.require_delivery is True


def test_parse_message_rejects_non_bool_require_delivery():
    parsed, error = mapping.parse_message({"content": "hi", "require_delivery": "yes"})
    assert parsed is None
    assert "require_delivery" in error


def test_parse_message_rejects_non_string_content():
    parsed, error = mapping.parse_message({"content": 5})
    assert parsed is None
    assert "content" in error


def test_parse_message_validates_fields_array():
    parsed, error = mapping.parse_message(
        {"title": "t", "fields": [{"name": "Run", "value": "run_1"}]}
    )
    assert error is None
    assert parsed.message.fields[0].name == "Run"
    assert parsed.message.fields[0].inline is False


def test_parse_message_rejects_field_missing_value():
    parsed, error = mapping.parse_message({"title": "t", "fields": [{"name": "Run"}]})
    assert parsed is None
    assert "fields[0].value" in error


def test_parse_message_rejects_non_dict_input():
    parsed, error = mapping.parse_message("not a dict")  # type: ignore[arg-type]
    assert parsed is None
    assert "object" in error


# -- result_for: fail-open by default -----------------------------------------


def test_default_outcome_is_sent_on_success():
    body = mapping.result_for(
        PostResult.POSTED, 204, require_delivery=False, ctx=CTX, actor_id="a", created_at="t"
    )
    assert body["outcome"] == mapping.OUTCOME_SENT
    assert body["output"] == {"delivered": True, "status_code": 204}
    assert body["usage"]["model"] == "unknown:notify-backend-cannot-report"


def test_default_outcome_is_still_sent_on_failure():
    """Issue #68's fail-open acceptance bullet: a webhook outage with the
    default settings leaves the run green -- the outcome stays `sent`
    (not an error, not a different edge) even though delivery failed."""
    body = mapping.result_for(
        PostResult.FAILED, 503, require_delivery=False, ctx=CTX, actor_id="a", created_at="t"
    )
    assert body["outcome"] == mapping.OUTCOME_SENT
    assert body["output"] == {"delivered": False, "status_code": 503}


def test_default_outcome_is_sent_when_webhook_disabled():
    body = mapping.result_for(
        PostResult.DISABLED, None, require_delivery=False, ctx=CTX, actor_id="a", created_at="t"
    )
    assert body["outcome"] == mapping.OUTCOME_SENT
    assert body["output"]["delivered"] is False


# -- result_for: require_delivery routes a domain outcome ---------------------


def test_require_delivery_true_stays_sent_on_2xx():
    body = mapping.result_for(
        PostResult.POSTED, 204, require_delivery=True, ctx=CTX, actor_id="a", created_at="t"
    )
    assert body["outcome"] == mapping.OUTCOME_SENT


def test_require_delivery_true_routes_delivery_failed_on_non_2xx():
    body = mapping.result_for(
        PostResult.FAILED, 500, require_delivery=True, ctx=CTX, actor_id="a", created_at="t"
    )
    assert body["outcome"] == mapping.OUTCOME_DELIVERY_FAILED


def test_require_delivery_true_routes_delivery_failed_when_disabled():
    body = mapping.result_for(
        PostResult.DISABLED, None, require_delivery=True, ctx=CTX, actor_id="a", created_at="t"
    )
    assert body["outcome"] == mapping.OUTCOME_DELIVERY_FAILED


# -- ledger record: only status codes -----------------------------------------


def test_claim_record_is_proposed_agent_origin():
    record = mapping.claim_record(
        CTX,
        actor_id="notify-bridge",
        created_at="t",
        outcome="sent",
        delivered=True,
        status_code=204,
    )
    assert record["authority"] == "proposed"
    assert record["origin"] == {"kind": "agent", "actor_id": "notify-bridge"}
    assert record["data"]["status_code"] == 204
    assert record["data"]["delivered"] is True


def test_ledger_record_and_result_never_carry_the_webhook_url_or_message_body():
    """The hard constraint issue #68 exists to enforce: the URL, and the
    message that was sent, must never reach a journaled/ledgered record --
    only status codes. This test proves it by building the full result
    around a payload that CONTAINS the secret URL as message text, and
    asserting the URL string is nowhere in the serialized result."""
    parsed, error = mapping.parse_message(
        {"content": f"posting to {SECRET_URL}", "require_delivery": True}
    )
    assert error is None
    body = mapping.result_for(
        PostResult.FAILED,
        500,
        require_delivery=parsed.require_delivery,
        ctx=CTX,
        actor_id="a",
        created_at="t",
    )
    serialized = json.dumps(body)
    assert SECRET_URL not in serialized
    # And the ledger record's `data` block is exactly the status-code
    # vocabulary -- no message content at all.
    record_data = body["ledger_delta"]["records"][0]["data"]
    assert set(record_data) == {"statement", "kind", "outcome", "delivered", "status_code"}
