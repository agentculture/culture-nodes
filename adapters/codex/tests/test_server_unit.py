"""Server-level unit tests: real HTTP over loopback, but `codex_cli`'s
subprocess calls are monkeypatched out — these prove the request-handling
ladder (auth, Idempotency-Key, protocol_version, repo allowlist, sandbox
validation, dispatch decision, idempotent replay) without needing a real
`codex` binary. `test_async_runner.py` covers the real-subprocess async
path against a fake executable; `test_integration_bridge.py` covers the
real `codex` subprocess path end to end (skipped without a real, logged-in
codex)."""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from http.client import HTTPConnection

import pytest
from codex_bridge import codex_cli, server
from codex_bridge.config import Config

from ._fakes import FakeCallbackReceiver


def _request(base_url, path, *, method="POST", body=None, headers=None):
    url = base_url + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


@pytest.fixture()
def bridge_url(tmp_path, monkeypatch):
    cfg = Config(
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        sync_max_steps=6,
        default_max_steps=6,
    )
    srv, thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg, tmp_path
    srv.shutdown()
    srv.server_close()


def _invocation_body(repo, **input_overrides):
    input_payload = {"instruction": "say hello", "repo": repo}
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
        "callback": {"url": "http://127.0.0.1:1/callback", "token": "cbtok"},
    }


def _auth_header(cfg):
    return {"Authorization": f"Bearer {cfg.auth_token}"}


def test_healthz(bridge_url):
    base, _cfg, _repo = bridge_url
    status, body = _request(base, "/healthz", method="GET")
    assert status == 200
    assert body == {"status": "ok"}


def test_missing_auth_is_401(bridge_url):
    base, cfg, repo = bridge_url
    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(str(repo)),
        headers={"Idempotency-Key": "att_1"},
    )
    assert status == 401
    assert body["class"] == "auth_or_policy"


def test_wrong_auth_is_401(bridge_url):
    base, cfg, repo = bridge_url
    headers = {"Authorization": "Bearer wrong", "Idempotency-Key": "att_1"}
    status, _body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status == 401


def test_missing_idempotency_key_is_400(bridge_url):
    base, cfg, repo = bridge_url
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=_auth_header(cfg)
    )
    assert status == 400
    assert "Idempotency-Key" in body["error"]


def test_wrong_protocol_version_is_400(bridge_url):
    base, cfg, repo = bridge_url
    payload = _invocation_body(str(repo))
    payload["protocol_version"] = "2.0"
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_x"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 400
    assert "protocol_version" in body["error"]


def test_missing_instruction_is_400(bridge_url):
    base, cfg, repo = bridge_url
    payload = _invocation_body(str(repo))
    del payload["input"]["instruction"]
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_no_instr"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 400
    assert "instruction" in body["error"]


def test_repo_outside_allowlist_is_403(bridge_url, tmp_path):
    base, cfg, repo = bridge_url
    other = tmp_path.parent / "not-allowed"
    other.mkdir(exist_ok=True)
    payload = _invocation_body(str(other))
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_other_repo"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 403
    assert "allowlist" in body["error"]


def test_unknown_sandbox_is_400(bridge_url):
    base, cfg, repo = bridge_url
    payload = _invocation_body(str(repo), sandbox="not-a-real-sandbox")
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_sandbox"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 400
    assert "sandbox" in body["error"]


