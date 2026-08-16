#!/usr/bin/env python3
"""Count cycle issue movement from GitHub or an offline issue snapshot."""

from __future__ import annotations

import argparse
import csv
import json
import subprocess
import sys
from datetime import datetime
from pathlib import Path
from typing import NamedTuple

ROOT = Path(__file__).resolve().parents[1]
CYCLE_START_COMMIT = "1e6a532"
DEFAULT_DISPOSITIONS = ROOT / "docs" / "triage" / "dispositions.csv"
METRICS = ("opened", "closed", "delta", "undispositioned")


class Accounting(NamedTuple):
    opened: int
    closed: int
    delta: int
    undispositioned: tuple[int, ...]


def parse_timestamp(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def cycle_start(commit: str = CYCLE_START_COMMIT) -> datetime:
    command = ["git", "show", "-s", "--format=%cI", commit]
    raw = subprocess.run(
        command, cwd=ROOT, check=True, text=True, capture_output=True
    ).stdout.strip()
    return parse_timestamp(raw)


def load_issues(path: Path | None, repo: str = "agentculture/culture-nodes") -> list[dict]:
    if path:
        raw = path.read_text(encoding="utf-8")
    else:
        command = [
            "gh", "issue", "list", "--repo", repo, "--state", "all",
            "--limit", "1000", "--json", "number,state,createdAt,closedAt",
        ]
        raw = subprocess.run(command, check=True, text=True, capture_output=True).stdout
    data = json.loads(raw)
    if not isinstance(data, list):
        raise ValueError("issue JSON must contain a list")
    return data


def load_dispositions(path: Path) -> set[int]:
    with path.open(encoding="utf-8", newline="") as handle:
        rows = list(csv.DictReader(handle))
    if rows and "issue" not in rows[0]:
        raise ValueError(f"{path}: missing issue column")
    return {int(row["issue"]) for row in rows}


def account(issues: list[dict], dispositioned: set[int], start: datetime) -> Accounting:
    opened = [item for item in issues if parse_timestamp(item["createdAt"]) >= start]
    closed = [
        item for item in issues
        if item.get("closedAt") and parse_timestamp(item["closedAt"]) >= start
    ]
    undispositioned = tuple(sorted(
        int(item["number"])
        for item in opened
        if item["state"].upper() == "OPEN" and int(item["number"]) not in dispositioned
    ))
    return Accounting(len(opened), len(closed), len(closed) - len(opened), undispositioned)


def metric_value(result: Accounting, metric: str) -> int:
    if metric == "undispositioned":
        return len(result.undispositioned)
    return int(getattr(result, metric))


def render(result: Accounting, command: str = "python3 scripts/cycle-accounting.py") -> str:
    return "\n".join([
        f"Issues opened during cycle: {result.opened}",
        f"  query: {command} --metric opened",
        f"Issues closed during cycle: {result.closed}",
        f"  query: {command} --metric closed",
        f"Closed minus opened (delta): {result.delta}",
        f"  query: {command} --metric delta",
        "Opened-by-cycle issues undispositioned at cycle close: "
        f"{len(result.undispositioned)}",
        f"  query: {command} --metric undispositioned",
    ])


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", default="agentculture/culture-nodes")
    parser.add_argument("--issues-json", type=Path, help="offline fixture in gh JSON shape")
    parser.add_argument("--dispositions", type=Path, default=DEFAULT_DISPOSITIONS)
    parser.add_argument("--metric", choices=METRICS)
    args = parser.parse_args()

    try:
        start = cycle_start()
        result = account(
            load_issues(args.issues_json, args.repo),
            load_dispositions(args.dispositions),
            start,
        )
    except (
        KeyError,
        OSError,
        TypeError,
        ValueError,
        json.JSONDecodeError,
        subprocess.CalledProcessError,
    ) as exc:
        print(f"cycle-accounting: {exc}", file=sys.stderr)
        return 2

    if args.metric:
        print(metric_value(result, args.metric))
    else:
        source = (
            f"--issues-json {args.issues_json}"
            if args.issues_json else f"--repo {args.repo}"
        )
        print(
            f"Cycle starts at {start.isoformat()} from commit {CYCLE_START_COMMIT}'s "
            "committer timestamp."
        )
        print(render(result, f"python3 scripts/cycle-accounting.py {source}"))
        if result.undispositioned:
            print(
                "Undispositioned issue numbers: "
                + ", ".join(f"#{number}" for number in result.undispositioned)
            )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
