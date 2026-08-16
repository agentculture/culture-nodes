"""t9 / #90, the wire: a SUCCESSFUL dispatch that asked for a handover
actually creates the ref and reports it.

`preserve.handover_ref` shipped fully written and fully unit-tested in all
three bridges with NO CALLER ANYWHERE — a verification dispatch measured
exactly that (`grep -rn "handover_ref(" adapters/*/src/*/ --include=*.py |
grep -v preserve.py` returned nothing in every bridge), so no dispatch in any
backend had ever created a handover ref. `test_preserve.py` proves what the
function does when it is called; this file proves the bridge calls it.

The all-backends rule (CLAUDE.md) is why this file mirrors
`adapters/codex/tests/test_handover_wire.py` — but on the REF CREATION only.
The codex bridge additionally refuses `input.handover` without
`sandbox=workspace-write`, and this bridge deliberately carries no copy of
that refusal: it guards a codex-only `-c sandbox_workspace_write.
writable_roots` widening, and `colleague` takes no sandbox flag at all — its
session can already write `.git`. Copying the 400 here would refuse
dispatches this bridge serves perfectly well, and inventing a widening to
justify it would be inventing a mechanism that does not exist.

The asynchronous path is covered separately from the synchronous one because
production takes it (`always_async`), and this bridge reaches its terminal
event through a DETACHED session whose result it re-reads from a file in the
repo — a success proven on the synchronous path says nothing about the
file-mediated one.
"""

from __future__ import annotations

import json
import subprocess
import sys
import urllib.error
import urllib.request

import pytest
from colleague_bridge import colleague_cli, mapping, preserve, server, workspace
from colleague_bridge.async_runner import AsyncRunner
from colleague_bridge.config import Config

from ._fakes import FakeCallbackReceiver

#: A remote another host could really fetch from. `git remote add` contacts
#: nothing, so a url that is never dialled is still a faithful fixture — and
#: a handover deliberately does not push, so nothing here ever would.
SHARED_REMOTE = "https://github.com/agentculture/culture-nodes.git"


def _git(repo, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["git", *args], cwd=repo, check=True, capture_output=True, text=True)


def _repo_with_changes(repo, *, remote: str | None = SHARED_REMOTE):
    """A scratch repo with one commit, a fetchable remote, and uncommitted
    session work in it."""
    repo.mkdir(parents=True, exist_ok=True)
    _git(repo, "init", "-q")
    _git(repo, "config", "user.email", "t9@example.com")
    _git(repo, "config", "user.name", "t9")
    (repo / "README.md").write_text("# scratch\n", encoding="utf-8")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-q", "-m", "init")
    if remote is not None:
        _git(repo, "remote", "add", "origin", remote)
    (repo / "delivered.txt").write_text("the work being handed over\n", encoding="utf-8")
    return repo


def _handover_refs(repo) -> list[str]:
    out = _git(repo, "for-each-ref", "--format=%(refname)", preserve.HANDOVER_REF_NAMESPACE)
    return [line for line in out.stdout.splitlines() if line]


def _git_state(repo) -> tuple[str, bytes, str, list[str]]:
    """Everything a handover must not disturb, and the ref namespace it is
    the only writer of."""
    return (
        _git(repo, "rev-parse", "HEAD").stdout.strip(),
        (repo / ".git" / "index").read_bytes(),
        _git(repo, "status", "--porcelain").stdout,
        _handover_refs(repo),
    )


# ---------------------------------------------------------------------------
# the synchronous path, over the real HTTP surface
# ---------------------------------------------------------------------------


