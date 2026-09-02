"""``nodes jira-token`` — the runbook for the Jira SERVICE ACCOUNT token.

Four verbs, none of which holds a secret for longer than one call:

* ``mint`` prints where a token is minted (the Atlassian admin UI — no API
  mints a service-account token, so the CLI cannot either) and how the
  other verbs consume it. Reads nothing.
* ``seal`` reads the token once — ``getpass`` on a TTY, one line of stdin
  otherwise — and hands it to ``grant set <name> - --hidden`` on stdin. The
  token never appears in an argv and is never written to a plaintext file
  by this CLI; a *hidden* grant secret can only be consumed through
  ``grant run --inject``.
* ``verify`` reads ``JIRA_API_TOKEN`` from the environment (the
  ``grant run --inject`` path, or a ``getpass`` prompt on a TTY) and calls
  ``GET <base>/rest/api/3/myself`` with the standard library. Exit ``0`` and
  the ``accountId`` on 200 **only when that accountId is
  ``SERVICE_ACCOUNT_ID``**; exit ``2`` with a hint otherwise. Verifying the
  *identity* and not merely the *credential* is the point: a token minted
  under any other Jira account — an operator's own, a second service
  account — authenticates perfectly well and would install cleanly, and the
  sweep's self-echo filter keys on this one id, so the wrong account would
  make the bot's own comments read back as human facts. Email and base
  default to the module constants — they are not secrets. The base is more
  than a default: it is *pinned* to the gateway, because ``verify`` hands the
  Basic credential to whatever base it is given, and an environment that can
  set ``JIRA_API_BASE`` must not thereby be able to choose who receives the
  token.
* ``install`` prints the ordered operator hand-turn sequence that lands a
  verified pair on thor and orin. It prints; it does not run.

The token is never rendered: not in results, not in error text, not in
``--json`` payloads. The whole point of this module is that the token was
lost once (issue #273) and the recovery path lived in nobody's head — and
the operator's decision on the amendment: it must never sit in a plaintext
file on spark either.

Why a gateway base at all: a Jira Cloud *service-account* token authenticates
only against ``https://api.atlassian.com/ex/jira/<cloudId>``. The site URL
(``https://agentculture.atlassian.net``) answers 401 for it, which is the
single most confusing symptom an operator meets on this path — so ``verify``
names it in its remediation and ``mint`` explains it up front.
"""

from __future__ import annotations

import argparse
import base64
import getpass
import json
import os
import re
import shutil
import subprocess  # nosec B404 - argv list only, never a shell
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import NoReturn

from culture_nodes.cli._errors import EXIT_ENV_ERROR, EXIT_USER_ERROR, CliError
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_result

DOC_PATH = "docs/operations/jira-service-account.md"

SERVICE_ACCOUNT_NAME = "culture-nodes"
SERVICE_ACCOUNT_EMAIL = "culture-spark-9lgwfn7mz2@serviceaccount.atlassian.com"
SERVICE_ACCOUNT_ID = "712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615"
SITE_URL = "https://agentculture.atlassian.net"
#: The browse host, without the scheme. ``JIRA_SITE`` is a BARE HOST
#: everywhere it is read — the bridge refuses anything with a ``/`` or a
#: ``:`` as "JIRA_SITE must be a host name" — so it cannot be spelled
#: ``SITE_URL``.
SITE_HOST = SITE_URL.split("://", 1)[1]
CLOUD_ID = "0610b05c-63f8-4935-bd7f-a30f907bba8c"
GATEWAY_BASE = f"https://api.atlassian.com/ex/jira/{CLOUD_ID}"
ADMIN_PATH = (
    "admin.atlassian.com -> Directory -> Service accounts -> culture-nodes -> API tokens -> Create"
)
MYSELF_PATH = "/rest/api/3/myself"

ENV_EMAIL = "JIRA_ACCOUNT_EMAIL"
ENV_TOKEN = "JIRA_API_TOKEN"  # nosec B105
ENV_BASE = "JIRA_API_BASE"
#: Read by ``deploy.sh``'s ``deploy_jira``, not by any verb here. It is in
#: this module because the install sequence is the thing that has to set it:
#: ``deploy_jira`` RETURNS EARLY when it is unset, so a step 4 that omits it
#: prints one ``say`` line in the middle of a long deploy log and merges
#: nothing, restarts nothing, and still exits 0.
ENV_SITE = "JIRA_SITE"

