"""`preserve.py`'s own unit tests (task t25, issue #49). Every assertion
here proves the plumbing sequence (write-tree -> commit-tree -> update-ref)
never disturbs the live checkout's HEAD, index, or working tree — the
acceptance bullet most likely to be faked by a test that merely doesn't
check (see `test_preserve_commit_leaves_head_index_and_status_byte_identical`).
"""

from __future__ import annotations

import subprocess

from claude_code_bridge import preserve, workspace


def _git(repo, *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["git", *args], cwd=repo, check=True, capture_output=True, text=True)


def _git_output(repo, *args: str) -> str:
    return _git(repo, *args).stdout


def _init_repo(repo) -> None:
    repo.mkdir(parents=True, exist_ok=True)
    _git(repo, "init", "-q")
    _git(repo, "config", "user.email", "preserve-test@example.com")
    _git(repo, "config", "user.name", "preserve test")
    (repo / "README.md").write_text("# scratch\n")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-q", "-m", "init")


def _head(repo) -> str:
    return _git_output(repo, "rev-parse", "HEAD").strip()


def _index_bytes(repo) -> bytes:
    return (repo / ".git" / "index").read_bytes()


def _measure_after_change(repo):
    handle = workspace.begin(str(repo))
    return handle, workspace.measure(handle)


def _preserve(repo, measured, **overrides):
    kwargs = dict(
        enabled=True,
        push=False,
        remote="origin",
        branch_prefix="preserve/",
        run_id="run_1",
        node_run_id="nr_1",
        attempt_id="att_1",
        reason="claude reported subtype=error_during_execution is_error=True",
    )
    kwargs.update(overrides)
    return preserve.preserve_on_failure(str(repo), measured, **kwargs)


# ---------------------------------------------------------------------------
# the load-bearing acceptance bullet: byte-identical HEAD/index/status
# ---------------------------------------------------------------------------


def test_preserve_commit_leaves_head_index_and_status_byte_identical(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))
    (repo / "README.md").write_text("# scratch\n\nedited by the failed session\n")
    (repo / "untracked.txt").write_text("left behind\n")
    measured = workspace.measure(handle)

    pre_status = _git_output(repo, "status", "--porcelain")
    pre_head = _head(repo)
    pre_index = _index_bytes(repo)

    result = _preserve(repo, measured)
    assert result.attempted is True
    assert result.committed is True
    assert result.branch is not None
    assert result.commit is not None

    post_status = _git_output(repo, "status", "--porcelain")
    post_head = _head(repo)
    post_index = _index_bytes(repo)

    assert post_status == pre_status
    assert post_head == pre_head
    assert post_index == pre_index
    # HEAD did not move to the new branch, and the current branch is
    # unchanged — update-ref only ever wrote the NEW preserve ref.
    assert _git_output(repo, "rev-parse", "--abbrev-ref", "HEAD").strip() != result.branch


def test_preserve_branch_carries_the_edits_head_did_not_move_to(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))
    (repo / "README.md").write_text("# scratch\n\nedited\n")
    (repo / "new_note.txt").write_text("agent wrote this\n")
    measured = workspace.measure(handle)

    result = _preserve(repo, measured)
    assert result.committed is True

    show = _git_output(repo, "show", "--stat", result.commit)
    assert "README.md" in show
    assert "new_note.txt" in show
    # The live checkout's own working copy still has the uncommitted edits —
    # preserve committed a SNAPSHOT, it never touched the working tree.
    assert (repo / "README.md").read_text() == "# scratch\n\nedited\n"


# ---------------------------------------------------------------------------
# the failure reason rides the commit message
# ---------------------------------------------------------------------------


def test_preserve_commit_message_carries_the_failure_reason(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))
    (repo / "README.md").write_text("# scratch\n\nedited\n")
    measured = workspace.measure(handle)

    reason = "claude reported a provider capacity refusal: 'rate_limit_error'"
    result = _preserve(repo, measured, reason=reason)
    assert result.committed is True

    message = _git_output(repo, "log", "-1", "--format=%B", result.commit)
    assert reason in message
    assert "run_1" in message
    assert "nr_1" in message
    assert "att_1" in message


# ---------------------------------------------------------------------------
# push best-effort: failure leaves the local commit intact, records local-only
# ---------------------------------------------------------------------------


def test_failed_push_leaves_local_commit_intact_and_records_local_only(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))
    (repo / "README.md").write_text("# scratch\n\nedited\n")
    measured = workspace.measure(handle)

    # No remote named "origin" exists at all — the most honest way to
    # reproduce "bridge-host push credentials are unverified" without
    # mocking git itself.
    result = _preserve(repo, measured, push=True, remote="origin")

    assert result.committed is True
    assert result.pushed is False
    assert result.local_only is True
    assert result.reason is not None and "origin" in result.reason

    # The local commit and its ref still exist untouched.
    assert _git_output(repo, "cat-file", "-e", result.commit) == ""
    assert _git_output(repo, "show-ref", "--verify", f"refs/heads/{result.branch}")


def test_successful_push_records_pushed_not_local_only(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    remote = tmp_path / "remote.git"
    _git(repo, "init", "-q", "--bare", str(remote))
    _git(repo, "remote", "add", "origin", str(remote))

    handle = workspace.begin(str(repo))
    (repo / "README.md").write_text("# scratch\n\nedited\n")
    measured = workspace.measure(handle)

    result = _preserve(repo, measured, push=True, remote="origin")

    assert result.committed is True
    assert result.pushed is True
    assert result.local_only is False

    # The branch really is on the remote now.
    remote_refs = _git_output(remote, "show-ref")
    assert result.branch in remote_refs


# ---------------------------------------------------------------------------
# honest skips — never a fabricated commit
# ---------------------------------------------------------------------------


def test_preserve_disabled_by_configuration_is_never_attempted(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))
    (repo / "README.md").write_text("# scratch\n\nedited\n")
    measured = workspace.measure(handle)

    result = _preserve(repo, measured, enabled=False)
    assert result.attempted is False
    assert result.committed is False
    assert result.reason is not None and "disabled" in result.reason


def test_preserve_skips_when_workspace_was_not_measurable(tmp_path):
    repo = tmp_path / "not-a-repo"
    repo.mkdir()
    handle = workspace.begin(str(repo))
    measured = workspace.measure(handle)

    result = _preserve(repo, measured)
    assert result.attempted is False
    assert result.reason is not None and "not measurable" in result.reason


def test_preserve_skips_when_there_are_no_changes(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle, measured = _measure_after_change(repo)

    result = _preserve(repo, measured)
    assert result.attempted is False
    assert result.reason is not None and "no workspace changes" in result.reason


# ---------------------------------------------------------------------------
# branch naming
# ---------------------------------------------------------------------------


def test_mint_branch_name_is_unique_across_calls_for_the_same_ids():
    a = preserve.mint_branch_name("preserve/", "run_1", "nr_1", "att_1")
    b = preserve.mint_branch_name("preserve/", "run_1", "nr_1", "att_1")
    assert a != b
    assert a.startswith("preserve/run_1-nr_1-att_1-")
    assert b.startswith("preserve/run_1-nr_1-att_1-")


def test_mint_branch_name_sanitizes_unsafe_characters():
    name = preserve.mint_branch_name("preserve/", "run one/two", None, None)
    assert " " not in name
    assert name.startswith("preserve/run-one-two-")
