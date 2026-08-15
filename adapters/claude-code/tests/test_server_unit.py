"""Server-level unit tests: real HTTP over loopback, but `claude_cli`'s
subprocess calls are monkeypatched out — these prove the request-handling
ladder (auth, Idempotency-Key, protocol_version, repo allowlist, role
validation, dispatch decision, idempotent replay, the version gate) without
needing a real (or even fake-subprocess) `claude` binary.
`test_integration_bridge.py` covers the real fake-subprocess path end to
end."""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from http.client import HTTPConnection

import pytest

from claude_code_bridge import claude_cli, flightfiles, server
from claude_code_bridge.config import Config

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


def test_workflow_scope_is_not_refused_from_the_instruction_text(bridge_url, monkeypatch):
    """Issue #98: this test used to assert the opposite — that naming
    `.github/workflows/` in the brief was refused 403 before dispatch. That
    guard read the operator's prose, so the safest possible instruction
    ("do not touch CI") was the one thing guaranteed to be rejected, while a
    session that edited CI silently passed. The boundary now lives in
    `scope_guard.py` and is decided on the measured change set; see
    `test_scope_guard.py` for both halves. Here the ladder must simply let
    the dispatch through."""
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        return claude_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "success",
                "is_error": False,
                "session_id": "sess-1",
                "result": "did it",
                "usage": {"input_tokens": 1, "output_tokens": 2},
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    payload = _invocation_body(
        str(repo), instruction="Update the go job, and do NOT touch .github/workflows/**"
    )
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_workflow_scope"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 200, body


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


def test_sync_dispatch_maps_success_result_to_200(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        return claude_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "success",
                "is_error": False,
                "session_id": "sess-1",
                "result": "did it",
                "total_cost_usd": 0.01,
                "usage": {"input_tokens": 1, "output_tokens": 2},
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)

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
        "cost": 0.01,
        "currency": "USD",
        "thread_id": "sess-1",
        "model": "unknown:claude-code-session-did-not-report",
    }
    assert body["ledger_delta"]["records"][0]["authority"] == "proposed"


def test_sync_dispatch_reads_top_level_continuation_ref_and_resumes(bridge_url, monkeypatch):
    """§13.1's continuation_ref (internal/actors/protocol.go's
    InvocationRequest) is a TOP-LEVEL request field, a sibling of run_id —
    NOT nested inside `input` (the seed this task inherited read it from
    the wrong place, which would have made a real engine's continuation_ref
    silently dispatch cold every time). This test pins the real wire shape
    end to end: a top-level continuation_ref reaches claude_cli.run_sync."""
    base, cfg, repo = bridge_url
    captured = {}

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        captured["continuation_ref"] = continuation_ref
        return claude_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "success",
                "is_error": False,
                "session_id": "sess-1",
                "result": "resumed",
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    payload = _invocation_body(str(repo))
    payload["continuation_ref"] = "sess-prior-999"  # top-level, per §13.1
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_resume"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 200
    assert captured["continuation_ref"] == "sess-prior-999"


