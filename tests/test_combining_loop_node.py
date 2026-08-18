"""scripts/combining-loop-node.py — the combining-loop workflow's code node.

Real local git fixtures throughout (temp repos + bare remotes), no network —
the same fixture style tests/test_merge_gate.py uses, for the same reason:
the whole claim this program makes is that it measured or moved real git
objects, and a test that stubbed git would assert nothing about that claim.

Three subprocess boundaries are exercised end to end:

  * harvest fetches from a real "upstream" repository over a `file://`-free
    local path (HARVEST_REMOTE), so no fixture ever depends on network access.
  * stage adds and tears down a real detached worktree against a real
    workspace repository.
  * merge pushes to a real local bare repository standing in for `origin`,
    through the actual GIT_ASKPASS + credential-helper-reset machinery — a
    small "git" shim on PATH records every invocation's argv so the
    helper-reset flags and the absence of the push credential from argv can
    be asserted on the real command line the OS would have executed, not on
    a mock of it.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).parents[1] / "scripts" / "combining-loop-node.py"
RUN_ID = "01M0COMBININGLOOPRUNID0"
TOKEN = "s3cr3t-worker-token-should-never-leak"  # noqa: S105 - fixture value, not a real credential


# ---------------------------------------------------------------------------
# git helpers
# ---------------------------------------------------------------------------


def git(repo: Path, *args: str, check: bool = True) -> str:
    proc = subprocess.run(
        ["git", *args],
        cwd=repo,
        capture_output=True,
        text=True,
        check=check,
        env={**os.environ, "GIT_TERMINAL_PROMPT": "0"},
    )
    return proc.stdout.strip()


def git_optional(repo: Path, *args: str) -> str:
    return git(repo, *args, check=False)


def _configure(repo: Path) -> None:
    git(repo, "config", "user.email", "combining-loop-test@example.invalid")
    git(repo, "config", "user.name", "combining-loop-test")


def commit_on_ref(repo: Path, ref: str, path: str, body: str = "content\n") -> str:
    """Commit onto `ref` without disturbing the checked-out branch, by
    committing on a throwaway branch and then pointing `ref` at the result
    directly. Mirrors how a bridge mints a refs/culture-nodes/ ref without
    ever checking it out itself."""
    original = git(repo, "rev-parse", "--abbrev-ref", "HEAD")
    git(repo, "branch", "-f", "_scratch", "HEAD")
    git(repo, "checkout", "-q", "_scratch")
    target = repo / path
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(body)
    git(repo, "add", path)
    git(repo, "commit", "-q", "-m", f"handover {path}")
    sha = git(repo, "rev-parse", "HEAD")
    git(repo, "update-ref", ref, sha)
    git(repo, "checkout", "-q", original)
    git(repo, "branch", "-D", "_scratch")
    return sha


def commit_on_detached_branch(repo: Path, base_ref: str, path: str, body: str) -> str:
    """Build one commit on top of `base_ref` on a throwaway branch, leaving
    every existing branch untouched, and return its sha. Used to build a
    harvested-package commit without moving `feature/combine` or `main`."""
    original = git(repo, "rev-parse", "--abbrev-ref", "HEAD")
    git(repo, "checkout", "-q", "-b", "_package", base_ref)
    target = repo / path
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(body)
    git(repo, "add", path)
    git(repo, "commit", "-q", "-m", f"package {path}")
    sha = git(repo, "rev-parse", "HEAD")
    git(repo, "checkout", "-q", original)
    git(repo, "branch", "-D", "_package")
    return sha


# ---------------------------------------------------------------------------
# fixtures
# ---------------------------------------------------------------------------


@pytest.fixture
def upstream(tmp_path: Path) -> Path:
    """The repository harvest fetches FROM — a stand-in for a session's own
    git host, playing no role beyond holding refs/culture-nodes/ refs."""
    root = tmp_path / "upstream"
    root.mkdir()
    git(root, "init", "-q", "-b", "main")
    _configure(root)
    (root / "README.md").write_text("upstream\n")
    git(root, "add", "README.md")
    git(root, "commit", "-q", "-m", "base")
    return root


@pytest.fixture
def workspace(tmp_path: Path) -> Path:
    """The integration repo every subcommand's NODES_WORKSPACE points at: a
    main history plus a feature branch, ready to receive a harvested
    package."""
    root = tmp_path / "workspace"
    root.mkdir()
    git(root, "init", "-q", "-b", "main")
    _configure(root)
    (root / "README.md").write_text("workspace\n")
    git(root, "add", "README.md")
    git(root, "commit", "-q", "-m", "base")
    git(root, "checkout", "-q", "-b", "feature/combine")
    (root / "feature.txt").write_text("feature work\n")
    git(root, "add", "feature.txt")
    git(root, "commit", "-q", "-m", "feature work")
    git(root, "checkout", "-q", "main")
    return root


@pytest.fixture
def workspace_with_origin(workspace: Path, tmp_path: Path) -> tuple[Path, Path]:
    """`workspace`, plus a real local bare repository standing in for
    `origin` and already carrying both branches — the merge push target."""
    bare = tmp_path / "origin.git"
    subprocess.run(["git", "init", "-q", "--bare", str(bare)], check=True)
    git(workspace, "remote", "add", "origin", str(bare))
    git(workspace, "push", "-q", "origin", "main")
    git(workspace, "push", "-q", "origin", "feature/combine")
    return workspace, bare


def bare_ref(bare: Path, ref: str) -> str:
    return git_optional(bare, "--git-dir", str(bare), "rev-parse", "--verify", ref)


# ---------------------------------------------------------------------------
# running the node
# ---------------------------------------------------------------------------


def run_node(
    workspace: Path,
    subcommand: str,
    input_payload: dict,
    *,
    env: dict | None = None,
    run_id: str = RUN_ID,
) -> subprocess.CompletedProcess:
    stripped = {"HARVEST_REMOTE", "GITHUB_TOKEN_WORKER"}
    clean = {
        k: v for k, v in os.environ.items() if not k.startswith("NODES_") and k not in stripped
    }
    full_env = {
        **clean,
        "NODES_INPUT_JSON": json.dumps(input_payload),
        "NODES_RUN_ID": run_id,
        "NODES_WORKSPACE": str(workspace),
        **(env or {}),
    }
    return subprocess.run(
        [sys.executable, str(SCRIPT), subcommand],
        capture_output=True,
        text=True,
        env=full_env,
    )


def result_of(proc: subprocess.CompletedProcess) -> dict:
    assert proc.stdout, f"no result on stdout; stderr was:\n{proc.stderr}"
    return json.loads(proc.stdout)


def hint_of(proc: subprocess.CompletedProcess) -> str:
    lines = [line for line in proc.stderr.splitlines() if line.startswith("hint:")]
    assert lines, f"no hint: line in stderr:\n{proc.stderr}"
    return lines[0]


# ---------------------------------------------------------------------------
# harvest
# ---------------------------------------------------------------------------


def test_harvest_happy_path_pins_the_fetched_commit(workspace: Path, upstream: Path):
    ref = "refs/culture-nodes/run-1/node-1"
    sha = commit_on_ref(upstream, ref, "handover.txt")

    proc = run_node(
        workspace,
        "harvest",
        {"handover_ref": ref, "expected_commit": sha},
        env={"HARVEST_REMOTE": str(upstream)},
    )

    assert proc.returncode == 0, proc.stderr
    result = result_of(proc)
    assert result["harvested_commit"] == sha
    assert result["recovery_ref"] == f"refs/culture-nodes/harvested/{RUN_ID}"
    assert git(workspace, "rev-parse", "--verify", result["recovery_ref"]) == sha


def test_harvest_refuses_a_ref_outside_the_handover_namespace(workspace: Path, upstream: Path):
    sha = git(upstream, "rev-parse", "HEAD")

    proc = run_node(
        workspace,
        "harvest",
        {"handover_ref": "refs/heads/main", "expected_commit": sha},
        env={"HARVEST_REMOTE": str(upstream)},
    )

    assert proc.returncode == 4, proc.stderr
    assert "refs/culture-nodes/" in proc.stderr
    # Refused before any git work: nothing was fetched, so no recovery ref
    # exists at all.
    assert (
        git_optional(workspace, "rev-parse", "--verify", f"refs/culture-nodes/harvested/{RUN_ID}")
        == ""
    )


def test_harvest_refuses_a_commit_mismatch(workspace: Path, upstream: Path):
    ref = "refs/culture-nodes/run-2/node-1"
    real_sha = commit_on_ref(upstream, ref, "handover2.txt")
    wrong_sha = git(workspace, "rev-parse", "HEAD")
    assert wrong_sha != real_sha

    proc = run_node(
        workspace,
        "harvest",
        {"handover_ref": ref, "expected_commit": wrong_sha},
        env={"HARVEST_REMOTE": str(upstream)},
    )

    assert proc.returncode == 4, proc.stderr
    assert real_sha in proc.stderr
    assert wrong_sha in proc.stderr
    assert "does not match" in proc.stderr


def test_harvest_recovery_ref_is_saved_before_the_comparison_that_can_refuse_it(
    workspace: Path, upstream: Path
):
    """The load-bearing ordering claim: the fetch writes the recovery ref
    directly (git only moves a ref after it has the objects a fetch names),
    so even when the SUBSEQUENT expected-commit comparison refuses the
    harvest, the fetched work is not lost — it is exactly what makes a run
    killed between the fetch and the comparison recoverable, without this
    test needing to race a real SIGKILL against git's own atomicity."""
    ref = "refs/culture-nodes/run-3/node-1"
    real_sha = commit_on_ref(upstream, ref, "handover3.txt")
    wrong_sha = git(workspace, "rev-parse", "HEAD")

    proc = run_node(
        workspace,
        "harvest",
        {"handover_ref": ref, "expected_commit": wrong_sha},
        env={"HARVEST_REMOTE": str(upstream)},
    )

    assert proc.returncode == 4, proc.stderr
    recovery_ref = f"refs/culture-nodes/harvested/{RUN_ID}"
    recovered = git(workspace, "rev-parse", "--verify", recovery_ref)
    assert recovered == real_sha, "the fetched commit must survive a refusal that happens after it"
    # And recoverable means inspectable content, not just a dangling sha.
    contents = git(workspace, "show", f"{recovery_ref}:handover3.txt")
    assert contents == "content"


