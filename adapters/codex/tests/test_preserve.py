"""`preserve.py`'s own unit tests (task t25, issue #49). Every assertion
here proves the plumbing sequence (write-tree -> commit-tree -> update-ref)
never disturbs the live checkout's HEAD, index, or working tree — the
acceptance bullet most likely to be faked by a test that merely doesn't
check (see `test_preserve_commit_leaves_head_index_and_status_byte_identical`).
"""

from __future__ import annotations

import json
import pathlib
import re
import subprocess

from codex_bridge import preserve, workspace


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


# ---------------------------------------------------------------------------
# the handover carrier (task t6, issue #74, spec decision q9)
# ---------------------------------------------------------------------------


def _handover(repo, measured, **overrides):
    kwargs = dict(
        enabled=True,
        remote="origin",
        run_id="run_1",
        node_run_id="nr_1",
        attempt_id="att_1",
        reason="fix completed; handing the diff to the review node on another host",
    )
    kwargs.update(overrides)
    return preserve.handover_ref(str(repo), measured, **kwargs)


def _repo_with_remote(tmp_path, url: str):
    """A scratch repo whose `origin` is *url* and whose working tree has
    changes to hand over. `git remote add` is pure config — it contacts
    nothing, so a url that does not resolve is still a faithful fixture."""
    repo = tmp_path / "repo"
    _init_repo(repo)
    _git(repo, "remote", "add", "origin", url)
    handle = workspace.begin(str(repo))
    (repo / "README.md").write_text("# scratch\n\nfixed by the session\n")
    (repo / "new_note.txt").write_text("the work being handed over\n")
    return repo, workspace.measure(handle)


def _canonical_git_ref_pattern() -> str:
    """The `git_ref` ref pattern read from schemas/workflow/handoff.schema.json
    — the ONE declaration of what a portable handle is (spec decision q9). The
    producer is asserted against that file rather than against a pattern
    retyped here, so this bridge cannot drift from the contract the engine and
    tests/lint/crosshosthandoff_test.go enforce."""
    schema_path = (
        pathlib.Path(__file__).resolve().parents[3] / "schemas" / "workflow" / "handoff.schema.json"
    )
    schema = json.loads(schema_path.read_text())
    for branch in schema["allOf"]:
        if branch["if"]["properties"]["kind"]["const"] == "git_ref":
            return branch["then"]["properties"]["ref"]["pattern"]
    raise AssertionError(f"{schema_path} declares no git_ref branch")


def test_handover_creates_a_run_scoped_ref_reachable_from_no_branch(tmp_path):
    repo, measured = _repo_with_remote(
        tmp_path, "https://github.com/agentculture/culture-nodes.git"
    )
    branches_before = _git_output(repo, "branch", "--list")

    result = _handover(repo, measured)

    assert result.created is True
    assert result.missing_capability is None
    assert result.ref is not None and result.ref.startswith("refs/culture-nodes/run_1/")
    assert _git_output(repo, "show-ref", "--verify", result.ref)
    # The handed-over commit really carries the session's work.
    show = _git_output(repo, "show", "--stat", result.commit)
    assert "README.md" in show
    assert "new_note.txt" in show
    # AGENTS.md: never a branch. No branch was created, and no branch reaches
    # the handover commit — it is the operator's or the control plane's move
    # to make it reachable, not this module's.
    assert _git_output(repo, "branch", "--list") == branches_before
    assert _git_output(repo, "branch", "--contains", result.commit).strip() == ""


def test_handover_leaves_head_index_and_status_byte_identical(tmp_path):
    repo, measured = _repo_with_remote(
        tmp_path, "https://github.com/agentculture/culture-nodes.git"
    )
    pre_status = _git_output(repo, "status", "--porcelain")
    pre_head = _head(repo)
    pre_index = _index_bytes(repo)

    result = _handover(repo, measured)
    assert result.created is True

    assert _git_output(repo, "status", "--porcelain") == pre_status
    assert _head(repo) == pre_head
    assert _index_bytes(repo) == pre_index


def test_handover_handle_satisfies_the_canonical_schema(tmp_path):
    repo, measured = _repo_with_remote(
        tmp_path, "https://github.com/agentculture/culture-nodes.git"
    )
    result = _handover(repo, measured)

    handle = result.handle
    assert handle is not None
    assert handle["kind"] == "git_ref"
    assert re.search(_canonical_git_ref_pattern(), handle["ref"]), handle["ref"]
    assert handle["ref"] == f"git+https://github.com/agentculture/culture-nodes.git#{result.ref}"
    # The commit is PINNED in the handle: a consuming host can verify it
    # fetched the object this producer named, not whatever the ref points at
    # by the time it looks.
    assert handle["commit"] == result.commit
    assert re.fullmatch(r"[0-9a-f]{40}", handle["commit"])
    # Honest about publication: this module never pushes, so the ref has not
    # reached the remote the handle names.
    assert handle["publication"] == "pending"


