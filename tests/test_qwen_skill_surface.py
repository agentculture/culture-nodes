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


def _qwen_scripts() -> list[Path]:
    return sorted(p for p in QWEN_SKILLS.rglob("*.sh") if p.is_file())


def test_the_qwen_surface_is_tracked_at_all():
    assert QWEN_SKILLS.is_dir(), ".qwen/skills is missing"
    assert (ROOT / ".qwen" / "settings.json").is_file()


@pytest.mark.parametrize("script", _qwen_scripts(), ids=lambda p: str(p.relative_to(QWEN_SKILLS)))
def test_a_vendored_script_body_is_byte_identical_across_surfaces(script):
    """A re-vendor that updates one surface and not the other is a failure."""
    twin = CLAUDE_SKILLS / script.relative_to(QWEN_SKILLS)
    assert twin.is_file(), f"{script} has no .claude/skills counterpart"
    assert script.read_bytes() == twin.read_bytes(), (
        f"{script.relative_to(ROOT)} has drifted from {twin.relative_to(ROOT)}; "
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
