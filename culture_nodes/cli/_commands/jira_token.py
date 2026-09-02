"""``nodes jira-token`` — the runbook for the Jira SERVICE ACCOUNT token.

Three verbs, none of which holds a secret for longer than one HTTP call:

* ``mint`` prints where a token is minted (the Atlassian admin UI — no API
  mints a service-account token, so the CLI cannot either) and the shape of
  the 0600 env file the other two verbs expect. Reads nothing.
* ``verify`` reads ``JIRA_ACCOUNT_EMAIL`` / ``JIRA_API_TOKEN`` /
  ``JIRA_API_BASE`` from the environment and calls
  ``GET <base>/rest/api/3/myself`` with the standard library. Exit ``0`` and
  the ``accountId`` on 200; exit ``2`` with a hint otherwise.
* ``install`` prints the ordered operator hand-turn sequence that lands a
  verified pair on thor and orin. It prints; it does not run.

The token is never rendered: not in results, not in error text, not in
``--json`` payloads. The whole point of this module is that the token was
lost once (issue #273) and the recovery path lived in nobody's head.

Why a gateway base at all: a Jira Cloud *service-account* token authenticates
only against ``https://api.atlassian.com/ex/jira/<cloudId>``. The site URL
(``https://agentculture.atlassian.net``) answers 401 for it, which is the
single most confusing symptom an operator meets on this path — so ``verify``
names it in its remediation and ``mint`` explains it up front.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import urllib.error
import urllib.request
from typing import NoReturn

from culture_nodes.cli._errors import EXIT_ENV_ERROR, EXIT_USER_ERROR, CliError
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_result

DOC_PATH = "docs/operations/jira-service-account.md"

SERVICE_ACCOUNT_NAME = "culture-nodes"
SERVICE_ACCOUNT_EMAIL = "culture-spark-9lgwfn7mz2@serviceaccount.atlassian.com"
SERVICE_ACCOUNT_ID = "712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615"
SITE_URL = "https://agentculture.atlassian.net"
CLOUD_ID = "0610b05c-63f8-4935-bd7f-a30f907bba8c"
GATEWAY_BASE = f"https://api.atlassian.com/ex/jira/{CLOUD_ID}"
ADMIN_PATH = (
    "admin.atlassian.com -> Directory -> Service accounts -> culture-nodes -> API tokens -> Create"
)
ENV_FILE = "~/.config/agent/jira-service-account.env"
MYSELF_PATH = "/rest/api/3/myself"

ENV_EMAIL = "JIRA_ACCOUNT_EMAIL"
ENV_TOKEN = "JIRA_API_TOKEN"  # nosec B105
ENV_BASE = "JIRA_API_BASE"

_VERIFY_TIMEOUT_SECONDS = 15.0

#: The install sequence. Each entry is (title, [executable lines], note).
#: `install` renders exactly these, in order; the doc repeats them verbatim.
INSTALL_STEPS: tuple[tuple[str, tuple[str, ...], str], ...] = (
    (
        "export the pair and the gateway base in ONE shell",
        (f"set -a; . {ENV_FILE}; set +a",),
        "the file is 0600 and holds JIRA_ACCOUNT_EMAIL, JIRA_API_TOKEN, JIRA_API_BASE;"
        " every later step runs from this same shell on spark",
    ),
    (
        "verify the token against the gateway base",
        ("nodes jira-token verify",),
        f"must print accountId: {SERVICE_ACCOUNT_ID}; stop here on anything else",
    ),
    (
        "land the pair in the runner secrets on thor and orin",
        ("deploy/prod/install-secrets.sh",),
        "its Jira lane rewrites ~/.culture-nodes/runner-secrets.env on both hosts with"
        " the pair + JIRA_API_BASE; it refuses when only one of email/token is set and"
        " leaves the file untouched when all three are unset",
    ),
    (
        "merge the keys into the jira bridge env and restart it (thor only)",
        (
            "ssh thor \"pgrep -af '[j]ira'\"  # pre-check: no jira bridge session in flight",
            "deploy/prod/deploy.sh thor",
        ),
        "deploy_jira merges JIRA_API_BASE (with the transition keys) into"
        " ~/.culture-nodes/jira-bridge-jira.env and restarts jira-bridge; the pair"
        " in that file is written by no lane, so on rotation edit its"
        " JIRA_ACCOUNT_EMAIL/JIRA_API_TOKEN lines by hand on thor first (umask 077);"
        " never restart a bridge mid-session",
    ),
    (
        "re-grant the bot account id to the sweep and restart the runners",
        (
            "export HOST=thor REVISION=$(git rev-parse HEAD) NODES_API_URL=http://thor:18080",
            'export PR_UPKEEP_REPOSITORIES=\'{"cycle":0,"repositories":[{"github_repo":'
            '"agentculture/culture-nodes","sonar_component":"agentculture_culture-nodes",'
            '"jira_site":"agentculture.atlassian.net","jira_project":"SCRUM",'
            f'"jira_bot_account_id":"{SERVICE_ACCOUNT_ID}"}}]}}\'',
            "deploy/prod/lanes/runner-env-write.sh",
            "HOST=orin deploy/prod/lanes/runner-env-write.sh",
            'ssh thor "systemctl --user restart nodes-runner"',
            'ssh orin "systemctl --user restart nodes-runner"',
        ),
        "runner-env-write.sh reads HOST, REVISION, NODES_API_URL, PR_UPKEEP_REPOSITORIES and"
        " the PR_UPKEEP_SWEEP_* overrides from the shell and retains what it is not given;"
        " every repository entry carries jira_bot_account_id so the sweep filters the"
        " bot's own comments as self-echo",
    ),
)

INSTALL_EPILOGUE = (
    "then: one sweep interval later the pr-upkeep-sweep-cycle run is green, and a"
    " comment posted from the OPERATOR'S OWN browser login is recorded as a human fact."
    " Do not post that comment through the jira skill: it reads runner-secrets.env and"
    " posts as the bot, which the sweep filters as self-echo."
)


# --- mint --------------------------------------------------------------------


def _mint_payload() -> dict[str, object]:
    return {
        "service_account": SERVICE_ACCOUNT_NAME,
        "email": SERVICE_ACCOUNT_EMAIL,
        "account_id": SERVICE_ACCOUNT_ID,
        "site_url": SITE_URL,
        "api_base": GATEWAY_BASE,
        "mint_at": ADMIN_PATH,
        "mints_via_api": False,
        "env_file": ENV_FILE,
        "env_file_mode": "0600",
        "env_keys": [ENV_EMAIL, ENV_TOKEN, ENV_BASE],
        "doc": DOC_PATH,
    }


def _mint_text() -> str:
    return "\n".join(
        [
            f"service account: {SERVICE_ACCOUNT_NAME} ({SERVICE_ACCOUNT_EMAIL})",
            f"accountId: {SERVICE_ACCOUNT_ID}",
            f"mint at: {ADMIN_PATH}",
            "  (no API mints a service-account token; this CLI cannot mint one either)",
            f"api base: {GATEWAY_BASE}",
            f"  (the site URL {SITE_URL} answers 401 for a service-account token;"
            " only the gateway base answers 200)",
            f"env file ({ENV_FILE}, chmod 600):",
            f"  {ENV_EMAIL}={SERVICE_ACCOUNT_EMAIL}",
            f"  {ENV_TOKEN}=<paste from the admin UI>",
            f"  {ENV_BASE}={GATEWAY_BASE}",
            "next: source it in one shell, then 'nodes jira-token verify' and"
            " 'nodes jira-token install'",
            f"doc: {DOC_PATH}",
        ]
    )


def cmd_mint(args: argparse.Namespace) -> int:
    json_mode = bool(getattr(args, "json", False))
    emit_result(_mint_payload() if json_mode else _mint_text(), json_mode=json_mode)
    return 0


# --- verify ------------------------------------------------------------------


def _read_env() -> tuple[str, str, str]:
    """Read the three variables; name the MISSING ones, never a value."""
    values = {name: os.environ.get(name, "") for name in (ENV_EMAIL, ENV_TOKEN, ENV_BASE)}
    missing = [name for name, value in values.items() if not value]
    if missing:
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"missing environment variable(s): {', '.join(missing)}",
            remediation=(
                f"set -a; . {ENV_FILE}; set +a  (or export {ENV_EMAIL}, {ENV_TOKEN} and"
                f" {ENV_BASE}); 'nodes jira-token mint' prints the file shape"
            ),
        )
    return values[ENV_EMAIL], values[ENV_TOKEN], values[ENV_BASE].rstrip("/")


def _check_base(base: str) -> None:
    if not base.startswith("https://"):
        raise CliError(
            code=EXIT_USER_ERROR,
            message=f"{ENV_BASE} must be an https:// URL",
            remediation=f"export {ENV_BASE}={GATEWAY_BASE}",
        )


def _myself(email: str, token: str, base: str) -> dict[str, object]:
    """One GET against ``<base>/rest/api/3/myself``; the token lives only here."""
    credential = base64.b64encode(f"{email}:{token}".encode()).decode("ascii")
    request = urllib.request.Request(
        base + MYSELF_PATH,
        headers={"Authorization": f"Basic {credential}", "Accept": "application/json"},
        method="GET",
    )
    try:
        # _check_base has already refused anything but https://, so the
        # scheme reaching urlopen is known-safe.
        response = urllib.request.urlopen(request, timeout=_VERIFY_TIMEOUT_SECONDS)  # nosec B310
        with response:
            raw = response.read()
    except urllib.error.HTTPError as err:
        _raise_http(err.code, base)
    except (urllib.error.URLError, OSError, TimeoutError) as err:
        reason = getattr(err, "reason", None) or err.__class__.__name__
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"could not reach {base}{MYSELF_PATH}: {reason}",
            remediation="check network access to api.atlassian.com and retry",
        ) from None
    try:
        payload = json.loads(raw.decode("utf-8"))
    except (ValueError, UnicodeDecodeError):
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"{base}{MYSELF_PATH} answered 200 with a non-JSON body",
            remediation=f"confirm {ENV_BASE} is the gateway base {GATEWAY_BASE}",
        ) from None
    if not isinstance(payload, dict) or not payload.get("accountId"):
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"{base}{MYSELF_PATH} answered 200 without an accountId",
            remediation=f"confirm {ENV_BASE} is the gateway base {GATEWAY_BASE}",
        )
    return payload


def _raise_http(status: int, base: str) -> NoReturn:
    if status in (401, 403):
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"{base}{MYSELF_PATH} answered HTTP {status}",
            remediation=(
                f"a service-account token is refused (401) at the site URL {SITE_URL};"
                f" {ENV_BASE} must be the gateway base {GATEWAY_BASE}. If it already is,"
                " the token was revoked or mistyped: re-mint it ('nodes jira-token mint')"
            ),
        ) from None
    raise CliError(
        code=EXIT_ENV_ERROR,
        message=f"{base}{MYSELF_PATH} answered HTTP {status}",
        remediation="retry; if it persists, check https://status.atlassian.com",
    ) from None


def cmd_verify(args: argparse.Namespace) -> int:
    json_mode = bool(getattr(args, "json", False))
    email, token, base = _read_env()
    _check_base(base)
    payload = _myself(email, token, base)
    account_id = str(payload["accountId"])
    if json_mode:
        emit_result(
            {"account_id": account_id, "email": email, "api_base": base},
            json_mode=True,
        )
    else:
        emit_result(f"accountId: {account_id}", json_mode=False)
    return 0


# --- install -----------------------------------------------------------------


def _install_payload() -> dict[str, object]:
    return {
        "steps": [
            {"n": index, "title": title, "commands": list(commands), "note": note}
            for index, (title, commands, note) in enumerate(INSTALL_STEPS, start=1)
        ],
        "then": INSTALL_EPILOGUE,
        "rotation": "mint a new token in the admin UI, revoke the old one there, repeat steps 1-5",
        "doc": DOC_PATH,
    }


def _install_text() -> str:
    lines: list[str] = [
        "# nodes jira-token install — the operator hand-turn sequence (run from spark)"
    ]
    for index, (title, commands, note) in enumerate(INSTALL_STEPS, start=1):
        lines.append("")
        lines.append(f"{index}. {title}")
        lines.extend(f"    {command}" for command in commands)
        lines.append(f"   # {note}")
    lines.append("")
    lines.append(INSTALL_EPILOGUE)
    lines.append("rotation: mint a new token in the admin UI, revoke the old one there, repeat 1-5")
    lines.append(f"doc: {DOC_PATH}")
    return "\n".join(lines)


def cmd_install(args: argparse.Namespace) -> int:
    json_mode = bool(getattr(args, "json", False))
    emit_result(_install_payload() if json_mode else _install_text(), json_mode=json_mode)
    return 0


# --- registration ------------------------------------------------------------


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes jira-token {mint,verify,install} ...\n"
        "run 'nodes explain jira-token' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "jira-token",
        help="Runbook for the Jira service-account token: mint, verify, install.",
    )
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="jira_token_command", parser_class=type(p))

    mint = noun_sub.add_parser(
        "mint", help="Print where the token is minted and the env file shape."
    )
    mint.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    mint.set_defaults(func=cmd_mint)

    verify = noun_sub.add_parser(
        "verify", help="GET /rest/api/3/myself at JIRA_API_BASE with the pair from the environment."
    )
    verify.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    verify.set_defaults(func=cmd_verify)

    install = noun_sub.add_parser(
        "install", help="Print the ordered install sequence for thor and orin (does not run it)."
    )
    install.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    install.set_defaults(func=cmd_install)
