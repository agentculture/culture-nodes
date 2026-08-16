"""``nodes actors`` — thin REST client over the actors API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``actors`` tag): list, get, resume. No engine
logic lives in this module (spec decision c28) — the capacity circuit
breaker's state is computed and cleared server-side.

``resume`` is the operator's lane out of a capacity pause (task t9, honesty
condition h38): when a dispatch to an actor fails with the
``capacity_exhausted`` §13.5 class, the worker marks that actor unavailable
until a deadline and defers work addressed to it instead of failing it. This
verb ends that pause early — the point being that an operator who knows
better than the automatic classification never has to reach for psql.

Like ``human-tasks decide``, ``resume`` is not authless (spec decision c45):
it requires the actor-registration bearer token, resolved from ``--token``
or the ``NODES_ACTOR_REGISTRATION_TOKEN`` environment variable and never
logged, printed, or included in ``--json`` output.
"""

from __future__ import annotations

import argparse
import os

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._errors import EXIT_USER_ERROR, CliError
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result

#: Env var carrying the actor-registration bearer token (second in the
#: resolution order, after --token). Distinct from the server-side
#: NODES_ACTOR_REGISTRATION_TOKEN_SECRET the API validates it against.
#: This is an env var *name*, not a credential value — bandit's B105
#: heuristic flags it anyway because the string contains "TOKEN".
ENV_REGISTRATION_TOKEN = "NODES_ACTOR_REGISTRATION_TOKEN"  # nosec B105

_ACTOR_ID_HELP = "The actor row id (see 'nodes actors list')."


def _resolve_registration_token(args: argparse.Namespace) -> str:
    """Resolve the bearer token: ``--token`` then ``$NODES_ACTOR_REGISTRATION_TOKEN``.

    Never returns an empty string — raises :class:`CliError` instead, and the
    message names the env var, never a token value (there is none to name at
    this point).
    """
    token = getattr(args, "token", None)
    if token:
        return token
    env_token = os.environ.get(ENV_REGISTRATION_TOKEN)
    if env_token:
        return env_token
    raise CliError(
        code=EXIT_USER_ERROR,
        message="no bearer token available for POST /v1alpha1/actors/{id}/resume",
        remediation=f"pass --token or set ${ENV_REGISTRATION_TOKEN}",
    )


def _availability_lines(availability: object) -> list[str]:
    """Render the capacity-breaker block, or nothing when the actor has none.

    An absent block means the actor has never been paused, which is a
    different fact from a lapsed pause (``paused: false``) — so absence
    renders as absence, never as ``paused: false``.
    """
    if not isinstance(availability, dict):
        return []
    lines = [f"availability.paused: {'yes' if availability.get('paused') else 'no'}"]
    for key in ("reason", "paused_until", "retry_after_seconds", "detail"):
        value = availability.get(key)
        if value is not None:
            lines.append(f"availability.{key}: {value}")
    for key in ("tripped_by_run_id", "tripped_by_attempt_id", "cleared_at", "cleared_by"):
        value = availability.get(key)
        if value:
            lines.append(f"availability.{key}: {value}")
    return lines


def _dispatch_rate_lines(rate: object) -> list[str]:
    """Render the pacing control's state for this actor, or nothing when it has none.

    Task t10: a worker can hold itself to a declared session rate, per actor
    and globally, against durable state every worker shares. Absence means no
    declared rate has ever admitted a dispatch to this actor — which is a
    different fact from a rate with nothing consumed yet, so absence renders
    as absence rather than as a zeroed rate.

    ``remaining`` is the number an operator planning a wave actually wants:
    how many more dispatches this actor may take before the window resets,
    which shrinks as the window runs out even when nothing has been consumed
    (the rate is spread across the REMAINING window). The global rate is not
    shown here because it is not this actor's — ``GET
    /v1alpha1/dispatch-rates`` carries every scope.
    """
    if not isinstance(rate, dict):
        return []
    lines = [
        f"dispatch_rate.limit_per_window: {rate.get('limit_per_window', '')}",
        f"dispatch_rate.dispatched: {rate.get('dispatched', '')}",
        f"dispatch_rate.remaining: {rate.get('remaining', '')}",
    ]
    for key in ("window_ends_at", "next_dispatch_at", "last_dispatch_at"):
        value = rate.get(key)
        if value:
            lines.append(f"dispatch_rate.{key}: {value}")
    return lines


