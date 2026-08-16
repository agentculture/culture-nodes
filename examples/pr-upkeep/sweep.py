#!/usr/bin/env python3
"""PR-upkeep pure emitter for GitHub PR and Jira findings.

This is the payload of the pr-upkeep workflow's `sweep` code node. It runs
through the runner boundary (never in a control-plane process), talks to
read-only source surfaces over an egress allowlist, then raises cursor-guarded
facts through POST /v1alpha1/events. Its only write credential is the narrow
event-ingress token; it performs no triage and has no merge credential.

The repositories are a CLOSED, deployment-granted set — never run input.
Each invocation selects one entry by the configured cycle index, preserving
the granted order and reporting the selected repository.

The workflow does not ship this file inside its image. It fetches it at
dispatch time from the URL its deployment grants
(`PR_UPKEEP_SWEEP_SOURCE_URL`) and refuses bytes whose sha256 does not match
the digest it also grants (`PR_UPKEEP_SWEEP_SOURCE_SHA256`), so a deployment
that is not this one runs ITS copy of this script — with its own two
constants — rather than ours (task t16).

Each PR watermark is its head SHA plus newest comment timestamp. Each Jira
watermark is the issue update timestamp plus newest comment timestamp. The
control plane advances that watermark in the same transaction that appends
the signal event, so restarting this process cannot report the same position
twice.

SonarCloud sees the MAIN branch only unless a query names a `pullRequest`
(found live: `?componentKeys=...&resolved=false` alone answered `total: 0`
against this repo while nine real issues sat on open PR #70's own analysis
context — three `python:S3516` BLOCKERs among them). The sweep queries BOTH:
the main-branch surface (unchanged) PLUS one `&pullRequest=<n>` query per
currently open PR, so a PR's own findings are visible before it merges —
the concrete gap that made a "clean" sweep miss PR #70's nine issues
entirely. See `MAX_PRS_PER_SWEEP` for the request-budget bound and
`dedupe_sonar_items` for how a finding that shows up on more than one query
in the same sweep collapses to one work item.

A red CI check is the THIRD finding source (issue #61, found live on PR
#60: the spec PR sat with a failed `lint` job while a sweep ran and
honestly reported nothing for it, because a check conclusion is neither a
SonarCloud issue nor a Qodo review body). `check_run_work_items` reads
`GET /repos/{repo}/commits/{head_sha}/check-runs` for each swept PR — the
SAME capped PR set the other two per-PR queries use, one more request per
PR — and emits a work item per failed check run. Sonar-named checks are
skipped there: a red quality gate already arrives as SonarCloud issues
above, and counting its check run too would book the same work twice.

Dependencies: Python 3.12 stdlib only, mirroring the reference bridges'
no-PyPI-graph constraint (enforced by a test that walks this module's
import statements against `sys.stdlib_module_names`). Unit tests:
tests/test_pr_upkeep_sweep.py runs the parsing functions against recorded
fixtures (see fixtures/).
"""

from __future__ import annotations

import contextlib
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from base64 import b64encode

# The blast radius used to be one repo pinned in this module. That narrowing
# existed because fetch_open_pulls enumerates EVERY open PR and then reads
# comments and checks with the sweep credential: accepting a repo from run
# input would let a caller redirect that authority. Multi-repo upkeep keeps
# the same reasoning but moves the boundary to a CLOSED, ordered set granted
# by the deployment. One entry is selected by that grant's cycle index; run
# input still cannot add, select, or re-point a repository.
#
# PR_UPKEEP_REPOSITORIES is JSON:
# {"cycle":0,"repositories":[{"github_repo":"owner/repo",
#   "sonar_component":"owner_repo","jira_site":"team.example.com",
#   "jira_project":"EX"}]}
# Jira fields are optional; both are required together to enable that source.
REPOSITORIES_ENV = "PR_UPKEEP_REPOSITORIES"

