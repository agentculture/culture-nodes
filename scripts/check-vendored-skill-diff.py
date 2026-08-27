#!/usr/bin/env python3
"""Refuse LOCAL EDITS to skills declared vendored by docs/skill-sources.md.

A re-vendor is allowed. A local patch is not. Those are different operations
and this guard used to conflate them: it failed on *any* change under a
vendored skill, which made its own remediation line -- "update the source
repository and re-vendor" -- name the one operation it rejected (#212). As
written, a vendored skill could never be updated through a PR.

The ledger is what tells the two apart. docs/skill-sources.md carries one row
per vendored skill whose last cell records when that skill was last synced
from its upstream. A re-vendor advances that cell; a local edit does not. So
the rule this enforces is:

    files under .claude/skills/<name>/ may change in a commit range only if
    that range also advances <name>'s `Last synced` cell.

That keeps the ledger honest as a side effect -- the same shape as
scripts/check-zero-runtime-deps.sh, where the manifest is a promise and the
import list is the fact. Here the ledger is the promise and the diff is the
fact, and neither is trusted alone.
"""

from __future__ import annotations

import re
import subprocess  # nosec B404 - fixed argv, no shell
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SOURCES = ROOT / "docs" / "skill-sources.md"
SOURCES_REL = "docs/skill-sources.md"

# The ledger table's header. Both the skill set and the sync cells are read
# from the SAME walk of this table, so the two can never disagree about where
# it starts or ends.
TABLE_HEADER = "| Skill | Upstream |"


def _table_lines(text: str) -> list[str]:
    """The ledger table's data rows, header and separator excluded."""
    rows: list[str] = []
    in_table = False
    for line in text.splitlines():
        if line.startswith(TABLE_HEADER):
            in_table = True
            continue
        if in_table and not line.startswith("|"):
            break
        if not in_table or line.startswith("|---"):
            continue
        rows.append(line)
    return rows


def _row_name(line: str) -> str | None:
    match = re.match(r"\|\s*`([^`]+)`\s*\|", line)
    return match.group(1) if match else None


# A vendored skill's files live under one of these roots, per agent surface.
# `.claude/` is the Claude backend's kit; `.qwen/` is the qwen surface's, whose
# SKILL.md files are deliberately adapted but whose scripts/ bodies are
# byte-identical copies of the same vendored originals. A copy the guard does
# not know about is a copy that can be edited or go stale silently, which is
# what the ledger exists to prevent -- so both roots are protected by the same
# ledger row.
SKILL_ROOTS = (".claude/skills/", ".qwen/skills/")


def skill_prefixes(names: set[str]) -> set[str]:
    """Every guarded path prefix for the given skill names, across all roots."""
    return {f"{root}{name}/" for root in SKILL_ROOTS for name in names}


def skill_names(text: str) -> set[str]:
    """Every skill name the given ledger body declares vendored.

    Pure: it never touches disk, so main() can ask the question of a specific
    revision. An EXPLICIT `None` is not accepted here -- conflating "caller
    passed nothing" with "that revision genuinely has no ledger" is what let
    the head-revision body be silently replaced by the work tree's copy.
    """
    return {name for name in (_row_name(line) for line in _table_lines(text)) if name is not None}


def vendored_paths() -> set[str]:
    """Path prefixes of every skill the CHECKED-OUT ledger declares vendored.

    tests/test_open_issue.py imports this as the parser of record, so its
    no-argument, disk-reading shape is a contract. Revision-scoped callers use
    skill_names() instead.
    """
    names = skill_names(SOURCES.read_text(encoding="utf-8"))
    if not names:
        raise SystemExit(f"no vendored skills parsed from {SOURCES_REL}")
    return skill_prefixes(names)


def ledger_rows(text: str) -> dict[str, str]:
    """Map skill name -> its `Last synced` cell, from a skill-sources.md body.

    The sync cell is the row's LAST populated cell, e.g.
    `2026-08-24 (colleague 1.63.0, direct)`. A row whose Notes cell changes but
    whose sync cell does not is not a re-vendor -- it is prose editing -- so
    only this cell is compared.
    """
    rows: dict[str, str] = {}
    for line in _table_lines(text):
        name = _row_name(line)
        if name is None:
            continue
        cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
        rows[name] = cells[-1] if cells else ""
    return rows


def _git(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        ["git", *args],
        cwd=ROOT,
        text=True,
        capture_output=True,
    )


def _ledger_at(rev: str) -> str | None:
    """The ledger body as of `rev`, or None if it did not exist there."""
    shown = _git("show", f"{rev}:{SOURCES_REL}")
    return shown.stdout if shown.returncode == 0 else None


def resynced_in_worktree() -> set[str]:
    """Skills whose `Last synced` cell differs between HEAD and the work tree.

    The range check above reasons about commits. A re-vendor in progress is not
    committed yet, so a work-tree check that simply refuses modified vendored
    files would forbid performing the re-sync procedure locally at all -- the
    same defect as #212, one step earlier. tests/test_open_issue.py calls this
    to exempt an in-progress re-vendor, so the work tree and the commit range
    are judged by ONE rule with one implementation.
    """
    committed = _ledger_at("HEAD")
    if committed is None:
        return set()
    working = SOURCES.read_text(encoding="utf-8")
    head_rows, work_rows = ledger_rows(committed), ledger_rows(working)
    # Rows on EITHER side, so a removed row counts as a ledger change too --
    # otherwise deleting a row is a way to edit the skill's files unnoticed,
    # the same bypass main() closes by unioning base and head.
    return {
        name
        for name in set(head_rows) | set(work_rows)
        if head_rows.get(name) != work_rows.get(name)
    }


