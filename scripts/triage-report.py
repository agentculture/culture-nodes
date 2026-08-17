#!/usr/bin/env python3
"""Render the open-issue triage table and reject missing dispositions.

Two dimensions, not one. The six BUCKETS are the cycle's disposition of an
issue and come from docs/triage/dispositions.csv. The issue TYPE is a separate,
live fact read from GitHub -- what kind of thing the issue is (Task, Bug,
Feature, Record). Neither replaces the other, and the type is deliberately not
a column in dispositions.csv: it is read at run time so the table cannot claim
a type the repository no longer carries.
"""

from __future__ import annotations

import argparse
import csv
import json
import subprocess
import sys
import time
from datetime import datetime
from pathlib import Path

# GitHub answers 503 often enough to matter: this check ran six times in one
# cycle and failed twice on a transient API error, once turning a PR red for a
# reason unrelated to its change. A retry is what actually stops that.
GH_ATTEMPTS = 4
GH_BACKOFF_SECONDS = 3


class GitHubUnreachable(RuntimeError):
    """The open-issue set could not be READ. Distinct from a stale table.

    Folding the two together is the same defect scripts/lint-all.sh was just
    corrected for: "could not check" must not render as "checked and it is
    wrong". This one exits 2 -- the code merge-gate.py uses for
    measurement_incomplete and _errors.py reserves for an environment error.
    """

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_TABLE = ROOT / "docs" / "triage" / "dispositions.csv"
DEFAULT_OUTPUT = ROOT / "docs" / "triage" / "open-issues.md"
DEFAULT_ORG = "agentculture"

# The date this repo started assigning issue types. Every per-type count names
# it, because a type cut taken over a backlog that predates adoption is a
# partial reading by construction -- 46 untyped issues are not 46 typeless
# ones, they are 46 nobody has typed yet. Printing the date beside the count is
# what stops the cut being read as complete.
TYPING_BEGAN = "2026-08-17"

# The boundary "closed since the previous cycle" resolves against, matching the
# commit scripts/cycle-accounting.py uses. Overridable with --closed-since.
PREVIOUS_CYCLE_COMMIT = "1e6a532"

NO_TYPE = "(no type)"

ORG_TYPES_QUERY = """
query($org: String!) {
  organization(login: $org) {
    issueTypes(first: 20) { nodes { name isEnabled } }
  }
}
"""

# One query per state. The state is a GraphQL enum, so it is substituted from
# the fixed map below and never from anything a caller supplies.
ISSUES_QUERY = """
query($owner: String!, $name: String!, $cursor: String, $since: DateTime) {
  repository(owner: $owner, name: $name) {
    issues(states: %s, first: 100, after: $cursor, filterBy: {since: $since}) {
      pageInfo { hasNextPage endCursor }
      nodes { number closedAt issueType { name } }
    }
  }
}
"""
ISSUE_STATES = {"open": "OPEN", "closed": "CLOSED"}

BUCKETS = {
    "verify-then-close",
    "operator-lane enablers",
    "bug tail",
    "finish work",
    "owner decisions",
    "large bets",
}


def invoke_gh(command: list[str]) -> tuple[int, str, str]:
    """The one process boundary. Tests substitute this callable."""
    proc = subprocess.run(command, check=False, text=True, capture_output=True)
    return proc.returncode, proc.stdout, proc.stderr


def run_gh(command: list[str], invoke=None, backoff: float = GH_BACKOFF_SECONDS) -> str:
    """Run one `gh` command with the retry, or raise GitHubUnreachable.

    Every GitHub read in this script goes through here -- the open-issue set and
    the type read alike -- so there is exactly one retry policy and exactly one
    way to say "could not measure".
    """
    invoke = invoke or invoke_gh
    last = ""
    for attempt in range(1, GH_ATTEMPTS + 1):
        code, stdout, stderr = invoke(command)
        if code == 0:
            return stdout
        tail = (stderr or stdout).strip().splitlines()[-1:] or [""]
        last = tail[0]
        if attempt < GH_ATTEMPTS:
            print(
                f"triage-report: gh failed ({last}); retry {attempt}/{GH_ATTEMPTS - 1}",
                file=sys.stderr,
            )
            if backoff:
                time.sleep(backoff * attempt)
    raise GitHubUnreachable(
        f"could not read from GitHub after {GH_ATTEMPTS} attempts: {last}"
    )


def graphql_command(query: str, variables: dict[str, str]) -> list[str]:
    command = ["gh", "api", "graphql", "-f", f"query={query}"]
    for key in sorted(variables):
        command += ["-f", f"{key}={variables[key]}"]
    return command


