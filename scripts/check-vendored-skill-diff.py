#!/usr/bin/env python3
"""Refuse changes to skills declared vendored by docs/skill-sources.md."""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SOURCES = ROOT / "docs" / "skill-sources.md"


def vendored_paths() -> set[str]:
    names: set[str] = set()
    in_table = False
    for line in SOURCES.read_text(encoding="utf-8").splitlines():
        if line.startswith("| Skill | Upstream |"):
            in_table = True
            continue
        if in_table and not line.startswith("|"):
            break
        if not in_table or line.startswith("|---"):
            continue
        match = re.match(r"\|\s*`([^`]+)`\s*\|", line)
        if match:
            names.add(f".claude/skills/{match.group(1)}/")
    if not names:
        raise SystemExit(f"no vendored skills parsed from {SOURCES.relative_to(ROOT)}")
    return names


def main() -> int:
    if len(sys.argv) != 3:
        print(f"usage: {Path(sys.argv[0]).name} <base-revision> <head-revision>", file=sys.stderr)
        return 2
    base, head = sys.argv[1:]
    changed = subprocess.run(
        ["git", "diff", "--name-only", f"{base}...{head}"],
        cwd=ROOT,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    ).stdout.splitlines()
    prefixes = vendored_paths()
    violations = sorted(path for path in changed if any(path.startswith(p) for p in prefixes))
    if violations:
        print("vendored skill files changed; update the source repository and re-vendor:", file=sys.stderr)
        print("\n".join(violations), file=sys.stderr)
        return 1
    print(f"ok: {len(changed)} changed path(s), none under {len(prefixes)} vendored skills")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