def test_harvest_refuses_an_unknown_input_key(workspace: Path, upstream: Path):
    ref = "refs/culture-nodes/run-4/node-1"
    sha = commit_on_ref(upstream, ref, "handover4.txt")

    proc = run_node(
        workspace,
        "harvest",
        {"handover_ref": ref, "expected_commit": sha, "extra": "nope"},
        env={"HARVEST_REMOTE": str(upstream)},
    )

    assert proc.returncode == 4, proc.stderr
    assert "'extra'" in proc.stderr
    assert "handover_ref" in hint_of(proc) and "expected_commit" in hint_of(proc)


# ---------------------------------------------------------------------------
# stage
# ---------------------------------------------------------------------------


def test_stage_happy_path_creates_a_two_parent_candidate(workspace: Path):
    feature_commit = git(workspace, "rev-parse", "feature/combine")
    harvested_sha = commit_on_detached_branch(workspace, "main", "package.txt", "payload\n")

    proc = run_node(
        workspace, "stage", {"harvested_commit": harvested_sha, "feature_branch": "feature/combine"}
    )

    assert proc.returncode == 0, proc.stderr
    result = result_of(proc)
    assert result["outcome"] == "candidate_staged"
    candidate = result["candidate_commit"]

    parents = git(workspace, "show", "-s", "--format=%P", candidate).split()
    assert parents == [feature_commit, harvested_sha]
    changed = git(workspace, "diff", "--name-only", feature_commit, candidate).splitlines()
    assert "package.txt" in changed

    # The staging worktree is cleaned up either way.
    worktrees = git(workspace, "worktree", "list", "--porcelain")
    assert worktrees.count("worktree ") == 1