SONAR_ISSUES_URL = (
    "https://sonarcloud.io/api/issues/search" "?componentKeys={key}&resolved=false&ps=100"
)
#: Per-PR analysis context (SonarCloud's `pullRequest` query param) — the
#: surface the plain SONAR_ISSUES_URL query cannot see (see module
#: docstring). Findings from this query carry `pr` so a downstream fix node
#: knows where to push and which thread to answer.
SONAR_PR_ISSUES_URL = (
    "https://sonarcloud.io/api/issues/search"
    "?componentKeys={key}&resolved=false&pullRequest={pr}&ps=100"
)
GITHUB_API = "https://api.github.com"
JIRA_SEARCH_PATH = "/rest/api/3/search/jql"
JIRA_RATE_LIMIT_PER_WINDOW = 350

#: Check runs for one PR's head commit. GitHub's default `filter=latest`
#: already collapses re-runs to the current attempt, so a job that failed
#: and was re-run green is reported green — the sweep does not have to
#: de-duplicate attempts itself.
GITHUB_CHECK_RUNS_URL = "{api}/repos/{repo}/commits/{sha}/check-runs?per_page=100"

#: Upper bound on open PRs swept per cycle, for BOTH the SonarCloud
#: per-PR query above and the existing Qodo per-PR comment fetch below —
#: one shared cap so the two stay consistent instead of drifting.
#:
#: Assumption, stated rather than guessed: SonarCloud does not publish a
#: request-rate ceiling as precise as GitHub's (t35's tracker arithmetic
#: does not transfer here), so this cap bounds worst-case SonarCloud calls
#: per sweep to `1 (main) + min(open_pr_count, cap)` regardless of how many
#: PRs this repo has open, rather than trying to plan against an unknown
#: number. 10 is deliberately conservative: a sweep is dispatched once per
#: human-gated loop iteration (hours apart, never a tight poll), not run
#: continuously like the GitHub merge tracker, so headroom matters less
#: than a hard, predictable ceiling. Override with
#: PR_UPKEEP_MAX_PRS_PER_SWEEP. PRs beyond the cap are NOT swept this
#: cycle — main() logs which ones were dropped rather than truncating
#: silently, and a dropped PR's issues surface on a later cycle once one of
#: the swept PRs closes or the cap is raised.
MAX_PRS_PER_SWEEP = 10

#: Unified priority rank across the three finding vocabularies. Lower sorts
#: first. SonarCloud: BLOCKER/CRITICAL/MAJOR/MINOR/INFO; Qodo badge
#: severities: High/Medium/Low; CI check runs map onto the SAME ladder via
#: REQUIRED_CHECK_SEVERITY / OPTIONAL_CHECK_SEVERITY below rather than
#: introducing a fourth vocabulary.
_SEVERITY_RANK = {
    "BLOCKER": 0,
    "HIGHEST": 0,
    "CRITICAL": 1,
    "HIGH": 1,
    "MAJOR": 2,
    "MEDIUM": 2,
    "MINOR": 3,
    "LOW": 3,
    "INFO": 4,
    "LOWEST": 4,
}

# SonarCloud issue statuses that still need work. The query already asks
# for resolved=false; this is the defensive re-check so a stale or
# hand-fed payload cannot resurrect closed issues.
_SONAR_OPEN_STATUSES = {"OPEN", "CONFIRMED", "REOPENED"}

#: Severity a failed check run inherits, by whether the check gates merge.
#: CRITICAL and HIGH share rank 1 above, so a failed required check sorts in
#: the same band as a Qodo `High` finding — a merge-blocking red job is the
#: most actionable thing this sweep can hand the fix node. MEDIUM (rank 2)
#: keeps a failed optional check visible without letting it outrank real
#: gate failures.
REQUIRED_CHECK_SEVERITY = "CRITICAL"
OPTIONAL_CHECK_SEVERITY = "MEDIUM"

#: Which check names gate a merge. This is a DECLARED policy list, not a
#: fact read from GitHub: the check-runs API does not report required-ness,
#: and `GET /repos/{repo}/branches/main/protection` answers 404 "Branch not
#: protected" for this repo (checked 2026-08-14), so there is no branch
#: protection to read it from either. These three are the jobs CLAUDE.md
#: names as merge gates (tests.yml's `test`, `lint`, and `version-check`).
#: When branch protection does land here, this list should be replaced by
#: the protection API's own answer rather than kept in sync by hand.
#: Override with PR_UPKEEP_REQUIRED_CHECKS (comma-separated; explicitly
#: empty means "nothing is required", which is the literal truth today and
#: demotes every check failure to MEDIUM).
REQUIRED_CHECKS = frozenset({"test", "lint", "version-check"})