def _actor_line(item: dict) -> str:
    availability = item.get("availability")
    state = "available"
    if isinstance(availability, dict) and availability.get("paused"):
        until = availability.get("paused_until", "")
        reason = availability.get("reason", "")
        state = f"PAUSED until {until} ({reason})"
    return (
        f"{item.get('id', '')}  {item.get('actor_key', '')}  r{item.get('revision', '')}  "
        f"{item.get('kind', '')}  {state}"
    )


def cmd_actors_list(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/actors")
    if bool(getattr(args, "json", False)):
        emit_json_passthrough(resp.raw)
        return 0
    items = (resp.payload or {}).get("items") or []
    if not items:
        emit_result("no actors", json_mode=False)
        return 0
    if getattr(args, "paused_only", False):
        items = [
            item
            for item in items
            if isinstance(item.get("availability"), dict) and item["availability"].get("paused")
        ]
        if not items:
            emit_result("no paused actors", json_mode=False)
            return 0
    emit_result("\n".join(_actor_line(item) for item in items), json_mode=False)
    return 0


def cmd_actors_get(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/actors/{args.id}")
    if bool(getattr(args, "json", False)):
        emit_json_passthrough(resp.raw)
        return 0
    payload = resp.payload or {}
    lines = [
        f"id: {payload.get('id', '')}",
        f"actor_key: {payload.get('actor_key', '')}",
        f"revision: {payload.get('revision', '')}",
        f"kind: {payload.get('kind', '')}",
        f"protocol: {payload.get('protocol', '')}",
    ]
    lines.extend(_availability_lines(payload.get("availability")))
    lines.extend(_dispatch_rate_lines(payload.get("dispatch_rate")))
    emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_actors_resume(args: argparse.Namespace) -> int:
    token = _resolve_registration_token(args)
    body: dict[str, object] = {}
    if args.cleared_by:
        body["cleared_by"] = args.cleared_by

    client = client_from_args(args)
    # A 401 (missing/wrong token) or 404 (unknown actor) is relayed by
    # ApiClient as a CliError carrying the API's own code/message/remediation
    # verbatim — the token never appears in that body, so nothing needs
    # scrubbing here.
    resp = client.request(
        "POST",
        f"{API_PREFIX}/actors/{args.id}/resume",
        json_body=body,
        headers={"Authorization": f"Bearer {token}"},
    )
    if bool(getattr(args, "json", False)):
        emit_json_passthrough(resp.raw)
        return 0
    payload = resp.payload or {}
    lines = [
        f"id: {payload.get('id', '')}",
        f"actor_key: {payload.get('actor_key', '')}",
    ]
    availability = payload.get("availability")
    if isinstance(availability, dict):
        lines.extend(_availability_lines(availability))
    else:
        # Nothing was paused. Saying so beats printing an empty block that
        # reads like a cleared pause.
        lines.append("availability: none recorded (this actor was not paused)")
    emit_result("\n".join(lines), json_mode=False)
    return 0


#: The three presence states GET /v1alpha1/dial-in-presence reports, and how
#: each renders in text. Absence is SHOUTED and connectedness is not: the
#: operator reading this view is looking for what is wrong, and today ten of
#: eleven actors are absent, so the exceptional rows have to be the legible
#: ones. `never_dialled` and `disconnected` are deliberately different words —
#: a bridge nobody ever configured and a bridge that died an hour ago are
#: different problems (task t6, issue #136).
_PRESENCE_LABELS = {
    "connected": "connected",
    "disconnected": "DISCONNECTED",
    "never_dialled": "NEVER DIALLED",
}


def _credential_note(credential: object) -> str:
    """Name the credential-side reason an actor is absent, or "".

    An actor can look absent for four unrelated reasons: its process is down,
    its credential was revoked, it is locked out after repeated failures, or
    it was never issued a control-plane credential. Only the first is an
    outage, and they are indistinguishable in presence alone — which is
    exactly the confusion an operator debugging at 03:00 cannot afford.
    """
    if credential is None:
        return "no credential record (this actor cannot dial in)"
    if not isinstance(credential, dict):
        return ""
    if credential.get("revoked"):
        return f"REVOKED at {credential.get('revoked_at', 'an unrecorded instant')}"
    if credential.get("locked_out"):
        return (
            f"LOCKED OUT until {credential.get('locked_until', '?')} "
            f"after {credential.get('failure_count', 0)} failures"
        )
    if not credential.get("issued"):
        return "credential not control-plane issued (dials are refused)"
    return ""


def _presence_line(item: dict) -> str:
    presence = str(item.get("presence", ""))
    label = _PRESENCE_LABELS.get(presence, presence or "unknown")
    parts = [f"{item.get('actor_key', ''):<34} {label:<13}"]

    last_seen = item.get("last_seen_at")
    if last_seen:
        seconds = item.get("seconds_since_last_seen")
        ago = f" ({int(seconds)}s ago)" if isinstance(seconds, (int, float)) else ""
        parts.append(f"last seen {last_seen}{ago}")
    elif presence == "never_dialled":
        # No instant to report, and none is invented: a fabricated one would
        # read as "seen just now", which is the opposite of the truth.
        parts.append("never seen")

    note = _credential_note(item.get("credential"))
    if note:
        parts.append(note)
    return "  ".join(parts).rstrip()


def cmd_actors_dial_in(args: argparse.Namespace) -> int:
    """Render current dial-in presence — who is connected right now.

    One GET, nothing dispatched, nothing probed. The control plane holds no
    address for a bridge (issue #121 retires the stored one), so this cannot
    be a reachability probe and must not become one: presence is a fact
    PostgreSQL already holds, written by the bridge's own poll.
    """
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/dial-in-presence")

    # One emit, one return. Every path below is a success by construction: a
    # failed request raises CliError before this line, per _errors.py's
    # exit-code policy, so the exit code is never in question and only the
    # rendering differs. Written with a single exit so the "always returns the
    # same value" reading is true because the function HAS one return, not
    # because four of them happen to agree.
    if bool(getattr(args, "json", False)):
        emit_json_passthrough(resp.raw)
    else:
        emit_result(
            _dial_in_text(
                resp.payload or {},
                absent_only=bool(getattr(args, "absent_only", False)),
            ),
            json_mode=False,
        )
    return 0


def _dial_in_text(payload: dict, *, absent_only: bool) -> str:
    """The text rendering of a dial-in presence payload."""
    items = payload.get("items") or []
    if not items:
        return "no registered actors in this namespace"

    window = payload.get("window_seconds", "?")
    header = (
        f"observed at {payload.get('observed_at', '?')} "
        f"(connected = polled within {window}s, the same window dispatch uses)\n"
        f"{payload.get('connected', 0)} connected, "
        f"{payload.get('disconnected', 0)} disconnected, "
        f"{payload.get('never_dialled', 0)} never dialled"
    )
    if absent_only:
        items = [item for item in items if item.get("presence") != "connected"]
        if not items:
            return f"{header}\n\nevery registered actor is dialled in"
    return "\n".join([header, "", *(_presence_line(item) for item in items)])


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes actors {list,get,resume,dial-in} ...\n"
        "run 'nodes explain actors' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "actors",
        help="Thin client for the actors API (list/get/resume a pause/dial-in presence).",
    )
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="actors_command", parser_class=type(p))

    listp = noun_sub.add_parser(
        "list", help="List every registered actor row, with any capacity pause."
    )
    listp.add_argument(
        "--paused-only",
        dest="paused_only",
        action="store_true",
        help="Only actors whose capacity pause is currently in force (text mode).",
    )
    listp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(listp)
    listp.set_defaults(func=cmd_actors_list, paused_only=False)

    getp = noun_sub.add_parser(
        "get", help="Fetch one actor row, its availability, and its dispatch rate."
    )
    getp.add_argument("id", help=_ACTOR_ID_HELP)
    getp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(getp)
    getp.set_defaults(func=cmd_actors_get)

    resume = noun_sub.add_parser(
        "resume", help="Clear an actor's capacity pause early, making it dispatchable again."
    )
    resume.add_argument("id", help=_ACTOR_ID_HELP)
    resume.add_argument(
        "--cleared-by",
        dest="cleared_by",
        default=None,
        help="Who is clearing the pause, recorded on the availability row (default: operator).",
    )
    resume.add_argument(
        "--token",
        dest="token",
        default=None,
        help=f"Registration bearer token (default: ${ENV_REGISTRATION_TOKEN}). Never logged.",
    )
    resume.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(resume)
    resume.set_defaults(func=cmd_actors_resume)

    dialin = noun_sub.add_parser(
        "dial-in",
        help="Show which bridges are dialled in right now (read-only; nothing is dispatched).",
    )
    dialin.add_argument(
        "--absent-only",
        dest="absent_only",
        action="store_true",
        help="Only actors that are NOT currently dialled in (text mode).",
    )
    dialin.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(dialin)
    dialin.set_defaults(func=cmd_actors_dial_in, absent_only=False)
