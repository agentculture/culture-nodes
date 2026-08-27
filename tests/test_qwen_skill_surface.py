"""The `.qwen/` agent surface is tracked, and its vendored copies cannot drift.

`.qwen/` is the qwen backend's skill kit, sitting beside `.claude/`. The two
are NOT the same thing and must not be conflated:

  - `SKILL.md` is deliberately ADAPTED per surface. The qwen copies carry a
    `<!-- lineage: ... -->` comment naming the chain they came down
    (claude -> colleague -> qwen) and the surface adaptation they applied, so
    a reader can tell an intentional divergence from a stale copy.
  - `scripts/*.sh` are NOT adapted. They are byte-identical copies of the same
    vendored bodies under `.claude/skills/`, and `cite, don't import` says a
    vendored script body is never edited downstream.

The second copy is the risk this file exists for. Re-vendoring a skill
advances `.claude/skills/<name>/` and the ledger row; nothing makes the
`.qwen/` copy follow, so it goes stale in silence. Byte-identity is the fact,
checked here rather than trusted.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
QWEN_SKILLS = ROOT / ".qwen" / "skills"
CLAUDE_SKILLS = ROOT / ".claude" / "skills"


# Scripts the qwen surface deliberately does NOT carry.
#
# All three are thin wrappers whose entire job is to resolve the devague CLI
# portably and forward every move to it verbatim. The qwen surface calls
# `devague` straight through its shell tool -- which is exactly what each of
# these skills' `<!-- lineage: ... -->` comment records as its surface
# adaptation -- so the wrapper has nothing left to do.
#
# The set is EXPLICIT rather than inferred, so the omissions stay countable and
# reviewable: a NEW one-sided absence fails, and a recorded one that quietly
# comes back fails too. It is also, in miniature, the shape issue #218 argues
# for generally -- logic in the CLI, the skill instructing rather than
# implementing.
DELIBERATELY_ABSENT_FROM_QWEN = frozenset(
    {
        "think/scripts/think.sh",
        "spec-to-plan/scripts/spec-to-plan.sh",
        "assign-to-workforce/scripts/assign-to-workforce.sh",
    }
)


def _shared_script_paths() -> list[str]:
    """Every `scripts/` path either surface carries, for skills qwen has.

    The UNION matters. Enumerating only the qwen side meant a script added on
    the claude side by a re-vendor -- or a qwen script deleted during one --
    had no case at all and passed by not being looked at, which is exactly the
    one-surface stale copy this file exists to catch.

    Scoped to skills present under `.qwen/skills/`, so a skill the qwen surface
    deliberately does not carry stays absent rather than being demanded here.
    Covers every file under `scripts/`, not just `*.sh`: `communicate` ships
    vendored `scripts/templates/*.md` with the same never-edit-downstream rule.
    """
    relative: set[str] = set()
    for skill in sorted(p.name for p in QWEN_SKILLS.iterdir() if p.is_dir()):
        for root in (QWEN_SKILLS, CLAUDE_SKILLS):
            scripts = root / skill / "scripts"
            if scripts.is_dir():
                relative |= {str(f.relative_to(root)) for f in scripts.rglob("*") if f.is_file()}
    return sorted(relative - DELIBERATELY_ABSENT_FROM_QWEN)


def test_the_qwen_surface_is_tracked_at_all():
    assert QWEN_SKILLS.is_dir(), ".qwen/skills is missing"
    assert (ROOT / ".qwen" / "settings.json").is_file()


@pytest.mark.parametrize("relative", _shared_script_paths())
def test_a_vendored_script_body_is_byte_identical_across_surfaces(relative):
    """A re-vendor that updates one surface and not the other is a failure.

    Both directions: a file missing from EITHER root is a failure, because a
    one-sided add and a one-sided delete are both the copies drifting apart.
    """
    qwen, claude = QWEN_SKILLS / relative, CLAUDE_SKILLS / relative
    assert qwen.is_file(), (
        f".qwen/skills/{relative} is missing while .claude/skills/{relative} exists; "
        "re-vendor both surfaces together"
    )
    assert claude.is_file(), (
        f".claude/skills/{relative} is missing while .qwen/skills/{relative} exists; "
        "a qwen-only script body has no vendored origin"
    )
    assert qwen.read_bytes() == claude.read_bytes(), (
        f".qwen/skills/{relative} has drifted from .claude/skills/{relative}; "
        "re-vendor both surfaces from the upstream, never edit one in place"
    )


def test_every_qwen_skill_records_the_lineage_it_was_adapted_from():
    """An adapted copy without a lineage note is indistinguishable from a stale one."""
    missing = [
        p.parent.name
        for p in sorted(QWEN_SKILLS.glob("*/SKILL.md"))
        if "lineage:" not in p.read_text(encoding="utf-8")
    ]
    assert missing == [], f"qwen SKILL.md without a lineage comment: {missing}"


def test_no_qwen_skill_carries_a_duplicate_top_level_heading():
    """The stray `# <name>` above the real title was a generation artifact.

    Seven of the nine files shipped with it; markdownlint flags it as MD025,
    which is how it was found. Pinned so a regenerated surface cannot bring it
    back unnoticed.
    """
    offenders = []
    for path in sorted(QWEN_SKILLS.glob("*/SKILL.md")):
        headings, fenced = [], False
        for line in path.read_text(encoding="utf-8").splitlines():
            if line.startswith("```"):
                fenced = not fenced
            elif not fenced and line.startswith("# "):
                headings.append(line)
        if len(headings) > 1:
            offenders.append((path.parent.name, headings))
    assert offenders == [], f"multiple H1s: {offenders}"


def test_the_settings_file_is_valid_json_and_declares_permissions():
    data = json.loads((ROOT / ".qwen" / "settings.json").read_text(encoding="utf-8"))
    assert isinstance(data["permissions"]["allow"], list)


def test_every_recorded_absence_is_still_absent():
    """A stale exemption is an exemption that hides a real drift.

    If a qwen copy of one of these wrappers appears, the reason for the
    exemption no longer holds and the file should be byte-compared like any
    other -- so the record must be deleted, not left to silently excuse it.
    """
    resurrected = [
        rel for rel in sorted(DELIBERATELY_ABSENT_FROM_QWEN) if (QWEN_SKILLS / rel).exists()
    ]
    assert resurrected == [], (
        f"recorded as deliberately absent from the qwen surface, but present: {resurrected}; "
        "remove them from DELIBERATELY_ABSENT_FROM_QWEN so they are byte-compared"
    )


def test_every_recorded_absence_names_a_real_claude_side_script():
    """The record must describe reality on the side that does have the file."""
    missing = [
        rel for rel in sorted(DELIBERATELY_ABSENT_FROM_QWEN) if not (CLAUDE_SKILLS / rel).is_file()
    ]
    assert missing == [], f"recorded absence names no .claude/skills script: {missing}"