def test_sync_dispatch_maps_ok_result_to_200(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, model, sandbox):
        return codex_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "task_id": "019fe54f-8e7b-7940-943c-1728fd3a7c6b",
                "status": "ok",
                "summary": "did it",
                "changed_files": ["a.py"],
                "usage": {"input_tokens": 1, "output_tokens": 2},
                "error": None,
            },
            timed_out=False,
        )

    monkeypatch.setattr(codex_cli, "run_sync", fake_run_sync)

    headers = {**_auth_header(cfg), "Idempotency-Key": "att_sync_ok"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status == 200
    assert body["outcome"] == "completed"
    assert body["output"]["summary"] == "did it"
    assert body["usage"] == {
        "input_tokens": 1,
        "output_tokens": 2,
        "cost": None,
        "currency": None,
        "thread_id": "019fe54f-8e7b-7940-943c-1728fd3a7c6b",
    }
    assert body["ledger_delta"]["records"][0]["authority"] == "proposed"


def test_sync_dispatch_maps_crashed_incomplete_session_to_execution_failure_never_success(
    bridge_url, monkeypatch
):
    """The literal acceptance-criterion test at the HTTP layer: a crashed
    codex session (status=incomplete, the shape `codex_cli.parse_session`
    produces when no terminal turn event was ever seen — regardless of exit
    code) must never come back as a 200 'completed' — no adapter-specific
    exemption."""
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, model, sandbox):
        return codex_cli.SyncRunResult(
            exit_code=0,  # deliberately 0 — mirrors the real grounded SIGTERM case
            stdout="",
            stderr="",
            task_result={
                "task_id": "019fe553-362a-7191-aa66-6c03191830b1",
                "status": "incomplete",
                "summary": "partial",
                "changed_files": [],
                "usage": {},
                "error": None,
            },
            timed_out=False,
        )

    monkeypatch.setattr(codex_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_sync_incomplete"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status != 200
    assert body.get("outcome") != "completed"
    assert body["class"] == "execution"


def test_sync_dispatch_maps_crash_before_any_output_to_execution_failure(bridge_url, monkeypatch):
    """A codex process that crashed before writing anything parseable at
    all (task_result is None) is also never success."""
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, model, sandbox):
        return codex_cli.SyncRunResult(
            exit_code=1, stdout="", stderr="segfault or similar", task_result=None, timed_out=False
        )

    monkeypatch.setattr(codex_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_sync_crash"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status != 200
    assert body.get("outcome") != "completed"
    assert body["class"] == "execution"


def test_idempotent_replay_returns_the_same_response_without_recalling_codex(
    bridge_url, monkeypatch
):
    base, cfg, repo = bridge_url
    calls = []

    def fake_run_sync(cfg_, instruction, repo_, *, model, sandbox):
        calls.append(instruction)
        return codex_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "task_id": "abc",
                "status": "ok",
                "summary": "did it",
                "changed_files": [],
                "usage": {},
            },
            timed_out=False,
        )

    monkeypatch.setattr(codex_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_replay"}
    body = _invocation_body(str(repo))
    status1, resp1 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
    status2, resp2 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
    assert status1 == status2 == 200
    assert resp1 == resp2
    assert len(calls) == 1  # codex was invoked exactly once


def test_validation_failure_is_not_cached_for_replay(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url
    payload = _invocation_body(str(repo))
    del payload["input"]["instruction"]
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_fix_and_retry"}

    status1, body1 = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status1 == 400

    def fake_run_sync(cfg_, instruction, repo_, *, model, sandbox):
        return codex_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "task_id": "abc",
                "status": "ok",
                "summary": "fixed",
                "changed_files": [],
                "usage": {},
            },
            timed_out=False,
        )

    monkeypatch.setattr(codex_cli, "run_sync", fake_run_sync)
    payload["input"]["instruction"] = "say hello"  # the caller fixes their mistake
    status2, body2 = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status2 == 200
    assert body2["output"]["summary"] == "fixed"


