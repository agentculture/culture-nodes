#!/usr/bin/env python3
"""Assign GitHub issue types in bulk from docs/triage/issue-types.csv.

Stdlib only, like every other script here: the runtime package declares zero
third-party dependencies and scripts/ keeps the same discipline.

Two facts shape this script, both measured rather than assumed:

* The installed `gh` (2.45.0) has no `issueType` JSON field -- `gh issue list
  --json issueType` fails with "Unknown JSON field". Issue types are reachable
  only through GraphQL, so every read and every write here is `gh api graphql`.
* The GitHub search `type:` qualifier FAILS OPEN (`type:NotARealType` returns 0
  results rather than an error) and its index lags writes. So this script never
  reads a type through search -- not to plan the run, not to verify it.

The write is the only irreversible step in the issue-types lane, and its
reversibility depends entirely on the pre-state snapshot: every target issue's
type BEFORE the run, written to disk and its path printed, before the first
mutation. A resumed run reuses that file rather than overwriting it -- an
overwritten snapshot taken halfway through is not a way back.
"""

from __future__ import annotations

import argparse
import csv
import json
import subprocess  # nosec B404 - `gh` is invoked with a fixed argv, never a shell
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

# GitHub answers 503 often enough to matter here; scripts/triage-report.py
# carries the same retry for the same reason.
GH_ATTEMPTS = 4
GH_BACKOFF_SECONDS = 3

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_TABLE = ROOT / "docs" / "triage" / "issue-types.csv"
DEFAULT_SNAPSHOT = ROOT / "docs" / "triage" / "issue-types-prestate.json"
DEFAULT_REPO = "agentculture/culture-nodes"
DEFAULT_ORG = "agentculture"

# A row whose type is this is "examined and deliberately left untyped". It is
# not the same as an issue nobody looked at, and the summary counts it apart.
UNDETERMINED = "UNDETERMINED"

# The type this repo's Record convention depends on. It is created by a human
# with an admin:org token, so it may simply not exist yet -- and if it does not,
# the run stops with ONE error, not one per issue.
REQUIRED_TYPE = "Record"

ISSUE_TYPES_QUERY = """
query($org: String!) {
  organization(login: $org) {
    issueTypes(first: 20) { nodes { id name isEnabled } }
  }
}
"""

OPEN_ISSUES_QUERY = """
query($owner: String!, $name: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, first: 100, after: $cursor) {
      pageInfo { hasNextPage endCursor }
      nodes { number id issueType { name } }
    }
  }
}
"""

SET_TYPE_MUTATION = """
mutation($id: ID!, $typeId: ID!) {
  updateIssue(input: {id: $id, issueTypeId: $typeId}) {
    issue { number issueType { name } }
  }
}
"""


class GitHubUnreachable(RuntimeError):
    """GitHub could not be READ or WRITTEN. Distinct from a wrong table.

    Exits 2 -- the code scripts/merge-gate.py uses for measurement_incomplete
    and culture_nodes/cli/_errors.py reserves for an environment error. A CSV
    that names a type the org does not define is a finding and exits 1.
    """


def invoke_gh(command: list[str]) -> tuple[int, str, str]:
    """The one process boundary. Tests substitute this callable."""
    proc = subprocess.run(  # nosec B603 - fixed argv, shell=False
        command, check=False, text=True, capture_output=True
    )
    return proc.returncode, proc.stdout, proc.stderr


def graphql_command(query: str, variables: dict[str, str]) -> list[str]:
    command = ["gh", "api", "graphql", "-f", f"query={query}"]
    for key in sorted(variables):
        command += ["-f", f"{key}={variables[key]}"]
    return command


def run_graphql(
    query: str,
    variables: dict[str, str],
    invoke,
    backoff: float = GH_BACKOFF_SECONDS,
    sleep=time.sleep,
) -> dict:
    """Every gh call -- read and write -- goes through this one retry path."""
    command = graphql_command(query, variables)
    last = ""
    raw = ""
    for attempt in range(1, GH_ATTEMPTS + 1):
        code, stdout, stderr = invoke(command)
        if code == 0:
            raw = stdout
            break
        tail = (stderr or stdout).strip().splitlines()[-1:] or [""]
        last = tail[0]
        if attempt < GH_ATTEMPTS:
            print(
                f"backfill-issue-types: gh failed ({last}); "
                f"retry {attempt}/{GH_ATTEMPTS - 1}",
                file=sys.stderr,
            )
            if backoff:
                sleep(backoff * attempt)
    else:
        raise GitHubUnreachable(
            f"could not reach GitHub after {GH_ATTEMPTS} attempts: {last}"
        )
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise GitHubUnreachable(f"gh returned non-JSON: {exc}") from exc
    if payload.get("errors"):
        message = "; ".join(str(item.get("message")) for item in payload["errors"])
        raise GitHubUnreachable(f"GraphQL error: {message}")
    return payload.get("data") or {}