#: Check-run conclusions that mean "this check found something a person or
#: the fix node must act on". `cancelled` is deliberately absent: a
#: cancelled run is a superseded or human-interrupted job, not a finding,
#: and reporting it would put noise at the top of a severity-ranked list.
#: `neutral`, `stale`, and `skipped` are likewise not failures.
_FAILING_CHECK_CONCLUSIONS = frozenset({"failure", "timed_out", "action_required"})

#: Sonar's own checks, matched on BOTH the check name and the app slug
#: (`sonarqubecloud`) so a renamed check is still recognised. Findings from
#: these arrive through the SonarCloud issues feed above; see
#: `check_run_work_items` for why they are skipped here.
_SONAR_CHECK_RE = re.compile(r"sonar", re.IGNORECASE)

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


def selected_repository(raw: str | None = None) -> dict:
    """Return the one repository selected by the deployment grant."""
    # Keep the literal at the read site so the exact-set AST guard sees every
    # environment capability this payload consumes.
    raw = os.environ.get("PR_UPKEEP_REPOSITORIES") if raw is None else raw
    if not raw:
        raise ValueError(f"{REPOSITORIES_ENV} is required")
    document = json.loads(raw)
    repositories = document.get("repositories") if isinstance(document, dict) else None
    cycle = document.get("cycle", 0) if isinstance(document, dict) else 0
    if not isinstance(repositories, list) or not repositories:
        raise ValueError(f"{REPOSITORIES_ENV}.repositories must be a non-empty list")
    if not isinstance(cycle, int) or cycle < 0:
        raise ValueError(f"{REPOSITORIES_ENV}.cycle must be a non-negative integer")
    repository = repositories[cycle % len(repositories)]
    if not isinstance(repository, dict):
        raise ValueError(f"{REPOSITORIES_ENV}.repositories entries must be objects")
    for name in ("github_repo", "sonar_component"):
        if not isinstance(repository.get(name), str) or not repository[name].strip():
            raise ValueError(f"{REPOSITORIES_ENV} entry requires {name}")
    jira_site = repository.get("jira_site")
    jira_project = repository.get("jira_project")
    if bool(jira_site) != bool(jira_project):
        raise ValueError("jira_site and jira_project must be configured together")
    if jira_site and ("/" in jira_site or ":" in jira_site):
        raise ValueError("jira_site must be a host name, not a URL")
    return dict(repository)


def severity_rank(severity: str) -> int:
    """Unified sort rank for a SonarCloud or Qodo severity name."""
    return _SEVERITY_RANK.get(severity.upper(), len(_SEVERITY_RANK))


