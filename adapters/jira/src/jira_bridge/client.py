"""Narrow Jira transport: this module can only construct a comment URL."""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from base64 import b64encode
from dataclasses import dataclass

from .rest import api_root


@dataclass(frozen=True)
class PostResult:
    ok: bool
    status: int
    comment_id: str = ""
    error: str = ""


def post_comment(
    site: str,
    issue: str,
    text: str,
    email: str,
    token: str,
    *,
    opener=urllib.request.urlopen,
    api_base: str = "",
) -> PostResult:
    root = api_root(site, api_base)
    if root is None:
        return PostResult(False, 0, error="JIRA_SITE must be a host name")
    auth = b64encode(f"{email}:{token}".encode()).decode("ascii")
    body = json.dumps({"body": {"type": "doc", "version": 1, "content": [{"type": "paragraph", "content": [{"type": "text", "text": text}]}]}}).encode()
    req = urllib.request.Request(
        f"{root}/rest/api/3/issue/{issue}/comment",
        data=body,
        headers={"Authorization": f"Basic {auth}", "Accept": "application/json", "Content-Type": "application/json", "User-Agent": "culture-nodes-jira-bridge/1"},
        method="POST",
    )
    try:
        with opener(req, timeout=10) as response:
            payload = json.loads(response.read() or b"{}")
            return PostResult(True, response.status, comment_id=str(payload.get("id", "")))
    except urllib.error.HTTPError as exc:
        return PostResult(False, exc.code, error=f"Jira comment request returned HTTP {exc.code}")
    except (OSError, ValueError) as exc:
        return PostResult(False, 0, error=f"Jira comment request failed: {exc}")
