"""``nodes dispatch`` — thin REST client over the clarify-then-commit gate.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``preflights`` tag): pending, show, confirm.
None of the gate's logic lives in this module (spec decision c28) — the
briefing is composed at the dispatch site, and the single-use/windowed
refusals are decided server-side (issue #67, task t14).

``confirm`` is the second, separate action of clarify-then-commit: it
records the acknowledging actor's *proposed* claim to have read the briefing
and turns the composed ``verdict: hold`` into a dispatch. It is the verb
issue #67 names, and the one ``acknowledgement.verb`` in every composed
document points the reader at.

The verb is deliberately usable by a human operator as well as by a bridge:
``pending`` answers "what is waiting", ``show`` prints the briefing itself
so it can be *read* before it is confirmed, and only then does ``confirm``
commit. A confirm that fires without a show is a keystroke, not an
acknowledgement — which is exactly what this gate exists to stop being the
norm.
"""

from __future__ import annotations

import argparse
import json

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result

_PREFLIGHT_ID_HELP = (
    "The preflight id, from `nodes dispatch pending` or the run's preflight_issued event."
)


def cmd_dispatch_pending(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET",
        f"{API_PREFIX}/preflights",
        query={"actor_key": args.actor_key, "limit": args.limit},
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        items = (resp.payload or {}).get("items") or []
        if not items:
            emit_result("no preflights waiting to be acknowledged", json_mode=False)
        else:
            lines = [
                f"{item.get('id', '')}  {item.get('actor_key', '')}  "
                f"{item.get('node_id', '')}  expires {item.get('expires_at', '')}"
                for item in items
            ]
            emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_dispatch_show(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/preflights/{args.id}")
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        document = payload.get("document") or {}
        # The document is printed in full rather than summarised: it IS the
        # briefing, and a reader who only ever sees a four-line digest of it
        # has been told about a briefing rather than given one.
        text = (
            f"id: {payload.get('id', '')}\n"
            f"actor_key: {payload.get('actor_key', '')}\n"
            f"node_id: {payload.get('node_id', '')}\n"
            f"run_id: {payload.get('run_id', '')}\n"
            f"record_id: {payload.get('record_id', '')}\n"
            f"expires_at: {payload.get('expires_at', '')}\n"
            f"acknowledged: {str(bool(payload.get('acknowledged'))).lower()}\n"
            f"expired: {str(bool(payload.get('expired'))).lower()}\n"
            "\n"
            f"{json.dumps(document, indent=2, sort_keys=True)}"
        )
        emit_result(text, json_mode=False)
    return 0


def cmd_dispatch_confirm(args: argparse.Namespace) -> int:
    body: dict[str, object] = {"actor_id": args.actor_id, "verdict": "proceed"}
    if args.note:
        body["note"] = args.note

    client = client_from_args(args)
    # A 409 (already acknowledged, already spent, or the window closed) is
    # relayed by ApiClient as a CliError carrying the API's own
    # code/message/remediation verbatim — the gate's refusals explain
    # themselves, and re-wording them here would put a second, drifting copy
    # of the protocol in the client.
    resp = client.request("POST", f"{API_PREFIX}/preflights/{args.id}/acknowledge", json_body=body)
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        text = (
            f"preflight_id: {payload.get('id', '')}\n"
            f"acknowledged_by: {payload.get('acknowledged_by', '')}\n"
            f"acknowledgement_record_id: {payload.get('acknowledgement_record_id', '')}\n"
            f"expires_at: {payload.get('expires_at', '')}"
        )
        emit_result(text, json_mode=False)
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes dispatch {pending,show,confirm} ...\n"
        "run 'nodes explain dispatch' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "dispatch",
        help="Thin client for the clarify-then-commit gate (pending/show/confirm).",
    )
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="dispatch_command", parser_class=type(p))

    pending = noun_sub.add_parser(
        "pending", help="List dispatch preflights waiting to be acknowledged."
    )
    pending.add_argument(
        "--actor-key",
        dest="actor_key",
        default=None,
        help="Narrow to one actor identity (e.g. company/codex-thor).",
    )
    pending.add_argument("--limit", type=int, default=None, help="Max items to return.")
    pending.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(pending)
    pending.set_defaults(func=cmd_dispatch_pending)

    show = noun_sub.add_parser("show", help="Print one preflight and the briefing it hands over.")
    show.add_argument("id", help=_PREFLIGHT_ID_HELP)
    show.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(show)
    show.set_defaults(func=cmd_dispatch_show)

    confirm = noun_sub.add_parser(
        "confirm", help="Acknowledge a briefing, committing the gated dispatch."
    )
    confirm.add_argument("id", help=_PREFLIGHT_ID_HELP)
    confirm.add_argument(
        "--actor-id",
        dest="actor_id",
        required=True,
        help="The registered actor this acknowledgement is recorded for.",
    )
    confirm.add_argument(
        "--note",
        dest="note",
        default=None,
        help="Freeform note recorded on the acknowledgement. Documentary.",
    )
    confirm.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(confirm)
    confirm.set_defaults(func=cmd_dispatch_confirm)
