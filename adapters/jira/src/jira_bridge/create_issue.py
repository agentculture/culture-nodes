"""Bridge-enforced, allowlisted Jira issue creation capability (task t9).

Same custody shape as the sibling transition verb: the credential stays in
this process's environment, and the policy boundary is an EXACT-MATCH project
allowlist read from configuration (``JIRA_CREATE_PROJECTS`` /
``create_projects``), never hardcoded. An empty allowlist refuses every
creation — a deployment that never configures it has not widened anything by
installing this code.
"""

from __future__ import annotations

import json
import re
import urllib.error
import urllib.request
from base64 import b64encode
from dataclasses import dataclass
from typing import Any

from .rest import api_root

VERB = "create_issue"
_PROJECT = re.compile(r"^[A-Z][A-Z0-9_]*$")


@dataclass(frozen=True)
class CreateIssue:
    project: str
    summary: str
    description: str = ""
    issue_type: str = "Task"


@dataclass(frozen=True)
class CreateResult:
    ok: bool
    status: int
    key: str = ""
    issue_id: str = ""
    error: str = ""


def parse(
    raw: Any,
    *,
    allowed_projects: tuple[str, ...],
    allowed_issue_types: tuple[str, ...] = ("Task",),
) -> tuple[CreateIssue | None, str | None]:
    if not isinstance(raw, dict):
        return None, "input must be a JSON object"
    required = {"verb", "project", "summary"}
    allowed = required | {"description", "issue_type"}
    if not required.issubset(raw) or not set(raw).issubset(allowed):
        return None, (
            "input must contain verb, project, and summary, "
            "plus optional description and issue_type"
        )
    if raw["verb"] != VERB:
        return None, f"unsupported verb; only {VERB!r} is allowed"
    project, summary = raw["project"], raw["summary"]
    if not isinstance(project, str) or not _PROJECT.fullmatch(project):
        return None, "project must be a Jira project key such as EX"
    if project not in allowed_projects:
        return None, (
            f"policy: project must be one of the configured creation targets {allowed_projects!r}"
        )
    if not isinstance(summary, str) or not summary.strip():
        return None, "summary must be a non-empty string"
    description = raw.get("description", "")
    if not isinstance(description, str):
        return None, "description must be a string when supplied"
    issue_type = raw.get("issue_type", allowed_issue_types[0] if allowed_issue_types else "Task")
    if not isinstance(issue_type, str) or not issue_type.strip():
        return None, "issue_type must be a non-empty string when supplied"
    if issue_type not in allowed_issue_types:
        return None, (
            f"policy: issue_type must be one of the configured types {allowed_issue_types!r}"
        )
    return (
        CreateIssue(
            project=project, summary=summary, description=description, issue_type=issue_type
        ),
        None,
    )


def _adf(text: str) -> dict:
    return {
        "type": "doc",
        "version": 1,
        "content": [{"type": "paragraph", "content": [{"type": "text", "text": text}]}],
    }


def create(
    site: str,
    issue: CreateIssue,
    email: str,
    token: str,
    *,
    opener=urllib.request.urlopen,
    api_base: str = "",
) -> CreateResult:
    root = api_root(site, api_base)
    if root is None:
        return CreateResult(False, 0, error="JIRA_SITE must be a host name")
    auth = b64encode(f"{email}:{token}".encode()).decode("ascii")
    fields: dict[str, Any] = {
        "project": {"key": issue.project},
        "summary": issue.summary,
        "issuetype": {"name": issue.issue_type},
    }
    if issue.description:
        fields["description"] = _adf(issue.description)
    req = urllib.request.Request(
        f"{root}/rest/api/3/issue",
        data=json.dumps({"fields": fields}).encode(),
        headers={
            "Authorization": f"Basic {auth}",
            "Accept": "application/json",
            "Content-Type": "application/json",
            "User-Agent": "culture-nodes-jira-bridge/1",
        },
        method="POST",
    )
    try:
        with opener(req, timeout=10) as response:
            payload = json.loads(response.read() or b"{}")
            return CreateResult(
                True,
                response.status,
                key=str(payload.get("key", "")),
                issue_id=str(payload.get("id", "")),
            )
    except urllib.error.HTTPError as exc:
        return CreateResult(False, exc.code, error=f"Jira create request returned HTTP {exc.code}")
    except (OSError, ValueError) as exc:
        return CreateResult(False, 0, error=f"Jira create request failed: {exc}")


def result(key: str, issue_id: str, actor_id: str) -> dict[str, Any]:
    return {
        "status": "completed",
        "outcome": "issue_created",
        "output": {"issue": key, "id": issue_id},
        "ledger_records": [{
            "record_type": "claim",
            "authority": "proposed",
            "origin": {"kind": "agent", "actor_id": actor_id},
            "payload": {"verb": VERB, "issue": key, "id": issue_id},
        }],
    }