def run_graphql(query: str, variables: dict[str, str], invoke=None, backoff=GH_BACKOFF_SECONDS):
    raw = run_gh(graphql_command(query, variables), invoke=invoke, backoff=backoff)
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise GitHubUnreachable(f"gh returned non-JSON: {exc}") from exc
    if payload.get("errors"):
        message = "; ".join(str(item.get("message")) for item in payload["errors"])
        # A forbidden read is still an absence of a reading, not a finding.
        raise GitHubUnreachable(f"GraphQL error: {message}")
    return payload.get("data") or {}


def org_type_names(org: str, invoke=None, backoff=GH_BACKOFF_SECONDS) -> list[str]:
    """The org's enabled issue-type names, resolved at run time.

    Hard-coding them would make an unknown name count zero silently, which is
    the failure mode the search `type:` qualifier already has.
    """
    data = run_graphql(ORG_TYPES_QUERY, {"org": org}, invoke=invoke, backoff=backoff)
    nodes = (data.get("organization") or {}).get("issueTypes", {}).get("nodes", [])
    return [node["name"] for node in nodes if node.get("isEnabled", True)]


def issue_types(
    repo: str,
    state: str,
    invoke=None,
    since: str | None = None,
    backoff: float = GH_BACKOFF_SECONDS,
) -> list[dict]:
    """THE type read: one function, per-issue GraphQL, no search qualifier.

    Returns [{"number": int, "type": str | None, "closedAt": str | None}, ...].

    Isolated on purpose: when `gitculture issue list --json issueType` exists
    (agentculture/gitculture-cli#17), replacing this body is the whole change.
    It must stay GraphQL-or-successor and never become a search query -- search
    fails open on an unknown type name and its index lags writes, so it can
    report a confident zero for a type that exists.
    """
    owner, _, name = repo.partition("/")
    query = ISSUES_QUERY % ISSUE_STATES[state]
    records: list[dict] = []
    cursor: str | None = None
    while True:
        variables = {"owner": owner, "name": name}
        if cursor:
            variables["cursor"] = cursor
        if since:
            variables["since"] = since
        data = run_graphql(query, variables, invoke=invoke, backoff=backoff)
        issues = (data.get("repository") or {}).get("issues", {})
        for node in issues.get("nodes", []):
            node_type = node.get("issueType") or {}
            records.append(
                {
                    "number": int(node["number"]),
                    "type": node_type.get("name"),
                    "closedAt": node.get("closedAt"),
                }
            )
        page = issues.get("pageInfo") or {}
        if not page.get("hasNextPage"):
            return records
        cursor = page.get("endCursor")


class UnknownIssueType(ValueError):
    """An issue carries a type the org does not define. A finding, exit 1."""


def count_by_type(records: list[dict], known: list[str]) -> list[tuple[str, int]]:
    """Count records per type, refusing a name the org does not define."""
    counts: dict[str, int] = {name: 0 for name in known}
    counts[NO_TYPE] = 0
    for record in records:
        name = record.get("type") or NO_TYPE
        if name not in counts:
            raise UnknownIssueType(
                f"issue #{record.get('number')} carries type {name!r}, which is not one of "
                f"the org's enabled types ({', '.join(known)}); refusing to count it as zero"
            )
        counts[name] += 1
    return [(name, counts[name]) for name in [*known, NO_TYPE]]


def previous_cycle_start(commit: str = PREVIOUS_CYCLE_COMMIT) -> str:
    """The 'previous cycle' boundary: the commit date of the cycle-start commit."""
    command = ["git", "show", "-s", "--format=%cI", commit]
    proc = subprocess.run(
        command, cwd=ROOT, check=False, text=True, capture_output=True
    )
    if proc.returncode != 0:
        raise GitHubUnreachable(
            f"could not resolve the previous-cycle boundary from {commit}: "
            f"{proc.stderr.strip()}"
        )
    return proc.stdout.strip()


def open_issue_numbers(repo: str, issues_json: Path | None, invoke=None, backoff=None) -> list[int]:
    if issues_json:
        raw = issues_json.read_text(encoding="utf-8")
    else:
        command = [
            "gh", "issue", "list", "--repo", repo, "--state", "open",
            "--limit", "1000", "--json", "number",
        ]
        kwargs = {} if backoff is None else {"backoff": backoff}
        raw = run_gh(command, invoke=invoke, **kwargs)
    data = json.loads(raw)
    return sorted({int(item["number"]) for item in data})


def dispositions(path: Path) -> dict[int, dict[str, str]]:
    with path.open(encoding="utf-8", newline="") as handle:
        rows = list(csv.DictReader(handle))
    required = {"issue", "bucket", "disposition", "evidence_pointer"}
    if not rows or set(rows[0]) != required:
        raise ValueError(f"{path}: expected columns {sorted(required)}")
    result: dict[int, dict[str, str]] = {}
    for line, row in enumerate(rows, 2):
        issue = int(row["issue"])
        if issue in result:
            raise ValueError(f"{path}:{line}: duplicate issue #{issue}")
        if row["bucket"] not in BUCKETS:
            raise ValueError(f"{path}:{line}: unknown bucket {row['bucket']!r}")
        if not row["disposition"].strip() or not row["evidence_pointer"].strip():
            raise ValueError(f"{path}:{line}: disposition and evidence pointer are required")
        result[issue] = row
    return result


