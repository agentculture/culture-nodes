#!/usr/bin/env python3
"""PR-upkeep sweep: unresolved SonarCloud issues + open Qodo PR findings
-> one prioritised work-item list (plan task t21, spec claims c15/c26).

This is the payload of the pr-upkeep workflow's `sweep` code node. It runs
through the runner boundary (never in a control-plane process), talks to
exactly two read-only surfaces over an egress allowlist, and prints a JSON
report to stdout. It holds no write credential of any kind.

The repo is HARD-CODED to culture-nodes and nothing else — spec claim c26.
`SONAR_COMPONENT_KEY` below is the single grep-able mention of the
SonarCloud component key in this example's configuration (the recorded
fixtures under fixtures/ also contain the key, as data inside recorded API
payloads, not as configuration).

Exit-code contract (the workflow's `triage` decision node routes on
`/nodes/sweep/output`'s `exit_code`, because a code node's persisted output
is runner metadata — operation id, state, exit code, artifact refs — not
the script's stdout):

* ``0``  — the sweep ran and found at least one work item (routes `passed`).
* ``10`` — the sweep ran cleanly and found NOTHING to do (`EXIT_EMPTY`;
  routes `failed`, which triage recognises as the benign empty sweep).
* anything else — the sweep itself broke (network, auth, parse); also
  routes `failed`, and triage sends it to the backoff wait instead of
  inventing an empty result.

Dependencies: Python 3.12 stdlib only, mirroring the reference bridges'
no-PyPI-graph constraint. Unit tests: tests/test_pr_upkeep_sweep.py runs
the parsing functions against recorded fixtures (see fixtures/).
"""

from __future__ import annotations

import json
import re
import sys
import urllib.error
import urllib.request

# The one repo this sweep is allowed to look at (spec claim c26).
SONAR_COMPONENT_KEY = "agentculture_culture-nodes"
GITHUB_REPO = "agentculture/culture-nodes"

SONAR_ISSUES_URL = (
    "https://sonarcloud.io/api/issues/search" "?componentKeys={key}&resolved=false&ps=100"
)
GITHUB_API = "https://api.github.com"

#: Exit code for a clean sweep that found no work. Deliberately not 0 and
#: not 1: the triage decision node tells "nothing to do" apart from "the
#: sweep broke" by this value alone.
EXIT_EMPTY = 10

#: Unified priority rank across the two finding vocabularies. Lower sorts
#: first. SonarCloud: BLOCKER/CRITICAL/MAJOR/MINOR/INFO; Qodo badge
#: severities: High/Medium/Low.
_SEVERITY_RANK = {
    "BLOCKER": 0,
    "CRITICAL": 1,
    "HIGH": 1,
    "MAJOR": 2,
    "MEDIUM": 2,
    "MINOR": 3,
    "LOW": 3,
    "INFO": 4,
}

# SonarCloud issue statuses that still need work. The query already asks
# for resolved=false; this is the defensive re-check so a stale or
# hand-fed payload cannot resurrect closed issues.
_SONAR_OPEN_STATUSES = {"OPEN", "CONFIRMED", "REOPENED"}

_QODO_BOT_LOGIN = "qodo-code-review[bot]"
_QODO_REVIEW_HEADER = "<h3>Code Review by Qodo</h3>"

# One severity badge precedes each severity GROUP of findings, e.g.
#   <img src="https://img.shields.io/badge/High-634FD1..." ...>
_QODO_BADGE_RE = re.compile(r"img\.shields\.io/badge/(High|Medium|Low)-")

# A finding's own <summary> line carries a leading index number; the
# decorative summaries ("Tip of the day", "Context") do not. Sub-details
# inside a finding (Description/Code/Evidence/...) sit inside a blockquote,
# so their lines start with ">" and never match at line start.
_QODO_FINDING_RE = re.compile(r"^<summary>\s*(\d+)\.\s+(.*?)</summary>")