#: The grant secret that holds the token. grant (0.9.0 on spark) accepts
#: only ``^[A-Z_][A-Z0-9_]{0,63}$`` as a name, so this is upper-case.
SECRET_NAME = "JIRA_SERVICE_ACCOUNT_TOKEN"  # nosec B105 - a name, not a value
#: grant's name rule (0.9.0 on spark, 0.11.0 on thor); a rename that breaks
#: it fails at import, not at the operator's seal.
GRANT_NAME_RE = re.compile(r"[A-Z_][A-Z0-9_]{0,63}")
assert GRANT_NAME_RE.fullmatch(SECRET_NAME), SECRET_NAME  # nosec B101
SECRET_STORE = "grant"  # nosec B105 - a store name, not a value
GRANT_BIN = "grant"
#: The only way a hidden grant secret reaches a process.
INJECT_PREFIX = f"grant run --inject {ENV_TOKEN}={SECRET_NAME} --"
GRANT_PURPOSE = (
    f"Jira service account {SERVICE_ACCOUNT_NAME} ({SERVICE_ACCOUNT_ID}) API token;"
    f" consumed via {INJECT_PREFIX}"
)
GRANT_ROTATE_HOWTO = (
    "mint a new token at admin.atlassian.com -> Directory -> Service accounts ->"
    " culture-nodes -> API tokens, then nodes jira-token seal, then nodes jira-token install"
)
_REDACTED = "<redacted>"

_VERIFY_TIMEOUT_SECONDS = 15.0

