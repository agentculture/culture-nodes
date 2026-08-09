"""``nodes human-tasks`` — thin REST client over the human tasks API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``human-tasks`` tag): list, get, decide. No
approval-node logic lives in this module (spec decision c28) — outcome
validation against ``allowed_outcomes``, ledger-version guarding, and edge
routing all happen server-side (PRD §9.9).

``decide`` is the one operation in this API that is not authless (spec
decision c45): it requires a bearer token matching the server's configured
``NODES_HUMAN_DECISION_TOKEN_SECRET``. This module resolves that token from
``--token`` or the ``NODES_HUMAN_DECISION_TOKEN`` environment variable and
never logs, prints, or otherwise surfaces it — not in text output, not in
``--json`` output, not in an error message.
"""

from __future__ import annotations

import argparse
import os

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._errors import EXIT_USER_ERROR, CliError
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result

#: Env var carrying the decision bearer token (second in the resolution
#: order, after --token). Distinct from the server-side
#: NODES_HUMAN_DECISION_TOKEN_SECRET the API validates it against.
#: This is an env var *name*, not a credential value — bandit's B105
#: heuristic flags it anyway because the string contains "TOKEN".
ENV_DECISION_TOKEN = "NODES_HUMAN_DECISION_TOKEN"  # nosec B105

_HUMAN_TASK_ID_HELP = "The human task id."


def _split_ids(raw: str | None) -> list[str]:
    if not raw:
        return []
    return [item.strip() for item in raw.split(",") if item.strip()]


def _resolve_decision_token(args: argparse.Namespace) -> str:
    """Resolve the decision bearer token: ``--token`` then ``$NODES_HUMAN_DECISION_TOKEN``.

    Never returns an empty string — raises :class:`CliError` instead, and the
    error message/remediation name the env var, never a token value (there is
    none to name at this point).
    """
    token = getattr(args, "token", None)
    if token:
        return token
    env_token = os.environ.get(ENV_DECISION_TOKEN)
    if env_token:
        return env_token
    raise CliError(
        code=EXIT_USER_ERROR,
        message="no decision token available for POST /v1alpha1/human-tasks/{id}/decision",
        remediation=f"pass --token or set ${ENV_DECISION_TOKEN}",
    )


def cmd_human_tasks_list(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET", f"{API_PREFIX}/human-tasks", query={"status": args.status, "limit": args.limit}
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        items = (resp.payload or {}).get("items") or []
        if not items:
            emit_result("no human tasks", json_mode=False)
        else:
            lines = [
                f"{item.get('id', '')}  {item.get('status', '')}  "
                f"{item.get('run_id', '')}  {item.get('created_at', '')}"
                for item in items
            ]
            emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_human_tasks_get(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/human-tasks/{args.id}")
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        text = (
            f"id: {payload.get('id', '')}\n"
            f"run_id: {payload.get('run_id', '')}\n"
            f"status: {payload.get('status', '')}\n"
            f"kind: {payload.get('kind', '')}\n"
            f"created_at: {payload.get('created_at', '')}"
        )
        emit_result(text, json_mode=False)
    return 0


def cmd_human_tasks_decide(args: argparse.Namespace) -> int:
    token = _resolve_decision_token(args)
    body: dict[str, object] = {
        "outcome": args.outcome,
        "decider_actor_id": args.decider_actor_id,
        "expected_ledger_version": args.expected_ledger_version,
    }
    if args.note:
        body["response"] = {"note": args.note}
    record_ids = _split_ids(args.record_ids)
    if record_ids:
        body["record_ids"] = record_ids

    client = client_from_args(args)
    # A 401 (missing/wrong token) or 409 (already decided, stale ledger
    # version) is relayed by ApiClient as a CliError carrying the API's own
    # code/message/remediation verbatim — the token itself never appears in
    # that body, so no scrubbing is needed here.
    resp = client.request(
        "POST",
        f"{API_PREFIX}/human-tasks/{args.id}/decision",
        json_body=body,
        headers={"Authorization": f"Bearer {token}"},
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        text = (
            f"human_task_id: {payload.get('human_task_id', '')}\n"
            f"run_id: {payload.get('run_id', '')}\n"
            f"outcome: {payload.get('outcome', '')}\n"
            f"run_state: {payload.get('run_state', '')}"
        )
        emit_result(text, json_mode=False)
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes human-tasks {list,get,decide} ...\n"
        "run 'nodes explain human-tasks' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser("human-tasks", help="Thin client for the human tasks API (list/get/decide).")
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="human_tasks_command", parser_class=type(p))

    listp = noun_sub.add_parser("list", help="List human tasks, newest first.")
    listp.add_argument(
        "--status",
        dest="status",
        default=None,
        choices=["pending", "decided"],
        help="Filter to pending or decided. Omit for every status.",
    )
    listp.add_argument("--limit", type=int, default=None, help="Max items to return.")
    listp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(listp)
    listp.set_defaults(func=cmd_human_tasks_list)

    getp = noun_sub.add_parser("get", help="Fetch one human task.")
    getp.add_argument("id", help=_HUMAN_TASK_ID_HELP)
    getp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(getp)
    getp.set_defaults(func=cmd_human_tasks_get)

    decide = noun_sub.add_parser(
        "decide", help="Commit a decision on a paused human task, resuming the run."
    )
    decide.add_argument("id", help=_HUMAN_TASK_ID_HELP)
    decide.add_argument(
        "--outcome",
        dest="outcome",
        required=True,
        help="The domain outcome selected. Must be one of the task's allowed_outcomes.",
    )
    decide.add_argument(
        "--decider-actor-id",
        dest="decider_actor_id",
        required=True,
        help="The human actor this decision is attributed to.",
    )
    decide.add_argument(
        "--expected-ledger-version",
        dest="expected_ledger_version",
        type=int,
        required=True,
        help="The run's ledger version last read. A mismatch is refused with 409.",
    )
    decide.add_argument(
        "--note",
        dest="note",
        default=None,
        help="Freeform note, recorded as the decision's response payload ({'note': TEXT}).",
    )
    decide.add_argument(
        "--record-ids",
        dest="record_ids",
        default=None,
        help="Comma-separated other ledger record ids to confirm alongside this decision.",
    )
    decide.add_argument(
        "--token",
        dest="token",
        default=None,
        help=f"Decision bearer token (default: ${ENV_DECISION_TOKEN}). Never logged.",
    )
    decide.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(decide)
    decide.set_defaults(func=cmd_human_tasks_decide)
