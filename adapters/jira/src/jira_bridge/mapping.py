"""The complete and deliberately tiny Jira capability surface."""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any

VERB = "post_comment"
_ISSUE = re.compile(r"^[A-Z][A-Z0-9_]*-[1-9][0-9]*$")


@dataclass(frozen=True)
class Comment:
    issue: str
    text: str


def parse(raw: Any) -> tuple[Comment | None, str | None]:
    if not isinstance(raw, dict):
        return None, "input must be a JSON object"
    if set(raw) != {"verb", "issue", "comment"}:
        return None, "input must contain exactly verb, issue, and comment"
    if raw["verb"] != VERB:
        return None, f"unsupported verb; only {VERB!r} is allowed"
    issue, text = raw["issue"], raw["comment"]
    if not isinstance(issue, str) or not _ISSUE.fullmatch(issue):
        return None, "issue must be a Jira issue key such as EX-123"
    if not isinstance(text, str) or not text.strip():
        return None, "comment must be a non-empty string"
    return Comment(issue=issue, text=text), None


def result(issue: str, comment_id: str, actor_id: str) -> dict[str, Any]:
    return {
        "status": "completed",
        "outcome": "comment_posted",
        "output": {"issue": issue, "comment_id": comment_id},
        "ledger_records": [{
            "record_type": "claim",
            "authority": "proposed",
            "origin": {"kind": "agent", "actor_id": actor_id},
            "payload": {"verb": VERB, "issue": issue, "comment_id": comment_id},
        }],
    }
