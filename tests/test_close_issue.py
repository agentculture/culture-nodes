"""`scripts/close-issue.sh` gains a third evidence shape: `--artifact`.

Issue #157, task t5. The closing contract has always demanded a disposition,
a reason, and ONE piece of checkable evidence -- either a Culture Nodes run
id or a test path plus the command that runs it. A **Record** issue (a
deviation, an audit snapshot, a counted operator hand-turn) has neither: it
was complete when it was written, and its content lives in the tree. So it
needs an evidence shape of its own -- the committed artifact it points at.

The load-bearing part is not "accept a path". It is refusing a path that is
not evidence:

  1. a path that does not exist, and
  2. a path that exists but is UNTRACKED by git.

The second is the one a naive `[[ -e ]]` check misses, and it is the more
likely failure: a record drafted on the author's disk and never committed
reads exactly like a committed one from inside the closing shell. Hence
`git ls-files --error-unmatch`.

`gh` is stubbed by prepending a temporary directory to PATH. Nothing here
touches GitHub, and the refusal tests exit before any `gh` call at all -- the
stub's argv log staying empty IS the assertion in those cases.
"""

import os
import subprocess  # nosec B404 - runs an in-repo shell script, no external input
import uuid
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "close-issue.sh"

# A path that is real AND tracked, so an artifact closure has something honest
# to point at. This file is part of the same task, which makes it a fair
# stand-in for the record a Record issue would cite.
TRACKED_ARTIFACT = "docs/triage/closing-comment-template.md"


@pytest.fixture
def gh_stub(tmp_path):
    """A fake `gh` on PATH that records its argv and never talks to GitHub.

    Returns ``(env, argv_log)``. ``argv_log`` is NUL-separated so the
    multi-line closing comment survives the round trip intact.
    """
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    argv_log = tmp_path / "gh-argv"
    stub = bin_dir / "gh"
    stub.write_text('#!/usr/bin/env bash\nprintf "%s\\0" "$@" > "$GH_ARGV_LOG"\n')
    stub.chmod(0o755)
    env = dict(
        os.environ,
        PATH=f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
        GH_ARGV_LOG=str(argv_log),
    )
    return env, argv_log


def run_close(env, *args):
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [str(SCRIPT), *args],
        cwd=str(ROOT),
        text=True,
        capture_output=True,
        env=env,
        timeout=60,
    )


def gh_argv(argv_log):
    if not argv_log.exists():
        return []
    raw = argv_log.read_text()
    return [part for part in raw.split("\0") if part != ""]


@pytest.fixture
def untracked_file():
    """A real file inside the repo that git does not track.

    Created with a unique name and removed afterwards so a parallel (xdist)
    run cannot collide and the worktree is left clean either way.
    """
    path = ROOT / f"untracked-artifact-{uuid.uuid4().hex}.md"
    path.write_text("# a record that was never committed\n")
    try:
        yield path.name
    finally:
        path.unlink(missing_ok=True)


# --- AC1: --artifact is a third shape, and the three are mutually exclusive ---


def test_artifact_closure_is_accepted(gh_stub):
    env, argv_log = gh_stub
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "a record, complete when written",
        "--artifact",
        TRACKED_ARTIFACT,
    )
    assert result.returncode == 0, result.stderr
    argv = gh_argv(argv_log)
    assert argv[:2] == ["issue", "close"]


def test_artifact_and_run_id_are_mutually_exclusive(gh_stub):
    env, argv_log = gh_stub
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "two shapes at once",
        "--artifact",
        TRACKED_ARTIFACT,
        "--run-id",
        "01M00AM5NME6TZ1PXDG4A454HE",
    )
    assert result.returncode != 0
    assert gh_argv(argv_log) == [], "refused closures must not reach gh"


def test_artifact_and_test_evidence_are_mutually_exclusive(gh_stub):
    env, argv_log = gh_stub
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "two shapes at once",
        "--artifact",
        TRACKED_ARTIFACT,
        "--test-path",
        "tests/test_close_issue.py",
        "--test-command",
        "uv run pytest tests/test_close_issue.py",
    )
    assert result.returncode != 0
    assert gh_argv(argv_log) == []


def test_run_id_and_test_evidence_remain_mutually_exclusive(gh_stub):
    """The pre-existing exclusion is not lost when a third shape is added."""
    env, argv_log = gh_stub
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "two shapes at once",
        "--run-id",
        "01M00AM5NME6TZ1PXDG4A454HE",
        "--test-path",
        "tests/test_close_issue.py",
        "--test-command",
        "uv run pytest tests/test_close_issue.py",
    )
    assert result.returncode != 0
    assert gh_argv(argv_log) == []


