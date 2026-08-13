"""§13.4 delivery discipline, mirroring the sibling bridges' test_callbacks:
stable event identity across redeliveries, bearer auth on every POST, and
give-up (not infinite retry) on statuses where retrying cannot help."""

from __future__ import annotations

import pytest

from human_inbox_bridge.callbacks import CallbackConfig, CallbackEmitter

from ._fakes import FakeCallbackReceiver


@pytest.fixture()
def receiver():
    r = FakeCallbackReceiver()
    yield r
    r.close()


def _emitter(receiver, **cfg_overrides):
    cfg = CallbackConfig(timeout_seconds=5.0, max_retries=3, backoff_seconds=0.01)
    for k, v in cfg_overrides.items():
        setattr(cfg, k, v)
    return CallbackEmitter(receiver.url, "cbtok", "hit_1", cfg)


def test_send_carries_bearer_token_and_sequence(receiver):
    emitter = _emitter(receiver)
    assert emitter.send("accepted", {"invocation_id": "hit_1"}) is True
    ev = receiver.wait_for_kind("accepted", timeout=5.0)
    assert ev is not None
    assert ev["sequence"] == 1
    assert ev["event_id"] == "evt_hit_1_1"
    assert receiver.tokens[0] == "Bearer cbtok"


def test_sequences_increment_per_distinct_event(receiver):
    emitter = _emitter(receiver)
    emitter.send("accepted", {})
    emitter.send("completed", {"outcome": "done"})
    completed = receiver.wait_for_kind("completed", timeout=5.0)
    assert completed["sequence"] == 2


def test_refused_delivery_is_redelivered_with_the_same_identity(receiver):
    emitter = _emitter(receiver)
    receiver.refuse_next("evt_hit_1_1", 2)
    assert emitter.send("completed", {"outcome": "done"}) is True
    ev = receiver.wait_for_kind("completed", timeout=5.0)
    # Same event id and sequence as the refused deliveries — never a fresh
    # identity for what must remain the same event.
    assert ev["event_id"] == "evt_hit_1_1"
    assert ev["sequence"] == 1


def test_resend_uses_the_supplied_identity(receiver):
    emitter = _emitter(receiver)
    assert emitter.resend("evt_hit_1_9", 9, "completed", {"outcome": "done"}) is True
    ev = receiver.wait_for_kind("completed", timeout=5.0)
    assert ev["event_id"] == "evt_hit_1_9"
    assert ev["sequence"] == 9


def test_exhausted_retries_report_failure(receiver):
    emitter = _emitter(receiver, max_retries=1)
    receiver.refuse_next("evt_hit_1_1", 10)
    assert emitter.send("completed", {"outcome": "done"}) is False


def test_unreachable_receiver_reports_failure():
    cfg = CallbackConfig(timeout_seconds=0.2, max_retries=0, backoff_seconds=0.01)
    emitter = CallbackEmitter("http://127.0.0.1:1/events", "cbtok", "hit_1", cfg)
    assert emitter.send("completed", {"outcome": "done"}) is False
