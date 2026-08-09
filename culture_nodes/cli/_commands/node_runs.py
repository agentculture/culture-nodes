"""``nodes node-runs`` — thin REST client over the cross-run node-runs listing.

The single verb here is one HTTP call to the Culture Nodes control-plane API
(``api/openapi/openapi.yaml``, ``node-runs`` tag): the cross-run "jobs
timeline" (task t11) — every node run in the namespace, newest first by
``updated_at``, distinct from the ``node_runs`` nested under one run's
Run-view payload (``nodes run get``). No engine logic lives in this module
(spec decision c28).
"""

from __future__ import annotations

import argparse

from culture_nodes.api_client import API_PREFIX, add_api_url_argument, client_from_args
from culture_nodes.cli._output import JSON_FLAG_HELP, emit_json_passthrough, emit_result


def cmd_node_runs_list(args: argparse.Namespace) -> int:
    client = client_from_args(args)
    resp = client.request(
        "GET",
        f"{API_PREFIX}/node-runs",
        query={
            "updated_since": args.updated_since,
            "updated_until": args.updated_until,
            "cursor": args.cursor,
            "limit": args.limit,
        },
    )
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_json_passthrough(resp.raw)
    else:
        payload = resp.payload or {}
        items = payload.get("items") or []
        if not items:
            emit_result("no node runs", json_mode=False)
        else:
            lines = [
                f"{item.get('id', '')}  {item.get('run_id', '')}  {item.get('node_id', '')}  "
                f"{item.get('state', '')}  {item.get('updated_at', '')}"
                for item in items
            ]
            next_cursor = payload.get("next_cursor")
            if next_cursor:
                lines.append(f"next_cursor: {next_cursor}")
            emit_result("\n".join(lines), json_mode=False)
    return 0


def _bare_noun(args: argparse.Namespace) -> int:
    emit_result(
        "usage: nodes node-runs list ...\nrun 'nodes explain node-runs' for details",
        json_mode=False,
    )
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "node-runs", help="Thin client for the cross-run node-runs listing (jobs timeline)."
    )
    p.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    p.set_defaults(func=_bare_noun, json=False)
    noun_sub = p.add_subparsers(dest="node_runs_command", parser_class=type(p))

    listp = noun_sub.add_parser(
        "list", help="List node runs across every run, newest first by updated_at."
    )
    listp.add_argument(
        "--updated-since",
        dest="updated_since",
        default=None,
        help="Only node runs updated at or after this instant (RFC3339).",
    )
    listp.add_argument(
        "--updated-until",
        dest="updated_until",
        default=None,
        help="Only node runs updated at or before this instant (RFC3339).",
    )
    listp.add_argument(
        "--cursor",
        dest="cursor",
        default=None,
        help=(
            "Opaque keyset cursor from a previous response's next_cursor. "
            "Omit for the first page."
        ),
    )
    listp.add_argument("--limit", type=int, default=None, help="Max items to return.")
    listp.add_argument("--json", action="store_true", help=JSON_FLAG_HELP)
    add_api_url_argument(listp)
    listp.set_defaults(func=cmd_node_runs_list)
