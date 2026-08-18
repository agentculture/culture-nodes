"""The complete and deliberately tiny Jira capability surface."""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any

VERB = "post_comment"
_ISSUE = re.compile(r"^[A-Z][A-Z0-9_]*-[1-9][0-9]*$")
_QUESTION_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")
COMMENT_MARKER = "[culture-nodes:jira-actor]"


@dataclass(frozen=True)
class Comment:
    issue: str
    text: str
    question_id: str = ""

    @property
    def marked_text(self) -> str:
        marker = COMMENT_MARKER
        if self.question_id:
            marker = f"[culture-nodes:jira-actor question_id={self.question_id}]"
        return f"{self.text.rstrip()}\n\n{marker}"


def parse(raw: Any) -> tuple[Comment | None, str | None]:
    if not isinstance(raw, dict):
        return None, "input must be a JSON object"
    allowed = {"verb", "issue", "comment", "question_id"}
    if not {"verb", "issue", "comment"}.issubset(raw) or not set(raw).issubset(allowed):
        return None, "input must contain verb, issue, and comment, plus optional question_id"
    if raw["verb"] != VERB:
        return None, f"unsupported verb; only {VERB!r} is allowed"
    issue, text = raw["issue"], raw["comment"]
    if not isinstance(issue, str) or not _ISSUE.fullmatch(issue):
        return None, "issue must be a Jira issue key such as EX-123"
    if not isinstance(text, str) or not text.strip():
        return None, "comment must be a non-empty string"
    question_id = raw.get("question_id", "")
    if not isinstance(question_id, str) or (question_id and not _QUESTION_ID.fullmatch(question_id)):
        return None, "question_id must be a safe non-empty identifier when supplied"
    return Comment(issue=issue, text=text, question_id=question_id), None


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