def test_stage_reports_a_merge_conflict_as_a_domain_outcome_not_a_refusal(workspace: Path):
    harvested_sha = commit_on_detached_branch(
        workspace, "main", "feature.txt", "conflicting content\n"
    )

    proc = run_node(
        workspace, "stage", {"harvested_commit": harvested_sha, "feature_branch": "feature/combine"}
    )

    assert proc.returncode == 1, proc.stderr
    result = result_of(proc)
    assert result["outcome"] == "merge_conflict"
    assert result["conflicted_paths"] == ["feature.txt"]
    assert "candidate_commit" not in result

    worktrees = git(workspace, "worktree", "list", "--porcelain")
    assert worktrees.count("worktree ") == 1, "the conflicted worktree must still be cleaned up"


def test_stage_routes_a_dot_github_change_to_a_human_unconditionally(workspace: Path):
    feature_commit = git(workspace, "rev-parse", "feature/combine")
    harvested_sha = commit_on_detached_branch(
        workspace, "main", ".github/workflows/ci.yml", "name: ci\n"
    )

    proc = run_node(
        workspace, "stage", {"harvested_commit": harvested_sha, "feature_branch": "feature/combine"}
    )

    assert proc.returncode == 3, proc.stderr
    result = result_of(proc)
    assert result["outcome"] == "routes_to_human"
    assert ".github/workflows/ci.yml" in result["guarded_paths"]
    assert "candidate_commit" not in result
    # No candidate object was ever materialized for a guarded change: the
    # feature branch itself must be exactly where it started.
    assert git(workspace, "rev-parse", "feature/combine") == feature_commit


