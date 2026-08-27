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


def vendored_paths(text: str | None = None) -> set[str]:
    """Path prefixes of every skill the ledger declares vendored.

    tests/test_open_issue.py imports this as the parser of record and calls it
    with no arguments, so the no-argument form must keep reading the ledger
    from disk. `text` exists so main() can read one side of a commit range and
    derive the skill set and its sync cells from that single body.
    """
    body = SOURCES.read_text(encoding="utf-8") if text is None else text
    names = {name for name in (_row_name(line) for line in _table_lines(body)) if name is not None}
    if not names:
        raise SystemExit(f"no vendored skills parsed from {SOURCES_REL}")
    return {f".claude/skills/{name}/" for name in names}


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
    return {
        name for name in work_rows if name not in head_rows or head_rows[name] != work_rows[name]
    }


def _skill_of(path: str) -> str:
    # Every violating path starts with `.claude/skills/<name>/`.
    return path.split("/")[2]


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

    head_ledger = _ledger_at(head)
    prefixes = vendored_paths(head_ledger)
    touched = sorted(path for path in changed if any(path.startswith(p) for p in prefixes))
    if not touched:
        print(f"ok: {len(changed)} changed path(s), none under {len(prefixes)} vendored skills")
        return 0

    # A vendored skill's files changed. Whether that is a re-vendor or a local
    # edit is decided per skill, by its ledger row across the range.
    base_ledger = _ledger_at(base)
    base_rows = ledger_rows(base_ledger) if base_ledger is not None else {}
    head_rows = (
        ledger_rows(head_ledger)
        if head_ledger is not None
        else ledger_rows(SOURCES.read_text(encoding="utf-8"))
    )

    unsynced: dict[str, list[str]] = {}
    resynced: set[str] = set()
    for path in touched:
        skill = _skill_of(path)
        if skill not in base_rows:
            # No row at base: this range is the skill's FIRST vendoring, and
            # its files arrive with it.
            resynced.add(skill)
        elif base_rows[skill] != head_rows.get(skill):
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

    synced = ", ".join(sorted(resynced))
    print(f"ok: {len(changed)} changed path(s); re-vendored with a ledger sync: {synced}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