def _request(base_url, path, *, body=None, headers=None):
    req = urllib.request.Request(
        base_url + path, data=json.dumps(body).encode("utf-8"), method="POST", headers=headers or {}
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _invocation_body(repo, **input_overrides):
    payload = {"instruction": "say hello", "repo": str(repo)}
    payload.update(input_overrides)
    return {
        "protocol_version": "1.0",
        "run_id": "run_handover",
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": "att_1",
        "attempt": 1,
        "workflow": {"name": "wf", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": payload,
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": "http://127.0.0.1:1/callback", "token": "cbtok"},
    }


#: The `--json` task result a successful colleague session writes — the one
#: shape both terminal paths here classify as `completed`.
OK_RESULT = {
    "task_id": "task-handover",
    "status": "ok",
    "summary": "did it",
    "changed_files": ["delivered.txt"],
    "artifacts_path": None,
    "usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
    "error": None,
}


def _ok_result():
    return colleague_cli.SyncRunResult(
        exit_code=0, stdout="", stderr="", task_result=dict(OK_RESULT), timed_out=False
    )


@pytest.fixture()
def bridge(tmp_path):
    repo = _repo_with_changes(tmp_path / "repo")
    cfg = Config(
        repo_allowlist=(str(repo),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg, repo
    srv.shutdown()
    srv.server_close()


def test_a_successful_handover_dispatch_reports_a_ref(bridge, monkeypatch):
    """The acceptance bullet: ask for a handover, succeed, and get a ref
    back — one that really resolves in the repository the session worked."""
    base, cfg, repo = bridge

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        return _ok_result()

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(repo, handover=True, **{"async": False}),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_ho_sync"},
    )

    assert status == 200, body
    block = body["handover"]
    assert block["attempted"] is True
    assert block["created"] is True, block.get("reason")
    assert block["ref"].startswith(preserve.HANDOVER_REF_NAMESPACE + "/run_handover/")
    assert block["commit"]
    # Measured, not reported: the ref the body names resolves, in this
    # repository, to the commit the body names.
    resolved = _git(repo, "rev-parse", block["ref"]).stdout.strip()
    assert resolved == block["commit"]
    # And the commit really carries the session's work.
    listed = _git(repo, "show", "--name-only", "--format=", block["commit"]).stdout
    assert "delivered.txt" in listed
    # The handle a consuming node would read, with publication still pending
    # because handover_ref deliberately never pushes.
    assert block["handle"]["kind"] == "git_ref"
    assert block["handle"]["publication"] == "pending"
    assert block["handle"]["ref"].endswith("#" + block["ref"])


def test_a_dispatch_that_asked_for_no_handover_creates_no_ref(bridge, monkeypatch):
    """The opt-in half. A package that hands nothing over must leave the
    repository byte-for-byte as it found it — no ref, no commit, no index or
    HEAD movement — and say nothing about a handover on the wire."""
    base, cfg, repo = bridge

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        return _ok_result()

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    before = _git_state(repo)
    assert before[3] == []

    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(repo, **{"async": False}),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_ho_off"},
    )

    assert status == 200, body
    assert "handover" not in body
    assert _git_state(repo) == before


def test_a_handover_dispatch_needs_no_sandbox_widening_here(bridge, monkeypatch):
    """The deliberate asymmetry with the codex bridge, asserted rather than
    left to a comment: this bridge dispatches `colleague`, which takes no
    sandbox flag, so a handover request carries no sandbox precondition and
    is served exactly like any other dispatch. See the module docstring."""
    base, cfg, repo = bridge
    seen = {}

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None):
        seen["called"] = True
        return _ok_result()

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(repo, handover=True, sandbox="read-only", **{"async": False}),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_ho_nosbx"},
    )
    assert status == 200, body
    assert seen.get("called") is True
    assert body["handover"]["created"] is True


# ---------------------------------------------------------------------------
# the asynchronous path — the one production actually dispatches on
# ---------------------------------------------------------------------------


def _async_cfg(tmp_path, repo, **overrides):
    kwargs = dict(
        repo_allowlist=(str(repo),),
        state_dir=str(tmp_path / "state"),
        heartbeat_after_seconds=1,
        poll_interval_seconds=0.05,
        async_wait_seconds=30.0,
        callback_retry_backoff_seconds=0.05,
    )
    kwargs.update(overrides)
    return Config(**kwargs)


def _seed_result_with_success(repo, invocation_id: str) -> None:
    """Write the `--json` result a finished detached colleague session leaves
    in its background stdout log. The poller re-reads it through
    `colleague_cli.read_background_result`, so the terminal event this
    produces is built by production code from a real file, not injected past
    it."""
    path = colleague_cli.background_stdout_path(repo, invocation_id)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(OK_RESULT) + "\n", encoding="utf-8")


def _live_pid():
    """A real, live pid for the runner's liveness probe, plus its handle so
    the test can reap it. The session's own work is already on disk (the
    fixture wrote it), so this process only has to exist."""
    return subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])  # noqa: S603


#: Named once so the caller can seed the result file BEFORE it snapshots the
#: repository: colleague's background log lives inside the repo, so seeding it
#: is itself a workspace change and must not be mistaken for one the handover
#: made.
ASYNC_INVOCATION_ID = "colleague_handover_async"