def _skill_of(path: str) -> str:
    # Every violating path starts with `<root>/skills/<name>/`.
    return path.split("/")[2]


def _root_of(path: str) -> str:
    for root in SKILL_ROOTS:
        if path.startswith(root):
            return root
    raise AssertionError(f"path outside every skill root: {path}")


def _existed_at(rev: str, prefix: str) -> bool:
    """Did any tracked file live under `prefix` at `rev`?"""
    listed = _git("ls-tree", "-r", "--name-only", rev, "--", prefix)
    return listed.returncode == 0 and bool(listed.stdout.strip())


def main() -> int:
    if len(sys.argv) != 3:
        print(f"usage: {Path(sys.argv[0]).name} <base-revision> <head-revision>", file=sys.stderr)
        return 2
    base, head = sys.argv[1:]

    diff = _git("diff", "--name-only", f"{base}...{head}")
    if diff.returncode != 0:
        print(f"error: could not diff {base}...{head}", file=sys.stderr)
        print(diff.stderr.strip(), file=sys.stderr)
        return 2
    changed = diff.stdout.splitlines()

    # The protected set is the UNION of what the ledger declared at BOTH ends
    # of the range. Deriving it from head alone left a bypass: drop a skill's
    # row and edit its files in one range, and those paths matched no prefix,
    # so the guard reported "none under N vendored skills" while the tree was
    # rewritten. Un-vendoring is a legitimate operation -- but it has to be
    # SEEN, not be the thing that hides the edit.
    base_ledger, head_ledger = _ledger_at(base), _ledger_at(head)
    base_rows = ledger_rows(base_ledger) if base_ledger is not None else {}
    head_rows = ledger_rows(head_ledger) if head_ledger is not None else {}
    prefixes = skill_prefixes(set(base_rows) | set(head_rows))
    if not prefixes:
        print(
            f"error: no vendored skills declared by {SOURCES_REL} at either {base} or {head}",
            file=sys.stderr,
        )
        print(
            f"hint: the ledger table under '{TABLE_HEADER}' is missing or empty.", file=sys.stderr
        )
        return 2

    touched = sorted(path for path in changed if any(path.startswith(p) for p in prefixes))
    if not touched:
        print(f"ok: {len(changed)} changed path(s), none under {len(prefixes)} vendored skills")
        return 0

    # A vendored skill's files changed. Whether that is a re-vendor or a local
    # edit is decided per skill, by its ledger row across the range.
    unsynced: dict[str, list[str]] = {}
    resynced: set[str] = set()
    unvendored: set[str] = set()
    first_copy: set[str] = set()
    for path in touched:
        skill = _skill_of(path)
        root = _root_of(path)
        if skill not in base_rows:
            # No row at base: this range is the skill's FIRST vendoring, and
            # its files arrive with it.
            resynced.add(skill)
        elif not _existed_at(base, f"{root}{skill}/"):
            # The row exists, but this ROOT had no copy of the skill at base:
            # a surface is gaining its first copy. That is a new copy, not an
            # edit to an existing one, so there is no sync to advance -- the
            # ledger row already declares the skill vendored and now governs
            # this root too. Byte-identity of any shared script bodies is a
            # separate check (tests/test_qwen_skill_surface.py).
            first_copy.add(f"{root}{skill}")
        elif skill not in head_rows:
            # The row was REMOVED. The skill stops being vendored, so its files
            # are free -- but say so, because the diff of the ledger is the only
            # other place that fact appears.
            unvendored.add(skill)
        elif base_rows[skill] != head_rows[skill]:
            resynced.add(skill)
        else:
            unsynced.setdefault(skill, []).append(path)

    if unsynced:
        print("vendored skill files changed without a ledger sync:", file=sys.stderr)
        for skill, paths in sorted(unsynced.items()):
            print(
                f"  {skill} — {len(paths)} file(s) changed, but its `Last synced` cell in "
                f"{SOURCES_REL} is unchanged ({base_rows[skill]}).",
                file=sys.stderr,
            )
            for path in paths:
                print(f"      {path}", file=sys.stderr)
        print(
            "hint: a re-vendor advances the skill's `Last synced` cell; a local edit to a "
            "vendored skill is not allowed at all.",
            file=sys.stderr,
        )
        print(
            f'      See the "Re-sync procedure" section of {SOURCES_REL}.',
            file=sys.stderr,
        )
        return 1

    notes = []
    if resynced:
        notes.append(f"re-vendored with a ledger sync: {', '.join(sorted(resynced))}")
    if unvendored:
        notes.append(f"un-vendored (ledger row removed): {', '.join(sorted(unvendored))}")
    if first_copy:
        notes.append(f"first copy under a new skill root: {', '.join(sorted(first_copy))}")
    print(f"ok: {len(changed)} changed path(s); " + "; ".join(notes))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