def test_sync_dispatch_without_a_prior_ref_dispatches_cold(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url
    captured = {}

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        captured["continuation_ref"] = continuation_ref
        return claude_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "success",
                "is_error": False,
                "session_id": "sess-1",
                "result": "cold",
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_cold"}
    status, _body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status == 200
    assert captured["continuation_ref"] is None


def test_session_key_and_continuation_ref_never_appear_in_the_prompt_text(bridge_url, monkeypatch):
    """Acceptance: 'session_key never appears in the prompt text handed to
    the model.' Both session_key and continuation_ref are transport keys —
    even when a caller nests them inside `input` (which the real wire shape
    never does for continuation_ref, but this bridge's own extras-forwarding
    block is defensive regardless — see server.py's own comment), neither
    may leak into the instruction text claude actually receives."""
    base, cfg, repo = bridge_url
    captured = {}

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        captured["instruction"] = instruction
        return claude_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "success",
                "is_error": False,
                "session_id": "sess-1",
                "result": "ok",
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    payload = _invocation_body(
        str(repo),
        session_key="actor:repo:workstream-secret",
        continuation_ref="sess-should-not-leak",
        fixReport={"summary": "a real bound input"},
    )
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_transport_keys"}
    status, _body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 200
    assert "actor:repo:workstream-secret" not in captured["instruction"]
    assert "sess-should-not-leak" not in captured["instruction"]
    # The negative control: a genuine bound input still DOES get forwarded,
    # proving the exclusion is specific to the transport keys, not a bug
    # that dropped the whole Bound-inputs block.
    assert "a real bound input" in captured["instruction"]


def test_sync_dispatch_maps_incomplete_without_declaration_to_execution_failure(
    bridge_url, monkeypatch
):
    """incomplete-never-success, exercised through the HTTP surface."""
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        return claude_cli.SyncRunResult(
            exit_code=1,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "error_max_turns",
                "is_error": True,
                "session_id": "s",
                "result": "partial",
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_sync_incomplete"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status != 200
    assert body.get("outcome") != "completed"
    assert body["class"] == "execution"


def test_sync_dispatch_maps_crashed_session_to_execution_failure_never_success(
    bridge_url, monkeypatch
):
    """The other half of incomplete-never-success: a crashed session (no
    parseable task_result at all) must never become a 200."""
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        return claude_cli.SyncRunResult(
            exit_code=1,
            stdout="garbage, not json",
            stderr="fatal",
            task_result=None,
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_sync_crash"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status != 200
    assert body.get("outcome") != "completed"
    assert body["class"] == "execution"


def test_idempotent_replay_returns_the_same_response_without_recalling_claude(
    bridge_url, monkeypatch
):
    base, cfg, repo = bridge_url
    calls = []

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        calls.append(instruction)
        return claude_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "success",
                "is_error": False,
                "session_id": "s",
                "result": "did it",
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_replay"}
    body = _invocation_body(str(repo))
    status1, resp1 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
    status2, resp2 = _request(base, server.INVOCATIONS_PATH, body=body, headers=headers)
    assert status1 == status2 == 200
    assert resp1 == resp2
    assert len(calls) == 1  # claude was invoked exactly once


def test_validation_failure_is_not_cached_for_replay(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url
    payload = _invocation_body(str(repo))
    del payload["input"]["instruction"]
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_fix_and_retry"}

    status1, body1 = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status1 == 400

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        return claude_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "success",
                "is_error": False,
                "session_id": "s",
                "result": "fixed",
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    payload["input"]["instruction"] = "say hello"  # the caller fixes their mistake
    status2, body2 = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status2 == 200
    assert body2["output"]["summary"] == "fixed"


def test_async_dispatch_returns_202_and_delivers_accepted_then_completed(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url
    receiver = FakeCallbackReceiver()
    try:
        handle_id = "cc_bg123"

        def fake_spawn_background(
            cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None
        ):
            # The async runner's poller tails flightfiles.feed_path(state_dir,
            # handle_id) directly (see async_runner.py) — write the terminal
            # `type: "result"` record there up front so the fake behaves
            # exactly like a real detached claude session that already
            # finished by the time the poller gets its first turn.
            feed = flightfiles.feed_path(cfg_.state_dir, handle_id)
            feed.parent.mkdir(parents=True, exist_ok=True)
            feed.write_text(
                json.dumps(
                    {
                        "type": "result",
                        "subtype": "success",
                        "is_error": False,
                        "session_id": handle_id,
                        "result": "async done",
                    }
                )
                + "\n"
            )
            return claude_cli.BackgroundStart(handle_id=handle_id, pid=999999, log_path=str(feed))

        monkeypatch.setattr(claude_cli, "spawn_background", fake_spawn_background)
        monkeypatch.setattr(claude_cli, "is_pid_alive", lambda pid: False)

        payload = _invocation_body(str(repo))
        payload["input"]["async"] = True
        payload["callback"] = {"url": receiver.url, "token": "cbtok"}
        headers = {**_auth_header(cfg), "Idempotency-Key": "att_async"}
        status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
        assert status == 202
        assert body["invocation_id"] == "cc_bg123"
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
        # Acceptance: "the async terminal payload carries continuation_ref"
        # — the seed only ever wired this onto the SYNC body.
        assert completed["payload"]["continuation_ref"] == handle_id
    finally:
        receiver.close()


def test_async_dispatch_passes_continuation_ref_through_to_spawn_background(
    bridge_url, monkeypatch
):
    """The async resume half of acceptance #1: a prior ref must reach
    `spawn_background` too, not just `run_sync` — the async path is the one
    long (and therefore resume-worth-it) sessions actually take."""
    base, cfg, repo = bridge_url
    receiver = FakeCallbackReceiver()
    captured = {}
    try:
        handle_id = "cc_bg_resume"

        def fake_spawn_background(
            cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None
        ):
            captured["continuation_ref"] = continuation_ref
            feed = flightfiles.feed_path(cfg_.state_dir, handle_id)
            feed.parent.mkdir(parents=True, exist_ok=True)
            feed.write_text(
                json.dumps(
                    {
                        "type": "result",
                        "subtype": "success",
                        "is_error": False,
                        "session_id": handle_id,
                        "result": "resumed async",
                    }
                )
                + "\n"
            )
            return claude_cli.BackgroundStart(handle_id=handle_id, pid=999999, log_path=str(feed))

        monkeypatch.setattr(claude_cli, "spawn_background", fake_spawn_background)
        monkeypatch.setattr(claude_cli, "is_pid_alive", lambda pid: False)

        payload = _invocation_body(str(repo))
        payload["input"]["async"] = True
        payload["continuation_ref"] = "sess-prior-async-1"
        payload["callback"] = {"url": receiver.url, "token": "cbtok"}
        headers = {**_auth_header(cfg), "Idempotency-Key": "att_async_resume"}
        status, _body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
        assert status == 202
        receiver.wait_for_kind("completed")
        assert captured["continuation_ref"] == "sess-prior-async-1"
    finally:
        receiver.close()


def test_sync_capacity_exhausted_failure_is_500_with_retry_after_header(bridge_url, monkeypatch):
    """Acceptance: a quota/rate-limit CLI failure maps to capacity_exhausted
    (deviation d4), and — since internal/actors/client.go reads the delay
    from the HTTP Retry-After header, never the JSON body — that header is
    actually set on the wire, exercised through the real HTTP response
    rather than only at the mapping layer."""
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        return claude_cli.SyncRunResult(
            exit_code=1,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "error_during_execution",
                "is_error": True,
                "session_id": "sess-cap",
                "result": "rate_limit_error: retry after 45 seconds",
            },
            timed_out=False,
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
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
        cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None
    ):
        raise claude_cli.BackgroundDispatchError("boom", stderr="engine unreachable")

    monkeypatch.setattr(claude_cli, "spawn_background", fake_spawn_background)
    payload = _invocation_body(str(repo))
    payload["input"]["async"] = True
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_async_fail"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 503
    assert body["class"] == "actor_unavailable"


def test_sync_dispatch_below_pinned_minimum_is_refused_with_honest_error(bridge_url, monkeypatch):
    """Acceptance criterion #3, exercised through the HTTP surface: a
    version-gate refusal is an actor_unavailable 503 naming both versions."""
    base, cfg, repo = bridge_url

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        raise claude_cli.UnsupportedClaudeVersionError(
            detected="2.0.5 (Claude Code)", minimum="2.1.220"
        )

    monkeypatch.setattr(claude_cli, "run_sync", fake_run_sync)
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_version_gate"}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(str(repo)), headers=headers
    )
    assert status == 503
    assert body["class"] == "actor_unavailable"
    assert "2.0.5" in body["error"]
    assert "2.1.220" in body["error"]


def test_async_dispatch_below_pinned_minimum_is_refused_with_honest_error(bridge_url, monkeypatch):
    base, cfg, repo = bridge_url

    def fake_spawn_background(
        cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None
    ):
        raise claude_cli.UnsupportedClaudeVersionError(
            detected="2.0.5 (Claude Code)", minimum="2.1.220"
        )

    monkeypatch.setattr(claude_cli, "spawn_background", fake_spawn_background)
    payload = _invocation_body(str(repo))
    payload["input"]["async"] = True
    headers = {**_auth_header(cfg), "Idempotency-Key": "att_async_version_gate"}
    status, body = _request(base, server.INVOCATIONS_PATH, body=payload, headers=headers)
    assert status == 503
    assert body["class"] == "actor_unavailable"
    assert "2.0.5" in body["error"]
    assert "2.1.220" in body["error"]


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
