"""Read and replay Jira issue history for the PR-upkeep sweep.

This sibling module owns the emitter's Jira GET surface and converts the
fully paginated changelog/comment history into cursor-positioned facts.  It
has no control-plane write path; ``sweep.py`` remains the sole event emitter.
"""

from __future__ import annotations

import json
import re
import urllib.parse
import urllib.request
from base64 import b64encode

JIRA_SEARCH_PATH = "/rest/api/3/search/jql"
JIRA_RATE_LIMIT_PER_WINDOW = 350
JIRA_RESOLVED_LOOKBACK_DAYS = 7
JIRA_COMMENT_EVENT_NAME = "pr-upkeep.jira.comment"
JIRA_CHANGELOG_EVENT_NAME = "pr-upkeep.jira.changed"
JIRA_ACTOR_MARKER = "culture-nodes:jira-actor"
_JIRA_QUESTION_MARKER_RE = re.compile(
    r"\[culture-nodes:jira-actor question_id=([A-Za-z0-9][A-Za-z0-9._:-]{0,127})\]"
)
_JIRA_STATUS_SLUG_RE = re.compile(r"[^a-z0-9]+")


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


def jira_transition_event_name(status: str) -> str:
    slug = _JIRA_STATUS_SLUG_RE.sub("-", status.strip().lower()).strip("-")
    return f"pr-upkeep.jira.transitioned.{slug or 'unspecified'}"


def _account_id(entry: dict) -> str:
    """Jira Cloud v3 author identity, or ``""`` if absent."""
    return (entry.get("author") or {}).get("accountId") or ""


def jira_comment_text(comment: dict) -> str:
    """Flatten text leaves in Jira Cloud v3's ADF comment body."""
    body = comment.get("body")
    if isinstance(body, str):
        return body
    parts: list[str] = []

    def visit(value: object) -> None:
        if isinstance(value, dict):
            if isinstance(value.get("text"), str):
                parts.append(value["text"])
            for child in value.get("content") or []:
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)

    visit(body)
    return "".join(parts)


def _comment_timestamp(comment: dict) -> str:
    return str(comment.get("updated") or comment.get("created") or "")


def jira_question_id_for_answer(comments: list[dict]) -> str:
    """Return the marked question preceding the newest human answer."""
    ordered = sorted(comments, key=_comment_timestamp)
    if not ordered or JIRA_ACTOR_MARKER in jira_comment_text(ordered[-1]):
        return ""
    for comment in reversed(ordered[:-1]):
        match = _JIRA_QUESTION_MARKER_RE.search(jira_comment_text(comment))
        if match:
            return match.group(1)
    return ""


def jira_comment_is_self_echo(comments: list[dict], bot_account_id: str | None) -> bool:
    """Whether the newest comment is the bridge's own comment.

    A configured account id is authoritative, so a human quoting the actor
    marker cannot suppress their own fact. The marker remains a fallback for
    deployments that cannot provide a distinct bridge account identity.
    """
    if not comments:
        return False
    latest = max(comments, key=_comment_timestamp)
    if bot_account_id:
        return _account_id(latest) == bot_account_id
    return JIRA_ACTOR_MARKER in jira_comment_text(latest)


def _get_json(url: str, *, basic: tuple[str, str]) -> dict:
    request = urllib.request.Request(url)  # noqa: S310 -- configured https Jira host
    request.add_header("Accept", "application/json")
    request.add_header(
        "Authorization",
        f"Basic {b64encode(f'{basic[0]}:{basic[1]}'.encode()).decode()}",
    )
    with urllib.request.urlopen(request, timeout=30) as response:  # noqa: S310
        return json.load(response)


def fetch_jira_issues(site: str, project: str, email: str, token: str) -> dict:
    """Fetch one project's issues and fully hydrate ordered Jira history."""
    basic = (email, token)
    params = {
        "jql": (
            f'project = "{project}" AND (resolution IS EMPTY OR resolved >= '
            f"-{JIRA_RESOLVED_LOOKBACK_DAYS}d) ORDER BY priority ASC"
        ),
        "fields": "summary,priority,status,issuetype,updated,comment",
        "expand": "changelog",
        "maxResults": "100",
    }
    issues = []
    while True:
        query = urllib.parse.urlencode(params)
        page = _get_json(f"https://{site}{JIRA_SEARCH_PATH}?{query}", basic=basic)
        issues.extend(page.get("issues") or [])
        next_page_token = page.get("nextPageToken")
        if page.get("isLast", not next_page_token) or not next_page_token:
            break
        params["nextPageToken"] = str(next_page_token)

    for issue in issues:
        issue_key = urllib.parse.quote(str(issue.get("key") or ""), safe="")
        changelog = issue.setdefault("changelog", {})
        histories = list(changelog.get("histories") or [])
        _extend_jira_issue_collection(
            site,
            issue_key,
            "changelog",
            histories,
            int(changelog.get("total") or len(histories)),
            basic,
        )
        changelog["histories"] = sorted(
            histories, key=lambda entry: _history_id_key(entry.get("id"))
        )

        fields = issue.setdefault("fields", {})
        comment_page = fields.setdefault("comment", {})
        comments = list(comment_page.get("comments") or [])
        _extend_jira_issue_collection(
            site,
            issue_key,
            "comment",
            comments,
            int(comment_page.get("total") or len(comments)),
            basic,
        )
        comment_page["comments"] = sorted(
            comments, key=lambda entry: _history_id_key(entry.get("id"))
        )
    return {"issues": issues, "isLast": True}


