"""Where a Jira REST call goes: the site URL, or a granted gateway base.

Jira Cloud accepts an UNSCOPED API token at the site URL. A SCOPED
service-account token is accepted only at the Atlassian gateway
(`.../ex/jira/<cloudId>/rest/api/3/...`), and the site URL answers 401 for
it. The two are the same credential shape, so the difference is a deployment
fact and lives here rather than in each verb.

One module for four verbs on purpose: the base decides where a credential is
SENT, and four copies of that decision is four places it can drift.
"""

from __future__ import annotations

import urllib.parse

ENV_API_BASE = "JIRA_API_BASE"


def parse_api_base(value: str) -> str:
    """Normalise a granted base, or raise ``ValueError`` naming the value.

    Empty is a legitimate grant: the deploy lane writes the NAME with an
    empty value on a deployment with no gateway base, and that means "use the
    site URL". Anything else must be an https origin with an optional path —
    no credentials, query or fragment — and trailing slashes are trimmed so a
    base and a base with a slash cannot build two different URLs.
    """
    value = value.strip()
    if not value:
        return ""
    parsed = urllib.parse.urlsplit(value)
    if (
        parsed.scheme != "https"
        or not parsed.netloc
        or "@" in parsed.netloc
        or parsed.query
        or parsed.fragment
    ):
        raise ValueError(
            f"{ENV_API_BASE} must be an https URL with no query or fragment, "
            "such as the Atlassian gateway base for this site's cloud id"
        )
    return f"https://{parsed.netloc}{parsed.path.rstrip('/')}"


def api_root(site: str, api_base: str = "") -> str | None:
    """The prefix a `/rest/api/3/...` path hangs off, or ``None`` to refuse.

    ``None`` means the site is not a bare host and no base was granted, so
    there is nothing safe to build a URL from — the caller reports it as the
    long-standing "JIRA_SITE must be a host name" refusal.
    """
    if api_base:
        return api_base
    if not site or "/" in site or ":" in site:
        return None
    return f"https://{site}"
