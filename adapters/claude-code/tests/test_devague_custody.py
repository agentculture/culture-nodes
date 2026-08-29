"""`.devague/` custody at the dispatch boundary (task t13, issue #199 / #230;
frame decision q1 of jira-flow-spec-read-related-bugs).

A devague frame lives in a checkout's `.devague/` and is committed on a
branch. The operator's own checkout is one lane; a bridge lane writing
frames beside it unannounced is the concurrent-writer mode (c42) applied to
the spec itself. So a lane that may write `.devague/` DECLARES it in its own
config (`Custody`), and a dispatch that intends to write `.devague/` says so
(`input.devague_write: true`). This file pins the four answers the bridge
gives that request, through the real HTTP surface:

* no declaration on this lane            -> 403 auth_or_policy
* a declaration that grants nothing      -> 403 auth_or_policy
* the grant, but a different checkout    -> 403 auth_or_policy
* the grant, in the declared checkout    -> dispatched, and the session's
  brief carries the declaration verbatim (checkout + branch prefix), so the
  branch namespace is the lane's, not the model's guess.

A dispatch that does not ask (`devague_write` absent) is untouched by any of
this — custody is opt-in per dispatch, like `handover`.
"""

from __future__ import annotations

import json
import subprocess
import urllib.error
import urllib.request
from pathlib import Path

import pytest

from claude_code_bridge import claude_cli, server
from claude_code_bridge.config import Config, Custody


def _request(base_url, path, *, body, headers):
    req = urllib.request.Request(
        base_url + path, data=json.dumps(body).encode("utf-8"), method="POST", headers=headers
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _git_repo(path: Path) -> Path:
    path.mkdir()
    for args in (
        ("init", "-q"),
        ("config", "user.email", "custody-test@example.com"),
        ("config", "user.name", "custody test"),
    ):
        subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
    (path / "README.md").write_text("# lane\n", encoding="utf-8")
    subprocess.run(["git", "add", "README.md"], cwd=path, check=True, capture_output=True)
    subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=path, check=True, capture_output=True)
    return path


@pytest.fixture()
def lanes(tmp_path):
    return _git_repo(tmp_path / "owe-developer"), _git_repo(tmp_path / "other-lane")


def _bridge(tmp_path, lanes, custody: Custody | None):
    cfg = Config(
        repo_allowlist=tuple(str(lane) for lane in lanes),
        custody=custody,
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        sync_max_steps=6,
        default_max_steps=6,
        preserve_push=False,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    return srv, f"http://{host}:{port}"


def _body(repo: Path, *, devague_write):
    input_ = {"instruction": "run the devague moves for SCRUM-9", "repo": str(repo)}
    if devague_write is not None:
        input_["devague_write"] = devague_write
    return {
        "protocol_version": "1.0",
        "run_id": "run_1",
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": "att_1",
        "attempt": 1,
        "workflow": {"name": "spec-chain-lane", "version_digest": "sha256:0"},
        "node": {"id": "think", "contract_digest": "sha256:1"},
        "input": input_,
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": "http://127.0.0.1:1/callback", "token": "cbtok"},
    }


HEADERS = {
    "Authorization": "Bearer s3cr3t",
    "Content-Type": "application/json",
    "Idempotency-Key": "idem-1",
}


def _capturing_session(seen: dict):
    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        seen["instruction"] = instruction
        seen["repo"] = repo_
        return claude_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "type": "result",
                "subtype": "success",
                "is_error": False,
                "session_id": "sess-1",
                "result": "frame posted",
                "total_cost_usd": 0.01,
                "usage": {"input_tokens": 1, "output_tokens": 2},
            },
            timed_out=False,
        )

    return fake_run_sync


def _dispatch(tmp_path, lanes, custody, repo, *, devague_write, monkeypatch):
    seen: dict = {}
    monkeypatch.setattr(claude_cli, "run_sync", _capturing_session(seen))
    srv, base = _bridge(tmp_path, lanes, custody)
    try:
        status, body = _request(
            base, "/v1/invocations", body=_body(repo, devague_write=devague_write), headers=HEADERS
        )
    finally:
        srv.shutdown()
        srv.server_close()
    return status, body, seen


def test_undeclared_lane_refuses_a_devague_write(tmp_path, lanes, monkeypatch):
    developer, _ = lanes
    status, body, seen = _dispatch(
        tmp_path, lanes, None, developer, devague_write=True, monkeypatch=monkeypatch
    )
    assert status == 403, body
    assert body["class"] == "auth_or_policy"
    assert "declares no .devague/ custody" in body["error"]
    assert seen == {}, "a refused custody request must never reach a session"


def test_declaration_without_the_grant_refuses(tmp_path, lanes, monkeypatch):
    developer, _ = lanes
    custody = Custody(checkout=str(developer.resolve()), branch_prefix="jira-flow/")
    status, body, seen = _dispatch(
        tmp_path, lanes, custody, developer, devague_write=True, monkeypatch=monkeypatch
    )
    assert status == 403, body
    assert "does not grant devague_write" in body["error"]
    assert seen == {}


def test_grant_is_checkout_bound(tmp_path, lanes, monkeypatch):
    developer, other = lanes
    custody = Custody(
        checkout=str(developer.resolve()), branch_prefix="jira-flow/", devague_write=True
    )
    status, body, seen = _dispatch(
        tmp_path, lanes, custody, other, devague_write=True, monkeypatch=monkeypatch
    )
    assert status == 403, body
    assert str(developer.resolve()) in body["error"]
    assert seen == {}


def test_granted_dispatch_carries_the_declaration_in_the_brief(tmp_path, lanes, monkeypatch):
    developer, _ = lanes
    custody = Custody(
        checkout=str(developer.resolve()), branch_prefix="jira-flow/", devague_write=True
    )
    status, body, seen = _dispatch(
        tmp_path, lanes, custody, developer, devague_write=True, monkeypatch=monkeypatch
    )
    assert status == 200, body
    assert seen["repo"] == str(developer.resolve())
    brief = seen["instruction"]
    assert brief.startswith("run the devague moves for SCRUM-9")
    assert "## Lane custody (bridge-declared, not negotiable)" in brief
    assert f"- checkout: {developer.resolve()}" in brief
    assert "- branch_prefix: jira-flow/" in brief
    # The request flag is a transport field, not a bound input the model
    # gets to read as prose.
    assert "Bound inputs" not in brief


def test_dispatch_that_does_not_ask_is_untouched(tmp_path, lanes, monkeypatch):
    developer, _ = lanes
    status, body, seen = _dispatch(
        tmp_path, lanes, None, developer, devague_write=None, monkeypatch=monkeypatch
    )
    assert status == 200, body
    assert "Lane custody" not in seen["instruction"]


def test_non_boolean_devague_write_is_a_400(tmp_path, lanes, monkeypatch):
    developer, _ = lanes
    status, body, seen = _dispatch(
        tmp_path, lanes, None, developer, devague_write="yes", monkeypatch=monkeypatch
    )
    assert status == 400, body
    assert "devague_write must be a boolean" in body["error"]
    assert seen == {}