def _run_async(cfg, repo, *, handover: bool):
    invocation_id = ASYNC_INVOCATION_ID
    proc = _live_pid()
    receiver = FakeCallbackReceiver()
    try:
        handle = workspace.begin(str(repo))
        assert handle.available, handle.reason
        AsyncRunner(cfg).start(
            start=colleague_cli.BackgroundStart(
                handle_id=invocation_id,
                pid=proc.pid,
                log_dir=str(colleague_cli.background_stdout_path(repo, invocation_id).parent),
                flight=None,
            ),
            repo=str(repo),
            ctx=mapping.InvocationContext(
                run_id="run_handover", node_run_id="nr_1", attempt_id="att_1"
            ),
            callback_url=receiver.url,
            callback_token="tok",
            heartbeat_after_seconds=cfg.heartbeat_after_seconds,
            workspace_handle=handle,
            handover=handover,
        )
        terminal = receiver.wait_for_any_kind(("completed", "failed"), timeout=30)
        assert terminal is not None
        return terminal
    finally:
        receiver.close()
        proc.kill()
        proc.wait(timeout=10)


def test_async_success_with_handover_reports_a_ref(tmp_path):
    repo = _repo_with_changes(tmp_path / "repo")
    cfg = _async_cfg(tmp_path, repo)
    _seed_result_with_success(repo, ASYNC_INVOCATION_ID)
    terminal = _run_async(cfg, repo, handover=True)
    assert terminal["kind"] == "completed", terminal["payload"]
    block = terminal["payload"]["handover"]
    assert block["created"] is True, block.get("reason")
    assert _git(repo, "rev-parse", block["ref"]).stdout.strip() == block["commit"]


def test_async_success_without_handover_creates_no_ref(tmp_path):
    repo = _repo_with_changes(tmp_path / "repo")
    cfg = _async_cfg(tmp_path, repo)
    # Seeded BEFORE the snapshot: the background log lives inside the repo,
    # so writing it is a workspace change of the harness's own making.
    _seed_result_with_success(repo, ASYNC_INVOCATION_ID)
    before = _git_state(repo)
    terminal = _run_async(cfg, repo, handover=False)
    assert terminal["kind"] == "completed", terminal["payload"]
    assert "handover" not in terminal["payload"]
    assert _git_state(repo) == before


# ---------------------------------------------------------------------------
# the honest-degradation path
# ---------------------------------------------------------------------------


def test_a_handover_that_cannot_name_a_remote_reports_the_missing_capability(tmp_path, monkeypatch):
    """A host with no fetchable remote cannot produce a portable handle. The
    dispatch still succeeds — the handover is a separate concern from the
    session's own outcome — and the response names the capability from the
    graph's closed enum rather than inventing prose."""
    repo = _repo_with_changes(tmp_path / "repo", remote=None)
    cfg = Config(
        repo_allowlist=(str(repo),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
    )
    srv, _thread = server.start_background(cfg)
    try:
        host, port = srv.server_address

        def fake_run_sync(
            cfg_, instruction, repo_, *, role, max_steps, mode, continuation_ref=None
        ):
            return _ok_result()

        monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)
        status, body = _request(
            f"http://{host}:{port}",
            server.INVOCATIONS_PATH,
            body=_invocation_body(repo, handover=True, **{"async": False}),
            headers={
                "Authorization": f"Bearer {cfg.auth_token}",
                "Idempotency-Key": "att_ho_noremote",
            },
        )
    finally:
        srv.shutdown()
        srv.server_close()

    assert status == 200, body
    block = body["handover"]
    assert block["attempted"] is True
    assert block["created"] is False
    assert block["missing_capability"] == preserve.MISSING_GIT_REF_PUBLISH
    # Refused before anything was written: no stray ref in the namespace.
    assert _handover_refs(repo) == []


def test_the_workspace_snapshot_is_unchanged_by_a_handover(tmp_path):
    """`handover_ref` is the only thing in the success path that touches
    git, and it must leave the live checkout alone — HEAD, index and status
    byte-identical, exactly as the preserve branch does on failure. Asserted
    directly against the function so the property is pinned at the wire's
    call site's own arguments, not only inside test_preserve.py."""
    repo = _repo_with_changes(tmp_path / "repo")
    handle = workspace.begin(str(repo))
    measured = workspace.measure(handle)
    before = _git_state(repo)

    result = preserve.handover_ref(
        str(repo),
        measured,
        enabled=True,
        remote=Config().handover_remote,
        run_id="run_handover",
        node_run_id="nr_1",
        attempt_id="att_1",
        reason=preserve.handover_success_reason("completed"),
    )

    assert result.created is True, result.reason
    after = _git_state(repo)
    assert after[:3] == before[:3]
    assert after[3] == [result.ref]
