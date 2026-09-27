import subprocess

from pi_bridge import preserve, stamping, workspace


def _git(repo, *args):
    return subprocess.run(
        ["git", *args], cwd=repo, check=True, capture_output=True, text=True
    ).stdout


def test_marked_handover_commit_has_trailer_and_unmarked_stays_unchanged(tmp_path):
    marker = "cn1:firing-a:git.ref:" + "ab" * 24 + ":" + "cd" * 32
    repo = tmp_path / "repo"
    repo.mkdir()
    _git(repo, "init", "-q")
    _git(repo, "config", "user.email", "test@example.com")
    _git(repo, "config", "user.name", "Test")
    (repo / "README.md").write_text("initial\n")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-m", "initial")
    _git(repo, "remote", "add", "origin", "https://github.com/agentculture/culture-nodes.git")
    handle = workspace.begin(str(repo))
    (repo / "work.txt").write_text("changed\n")
    measured = workspace.measure(handle)
    args = dict(
        enabled=True,
        remote="origin",
        run_id="run_1",
        node_run_id="nr_1",
        attempt_id="att_1",
        reason="completed",
    )
    marked = preserve.handover_ref(str(repo), measured, marker=marker, **args)
    assert marked.created
    assert stamping.artifact_result(marked.ref, marker)["artifact_id"] == marked.ref
    assert _git(repo, "show", "-s", "--format=%B", marked.commit).rstrip().endswith(marker)
    plain = preserve.handover_ref(str(repo), measured, **args)
    assert plain.created
    assert marker not in _git(repo, "show", "-s", "--format=%B", plain.commit)
