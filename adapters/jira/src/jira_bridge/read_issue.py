"""Bridge-enforced, allowlisted Jira issue read capability (task t20)."""

from __future__ import annotations

import json
import re
import urllib.error
import urllib.request
from base64 import b64encode
from dataclasses import dataclass
from typing import Any

from .rest import api_root

VERB = "read_issue"
DESCRIPTION_MAX_CHARS = 4000
_ISSUE = re.compile(r"^[A-Z][A-Z0-9_]*-[1-9][0-9]*$")
_FIELDS = "summary,description,status,comment,issuelinks"


@dataclass(frozen=True)
class ReadIssue:
    issue: str
    comment_limit: int


@dataclass(frozen=True)
class ReadResult:
    ok: bool
    status: int
    output: dict[str, Any] | None = None
    error: str = ""


def parse(
    raw: Any, *, project_prefix: str, comment_limit: Any
) -> tuple[ReadIssue | None, str | None]:
    if not isinstance(raw, dict):
        return None, "input must be a JSON object"
    if set(raw) != {"verb", "issue"}:
        return None, "input must contain only verb and issue"
    if raw["verb"] != VERB:
        return None, f"unsupported verb; only {VERB!r} is allowed"
    issue = raw["issue"]
    if not isinstance(issue, str) or not _ISSUE.fullmatch(issue):
        return None, "issue must be a Jira issue key such as EX-123"
    if not project_prefix or not issue.startswith(project_prefix):
        return None, f"policy: issue must match configured project prefix {project_prefix!r}"
    if isinstance(comment_limit, bool) or not isinstance(comment_limit, int) or comment_limit <= 0:
        return None, "policy: JIRA_READ_COMMENT_LIMIT must be a positive integer"
    return ReadIssue(issue=issue, comment_limit=comment_limit), None


# Copied from examples/pr-upkeep/pr_upkeep_jira.py:jira_description_text.
# Keep the sweep and bridge import-free of one another.
def jira_description_text(description: object) -> str:
    """Flatten a plain-text or Atlassian Document Format description."""
    if isinstance(description, str):
        return description
    parts: list[str] = []
    block_types = {"blockquote", "heading", "listItem", "paragraph"}

    def visit(value: object) -> None:
        if isinstance(value, dict):
            node_type = value.get("type")
            if node_type == "hardBreak":
                parts.append("\n")
            elif isinstance(value.get("text"), str):
                parts.append(value["text"])
            for child in value.get("content") or []:
                visit(child)
            if node_type in block_types and parts and not parts[-1].endswith("\n"):
                parts.append("\n")
        elif isinstance(value, list):
            for child in value:
                visit(child)

    visit(description)
    return "".join(parts).strip()


def issue_output(payload: dict[str, Any], *, comment_limit: int) -> dict[str, Any]:
    fields = payload.get("fields") or {}
    full_description = jira_description_text(fields.get("description"))
    comments = []
    for comment in (fields.get("comment") or {}).get("comments", [])[:comment_limit]:
        comments.append(
            {
                "id": str(comment.get("id", "")),
                "author": str((comment.get("author") or {}).get("accountId", "")),
                "created": str(comment.get("created", "")),
                "body": jira_description_text(comment.get("body")),
            }
        )
    links = []
    for link in fields.get("issuelinks") or []:
        linked = link.get("outwardIssue")
        direction = "outward"
        if not isinstance(linked, dict):
            linked = link.get("inwardIssue")
            direction = "inward"
        if isinstance(linked, dict) and linked.get("key"):
            links.append(
                {
                    "type": str((link.get("type") or {}).get("name", "")),
                    "direction": direction,
                    "linked_key": str(linked["key"]),
                }
            )
    return {
        "issue": str(payload.get("key", "")),
        "summary": str(fields.get("summary") or ""),
        "description": full_description[:DESCRIPTION_MAX_CHARS],
        "description_truncated": len(full_description) > DESCRIPTION_MAX_CHARS,
        "status": str((fields.get("status") or {}).get("name", "")),
        "comments": comments,
        "links": links,
    }


def read(
    site: str,
    request: ReadIssue,
    email: str,
    token: str,
    *,
    opener=urllib.request.urlopen,
    api_base: str = "",
) -> ReadResult:
    root = api_root(site, api_base)
    if root is None:
        return ReadResult(False, 0, error="JIRA_SITE must be a host name")
    auth = b64encode(f"{email}:{token}".encode()).decode("ascii")
    url = f"{root}/rest/api/3/issue/{request.issue}?fields={_FIELDS}&expand=renderedFields"
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"Basic {auth}",
            "Accept": "application/json",
            "User-Agent": "culture-nodes-jira-bridge/1",
        },
        method="GET",
    )
    try:
        with opener(req, timeout=10) as response:
            payload = json.loads(response.read() or b"{}")
            return ReadResult(
                True,
                response.status,
                output=issue_output(payload, comment_limit=request.comment_limit),
            )
    except urllib.error.HTTPError as exc:
        return ReadResult(False, exc.code, error=f"Jira read request returned HTTP {exc.code}")
    except (OSError, ValueError) as exc:
        return ReadResult(False, 0, error=f"Jira read request failed: {exc}")


def result(output: dict[str, Any], actor_id: str) -> dict[str, Any]:
    issue = str(output.get("issue", ""))
    return {
        "status": "completed",
        "outcome": "issue_read",
        "output": output,
        "ledger_records": [
            {
                "record_type": "claim",
                "authority": "proposed",
                "origin": {"kind": "agent", "actor_id": actor_id},
                "payload": {"verb": VERB, "issue": issue},
            }
        ],
    }
