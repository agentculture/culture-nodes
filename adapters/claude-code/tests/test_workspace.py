"""`workspace.py`'s own unit tests (task t10, acceptance criterion #1): every
assertion here proves a field came from THIS PROCESS running `git` against a
real temp git repo fixture — never from a model/task-result literal, which
none of these tests ever construct.
"""

from __future__ import annotations

import subprocess

import pytest

from claude_code_bridge import workspace


def _git(repo, *args: str) -> None:
    subprocess.run(["git", *args], cwd=repo, check=True, capture_output=True, text=True)


def _init_repo(repo) -> None:
    repo.mkdir(parents=True, exist_ok=True)
    _git(repo, "init", "-q")
    _git(repo, "config", "user.email", "workspace-test@example.com")
    _git(repo, "config", "user.name", "workspace test")
    (repo / "README.md").write_text("# scratch\n")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-q", "-m", "init")


def _head(repo) -> str:
    proc = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=repo, check=True, capture_output=True, text=True
    )
    return proc.stdout.strip()


def test_provision_mints_a_sibling_worktree_and_refuses_reuse(tmp_path):
    repo = tmp_path / "culture-nodes"
    _init_repo(repo)
    root = tmp_path / ".worktrees.culture-nodes"

    minted = workspace.provision(str(repo), str(root), "writer-t16", forbidden_roots=(str(repo),))

    assert minted == str(root / "writer-t16")
    assert (root / "writer-t16" / "README.md").is_file()
    with pytest.raises(workspace.WorkspaceProvisionError, match="already exists"):
        workspace.provision(str(repo), str(root), "writer-t16", forbidden_roots=(str(repo),))


def test_provision_refuses_a_worktree_nested_in_another_writer_allowlisted_root(tmp_path):
    repo = tmp_path / "culture-nodes"
    _init_repo(repo)
    nested_root = repo / ".claude" / "worktrees"

    with pytest.raises(workspace.WorkspaceProvisionError, match="reachable"):
        workspace.provision(
            str(repo),
            str(nested_root),
            "web-ux-quick-wins",
            forbidden_roots=(str(repo),),
        )


@pytest.mark.parametrize("name", ["", ".", "../escape", "a/b", "a\\b"])
def test_provision_refuses_non_local_writer_names(tmp_path, name):
    repo = tmp_path / "culture-nodes"
    _init_repo(repo)

    with pytest.raises(workspace.WorkspaceProvisionError):
        workspace.provision(
            str(repo), str(tmp_path / "worktrees"), name, forbidden_roots=()
        )


# ---------------------------------------------------------------------------
# begin() / measure() over a real git repo — the process-measured guarantee
# ---------------------------------------------------------------------------


def test_begin_captures_the_real_head_and_measure_reports_no_changes_on_a_clean_repo(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    real_head = _head(repo)

    handle = workspace.begin(str(repo))
    assert handle.available is True
    assert handle.head_before == real_head  # measured, not fabricated

    measured = workspace.measure(handle)
    assert measured["measured"] is True
    assert measured["reason"] is None
    assert measured["repo"] == str(repo)
    assert measured["head_before"] == real_head
    assert measured["head_after"] == real_head
    assert measured["status_porcelain"] == ""
    assert measured["changed_files"] == []
    assert measured["diffstat"] == ""


def test_measure_reports_the_current_branch(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))
    measured = workspace.measure(handle)
    # A fresh `git init` defaults to "main" or "master" depending on the
    # host's git config — assert only that SOME real branch name came back,
    # not a specific one this test would have to guess.
    expected = subprocess.run(
        ["git", "rev-parse", "--abbrev-ref", "HEAD"],
        cwd=repo,
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    assert measured["branch"] == expected


def test_measure_detects_a_new_untracked_file_created_after_begin(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))

    (repo / "new_note.txt").write_text("agent wrote this\n")

    measured = workspace.measure(handle)
    assert measured["measured"] is True
    assert "new_note.txt" in measured["changed_files"]
    assert "?? new_note.txt" in measured["status_porcelain"]
    # HEAD did not move — this is an uncommitted, untracked change.
    assert measured["head_after"] == measured["head_before"]


def test_measure_detects_a_modified_tracked_file_and_produces_a_diffstat(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))

    (repo / "README.md").write_text("# scratch\n\nedited by the agent\n")

    measured = workspace.measure(handle)
    assert "README.md" in measured["changed_files"]
    assert measured["diffstat"]  # non-empty: git diff --stat found a change
    assert "README.md" in measured["diffstat"]
    assert " M README.md" in measured["status_porcelain"]


