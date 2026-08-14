"""Server-level tests: real HTTP over loopback against a real state dir and
a fake §13.4 callback receiver. These prove the whole t12 acceptance set:

* an invocation POST parks as 202 with a durable pending task and no
  lease-holding behavior (heartbeat_after_seconds 0, no heartbeat events);
* a submission through the human surface completes the invocation via the
  standard authenticated callback (envelope asserted on a stub receiver);
* pending tasks survive a bridge restart (server torn down and rebuilt over
  the same state dir).
"""

from __future__ import annotations

import json
import socket
import time
import urllib.error
import urllib.request

import pytest

from human_inbox_bridge import server
from human_inbox_bridge.config import Config
from human_inbox_bridge.store import TaskStore

from ._fakes import FakeCallbackReceiver

AUTH = {"Authorization": "Bearer s3cr3t"}


def _request(base_url, path, *, method="POST", body=None, headers=None):
    url = base_url + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _config(tmp_path, **overrides):
    fields = dict(
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        callback_timeout_seconds=5.0,
        callback_max_retries=1,
        callback_retry_backoff_seconds=0.01,
    )
    fields.update(overrides)
    return Config(**fields)


@pytest.fixture()
def receiver():
    r = FakeCallbackReceiver()
    yield r
    r.close()


@pytest.fixture()
def bridge(tmp_path):
    cfg = _config(tmp_path)
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg
    srv.shutdown()
    srv.server_close()


def _invocation_body(receiver, **input_overrides):
    input_payload = {"instruction": "approve the release"}
    input_payload.update(input_overrides)
    return {
        "protocol_version": "1.0",
        "run_id": "run_1",
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": "att_1",
        "attempt": 1,
        "workflow": {"name": "wf", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": input_payload,
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": receiver.url, "token": "cbtok"},
    }


def _invoke(base, receiver, *, idem_key="att_1", **input_overrides):
    return _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(receiver, **input_overrides),
        headers={**AUTH, "Idempotency-Key": idem_key},
    )


# -- plumbing ---------------------------------------------------------------


def test_healthz_is_open(bridge):
    base, _cfg = bridge
    status, body = _request(base, "/healthz", method="GET")
    assert status == 200
    assert body == {"status": "ok"}


def test_unknown_route_is_404(bridge):
    base, _cfg = bridge
    status, _ = _request(base, "/nope", method="GET")
    assert status == 404


def _host_port(base):
    host, port = base.removeprefix("http://").split(":")
    return host, int(port)


def test_oversized_body_is_413_and_closes_the_connection(bridge, monkeypatch):
    """A body over MAX_BODY_BYTES must be refused with 413 + Connection:
    close, never silently truncated: a truncating read leaves the remainder
    unread on the keep-alive connection, and the next request's parse would
    start mid-body (request desynchronization)."""
    base, _cfg = bridge
    monkeypatch.setattr(server, "MAX_BODY_BYTES", 1024)
    host, port = _host_port(base)
    body = b"x" * 4096
    request = (
        f"POST {server.INVOCATIONS_PATH} HTTP/1.1\r\n"
        f"Host: {host}\r\n"
        "Authorization: Bearer s3cr3t\r\n"
        "Idempotency-Key: att_oversize\r\n"
        "Content-Type: application/json\r\n"
        f"Content-Length: {len(body)}\r\n"
        "\r\n"
    ).encode("ascii") + body

    with socket.create_connection((host, port), timeout=10) as sock:
        sock.sendall(request)
        raw = b""
        while True:
            chunk = sock.recv(65536)
            if not chunk:  # EOF: the server really closed the connection
                break
            raw += chunk

    head = raw.split(b"\r\n\r\n", 1)[0].decode("latin-1")
    status_line = head.splitlines()[0]
    assert " 413 " in status_line, status_line
    assert "connection: close" in head.lower()
    # The refusal poisoned nothing: a fresh connection is served normally.
    status, _ = _request(base, "/healthz", method="GET")
    assert status == 200


