#!/usr/bin/env python3
"""Disposition a freshly opened issue: append its two triage rows, regenerate.

Plan loop-closure-claude-codex, task t15. Every issue the loop opens turns the
next PR's `triage` lint step red until someone appends a row to
docs/triage/dispositions.csv, a row to docs/triage/issue-types.csv, and
regenerates docs/triage/open-issues.md -- one hand-turn per issue, and the kind
that is forgotten until CI says so. This helper does all three in one call so
`scripts/open-issue.sh --disposition "bucket|text|evidence"` can run it right
after the issue exists.

Why the CSV logic lives HERE and not in the wrapper: open-issue.sh is scheduled
for deletion when agentculture/agtag#19 lands (agtag absorbs template + type at
creation). Whatever replaces it will still need to disposition the issue it
opened, and that is this script -- one positional number, one `--type`, one
`--disposition`. Nothing about GitHub is known here beyond the open-issue list
read, which is borrowed from triage-report.py so the two cannot disagree.

Refusal contract (mirrors the wrapper's type validation): malformed input --
a missing field, an unknown bucket or type, a number already dispositioned --
is refused BY NAME with exit 2 and NOTHING is written. `--check-only` runs
exactly that validation without a number, so the wrapper can refuse before it
posts rather than leave an issue behind with no rows.

What the regenerated report does and does not refresh: the disposition table
(everything `triage-report.py --check` verifies) is rebuilt from the appended
CSV; the `## Issue types` section is preserved verbatim, because refreshing it
needs an org-level GitHub read (deviation d1, see triage-report.py). A plain
`python3 scripts/triage-report.py` run refreshes it when a token that can read
the org is available.

Usage:
  python3 scripts/triage-rows.py <number> --type Record \\
      --disposition "verify-then-close|what to do|where the evidence is" \\
      [--type-evidence "..."] [--issues-json PATH] [--triage-dir DIR] [--repo OWNER/REPO]
  python3 scripts/triage-rows.py --check-only --type Bug --disposition "bucket|text|evidence"
"""

from __future__ import annotations

import argparse
import csv
import importlib.util
import io
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_TRIAGE = ROOT / "docs" / "triage"
DEFAULT_REPO = "agentculture/culture-nodes"

DISPOSITION_FIELDS = ("issue", "bucket", "disposition", "evidence_pointer")
TYPE_FIELDS = ("issue", "type", "evidence_pointer")
SHAPE = "bucket|text|evidence"


class Refused(ValueError):
    """Malformed input. Named, exit 2, nothing written."""


def load_report_module():
    """triage-report.py is the reader and renderer of record; borrow, do not copy."""
    path = Path(__file__).resolve().with_name("triage-report.py")
    spec = importlib.util.spec_from_file_location("triage_report", path)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader  # nosec B101 - importlib contract, not input validation
    spec.loader.exec_module(module)
    return module


def parse_disposition(raw: str) -> tuple[str, str, str]:
    """Split `bucket|text|evidence`; every field required, exactly three."""
    parts = [part.strip() for part in raw.split("|")]
    if len(parts) != 3:
        raise Refused(
            f"--disposition must have exactly three '|'-separated fields ({SHAPE}); "
            f"got {len(parts)}: {raw!r}"
        )
    for name, value in zip(("bucket", "disposition", "evidence"), parts):
        if not value:
            raise Refused(f"--disposition is missing its {name} field ({SHAPE}): {raw!r}")
    return parts[0], parts[1], parts[2]


def read_types(path: Path) -> list[dict[str, str]]:
    with path.open(encoding="utf-8", newline="") as handle:
        rows = list(csv.DictReader(handle))
    if not rows or tuple(rows[0]) != TYPE_FIELDS:
        raise Refused(f"{path}: expected columns {list(TYPE_FIELDS)}")
    for line, row in enumerate(rows, 2):
        if None in row or any(value is None for value in row.values()):
            raise Refused(f"{path}:{line}: expected exactly {len(TYPE_FIELDS)} fields")
    return rows


