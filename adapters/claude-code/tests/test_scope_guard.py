"""Issue #98: the workflow-scope boundary is enforced on what the session
CHANGED, not on what its brief SAID.

The two tests that matter are the two halves of the old guard's failure,
and they are deliberately written against a REAL git repository through the
real HTTP surface, because the bug was in the choice of input — asserting
on `scope_guard.violations()` alone would prove the matcher and miss the
question of what it is fed:

* `test_instruction_that_merely_names_a_guarded_path_is_accepted` — the
  brief names `.github/workflows/` (the way a careful operator naming a
  boundary does, and the way Culture Nodes run
  `01M039KA0QQ73XM3WQCQEQF1CN` did when it was refused 403 before any model
  ran). The session touches nothing guarded. It must be accepted.
* `test_session_that_modifies_a_guarded_path_is_refused` — the brief never
  mentions CI at all; the session writes `.github/workflows/go.yml`. Under
  the old prompt-side guard this sailed through. It must be refused.

Both fail on the parent commit, in opposite directions.

Mirrored by `adapters/codex/tests/test_scope_guard.py` and
`adapters/colleague/tests/test_scope_guard.py` (all-backends rule).
"""

from __future__ import annotations

import json
import subprocess
import urllib.error
import urllib.request
from pathlib import Path

import pytest

from claude_code_bridge import claude_cli, scope_guard, server
from claude_code_bridge.config import Config


def _request(base_url, path, *, method="POST", body=None, headers=None):
    url = base_url + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _git(repo: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["git", *args], cwd=repo, check=True, capture_output=True, text=True)


@pytest.fixture()
def git_bridge(tmp_path):
    """A bridge whose allowlisted repo is a REAL git working tree — the
    guard reads `workspace.measure()`, which reports nothing at all in a
    directory git cannot describe."""
    repo = tmp_path / "repo"
    repo.mkdir()
    _git(repo, "init", "-q")
    _git(repo, "config", "user.email", "scope-guard-test@example.com")
    _git(repo, "config", "user.name", "scope guard test")
    (repo / "README.md").write_text("# scratch\n", encoding="utf-8")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-q", "-m", "init")

    cfg = Config(
        repo_allowlist=(str(repo),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        sync_max_steps=6,
        default_max_steps=6,
        # The refusal path runs the preserve hook (a non-200 response
        # always does). Keep it local: a push would need a remote.
        preserve_push=False,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg, repo
    srv.shutdown()
    srv.server_close()


def _invocation_body(repo: Path, instruction: str):
    return {
        "protocol_version": "1.0",
        "run_id": "run_1",
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": "att_1",
        "attempt": 1,
        "workflow": {"name": "wf", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": {"instruction": instruction, "repo": str(repo)},
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": "http://127.0.0.1:1/callback", "token": "cbtok"},
    }


def _fake_session(writes: dict[str, str]):
    """A stand-in for the model: it writes exactly `writes` into the repo
    and reports success. What it writes is what the guard must judge."""

    def fake_run_sync(cfg_, instruction, repo_, *, role, max_steps, model, continuation_ref=None):
        for relative, content in writes.items():
            target = Path(repo_) / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(content, encoding="utf-8")
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

    return fake_run_sync


# ---------------------------------------------------------------------------
# the two acceptance halves
# ---------------------------------------------------------------------------


def test_instruction_that_merely_names_a_guarded_path_is_accepted(git_bridge, monkeypatch):
    base, cfg, repo = git_bridge
    monkeypatch.setattr(claude_cli, "run_sync", _fake_session({"notes.md": "no CI here\n"}))

    brief = (
        "Add a triage report script under scripts/.\n"
        "- Do NOT touch .github/workflows/**\n"
        "- CI configuration is out of scope for this package."
    )
    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(repo, brief),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_mention"},
    )
    assert status == 200, f"a brief that only NAMES the boundary must dispatch: {body}"
    assert body["outcome"] == "completed"
    assert "scope_violations" not in body


def test_session_that_modifies_a_guarded_path_is_refused(git_bridge, monkeypatch):
    base, cfg, repo = git_bridge
    monkeypatch.setattr(
        claude_cli,
        "run_sync",
        _fake_session({".github/workflows/go.yml": "name: go\n"}),
    )

    # Note what this brief does NOT say: nothing about CI, nothing about
    # workflows. The old prompt-side guard had nothing to match on, and this
    # dispatch was reported as a plain success.
    brief = "Make the Go tree build on ubuntu and add whatever gating is needed."
    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(repo, brief),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_touch"},
    )
    assert status == 403, f"a session that CHANGED a guarded path must be refused: {body}"
    assert body["class"] == "auth_or_policy"
    assert body["scope_violations"] == [".github/workflows/go.yml"]
    assert "workflow-scope boundary" in body["error"]
    # The refusal must not throw the work away: a non-200 runs the preserve
    # hook, so the change is on a branch a human can look at.
    assert body["preserve"]["attempted"] is True


# ---------------------------------------------------------------------------
# the matcher itself
# ---------------------------------------------------------------------------


def test_violations_are_empty_for_an_unmeasured_workspace(tmp_path):
    """The bridge refuses to invent a verdict it did not measure (this
    module's documented limit 2), rather than failing closed on every
    non-git repo."""
    assert scope_guard.violations(str(tmp_path), None) == ()
    unmeasured = {"measured": False, "changed_files": [".github/workflows/x.yml"]}
    assert scope_guard.violations(str(tmp_path), unmeasured) == ()


def test_guarded_matches_guarded_paths_and_ignores_lookalikes():
    assert scope_guard.guarded(
        [
            "docs/github/workflows/notes.md",  # not at the root
            ".githubbed/workflows/x.yml",  # prefix lookalike
            "./.github/workflows/go.yml",  # a leading ./ must not eat the dot
            ".github/dependabot.yml",  # inside .github, outside workflows/
            ".github/workflows/nested/deep.yml",
        ]
    ) == (".github/workflows/go.yml", ".github/workflows/nested/deep.yml")


def test_violations_see_a_guarded_file_git_status_collapsed_into_a_directory(git_bridge):
    """The measurement `measure()` hands over reports a brand-new
    `.github/workflows/go.yml` as the single collapsed entry `.github/` —
    which names no guarded path. Without the targeted probe the guard would
    read that as clean, which is precisely the under-inclusive half of #98
    all over again."""
    _base, _cfg, repo = git_bridge
    (repo / ".github" / "workflows").mkdir(parents=True)
    (repo / ".github" / "workflows" / "go.yml").write_text("name: go\n", encoding="utf-8")

    from claude_code_bridge import workspace

    measured = workspace.measure(workspace.begin(str(repo)))
    assert measured["changed_files"] == [".github/"], "fixture assumption: git collapses it"
    assert scope_guard.violations(str(repo), measured) == (".github/workflows/go.yml",)
