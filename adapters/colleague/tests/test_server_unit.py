"""Server-level unit tests: real HTTP over loopback, but `colleague_cli`'s
subprocess calls are monkeypatched out — these prove the request-handling
ladder (auth, Idempotency-Key, protocol_version, repo allowlist, role
validation, dispatch decision, idempotent replay) without needing a real
`colleague` binary. `test_integration_bridge.py` covers the real subprocess
path end to end."""

from __future__ import annotations

import json
import subprocess
import urllib.error
import urllib.request
from http.client import HTTPConnection

import pytest

from colleague_bridge import colleague_cli, server
from colleague_bridge.config import Config

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


def test_unknown_role_is_400(bridge_url):
    base, cfg, repo = bridge_url
    payload = _invocation_body(str(repo), role="not-a-real-role")
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_role"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 400
    assert "role" in body["error"]


def test_sync_dispatch_maps_ok_result_to_200(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        return colleague_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "task_id": "abc",
                "status": "ok",
                "summary": "did it",
                "changed_files": ["a.py"],
                "artifacts_path": "/x/.colleague/abc.json",
                "usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
                "error": None,
            },
            timed_out=False,
        )

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)

    headers = {**_auth_header(cfg), "Idempotency-Key": "att_sync_ok"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status == 200
    assert body["outcome"] == "completed"
    assert body["output"]["summary"] == "did it"
    # An exact-dict assertion on purpose: the wire shape is the contract, so a
    # field appearing or vanishing should fail here rather than pass silently.
    # `model` is the t15 addition — an explicit sentinel, so a reader of the
    # attempt can tell "this backend cannot report a model" apart from "nobody
    # wrote the field", which was indistinguishable while it was omitted (#77).
    assert body["usage"] == {
        "input_tokens": 1,
        "output_tokens": 2,
        "cost": None,
        "currency": None,
        "model": "unknown:colleague-backend-cannot-report",
    }
    assert body["ledger_delta"]["records"][0]["authority"] == "proposed"


def test_session_key_and_continuation_ref_never_appear_in_the_prompt_text(bridge_url, monkeypatch):
    """Acceptance: session_key never appears in the prompt text handed to
    the model. colleague never acts on continuation_ref (it always returns
    a null ref, issue #62), but a value it ignores must still be excluded
    from the Bound-inputs block, not merely unused."""
    base, cfg, repo = bridge_url
    captured = {}

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        captured["instruction"] = instruction
        return colleague_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "task_id": "abc",
                "status": "ok",
                "summary": "ok",
                "changed_files": [],
                "artifacts_path": None,
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                "error": None,
            },
            timed_out=False,
        )

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    payload = _invocation_body(
        str(repo),
        session_key="actor:repo:workstream-secret",
        continuation_ref="should-not-leak",
        fixReport={"summary": "a real bound input"},
    )
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_transport_keys"}
    status, _body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 200
    assert "actor:repo:workstream-secret" not in captured["instruction"]
    assert "should-not-leak" not in captured["instruction"]
    assert "a real bound input" in captured["instruction"]


