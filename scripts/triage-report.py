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

# The heading that begins the type section. `--check` compares everything
# BEFORE it and nothing after, so this string is a contract, not a label.
TYPES_HEADING = "## Issue types"

ORG_TYPES_QUERY = """
query($org: String!) {
  organization(login: $org) {
    issueTypes(first: 100) {
      pageInfo { hasNextPage }
      nodes { name isEnabled }
    }
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


def run_gh(
    command: list[str],
    invoke=None,
    backoff: float = GH_BACKOFF_SECONDS,
    what: str = "GitHub",
) -> str:
    """Run one `gh` command with the retry, or raise GitHubUnreachable.

    Every GitHub read in this script goes through here -- the open-issue set,
    the org's type menu and the per-issue types alike -- so there is exactly one
    retry policy and exactly one way to say "could not measure".

    `what` names the thing being read, and it is not decoration. The reads need
    different privileges: the issue reads are repository-scoped, while the org
    type menu is an ORGANISATION object that a repository-scoped token (every
    Actions GITHUB_TOKEN is one) cannot see at all. An error that says only
    "could not read from GitHub" makes those two indistinguishable, which cost
    a CI round-trip to tell apart.
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
    raise GitHubUnreachable(f"could not read {what} after {GH_ATTEMPTS} attempts: {last}")


def graphql_command(query: str, variables: dict[str, str]) -> list[str]:
    command = ["gh", "api", "graphql", "-f", f"query={query}"]
    for key in sorted(variables):
        command += ["-f", f"{key}={variables[key]}"]
    return command


def run_graphql(
    query: str,
    variables: dict[str, str],
    invoke=None,
    backoff=GH_BACKOFF_SECONDS,
    what: str = "GitHub",
):
    raw = run_gh(graphql_command(query, variables), invoke=invoke, backoff=backoff, what=what)
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
    data = run_graphql(
        ORG_TYPES_QUERY,
        {"org": org},
        invoke=invoke,
        backoff=backoff,
        what=f"the {org} org's issue-type menu (an ORGANISATION read: a "
        "repository-scoped token such as an Actions GITHUB_TOKEN cannot do it)",
    )
    menu = (data.get("organization") or {}).get("issueTypes", {})
    # A menu read only in part is worse than no menu: a type that exists but
    # fell off page one would be reported as unknown, and this function's whole
    # job is to make "unknown" mean something.
    if (menu.get("pageInfo") or {}).get("hasNextPage"):
        raise GitHubUnreachable(
            f"the {org} org defines more issue types than one page returns; "
            "the menu was not read in full, so no count can be trusted"
        )
    nodes = menu.get("nodes", [])
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
        data = run_graphql(
            query,
            variables,
            invoke=invoke,
            backoff=backoff,
            what=f"the {state} issues of {repo} and their types",
        )
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


class DispositionTableError(ValueError):
    """The disposition table is malformed. A finding, exit 1."""


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
    proc = subprocess.run(command, cwd=ROOT, check=False, text=True, capture_output=True)
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
            "gh",
            "issue",
            "list",
            "--repo",
            repo,
            "--state",
            "open",
            "--limit",
            "1000",
            "--json",
            "number",
        ]
        kwargs = {} if backoff is None else {"backoff": backoff}
        raw = run_gh(command, invoke=invoke, what=f"the open-issue set of {repo}", **kwargs)
    data = json.loads(raw)
    return sorted({int(item["number"]) for item in data})


