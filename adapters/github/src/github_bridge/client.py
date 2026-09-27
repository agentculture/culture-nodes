"""GitHub REST transport for the two messaging writes from land_reply.py."""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from dataclasses import dataclass


@dataclass(frozen=True)
class PostResult:
    ok: bool
    status: int
    comment_id: str = ""
    error: str = ""


def _post(path: str, body: str, token: str, *, opener=urllib.request.urlopen) -> PostResult:
    request = urllib.request.Request(
        "https://api.github.com" + path,
        data=json.dumps({"body": body}).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "Content-Type": "application/json",
            "User-Agent": "culture-nodes-github-bridge/1",
        },
        method="POST",
    )
    try:
        with opener(request, timeout=30) as response:
            payload = json.loads(response.read())
            comment_id = payload.get("id") if isinstance(payload, dict) else None
            if type(comment_id) is not int or comment_id <= 0:
                return PostResult(False, response.status, error="GitHub returned no comment ID")
            return PostResult(True, response.status, str(comment_id))
    except urllib.error.HTTPError as exc:
        return PostResult(False, exc.code, error=f"GitHub comment request returned HTTP {exc.code}")
    except (OSError, ValueError) as exc:
        return PostResult(False, 0, error=f"GitHub comment request failed: {exc}")


def post_comment(
    repository: str, number: int, body: str, token: str, *, opener=urllib.request.urlopen
) -> PostResult:
    return _post(f"/repos/{repository}/issues/{number}/comments", body, token, opener=opener)


def reply_to_review_thread(
    repository: str,
    number: int,
    comment_id: int,
    body: str,
    token: str,
    *,
    opener=urllib.request.urlopen,
) -> PostResult:
    path = f"/repos/{repository}/pulls/{number}/comments/{comment_id}/replies"
    return _post(path, body, token, opener=opener)
