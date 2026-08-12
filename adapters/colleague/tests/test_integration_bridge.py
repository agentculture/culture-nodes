"""End-to-end tests against a REAL `colleague work` subprocess.

Uses `COLLEAGUE_ENGINE=mock` (colleague's deterministic offline engine —
`colleague_bin`/`scratch_repo` in conftest.py verify it's reachable before
any test here runs) in the scratch repo the task's own acceptance
instructions name. These tests never touch the network beyond loopback.

Skips (never fails) when `colleague` is not on PATH — see conftest.py.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

from colleague_bridge import flightfiles, server
from colleague_bridge.config import Config

from ._fakes import FakeCallbackReceiver


def _request(base_url, path, *, method="POST", body=None, headers=None):
    url = base_url + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _invocation_body(repo, *, instruction, run_id, attempt_id, callback_url, callback_token, **input_overrides):
    input_payload = {"instruction": instruction, "repo": str(repo)}
    input_payload.update(input_overrides)
    return {
        "protocol_version": "1.0",
        "run_id": run_id,
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": attempt_id,
        "attempt": 1,
        "workflow": {"name": "t26-integration", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": input_payload,
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": callback_url, "token": callback_token},
    }


def _bridge(scratch_repo, colleague_bin, colleague_env, tmp_path, **overrides):
    cfg = Config(
        repo_allowlist=(str(scratch_repo),),
        colleague_bin=colleague_bin,
        colleague_env=colleague_env,
        open_pr=False,
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


def test_sync_invocation_completes_with_mapped_output(scratch_repo, colleague_bin, colleague_env, tmp_path):
    srv, cfg = _bridge(scratch_repo, colleague_bin, colleague_env, tmp_path)
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_sync_it"}
        body = _invocation_body(
            scratch_repo,
            instruction="write a short note about t26 (sync)",
            run_id="run_sync_it",
            attempt_id="att_sync_it",
            callback_url="http://127.0.0.1:1/unused",
            callback_token="unused",
        )
        status, response = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status == 200, response
        assert response["outcome"] == "completed"
        assert response["output"]["summary"]
        assert isinstance(response["output"]["changed_files"], list)
        assert response["output"]["artifacts_path"]
        assert response["ledger_delta"]["records"][0]["authority"] == "proposed"
        assert response["ledger_delta"]["records"][0]["record_type"] == "claim"
        assert response["usage"]["input_tokens"] >= 0
        assert response["usage"]["output_tokens"] >= 0
        # t10, over a REAL colleague session: workspace_measured is the
        # bridge's OWN git measurement of scratch_repo, structurally
        # distinct from output.changed_files above (which is whatever
        # colleague itself reported).
        wm = response["workspace_measured"]
        assert wm["measured"] is True
        assert wm["repo"] == str(scratch_repo)
        assert wm["head_before"]
        assert wm["head_after"]
        assert isinstance(wm["changed_files"], list)
    finally:
        srv.shutdown()
        srv.server_close()


def test_idempotent_replay_does_not_dispatch_colleague_twice(scratch_repo, colleague_bin, colleague_env, tmp_path):
    srv, cfg = _bridge(scratch_repo, colleague_bin, colleague_env, tmp_path)
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_replay_it"}
        body = _invocation_body(
            scratch_repo,
            instruction="write a short note about t26 (replay)",
            run_id="run_replay_it",
            attempt_id="att_replay_it",
            callback_url="http://127.0.0.1:1/unused",
            callback_token="unused",
        )
        status1, response1 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        status2, response2 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status1 == status2 == 200
        # Same task_id in the replayed claim record proves colleague ran once.
        assert response1["ledger_delta"]["records"][0]["data"]["colleague_task_id"] == \
            response2["ledger_delta"]["records"][0]["data"]["colleague_task_id"]
        assert response1 == response2
    finally:
        srv.shutdown()
        srv.server_close()


def test_async_invocation_202s_and_delivers_accepted_then_completed(scratch_repo, colleague_bin, colleague_env, tmp_path):
    srv, cfg = _bridge(scratch_repo, colleague_bin, colleague_env, tmp_path)
    receiver = FakeCallbackReceiver()
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_async_it"}
        body = _invocation_body(
            scratch_repo,
            instruction="write a short note about t26 (async)",
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

        # Every delivery carried the invocation's own callback token.
        assert all(tok == "Bearer async-cb-token" for tok in receiver.tokens)
    finally:
        receiver.close()
        srv.shutdown()
        srv.server_close()


def test_cancellation_writes_the_flight_control_file(scratch_repo, colleague_bin, colleague_env, tmp_path):
    srv, cfg = _bridge(scratch_repo, colleague_bin, colleague_env, tmp_path)
    receiver = FakeCallbackReceiver()
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_cancel_it"}
        body = _invocation_body(
            scratch_repo,
            instruction="write a short note about t26 (cancel)",
            run_id="run_cancel_it",
            attempt_id="att_cancel_it",
            callback_url=receiver.url,
            callback_token="cancel-cb-token",
            **{"async": True},
        )
        status, accepted = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
        assert status == 202, accepted
        invocation_id = accepted["invocation_id"]

        cancel_status, cancel_body = _request(
            base, f"/v1/invocations/{invocation_id}/cancel",
            body={"invocation_id": invocation_id, "reason": "integration test"},
            headers={"Authorization": f"Bearer {cfg.auth_token}"},
        )
        assert cancel_status == 202
        assert cancel_body["invocation_id"] == invocation_id

        control_path = flightfiles.control_path(scratch_repo, invocation_id)
        assert control_path.is_file(), f"expected a cooperative-stop control file at {control_path}"
        data = json.loads(control_path.read_text())
        assert data["stop"] is True

        # A terminal event still arrives either way — cancellation is
        # cooperative (PRD §13.6): the mock engine may already have
        # finished cleanly ("completed"), or the stop may have landed at a
        # turn boundary and produced an undeclared "incomplete" status,
        # which this bridge reports as a "failed" execution (incomplete is
        # never silently success). Either is a valid terminal outcome here;
        # what this test asserts is the control file above.
        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=15)
        assert terminal is not None, "expected SOME terminal callback event after cancellation"
    finally:
        receiver.close()
        srv.shutdown()
        srv.server_close()


def test_repo_outside_allowlist_is_refused_before_any_dispatch(scratch_repo, colleague_bin, colleague_env, tmp_path):
    srv, cfg = _bridge(scratch_repo, colleague_bin, colleague_env, tmp_path)
    try:
        host, port = srv.server_address
        base = f"http://{host}:{port}"
        headers = {"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_disallowed_it"}
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