def _extend_jira_issue_collection(
    site: str,
    issue_key: str,
    collection: str,
    entries: list[dict],
    total: int,
    basic: tuple[str, str],
) -> None:
    """Fetch expansion overflow from Jira's issue-scoped paginated APIs."""
    while len(entries) < total:
        query = urllib.parse.urlencode({"startAt": len(entries), "maxResults": 100})
        page = _get_json(
            f"https://{site}/rest/api/3/issue/{issue_key}/{collection}?{query}", basic=basic
        )
        values = page.get("values" if collection == "changelog" else "comments") or []
        if not values:
            break
        entries.extend(values)
        total = int(page.get("total") or total)


def _history_id(value: object) -> str:
    return str(value or "")


def _history_id_key(value: object) -> tuple[int, int | str]:
    text = _history_id(value)
    return (0, int(text)) if text.isdigit() else (1, text)


def jira_watermark(issue: dict) -> dict:
    histories = (issue.get("changelog") or {}).get("histories") or []
    comments = ((issue.get("fields") or {}).get("comment") or {}).get("comments") or []
    return {
        "changelog_id": max(
            (_history_id(h.get("id")) for h in histories), key=_history_id_key, default=""
        ),
        "comment_id": max(
            (_history_id(c.get("id")) for c in comments), key=_history_id_key, default=""
        ),
    }


def jira_history_facts(
    issue: dict, bot_account_id: str | None, base_payload: dict | None = None
) -> list[tuple[str, dict, dict, str, str]]:
    """Replay an issue's changelog and comments as ordered, pure facts."""
    fields = issue.get("fields") or {}
    payload_template = dict(base_payload or {})
    payload_template.setdefault("id", str(issue.get("key") or ""))
    histories = sorted(
        (issue.get("changelog") or {}).get("histories") or [],
        key=lambda history: _history_id_key(history.get("id")),
    )
    comments = sorted(
        (fields.get("comment") or {}).get("comments") or [],
        key=lambda comment: _history_id_key(comment.get("id")),
    )
    timeline = [
        (str(h.get("created") or ""), 0, _history_id_key(h.get("id")), "changelog", h)
        for h in histories
    ] + [
        (
            str(c.get("created") or c.get("updated") or ""),
            1,
            _history_id_key(c.get("id")),
            "comment",
            c,
        )
        for c in comments
    ]
    timeline.sort(key=lambda entry: entry[:3])

    facts = []
    changelog_id = ""
    comment_id = ""
    comments_seen = []
    for _created, _kind_order, _id_key, kind, entry in timeline:
        position_id = _history_id(entry.get("id"))
        if kind == "changelog":
            changelog_id = position_id
        else:
            comment_id = position_id
            comments_seen.append(entry)
        watermark = {"changelog_id": changelog_id, "comment_id": comment_id}

        if kind == "comment":
            if jira_comment_is_self_echo([entry], bot_account_id):
                continue
            payload = dict(payload_template)
            question_id = jira_question_id_for_answer(comments_seen)
            if question_id:
                payload["originating_question_id"] = question_id
            payload["answer"] = {"comment_id": position_id, "body": jira_comment_text(entry)}
            facts.append((JIRA_COMMENT_EVENT_NAME, payload, watermark, kind, position_id))
            continue

        status_item = next(
            (item for item in entry.get("items") or [] if item.get("field") == "status"), None
        )
        # Transition self-echo is exact author identity only. Do not inspect
        # marker substrings here: s14 proved that quoted marker text can
        # suppress unrelated human activity for an entire sweep interval.
        if status_item is not None and bot_account_id and _account_id(entry) == bot_account_id:
            continue
        payload = dict(payload_template)
        payload["changelog_id"] = position_id
        payload["actor_account_id"] = _account_id(entry)
        if status_item is None:
            payload["changes"] = list(entry.get("items") or [])
            name = JIRA_CHANGELOG_EVENT_NAME
        else:
            payload["status"] = str(status_item.get("toString") or "")
            payload["from_status"] = str(status_item.get("fromString") or "")
            name = jira_transition_event_name(payload["status"])
        facts.append((name, payload, watermark, kind, position_id))
    return facts