def test_stage_refuses_an_unknown_input_key(workspace: Path):
    harvested_sha = commit_on_detached_branch(workspace, "main", "package.txt", "payload\n")

    proc = run_node(
        workspace,
        "stage",
        {
            "harvested_commit": harvested_sha,
            "feature_branch": "feature/combine",
            "cwd": ".",
        },
    )

    assert proc.returncode == 4, proc.stderr
    assert "'cwd'" in proc.stderr


# ---------------------------------------------------------------------------
# merge
# ---------------------------------------------------------------------------


def build_candidate(workspace: Path, package_body: str = "payload\n") -> tuple[str, str]:
    """A detached two-parent merge commit exactly like `stage` produces,
    without moving `feature/combine` itself — merge's job is to move it."""
    feature_commit = git(workspace, "rev-parse", "feature/combine")
    package_sha = commit_on_detached_branch(workspace, "main", "package.txt", package_body)
    original = git(workspace, "rev-parse", "--abbrev-ref", "HEAD")
    git(workspace, "checkout", "-q", "--detach", feature_commit)
    git(workspace, "merge", "-q", "--no-ff", "--no-edit", package_sha)
    candidate = git(workspace, "rev-parse", "HEAD")
    git(workspace, "checkout", "-q", original)
    return candidate, feature_commit


def test_merge_happy_path_fast_forwards_and_pushes(workspace_with_origin: tuple[Path, Path]):
    workspace, bare = workspace_with_origin
    candidate, _feature_commit = build_candidate(workspace)

    proc = run_node(
        workspace,
        "merge",
        {
            "candidate_commit": candidate,
            "feature_branch": "feature/combine",
            "verdict": {"outcome": "gates_passed", "candidate": candidate},
        },
        env={"GITHUB_TOKEN_WORKER": TOKEN},
    )

    assert proc.returncode == 0, proc.stderr
    result = result_of(proc)
    assert result["merged_commit"] == candidate
    assert git(workspace, "rev-parse", "feature/combine") == candidate
    assert bare_ref(bare, "refs/heads/feature/combine") == candidate


def test_merge_refuses_a_verdict_that_does_not_match_the_candidate(
    workspace_with_origin: tuple[Path, Path],
):
    workspace, bare = workspace_with_origin
    candidate, feature_commit = build_candidate(workspace)
    before = bare_ref(bare, "refs/heads/feature/combine")

    proc = run_node(
        workspace,
        "merge",
        {
            "candidate_commit": candidate,
            "feature_branch": "feature/combine",
            "verdict": {"outcome": "changes_required", "candidate": candidate},
        },
        env={"GITHUB_TOKEN_WORKER": TOKEN},
    )

    assert proc.returncode == 4, proc.stderr
    assert "changes_required" in proc.stderr
    # Nothing moved, locally or remotely.
    assert git(workspace, "rev-parse", "feature/combine") == feature_commit
    assert bare_ref(bare, "refs/heads/feature/combine") == before


def test_merge_refuses_a_verdict_for_a_different_candidate_no_toctou(
    workspace_with_origin: tuple[Path, Path],
):
    """A gates_passed verdict is only authorization for the EXACT candidate
    it names — a stale verdict for an earlier candidate must not merge a
    later one."""
    workspace, bare = workspace_with_origin
    candidate, feature_commit = build_candidate(workspace)
    stale_candidate, _ = build_candidate(workspace, package_body="a different payload\n")

    proc = run_node(
        workspace,
        "merge",
        {
            "candidate_commit": candidate,
            "feature_branch": "feature/combine",
            "verdict": {"outcome": "gates_passed", "candidate": stale_candidate},
        },
        env={"GITHUB_TOKEN_WORKER": TOKEN},
    )

    assert proc.returncode == 4, proc.stderr
    assert git(workspace, "rev-parse", "feature/combine") == feature_commit
    assert bare_ref(bare, "refs/heads/feature/combine") == feature_commit