def org_issue_types(org: str, invoke, fixture: Path | None = None, **kwargs) -> dict[str, str]:
    """Resolve type NAME -> node id from organization.issueTypes, at run time.

    Names are never hard-coded into a write: an id resolved here is the only
    thing this script will send to updateIssue.
    """
    if fixture:
        nodes = json.loads(fixture.read_text(encoding="utf-8"))
    else:
        data = run_graphql(ISSUE_TYPES_QUERY, {"org": org}, invoke, **kwargs)
        nodes = (data.get("organization") or {}).get("issueTypes", {}).get("nodes", [])
    return {
        node["name"]: node["id"]
        for node in nodes
        if node.get("isEnabled", True)
    }


def open_issues(repo: str, invoke, fixture: Path | None = None, **kwargs) -> dict[int, dict]:
    """number -> {"id": node id, "type": name | None} for every OPEN issue.

    Per-issue GraphQL. Never `no:type` / `type:` search: that qualifier fails
    open on an unknown name and lags writes, so it cannot decide a mutation.
    """
    if fixture:
        nodes = json.loads(fixture.read_text(encoding="utf-8"))
    else:
        owner, _, name = repo.partition("/")
        nodes = []
        cursor: str | None = None
        while True:
            variables = {"owner": owner, "name": name}
            if cursor:
                variables["cursor"] = cursor
            data = run_graphql(OPEN_ISSUES_QUERY, variables, invoke, **kwargs)
            issues = (data.get("repository") or {}).get("issues", {})
            nodes.extend(issues.get("nodes", []))
            page = issues.get("pageInfo") or {}
            if not page.get("hasNextPage"):
                break
            cursor = page.get("endCursor")
    result = {}
    for node in nodes:
        issue_type = node.get("issueType") or {}
        result[int(node["number"])] = {
            "id": node["id"],
            "type": issue_type.get("name"),
        }
    return result


def set_issue_type(issue_id: str, type_id: str, invoke, **kwargs) -> None:
    run_graphql(SET_TYPE_MUTATION, {"id": issue_id, "typeId": type_id}, invoke, **kwargs)


def load_table(path: Path) -> dict[int, dict[str, str]]:
    with path.open(encoding="utf-8", newline="") as handle:
        rows = list(csv.DictReader(handle))
    required = {"issue", "type", "evidence_pointer"}
    if not rows or set(rows[0]) != required:
        raise ValueError(f"{path}: expected columns {sorted(required)}")
    result: dict[int, dict[str, str]] = {}
    for line, row in enumerate(rows, 2):
        issue = int(row["issue"])
        if issue in result:
            raise ValueError(f"{path}:{line}: duplicate issue #{issue}")
        if not row["type"].strip() or not row["evidence_pointer"].strip():
            raise ValueError(f"{path}:{line}: type and evidence pointer are required")
        result[issue] = {
            "type": row["type"].strip(),
            "evidence_pointer": row["evidence_pointer"].strip(),
        }
    return result


def plan_run(table: dict[int, dict[str, str]], current: dict[int, dict]) -> dict[str, list]:
    """Split the table into what will be written and what will not."""
    to_set, correct, untyped, not_open = [], [], [], []
    for issue in sorted(table):
        want = table[issue]["type"]
        state = current.get(issue)
        if state is None:
            not_open.append(issue)
        elif want == UNDETERMINED:
            untyped.append(issue)
        elif state["type"] == want:
            correct.append(issue)
        else:
            to_set.append(issue)
    return {"set": to_set, "correct": correct, "untyped": untyped, "not_open": not_open}


def render_mapping(table, current, plan) -> str:
    action = {}
    for name, issues in plan.items():
        for issue in issues:
            action[issue] = name
    labels = {
        "set": "set",
        "correct": "already correct",
        "untyped": "leave untyped",
        "not_open": "skip (not open)",
    }
    lines = [
        "| Issue | Current type | Target type | Action | Evidence |",
        "|---:|---|---|---|---|",
    ]
    for issue in sorted(table):
        state = current.get(issue) or {}
        now = state.get("type") or "(none)"
        want = table[issue]["type"]
        evidence = table[issue]["evidence_pointer"].replace("|", "\\|")
        lines.append(
            f"| #{issue} | {now} | {want} | {labels[action[issue]]} | {evidence} |"
        )
    return "\n".join(lines)