def validate(
    report,
    triage: Path,
    number: int | None,
    type_name: str,
    raw_disposition: str,
) -> tuple[tuple[str, str, str], dict[int, dict[str, str]]]:
    """Everything that can be refused, refused before anything is written."""
    bucket, text, evidence = parse_disposition(raw_disposition)

    try:
        existing = report.dispositions(triage / "dispositions.csv")
    except report.DispositionTableError as exc:
        raise Refused(str(exc)) from exc
    # The buckets already in the table are the vocabulary. A new bucket is a
    # cycle decision made in the CSV by hand, never introduced by a typo here.
    buckets = sorted({row["bucket"] for row in existing.values()})
    if bucket not in buckets:
        raise Refused(f"unknown bucket {bucket!r}; the table's buckets are: {', '.join(buckets)}")

    types = read_types(triage / "issue-types.csv")
    known_types = sorted({row["type"] for row in types})
    if type_name not in known_types:
        raise Refused(
            f"unknown issue type {type_name!r}; issue-types.csv carries: {', '.join(known_types)}"
        )

    if number is not None:
        if number in existing:
            raise Refused(f"issue #{number} already has a disposition row")
        if any(row["issue"] == str(number) for row in types):
            raise Refused(f"issue #{number} already has an issue-types.csv row")
    return (bucket, text, evidence), existing


def append_row(path: Path, fields: list[str]) -> None:
    """Append one CSV row, quoting as the strict reader in triage-report.py needs."""
    current = path.read_text(encoding="utf-8")
    buffer = io.StringIO()
    csv.writer(buffer, lineterminator="\n").writerow(fields)
    prefix = "" if current.endswith("\n") else "\n"
    path.write_text(current + prefix + buffer.getvalue(), encoding="utf-8")


def regenerate(report, triage: Path, numbers: list[int]) -> None:
    """Rebuild the disposition table; keep the type section as it stands."""
    output = triage / "open-issues.md"
    table = report.render(numbers, report.dispositions(triage / "dispositions.csv"))
    current = output.read_text(encoding="utf-8") if output.exists() else ""
    heading = report.TYPES_HEADING
    if heading in current:
        content = table + "\n" + heading + current.split(heading, 1)[1]
    else:
        content = table
    output.write_text(content, encoding="utf-8")


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("number", nargs="?", type=int, help="the issue number just created")
    parser.add_argument("--type", required=True, dest="type_name", help="issue type name")
    parser.add_argument("--disposition", required=True, help=f"'{SHAPE}'")
    parser.add_argument(
        "--type-evidence",
        help="evidence_pointer for the issue-types.csv row (default: the disposition's evidence)",
    )
    parser.add_argument("--check-only", action="store_true", help="validate, write nothing")
    parser.add_argument("--repo", default=DEFAULT_REPO)
    parser.add_argument("--triage-dir", type=Path, default=DEFAULT_TRIAGE)
    parser.add_argument("--issues-json", type=Path, help="offline open-issue fixture")
    parser.add_argument("--backoff-seconds", type=float, default=None)
    args = parser.parse_args(argv)

    report = load_report_module()
    try:
        (bucket, text, evidence), _existing = validate(
            report, args.triage_dir, args.number, args.type_name, args.disposition
        )
    except (Refused, OSError) as exc:
        print(f"triage-rows: {exc}", file=sys.stderr)
        return 2
    if args.check_only:
        print("triage-rows: disposition is well-formed; nothing written")
        return 0
    if args.number is None:
        print("triage-rows: an issue number is required unless --check-only", file=sys.stderr)
        return 2

    # Read the open set BEFORE writing anything, so an unreachable GitHub is a
    # clean refusal (exit 2, retry the same command) rather than half a write.
    try:
        numbers = report.open_issue_numbers(
            args.repo, args.issues_json, backoff=args.backoff_seconds
        )
    except (report.GitHubUnreachable, OSError, ValueError, KeyError) as exc:
        print(f"triage-rows: could not read the open-issue set: {exc}", file=sys.stderr)
        return 2
    # The issue was just created: it is open whether or not the list has caught up.
    numbers = sorted(set(numbers) | {args.number})

    try:
        append_row(args.triage_dir / "dispositions.csv", [str(args.number), bucket, text, evidence])
        append_row(
            args.triage_dir / "issue-types.csv",
            [str(args.number), args.type_name, args.type_evidence or evidence],
        )
        regenerate(report, args.triage_dir, numbers)
    except (OSError, report.DispositionTableError) as exc:
        print(f"triage-rows: writing the triage rows failed: {exc}", file=sys.stderr)
        return 2

    print(
        f"triage-rows: #{args.number} -> {bucket} / {args.type_name}; "
        f"{args.triage_dir / 'open-issues.md'} regenerated ({len(numbers)} open issues)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