def test_sync_capacity_exhausted_failure_is_500_with_retry_after_header(bridge_url, monkeypatch):
    """Acceptance: a quota/rate-limit CLI failure maps to capacity_exhausted
    (deviation d4), exercised through the real HTTP response including the
    Retry-After header internal/actors/client.go reads the delay from."""
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        return colleague_cli.SyncRunResult(
            exit_code=1,
            stdout="",
            stderr="",
            task_result={
                "task_id": "abc",
                "status": "error",
                "summary": "",
                "changed_files": [],
                "artifacts_path": None,
                "usage": {"prompt_tokens": 1, "completion_tokens": 0, "total_tokens": 1},
                "error": "rate_limit_error: retry after 45 seconds",
            },
            timed_out=False,
        )

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_capacity"}
    req = urllib.request.Request(
        base + server.INVOCATIONS_PATH,
        data=json.dumps(_invocation_body(str(repo))).encode("utf-8"),
        method="POST",
        headers=headers,
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:  # pragma: no cover - never 2xx here
            status, resp_headers = resp.status, resp.headers
            resp_body = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        status, resp_headers = exc.code, exc.headers
        resp_body = json.loads(exc.read().decode("utf-8"))
    assert status == 500
    assert resp_body["class"] == "capacity_exhausted"
    assert resp_headers.get("Retry-After") == "45"


def test_sync_dispatch_maps_incomplete_without_declaration_to_execution_failure(
    bridge_url, monkeypatch
):
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        return colleague_cli.SyncRunResult(
            exit_code=2,
            stdout="",
            stderr="",
            task_result={
                "task_id": "abc",
                "status": "incomplete",
                "summary": "partial",
                "changed_files": [],
                "usage": {},
            },
            timed_out=False,
        )

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_sync_incomplete"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status != 200
    assert body.get("outcome") != "completed"
    assert body["class"] == "execution"


def test_idempotent_replay_returns_the_same_response_without_recalling_colleague(
    bridge_url, monkeypatch
):
    base, cfg, repo = bridge_url
    calls = []

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        calls.append(instruction)
        return colleague_cli.SyncRunResult(
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

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_replay"}
    body = _invocation_body(str(repo))
    status1, resp1 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
    status2, resp2 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
    assert status1 == status2 == 200
    assert resp1 == resp2
    assert len(calls) == 1  # colleague was invoked exactly once


def test_validation_failure_is_not_cached_for_replay(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url
    payload = _invocation_body(str(repo))
    del payload["input"]["instruction"]
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_fix_and_retry"}

    status1, body1 = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status1 == 400

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        return colleague_cli.SyncRunResult(
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

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    payload["input"]["instruction"] = "say hello"  # the caller fixes their mistake
    status2, body2 = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status2 == 200
    assert body2["output"]["summary"] == "fixed"


def test_async_dispatch_returns_202_and_delivers_accepted_then_completed(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url
    receiver = FakeCallbackReceiver()
    try:

        def fake_spawn_background(
            cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None
        ):
            return colleague_cli.BackgroundStart(
                handle_id="bg123",
                pid=999999,
                log_dir=".colleague/background/bg123/",
                flight="bg123",
            )

        monkeypatch.setattr(colleague_cli, "spawn_background", fake_spawn_background)
        monkeypatch.setattr(colleague_cli, "is_pid_alive", lambda pid: False)
        monkeypatch.setattr(
            colleague_cli,
            "read_background_result",
            lambda repo_, handle_id: {
                "task_id": handle_id,
                "status": "ok",
                "summary": "async done",
                "changed_files": [],
                "usage": {},
            },
        )

        payload = _invocation_body(str(repo))
        payload["input"]["async"] = True
        payload["callback"] = {"url": receiver.url, "token": "cbtok"}
        headers = {**_auth_header(cfg), "Idempotency-Key": "att_async"}
        status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
        assert status == 202
        assert body["invocation_id"] == "bg123"
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


def test_async_dispatch_preserves_workspace_changes_on_a_real_failure(bridge_url, monkeypatch):
    """t25 (c26/h17, c41/h34): the async equivalent of the sync wiring test
    above — a "failed" terminal event carries the preserve outcome, never
    the "completed" one, and the uncommitted edit really lands on the
    minted branch."""
    base, cfg, repo = bridge_url
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    subprocess.run(["git", "config", "user.email", "t@t"], cwd=repo, check=True)
    subprocess.run(["git", "config", "user.name", "t"], cwd=repo, check=True)
    (repo / "README.md").write_text("# scratch\n")
    subprocess.run(["git", "add", "README.md"], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=repo, check=True)
    (repo / "note.txt").write_text("left behind by the failed async session\n")

    receiver = FakeCallbackReceiver()
    try:

        def fake_spawn_background(
            cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None
        ):
            return colleague_cli.BackgroundStart(
                handle_id="bg_preserve",
                pid=999999,
                log_dir=".colleague/background/bg_preserve/",
                flight="bg_preserve",
            )

        monkeypatch.setattr(colleague_cli, "spawn_background", fake_spawn_background)
        monkeypatch.setattr(colleague_cli, "is_pid_alive", lambda pid: False)
        monkeypatch.setattr(
            colleague_cli,
            "read_background_result",
            lambda repo_, handle_id: {
                "task_id": handle_id,
                "status": "error",
                "summary": "",
                "changed_files": [],
                "usage": {},
                "error": "fake async failure",
            },
        )

        payload = _invocation_body(str(repo))
        payload["input"]["async"] = True
        payload["callback"] = {"url": receiver.url, "token": "cbtok"}
        headers = {**_auth_header(cfg), "Idempotency-Key": "att_preserve_async"}
        status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
        assert status == 202, body

        failed = receiver.wait_for_kind("failed")
        assert failed is not None
        preserve_block = failed["payload"]["preserve"]
        assert preserve_block["attempted"] is True
        assert preserve_block["committed"] is True
        assert preserve_block["pushed"] is False
        assert preserve_block["local_only"] is True
        assert preserve_block["branch"]

        show = subprocess.run(
            ["git", "show", "--stat", preserve_block["commit"]],
            cwd=repo,
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        assert "note.txt" in show
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

    def fake_spawn_background(
        cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None
    ):
        raise colleague_cli.BackgroundDispatchError("boom", stderr="engine unreachable")

    monkeypatch.setattr(colleague_cli, "spawn_background", fake_spawn_background)
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


def test_sync_dispatch_preserves_workspace_changes_on_a_real_failure(bridge_url, monkeypatch):
    """t25 (c26/h17, c41/h34), wired end to end over the real HTTP surface:
    a genuine execution failure (never a domain outcome) gets the
    uncommitted edit left in the repo preserved on a code-minted branch,
    and the failure body records pushed False / local_only True since no
    remote is configured here."""
    base, cfg, repo = bridge_url
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    subprocess.run(["git", "config", "user.email", "t@t"], cwd=repo, check=True)
    subprocess.run(["git", "config", "user.name", "t"], cwd=repo, check=True)
    (repo / "README.md").write_text("# scratch\n")
    subprocess.run(["git", "add", "README.md"], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=repo, check=True)
    (repo / "note.txt").write_text("left behind by the failed session\n")

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        return colleague_cli.SyncRunResult(
            exit_code=1,
            stdout="",
            stderr="",
            task_result={
                "task_id": "abc",
                "status": "error",
                "summary": "",
                "changed_files": [],
                "artifacts_path": None,
                "usage": {"prompt_tokens": 1, "completion_tokens": 0, "total_tokens": 1},
                "error": "fake failure",
            },
            timed_out=False,
        )

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_preserve_sync"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status == 500, body
    preserve_block = body["preserve"]
    assert preserve_block["attempted"] is True
    assert preserve_block["committed"] is True
    assert preserve_block["pushed"] is False
    assert preserve_block["local_only"] is True
    assert preserve_block["branch"]
    assert preserve_block["commit"]

    show = subprocess.run(
        ["git", "show", "--stat", preserve_block["commit"]],
        cwd=repo,
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    assert "note.txt" in show


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
