"""Closed GitHub messaging verb surface and actor results."""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any

VERBS = ("post_comment", "reply_to_review_thread")
CLASS_ACTOR_REJECTED_INPUT = "actor_rejected_input"
_REPO = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")


@dataclass(frozen=True)
class Message:
    verb: str
    repository: str
    number: int
    comment: str
    comment_id: int | None = None


def parse(raw: Any, allowed_repositories: tuple[str, ...]) -> tuple[Message | None, str | None]:
    if not isinstance(raw, dict):
        return None, "input must be a JSON object"
    verb = raw.get("verb")
    if verb not in VERBS:
        return None, "unsupported verb"
    required = {"verb", "repository", "number", "comment"}
    if verb == "reply_to_review_thread":
        required.add("comment_id")
    if set(raw) != required:
        return None, "input has missing or unexpected fields"
    repo = raw["repository"]
    if not isinstance(repo, str) or not _REPO.fullmatch(repo):
        return None, "repository must be an owner/name pair"
    if repo not in allowed_repositories:
        return None, "repository is not allowlisted"
    number = raw["number"]
    if type(number) is not int or number <= 0:
        return None, "number must be a positive integer"
    comment = raw["comment"]
    if not isinstance(comment, str) or not comment.strip():
        return None, "comment must be a non-empty string"
    comment_id = raw.get("comment_id")
    if verb == "reply_to_review_thread" and (type(comment_id) is not int or comment_id <= 0):
        return None, "comment_id must be a positive integer"
    return Message(verb, repo, number, comment, comment_id), None


def result(
    verb: str, repository: str, number: int, comment_id: str, actor_id: str
) -> dict[str, Any]:
    payload = {"verb": verb, "repository": repository, "number": number, "comment_id": comment_id}
    return {
        "status": "completed",
        "outcome": "comment_posted" if verb == "post_comment" else "review_reply_posted",
        "output": payload,
        "ledger_records": [
            {
                "record_type": "claim",
                "authority": "proposed",
                "origin": {"kind": "agent", "actor_id": actor_id},
                "payload": payload,
            }
        ],
    }