#: The install sequence. Each entry is (title, [executable lines], note).
#: `install` renders exactly these, in order; the doc repeats them verbatim.
#: No step sources a file: the token lives in grant, hidden, and reaches a
#: lane only through `grant run --inject`.
INSTALL_STEPS: tuple[tuple[str, tuple[str, ...], str], ...] = (
    (
        "seal the token in grant (once)",
        ("nodes jira-token seal",),
        f"prompts without echo and stores it hidden as {SECRET_NAME}; skip when"
        f" 'grant show {SECRET_NAME}' already succeeds; the token is never written to a"
        " plaintext file on spark",
    ),
    (
        "verify the token against the gateway base",
        (f"{INJECT_PREFIX} nodes jira-token verify",),
        f"must print accountId: {SERVICE_ACCOUNT_ID}; verify itself exits 2 on any other"
        " account, so a token minted under the wrong one stops here rather than at step 5"
        f" ({ENV_EMAIL} and {ENV_BASE} default to the service account and the gateway base)",
    ),
    (
        "land the pair in the runner secrets on thor and orin",
        (
            f"export {ENV_EMAIL}={SERVICE_ACCOUNT_EMAIL} {ENV_BASE}={GATEWAY_BASE}",
            f"{INJECT_PREFIX} deploy/prod/install-secrets.sh",
        ),
        "the lane reads the three from its environment: email and base are non-secret"
        " exports, the token is injected for that one process; its Jira lane rewrites"
        " ~/.culture-nodes/runner-secrets.env on both hosts, refuses when only one of"
        " email/token is set, and leaves the file untouched when all three are unset",
    ),
    (
        "merge the keys into the jira bridge env and restart it (thor only)",
        (
            f"export {ENV_SITE}={SITE_HOST}",
            "ssh thor \"pgrep -af '[j]ira'\"  # pre-check: no jira bridge session in flight",
            f"{INJECT_PREFIX} deploy/prod/deploy.sh thor",
        ),
        f"the {ENV_SITE} export is load-bearing and it is a BARE HOST: deploy_jira"
        f" returns at its unset-{ENV_SITE} guard, so without it deploy.sh says one line"
        " mid-log, merges nothing, restarts nothing and still exits 0 — which is how the"
        " bridge was last reinstalled by hand (t29). Then"
        f" deploy_jira merges {ENV_BASE} (exported in step 3, with the transition keys)"
        " into ~/.culture-nodes/jira-bridge-jira.env and restarts jira-bridge; the pair"
        " in that file is written by no lane: it is a hand edit on thor (umask 077),"
        " and the token reaches it by the operator pasting it once, or by sealing it"
        " on thor's grant too and writing the line under grant run there; do it before"
        " deploy.sh on a rotation; never restart a bridge mid-session",
    ),
    (
        "re-grant the bot account id to the sweep and restart the runners",
        (
            "export HOST=thor REVISION=$(git rev-parse HEAD) NODES_API_URL=http://thor:18080",
            'export PR_UPKEEP_REPOSITORIES=\'{"cycle":0,"repositories":[{"github_repo":'
            '"agentculture/culture-nodes","sonar_component":"agentculture_culture-nodes",'
            '"jira_site":"agentculture.atlassian.net","jira_project":"SCRUM",'
            f'"jira_bot_account_id":"{SERVICE_ACCOUNT_ID}"}}]}}\'',
            "bash deploy/prod/lanes/runner-env-write.sh",
            "HOST=orin bash deploy/prod/lanes/runner-env-write.sh",
            'ssh thor "systemctl --user restart nodes-runner"',
            'ssh orin "systemctl --user restart nodes-runner"',
        ),
        "invoke the lane through bash: it is not executable. Run this way it has no"
        " deploy.sh around it, so it supplies deploy.sh's own shell options and helpers"
        " itself — it takes the same timestamped runner.env backup, prints the restore"
        " command, and a clean re-grant exits 0 (before 0.47.4 it exited 127 on success"
        " and took no backup). It reads HOST, REVISION, NODES_API_URL, PR_UPKEEP_REPOSITORIES and"
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

ROTATION = (
    "mint a new token in the admin UI, revoke the old one there, 'nodes jira-token seal'"
    " (overwrites the sealed secret), repeat steps 2-5"
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
        "secret_store": SECRET_STORE,
        "secret_name": SECRET_NAME,
        "seal": "nodes jira-token seal",
        "inject": f"{INJECT_PREFIX} <cmd>",
        "env_keys": [ENV_EMAIL, ENV_TOKEN, ENV_BASE],
        "doc": DOC_PATH,
    }


def _mint_text() -> str:
    return "\n".join(
        [
            f"service account: {SERVICE_ACCOUNT_NAME} ({SERVICE_ACCOUNT_EMAIL})",
            f"accountId: {SERVICE_ACCOUNT_ID}  (verify refuses a token for any other account)",
            f"mint at: {ADMIN_PATH}",
            "  (no API mints a service-account token; this CLI cannot mint one either)",
            f"api base: {GATEWAY_BASE}",
            f"  (the site URL {SITE_URL} answers 401 for a service-account token;"
            " only the gateway base answers 200)",
            f"seal it: nodes jira-token seal  (stores it hidden in {SECRET_STORE} as"
            f" {SECRET_NAME}; no plaintext file on spark)",
            f"consume it: {INJECT_PREFIX} <cmd>",
            f"  ({ENV_EMAIL} and {ENV_BASE} are not secrets; {ENV_EMAIL} defaults to"
            f" the address above and {ENV_BASE} is pinned to the base above — verify"
            " sends the credential nowhere else)",
            f"next: 'nodes jira-token seal', then '{INJECT_PREFIX} nodes jira-token verify',"
            " then 'nodes jira-token install'",
            f"doc: {DOC_PATH}",
        ]
    )


def cmd_mint(args: argparse.Namespace) -> int:
    json_mode = bool(getattr(args, "json", False))
    emit_result(_mint_payload() if json_mode else _mint_text(), json_mode=json_mode)
    return 0


# --- seal --------------------------------------------------------------------


def _prompt_token(prompt: str) -> str:
    """Read the token without echo on a TTY, or one line of stdin otherwise."""
    if sys.stdin.isatty():
        return getpass.getpass(prompt).strip()
    return sys.stdin.readline().rstrip("\r\n").strip()


def _scrub(text: str, token: str) -> str:
    return text.replace(token, _REDACTED) if token else text


def _seal(token: str) -> None:
    """``grant set SECRET_NAME - --hidden ...`` with the token on stdin only."""
    grant = shutil.which(GRANT_BIN)
    if grant is None:
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"{GRANT_BIN} (the secrets manager) is not on PATH",
            remediation=(
                f"uv tool install {GRANT_BIN} (or add the operator's install path, e.g."
                " ~/.local/bin, to PATH), then rerun 'nodes jira-token seal'"
            ),
        )
    argv = [
        grant,
        "set",
        SECRET_NAME,
        "-",
        "--hidden",
        "--purpose",
        GRANT_PURPOSE,
        "--rotate-howto",
        GRANT_ROTATE_HOWTO,
    ]
    try:
        completed = subprocess.run(  # nosec B603 - literal argv, shell=False
            argv,
            input=token,
            capture_output=True,
            text=True,
            check=False,
        )
    except OSError as err:
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"could not run {GRANT_BIN}: {_scrub(str(err), token)}",
            remediation=f"check that {grant} is executable, then rerun 'nodes jira-token seal'",
        ) from None
    if completed.returncode != 0:
        stderr = _scrub(completed.stderr.strip(), token) or "(no stderr)"
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"{GRANT_BIN} set exited {completed.returncode}: {stderr!r}",
            remediation=f"run '{GRANT_BIN} doctor' and '{GRANT_BIN} explain set', then rerun",
        )