def sonar_work_items(payload: dict, *, pr: int | None = None) -> list[dict]:
    """SonarCloud issues-search response -> work items.

    Keeps only issues whose status still needs work and strips the
    component key prefix so `file` is repo-relative. `pr` names the PR
    analysis context the caller queried (None for the main-branch query) —
    stamped onto every item so a downstream fix node knows where to push
    and which thread to answer; a finding with no PR context is not
    actionable by the fix lane the way a Qodo finding already is.
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
                "pr": pr,
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


def dedupe_sonar_items(items: list[dict]) -> list[dict]:
    """Collapse SonarCloud findings that appear more than once in the SAME
    sweep — the main-branch query and one or more per-PR queries can name
    the same underlying finding (e.g. a PR's own issue, and the same code
    transiently visible on main around a merge). A work item that
    reappears every cycle only because it was double-counted within one
    sweep is noise; a work item that reappears because it is genuinely
    still unresolved is real, ongoing work and is meant to keep showing up
    (the existing `resolved=false` filter above is what tells the two
    apart across cycles — this function only dedupes WITHIN one).

    Two passes, because SonarCloud's key-stability across analysis contexts
    (main vs. a specific PR) is not documented precisely enough to trust
    exclusively — this is a stated assumption, not a verified guarantee:

    1. Exact `id` (SonarCloud's own issue key), the strong signal when it
       holds across contexts.
    2. A ``(rule, file, line, title)`` fingerprint, the fallback for the
       case a PR-scoped analysis and the main-branch analysis mint
       DIFFERENT keys for what a human would call the same finding.

    On a collision, the PR-scoped entry wins: it names an actionable fix
    target (push here, answer this thread) that a main-scoped entry does
    not.
    """
    ordered: list[dict] = []
    by_key: dict[str, int] = {}
    by_fingerprint: dict[tuple, int] = {}

    for item in items:
        key = item.get("id") or ""
        fingerprint = (
            item.get("rule", ""),
            item.get("file", ""),
            item.get("line"),
            item.get("title", ""),
        )
        index = by_key.get(key) if key else None
        if index is None:
            index = by_fingerprint.get(fingerprint)
        if index is None:
            by_key[key] = len(ordered)
            by_fingerprint[fingerprint] = len(ordered)
            ordered.append(item)
            continue
        existing = ordered[index]
        if existing.get("pr") is None and item.get("pr") is not None:
            ordered[index] = item
            by_key[key] = index
            by_fingerprint[fingerprint] = index
    return ordered


def _required_checks() -> frozenset:
    """The declared required-check set, or the PR_UPKEEP_REQUIRED_CHECKS
    override. An explicitly empty override is honoured as an answer (see
    REQUIRED_CHECKS) — unlike the numeric cap override, where empty is a
    typo rather than a meaning."""
    raw = os.environ.get("PR_UPKEEP_REQUIRED_CHECKS")
    if raw is None:
        return REQUIRED_CHECKS
    return frozenset(name.strip() for name in raw.split(",") if name.strip())


def _is_sonar_check(check: dict) -> bool:
    slug = (check.get("app") or {}).get("slug") or ""
    return bool(_SONAR_CHECK_RE.search(check.get("name", "")) or _SONAR_CHECK_RE.search(slug))


def check_run_work_items(
    payload: dict, *, pr: int, required: frozenset | None = None
) -> list[dict]:
    """GitHub check-runs response for one PR's head commit -> work items.

    Only COMPLETED check runs with a failing conclusion become work (see
    `_FAILING_CHECK_CONCLUSIONS`); a still-running check is not yet a
    failure, and a green or skipped one is not work at all.

    Sonar-named checks are skipped outright. A red SonarCloud quality gate
    is not a separate piece of work from the issues that made it red — those
    already arrive through `sonar_work_items`, with a rule, a file and a
    line the fix node can act on, where the check run carries only "the gate
    failed". Emitting both would put two items on the list for one fix.

    `required` defaults to `_required_checks()`; it is a parameter so tests
    (and a future branch-protection lookup) can supply the set directly.
    """
    required = _required_checks() if required is None else required
    items = []
    for check in payload.get("check_runs", []):
        if check.get("status") != "completed":
            continue
        conclusion = (check.get("conclusion") or "").lower()
        if conclusion not in _FAILING_CHECK_CONCLUSIONS:
            continue
        if _is_sonar_check(check):
            continue
        name = check.get("name", "")
        output_title = (check.get("output") or {}).get("title") or ""
        # GitHub Actions leaves output.title null on a plain job failure, so
        # the check name has to carry the item's identity by itself.
        title = f"{name}: {output_title}" if output_title else f"{name}: check run {conclusion}"
        items.append(
            {
                "source": "github-check",
                "id": f"pr{pr}-check-{check.get('id', name)}",
                "pr": pr,
                "severity": (
                    REQUIRED_CHECK_SEVERITY if name in required else OPTIONAL_CHECK_SEVERITY
                ),
                "kind": "CI check failure",
                "check": name,
                "required": name in required,
                "conclusion": conclusion,
                "file": "",
                "line": None,
                "title": title,
                "details_url": check.get("details_url") or check.get("html_url") or "",
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


def jira_work_items(payload: dict, *, site: str, project: str) -> list[dict]:
    """Recorded Jira Cloud REST v3 search response -> work items."""
    items = []
    for issue in payload.get("issues", []):
        fields = issue.get("fields") or {}
        priority = (fields.get("priority") or {}).get("name") or "Medium"
        status = (fields.get("status") or {}).get("name") or ""
        key = issue.get("key") or ""
        items.append(
            {
                "source": "jira",
                "id": key,
                "project": project,
                "severity": priority,
                "kind": (fields.get("issuetype") or {}).get("name") or "Jira issue",
                "file": "",
                "line": None,
                "title": fields.get("summary") or "",
                "status": status,
                "details_url": f"https://{site}/browse/{urllib.parse.quote(key)}",
            }
        )
    return items


def fetch_jira_issues(site: str, project: str, email: str, token: str) -> dict:
    """Fetch one project's unresolved backlog using Jira Cloud Basic auth."""
    query = urllib.parse.urlencode(
        {
            "jql": f'project = "{project}" AND resolution IS EMPTY ORDER BY priority ASC',
            "fields": "summary,priority,status,issuetype,updated,comment",
            "maxResults": "100",
        }
    )
    return _get_json(
        f"https://{site}{JIRA_SEARCH_PATH}?{query}", basic=(email, token)
    )


def prioritise(items: list[dict]) -> list[dict]:
    """Stable severity-ranked ordering: the list IS the priority."""
    return sorted(items, key=lambda item: severity_rank(item["severity"]))


def _get_json(url: str, token: str | None = None, *, basic: tuple[str, str] | None = None):
    request = urllib.request.Request(url)  # noqa: S310 — fixed https hosts
    request.add_header("Accept", "application/json")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    if basic:
        encoded = b64encode(f"{basic[0]}:{basic[1]}".encode()).decode("ascii")
        request.add_header("Authorization", f"Basic {encoded}")
    with urllib.request.urlopen(request, timeout=30) as response:  # noqa: S310
        return json.load(response)


def fetch_sonar_issues(component: str, pr: int | None = None) -> dict:
    """The main-branch query when `pr` is None, else that same PR's own
    analysis context (see module docstring for why both are needed)."""
    if pr is None:
        return _get_json(SONAR_ISSUES_URL.format(key=component))
    return _get_json(SONAR_PR_ISSUES_URL.format(key=component, pr=pr))


def fetch_open_pulls(token: str | None, repository: str) -> list[dict]:
    """Every currently open PR as ``{"number": int, "head_sha": str}``,
    unfiltered. The cap lives with the caller (`main`) so the SAME swept set
    feeds all three per-PR queries — the SonarCloud per-PR query, the Qodo
    comment fetch, and the check-runs fetch below, one request per PR each —
    rather than independently-capped (and possibly diverging) sets.

    The head sha rides along from this ONE list request because the
    check-runs endpoint is keyed by commit: fetching it per PR instead would
    double this source's request cost for nothing. A PR object that arrives
    without a head sha keeps its entry with an empty one; `main` reports it
    rather than dropping it quietly."""
    pulls = _get_json(f"{GITHUB_API}/repos/{repository}/pulls?state=open&per_page=50", token)
    open_pulls = []
    for pull in pulls:
        if not isinstance(pull.get("number"), int):
            continue
        head = pull.get("head") or {}
        open_pulls.append({"number": pull["number"], "head_sha": head.get("sha") or ""})
    return open_pulls


def fetch_check_runs(token: str | None, repository: str, head_sha: str) -> dict:
    """Check runs for one PR's head commit (issue #61's third source)."""
    return _get_json(
        GITHUB_CHECK_RUNS_URL.format(api=GITHUB_API, repo=repository, sha=head_sha), token
    )


def fetch_open_pr_comments(
    token: str | None, repository: str, pr_numbers: list[int]
) -> tuple[list[str], list[int]]:
    """Qodo review bodies for the given (already-capped) open PR numbers."""
    bodies: list[str] = []
    numbers: list[int] = []
    for number in pr_numbers:
        comments = _get_json(
            f"{GITHUB_API}/repos/{repository}/issues/{number}/comments" "?per_page=100",
            token,
        )
        for body in qodo_review_bodies(comments):
            bodies.append(body)
            numbers.append(number)
    return bodies, numbers


def fetch_pr_comments(token: str | None, repository: str, number: int) -> list[dict]:
    """All issue comments for one PR, including timestamps for its cursor."""
    return _get_json(
        f"{GITHUB_API}/repos/{repository}/issues/{number}/comments?per_page=100", token
    )


def newest_comment_timestamp(comments: list[dict]) -> str:
    return max((str(c.get("updated_at") or c.get("created_at") or "") for c in comments), default="")


def jira_watermark(issue: dict) -> dict:
    fields = issue.get("fields") or {}
    comments = ((fields.get("comment") or {}).get("comments") or [])
    return {
        "updated_at": fields.get("updated") or "",
        "newest_comment_at": newest_comment_timestamp(comments),
    }


def raise_event(name: str, payload: dict, source_key: str, watermark: dict) -> dict:
    """Raise one cursor-guarded fact through the control plane's event path."""
    base = os.environ.get("NODES_API_URL")
    token = os.environ.get("NODES_EVENT_TOKEN")
    if not base or not token:
        raise ValueError("NODES_API_URL and NODES_EVENT_TOKEN are required")
    body = json.dumps({
        "name": name,
        "payload": payload,
        "emitter": "pr-upkeep/sweep",
        "source_key": source_key,
        "watermark": watermark,
    }).encode()
    request = urllib.request.Request(base.rstrip("/") + "/v1alpha1/events", data=body, method="POST")
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(request, timeout=30) as response:  # noqa: S310
        return json.load(response)


def _max_prs_per_sweep() -> int:
    raw = os.environ.get("PR_UPKEEP_MAX_PRS_PER_SWEEP")
    if raw is None:
        return MAX_PRS_PER_SWEEP
    try:
        value = int(raw)
    except ValueError:
        return MAX_PRS_PER_SWEEP
    return value if value > 0 else MAX_PRS_PER_SWEEP


#: The failure classes every source surface can raise. A network surface can
#: be unreachable, slow, or answer with something that is not JSON; a config
#: surface can be malformed. Kept in one place so `attempting` and `main`'s
#: boundary cannot drift apart about what counts as a sweep failure.
SWEEP_FAILURES = (urllib.error.URLError, TimeoutError, json.JSONDecodeError, ValueError)


class SweepFailure(Exception):
    """A sweep step that failed, carrying WHICH step it was.

    The sweep reads four unrelated surfaces — its own configuration, GitHub,
    SonarCloud, Jira — and before this it reported all four the same way::

        sweep failed: Expecting value: line 1 column 1 (char 0)

    That message names nothing. It is what a JSON decoder says about an empty
    body, and an empty body is what a wrong token, a rate limit, an outage, an
    SPA catch-all and a malformed environment variable all look like from
    here. Diagnosing one instance took a monkey-patched ``json.loads`` to
    discover the culprit was a malformed ``PR_UPKEEP_REPOSITORIES``.

    This sweep is meant to run unattended (#107). An always-on emitter whose
    failures name nothing is one an operator stops reading, and a sweep nobody
    reads is a sweep that has silently stopped.
    """

    def __init__(self, stage: str, cause: BaseException) -> None:
        self.stage = stage
        self.cause = cause
        super().__init__(f"{stage}: {type(cause).__name__}: {cause}")


@contextlib.contextmanager
def attempting(stage: str):
    """Tag whatever fails inside with the surface that was being read.

    Only the classes in `SWEEP_FAILURES` are tagged. Anything else is a defect
    in this file rather than a surface being unavailable, and it keeps its own
    traceback instead of being flattened into a one-line report.
    """
    try:
        yield
    except SweepFailure:
        raise
    except SWEEP_FAILURES as exc:
        raise SweepFailure(stage, exc) from exc


def main() -> int:
    token = os.environ.get("GITHUB_TOKEN")
    max_prs = _max_prs_per_sweep()
    try:
        with attempting(f"reading {REPOSITORIES_ENV}"):
            repository = selected_repository()
        github_repo = repository["github_repo"]
        component = repository["sonar_component"]
        with attempting(f"listing open PRs of {github_repo} (GitHub)"):
            open_pulls = sorted(
                fetch_open_pulls(token, github_repo), key=lambda pull: pull["number"]
            )
        swept, dropped = open_pulls[:max_prs], open_pulls[max_prs:]
        if dropped:
            dropped_prs = [pull["number"] for pull in dropped]
            print(
                f"pr-upkeep sweep: {len(dropped_prs)} open PR(s) exceed the "
                f"{max_prs}-PR-per-sweep cap and were NOT swept this cycle: "
                f"{dropped_prs} (raise PR_UPKEEP_MAX_PRS_PER_SWEEP or wait "
                "for a swept PR to close)",
                file=sys.stderr,
            )

        emitted = []
        for pull in swept:
            if not pull["head_sha"]:
                print(
                    f"pr-upkeep sweep: PR #{pull['number']} arrived with no "
                    "head sha, so its check runs were NOT read this cycle "
                    "(the check-runs endpoint is keyed by commit)",
                    file=sys.stderr,
                )
                continue
            with attempting(f"reading comments on PR #{pull['number']} (GitHub)"):
                comments = fetch_pr_comments(token, github_repo, pull["number"])
            qodo_bodies = qodo_review_bodies(comments)
            qodo_items = qodo_work_items(qodo_bodies, [pull["number"]] * len(qodo_bodies))
            with attempting(f"reading check runs for PR #{pull['number']} (GitHub)"):
                check_items = check_run_work_items(
                    fetch_check_runs(token, github_repo, pull["head_sha"]), pr=pull["number"]
                )
            with attempting(f"reading issues for PR #{pull['number']} (SonarCloud {component})"):
                pr_sonar = sonar_work_items(
                    fetch_sonar_issues(component, pr=pull["number"]), pr=pull["number"]
                )
            payload = {
                "source": "github_pr", "repository": github_repo,
                "number": pull["number"], "head_sha": pull["head_sha"],
                "findings": prioritise(pr_sonar + qodo_items + check_items),
            }
            with attempting(f"emitting pr-upkeep.pr for #{pull['number']} (control plane)"):
                emitted.append(raise_event(
                    "pr-upkeep.pr", payload, f"github:{github_repo}:pr:{pull['number']}",
                    {
                        "head_sha": pull["head_sha"],
                        "newest_comment_at": newest_comment_timestamp(comments),
                    },
                ))

        jira_items = []
        if repository.get("jira_site"):
            email = os.environ.get("JIRA_ACCOUNT_EMAIL")
            jira_token = os.environ.get("JIRA_API_TOKEN")
            if not email or not jira_token:
                raise ValueError(
                    "JIRA_ACCOUNT_EMAIL and JIRA_API_TOKEN are both required "
                    "when Jira is configured"
                )
            site, project = repository["jira_site"], repository["jira_project"]
            with attempting(f"reading {project} issues (Jira {site})"):
                jira_payload = fetch_jira_issues(site, project, email, jira_token)
            jira_items = jira_work_items(jira_payload, site=site, project=project)
            by_key = {issue.get("key"): issue for issue in jira_payload.get("issues", [])}
            for item in jira_items:
                issue = by_key.get(item["id"], {})
                with attempting(f"emitting pr-upkeep.jira for {item['id']} (control plane)"):
                    emitted.append(raise_event(
                        "pr-upkeep.jira", item,
                        f"jira:{site}:{item['id']}", jira_watermark(issue),
                    ))
    except SweepFailure as failure:
        # The stage is the whole point: four unrelated surfaces used to fail
        # with the same unattributable message. The cause keeps its own type
        # name so "not JSON" and "unreachable" stay distinguishable.
        print(f"sweep failed while {failure}", file=sys.stderr)
        return 1
    except SWEEP_FAILURES as exc:
        # Untagged: a failure outside every `attempting` block. Say so rather
        # than implying a surface was named, so the gap is visible and can be
        # given a stage of its own.
        print(f"sweep failed at an unattributed step: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1
    json.dump({"sweep": "pr-upkeep", "emitted": len(emitted)}, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
