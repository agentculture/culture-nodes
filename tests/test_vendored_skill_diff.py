"""The vendored-skill guard tells a re-vendor apart from a local patch (#212).

Before this, the guard failed on ANY change under a vendored skill -- so the
re-vendor its own error message recommended could never pass CI, and a vendored
skill could not be updated through a PR at all.

The negative cases below matter more than the positive one. A guard that is
only ever observed passing is how this class of defect gets re-introduced, so
each way of NOT earning a pass is pinned separately: no ledger bump at all, a
bump on the wrong skill, and a bump that edits prose without advancing the sync
cell.
"""

from __future__ import annotations

import shutil
import subprocess  # nosec B404 - fixed argv, no shell
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
GUARD = ROOT / "scripts" / "check-vendored-skill-diff.py"

HEADER = "| Skill | Upstream | Origin | Notes | Last synced |"
SEPARATOR = "|-------|----------|--------|-------|-------------|"


def _ledger(rows: dict[str, tuple[str, str]]) -> str:
    """Render a skill-sources.md body. rows maps name -> (notes, last synced)."""
    lines = ["# Skill upstream sources", "", "Prose above the table.", "", HEADER, SEPARATOR]
    for name, (notes, synced) in rows.items():
        lines.append(f"| `{name}` | `../up/{name}/` | up | {notes} | {synced} |")
    lines += ["", "## Re-sync procedure", ""]
    return "\n".join(lines) + "\n"


def _git(repo: Path, *args: str) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(  # nosec B603 - fixed argv, no shell
        ["git", *args], cwd=repo, text=True, capture_output=True, timeout=60
    )
    assert result.returncode == 0, f"git {' '.join(args)} failed: {result.stderr}"
    return result


def _commit(repo: Path, message: str) -> None:
    _git(repo, "add", "-A")
    _git(repo, "commit", "-q", "-m", message)


def _write(repo: Path, rel: str, text: str) -> None:
    path = repo / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


@pytest.fixture
def repo(tmp_path):
    """A throwaway repo laid out so the guard's ROOT resolves inside it.

    The guard computes ROOT from its own location (`parents[1]`) and runs git
    there, so driving it against a synthetic history means copying it to
    <tmp>/scripts/ -- not passing a flag.
    """
    repo = tmp_path / "consumer"
    (repo / "scripts").mkdir(parents=True)
    shutil.copy2(GUARD, repo / "scripts" / GUARD.name)
    _git(repo.parent, "init", "-q", "-b", "main", str(repo))
    _git(repo, "config", "user.email", "guard@test.invalid")
    _git(repo, "config", "user.name", "guard test")

    _write(
        repo, "docs/skill-sources.md", _ledger({"ask-colleague": ("Notes.", "2026-06-12 (1.7.0)")})
    )
    _write(repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\nv1\n")
    _write(repo, ".claude/skills/nodes-operator/SKILL.md", "first-party, not in the ledger\n")
    _write(repo, "README.md", "base\n")
    _commit(repo, "base")
    return repo


def run_guard(repo: Path, base: str = "HEAD~1", head: str = "HEAD"):
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [sys.executable, str(repo / "scripts" / GUARD.name), base, head],
        cwd=repo,
        text=True,
        capture_output=True,
        timeout=60,
    )


# --- the case that was broken: a re-vendor must be able to land --------------