def cmd_seal(args: argparse.Namespace) -> int:
    json_mode = bool(getattr(args, "json", False))
    token = _prompt_token("Jira service-account token (no echo): ")
    if not token:
        raise CliError(
            code=EXIT_USER_ERROR,
            message="empty token; nothing sealed",
            remediation=(
                "paste the token minted in the admin UI at the prompt, or pipe it:"
                ' printf %s "$TOKEN" | nodes jira-token seal'
            ),
        )
    _seal(token)
    next_command = f"{INJECT_PREFIX} nodes jira-token verify"
    if json_mode:
        emit_result(
            {
                "sealed": SECRET_NAME,
                "hidden": True,
                "secret_store": SECRET_STORE,
                "next": next_command,
            },
            json_mode=True,
        )
    else:
        emit_result(f"sealed: {SECRET_NAME} (hidden)\nnext: {next_command}", json_mode=False)
    return 0


# --- verify ------------------------------------------------------------------


def _read_env() -> tuple[str, str, str]:
    """Email defaults to the constant, the base is pinned, the token is the secret.

    The base is settled *before* the token is read, so a base this command
    would refuse never gets as far as prompting an operator for a secret.
    """
    email = os.environ.get(ENV_EMAIL, "") or SERVICE_ACCOUNT_EMAIL
    base = _pinned_base((os.environ.get(ENV_BASE, "") or GATEWAY_BASE).rstrip("/"))
    token = os.environ.get(ENV_TOKEN, "")
    if not token and sys.stdin.isatty():
        token = getpass.getpass("Jira service-account token (no echo): ").strip()
    if not token:
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"{ENV_TOKEN} is not set",
            remediation=(
                f"{INJECT_PREFIX} nodes jira-token verify  (seal it first with"
                " 'nodes jira-token seal' if 'grant show"
                f" {SECRET_NAME}' fails)"
            ),
        )
    return email, token, base


def _base_identity(base: str) -> tuple[str, str, str]:
    """Scheme/host/path of a base, so only cosmetic differences compare equal."""
    parsed = urllib.parse.urlsplit(base.rstrip("/"))
    return parsed.scheme.lower(), parsed.netloc.lower(), parsed.path.rstrip("/")


def _pinned_base(base: str) -> str:
    """Refuse every base but the gateway — the credential goes nowhere else.

    ``verify`` attaches the service-account email and token as Basic auth to
    whatever base it is handed, so an environment that can set ``JIRA_API_BASE``
    but cannot read the sealed grant secret would otherwise be able to
    exfiltrate the token by naming a host of its own. Checking the scheme does
    not stop that: the base is pinned to ``GATEWAY_BASE`` — host *and* path —
    which costs nothing, because that gateway is the only address this
    account's token authenticates at anyway. Returns the canonical spelling,
    so everything downstream — the request, the errors, ``--json`` — names one
    address rather than whatever variant the environment happened to hold.
    """
    if _base_identity(base) != _base_identity(GATEWAY_BASE):
        raise CliError(
            code=EXIT_USER_ERROR,
            message=f"{ENV_BASE} must be the gateway base {GATEWAY_BASE}, not {base}",
            remediation=(
                f"export {ENV_BASE}={GATEWAY_BASE}, or unset it (verify defaults to the"
                f" gateway). The site URL {SITE_URL} answers 401 for a service-account"
                " token, and no other host is ever sent the credential"
            ),
        )
    return GATEWAY_BASE