def test_merge_refuses_an_unknown_input_key(workspace_with_origin: tuple[Path, Path]):
    workspace, _bare = workspace_with_origin
    candidate, _ = build_candidate(workspace)

    proc = run_node(
        workspace,
        "merge",
        {
            "candidate_commit": candidate,
            "feature_branch": "feature/combine",
            "verdict": {"outcome": "gates_passed", "candidate": candidate, "actor": "codex-thor"},
        },
        env={"GITHUB_TOKEN_WORKER": TOKEN},
    )

    assert proc.returncode == 4, proc.stderr
    assert "'actor'" in proc.stderr


def _git_argv_shim(tmp_path: Path) -> tuple[Path, Path]:
    """A `git` on PATH that logs every invocation's argv before exec'ing the
    real binary, so the actual command line a push ran with can be inspected
    directly rather than trusted from the production code's own claims."""
    real_git = shutil.which("git")
    assert real_git, "this test environment has no git on PATH"
    shim_dir = tmp_path / "shim"
    shim_dir.mkdir()
    log_path = tmp_path / "git-argv.log"
    shim = shim_dir / "git"
    shim.write_text(
        "#!/bin/sh\n" f'printf "%s\\n" "$*" >> "{log_path}"\n' f'exec "{real_git}" "$@"\n'
    )
    shim.chmod(0o755)
    return shim_dir, log_path


def test_merge_push_resets_credential_helpers_and_never_puts_the_token_in_argv(
    workspace_with_origin: tuple[Path, Path], tmp_path: Path
):
    workspace, bare = workspace_with_origin
    candidate, _ = build_candidate(workspace)
    shim_dir, log_path = _git_argv_shim(tmp_path)

    proc = run_node(
        workspace,
        "merge",
        {
            "candidate_commit": candidate,
            "feature_branch": "feature/combine",
            "verdict": {"outcome": "gates_passed", "candidate": candidate},
        },
        env={
            "GITHUB_TOKEN_WORKER": TOKEN,
            "PATH": f"{shim_dir}:{os.environ['PATH']}",
        },
    )

    assert proc.returncode == 0, proc.stderr
    assert TOKEN not in proc.stdout
    assert TOKEN not in proc.stderr
    assert bare_ref(bare, "refs/heads/feature/combine") == candidate

    log_lines = log_path.read_text().splitlines()
    assert log_lines, "the shim recorded no git invocations at all"
    push_lines = [line for line in log_lines if "push" in line and "porcelain" in line]
    assert len(push_lines) == 1, f"expected exactly one push invocation, saw: {log_lines}"
    push_argv = push_lines[0]
    assert "credential.helper=" in push_argv
    assert "credential.https://github.com.helper=" in push_argv
    assert "--porcelain" in push_argv
    for line in log_lines:
        assert TOKEN not in line, f"the worker token leaked into a git invocation: {line!r}"


def test_merge_requires_the_worker_credential_grant(workspace_with_origin: tuple[Path, Path]):
    workspace, bare = workspace_with_origin
    candidate, feature_commit = build_candidate(workspace)
    before = bare_ref(bare, "refs/heads/feature/combine")

    proc = run_node(
        workspace,
        "merge",
        {
            "candidate_commit": candidate,
            "feature_branch": "feature/combine",
            "verdict": {"outcome": "gates_passed", "candidate": candidate},
        },
    )

    assert proc.returncode == 2, proc.stderr
    assert "GITHUB_TOKEN_WORKER" in proc.stderr
    assert git(workspace, "rev-parse", "feature/combine") == feature_commit
    assert bare_ref(bare, "refs/heads/feature/combine") == before


# ---------------------------------------------------------------------------
# --help documents the whole contract
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("subcommand", ["harvest", "stage", "merge"])
def test_help_documents_the_input_schema_and_exit_codes(subcommand: str):
    proc = subprocess.run(
        [sys.executable, str(SCRIPT), subcommand, "--help"],
        capture_output=True,
        text=True,
        check=False,
    )

    assert proc.returncode == 0, proc.stderr
    assert "NODES_INPUT_JSON" in proc.stdout
    assert "Exit 0" in proc.stdout


def test_top_level_help_names_every_subcommand():
    proc = subprocess.run(
        [sys.executable, str(SCRIPT), "--help"], capture_output=True, text=True, check=False
    )

    assert proc.returncode == 0, proc.stderr
    for subcommand in ("harvest", "stage", "merge"):
        assert subcommand in proc.stdout
