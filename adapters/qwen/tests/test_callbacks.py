from __future__ import annotations

import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from qwen_bridge.callbacks import CallbackConfig, CallbackEmitter

from ._fakes import FakeCallbackReceiver


@pytest.fixture()
def receiver():
    r = FakeCallbackReceiver()
    yield r
    r.close()


def test_send_delivers_with_stable_id_and_incrementing_sequence(receiver):
    emitter = CallbackEmitter(
        receiver.url, "tok", "task1", CallbackConfig(max_retries=1, backoff_seconds=0.01)
    )
    assert emitter.send("accepted", {"a": 1}) is True
    assert emitter.send("progress", {"b": 2}) is True
    assert [e["event_id"] for e in receiver.events] == ["evt_task1_1", "evt_task1_2"]
    assert [e["sequence"] for e in receiver.events] == [1, 2]
    assert receiver.tokens == ["Bearer tok", "Bearer tok"]


def test_retries_and_redelivers_the_same_event_id_and_sequence(receiver):
    emitter = CallbackEmitter(
        receiver.url, "tok", "task1", CallbackConfig(max_retries=3, backoff_seconds=0.01)
    )
    event_id, sequence = emitter.next_event_id_and_sequence()
    receiver.refuse_next(event_id, 2)  # refuse the first two deliveries
    ok = emitter.resend(event_id, sequence, "completed", {"outcome": "completed"})
    assert ok is True
    assert len(receiver.events) == 1
    assert receiver.events[0]["event_id"] == event_id
    assert receiver.events[0]["sequence"] == sequence


def test_gives_up_after_max_retries_exhausted(receiver):
    emitter = CallbackEmitter(
        receiver.url, "tok", "task1", CallbackConfig(max_retries=1, backoff_seconds=0.01)
    )
    event_id, sequence = emitter.next_event_id_and_sequence()
    receiver.refuse_next(event_id, 10)  # always refuse
    ok = emitter.resend(event_id, sequence, "completed", {})
    assert ok is False
    assert receiver.events == []


def test_gives_up_immediately_on_401_without_retrying(receiver):
    receiver.close()  # replace with a receiver that always answers 401

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):  # noqa: N802
            length = int(self.headers.get("Content-Length", "0") or "0")
            self.rfile.read(length)
            self.send_response(401)
            self.send_header("Content-Length", "0")
            self.end_headers()

    server = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(
        target=server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True
    )
    thread.start()
    try:
        host, port = server.server_address
        emitter = CallbackEmitter(
            f"http://{host}:{port}/events",
            "tok",
            "task1",
            CallbackConfig(max_retries=5, backoff_seconds=0.01),
        )
        assert emitter.send("accepted", {}) is False
    finally:
        server.shutdown()
        server.server_close()


def test_unreachable_url_is_treated_as_a_retryable_failure():
    emitter = CallbackEmitter(
        "http://127.0.0.1:1/nope",
        "tok",
        "task1",
        CallbackConfig(max_retries=1, backoff_seconds=0.01, timeout_seconds=1),
    )
    assert emitter.send("accepted", {}) is False


def test_non_http_scheme_is_refused_without_ever_calling_urlopen():
    """A callback URL outside {http, https} (e.g. `file:///etc/passwd`) is
    refused before urlopen is ever called — the explicit scheme allowlist
    behind bandit's B310 check, not a suppression of it."""
    emitter = CallbackEmitter(
        "file:///etc/passwd",
        "tok",
        "task1",
        CallbackConfig(max_retries=0, backoff_seconds=0.01, timeout_seconds=1),
    )
    assert emitter.send("accepted", {}) is False
