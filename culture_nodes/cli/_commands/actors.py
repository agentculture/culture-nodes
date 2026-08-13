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


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes actors {list,get,resume} ...\nrun 'nodes explain actors' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "actors", help="Thin client for the actors API (list/get/resume a capacity pause)."
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

    getp = noun_sub.add_parser("get", help="Fetch one actor row and its availability.")
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