def test_keep_alive_connection_serves_sequential_requests(bridge):
    """The 413 close path must not cost normal requests their keep-alive:
    two well-formed requests on one connection both get answered."""
    base, _cfg = bridge
    host, port = _host_port(base)
    get = f"GET /healthz HTTP/1.1\r\nHost: {host}\r\n\r\n".encode("ascii")

    with socket.create_connection((host, port), timeout=10) as sock:
        sock.sendall(get + get)
        raw = b""
        while raw.count(b"HTTP/1.1 200") < 2:
            chunk = sock.recv(65536)
            if not chunk:
                break
            raw += chunk

    assert raw.count(b"HTTP/1.1 200") == 2


# -- auth -------------------------------------------------------------------


def test_invocation_without_auth_is_401(bridge, receiver):
    base, _cfg = bridge
    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(receiver),
        headers={"Idempotency-Key": "att_1"},
    )
    assert status == 401
    assert body["class"] == "auth_or_policy"


def test_invocation_with_wrong_token_is_401(bridge, receiver):
    base, _cfg = bridge
    status, _ = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(receiver),
        headers={"Authorization": "Bearer wrong", "Idempotency-Key": "att_1"},
    )
    assert status == 401


def test_inbox_list_requires_auth(bridge):
    base, _cfg = bridge
    status, _ = _request(base, "/inbox/tasks", method="GET")
    assert status == 401


def test_inbox_submit_requires_auth(bridge):
    base, _cfg = bridge
    status, _ = _request(base, "/inbox/tasks/hit_x/submit", body={"outcome": "ok"})
    assert status == 401


# -- invocation validation --------------------------------------------------


def test_missing_idempotency_key_is_400(bridge, receiver):
    base, _cfg = bridge
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(receiver), headers=AUTH
    )
    assert status == 400
    assert "Idempotency-Key" in body["error"]


def test_wrong_protocol_version_is_400(bridge, receiver):
    base, _cfg = bridge
    payload = _invocation_body(receiver)
    payload["protocol_version"] = "2.0"
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=payload, headers={**AUTH, "Idempotency-Key": "k"}
    )
    assert status == 400
    assert "protocol_version" in body["error"]


def test_missing_instruction_is_400(bridge, receiver):
    base, _cfg = bridge
    payload = _invocation_body(receiver)
    payload["input"] = {}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=payload, headers={**AUTH, "Idempotency-Key": "k"}
    )
    assert status == 400
    assert "instruction" in body["error"]


def test_missing_callback_is_400(bridge, receiver):
    # This bridge is ALWAYS asynchronous — a human answers later or never —
    # so an invocation without a callback could never be completed.
    base, _cfg = bridge
    payload = _invocation_body(receiver)
    payload["callback"] = {}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=payload, headers={**AUTH, "Idempotency-Key": "k"}
    )
    assert status == 400
    assert "callback" in body["error"]


def test_empty_body_is_400(bridge):
    base, _cfg = bridge
    status, _ = _request(
        base,
        server.INVOCATIONS_PATH,
        body=None,
        headers={**AUTH, "Idempotency-Key": "k", "Content-Type": "application/json"},
    )
    assert status == 400


# -- acceptance 1: park as 202, durable, no lease -----------------------------


def test_invocation_parks_202_durable_no_lease(bridge, receiver):
    base, cfg = bridge
    status, body = _invoke(base, receiver)
    assert status == 202
    invocation_id = body["invocation_id"]
    assert invocation_id
    # No liveness promise: the control plane's wait must be open-ended.
    assert body["heartbeat_after_seconds"] == 0
    assert body["supports_cancellation"] is True

    # The accepted (non-terminal) event is delivered through the callback.
    accepted = receiver.wait_for_kind("accepted", timeout=10.0)
    assert accepted is not None
    assert accepted["sequence"] == 1
    assert accepted["payload"]["invocation_id"] == invocation_id
    assert receiver.tokens[0] == "Bearer cbtok"

    # Durable: the task is on disk, pending, before any submission.
    task = TaskStore(cfg.state_dir).get(invocation_id)
    assert task is not None
    assert task.status == "pending"
    assert task.instruction == "approve the release"

    # No lease-holding behavior: no heartbeat/progress polling loop runs.
    time.sleep(0.4)
    kinds = [ev["kind"] for ev in receiver.events]
    assert "heartbeat" not in kinds
    assert kinds.count("accepted") == 1
    assert not any(ev["kind"] in ("completed", "failed") for ev in receiver.events)