def parse_timestamp(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def closed_since(records: list[dict], since: str) -> list[dict]:
    boundary = parse_timestamp(since)
    return [
        record
        for record in records
        if record.get("closedAt") and parse_timestamp(record["closedAt"]) >= boundary
    ]


def render_type_block(title: str, records: list[dict], known: list[str]) -> list[str]:
    lines = [
        f"### {title}",
        "",
        "| Type | Issues | Typing began |",
        "|---|---:|---|",
    ]
    for name, count in count_by_type(records, known):
        lines.append(f"| {name} | {count} | {TYPING_BEGAN} |")
    lines.append("")
    return lines


def render_types(
    open_records: list[dict],
    closed_records: list[dict],
    known: list[str],
    since: str,
) -> str:
    lines = [
        "## Issue types",
        "",
        f"Types are read per-issue from GitHub at render time, never from the "
        f"search `type:` qualifier (it fails open on an unknown name and lags "
        f"writes). Typing began {TYPING_BEGAN}: every count below is a cut over "
        f"a backlog that predates adoption, so an untyped issue means nobody has "
        f"typed it yet, not that it has no kind.",
        "",
    ]
    lines += render_type_block("Open issues by type", open_records, known)
    lines += render_type_block(
        f"Issues closed since {since[:10]} by type", closed_records, known
    )
    return "\n".join(lines)


def render(numbers: list[int], rows: dict[int, dict[str, str]]) -> str:
    lines = [
        "# Open-issue triage",
        "",
        "Generated by `python3 scripts/triage-report.py`. Do not edit this file by hand.",
        "",
        "| Issue | Bucket | Disposition | Evidence pointer |",
        "|---:|---|---|---|",
    ]
    for number in numbers:
        row = rows[number]
        values = [row[key].replace("|", "\\|") for key in ("bucket", "disposition", "evidence_pointer")]
        lines.append(f"| #{number} | {values[0]} | {values[1]} | {values[2]} |")
    lines.extend(["", f"Open issues with dispositions: {len(numbers)}", ""])
    return "\n".join(lines)


def main(argv=None, invoke=None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", default="agentculture/culture-nodes")
    parser.add_argument("--org", default=DEFAULT_ORG)
    parser.add_argument("--table", type=Path, default=DEFAULT_TABLE)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--issues-json", type=Path, help="offline fixture in gh JSON shape")
    parser.add_argument(
        "--closed-since",
        help="ISO timestamp for the closed-by-type block (default: the previous cycle)",
    )
    parser.add_argument("--backoff-seconds", type=float, default=GH_BACKOFF_SECONDS)
    parser.add_argument("--check", action="store_true", help="require output to be up to date")
    args = parser.parse_args(argv)
    backoff = args.backoff_seconds

    try:
        numbers = open_issue_numbers(args.repo, args.issues_json, invoke=invoke, backoff=backoff)
        rows = dispositions(args.table)
        since = args.closed_since or previous_cycle_start()
        known = org_type_names(args.org, invoke=invoke, backoff=backoff)
        open_records = issue_types(args.repo, "open", invoke=invoke, backoff=backoff)
        closed_records = closed_since(
            issue_types(args.repo, "closed", invoke=invoke, since=since, backoff=backoff),
            since,
        )
    except (
        GitHubUnreachable,
        KeyError,
        OSError,
        TypeError,
        ValueError,
        json.JSONDecodeError,
        subprocess.CalledProcessError,
    ) as exc:
        print(f"triage-report: {exc}", file=sys.stderr)
        # 2 == could not measure. A stale table or a missing disposition is a
        # finding and returns 1 further down; this is the absence of a reading.
        return 2

    missing = sorted(set(numbers) - set(rows))
    if missing:
        print("triage-report: open issues have no disposition: " + ", ".join(f"#{n}" for n in missing), file=sys.stderr)
        return 1

    try:
        types_block = render_types(open_records, closed_records, known, since)
    except UnknownIssueType as exc:
        # A finding, not a failed measurement: the read succeeded and returned a
        # name the org does not define. Counting it as zero is what search does.
        print(f"triage-report: {exc}", file=sys.stderr)
        return 1

    content = render(numbers, rows) + "\n" + types_block
    if args.check:
        current = args.output.read_text(encoding="utf-8") if args.output.exists() else ""
        if current != content:
            print(f"triage-report: {args.output} is stale; regenerate it", file=sys.stderr)
            return 1
    else:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(content, encoding="utf-8")
    print(f"triage-report: {len(numbers)} open issues, all dispositioned; output {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