def test_a_revendor_that_advances_the_ledger_passes(repo):
    _write(repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\nv2\n")
    _write(repo, ".claude/skills/ask-colleague/scripts/run.sh", "#!/bin/sh\necho v2\n")
    _write(
        repo,
        "docs/skill-sources.md",
        _ledger({"ask-colleague": ("Notes.", "2026-08-24 (1.63.0)")}),
    )
    _commit(repo, "re-vendor ask-colleague @ 1.63.0")

    result = run_guard(repo)
    assert result.returncode == 0, result.stderr
    assert "ask-colleague" in result.stdout


def test_a_first_vendoring_arrives_with_its_files(repo):
    """No row at base means the skill is new; its files come with the row."""
    _write(repo, ".claude/skills/newcomer/SKILL.md", "---\ntype: command\n---\nnew\n")
    _write(
        repo,
        "docs/skill-sources.md",
        _ledger(
            {
                "ask-colleague": ("Notes.", "2026-06-12 (1.7.0)"),
                "newcomer": ("Notes.", "2026-08-24 (0.1.0)"),
            }
        ),
    )
    _commit(repo, "vendor newcomer for the first time")

    result = run_guard(repo)
    assert result.returncode == 0, result.stderr


# --- the rule the guard actually exists to enforce ---------------------------


def test_a_local_patch_without_a_ledger_bump_is_refused(repo):
    _write(
        repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\nlocally hacked\n"
    )
    _commit(repo, "tweak the vendored skill in place")

    result = run_guard(repo)
    assert result.returncode == 1
    assert "ask-colleague" in result.stderr
    assert "Last synced" in result.stderr
    # The old message told the author to do the thing the guard rejected.
    assert "and re-vendor" not in result.stderr


def test_a_ledger_bump_for_one_skill_does_not_unlock_another(repo):
    """Guards against 'any ledger edit unlocks everything'."""
    _write(
        repo,
        "docs/skill-sources.md",
        _ledger(
            {
                "ask-colleague": ("Notes.", "2026-06-12 (1.7.0)"),
                "cicd": ("Notes.", "2026-08-24 (0.9.0)"),
            }
        ),
    )
    _write(repo, ".claude/skills/cicd/SKILL.md", "---\ntype: command\n---\ncicd\n")
    # cicd is legitimately re-vendored; ask-colleague is quietly patched too.
    _write(repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\nsmuggled\n")
    _commit(repo, "re-vendor cicd, and sneak an edit into ask-colleague")

    result = run_guard(repo)
    assert result.returncode == 1
    assert "ask-colleague" in result.stderr
    assert "cicd" not in result.stderr


def test_editing_the_notes_cell_is_not_a_resync(repo):
    """Prose editing is not a re-vendor -- only the sync cell counts."""
    _write(repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\nchanged\n")
    _write(
        repo,
        "docs/skill-sources.md",
        _ledger({"ask-colleague": ("Rewritten notes.", "2026-06-12 (1.7.0)")}),
    )
    _commit(repo, "reword the ledger notes and edit the skill")

    result = run_guard(repo)
    assert result.returncode == 1
    assert "ask-colleague" in result.stderr


# --- everything the guard must keep ignoring ---------------------------------


def test_changes_outside_the_vendored_tree_pass(repo):
    _write(repo, "README.md", "changed\n")
    _write(repo, "culture_nodes/thing.py", "x = 1\n")
    _commit(repo, "ordinary change")

    result = run_guard(repo)
    assert result.returncode == 0, result.stderr
    assert "none under" in result.stdout


def test_a_first_party_skill_is_not_guarded(repo):
    """.claude/skills/ is not all-vendored; the ledger declares the set."""
    _write(repo, ".claude/skills/nodes-operator/SKILL.md", "authored here, freely edited\n")
    _commit(repo, "edit a first-party skill")

    result = run_guard(repo)
    assert result.returncode == 0, result.stderr


def test_wrong_argument_count_is_an_environment_error(repo):
    result = subprocess.run(  # nosec B603 - fixed argv, no shell
        [sys.executable, str(repo / "scripts" / GUARD.name), "only-one"],
        cwd=repo,
        text=True,
        capture_output=True,
        timeout=60,
    )
    assert result.returncode == 2
    assert "usage:" in result.stderr


def test_an_unresolvable_range_is_reported_not_passed(repo):
    """'could not check' must never render as 'checked and it is clean'."""
    result = run_guard(repo, base="does-not-exist", head="HEAD")
    assert result.returncode == 2
    assert "could not diff" in result.stderr


# --- the work-tree half: an in-progress re-vendor must be performable --------


def _resynced_in_worktree(repo: Path) -> set[str]:
    """Call the guard's work-tree helper with its ROOT inside `repo`."""
    guard_path = str(repo / "scripts" / GUARD.name)
    program = (
        "import importlib.util,json;"
        f"spec=importlib.util.spec_from_file_location('g', {guard_path!r});"
        "m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);"
        "print(json.dumps(sorted(m.resynced_in_worktree())))"
    )
    result = subprocess.run(  # nosec B603 - fixed argv, no shell
        [sys.executable, "-c", program],
        cwd=repo,
        text=True,
        capture_output=True,
        timeout=60,
    )
    assert result.returncode == 0, result.stderr
    import json

    return set(json.loads(result.stdout))


def test_an_uncommitted_revendor_is_recognised(repo):
    _write(repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\nv2\n")
    _write(
        repo,
        "docs/skill-sources.md",
        _ledger({"ask-colleague": ("Notes.", "2026-08-24 (1.63.0)")}),
    )
    # Deliberately NOT committed -- this is the re-sync procedure mid-flight.
    assert _resynced_in_worktree(repo) == {"ask-colleague"}


def test_an_uncommitted_local_patch_is_not_exempt(repo):
    _write(repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\nhacked\n")
    assert _resynced_in_worktree(repo) == set()


# --- the bypasses qodo found on PR #213 --------------------------------------


def test_deleting_the_ledger_row_does_not_hide_edits_to_the_skill(repo):
    """Un-vendoring is legitimate, but it must be SEEN, not hide the edit.

    The protected set used to come from the HEAD ledger alone. Drop a skill's
    row and rewrite its files in one range and those paths matched no prefix,
    so the guard printed "none under N vendored skills" over a rewritten tree.
    The set is now the union of base and head, so the removal is reported.
    """
    _write(repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\nrewritten\n")
    _write(repo, "docs/skill-sources.md", _ledger({"cicd": ("Notes.", "2026-06-12 (1.0.0)")}))
    _commit(repo, "drop the ask-colleague row and rewrite its files")

    result = run_guard(repo)
    assert result.returncode == 0, result.stderr
    assert "un-vendored" in result.stdout
    assert "ask-colleague" in result.stdout
    # The old wording claimed nothing vendored was touched. It was.
    assert "none under" not in result.stdout


def test_deleting_the_row_still_does_not_unlock_a_different_skill(repo):
    both = {
        "ask-colleague": ("Notes.", "2026-06-12 (1.7.0)"),
        "cicd": ("Notes.", "2026-06-12 (1.0.0)"),
    }
    _write(repo, "docs/skill-sources.md", _ledger(both))
    _write(repo, ".claude/skills/cicd/SKILL.md", "---\ntype: command\n---\ncicd\n")
    _commit(repo, "add cicd as a second vendored skill")

    # Drop ask-colleague's row -- cicd's row is untouched -- and edit both.
    _write(repo, "docs/skill-sources.md", _ledger({"cicd": both["cicd"]}))
    _write(repo, ".claude/skills/ask-colleague/SKILL.md", "---\ntype: command\n---\ngone\n")
    _write(repo, ".claude/skills/cicd/SKILL.md", "---\ntype: command\n---\nsmuggled\n")
    _commit(repo, "un-vendor ask-colleague, and sneak an edit into cicd")

    result = run_guard(repo)
    assert result.returncode == 1
    assert "cicd" in result.stderr
    assert "ask-colleague" not in result.stderr


def test_no_ledger_at_either_end_is_an_environment_error_not_a_pass(repo):
    """A missing ledger must not silently fall back to the work tree's copy.

    `vendored_paths(head_ledger)` used to be called with an explicit None,
    which the default-argument check could not tell from "no argument", so the
    head revision's body was quietly replaced by whatever was on disk.
    """
    (repo / "docs" / "skill-sources.md").unlink()
    _commit(repo, "remove the ledger entirely")
    # Range base -> head where neither side is usable as a declaration.
    result = run_guard(repo, base="HEAD", head="HEAD")
    assert result.returncode == 2
    assert "no vendored skills declared" in result.stderr


def test_removing_a_row_in_the_work_tree_counts_as_a_ledger_change(repo):
    """The work-tree helper closes the same bypass main() does."""
    _write(repo, "docs/skill-sources.md", _ledger({"cicd": ("Notes.", "2026-06-12 (1.0.0)")}))
    assert "ask-colleague" in _resynced_in_worktree(repo)