def _myself(email: str, token: str, base: str) -> dict[str, object]:
    """One GET against ``<base>/rest/api/3/myself``; the token lives only here."""
    credential = base64.b64encode(f"{email}:{token}".encode()).decode("ascii")
    request = urllib.request.Request(
        base + MYSELF_PATH,
        headers={"Authorization": f"Basic {credential}", "Accept": "application/json"},
        method="GET",
    )
    try:
        # _pinned_base has already pinned the base to the https:// gateway,
        # so the URL reaching urlopen is a known constant, not env input.
        response = urllib.request.urlopen(request, timeout=_VERIFY_TIMEOUT_SECONDS)  # nosec B310
        with response:
            raw = response.read()
    except urllib.error.HTTPError as err:
        _raise_http(err.code, base)
    except OSError as err:  # URLError and TimeoutError both derive from OSError
        reason = getattr(err, "reason", None) or err.__class__.__name__
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=f"could not reach {base}{MYSELF_PATH}: {reason}",
            remediation="check network access to api.atlassian.com and retry",
        ) from None
    try:
        payload = json.loads(raw.decode("utf-8"))
    except ValueError:  # UnicodeDecodeError derives from ValueError
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
                f"the base is pinned to the gateway {GATEWAY_BASE} (the site URL"
                f" {SITE_URL} answers 401 for a service-account token, which is why"
                " nothing else is accepted), so a 401 here means the token was revoked"
                " or mistyped: re-mint it ('nodes jira-token mint') and re-seal it"
                " ('nodes jira-token seal')"
            ),
        ) from None
    raise CliError(
        code=EXIT_ENV_ERROR,
        message=f"{base}{MYSELF_PATH} answered HTTP {status}",
        remediation="retry; if it persists, check https://status.atlassian.com",
    ) from None


def _check_identity(account_id: str) -> None:
    """Refuse a token that authenticates as anything but the service account.

    A 200 proves the credential is *valid*, not that it is *ours*. Any Jira
    token — an operator's personal one, a second service account — answers 200
    at this gateway with its own ``accountId``, and every step after verify
    treats the pair as the bot's: ``install-secrets.sh`` writes it to both
    runners, and the sweep filters its own Jira comments by
    ``jira_bot_account_id``, which is this constant. Installed under the wrong
    account, the sweep would read its own comments back as human facts. So the
    id is compared here rather than left to the operator reading step 2's
    output. Account ids are not secrets (this one is a source constant), so the
    mismatch names both.
    """
    if account_id != SERVICE_ACCOUNT_ID:
        raise CliError(
            code=EXIT_ENV_ERROR,
            message=(
                f"{ENV_TOKEN} authenticates as accountId {account_id}, not the"
                f" {SERVICE_ACCOUNT_NAME} service account {SERVICE_ACCOUNT_ID}"
            ),
            remediation=(
                "the token is valid but belongs to another Jira account; do not install it"
                f" (the sweep filters its own comments by {SERVICE_ACCOUNT_ID}). Mint one at"
                f" {ADMIN_PATH} and re-seal it with 'nodes jira-token seal'"
            ),
        )


def cmd_verify(args: argparse.Namespace) -> int:
    json_mode = bool(getattr(args, "json", False))
    email, token, base = _read_env()
    payload = _myself(email, token, base)
    account_id = str(payload["accountId"])
    _check_identity(account_id)
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
        "rotation": ROTATION,
        "secret_store": SECRET_STORE,
        "secret_name": SECRET_NAME,
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
    lines.append(f"rotation: {ROTATION}")
    lines.append(f"doc: {DOC_PATH}")
    return "\n".join(lines)


def cmd_install(args: argparse.Namespace) -> int:
    json_mode = bool(getattr(args, "json", False))
    emit_result(_install_payload() if json_mode else _install_text(), json_mode=json_mode)
    return 0


# --- registration ------------------------------------------------------------


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes jira-token {mint,seal,verify,install} ...\n"
        "run 'nodes explain jira-token' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "jira-token",
        help="Runbook for the Jira service-account token: mint, seal, verify, install.",
    )
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="jira_token_command", parser_class=type(p))

    mint = noun_sub.add_parser(
        "mint", help="Print where the token is minted and how it is sealed and consumed."
    )
    mint.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    mint.set_defaults(func=cmd_mint)

    seal = noun_sub.add_parser(
        "seal",
        help=(
            f"Read the token (no echo, or one line of stdin) and store it hidden in grant"
            f" as {SECRET_NAME}."
        ),
    )
    seal.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    seal.set_defaults(func=cmd_seal)

    verify = noun_sub.add_parser(
        "verify",
        help=(
            "GET /rest/api/3/myself at the gateway base with JIRA_API_TOKEN from the"
            " environment (grant run --inject)."
        ),
    )
    verify.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    verify.set_defaults(func=cmd_verify)

    install = noun_sub.add_parser(
        "install", help="Print the ordered install sequence for thor and orin (does not run it)."
    )
    install.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    install.set_defaults(func=cmd_install)