# --- AC2: a path that is not evidence is refused -----------------------------


def test_missing_artifact_path_is_refused(gh_stub):
    env, argv_log = gh_stub
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "points at nothing",
        "--artifact",
        "docs/deviations/this-file-does-not-exist.md",
    )
    assert result.returncode != 0
    assert gh_argv(argv_log) == []
    assert "artifact" in result.stderr.lower()


def test_untracked_artifact_path_is_refused(gh_stub, untracked_file):
    """Exists on disk, absent from git -- the failure `-e` alone cannot see."""
    env, argv_log = gh_stub
    assert (ROOT / untracked_file).exists()
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "never committed",
        "--artifact",
        untracked_file,
    )
    assert result.returncode != 0
    assert gh_argv(argv_log) == []
    assert "track" in result.stderr.lower()


# --- AC3: reason=completed unchanged, bare close still impossible ------------


def test_bare_close_is_still_refused(gh_stub):
    env, argv_log = gh_stub
    result = run_close(env, "999", "closed-with-reason", "no evidence of any shape")
    assert result.returncode != 0
    assert gh_argv(argv_log) == []


def test_test_path_without_command_is_still_refused(gh_stub):
    env, argv_log = gh_stub
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "half a shape",
        "--test-path",
        "tests/test_close_issue.py",
    )
    assert result.returncode != 0
    assert gh_argv(argv_log) == []


@pytest.mark.parametrize(
    "evidence",
    [
        ("--run-id", "01M00AM5NME6TZ1PXDG4A454HE"),
        ("--artifact", TRACKED_ARTIFACT),
    ],
)
def test_reason_completed_is_unchanged(gh_stub, evidence):
    env, argv_log = gh_stub
    result = run_close(env, "999", "closed-with-evidence", "why", *evidence)
    assert result.returncode == 0, result.stderr
    argv = gh_argv(argv_log)
    assert "--reason" in argv
    assert argv[argv.index("--reason") + 1] == "completed"


def test_run_id_closure_still_renders_its_field(gh_stub):
    env, argv_log = gh_stub
    result = run_close(
        env, "999", "closed-with-evidence", "why", "--run-id", "01M00AM5NME6TZ1PXDG4A454HE"
    )
    assert result.returncode == 0, result.stderr
    comment = gh_argv(argv_log)[-1]
    assert "Culture Nodes run id: `01M00AM5NME6TZ1PXDG4A454HE`" in comment


def test_test_evidence_closure_still_renders_its_fields(gh_stub):
    env, argv_log = gh_stub
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "why",
        "--test-path",
        "tests/test_close_issue.py",
        "--test-command",
        "uv run pytest tests/test_close_issue.py",
    )
    assert result.returncode == 0, result.stderr
    comment = gh_argv(argv_log)[-1]
    assert "Test path: `tests/test_close_issue.py`" in comment
    assert "Command: `uv run pytest tests/test_close_issue.py`" in comment


# --- AC5: the rendered comment carries the artifact path ---------------------


def test_rendered_comment_names_the_artifact(gh_stub):
    env, argv_log = gh_stub
    result = run_close(
        env,
        "999",
        "closed-with-evidence",
        "a record, complete when written",
        "--artifact",
        TRACKED_ARTIFACT,
    )
    assert result.returncode == 0, result.stderr
    comment = gh_argv(argv_log)[-1]
    assert f"Artifact: `{TRACKED_ARTIFACT}`" in comment
    assert "Disposition: closed-with-evidence" in comment
    assert "Reason: a record, complete when written" in comment


def test_signature_block_survives_the_new_shape(gh_stub):
    """The auto-signature resolves the nick from culture.yaml, as before."""
    env, argv_log = gh_stub
    result = run_close(env, "999", "closed-with-evidence", "why", "--artifact", TRACKED_ARTIFACT)
    assert result.returncode == 0, result.stderr
    comment = gh_argv(argv_log)[-1]
    assert "- culture-nodes (Claude)" in comment


# --- AC4: the template documents the artifact field --------------------------


def test_closing_comment_template_documents_the_artifact_field():
    template = (ROOT / "docs" / "triage" / "closing-comment-template.md").read_text()
    assert "Artifact: `" in template, "the artifact shape needs the same fixed-label format"
    assert "Culture Nodes run id: `" in template
    assert "Test path: `" in template