def test_idempotent_replay_returns_same_body_without_a_second_task(bridge, receiver):
    base, cfg = bridge
    _, first = _invoke(base, receiver, idem_key="att_same")
    status, replay = _invoke(base, receiver, idem_key="att_same")
    assert status == 202
    assert replay == first
    assert len(TaskStore(cfg.state_dir).list()) == 1


def test_inbox_list_shows_pending_task_without_callback_token(bridge, receiver):
    base, _cfg = bridge
    _, body = _invoke(base, receiver)
    status, listing = _request(base, "/inbox/tasks?status=pending", method="GET", headers=AUTH)
    assert status == 200
    assert len(listing["tasks"]) == 1
    task = listing["tasks"][0]
    assert task["invocation_id"] == body["invocation_id"]
    assert task["instruction"] == "approve the release"
    assert "cbtok" not in json.dumps(listing)


# -- acceptance 2: submission completes via the standard callback -------------


def test_submission_delivers_completed_event(bridge, receiver):
    base, cfg = bridge
    _, accepted_body = _invoke(base, receiver)
    invocation_id = accepted_body["invocation_id"]
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None

    status, body = _request(
        base,
        f"/inbox/tasks/{invocation_id}/submit",
        body={"outcome": "approved", "output": {"verdict": "ship"}, "note": "checked by hand"},
        headers=AUTH,
    )
    assert status == 200
    assert body["status"] == "completed"
    assert body["delivered"] is True

    ev = receiver.wait_for_kind("completed", timeout=10.0)
    assert ev is not None
    # Envelope: stable id, monotonic sequence after the accepted event.
    assert ev["event_id"] == f"evt_{invocation_id}_2"
    assert ev["sequence"] == 2
    payload = ev["payload"]
    assert payload["outcome"] == "approved"
    assert payload["output"] == {"verdict": "ship"}
    assert "usage" not in payload
    rec = payload["ledger_delta"]["records"][0]
    assert rec["origin"]["kind"] == "human"
    assert rec["authority"] == "proposed"
    assert rec["run_id"] == "run_1"
    assert rec["attempt_id"] == "att_1"
    assert rec["data"]["statement"] == "checked by hand"

    # The task left the pending list durably.
    task = TaskStore(cfg.state_dir).get(invocation_id)
    assert task.status == "completed"
    assert task.submission["outcome"] == "approved"


def test_submit_unknown_task_is_404(bridge):
    base, _cfg = bridge
    status, _ = _request(
        base, "/inbox/tasks/hit_missing/submit", body={"outcome": "ok"}, headers=AUTH
    )
    assert status == 404


def test_submit_invalid_body_is_400(bridge, receiver):
    base, _cfg = bridge
    _, body = _invoke(base, receiver)
    status, err = _request(
        base, f"/inbox/tasks/{body['invocation_id']}/submit", body={"output": {}}, headers=AUTH
    )
    assert status == 400
    assert "outcome" in err["error"]


def test_double_submit_is_409(bridge, receiver):
    base, _cfg = bridge
    _, body = _invoke(base, receiver)
    invocation_id = body["invocation_id"]
    _request(base, f"/inbox/tasks/{invocation_id}/submit", body={"outcome": "ok"}, headers=AUTH)
    status, _ = _request(
        base, f"/inbox/tasks/{invocation_id}/submit", body={"outcome": "ok"}, headers=AUTH
    )
    assert status == 409


def test_failed_delivery_keeps_the_task_pending(bridge, receiver):
    base, cfg = bridge
    _, body = _invoke(base, receiver)
    invocation_id = body["invocation_id"]
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None
    receiver.close()  # the callback endpoint goes away

    status, resp = _request(
        base, f"/inbox/tasks/{invocation_id}/submit", body={"outcome": "ok"}, headers=AUTH
    )
    assert status == 502
    assert resp["delivered"] is False
    # Not silently lost: the task stays pending so the human can resubmit.
    assert TaskStore(cfg.state_dir).get(invocation_id).status == "pending"


# -- acceptance 3: restart survival ------------------------------------------