def test_measure_reflects_a_new_commit_made_during_the_session(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))

    (repo / "committed.txt").write_text("this got committed\n")
    _git(repo, "add", "committed.txt")
    _git(repo, "commit", "-q", "-m", "agent commit")
    new_head = _head(repo)

    measured = workspace.measure(handle)
    assert measured["head_before"] != new_head
    assert measured["head_after"] == new_head
    # git diff --name-only against head_before still finds the committed
    # file even though the working tree is clean relative to the new HEAD.
    assert "committed.txt" in measured["changed_files"]
    assert measured["status_porcelain"] == ""


def test_changed_files_union_of_committed_and_untracked_has_no_duplicates(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))

    (repo / "README.md").write_text("# scratch\n\nedited\n")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-q", "-m", "edit readme")
    (repo / "README.md").write_text("# scratch\n\nedited again, uncommitted\n")

    measured = workspace.measure(handle)
    assert measured["changed_files"].count("README.md") == 1


# ---------------------------------------------------------------------------
# honest degradation — never fabricated
# ---------------------------------------------------------------------------


def test_begin_degrades_honestly_when_repo_is_not_a_git_working_tree(tmp_path):
    repo = tmp_path / "not-a-repo"
    repo.mkdir()

    handle = workspace.begin(str(repo))
    assert handle.available is False
    assert handle.head_before is None
    assert "not a git working tree" in handle.reason

    measured = workspace.measure(handle)
    assert measured["measured"] is False
    assert measured["reason"] == handle.reason
    assert measured["head_before"] is None
    assert measured["head_after"] is None
    assert measured["status_porcelain"] is None
    assert measured["changed_files"] == []
    assert measured["diffstat"] is None


def test_begin_degrades_honestly_when_git_is_not_installed(tmp_path, monkeypatch):
    monkeypatch.setattr(workspace.shutil, "which", lambda _name: None)
    repo = tmp_path / "repo"
    repo.mkdir()

    handle = workspace.begin(str(repo))
    assert handle.available is False
    assert "git is not installed" in handle.reason

    measured = workspace.measure(handle)
    assert measured["measured"] is False
    assert measured["reason"] == handle.reason


def test_begin_degrades_honestly_on_an_unborn_head(tmp_path):
    repo = tmp_path / "repo"
    repo.mkdir()
    _git(repo, "init", "-q")
    # No commit yet: HEAD is unborn.

    handle = workspace.begin(str(repo))
    assert handle.available is False
    assert "unborn" in handle.reason or "HEAD" in handle.reason


def test_unmeasured_shape_matches_a_degraded_measure(tmp_path):
    direct = workspace.unmeasured("/some/repo", "some reason")
    handle = workspace.WorkspaceHandle(repo="/some/repo", available=False, reason="some reason")
    via_measure = workspace.measure(handle)
    assert direct == via_measure
    assert set(direct.keys()) == {
        "measured",
        "repo",
        "reason",
        "branch",
        "head_before",
        "head_after",
        "status_porcelain",
        "changed_files",
        "diffstat",
    }


def test_vanished_workspace_degrades_to_unmeasured_after_begin(tmp_path):
    """A repo that disappears mid-session must not report measured facts:
    the post-session HEAD probe is the anchor — when it fails, the block
    degrades to the honest unmeasured shape with a reason, never
    `measured: True` with silent nulls."""
    import shutil
    import subprocess

    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    (repo / "f.txt").write_text("hello\n")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(
        ["git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "c1"],
        cwd=repo,
        check=True,
    )

    handle = workspace.begin(str(repo))
    assert handle.available is True

    shutil.rmtree(repo)

    block = workspace.measure(handle)
    assert block["measured"] is False
    assert block["reason"] is not None and "HEAD" in block["reason"]
    assert block["head_after"] is None
    assert block["changed_files"] == []


def test_repo_configured_diff_commands_never_execute(tmp_path):
    """A measured repo must not be able to run code on the bridge host:
    diff invocations pass --no-ext-diff/--no-textconv so repo-configured
    external diff drivers and textconv filters never execute."""
    import subprocess

    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    marker = tmp_path / "PWNED"
    (repo / "f.txt").write_text("v1\n")
    (repo / ".gitattributes").write_text("*.txt diff=evil\n")
    subprocess.run(["git", "config", "diff.external", f"touch {marker}"], cwd=repo, check=True)
    subprocess.run(["git", "config", "diff.evil.textconv", f"touch {marker}"], cwd=repo, check=True)
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(
        ["git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "c1"],
        cwd=repo,
        check=True,
    )

    handle = workspace.begin(str(repo))
    (repo / "f.txt").write_text("v2\n")

    block = workspace.measure(handle)
    assert block["measured"] is True
    assert "f.txt" in block["changed_files"]
    assert block["diffstat"] is not None
    assert not marker.exists(), "repo-configured diff command executed on the bridge host"