def write_snapshot(path: Path, repo: str, table, current) -> bool:
    """Record every target issue's type BEFORE the first mutation.

    Returns True when this call created the file. An existing snapshot is left
    exactly as it is: on a resumed run it is the pre-state of the ORIGINAL run,
    and rewriting it halfway through would record a state the run itself caused.
    """
    if path.exists():
        return False
    payload = {
        "captured_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "repo": repo,
        "note": (
            "Pre-state for scripts/backfill-issue-types.py. Restore a type with "
            "updateIssue(input:{id, issueTypeId}); a null type is cleared with "
            "issueTypeId:null."
        ),
        "issues": {
            str(issue): {
                "id": current[issue]["id"],
                "type": current[issue]["type"],
            }
            for issue in sorted(table)
            if issue in current
        },
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    return True


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--repo", default=DEFAULT_REPO)
    parser.add_argument("--org", default=DEFAULT_ORG)
    parser.add_argument("--table", type=Path, default=DEFAULT_TABLE)
    parser.add_argument("--snapshot", type=Path, default=DEFAULT_SNAPSHOT)
    parser.add_argument("--issues-json", type=Path, help="offline fixture: open issue nodes")
    parser.add_argument("--org-types-json", type=Path, help="offline fixture: org issue types")
    parser.add_argument("--backoff-seconds", type=float, default=GH_BACKOFF_SECONDS)
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="print the full issue-to-type mapping and perform no mutation",
    )
    return parser


def main(argv=None, invoke=None) -> int:
    args = build_parser().parse_args(argv)
    invoke = invoke or invoke_gh
    retry = {"backoff": args.backoff_seconds}

    try:
        table = load_table(args.table)
        types = org_issue_types(args.org, invoke, args.org_types_json, **retry)
    except (GitHubUnreachable, OSError, TypeError, ValueError, KeyError) as exc:
        print(f"backfill-issue-types: {exc}", file=sys.stderr)
        return 2 if isinstance(exc, GitHubUnreachable) else 1

    # Preflight, in this order, so a run that cannot succeed says so once.
    if REQUIRED_TYPE not in types:
        print(
            f"backfill-issue-types: the {REQUIRED_TYPE!r} issue type does not exist in "
            f"the {args.org!r} org (found: {', '.join(sorted(types)) or 'none'}); "
            "create it with an admin:org token before backfilling",
            file=sys.stderr,
        )
        return 1

    wanted = {row["type"] for row in table.values()} - {UNDETERMINED}
    unknown = sorted(wanted - set(types))
    if unknown:
        print(
            f"backfill-issue-types: {args.table} names issue types the {args.org!r} org "
            f"does not define: {', '.join(unknown)}",
            file=sys.stderr,
        )
        return 1

    try:
        current = open_issues(args.repo, invoke, args.issues_json, **retry)
    except (GitHubUnreachable, OSError, TypeError, ValueError, KeyError) as exc:
        print(f"backfill-issue-types: {exc}", file=sys.stderr)
        return 2 if isinstance(exc, GitHubUnreachable) else 1

    plan = plan_run(table, current)
    for issue in plan["not_open"]:
        print(
            f"backfill-issue-types: #{issue} is in {args.table} but is not open; skipping",
            file=sys.stderr,
        )

    summary = (
        f"{len(plan['set'])} to set, {len(plan['correct'])} already correct, "
        f"{len(plan['untyped'])} left untyped ({UNDETERMINED}), "
        f"{len(plan['not_open'])} not open"
    )

    if args.dry_run:
        print("backfill-issue-types: dry run -- no mutation will be performed")
        print(render_mapping(table, current, plan))
        print(f"backfill-issue-types: {summary}")
        return 0

    if not plan["set"]:
        print(render_mapping(table, current, plan))
        print(f"backfill-issue-types: nothing to write; {summary}")
        return 0

    created = write_snapshot(args.snapshot, args.repo, table, current)
    verb = "wrote" if created else "reusing existing"
    print(f"backfill-issue-types: {verb} pre-state snapshot {args.snapshot}")

    written = 0
    try:
        for issue in plan["set"]:
            want = table[issue]["type"]
            set_issue_type(current[issue]["id"], types[want], invoke, **retry)
            written += 1
            print(f"backfill-issue-types: #{issue} -> {want}")
    except GitHubUnreachable as exc:
        print(f"backfill-issue-types: {exc}", file=sys.stderr)
        print(
            f"backfill-issue-types: wrote {written} of {len(plan['set'])}; "
            f"re-run to resume from {args.snapshot}",
            file=sys.stderr,
        )
        return 2

    print(f"backfill-issue-types: {summary}; wrote {written}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