def dispositions(path: Path) -> dict[int, dict[str, str]]:
    with path.open(encoding="utf-8", newline="") as handle:
        rows = list(csv.DictReader(handle))
    required = {"issue", "bucket", "disposition", "evidence_pointer"}
    if not rows or set(rows[0]) != required:
        raise DispositionTableError(f"{path}: expected columns {sorted(required)}")
    result: dict[int, dict[str, str]] = {}
    for line, row in enumerate(rows, 2):
        # A row with the wrong field count is a REFUSAL, not a warning. Only the
        # header was checked before, so an unquoted comma inside a disposition
        # silently shifted every later field left -- the evidence pointer became
        # a fragment of the disposition, the tail was dropped under DictReader's
        # restkey, and the generated table rendered a plausible-looking wrong
        # row that --check then approved. A malformed row must not be able to
        # produce an accurate-looking disposition (#215).
        if None in row or any(value is None for value in row.values()):
            raise DispositionTableError(
                f"{path}:{line}: expected exactly {len(required)} fields; a value "
                "containing a comma must be quoted"
            )
        issue = int(row["issue"])
        if issue in result:
            raise DispositionTableError(f"{path}:{line}: duplicate issue #{issue}")
        if row["bucket"] not in BUCKETS:
            raise DispositionTableError(f"{path}:{line}: unknown bucket {row['bucket']!r}")
        if not row["disposition"].strip() or not row["evidence_pointer"].strip():
            raise DispositionTableError(
                f"{path}:{line}: disposition and evidence pointer are required"
            )
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
        TYPES_HEADING,
        "",
        f"Types are read per-issue from GitHub at render time, never from the "
        f"search `type:` qualifier (it fails open on an unknown name and lags "
        f"writes). Typing began {TYPING_BEGAN}: every count below is a cut over "
        f"a backlog that predates adoption, so an untyped issue means nobody has "
        f"typed it yet, not that it has no kind.",
        "",
    ]
    lines += render_type_block("Open issues by type", open_records, known)
    lines += render_type_block(f"Issues closed since {since[:10]} by type", closed_records, known)
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
        values = [
            row[key].replace("|", "\\|") for key in ("bucket", "disposition", "evidence_pointer")
        ]
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
        # --check deliberately reads NO types. The org's issue-type menu is an
        # ORGANISATION object, and CI's GITHUB_TOKEN is repository-scoped, so
        # the validating read cannot run there at all -- proven in CI, not
        # assumed. Rather than let the guard fail as "could not measure" on
        # every run, --check verifies the half it can verify: the disposition
        # table. The type blocks are regenerated by a plain run, where a token
        # that can read the org is available.
        #
        # The cost is stated rather than hidden: a stale type block is NOT
        # caught by CI. Recorded as deviation d1 against claims c16/h15/c26.
        #
        # `since` is computed inside this branch for the same reason, and it is
        # not a tidiness point: previous_cycle_start() resolves a hardcoded
        # commit through `git show`, which FAILS on a shallow checkout.
        # publish.yml checks out shallow, so computing it unconditionally made
        # --check depend on git history it does not use. Only the closed-issue
        # type block needs a boundary date.
        since = ""
        known, open_records, closed_records = [], [], []
        if not args.check:
            since = args.closed_since or previous_cycle_start()
            known = org_type_names(args.org, invoke=invoke, backoff=backoff)
            open_records = issue_types(args.repo, "open", invoke=invoke, backoff=backoff)
            closed_records = closed_since(
                issue_types(args.repo, "closed", invoke=invoke, since=since, backoff=backoff),
                since,
            )
    except DispositionTableError as exc:
        print(f"triage-report: {exc}", file=sys.stderr)
        return 1
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
        print(
            "triage-report: open issues have no disposition: "
            + ", ".join(f"#{n}" for n in missing),
            file=sys.stderr,
        )
        return 1

    table = render(numbers, rows)
    if args.check:
        current = args.output.read_text(encoding="utf-8") if args.output.exists() else ""
        # Everything before the type heading is what this check owns. The
        # separator newline between the table and the type section belongs to
        # neither, so it is normalised away rather than reported as drift.
        checked = current.split(TYPES_HEADING)[0].rstrip("\n")
        if checked != table.rstrip("\n"):
            print(f"triage-report: {args.output} is stale; regenerate it", file=sys.stderr)
            return 1
        print(
            f"triage-report: {len(numbers)} open issues, all dispositioned; "
            f"type blocks NOT checked (they need an org read this token cannot do)"
        )
        return 0

    try:
        types_block = render_types(open_records, closed_records, known, since)
    except UnknownIssueType as exc:
        # A finding, not a failed measurement: the read succeeded and returned a
        # name the org does not define. Counting it as zero is what search does.
        print(f"triage-report: {exc}", file=sys.stderr)
        return 1

    content = table + "\n" + types_block
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(content, encoding="utf-8")
    print(f"triage-report: {len(numbers)} open issues, all dispositioned; output {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