# Resolution markers Qodo adds to a closed finding's summary. An OPEN
# finding has neither marker (and no <s> strike-through).
_QODO_CLOSED_MARKERS = ("✓ Resolved", "✗ Dismissed")

_QODO_KINDS = {
    "\U0001f41e": "Bug",  # 🐞
    "\U0001f4d8": "Rule violation",  # 📘
    "\U0001f4dc": "Skill insight",  # 📜
}
_QODO_CATEGORIES = {
    "☼": "Reliability",  # ☼
    "≡": "Correctness",  # ≡
    "⛨": "Security",  # ⛨
    "⚙": "Maintainability",  # ⚙
}

_QODO_COUNTS_RE = re.compile(
    r"\U0001f41e Bugs \((\d+)\).*?"
    r"\U0001f4d8 Rule violations \((\d+)\).*?"
    r"\U0001f4dc Skill insights \((\d+)\)",
    re.DOTALL,
)

# First file reference inside a finding's Code/Evidence block:
#   <code>[internal/api/runs.go[R74-79]](https://...)</code>
_QODO_FILE_RE = re.compile(r"\[([^\[\]]+?)\[R?\d")

_TAG_RE = re.compile(r"<[^>]+>")


def severity_rank(severity: str) -> int:
    """Unified sort rank for a SonarCloud or Qodo severity name."""
    return _SEVERITY_RANK.get(severity.upper(), len(_SEVERITY_RANK))


def sonar_work_items(payload: dict) -> list[dict]:
    """SonarCloud issues-search response -> work items.

    Keeps only issues whose status still needs work and strips the
    component key prefix so `file` is repo-relative.
    """
    items = []
    for issue in payload.get("issues", []):
        if issue.get("status") not in _SONAR_OPEN_STATUSES:
            continue
        component = issue.get("component", "")
        _, _, file_path = component.partition(":")
        items.append(
            {
                "source": "sonarcloud",
                "id": issue.get("key", ""),
                "severity": issue.get("severity", ""),
                "kind": issue.get("type", ""),
                "rule": issue.get("rule", ""),
                "file": file_path or component,
                "line": issue.get("line"),
                "title": issue.get("message", ""),
                "effort": issue.get("effort"),
            }
        )
    return items


def qodo_counts(body: str) -> dict:
    """Parse the counts header of a Qodo code-review comment.

    Note the counts reflect OPEN findings only: a review whose findings
    were all resolved honestly reads ``Bugs (0)`` while still carrying the
    resolved finding blocks below it.
    """
    match = _QODO_COUNTS_RE.search(body)
    if not match:
        return {"bugs": 0, "rule_violations": 0, "skill_insights": 0}
    bugs, rules, skills = (int(group) for group in match.groups())
    return {"bugs": bugs, "rule_violations": rules, "skill_insights": skills}


def _summary_fields(summary_html: str) -> tuple[str, str, str]:
    """Title, kind, category from a finding's summary markup."""
    kind = category = ""
    for glyph, name in _QODO_KINDS.items():
        if glyph in summary_html:
            kind = name
            break
    for glyph, name in _QODO_CATEGORIES.items():
        if glyph in summary_html:
            category = name
            break
    title_part = summary_html.split("<code>", 1)[0]
    title = _TAG_RE.sub("", title_part).strip()
    return title, kind, category


def qodo_findings(body: str) -> list[dict]:
    """One Qodo code-review comment body -> its OPEN findings.

    Resolved (``✓ Resolved``) and dismissed (``✗ Dismissed``) findings are
    skipped: they are Qodo's record of closed work, not work. The severity
    badge that precedes a group of findings applies to every finding until
    the next badge.
    """
    findings = []
    severity = ""
    current: dict | None = None
    for line in body.splitlines():
        badge = _QODO_BADGE_RE.search(line)
        if badge and "shields.io" in line:
            severity = badge.group(1)
            continue
        match = _QODO_FINDING_RE.match(line)
        if match:
            index, summary_html = match.groups()
            current = None
            if any(marker in summary_html for marker in _QODO_CLOSED_MARKERS):
                continue
            if "<s>" in summary_html:
                continue
            title, kind, category = _summary_fields(summary_html)
            current = {
                "index": int(index),
                "title": title,
                "severity": severity,
                "kind": kind,
                "category": category,
                "file": "",
            }
            findings.append(current)
            continue
        if current is not None and not current["file"]:
            file_match = _QODO_FILE_RE.search(line)
            if file_match:
                current["file"] = file_match.group(1)
    return findings


