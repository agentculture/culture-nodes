"""Bridge-enforced, allowlisted Jira issue transition capability."""

from __future__ import annotations

import json
import re
import urllib.error
import urllib.request
from base64 import b64encode
from dataclasses import dataclass
from typing import Any

from .rest import api_root

VERB = "transition_issue"
_ISSUE = re.compile(r"^[A-Z][A-Z0-9_]*-[1-9][0-9]*$")


@dataclass(frozen=True)
class Transition:
    issue: str
    target: str


@dataclass(frozen=True)
class TransitionResult:
    ok: bool
    status: int
    error: str = ""


def parse(
    raw: Any, *, project_prefix: str, allowed_targets: tuple[str, ...]
) -> tuple[Transition | None, str | None]:
    """Validate a transition request against the bridge's own allowlist.

    ``allowed_targets`` became a tuple in task t11: culture-nodes now moves a
    ticket to 'Pending' when it raises a human decision and to 'Done' when the
    work finishes, and one bridge serves both. Membership, not equality, is
    the check -- but it is still EXACT membership, so widening the allowlist
    stays a deployment decision and never a request-supplied one.
    """
    if not isinstance(raw, dict):
        return None, "input must be a JSON object"
    if set(raw) != {"verb", "issue", "target"}:
        return None, "input must contain only verb, issue, and target"
    if raw["verb"] != VERB:
        return None, f"unsupported verb; only {VERB!r} is allowed"
    issue, target = raw["issue"], raw["target"]
    if not isinstance(issue, str) or not _ISSUE.fullmatch(issue):
        return None, "issue must be a Jira issue key such as EX-123"
    if not project_prefix or not issue.startswith(project_prefix):
        return None, f"policy: issue must match configured project prefix {project_prefix!r}"
    if not isinstance(target, str) or not target:
        return None, "target must be a non-empty string"
    if not allowed_targets or target not in allowed_targets:
        allowed = ", ".join(repr(item) for item in allowed_targets) or "(none configured)"
        return None, f"policy: target must be one of the configured transitions: {allowed}"
    return Transition(issue=issue, target=target), None


def transition(
    site: str,
    issue: str,
    target: str,
    email: str,
    token: str,
    *,
    opener=urllib.request.urlopen,
    api_base: str = "",
) -> TransitionResult:
    root = api_root(site, api_base)
    if root is None:
        return TransitionResult(False, 0, error="JIRA_SITE must be a host name")
    auth = b64encode(f"{email}:{token}".encode()).decode("ascii")
    url = f"{root}/rest/api/3/issue/{issue}/transitions"
    headers = {
        "Authorization": f"Basic {auth}",
        "Accept": "application/json",
        "Content-Type": "application/json",
        "User-Agent": "culture-nodes-jira-bridge/1",
    }
    try:
        with opener(urllib.request.Request(url, headers=headers, method="GET"), timeout=10) as response:
            payload = json.loads(response.read() or b"{}")
        matches = [
            item for item in payload.get("transitions", [])
            if isinstance(item, dict) and item.get("name") == target and item.get("id")
        ]
        if len(matches) != 1:
            return TransitionResult(False, 0, error="configured Jira transition is not uniquely available")
        body = json.dumps({"transition": {"id": str(matches[0]["id"])}}).encode()
        req = urllib.request.Request(url, data=body, headers=headers, method="POST")
        with opener(req, timeout=10) as response:
            response.read()
            return TransitionResult(True, response.status)
    except urllib.error.HTTPError as exc:
        return TransitionResult(False, exc.code, error=f"Jira transition request returned HTTP {exc.code}")
    except (OSError, ValueError) as exc:
        return TransitionResult(False, 0, error=f"Jira transition request failed: {exc}")


def result(issue: str, target: str, actor_id: str) -> dict[str, Any]:
    return {
        "status": "completed",
        "outcome": "issue_transitioned",
        "output": {"issue": issue, "target": target},
        "ledger_records": [{
            "record_type": "claim",
            "authority": "proposed",
            "origin": {"kind": "agent", "actor_id": actor_id},
            "payload": {"verb": VERB, "issue": issue, "target": target},
        }],
    }