def test_pending_tasks_survive_a_bridge_restart(tmp_path, receiver):
    cfg = _config(tmp_path)
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    base = f"http://{host}:{port}"
    _, body = _invoke(base, receiver)
    invocation_id = body["invocation_id"]
    assert receiver.wait_for_kind("accepted", timeout=10.0) is not None
    # Kill the bridge.
    srv.shutdown()
    srv.server_close()

    # Reload over the same state dir — as after a process restart.
    cfg2 = _config(tmp_path)
    srv2, _thread2 = server.start_background(cfg2)
    host2, port2 = srv2.server_address
    base2 = f"http://{host2}:{port2}"
    try:
        status, listing = _request(base2, "/inbox/tasks?status=pending", method="GET", headers=AUTH)
        assert status == 200
        assert [t["invocation_id"] for t in listing["tasks"]] == [invocation_id]

        # And the submission still completes through the callback, with the
        # persisted sequence counter continuing after the accepted event.
        status, resp = _request(
            base2, f"/inbox/tasks/{invocation_id}/submit", body={"outcome": "done"}, headers=AUTH
        )
        assert status == 200
        assert resp["delivered"] is True
        ev = receiver.wait_for_kind("completed", timeout=10.0)
        assert ev is not None
        assert ev["sequence"] == 2
        assert ev["event_id"] == f"evt_{invocation_id}_2"
    finally:
        srv2.shutdown()
        srv2.server_close()


# -- cancellation -------------------------------------------------------------


def test_cancel_removes_from_pending(bridge, receiver):
    base, cfg = bridge
    _, body = _invoke(base, receiver)
    invocation_id = body["invocation_id"]
    status, resp = _request(base, f"/v1/invocations/{invocation_id}/cancel", body={}, headers=AUTH)
    assert status == 202
    assert resp["status"] == "cancel-requested"
    assert TaskStore(cfg.state_dir).get(invocation_id).status == "cancelled"
    _, listing = _request(base, "/inbox/tasks?status=pending", method="GET", headers=AUTH)
    assert listing["tasks"] == []


def test_delete_alias_cancels(bridge, receiver):
    base, cfg = bridge
    _, body = _invoke(base, receiver)
    invocation_id = body["invocation_id"]
    status, _ = _request(base, f"/v1/invocations/{invocation_id}", method="DELETE", headers=AUTH)
    assert status == 202
    assert TaskStore(cfg.state_dir).get(invocation_id).status == "cancelled"


def test_cancel_unknown_id_still_202(bridge):
    # PRD §13.6: cancellation is best-effort at the actor; nothing to
    # cooperate with is not an error.
    base, _cfg = bridge
    status, _ = _request(base, "/v1/invocations/hit_gone/cancel", body={}, headers=AUTH)
    assert status == 202


# -- exposure guard -----------------------------------------------------------


def test_non_loopback_bind_without_token_is_refused(tmp_path):
    cfg = _config(tmp_path, auth_token=None, host="0.0.0.0")  # noqa: S104 - the refused case
    with pytest.raises(SystemExit):
        server.make_server(cfg)


# -- observable-declaration convention (t15 / c11 / h8) ----------------------


def test_observe_input_key_rounds_trips_verbatim_in_extra_input(bridge, receiver):
    """A non-instruction input key (e.g. ``observe``) persists verbatim in
    ``extra_input`` and is returned on the task GET — the bridge does not
    strip or transform it (server.py line 369: ``extra_input={k: v for k, v
    in raw_input.items() if k != "instruction"}``)."""
    base, cfg = bridge
    observe_payload = {
        "kind": "github_pr_merged",
        "pr": 42,
    }
    body = _invocation_body(receiver, instruction="merge the PR", observe=observe_payload)
    status, accepted_body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=body,
        headers={**AUTH, "Idempotency-Key": "att_observe"},
    )
    assert status == 202
    invocation_id = accepted_body["invocation_id"]

    # The task's extra_input carries observe verbatim.
    task = TaskStore(cfg.state_dir).get(invocation_id)
    assert task is not None
    assert task.extra_input == {"observe": observe_payload}

    # The inbox GET also returns it (public_dict includes extra_input).
    status, listing = _request(base, "/inbox/tasks?status=pending", method="GET", headers=AUTH)
    assert status == 200
    task_dict = next(t for t in listing["tasks"] if t["invocation_id"] == invocation_id)
    assert task_dict["extra_input"] == {"observe": observe_payload}