def qodo_review_bodies(comments: list[dict]) -> list[str]:
    """GitHub issue-comment objects -> Qodo code-review bodies only.

    The bot also posts a ``PR Summary by Qodo`` comment; only the
    ``Code Review by Qodo`` comment is a findings surface.
    """
    bodies = []
    for comment in comments:
        login = (comment.get("user") or {}).get("login", "")
        body = comment.get("body", "")
        if login == _QODO_BOT_LOGIN and body.startswith(_QODO_REVIEW_HEADER):
            bodies.append(body)
    return bodies


def qodo_work_items(bodies: list[str], pr_numbers: list[int]) -> list[dict]:
    """Open findings across per-PR review bodies -> work items."""
    items = []
    for body, pr_number in zip(bodies, pr_numbers):
        for finding in qodo_findings(body):
            items.append(
                {
                    "source": "qodo",
                    "id": f"pr{pr_number}-qodo-{finding['index']}",
                    "pr": pr_number,
                    "severity": finding["severity"],
                    "kind": finding["kind"],
                    "category": finding["category"],
                    "file": finding["file"],
                    "line": None,
                    "title": finding["title"],
                }
            )
    return items


def prioritise(items: list[dict]) -> list[dict]:
    """Stable severity-ranked ordering: the list IS the priority."""
    return sorted(items, key=lambda item: severity_rank(item["severity"]))


def build_report(items: list[dict]) -> dict:
    ordered = prioritise(items)
    return {
        "sweep": "pr-upkeep",
        "sonar_component": SONAR_COMPONENT_KEY,
        "github_repo": GITHUB_REPO,
        "count": len(ordered),
        "items": ordered,
    }


def exit_code_for(items: list[dict]) -> int:
    return 0 if items else EXIT_EMPTY


def _get_json(url: str, token: str | None = None):
    request = urllib.request.Request(url)  # noqa: S310 — fixed https hosts
    request.add_header("Accept", "application/json")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(request, timeout=30) as response:  # noqa: S310
        return json.load(response)


def fetch_sonar_issues() -> dict:
    return _get_json(SONAR_ISSUES_URL.format(key=SONAR_COMPONENT_KEY))


def fetch_open_pr_comments(token: str | None) -> tuple[list[str], list[int]]:
    """Qodo review bodies for every open PR, with their PR numbers."""
    pulls = _get_json(f"{GITHUB_API}/repos/{GITHUB_REPO}/pulls?state=open&per_page=50", token)
    bodies: list[str] = []
    numbers: list[int] = []
    for pull in pulls:
        number = pull.get("number")
        comments = _get_json(
            f"{GITHUB_API}/repos/{GITHUB_REPO}/issues/{number}/comments" "?per_page=100",
            token,
        )
        for body in qodo_review_bodies(comments):
            bodies.append(body)
            numbers.append(number)
    return bodies, numbers


def main() -> int:
    import os

    token = os.environ.get("GITHUB_TOKEN")
    try:
        sonar_items = sonar_work_items(fetch_sonar_issues())
        qodo_bodies, pr_numbers = fetch_open_pr_comments(token)
        qodo_items = qodo_work_items(qodo_bodies, pr_numbers)
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as exc:
        print(f"sweep failed: {exc}", file=sys.stderr)
        return 1
    items = sonar_items + qodo_items
    json.dump(build_report(items), sys.stdout, indent=2, ensure_ascii=False)
    sys.stdout.write("\n")
    return exit_code_for(items)


if __name__ == "__main__":
    sys.exit(main())
