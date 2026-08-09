"""``nodes ledger`` — thin REST client over the ledger read API.

Every verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``ledger`` tag): records, projection. No
engine or projection logic lives in this module (spec decision c28) — the
PRD §10.9 standard projections are computed server-side.
"""

from __future__ import annotations

import argparse

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result


def cmd_ledger_records(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request("GET", f"{API_PREFIX}/runs/{args.run_id}/ledger")
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        items = payload.get("items") or []
        lines = [f"ledger_version: {payload.get('ledger_version', '')}", f"records: {len(items)}"]
        for rec in items:
            lines.append(
                f"  - {rec.get('id', '')}  {rec.get('record_type', '')}  "
                f"{rec.get('authority', '')}  {rec.get('created_at', '')}"
            )
        emit_result("\n".join(lines), json_mode=False)
    return 0


def cmd_ledger_projection(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET",
        f"{API_PREFIX}/runs/{args.run_id}/ledger/projections/{args.name}",
        query={"subject": args.subject},
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        items = payload.get("items") or []
        lines = [
            f"kind: {payload.get('kind', '')}",
            f"subject: {payload.get('subject', '')}",
            f"digest: {payload.get('digest', '')}",
            f"items: {len(items)}",
        ]
        emit_result("\n".join(lines), json_mode=False)
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes ledger {records,projection} ...\nrun 'nodes explain ledger' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser("ledger", help="Thin client for the ledger read API (records/projections).")
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="ledger_command", parser_class=type(p))

    records = noun_sub.add_parser("records", help="List a run's ledger records.")
    records.add_argument("run_id", help="The run id.")
    records.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(records)
    records.set_defaults(func=cmd_ledger_records)

    projection = noun_sub.add_parser(
        "projection", help="Compute one standard projection over a run's ledger."
    )
    projection.add_argument("run_id", help="The run id.")
    projection.add_argument(
        "name", help="Projection name (e.g. current_scope, ready_tasks, delivery_summary)."
    )
    projection.add_argument(
        "--subject", dest="subject", default=None, help="Required by evidence_for_subject."
    )
    projection.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(projection)
    projection.set_defaults(func=cmd_ledger_projection)
