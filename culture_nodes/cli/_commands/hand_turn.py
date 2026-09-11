"""``nodes hand-turn`` — record one hand-turn against a work item.

One HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``hand-turns`` tag): ``POST
/v1alpha1/hand-turns``. No ledger authority logic lives in this module
(spec decision c28) — the record ALWAYS lands ``proposed`` under the
recording actor's identity (issue #319, decision c25) and reaches
``confirmed`` only through ``nodes review create`` / ``commit`` on the work
item's run. Like ``human-tasks decide`` the route is a decision surface, so
it carries the decision bearer (``--token`` or ``$NODES_HUMAN_DECISION_TOKEN``);
an agent observer uses its own actor bearer the same way.
"""

from __future__ import annotations

import argparse
import os

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._errors import EXIT_USER_ERROR, CliError
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result

#: Env var carrying the decision bearer token (second in the resolution
#: order, after --token) — the same variable ``human-tasks decide`` reads.
ENV_DECISION_TOKEN = "NODES_HUMAN_DECISION_TOKEN"  # nosec B105 - a variable NAME, not a secret
#: Env var naming the actor a hand-turn is recorded as when ``--as`` is
#: omitted — the person's own actor id, or the observer's.
ENV_ACTOR_ID = "NODES_HAND_TURN_ACTOR_ID"


def _resolve_token(args: argparse.Namespace) -> str:
    token = getattr(args, "token", None) or os.environ.get(ENV_DECISION_TOKEN)
    if token:
        return token
    raise CliError(
        code=EXIT_USER_ERROR,
        message="no bearer token available for POST /v1alpha1/hand-turns",
        remediation=f"pass --token or set ${ENV_DECISION_TOKEN}",
    )


def _resolve_actor(args: argparse.Namespace) -> str:
    actor = getattr(args, "as_actor", None) or os.environ.get(ENV_ACTOR_ID)
    if actor:
        return actor
    raise CliError(
        code=EXIT_USER_ERROR,
        message="no actor to record the hand-turn as",
        remediation=f"pass --as <actor-id> (your registered actor) or set ${ENV_ACTOR_ID}",
    )


def cmd_hand_turn(args: argparse.Namespace) -> int:
    body: dict[str, object] = {
        "what": args.what,
        "stage": args.stage,
        "work_item": args.work_item,
        "actor_id": _resolve_actor(args),
    }
    if args.run_id:
        body["run_id"] = args.run_id
    if args.definition_ref:
        body["definition_ref"] = args.definition_ref
    if args.rule:
        body["rule"] = args.rule
    if args.evidence:
        body["evidence_refs"] = list(args.evidence)
    token = _resolve_token(args)
    client = client_from_args(args)
    # A 404 (no run carries the work item) or 401 (wrong token) is relayed by
    # ApiClient as a CliError carrying the API's own remediation verbatim; the
    # token itself never appears in output.
    resp = client.request(
        "POST",
        f"{API_PREFIX}/hand-turns",
        json_body=body,
        headers={"Authorization": f"Bearer {token}"},
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        data = payload.get("data") or {}
        origin = payload.get("origin") or {}
        lines = [
            f"id: {payload.get('id', '')}",
            f"authority: {payload.get('authority', '')}",
            f"origin: {origin.get('kind', '')} ({origin.get('actor_id', '')})",
            f"run_id: {payload.get('run_id', '')}",
            f"stage: {data.get('stage', '')}",
            f"work_item: {data.get('work_item', '')}",
            f"what: {data.get('what', '')}",
        ]
        if data.get("definition_ref"):
            lines.append(f"definition_ref: {data['definition_ref']}")
        lines.append("confirm it through: nodes review create <run-id> --records <id> ...")
        emit_result("\n".join(lines), json_mode=False)
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "hand-turn",
        help=(
            "Record one hand-turn (a step a person did by hand) against a work item; "
            "lands proposed."
        ),
    )
    p.add_argument("what", help="What was done by hand, in one line.")
    p.add_argument(
        "--stage",
        dest="stage",
        required=True,
        help=(
            "Loop stage from the confirmed hand_turn_definition "
            "(e.g. dispatch, land, review, cleanup)."
        ),
    )
    p.add_argument(
        "--work-item",
        dest="work_item",
        required=True,
        help=(
            "Work-item key the turn served (e.g. SCRUM-9); the record files against "
            "its newest run."
        ),
    )
    p.add_argument(
        "--as",
        dest="as_actor",
        default=None,
        help=(
            f"Actor id to record as (default: ${ENV_ACTOR_ID}). Its kind decides the "
            "origin; authority is always proposed."
        ),
    )
    p.add_argument(
        "--run",
        dest="run_id",
        default=None,
        help=(
            "File against this run instead of the newest one carrying --work-item "
            "(must carry it too)."
        ),
    )
    p.add_argument(
        "--definition-ref",
        dest="definition_ref",
        default=None,
        help="Ledger id of the hand_turn_definition the turn is recognised against.",
    )
    p.add_argument("--rule", dest="rule", default=None, help="Definition rule id that matched.")
    p.add_argument(
        "--evidence",
        dest="evidence",
        action="append",
        default=None,
        metavar="REF",
        help="Pointer to what was seen (commit sha, comment URL); repeatable.",
    )
    p.add_argument(
        "--token",
        dest="token",
        default=None,
        help=f"Decision bearer token (default: ${ENV_DECISION_TOKEN}). Never logged.",
    )
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(p)
    p.set_defaults(func=cmd_hand_turn)