def test_handover_never_runs_git_push(tmp_path):
    """The push refusal is POLICY, not a missing credential, so it is asserted
    on the command list rather than on a push's outcome: a test that watched a
    push fail would still pass if the push were attempted."""
    repo, measured = _repo_with_remote(
        tmp_path, "https://github.com/agentculture/culture-nodes.git"
    )
    calls: list[tuple[str, ...]] = []
    real_run_git = preserve._run_git

    def recording(repo_arg, *args, **kwargs):
        calls.append(args)
        return real_run_git(repo_arg, *args, **kwargs)

    preserve._run_git = recording
    try:
        result = _handover(repo, measured)
    finally:
        preserve._run_git = real_run_git

    assert result.created is True
    assert calls, "no git command ran at all"
    assert not any(args and args[0] == "push" for args in calls), calls
    # And the one ref-writing command wrote the handover ref, nothing else.
    ref_writes = [args for args in calls if args and args[0] == "update-ref"]
    assert ref_writes == [("update-ref", result.ref, result.commit)]


def test_handover_refuses_a_filesystem_remote_and_names_the_capability(tmp_path):
    remote = tmp_path / "remote.git"
    subprocess.run(
        ["git", "init", "-q", "--bare", str(remote)], check=True, capture_output=True, text=True
    )
    repo, measured = _repo_with_remote(tmp_path, str(remote))

    result = _handover(repo, measured)

    assert result.created is False
    assert result.missing_capability == preserve.MISSING_GIT_REF_PUBLISH
    assert result.reason is not None and "filesystem path" in result.reason
    assert result.handle is None
    # Refused BEFORE anything was written: no stray handover ref is left in a
    # checkout whose handle could never have been portable.
    assert _git_output(repo, "for-each-ref", preserve.HANDOVER_REF_NAMESPACE).strip() == ""


def test_handover_refuses_a_missing_remote_and_names_the_capability(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    handle = workspace.begin(str(repo))
    (repo / "README.md").write_text("# scratch\n\nfixed\n")
    measured = workspace.measure(handle)

    result = _handover(repo, measured)

    assert result.created is False
    assert result.missing_capability == preserve.MISSING_GIT_REF_PUBLISH
    assert result.reason is not None and "origin" in result.reason


def test_handover_strips_a_credential_out_of_the_remote_url(tmp_path):
    """A handle is reported to the control plane and lands in the run record,
    so a credential embedded in the remote url would be written into the
    ledger by the act of handing work over."""
    # A reserved domain and an obviously inert password: the point under test
    # is the STRIPPING, and a committed fixture never carries a credential
    # shape a scanner would have to reason about (spec claim c25).
    repo, measured = _repo_with_remote(
        tmp_path,
        "https://x-access-token:inert-not-a-real-secret@git.example.com/agentculture/nodes.git",
    )

    result = _handover(repo, measured)

    assert result.created is True
    assert result.handle["ref"].startswith("git+https://git.example.com/agentculture/nodes.git#")
    assert "inert-not-a-real-secret" not in result.handle["ref"]
    assert "x-access-token" not in result.handle["ref"]
    assert "@" not in result.handle["ref"]


def test_handover_normalises_the_scp_like_remote_to_an_ssh_url(tmp_path):
    repo, measured = _repo_with_remote(tmp_path, "git@github.com:agentculture/culture-nodes.git")

    result = _handover(repo, measured)

    assert result.created is True
    assert result.handle["ref"].startswith(
        "git+ssh://git@github.com/agentculture/culture-nodes.git#"
    )
    assert re.search(_canonical_git_ref_pattern(), result.handle["ref"])


def test_handover_not_requested_writes_nothing(tmp_path):
    repo, measured = _repo_with_remote(
        tmp_path, "https://github.com/agentculture/culture-nodes.git"
    )

    result = _handover(repo, measured, enabled=False)

    assert result.attempted is False
    assert result.created is False
    assert result.missing_capability is None
    assert result.reason is not None and "no handover" in result.reason
    assert _git_output(repo, "for-each-ref", preserve.HANDOVER_REF_NAMESPACE).strip() == ""


def test_handover_with_nothing_to_hand_over_names_workspace_export(tmp_path):
    repo = tmp_path / "repo"
    _init_repo(repo)
    _git(repo, "remote", "add", "origin", "https://github.com/agentculture/culture-nodes.git")
    handle, measured = _measure_after_change(repo)

    result = _handover(repo, measured)

    assert result.created is False
    assert result.missing_capability == preserve.MISSING_WORKSPACE_EXPORT
    assert result.reason is not None and "nothing to hand over" in result.reason


def test_handover_on_an_unmeasurable_workspace_names_workspace_export(tmp_path):
    repo = tmp_path / "not-a-repo"
    repo.mkdir()
    handle = workspace.begin(str(repo))
    measured = workspace.measure(handle)

    result = _handover(repo, measured)

    assert result.created is False
    assert result.missing_capability == preserve.MISSING_WORKSPACE_EXPORT


def test_mint_handover_ref_is_run_scoped_and_unique_across_calls():
    a = preserve.mint_handover_ref("run_1", "nr_1", "att_1")
    b = preserve.mint_handover_ref("run_1", "nr_1", "att_1")
    assert a != b
    for name in (a, b):
        assert name.startswith("refs/culture-nodes/run_1/nr_1-att_1-")
        # Never a branch, by construction rather than by convention.
        assert not name.startswith("refs/heads/")


def test_mint_handover_ref_sanitizes_unsafe_characters():
    name = preserve.mint_handover_ref("run one/two", None, None)
    assert " " not in name
    assert name.startswith("refs/culture-nodes/run-one-two/")
