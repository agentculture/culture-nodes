"""End-to-end tests against a REAL subprocess — but the fake `claude` CLI
(`tests/fake_claude.py`), never the real, billed, network-dependent binary.
See that script's own docstring, and README.md's "Tests" section, for why:
unlike colleague's own `COLLEAGUE_ENGINE=mock`, the real `claude` CLI has no
offline mock mode.

These tests exercise the full stack `test_server_unit.py` monkeypatches
around: real `subprocess.Popen`, real stdout parsing, real detached
background dispatch + flight-file tailing, real SIGTERM-based cancellation.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request

from claude_code_bridge import flightfiles, server
from claude_code_bridge.config import Config

from ._fakes import FakeCallbackReceiver, fake_claude_path


def _request(base_url, path, *, method="POST", body=None, headers=None):
    url = base_url + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _invocation_body(
    repo, *, instruction, run_id, attempt_id, callback_url, callback_token, **input_overrides
):
    input_payload = {"instruction": instruction, "repo": str(repo)}
    input_payload.update(input_overrides)
    return {
        "protocol_version": "1.0",
        "run_id": run_id,
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": attempt_id,
        "attempt": 1,
        "workflow": {"name": "t12-integration", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": input_payload,
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": callback_url, "token": callback_token},
    }


def _bridge(repo, tmp_path, **overrides):
    cfg = Config(
        repo_allowlist=(str(repo),),
        claude_bin=fake_claude_path(),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="integration-secret",
        sync_max_steps=6,
        default_max_steps=6,
        poll_interval_seconds=0.05,
        callback_retry_backoff_seconds=0.1,
        heartbeat_after_seconds=2,
    )
    for k, v in overrides.items():
        setattr(cfg, k, v)
    srv, thread = server.start_background(cfg)
    return srv, cfg


def test_sync_invocation_completes_with_mapped_output(tmp_path, monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_RESULT_TEXT", "a short note about t12 (sync)")
    repo = tmp_path / "repo"
    repo.mkdir()
    srv, cfg = _bridge(repo, tmp_path)
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_sync_it"}
        body = _invocation_body(
            repo,
            instruction="write a short note about t12 (sync)",
            run_id="run_sync_it",
            attempt_id="att_sync_it",
            callback_url="http://127.0.0.1:1/unused",
            callback_token="unused",
        )
        status, response = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status == 200, response
        assert response["outcome"] == "completed"
        assert response["output"]["summary"] == "a short note about t12 (sync)"
        assert response["ledger_delta"]["records"][0]["authority"] == "proposed"
        assert response["ledger_delta"]["records"][0]["record_type"] == "claim"
        assert response["usage"]["input_tokens"] >= 0
        assert response["usage"]["output_tokens"] >= 0
    finally:
        srv.shutdown()
        srv.server_close()


def test_idempotent_replay_does_not_dispatch_claude_twice(tmp_path, monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    repo = tmp_path / "repo"
    repo.mkdir()
    srv, cfg = _bridge(repo, tmp_path)
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_replay_it"}
        body = _invocation_body(
            repo,
            instruction="write a short note about t12 (replay)",
            run_id="run_replay_it",
            attempt_id="att_replay_it",
            callback_url="http://127.0.0.1:1/unused",
            callback_token="unused",
        )
        status1, response1 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        status2, response2 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status1 == status2 == 200
        assert (
            response1["ledger_delta"]["records"][0]["data"]["claude_session_id"]
            == response2["ledger_delta"]["records"][0]["data"]["claude_session_id"]
        )
        assert response1 == response2
    finally:
        srv.shutdown()
        srv.server_close()


def test_async_invocation_202s_and_delivers_accepted_then_completed(tmp_path, monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_STREAM_DELAY", "0.02")
    repo = tmp_path / "repo"
    repo.mkdir()
    srv, cfg = _bridge(repo, tmp_path)
    receiver = FakeCallbackReceiver()
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_async_it"}
        body = _invocation_body(
            repo,
            instruction="write a short note about t12 (async)",
            run_id="run_async_it",
            attempt_id="att_async_it",
            callback_url=receiver.url,
            callback_token="async-cb-token",
            **{"async": True},
        )
        status, accepted = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status == 202, accepted
        assert accepted["invocation_id"]
        assert accepted["supports_cancellation"] is True

        accepted_event = receiver.wait_for_kind("accepted", timeout=30)
        assert accepted_event is not None
        assert accepted_event["payload"]["invocation_id"] == accepted["invocation_id"]

        completed_event = receiver.wait_for_kind("completed", timeout=30)
        assert completed_event is not None
        assert completed_event["sequence"] > accepted_event["sequence"]
        assert completed_event["payload"]["outcome"] == "completed"
        assert completed_event["payload"]["ledger_delta"]["records"][0]["authority"] == "proposed"

        # At least one progress event arrived from the fake's stream-json
        # lines before the terminal one.
        progress = receiver.wait_for_kind("progress", timeout=1)
        assert progress is not None

        assert all(tok == "Bearer async-cb-token" for tok in receiver.tokens)
    finally:
        receiver.close()
        srv.shutdown()
        srv.server_close()


def test_cancellation_writes_the_control_file_and_sigterms_the_child(tmp_path, monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_HANG", "1")  # never finishes on its own
    repo = tmp_path / "repo"
    repo.mkdir()
    srv, cfg = _bridge(repo, tmp_path, poll_interval_seconds=0.02)
    receiver = FakeCallbackReceiver()
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_cancel_it"}
        body = _invocation_body(
            repo,
            instruction="hang until cancelled (t12 cancel)",
            run_id="run_cancel_it",
            attempt_id="att_cancel_it",
            callback_url=receiver.url,
            callback_token="cancel-cb-token",
            **{"async": True},
        )
        status, accepted = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status == 202, accepted
        invocation_id = accepted["invocation_id"]

        # Give the poller a moment to actually start (and the fake hung
        # process to be alive) before cancelling.
        time.sleep(0.1)

        cancel_status, cancel_body = _request(
            base,
            f"/v1/invocations/{invocation_id}/cancel",
            body={"invocation_id": invocation_id, "reason": "integration test"},
            headers={"Authorization": f"Bearer {cfg.auth_token}"},
        )
        assert cancel_status == 202
        assert cancel_body["invocation_id"] == invocation_id

        control_path = flightfiles.control_path(cfg.state_dir, invocation_id)
        assert control_path.is_file(), f"expected a cooperative-stop control file at {control_path}"
        data = json.loads(control_path.read_text())
        assert data["stop"] is True

        # A terminal event still arrives — the hung fake process is SIGTERM'd
        # by the poller once it observes the control file, exits without
        # producing a `type: "result"` record, and that maps to `failed`
        # (never a silent success) per incomplete/crashed-never-success.
        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=15)
        assert terminal is not None, "expected SOME terminal callback event after cancellation"
        assert terminal["kind"] == "failed"
    finally:
        receiver.close()
        srv.shutdown()
        srv.server_close()


def test_crashed_session_never_success_over_a_real_subprocess(tmp_path, monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    monkeypatch.setenv("FAKE_CLAUDE_CRASH", "1")
    repo = tmp_path / "repo"
    repo.mkdir()
    srv, cfg = _bridge(repo, tmp_path)
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_crash_it"}
        body = _invocation_body(
            repo,
            instruction="this session will crash",
            run_id="run_crash_it",
            attempt_id="att_crash_it",
            callback_url="http://127.0.0.1:1/unused",
            callback_token="unused",
        )
        status, response = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status != 200
        assert response.get("outcome") != "completed"
        assert response["class"] == "execution"
    finally:
        srv.shutdown()
        srv.server_close()


def test_repo_outside_allowlist_is_refused_before_any_dispatch(tmp_path, monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.226")
    repo = tmp_path / "repo"
    repo.mkdir()
    srv, cfg = _bridge(repo, tmp_path)
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {
            "Authorization": f"Bearer {cfg.auth_token}",
            "Idempotency-Key": "att_disallowed_it",
        }
        body = _invocation_body(
            "/etc",
            instruction="this must never run",
            run_id="run_disallowed_it",
            attempt_id="att_disallowed_it",
            callback_url="http://127.0.0.1:1/unused",
            callback_token="unused",
        )
        status, response = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status == 403
        assert "allowlist" in response["error"]
    finally:
        srv.shutdown()
        srv.server_close()


def test_version_gate_refuses_a_real_dispatch_against_an_old_fake_binary(tmp_path, monkeypatch):
    """Acceptance criterion #3, end to end over a real subprocess: the
    version probe itself uses the same fake binary, reporting a version
    below the pin."""
    monkeypatch.setenv("FAKE_CLAUDE_VERSION", "2.1.100")
    repo = tmp_path / "repo"
    repo.mkdir()
    srv, cfg = _bridge(repo, tmp_path, min_claude_version="2.1.220")
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_version_it"}
        body = _invocation_body(
            repo,
            instruction="should never run",
            run_id="run_version_it",
            attempt_id="att_version_it",
            callback_url="http://127.0.0.1:1/unused",
            callback_token="unused",
        )
        status, response = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status == 503
        assert response["class"] == "actor_unavailable"
        assert "2.1.100" in response["error"]
        assert "2.1.220" in response["error"]
    finally:
        srv.shutdown()
        srv.server_close()