def test_async_dispatch_returns_202_and_delivers_accepted_then_completed(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url
    receiver = FakeCallbackReceiver()
    try:

        def fake_start(
            self,
            *,
            instruction,
            repo,
            model,
            sandbox,
            ctx,
            callback_url,
            callback_token,
            heartbeat_after_seconds,
        ):
            from codex_bridge.callbacks import CallbackConfig, CallbackEmitter

            emitter = CallbackEmitter(
                callback_url, callback_token, "async123", CallbackConfig(backoff_seconds=0.01)
            )
            emitter.send(
                "accepted",
                {"invocation_id": "async123", "heartbeat_after_seconds": heartbeat_after_seconds},
            )
            from codex_bridge import mapping as mapping_mod

            ev = mapping_mod.terminal_event(
                {
                    "task_id": "async123",
                    "status": "ok",
                    "summary": "async done",
                    "changed_files": [],
                    "usage": {},
                },
                ctx,
                default_success_outcome=self._cfg.default_success_outcome,
                actor_id=self._cfg.actor_id,
                created_at="now",
            )
            emitter.send(ev.kind, ev.payload)
            return "async123"

        monkeypatch.setattr(server.AsyncRunner, "start", fake_start)

        payload = _invocation_body(str(repo))
        payload["input"]["async"] = True
        payload["callback"] = {"url": receiver.url, "token": "cbtok"}
        headers = {**_auth_header(cfg), "Idempotency-Key": "att_async"}
        status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
        assert status == 202
        assert body["invocation_id"] == "async123"
        assert body["supports_cancellation"] is True
        assert body["heartbeat_after_seconds"] == cfg.heartbeat_after_seconds

        accepted = receiver.wait_for_kind("accepted")
        assert accepted is not None
        assert accepted["sequence"] == 1

        completed = receiver.wait_for_kind("completed")
        assert completed is not None
        assert completed["sequence"] > accepted["sequence"]
        assert completed["payload"]["outcome"] == "completed"
        assert completed["payload"]["ledger_delta"]["records"][0]["authority"] == "proposed"
    finally:
        receiver.close()


def test_async_missing_callback_is_400(bridge_url):
    base, cfg, repo = bridge_url
    payload = _invocation_body(str(repo))
    payload["input"]["async"] = True
    payload["callback"] = {"url": "", "token": ""}
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_async_no_cb"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 400
    assert "callback" in body["error"]


def test_async_dispatch_failure_is_503(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url

    def fake_start(self, **kwargs):
        raise codex_cli.SpawnError("boom: codex binary not found")

    monkeypatch.setattr(server.AsyncRunner, "start", fake_start)
    payload = _invocation_body(str(repo))
    payload["input"]["async"] = True
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_async_fail"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 503
    assert body["class"] == "actor_unavailable"


def test_cancel_unknown_invocation_still_answers_success(bridge_url):
    base, cfg, repo = bridge_url
    status, body = _request(
        base,
        "/v1/invocations/no-such-id/cancel",
        body={"invocation_id": "no-such-id"},
        headers=_auth_header(cfg),
    )
    assert status == 202
    assert body["invocation_id"] == "no-such-id"


def test_cancel_requires_auth(bridge_url):
    base, cfg, repo = bridge_url
    status, _body = _request(base, "/v1/invocations/x/cancel", body={"invocation_id": "x"})
    assert status == 401


def test_delete_alias_for_cancel(bridge_url):
    base, cfg, repo = bridge_url
    status, body = _request(
        base,
        "/v1/invocations/x",
        method="DELETE",
        body={"invocation_id": "x"},
        headers=_auth_header(cfg),
    )
    assert status == 202
    assert body["invocation_id"] == "x"


def test_unauthenticated_bridge_allows_requests_without_a_token(tmp_path):
    cfg = Config(
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token=None,
    )
    srv, thread = server.start_background(cfg)
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        status, body = _request(base, "/healthz", method="GET")
        assert status == 200
    finally:
        srv.shutdown()
        srv.server_close()


def test_connection_reuse_keep_alive(bridge_url):
    """The server declares HTTP/1.1; a client should be able to issue more
    than one request over the same TCP connection."""
    base, cfg, _repo = bridge_url
    host_port = base.split("//", 1)[1]
    host, port = host_port.split(":")
    conn = HTTPConnection(host, int(port))
    try:
        conn.request("GET", "/healthz")
        resp1 = conn.getresponse()
        assert resp1.status == 200
        resp1.read()
        conn.request("GET", "/healthz")
        resp2 = conn.getresponse()
        assert resp2.status == 200
        resp2.read()
    finally:
        conn.close()
